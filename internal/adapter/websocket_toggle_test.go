package adapter

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWebSocketToggleDefaultsAndReload(t *testing.T) {
	for _, format := range []string{"v1", "backend-api"} {
		t.Run(format, func(t *testing.T) {
			var upgrades atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if websocket.IsWebSocketUpgrade(r) {
					upgrades.Add(1)
					conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer conn.Close()
					for {
						kind, body, err := conn.ReadMessage()
						if err != nil || conn.WriteMessage(kind, body) != nil {
							return
						}
					}
				}
				body, _ := io.ReadAll(r.Body)
				if bytes.Contains(body, []byte(`"stream":true`)) {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"model\":\"target\"}}\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"model":"target","output":[]}`)
			}))
			t.Cleanup(upstream.Close)
			cfg := testConfig()
			cfg.ListenMode, cfg.ListenAddress = "tcp", tcpTestAddress(t)
			cfg.APIs[0].Upstream, cfg.APIs[0].Format = upstream.URL, format
			wire, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var legacy map[string]json.RawMessage
			if err := json.Unmarshal(wire, &legacy); err != nil {
				t.Fatal(err)
			}
			delete(legacy, "websocket_enabled")
			wire, err = json.Marshal(legacy)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err = ParseConfig(wire)
			if err != nil {
				t.Fatal(err)
			}
			manager := NewManager()
			t.Cleanup(manager.Stop)
			path := "/responses"
			if format == "backend-api" {
				path = "/codex/responses"
			}
			base := "http://" + cfg.ListenAddress + cfg.APIs[0].BasePath() + path
			dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
			var active *websocket.Conn
			for _, step := range []string{"omitted", "enabled", "disabled", "reenabled"} {
				if step != "omitted" {
					cfg.WebSocketEnabled = step != "disabled"
					wire, err = json.Marshal(cfg)
					if err != nil {
						t.Fatal(err)
					}
					cfg, err = ParseConfig(wire)
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := manager.Configure(cfg); err != nil || !manager.Status().Running {
					t.Fatalf("%s configure: %v %+v", step, err, manager.Status())
				}
				if active != nil {
					active.SetReadDeadline(time.Now().Add(5 * time.Second))
					_, _, err := active.ReadMessage()
					if err == nil {
						t.Fatal("disabling WebSocket left an existing connection open")
					}
					if networkErr, ok := err.(net.Error); ok && networkErr.Timeout() {
						t.Fatal("existing WebSocket did not close after disabling")
					}
					active.Close()
					active = nil
				}
				before := upgrades.Load()
				conn, response, err := dialer.Dial("ws"+strings.TrimPrefix(base, "http"), http.Header{"Authorization": {"Bearer client-secret"}})
				if step == "omitted" || step == "disabled" {
					if conn != nil {
						conn.Close()
					}
					if err == nil || response == nil {
						t.Fatalf("%s WebSocket accepted: %v", step, err)
					}
					body, _ := io.ReadAll(response.Body)
					response.Body.Close()
					if response.StatusCode != http.StatusForbidden || !bytes.Contains(body, []byte("WebSocket 已禁用")) || upgrades.Load() != before {
						t.Fatalf("%s rejection: %d %s, upstream upgrades=%d", step, response.StatusCode, body, upgrades.Load())
					}
				} else {
					if err != nil {
						t.Fatalf("%s handshake: %v", step, err)
					}
					t.Cleanup(func() { conn.Close() })
					wsRegressionWrite(t, conn, websocket.TextMessage, `{"type":"response.create","model":"target"}`)
					conn.SetReadDeadline(time.Now().Add(5 * time.Second))
					_, body, err := conn.ReadMessage()
					if err != nil || !bytes.Contains(body, []byte(`"model":"target"`)) {
						t.Fatalf("%s forwarding: %s %v", step, body, err)
					}
					active = conn
				}
				for _, stream := range []bool{false, true} {
					request, _ := json.Marshal(map[string]any{"model": "target", "stream": stream})
					status, body := regressionRequest(t, http.MethodPost, base, bytes.NewReader(request), http.Header{"Content-Type": {"application/json"}})
					if status != http.StatusOK || !bytes.Contains(body, []byte(`"model":"target"`)) || stream && !bytes.Contains(body, []byte("data: ")) {
						t.Fatalf("%s HTTP/SSE stream=%v: %d %s", step, stream, status, body)
					}
				}
			}
		})
	}
}
