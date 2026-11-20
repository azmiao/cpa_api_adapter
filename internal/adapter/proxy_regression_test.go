package adapter

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func regressionProxy(t *testing.T, upstream *httptest.Server, format string, mappings []Model, maxBodyMB int) string {
	t.Helper()
	cfg := DefaultConfig()
	cfg.ListenMode, cfg.ListenAddress, cfg.MaxBodyMB = "tcp", tcpTestAddress(t), maxBodyMB
	cfg.APIs = []APIConfig{{Name: "regression", APIKey: "client-secret", UpstreamAPIKey: "upstream-secret", Upstream: upstream.URL, Prefix: "/work", Format: format, Mappings: mappings}}
	manager := NewManager()
	if err := manager.Configure(cfg); err != nil || !manager.Status().Running {
		t.Fatalf("configure proxy: %v %+v", err, manager.Status())
	}
	t.Cleanup(manager.Stop)
	return "http://" + cfg.ListenAddress + cfg.APIs[0].BasePath()
}

func regressionRequest(t *testing.T, method, target string, body io.Reader, headers http.Header) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, target, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = headers.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Set("Authorization", "Bearer client-secret")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data
}

func TestProxyRejectsUnregisteredUpgradeWithoutContactingUpstream(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer upstream.Close()
	base := regressionProxy(t, upstream, "v1", []Model{{ID: "target", Target: "target"}}, 1)
	for _, suffix := range []string{"/realtime", "/models", "/chat/completions"} {
		conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(base, "http")+suffix, http.Header{"Authorization": {"Bearer client-secret"}})
		if conn != nil {
			_ = conn.Close()
		}
		if response != nil {
			_ = response.Body.Close()
		}
		if err == nil || response == nil || response.StatusCode != http.StatusBadRequest {
			t.Fatalf("unregistered upgrade accepted: %s, response=%v err=%v", suffix, response, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("unregistered upgrade reached upstream")
	}
}

func TestProxyRestoresSelectedAuthorizationAfterHopHeaderRemoval(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer upstream-secret" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("X-Goog-Api-Key") != "" || r.Header.Get("X-Private") != "" {
			http.Error(w, "incorrect forwarded identity", http.StatusUnauthorized)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"target"}`)
	}))
	defer upstream.Close()
	base := regressionProxy(t, upstream, "v1", []Model{{ID: "target", Target: "target"}}, 1)
	status, body := regressionRequest(t, "POST", base+"/chat/completions", strings.NewReader(`{"model":"target"}`), http.Header{"Connection": {"authorization, x-private, x-api-key, x-goog-api-key"}, "X-Api-Key": {"client-secret"}, "X-Goog-Api-Key": {"client-secret"}, "X-Private": {"remove-me"}, "Content-Type": {"application/json"}})
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"model":"target"`)) {
		t.Fatalf("valid key was lost during forwarding: %d %s", status, body)
	}
}

func TestProxyChunkedOversizeReturns413(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	base := regressionProxy(t, upstream, "v1", nil, 1)
	status, body := regressionRequest(t, "POST", base+"/chat/completions", io.NopCloser(strings.NewReader(strings.Repeat("x", (1<<20)+1))), nil)
	if status != http.StatusRequestEntityTooLarge || !bytes.Contains(body, []byte("请求体超过")) || bytes.Contains(body, []byte("无法连接")) {
		t.Fatalf("chunked limit misreported as upstream failure: %d %s", status, body)
	}
}

