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

var wsWriteBufferPool sync.Pool

type wsResponseMapping struct {
	id      string
	mapping Model
}

type wsTurns struct {
	mu         sync.Mutex
	active     Model
	activeID   string
	busy       bool
	accepted   bool
	last       Model
	haveLast   bool
	recent     [4]wsResponseMapping
	nextRecent int
}

func (t *wsTurns) startLocked(mapping Model) {
	t.active = mapping
	t.activeID = ""
	t.busy = true
	t.accepted = false
}

func (t *wsTurns) finishLocked() {
	if t.activeID != "" {
		t.recent[t.nextRecent] = wsResponseMapping{t.activeID, t.active}
		t.nextRecent = (t.nextRecent + 1) % len(t.recent)
	}
	t.active = Model{}
	t.activeID = ""
	t.busy = false
	t.accepted = false
}

func (t *wsTurns) rewrite(r *http.Request, api APIConfig, data []byte) ([]byte, error) {
	fields, err := jsonObjectFields(data)
	if err != nil {
		if len(api.Mappings) == 0 {
			return data, nil
		}
		return nil, &apiError{http.StatusBadRequest, "WebSocket 事件必须是无重复字段的有效 JSON 对象"}
	}
	if len(api.Mappings) > 0 {
		for key := range fields {
			if key != "type" && strings.EqualFold(key, "type") || key != "model" && strings.EqualFold(key, "model") {
				return nil, &apiError{http.StatusBadRequest, "WebSocket 字段 type 和 model 必须使用小写名称"}
			}
		}
	}
	var eventType string
	if span, ok := fields["type"]; ok {
		if err := json.Unmarshal(data[span.start:span.end], &eventType); err != nil {
			return nil, &apiError{http.StatusBadRequest, "WebSocket 事件类型必须是字符串"}
		}
	}
	span, hasModel := fields["model"]
	var id string
	if hasModel {
		if err := json.Unmarshal(data[span.start:span.end], &id); err != nil || id == "" && len(api.Mappings) > 0 {
			return nil, &apiError{http.StatusBadRequest, "请求必须指定有效的模型"}
		}
	}
	if len(api.Mappings) > 0 && eventType == "" && !hasModel {
		return nil, &apiError{http.StatusBadRequest, "WebSocket 事件必须指定 type 或 model"}
	}
	create := eventType == "response.create" || eventType == ""
	appendResponse := eventType == "response.append"
	if !create {
		t.mu.Lock()
		awaitingAcceptance := t.busy && !t.accepted
		t.mu.Unlock()
		if awaitingAcceptance {
			return nil, &apiError{http.StatusConflict, "请等待上游确认当前响应后再发送后续事件"}
		}
	}
	if !create && !appendResponse {
		if !hasModel {
			return data, nil
		}
		updated, _, _, err := rewriteRequest(r, api, data)
		return updated, err
	}

	t.mu.Lock()
	if t.busy && create {
		t.mu.Unlock()
		return nil, &apiError{http.StatusConflict, "此 WebSocket 连接已有未完成的响应"}
	}
	if t.busy && id != "" && id != t.active.ID {
		t.mu.Unlock()
		return nil, &apiError{http.StatusConflict, "不能更改活动响应的模型"}
	}
	if id == "" {
		if !t.busy {
			if !t.haveLast && len(api.Mappings) > 0 {
				t.mu.Unlock()
				return nil, &apiError{http.StatusBadRequest, "请求必须指定模型"}
			}
			t.startLocked(t.last)
		}
		t.mu.Unlock()
		return data, nil
	}
	t.mu.Unlock()

	updated, mapping, _, err := rewriteRequest(r, api, data)
	if err != nil {
		return nil, err
	}
	if len(api.Mappings) == 0 {
		mapping = Model{ID: id, Target: id}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.busy {
		if create {
			return nil, &apiError{http.StatusConflict, "此 WebSocket 连接已有未完成的响应"}
		}
		if mapping.ID != t.active.ID || mapping.Target != t.active.Target {
			return nil, &apiError{http.StatusConflict, "不能更改活动响应的模型"}
		}
	} else {
		t.startLocked(mapping)
	}
	return updated, nil
}

func (t *wsTurns) restore(data []byte) []byte {
	var event struct {
		Type       string `json:"type"`
		ResponseID string `json:"response_id"`
		Response   struct {
			ID string `json:"id"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return data
	}
	id := event.ResponseID
	if id == "" {
		id = event.Response.ID
	} else if event.Response.ID != "" && id != event.Response.ID {
		return data
	}

	t.mu.Lock()
	var mapping Model
	found := false
	if id != "" {
		for i := range t.recent {
			if t.recent[i].id == id {
				mapping, found = t.recent[i].mapping, true
				break
			}
		}
	}
	if !found && t.busy && (t.activeID == "" || id == "" || id == t.activeID) {
		mapping, found = t.active, true
		// Once an ID is confirmed, unowned lifecycle events cannot end the turn.
		if t.activeID == "" || id == t.activeID {
			switch event.Type {
			case "response.created":
				if id != "" {
					t.activeID = id
				}
				t.accepted = true
				t.last, t.haveLast = t.active, true
			case "response.completed":
				if id != "" {
					t.activeID = id
				}
				t.last, t.haveLast = t.active, true
				t.finishLocked()
			case "response.failed", "response.incomplete":
				if id != "" {
					t.activeID = id
				}
				t.finishLocked()
			case "error":
				// An unowned error may reject an append, not the accepted response.
				if !t.accepted || id != "" && t.activeID != "" && id == t.activeID {
					if t.activeID == "" {
						t.activeID = id
					}
					t.finishLocked()
				}
			default:
				if t.accepted && t.activeID == "" && id != "" {
					t.activeID = id
				}
			}
		}
	}
	t.mu.Unlock()
	if found && mapping.ID != "" && mapping.ID != mapping.Target {
		return restoreJSON(data, mapping)
	}
	return data
}

func writeWSMessage(conn *websocket.Conn, kind int, data []byte) error {
	if err := conn.SetWriteDeadline(time.Now().Add(streamWriteTimeout)); err != nil {
		return err
	}
	return conn.WriteMessage(kind, data)
}

func (m *Manager) proxyWebSocket(w http.ResponseWriter, r *http.Request, cfg Config, api APIConfig, generation uint64) {
	if !websocket.IsWebSocketUpgrade(r) {
		writeError(w, &apiError{http.StatusBadRequest, "此接口需要 WebSocket 升级"})
		return
	}
	u := targetURL(api, "/"+api.Format+r.URL.Path[len(api.Prefix)+len(api.Format)+1:], r.URL.RawQuery)
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	headers := r.Header.Clone()
	removeHopHeaders(headers)
	for _, key := range []string{"Sec-Websocket-Key", "Sec-Websocket-Version", "Sec-Websocket-Extensions", "Sec-Websocket-Protocol"} {
		headers.Del(key)
	}
	headers.Set("Authorization", "Bearer "+api.UpstreamAPIKey)
	dialer := websocket.Dialer{HandshakeTimeout: 20 * time.Second, Subprotocols: websocket.Subprotocols(r), WriteBufferSize: 4096, WriteBufferPool: &wsWriteBufferPool}
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
	upgrader := websocket.Upgrader{WriteBufferSize: 4096, WriteBufferPool: &wsWriteBufferPool}
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
			err = writeWSMessage(client, kind, data)
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
			updated, err := turns.rewrite(r, api, data)
			if err != nil {
				clientWrite.Lock()
				writeErr := writeWSMessage(client, websocket.TextMessage, mappingErrorEvent(err))
				clientWrite.Unlock()
				if writeErr != nil {
					break
				}
				continue
			}
			data = updated
		}
		if err := writeWSMessage(upstream, kind, data); err != nil {
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
