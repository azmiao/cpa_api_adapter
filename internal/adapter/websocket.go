package adapter

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type wsTurns struct {
	mu      sync.Mutex
	pending []Model
	last    Model
}

func (t *wsTurns) add(mapping Model, explicit bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if explicit {
		t.last = mapping
	}
	t.pending = append(t.pending, t.last)
}
func (t *wsTurns) restore(data []byte) []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.pending) == 0 {
		return data
	}
	mapping := t.pending[0]
	if mapping.ID != "" && mapping.ID != mapping.Target {
		data = restoreJSON(data, mapping)
	}
	var event struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &event) == nil {
		switch event.Type {
		case "response.completed", "response.failed", "response.incomplete", "error":
			t.pending = t.pending[1:]
		}
	}
	return data
}

func (m *Manager) proxyWebSocket(w http.ResponseWriter, r *http.Request, cfg Config, generation uint64) {
	if !websocket.IsWebSocketUpgrade(r) {
		writeError(w, &apiError{http.StatusBadRequest, "此接口需要 WebSocket 升级"})
		return
	}
	u := targetURL(cfg, "/backend-api/codex/responses", r.URL.RawQuery)
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	headers := r.Header.Clone()
	for _, key := range []string{"Connection", "Upgrade", "Sec-Websocket-Key", "Sec-Websocket-Version", "Sec-Websocket-Extensions", "Sec-Websocket-Protocol"} {
		headers.Del(key)
	}
	dialer := websocket.Dialer{HandshakeTimeout: 20 * time.Second, Subprotocols: websocket.Subprotocols(r)}
	upstream, response, err := dialer.DialContext(r.Context(), u.String(), headers)
	if err != nil {
		if response != nil {
			defer response.Body.Close()
			for _, key := range []string{"Content-Type", "Retry-After", "Www-Authenticate"} {
				for _, value := range response.Header.Values(key) {
					w.Header().Add(key, value)
				}
			}
			w.WriteHeader(response.StatusCode)
			_, _ = io.Copy(w, io.LimitReader(response.Body, 1<<20))
			return
		}
		writeError(w, &apiError{http.StatusBadGateway, "无法连接 CPA WebSocket 接口"})
		return
	}
	defer upstream.Close()
	upgrader := websocket.Upgrader{}
	if p := upstream.Subprotocol(); p != "" {
		upgrader.Subprotocols = []string{p}
	}
	client, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer client.Close()
	if !m.trackPair(generation, client, upstream) {
		return
	}
	defer m.untrack(client, upstream)
	client.SetReadLimit(int64(cfg.MaxBodyMB) << 20)
	upstream.SetReadLimit(int64(cfg.MaxBodyMB) << 20)
	turns := &wsTurns{}
	var clientWrite sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			kind, data, err := upstream.ReadMessage()
			if err != nil {
				relayWSClose(client, err)
				_ = client.Close()
				return
			}
			if kind == websocket.TextMessage || kind == websocket.BinaryMessage {
				data = turns.restore(data)
			}
			clientWrite.Lock()
			err = client.WriteMessage(kind, data)
			clientWrite.Unlock()
			if err != nil {
				_ = client.Close()
				return
			}
		}
	}()
	for {
		kind, data, err := client.ReadMessage()
		if err != nil {
			relayWSClose(upstream, err)
			break
		}
		if kind == websocket.TextMessage || kind == websocket.BinaryMessage {
			var event struct {
				Type  string          `json:"type"`
				Model json.RawMessage `json:"model"`
			}
			_ = json.Unmarshal(data, &event)
			if event.Type == "response.create" || event.Type == "response.append" {
				updated, mapping, _, err := rewriteRequest(r, cfg, data)
				if err != nil {
					clientWrite.Lock()
					writeErr := client.WriteMessage(websocket.TextMessage, mappingErrorEvent(err))
					clientWrite.Unlock()
					if writeErr != nil {
						break
					}
					continue
				}
				data = updated
				turns.add(mapping, len(event.Model) > 0 && string(event.Model) != "null" && string(event.Model) != `""`)
			}
		}
		if err := upstream.WriteMessage(kind, data); err != nil {
			break
		}
	}
	_ = upstream.Close()
	_ = client.Close()
	<-done
}

func relayWSClose(conn *websocket.Conn, err error) {
	if closeErr, ok := err.(*websocket.CloseError); ok {
		code := closeErr.Code
		if code == websocket.CloseNoStatusReceived || code == websocket.CloseAbnormalClosure || code == websocket.CloseTLSHandshake {
			code = websocket.CloseNormalClosure
		}
		text := closeErr.Text
		if len(text) > 123 {
			text = strings.ToValidUTF8(text[:123], "")
		}
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, text), time.Now().Add(time.Second))
	}
}