func TestResponsesResourceWhitelistCoversReadsAndActions(t *testing.T) {
	var hiddenActions atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer upstream-secret" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("X-Goog-Api-Key") != "" {
			http.Error(w, "wrong identity", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/responses/allowed":
			if r.Header.Get("Range") != "" || r.Header.Get("If-None-Match") != "" {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			_, _ = io.WriteString(w, `{"id":"allowed","model":"target","value":9007199254740993}`)
		case "/v1/responses/hidden":
			if r.Method != http.MethodGet {
				hiddenActions.Add(1)
			}
			_, _ = io.WriteString(w, `{"id":"hidden","model":"not-allowed","output":"private"}`)
		case "/v1/responses/allowed/input_items":
			_, _ = io.WriteString(w, `{"data":[{"text":"allowed-input"}]}`)
		case "/v1/responses/allowed/cancel":
			_, _ = io.WriteString(w, `{"id":"allowed","model":"target","status":"cancelled"}`)
		case "/v1/responses/hidden/input_items", "/v1/responses/hidden/cancel":
			hiddenActions.Add(1)
			_, _ = io.WriteString(w, `{"private":true}`)
		case "/v1/responses/ambiguous":
			_, _ = io.WriteString(w, `{"model":"target","MODEL":"not-allowed","output":"private"}`)
		case "/v1/responses/unknown":
			_, _ = io.WriteString(w, `{"id":"unknown"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	base := regressionProxy(t, upstream, "v1", []Model{{ID: "visible", Target: "target"}}, 1)
	for _, request := range []struct {
		method, suffix, contains string
		status                   int
	}{
		{"GET", "/responses/allowed", `"model":"visible"`, 200},
		{"GET", "/responses/allowed/input_items", "allowed-input", 200},
		{"POST", "/responses/allowed/cancel", `"model":"visible"`, 200},
		{"GET", "/responses/hidden", "未对当前 API 开放", 404},
		{"HEAD", "/responses/hidden", "", 404},
		{"DELETE", "/responses/hidden", "未对当前 API 开放", 404},
		{"GET", "/responses/hidden/input_items", "未对当前 API 开放", 404},
		{"POST", "/responses/hidden/cancel", "未对当前 API 开放", 404},
		{"GET", "/responses/unknown", "无法确认", 502},
		{"GET", "/responses/ambiguous", "不明确", 502},
	} {
		status, body := regressionRequest(t, request.method, base+request.suffix, nil, http.Header{"X-Api-Key": {"client-secret"}, "X-Goog-Api-Key": {"client-secret"}})
		if status != request.status || !bytes.Contains(body, []byte(request.contains)) || bytes.Contains(body, []byte("private")) {
			t.Fatalf("resource policy failed: %s %s => %d %s", request.method, request.suffix, status, body)
		}
		if request.suffix == "/responses/allowed" && !bytes.Contains(body, []byte(`9007199254740993`)) {
			t.Fatal("resource rewrite rounded a large integer")
		}
	}
	status, body := regressionRequest(t, "GET", base+"/responses/allowed", nil, http.Header{"Range": {"bytes=0-1"}, "If-None-Match": {"old-policy"}, "Connection": {"Authorization"}})
	if status != 200 || !bytes.Contains(body, []byte(`"model":"visible"`)) {
		t.Fatalf("conditional resource read bypassed policy: %d %s", status, body)
	}
	if hiddenActions.Load() != 0 {
		t.Fatal("disallowed resource subroute/action reached upstream")
	}
	all := regressionProxy(t, upstream, "v1", nil, 1)
	status, body = regressionRequest(t, "GET", all+"/responses/hidden", nil, nil)
	if status != 200 || !bytes.Contains(body, []byte(`"model":"not-allowed"`)) {
		t.Fatalf("unrestricted resource access was narrowed: %d %s", status, body)
	}
}

func TestRequestRewritePreservesPayloadAndRejectsAmbiguousFields(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"target"}]}`)
	}))
	defer upstream.Close()
	cfg := APIConfig{APIKey: "client-secret", UpstreamAPIKey: "upstream-secret", Upstream: upstream.URL, Format: "v1", Mappings: []Model{{ID: "visible", Target: "target"}}}
	body := []byte("{ \"input\":{\"text\":\"a } , \\\" b\",\"number\":9007199254740993}, \"model\":\"visible\", \"extra\":[true,null,{\"x\":2}] }")
	req := httptest.NewRequest(http.MethodPost, "/work/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer client-secret")
	updated, mapping, changed, err := rewriteRequest(req, cfg, append([]byte(nil), body...))
	want := bytes.Replace(body, []byte(`"visible"`), []byte(`"target"`), 1)
	if err != nil || !changed || mapping.ID != "visible" || !bytes.Equal(updated, want) || !json.Valid(updated) {
		t.Fatalf("rewrite changed unrelated content: %s mapping=%+v changed=%v err=%v", updated, mapping, changed, err)
	}
	for _, body := range []string{`{"model":"visible","mo\u0064el":"target"}`, `{"model":"visible","MODEL":"target"}`, `{"model":"visible","Model":"target"}`, `{"model":"visible","x":1,"\u0078":2}`, `{"model":"visible"} {}`, `[]`, `{"model":null}`} {
		if _, _, _, err := rewriteRequest(req, cfg, []byte(body)); err == nil {
			t.Fatalf("ambiguous/invalid model request accepted: %s", body)
		}
	}
}

func TestLargeMultipartReplayKeepsBinaryAndRejectsDuplicateModel(t *testing.T) {
	payload := bytes.Repeat([]byte{0, 255, '\r', '\n', '=', 1}, (2<<20)/6)
	var uploads atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			_, _ = io.WriteString(w, `{"data":[{"id":"target=audio"}]}`)
			return
		}
		if err := r.ParseMultipartForm(4 << 20); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		defer r.MultipartForm.RemoveAll()
		file, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil || !bytes.Equal(data, payload) || r.FormValue("model") != "target=audio" {
			http.Error(w, "multipart content corrupted", 400)
			return
		}
		uploads.Add(1)
		_, _ = io.WriteString(w, `{"model":"target=audio"}`)
	}))
	defer upstream.Close()
	base := regressionProxy(t, upstream, "v1", []Model{{ID: "visible", Target: "target=audio"}}, 4)
	for _, duplicate := range []bool{false, true} {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		headers := make(http.Header)
		headers.Set("Content-Disposition", `form-data; name="model"`)
		headers.Set("Content-Transfer-Encoding", "quoted-printable")
		part, err := writer.CreatePart(textproto.MIMEHeader(headers))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(part, "visi=62le")
		file, err := writer.CreateFormFile("file", "audio.bin")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = file.Write(payload)
		if duplicate {
			_ = writer.WriteField("model", "not-allowed")
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		status, result := regressionRequest(t, "POST", base+"/audio/transcriptions", bytes.NewReader(body.Bytes()), http.Header{"Content-Type": {writer.FormDataContentType()}})
		if !duplicate && (status != 200 || !bytes.Contains(result, []byte(`"model":"visible"`))) || duplicate && status != 400 {
			t.Fatalf("multipart replay/policy failed: duplicate=%v status=%d %s", duplicate, status, result)
		}
	}
	if uploads.Load() != 1 {
		t.Fatal("duplicate model upload reached upstream")
	}
}
