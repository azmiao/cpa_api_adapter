package adapter

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type wsRegressionFrame struct {
	kind int
	data []byte
}

type wsRegressionEvent struct {
	Type     string `json:"type"`
	Status   int    `json:"status"`
	Model    string `json:"model"`
	Response struct {
		Model string `json:"model"`
	} `json:"response"`
}

type wsRegressionFixture struct {
	client         *websocket.Conn
	upstream       *websocket.Conn
	headers        http.Header
	received       <-chan wsRegressionFrame
	upstreamClosed <-chan error
}

func newWSRegressionFixture(t *testing.T, injectHopHeaders bool) *wsRegressionFixture {
	t.Helper()
	type connection struct {
		conn    *websocket.Conn
		headers http.Header
	}
	connected := make(chan connection, 1)
	received := make(chan wsRegressionFrame, 32)
	closed := make(chan error, 1)
	upgrader := websocket.Upgrader{Subprotocols: []string{"responses.regression"}}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer upstream-secret" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("X-Goog-Api-Key") != "" {
			http.Error(w, "wrong upstream authorization", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"target-a","object":"model"},{"id":"target-b","object":"model"}]}`))
			return
		}
		if r.URL.Path != "/v1/responses" || !websocket.IsWebSocketUpgrade(r) {
			http.NotFound(w, r)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		defer close(received)
		connected <- connection{conn, r.Header.Clone()}
		for {
			kind, data, err := conn.ReadMessage()
			if err != nil {
				closed <- err
				return
			}
			received <- wsRegressionFrame{kind, data}
		}
	}))
	t.Cleanup(upstream.Close)
	cfg := testConfig()
	cfg.ListenMode = "tcp"
	cfg.ListenAddress = tcpTestAddress(t)
	cfg.MaxBodyMB = 1
	cfg.APIs[0].Upstream = upstream.URL
	cfg.APIs[0].Format = "v1"
	cfg.APIs[0].Mappings = []Model{{ID: "alias-a", Target: "target-a"}, {ID: "alias-b", Target: "target-b"}}
	manager := NewManager()
	t.Cleanup(manager.Stop)
	if err := manager.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	if status := manager.Status(); !status.Running {
		t.Fatalf("adapter did not start: %+v", status)
	}
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if injectHopHeaders {
			r.Header.Add("Connection", "Authorization, X-Remove-Me")
			r.Header.Set("X-Remove-Me", "must-not-forward")
			r.Header.Set("Proxy-Connection", "keep-alive")
			r.Header.Set("Keep-Alive", "timeout=5")
			r.Header.Set("Proxy-Authenticate", "must-not-forward")
			r.Header.Set("Proxy-Authorization", "must-not-forward")
			r.Header.Set("Te", "trailers")
			r.Header.Set("Trailer", "Digest")
			r.Header.Set("Transfer-Encoding", "chunked")
		}
		manager.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second, Subprotocols: []string{"responses.regression"}}
	client, response, err := dialer.Dial("ws"+strings.TrimPrefix(front.URL, "http")+cfg.APIs[0].BasePath()+"/responses", http.Header{"Authorization": []string{"Bearer client-secret"}, "X-Api-Key": []string{"client-secret"}, "X-Goog-Api-Key": []string{"client-secret"}})
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	var peer connection
	select {
	case peer = <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream WebSocket did not connect")
	}
	t.Cleanup(func() { _ = peer.conn.Close() })
	return &wsRegressionFixture{client, peer.conn, peer.headers, received, closed}
}

func wsRegressionWrite(t *testing.T, conn *websocket.Conn, kind int, body string) {
	t.Helper()
	if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteMessage(kind, []byte(body)); err != nil {
		t.Fatal(err)
	}
}

func (f *wsRegressionFixture) readClient(t *testing.T) wsRegressionFrame {
	t.Helper()
	if err := f.client.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	kind, data, err := f.client.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	return wsRegressionFrame{kind, data}
}

func (f *wsRegressionFixture) readUpstream(t *testing.T) wsRegressionFrame {
	t.Helper()
	select {
	case frame, ok := <-f.received:
		if !ok {
			t.Fatal("upstream WebSocket closed before receiving the frame")
		}
		return frame
	case <-time.After(5 * time.Second):
		t.Fatal("frame was not forwarded upstream")
		return wsRegressionFrame{}
	}
}

func wsRegressionDecode(t *testing.T, frame wsRegressionFrame, kind int, eventType, model string) wsRegressionEvent {
	t.Helper()
	var event wsRegressionEvent
	if frame.kind != kind || json.Unmarshal(frame.data, &event) != nil {
		t.Fatalf("unexpected WebSocket frame: kind=%d body=%s", frame.kind, frame.data)
	}
	if event.Type != eventType {
		t.Fatalf("expected %s, got %s", eventType, frame.data)
	}
	if model != "" && event.Model != model && event.Response.Model != model {
		t.Fatalf("expected model %q, got %s", model, frame.data)
	}
	return event
}

func (f *wsRegressionFixture) expectClient(t *testing.T, kind int, eventType, model string) wsRegressionEvent {
	t.Helper()
	return wsRegressionDecode(t, f.readClient(t), kind, eventType, model)
}

func (f *wsRegressionFixture) expectLocalError(t *testing.T, status int) {
	t.Helper()
	if event := f.expectClient(t, websocket.TextMessage, "error", ""); event.Status != status {
		t.Fatalf("expected local status %d, got %+v", status, event)
	}
}

func (f *wsRegressionFixture) expectUpstream(t *testing.T, kind int, eventType, model string) {
	t.Helper()
	wsRegressionDecode(t, f.readUpstream(t), kind, eventType, model)
}

func (f *wsRegressionFixture) expectUpstreamBytes(t *testing.T, kind int, body string) {
	t.Helper()
	if frame := f.readUpstream(t); frame.kind != kind || !bytes.Equal(frame.data, []byte(body)) {
		t.Fatalf("unexpected upstream frame: kind=%d body=%s", frame.kind, frame.data)
	}
}

func TestWebSocketRejectsConcurrentCreateWithoutChangingAlias(t *testing.T) {
	f := newWSRegressionFixture(t, false)
	wsRegressionWrite(t, f.client, websocket.TextMessage, `{"type":"response.create","model":"alias-a"}`)
	f.expectUpstream(t, websocket.TextMessage, "response.create", "target-a")
	wsRegressionWrite(t, f.client, websocket.BinaryMessage, `{"type":"response.create","model":"alias-b"}`)
	f.expectLocalError(t, http.StatusConflict)

	wsRegressionWrite(t, f.upstream, websocket.TextMessage, `{"type":"response.created","response":{"id":"response-a","model":"target-a"}}`)
	f.expectClient(t, websocket.TextMessage, "response.created", "alias-a")
	wsRegressionWrite(t, f.upstream, websocket.TextMessage, `{"type":"error","error":{"message":"unowned append error"}}`)
	f.expectClient(t, websocket.TextMessage, "error", "")
	wsRegressionWrite(t, f.client, websocket.TextMessage, `{"type":"response.create","model":"alias-b"}`)
	f.expectLocalError(t, http.StatusConflict)
	wsRegressionWrite(t, f.upstream, websocket.BinaryMessage, `{"type":"response.completed","response":{"id":"response-a","model":"target-a"}}`)
	f.expectClient(t, websocket.BinaryMessage, "response.completed", "alias-a")

	const appendBody = `{"type":"response.append"}`
	wsRegressionWrite(t, f.client, websocket.TextMessage, appendBody)
	f.expectUpstreamBytes(t, websocket.TextMessage, appendBody)
	wsRegressionWrite(t, f.upstream, websocket.TextMessage, `{"type":"response.completed","response":{"id":"response-a-next","model":"target-a"}}`)
	f.expectClient(t, websocket.TextMessage, "response.completed", "alias-a")
}

func TestWebSocketRejectedCreateReleasesTurnAndRollsBackModel(t *testing.T) {
	f := newWSRegressionFixture(t, false)
	wsRegressionWrite(t, f.client, websocket.TextMessage, `{"type":"response.create","model":"alias-a"}`)
	f.expectUpstream(t, websocket.TextMessage, "response.create", "target-a")
	wsRegressionWrite(t, f.client, websocket.BinaryMessage, `{"type":"response.append"}`)
	f.expectLocalError(t, http.StatusConflict)
	wsRegressionWrite(t, f.upstream, websocket.TextMessage, `{"type":"error","error":{"message":"create rejected"}}`)
	f.expectClient(t, websocket.TextMessage, "error", "")
	wsRegressionWrite(t, f.client, websocket.TextMessage, `{"type":"response.append"}`)
	f.expectLocalError(t, http.StatusBadRequest)

	wsRegressionWrite(t, f.client, websocket.BinaryMessage, `{"type":"response.create","model":"alias-b"}`)
	f.expectUpstream(t, websocket.BinaryMessage, "response.create", "target-b")
	wsRegressionWrite(t, f.upstream, websocket.BinaryMessage, `{"type":"response.completed","response":{"id":"response-b","model":"target-b"}}`)
	f.expectClient(t, websocket.BinaryMessage, "response.completed", "alias-b")
	wsRegressionWrite(t, f.client, websocket.TextMessage, `{"type":"response.create","model":"alias-a"}`)
	f.expectUpstream(t, websocket.TextMessage, "response.create", "target-a")
	wsRegressionWrite(t, f.upstream, websocket.TextMessage, `{"type":"error","error":{"message":"create rejected again"}}`)
	f.expectClient(t, websocket.TextMessage, "error", "")

	wsRegressionWrite(t, f.client, websocket.TextMessage, `{"type":"response.append"}`)
	f.expectUpstreamBytes(t, websocket.TextMessage, `{"type":"response.append"}`)
	wsRegressionWrite(t, f.upstream, websocket.TextMessage, `{"type":"response.created","response":{"id":"response-b-next","model":"target-b"}}`)
	f.expectClient(t, websocket.TextMessage, "response.created", "alias-b")
	wsRegressionWrite(t, f.upstream, websocket.TextMessage, `{"type":"response.completed","response":{"id":"response-b-next","model":"target-b"}}`)
	f.expectClient(t, websocket.TextMessage, "response.completed", "alias-b")
}

func TestWebSocketAppendDoesNotQueueOrChangeActiveModel(t *testing.T) {
	f := newWSRegressionFixture(t, false)
	wsRegressionWrite(t, f.client, websocket.TextMessage, `{"type":"response.create","model":"alias-a"}`)
	f.expectUpstream(t, websocket.TextMessage, "response.create", "target-a")
	wsRegressionWrite(t, f.upstream, websocket.TextMessage, `{"type":"response.created","response":{"id":"response-a","model":"target-a"}}`)
	f.expectClient(t, websocket.TextMessage, "response.created", "alias-a")
	wsRegressionWrite(t, f.client, websocket.BinaryMessage, `{"type":"response.append","input":[{"role":"user","content":"more"}]}`)
	f.expectUpstreamBytes(t, websocket.BinaryMessage, `{"type":"response.append","input":[{"role":"user","content":"more"}]}`)
	wsRegressionWrite(t, f.client, websocket.TextMessage, `{"type":"response.append","model":"alias-a"}`)
	f.expectUpstream(t, websocket.TextMessage, "response.append", "target-a")
	wsRegressionWrite(t, f.client, websocket.TextMessage, `{"type":"response.append","model":"alias-b"}`)
	f.expectLocalError(t, http.StatusConflict)
	wsRegressionWrite(t, f.upstream, websocket.BinaryMessage, `{"type":"response.completed","response":{"id":"response-a","model":"target-a"}}`)
	f.expectClient(t, websocket.BinaryMessage, "response.completed", "alias-a")

	wsRegressionWrite(t, f.client, websocket.BinaryMessage, `{"type":"response.create","model":"alias-b"}`)
	f.expectUpstream(t, websocket.BinaryMessage, "response.create", "target-b")
	wsRegressionWrite(t, f.upstream, websocket.TextMessage, `{"type":"response.created","response":{"id":"response-b","model":"target-b"}}`)
	f.expectClient(t, websocket.TextMessage, "response.created", "alias-b")
	wsRegressionWrite(t, f.upstream, websocket.TextMessage, `{"type":"response.completed","response":{"id":"response-b","model":"target-b"}}`)
	f.expectClient(t, websocket.TextMessage, "response.completed", "alias-b")
	wsRegressionWrite(t, f.client, websocket.BinaryMessage, `{"type":"response.append"}`)
	f.expectUpstreamBytes(t, websocket.BinaryMessage, `{"type":"response.append"}`)
	wsRegressionWrite(t, f.upstream, websocket.BinaryMessage, `{"type":"response.completed","response":{"id":"response-b-next","model":"target-b"}}`)
	f.expectClient(t, websocket.BinaryMessage, "response.completed", "alias-b")
}

func TestWebSocketLateResponseIDsDoNotEndNewTurn(t *testing.T) {
	f := newWSRegressionFixture(t, false)
	wsRegressionWrite(t, f.client, websocket.TextMessage, `{"type":"response.create","model":"alias-a"}`)
	f.expectUpstream(t, websocket.TextMessage, "response.create", "target-a")
	wsRegressionWrite(t, f.upstream, websocket.TextMessage, `{"type":"response.completed","response":{"id":"response-a","model":"target-a"}}`)
	f.expectClient(t, websocket.TextMessage, "response.completed", "alias-a")
	wsRegressionWrite(t, f.client, websocket.TextMessage, `{"type":"response.create","model":"alias-b"}`)
	f.expectUpstream(t, websocket.TextMessage, "response.create", "target-b")
	wsRegressionWrite(t, f.upstream, websocket.BinaryMessage, `{"type":"response.completed","response":{"id":"response-a","model":"target-a"}}`)
	f.expectClient(t, websocket.BinaryMessage, "response.completed", "alias-a")
	wsRegressionWrite(t, f.client, websocket.TextMessage, `{"type":"response.create","model":"alias-a"}`)
	f.expectLocalError(t, http.StatusConflict)

	wsRegressionWrite(t, f.upstream, websocket.TextMessage, `{"type":"response.created","response":{"id":"response-b","model":"target-b"}}`)
	f.expectClient(t, websocket.TextMessage, "response.created", "alias-b")
	wsRegressionWrite(t, f.upstream, websocket.TextMessage, `{"type":"response.failed","response_id":"response-a","response":{"model":"target-a"}}`)
	f.expectClient(t, websocket.TextMessage, "response.failed", "alias-a")
	wsRegressionWrite(t, f.upstream, websocket.TextMessage, `{"type":"error","response_id":"response-a","model":"target-a"}`)
	f.expectClient(t, websocket.TextMessage, "error", "alias-a")
	wsRegressionWrite(t, f.upstream, websocket.TextMessage, `{"type":"response.incomplete","response_id":"response-foreign","response":{"model":"target-b"}}`)
	f.expectClient(t, websocket.TextMessage, "response.incomplete", "target-b")
	wsRegressionWrite(t, f.upstream, websocket.TextMessage, `{"type":"response.completed","response":{"model":"target-a"}}`)
	f.expectClient(t, websocket.TextMessage, "response.completed", "target-a")
	wsRegressionWrite(t, f.client, websocket.BinaryMessage, `{"type":"response.create","model":"alias-a"}`)
	f.expectLocalError(t, http.StatusConflict)

	wsRegressionWrite(t, f.upstream, websocket.BinaryMessage, `{"type":"response.completed","response_id":"response-b","response":{"model":"target-b"}}`)
	f.expectClient(t, websocket.BinaryMessage, "response.completed", "alias-b")
	wsRegressionWrite(t, f.client, websocket.TextMessage, `{"type":"response.create","model":"alias-a"}`)
	f.expectUpstream(t, websocket.TextMessage, "response.create", "target-a")
}

func TestWebSocketRejectsDuplicateAndTrailingJSONBeforeForwarding(t *testing.T) {
	f := newWSRegressionFixture(t, false)
	wsRegressionWrite(t, f.client, websocket.TextMessage, `{"type":"response.create","model":"alias-a"}`)
	f.expectUpstream(t, websocket.TextMessage, "response.create", "target-a")
	wsRegressionWrite(t, f.upstream, websocket.TextMessage, `{"type":"response.completed","response":{"id":"response-a","model":"target-a"}}`)
	f.expectClient(t, websocket.TextMessage, "response.completed", "alias-a")
	for i, body := range []string{
		`{"type":"response.create"} trailing`,
		`{"type":"response.create","model":"alias-a"} {}`,
		`{"type":"response.create","ty\u0070e":"client.barrier"}`,
		`{"type":"response.create","model":"alias-a","mod\u0065l":"alias-b"}`,
		`{"type":"response.append","client":1,"client":2}`,
		`{"type":123,"model":"alias-a"}`,
		`{"type":"response.create","model":{}}`,
		`{"type":"response.append","model":null}`,
		`{"type":"response.append","model":""}`,
		`[]`,
		`{"type":"response.create","model":"alias-a"`,
		`{"type":"client.barrier"} null`,
		`{"Type":"response.create"}`,
		`{"TYPE":"response.create","model":"alias-a"}`,
		`{"type":"response.append","Model":"target-b"}`,
		`{"type":"response.append","model":"alias-a","MODEL":"target-b"}`,
		`{"type":"response.append","TYPE":"response.create"}`,
		`{"input":[{"role":"user","content":"implicit default model"}]}`,
		`{}`,
		`{"type":null}`,
		`{"type":""}`,
	} {
		kind := websocket.TextMessage
		if i%2 != 0 {
			kind = websocket.BinaryMessage
		}
		wsRegressionWrite(t, f.client, kind, body)
		f.expectLocalError(t, http.StatusBadRequest)
	}
	const valid = `{"type":"response.append","input":{"model":"unmodified","value":9007199254740993}}`
	wsRegressionWrite(t, f.client, websocket.BinaryMessage, valid)
	f.expectUpstreamBytes(t, websocket.BinaryMessage, valid)
	wsRegressionWrite(t, f.upstream, websocket.BinaryMessage, `{"type":"response.completed","response":{"id":"response-a-next","model":"target-a"}}`)
	f.expectClient(t, websocket.BinaryMessage, "response.completed", "alias-a")
}

func TestWebSocketStripsHopHeadersAndPreservesControlAndClose(t *testing.T) {
	f := newWSRegressionFixture(t, true)
	if f.headers.Get("Authorization") != "Bearer upstream-secret" || f.headers.Get("Connection") != "Upgrade" || f.headers.Get("X-Api-Key") != "" || f.headers.Get("X-Goog-Api-Key") != "" {
		t.Fatalf("incorrect upstream handshake headers: %v", f.headers)
	}
	for _, key := range []string{"X-Remove-Me", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding"} {
		if f.headers.Get(key) != "" {
			t.Fatalf("hop header %s reached upstream: %v", key, f.headers)
		}
	}
	if f.client.Subprotocol() != "responses.regression" || f.upstream.Subprotocol() != "responses.regression" {
		t.Fatal("negotiated subprotocol was not preserved")
	}
	pong := make(chan string, 1)
	f.client.SetPongHandler(func(data string) error {
		pong <- data
		return nil
	})
	if err := f.client.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	readDone := make(chan error, 1)
	go func() {
		for {
			if _, _, err := f.client.ReadMessage(); err != nil {
				readDone <- err
				return
			}
		}
	}()
	if err := f.client.WriteControl(websocket.PingMessage, []byte("control-payload"), time.Now().Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	select {
	case data := <-pong:
		if data != "control-payload" {
			t.Fatalf("pong payload changed: %q", data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ping did not receive a pong")
	}
	if err := f.client.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "closed by client"), time.Now().Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-f.upstreamClosed:
		closeErr, ok := err.(*websocket.CloseError)
		if !ok || closeErr.Code != websocket.ClosePolicyViolation || closeErr.Text != "closed by client" {
			t.Fatalf("close was not relayed upstream: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not receive the close")
	}
	select {
	case <-readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("client reader did not stop after close")
	}
}

func TestWebSocketImplicitCreateRemainsSingleTurn(t *testing.T) {
	f := newWSRegressionFixture(t, false)
	wsRegressionWrite(t, f.client, websocket.TextMessage, `{"model":"alias-a","input":[]}`)
	f.expectUpstream(t, websocket.TextMessage, "", "target-a")
	wsRegressionWrite(t, f.client, websocket.BinaryMessage, `{"model":"alias-b","input":[]}`)
	f.expectLocalError(t, http.StatusConflict)
	wsRegressionWrite(t, f.upstream, websocket.TextMessage, `{"type":"response.created","response":{"id":"implicit-a","model":"target-a"}}`)
	f.expectClient(t, websocket.TextMessage, "response.created", "alias-a")
	wsRegressionWrite(t, f.client, websocket.TextMessage, `{"type":"response.create","model":"alias-b"}`)
	f.expectLocalError(t, http.StatusConflict)
	wsRegressionWrite(t, f.upstream, websocket.BinaryMessage, `{"type":"response.completed","response":{"id":"implicit-a","model":"target-a"}}`)
	f.expectClient(t, websocket.BinaryMessage, "response.completed", "alias-a")
}

func TestWebSocketPendingCreateDoesNotForwardOtherEvents(t *testing.T) {
	f := newWSRegressionFixture(t, false)
	wsRegressionWrite(t, f.client, websocket.TextMessage, `{"type":"response.create","model":"alias-a"}`)
	f.expectUpstream(t, websocket.TextMessage, "response.create", "target-a")
	for _, body := range []string{`{"type":"response.append"}`, `{"type":"client.notification"}`, `{"type":"client.notification","model":"alias-b"}`} {
		wsRegressionWrite(t, f.client, websocket.TextMessage, body)
		f.expectLocalError(t, http.StatusConflict)
	}
	wsRegressionWrite(t, f.upstream, websocket.TextMessage, `{"type":"response.created","response":{"id":"pending-a","model":"target-a"}}`)
	f.expectClient(t, websocket.TextMessage, "response.created", "alias-a")
	wsRegressionWrite(t, f.client, websocket.TextMessage, `{"type":"response.append"}`)
	f.expectUpstream(t, websocket.TextMessage, "response.append", "")
	wsRegressionWrite(t, f.upstream, websocket.TextMessage, `{"type":"error","error":{"message":"append rejected"}}`)
	f.expectClient(t, websocket.TextMessage, "error", "")
	wsRegressionWrite(t, f.client, websocket.TextMessage, `{"type":"response.create","model":"alias-b"}`)
	f.expectLocalError(t, http.StatusConflict)
	wsRegressionWrite(t, f.upstream, websocket.TextMessage, `{"type":"response.completed","response":{"id":"pending-a","model":"target-a"}}`)
	f.expectClient(t, websocket.TextMessage, "response.completed", "alias-a")
	wsRegressionWrite(t, f.client, websocket.TextMessage, `{"type":"response.create","model":"alias-b"}`)
	f.expectUpstream(t, websocket.TextMessage, "response.create", "target-b")
	wsRegressionWrite(t, f.upstream, websocket.TextMessage, `{"type":"response.completed","response":{"id":"next-b","model":"target-b"}}`)
	f.expectClient(t, websocket.TextMessage, "response.completed", "alias-b")
}
