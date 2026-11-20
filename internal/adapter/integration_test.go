package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestAdapterCatalogHTTPAndWebSocket(t *testing.T) {
	for _, network := range []string{"unix", "tcp"} {
		t.Run(network, func(t *testing.T) { testAdapterCatalogHTTPAndWebSocket(t, network) })
	}
}

func testAdapterCatalogHTTPAndWebSocket(t *testing.T, network string) {
	requests := make(chan string, 32)
	var catalogCalls atomic.Int32
	upgrader := websocket.Upgrader{}
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer upstream-secret" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("X-Goog-Api-Key") != "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/v1/models":
			catalogCalls.Add(1)
			if r.URL.Query().Get("client_version") == "" {
				t.Error("missing client_version")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"models":[{"slug":"account-a/gpt","display_name":"A","context_window":64000,"input_modalities":["text"]},{"slug":"account-b/gpt","display_name":"B","context_window":32000}]}`)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/backend-api/codex/responses"):
			body, _ := io.ReadAll(r.Body)
			requests <- r.URL.RequestURI() + " " + string(body)
			var payload struct {
				Model string `json:"model"`
			}
			_ = json.Unmarshal(body, &payload)
			if strings.HasSuffix(r.URL.Path, "/compact") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"model":%q,"value":9007199254740993}`, payload.Model)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":%q}}\n\n", payload.Model)
		case r.Method == http.MethodGet && r.URL.Path == "/backend-api/codex/responses":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			for {
				_, body, err := conn.ReadMessage()
				if err != nil {
					return
				}
				requests <- "ws " + string(body)
				var payload struct {
					Model string `json:"model"`
				}
				_ = json.Unmarshal(body, &payload)
				reply := fmt.Sprintf(`{"type":"response.completed","response":{"model":%q}}`, payload.Model)
				if conn.WriteMessage(websocket.TextMessage, []byte(reply)) != nil {
					return
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer cpa.Close()

	dir, err := os.MkdirTemp("", "api-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	cfg := testConfig()
	cfg.SocketPath = filepath.Join(dir, "adapter.sock")
	cfg.ListenMode = network
	if network == "tcp" {
		cfg.ListenAddress = tcpTestAddress(t)
	}
	cfg.APIs[0].Upstream = cpa.URL
	cfg.APIs[0].Mappings = []Model{
		{ID: "work", Target: "account-a/gpt", DisplayName: "工作模型", ContextWindow: 12345, InputModalities: []string{"text"}},
		{ID: "personal", Target: "account-b/gpt"},
	}
	manager := NewManager()
	if err := manager.Configure(cfg); err != nil || !manager.Status().Running {
		t.Fatalf("start: %v, %+v", err, manager.Status())
	}
	defer manager.Stop()
	upstreamURL, err := url.Parse(cpa.URL)
	if err != nil {
		t.Fatal(err)
	}
	cpaProxy := httputil.NewSingleHostReverseProxy(upstreamURL)
	adapterProxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: "adapter"})
	adapterProxy.Transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		address := cfg.SocketPath
		if network == "tcp" {
			address = cfg.ListenAddress
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, cfg.APIs[0].BasePath()+"/") {
			adapterProxy.ServeHTTP(w, r)
			return
		}
		cpaProxy.ServeHTTP(w, r)
	}))
	defer front.Close()
	base := front.URL + cfg.APIs[0].BasePath()
	client := &http.Client{Timeout: 10 * time.Second}
	do := func(method, path, body, key string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(data)
	}
	for _, origin := range []string{cpa.URL, front.URL} {
		req, err := http.NewRequest(http.MethodGet, origin+"/v1/models", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer client-secret")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized || len(requests) != 0 {
			t.Fatalf("plugin key bypassed the model policy through direct CPA: %d", resp.StatusCode)
		}
	}

	for _, path := range []string{"/codex/models?client_version=9.9.9", "/models?client_version=9.9.9"} {
		status, body := do("GET", path, "", "client-secret")
		if status != 200 || strings.Contains(body, `"slug":"account-a/gpt"`) || !strings.Contains(body, `"slug":"work"`) || !strings.Contains(body, `"display_name":"工作模型"`) || !strings.Contains(body, `"context_window":12345`) {
			t.Fatalf("catalog %s: %d %s", path, status, body)
		}
	}
	if status, _ := do("GET", "/codex/models", "", "wrong"); status != 401 {
		t.Fatalf("invalid key: %d", status)
	}
	status, body := do("POST", "/codex/responses?trace=1", `{"model":"work","value":9007199254740993,"stream":true}`, "client-secret")
	if status != 200 || !strings.Contains(body, `"model":"work"`) {
		t.Fatalf("stream: %d %s", status, body)
	}
	request := <-requests
	if !strings.Contains(request, `/backend-api/codex/responses?trace=1 `) || !strings.Contains(request, `"model":"account-a/gpt"`) || !strings.Contains(request, `9007199254740993`) {
		t.Fatalf("forwarded stream: %s", request)
	}
	status, body = do("POST", "/codex/responses/compact", `{"model":"personal"}`, "client-secret")
	if status != 200 || !strings.Contains(body, `"model":"personal"`) || !strings.Contains(body, `9007199254740993`) {
		t.Fatalf("compact: %d %s", status, body)
	}
	if request = <-requests; !strings.Contains(request, `"model":"account-b/gpt"`) {
		t.Fatalf("forwarded compact: %s", request)
	}
	status, _ = do("POST", "/codex/responses", `{"model":"work","model":"account-b/gpt"}`, "client-secret")
	if status != 400 {
		t.Fatalf("duplicate model accepted: %d", status)
	}
	if status, _ := do("POST", "/codex/responses", `{"model":"account-b/gpt"}`, "client-secret"); status != http.StatusNotFound || len(requests) != 0 {
		t.Fatalf("disabled model reached upstream inference: %d", status)
	}
	if status, _ := do("GET", "/codex/models", "", "upstream-secret"); status != http.StatusUnauthorized {
		t.Fatalf("private upstream key was accepted as a plugin user key: %d", status)
	}

	header := http.Header{"Authorization": []string{"Bearer client-secret"}}
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(front.URL, "http")+cfg.APIs[0].BasePath()+"/codex/responses", header)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"work"}`)); err != nil {
		t.Fatal(err)
	}
	_, data, err := conn.ReadMessage()
	if err != nil || !bytes.Contains(data, []byte(`"model":"work"`)) {
		t.Fatalf("websocket response: %s %v", data, err)
	}
	if request = <-requests; !strings.Contains(request, `"model":"account-a/gpt"`) {
		t.Fatalf("forwarded websocket: %s", request)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte(`{"type":"response.create","model":"personal"}`)); err != nil {
		t.Fatal(err)
	}
	_, data, err = conn.ReadMessage()
	if err != nil || !bytes.Contains(data, []byte(`"model":"personal"`)) {
		t.Fatalf("binary websocket response: %s %v", data, err)
	}
	if request = <-requests; !strings.Contains(request, `"model":"account-b/gpt"`) {
		t.Fatalf("forwarded binary websocket: %s", request)
	}

	cfg.APIs[0].Mappings = []Model{{ID: "account-b/gpt", Target: "account-a/gpt"}}
	if err := manager.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("reconfiguration did not close the old WebSocket")
	}
	if status, _ := do("GET", "/codex/models", "", "client-secret"); status != 409 {
		t.Fatalf("catalog collision: %d", status)
	}
	if status, _ := do("POST", "/codex/responses", `{"model":"account-b/gpt"}`, "client-secret"); status != 409 {
		t.Fatalf("collision routed: %d", status)
	}
	catalogInput, err := json.Marshal(cfg.APIs[0])
	if err != nil {
		t.Fatal(err)
	}
	modelCatalogCache.clear()
	catalogBefore := catalogCalls.Load()
	catalogResponse := (&RPC{Manager: manager}).handleManagement(managementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/plugins/cpa-api-adapter/catalog",
		Body:   catalogInput,
	})
	var catalogResult struct {
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(catalogResponse.Body, &catalogResult); err != nil {
		t.Fatal(err)
	}
	if catalogResponse.StatusCode != http.StatusOK || len(catalogResult.Models) != 2 || len(catalogResult.Warnings) != 1 || !strings.Contains(catalogResult.Warnings[0], "account-b/gpt") {
		t.Fatalf("management catalog with conflict: status=%d body=%s", catalogResponse.StatusCode, catalogResponse.Body)
	}
	if catalogCalls.Load() != catalogBefore+1 {
		t.Fatal("management catalog did not authenticate an independent CPA request")
	}
	if bytes.Contains(catalogResponse.Body, []byte("client-secret")) || bytes.Contains(catalogResponse.Body, []byte("upstream-secret")) {
		t.Fatal("management catalog leaked credentials")
	}
	cfg.APIs[0].Mappings = []Model{{ID: "missing", Target: "account-z/gpt"}}
	if err := manager.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	if status, _ := do("POST", "/codex/responses", `{"model":"missing"}`, "client-secret"); status != 404 {
		t.Fatalf("missing target routed: %d", status)
	}
}

func TestConfigurationRejectsUnsafeSettings(t *testing.T) {
	cfg := testConfig()
	cfg.SocketPath = "relative.sock"
	if err := cfg.Validate(); err == nil {
		t.Fatal("relative socket path accepted")
	}
	cfg = testConfig()
	cfg.APIs[0].Prefix = "/"
	if err := cfg.Validate(); err == nil {
		t.Fatal("root prefix accepted")
	}
	cfg = testConfig()
	cfg.APIs[0].Upstream = "http://127.0.0.1:8317/work"
	if err := cfg.Validate(); err == nil {
		t.Fatal("adapter prefix accepted as CPA upstream")
	}
}

func TestConfigurationAllowsRemoteHTTP(t *testing.T) {
	for _, upstream := range []string{
		"http://192.168.1.20:8317",
		"http://cpa.internal:8317",
		"http://[fd00::1]:8317",
	} {
		t.Run(upstream, func(t *testing.T) {
			cfg := testConfig()
			cfg.APIs[0].Upstream = upstream
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ParseConfig(data); err != nil {
				t.Fatalf("saved configuration rejected: %v", err)
			}
			response := (&RPC{Manager: NewManager()}).handleManagement(managementRequest{
				Method: http.MethodPost,
				Path:   "/v0/management/plugins/cpa-api-adapter/validate",
				Body:   data,
			})
			if response.StatusCode != http.StatusOK {
				t.Fatalf("management validation rejected configuration: %d %s", response.StatusCode, response.Body)
			}
		})
	}
}

func TestSocketDoesNotReplaceExistingFile(t *testing.T) {
	name := filepath.Join(t.TempDir(), "occupied.sock")
	if err := os.WriteFile(name, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.SocketPath = name
	manager := NewManager()
	defer manager.Stop()
	if err := manager.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	if manager.Status().Running {
		t.Fatal("started on an occupied path")
	}
	contents, err := os.ReadFile(name)
	if err != nil || string(contents) != "keep" {
		t.Fatalf("existing file changed: %q %v", contents, err)
	}
}

func TestStaleSocketRecoveryDoesNotRemoveActiveListener(t *testing.T) {
	dir, err := os.MkdirTemp("", "api-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	name := filepath.Join(dir, "stale.sock")
	address := &net.UnixAddr{Name: name, Net: "unix"}
	other, err := net.ListenUnix("unix", address)
	if err != nil {
		t.Fatal(err)
	}
	other.SetUnlinkOnClose(false)
	cfg := testConfig()
	cfg.SocketPath = name
	manager := NewManager()
	defer manager.Stop()
	if err := manager.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	if manager.Status().Running {
		t.Fatal("took over an active socket")
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	if err := manager.Configure(cfg); err != nil || !manager.Status().Running {
		t.Fatalf("stale socket recovery: %v, %+v", err, manager.Status())
	}
}
