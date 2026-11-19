package adapter

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"gopkg.in/yaml.v3"
)

func testConfig() Config {
	cfg := DefaultConfig()
	cfg.APIs = []APIConfig{{Name: "Work", APIKey: "client-secret", Upstream: "http://127.0.0.1:8317", Prefix: "/work", Format: "backend-api", Mappings: []Model{}}}
	return cfg
}

func TestPerKeyStorageRoutesAndModelPolicy(t *testing.T) {
	var inferenceCalls atomic.Int32
	upstream := func(key, target string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+key && r.Header.Get("Authorization") != "Bearer all-key" {
				t.Errorf("wrong upstream key for %s", target)
				http.Error(w, "wrong key", 401)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/v1/models" {
				if r.URL.Query().Get("client_version") == "" {
					_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]string{"id": target, "object": "model"}, map[string]string{"id": "hidden", "object": "model"}}})
				} else {
					_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{map[string]string{"slug": target}, map[string]string{"slug": "hidden"}}})
				}
				return
			}
			if strings.HasPrefix(r.URL.Path, "/v1/models/") {
				_ = json.NewEncoder(w).Encode(map[string]string{"object": "model", "id": strings.TrimPrefix(r.URL.Path, "/v1/models/")})
				return
			}
			if websocket.IsWebSocketUpgrade(r) {
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				last := ""
				for {
					_, data, err := conn.ReadMessage()
					if err != nil {
						return
					}
					var event struct{ Model string }
					_ = json.Unmarshal(data, &event)
					if event.Model != "" {
						last = event.Model
					}
					if last != target {
						t.Errorf("WebSocket routed model %q instead of %q", last, target)
					}
					inferenceCalls.Add(1)
					reply, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]string{"model": last}})
					if conn.WriteMessage(websocket.TextMessage, reply) != nil {
						return
					}
				}
			}
			var model string
			var stream bool
			if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Error(err)
					return
				}
				defer r.MultipartForm.RemoveAll()
				model = r.FormValue("model")
				file, _, err := r.FormFile("file")
				if err != nil {
					t.Error(err)
					return
				}
				defer file.Close()
				data, _ := io.ReadAll(file)
				if !bytes.Equal(data, []byte{0, 255, 1, 13, 10}) {
					t.Errorf("upload corrupted: %v", data)
				}
			} else {
				var input struct {
					Model  string `json:"model"`
					Stream bool   `json:"stream"`
				}
				_ = json.NewDecoder(r.Body).Decode(&input)
				model, stream = input.Model, input.Stream
			}
			if model != target {
				t.Errorf("routed model %q instead of %q", model, target)
			}
			inferenceCalls.Add(1)
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"model\":\""+model+"\"}\n\ndata: [DONE]\n\n")
			} else {
				_ = json.NewEncoder(w).Encode(map[string]string{"model": model, "path": r.URL.Path, "query": r.URL.RawQuery})
			}
		}))
	}
	a, b := upstream("a-key", "target-a"), upstream("b-key", "target-b")
	defer a.Close()
	defer b.Close()
	cfg := DefaultConfig()
	cfg.APIs = []APIConfig{
		{Name: "A", APIKey: "a-key", Upstream: a.URL, Prefix: "/a", Format: "v1", Mappings: []Model{{ID: "shared", Target: "target-a"}}},
		{Name: "B", APIKey: "b-key", Upstream: b.URL, Prefix: "/b", Format: "backend-api", Mappings: []Model{{ID: "shared", Target: "target-b"}}},
		{Name: "All", APIKey: "all-key", Upstream: a.URL, Prefix: "/all", Format: "v1", Mappings: []Model{}},
	}
	dir, err := os.MkdirTemp("", "api-keys-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	cfg.SocketPath = filepath.Join(dir, "adapter.sock")
	saved, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseConfig(saved)
	if err != nil || !reflect.DeepEqual(parsed, cfg) {
		t.Fatalf("per-key storage changed: %v", err)
	}
	manager := NewManager()
	if err := manager.Configure(parsed); err != nil || !manager.Status().Running {
		t.Fatalf("configure: %v", err)
	}
	defer manager.Stop()
	parsed.APIs[0].APIKey = "mutated"
	parsed.APIs[0].Mappings[0].Target = "mutated"
	front := httptest.NewServer(manager)
	defer front.Close()
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	do := func(method, path, key, contentType string, body []byte) (int, string) {
		t.Helper()
		req, err := http.NewRequest(method, front.URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Api-Key", key)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(data)
	}
	for _, item := range []struct{ path, key, field string }{{"/a/v1/models?client_version=9", "a-key", "id"}, {"/b/backend-api/models", "b-key", "slug"}} {
		status, body := do("GET", item.path, item.key, "", nil)
		if status != 200 || !strings.Contains(body, `"`+item.field+`":"shared"`) || strings.Contains(body, "target-") || strings.Contains(body, "hidden") {
			t.Fatalf("isolated catalog: %d %s", status, body)
		}
	}
	if status, body := do("GET", "/all/v1/models", "all-key", "", nil); status != 200 || !strings.Contains(body, `"id":"target-a"`) || !strings.Contains(body, `"id":"hidden"`) {
		t.Fatalf("unconfigured catalog: %d %s", status, body)
	}
	for _, item := range []struct {
		path, key, body string
		status          int
	}{
		{"/a/v1/chat/completions", "b-key", `{"model":"shared"}`, 401},
		{"/b/backend-api/codex/responses", "a-key", `{"model":"shared"}`, 401},
		{"/a/v1/chat/completions", "unknown", `{"model":"shared"}`, 401},
		{"/a/v1/chat/completions", "a-key", `{"model":"target-a"}`, 404},
		{"/a/v1/chat/completions", "a-key", `{"model":"hidden"}`, 404},
		{"/a/v1/chat/completions", "a-key", `{}`, 400},
		{"/a/v1/chat/completions", "a-key", `{"model":null}`, 400},
		{"/a/v1/chat/completions", "a-key", `{"model":"shared"} {}`, 400},
	} {
		before := inferenceCalls.Load()
		if status, body := do("POST", item.path, item.key, "application/json", []byte(item.body)); status != item.status {
			t.Fatalf("model/key policy: want %d got %d %s", item.status, status, body)
		}
		if inferenceCalls.Load() != before {
			t.Fatal("denied request reached upstream inference")
		}
	}
	if status, _ := do("GET", "/a/v1/./models", "a-key", "", nil); status != 400 {
		t.Fatalf("noncanonical catalog path bypassed filtering: %d", status)
	}
	if status, body := do("POST", "/a/v1/chat/completions?trace=1", "a-key", "application/json", []byte(`{"model":"shared"}`)); status != 200 || !strings.Contains(body, `"model":"shared"`) || !strings.Contains(body, `"path":"/v1/chat/completions"`) || !strings.Contains(body, `"query":"trace=1"`) {
		t.Fatalf("v1 forwarding: %d %s", status, body)
	}
	if status, body := do("POST", "/a/v1/responses", "a-key", "application/json", []byte(`{"model":"shared","stream":true}`)); status != 200 || !strings.Contains(body, `"model":"shared"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("v1 SSE: %d %s", status, body)
	}
	if status, body := do("GET", "/a/v1/models/shared", "a-key", "", nil); status != 200 || !strings.Contains(body, `"id":"shared"`) {
		t.Fatalf("v1 model resource alias: %d %s", status, body)
	}
	for _, duplicate := range []bool{false, true} {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		_ = writer.WriteField("model", "shared")
		if duplicate {
			_ = writer.WriteField("model", "hidden")
		}
		file, _ := writer.CreateFormFile("file", "audio.bin")
		_, _ = file.Write([]byte{0, 255, 1, 13, 10})
		_ = writer.Close()
		status, result := do("POST", "/a/v1/audio/transcriptions", "a-key", writer.FormDataContentType(), body.Bytes())
		if duplicate && status != 400 || !duplicate && (status != 200 || !strings.Contains(result, `"model":"shared"`)) {
			t.Fatalf("multipart policy: %d %s", status, result)
		}
	}
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(front.URL, "http")+"/a/v1/responses", http.Header{"Authorization": []string{"Bearer a-key"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for _, item := range []struct{ body, expected string }{
		{`{"type":"response.create"}`, `"status":400`},
		{`{"type":"response.create","model":"hidden"}`, `"status":404`},
		{`{"type":"response.create","model":"shared"}`, `"model":"shared"`},
		{`{"type":"response.append"}`, `"model":"shared"`},
	} {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(item.body)); err != nil {
			t.Fatal(err)
		}
		_, body, err := conn.ReadMessage()
		if err != nil || !bytes.Contains(body, []byte(item.expected)) {
			t.Fatalf("WebSocket policy: %s %v", body, err)
		}
	}
}

func TestPerKeyConfigurationRejectsAmbiguousRoutes(t *testing.T) {
	for _, change := range []func(*Config){
		func(c *Config) { c.APIs[0].Prefix = "" },
		func(c *Config) { c.APIs[0].Format = "invalid" },
		func(c *Config) { c.APIs = append(c.APIs, c.APIs[0]); c.APIs[1].Prefix = "/other" },
		func(c *Config) { c.APIs = append(c.APIs, c.APIs[0]); c.APIs[1].APIKey = "other-key" },
	} {
		cfg := testConfig()
		change(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Fatal("ambiguous or incomplete API configuration accepted")
		}
	}
	if _, err := ParseConfig([]byte("prefix: /omp\nupstream: http://localhost:8317\n")); err == nil {
		t.Fatal("old global configuration silently accepted")
	}
}
