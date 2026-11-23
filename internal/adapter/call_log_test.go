package adapter

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func callLogManager(t *testing.T, upstream *httptest.Server, format string, mappings []Model) *Manager {
	t.Helper()
	cfg := DefaultConfig()
	cfg.ListenMode, cfg.ListenAddress = "tcp", tcpTestAddress(t)
	cfg.APIs = []APIConfig{{Name: "分发 A", APIKey: "client-secret", UpstreamAPIKey: "upstream-secret", Prefix: "/work", Format: format, Upstream: upstream.URL, Mappings: mappings}}
	manager := NewManager()
	if err := manager.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Stop)
	return manager
}

func callLogRequest(t *testing.T, manager *Manager, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer client-secret")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "203.0.113.4, 10.0.0.1")
	out := httptest.NewRecorder()
	manager.ServeHTTP(out, req)
	return out
}

func TestCallLogsHTTPPreservesBodiesAndUsage(t *testing.T) {
	for _, test := range []struct {
		name, path, response, expected, contentType, result string
		status                                              int
		input, output, cached                               tokenCount
		alias                                               bool
	}{
		{"responses", "/responses", `{"model":"target","output":[{"content":[{"text":"private-response"}]}],"usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"cached_tokens":50}}}`, "", "application/json", "completed", 200, 100, 20, 50, false},
		{"alias", "/responses", `{"object":"response","model":"target","usage":{"input_tokens":100,"output_tokens":20}}`, `{"object":"response","model":"public","usage":{"input_tokens":100,"output_tokens":20}}`, "application/json", "completed", 200, 100, 20, -1, true},
		{"completions", "/chat/completions", "data: {\"model\":\"target\",\"choices\":[{\"delta\":{\"content\":\"private-response\"}}]}\r\n\r\ndata: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20,\"prompt_tokens_details\":{\"cached_tokens\":0}}}\r\n\r\ndata: [DONE]\r\n\r\n", "", "text/event-stream", "completed", 200, 100, 20, 0, false},
		{"stream-usage", "/responses", "event: response.completed\ndata: {\"type\":\"response.completed\",\n" + "data: \"response\":{\"model\":\"target\",\"usage\":{\"input_tokens\":100,\"output_tokens\":20,\"input_tokens_details\":{\"cached_tokens\":50}}}}\n\n", "", "text/event-stream", "completed", 200, 100, 20, 50, false},
		{"stream-failure", "/responses", "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"private-response\"}}}\n\n", "", "text/event-stream", "failed", 200, -1, -1, -1, false},
		{"stream-incomplete", "/responses", "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\"}}\n\n", "", "text/event-stream", "incomplete", 200, -1, -1, -1, false},
		{"missing-terminal", "/responses", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"private-response\"}\n\n", "", "text/event-stream", "interrupted", 200, -1, -1, -1, false},
		{"limited", "/responses", `{"error":{"code":"rate_limit_exceeded","message":"private-response"}}`, "", "application/json", "failed", 429, -1, -1, -1, false},
		{"no-usage", "/responses", `{"model":"target","output":[]}`, "", "application/json", "completed", 200, -1, -1, -1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var sent string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/models" {
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"data":[{"id":"target"}]}`)
					return
				}
				data, _ := io.ReadAll(r.Body)
				sent = string(data)
				w.Header().Set("Content-Type", test.contentType)
				if test.status == 429 {
					w.Header().Set("Retry-After", "12")
				}
				w.WriteHeader(test.status)
				io.WriteString(w, test.response)
			}))
			defer upstream.Close()
			var mappings []Model
			model := "target"
			if test.alias {
				model = "public"
				mappings = []Model{{ID: "public", Target: "target"}}
			}
			manager := callLogManager(t, upstream, "v1", mappings)
			requestBody := `{"model":"` + model + `","input":"private-request"}`
			out := callLogRequest(t, manager, "/work/v1"+test.path, requestBody)
			expected := test.expected
			if expected == "" {
				expected = test.response
			}
			if out.Code != test.status || out.Body.String() != expected {
				t.Fatalf("response changed: %d %q", out.Code, out.Body.String())
			}
			if sent != strings.Replace(requestBody, `"model":"public"`, `"model":"target"`, 1) {
				t.Fatalf("request changed: %s", sent)
			}
			page := manager.calls.page(callLogQuery{})
			if len(page.Entries) != 1 || page.Summary.Active != 0 {
				t.Fatalf("missing finished call: %+v", page)
			}
			entry := page.Entries[0]
			if entry.Model != model || entry.TargetModel != "target" || entry.IP != "203.0.113.4" || entry.IPSource != "x-forwarded-for" || entry.Status != test.status || entry.Result != test.result {
				t.Fatalf("wrong metadata: %+v", entry)
			}
			if entry.Usage.Input != test.input || entry.Usage.Output != test.output || entry.Usage.Cached != test.cached {
				t.Fatalf("wrong usage: %+v", entry.Usage)
			}
			if test.input >= 0 && entry.Usage.Total != test.input+test.output {
				t.Fatalf("missing token total: %+v", entry.Usage)
			}
			if test.status == 429 && (entry.RetryAfter != "12" || page.Summary.Limited != 1) {
				t.Fatalf("lost rate limit: %+v", page)
			}
			encoded, _ := json.Marshal(page)
			for _, secret := range []string{"private-request", "private-response", "client-secret", "upstream-secret"} {
				if bytes.Contains(encoded, []byte(secret)) {
					t.Fatalf("logged private content: %s", encoded)
				}
			}
		})
	}
}

func TestCallLogsEncodedStreamKeepsBytesAndUnknownUsage(t *testing.T) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":100}}}\n\n")
	writer.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(compressed.Bytes())
	}))
	defer upstream.Close()
	manager := callLogManager(t, upstream, "v1", nil)
	req := httptest.NewRequest(http.MethodPost, "/work/v1/responses", strings.NewReader(`{"model":"target"}`))
	req.Header.Set("Authorization", "Bearer client-secret")
	req.Header.Set("Accept-Encoding", "gzip")
	out := httptest.NewRecorder()
	manager.ServeHTTP(out, req)
	if !bytes.Equal(out.Body.Bytes(), compressed.Bytes()) || out.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("compressed forwarding changed")
	}
	entry := manager.calls.page(callLogQuery{}).Entries[0]
	if entry.Result != "unconfirmed" || entry.Usage.Input != -1 {
		t.Fatalf("invented encoded stream outcome: %+v", entry)
	}
}

func TestCallMetadataFragmentationIgnoresContentAndHandlesEscapedKeys(t *testing.T) {
	payload := `{"mo\u0064el":"target","output":[{"model":"private-model","usage":{"input_tokens":999},"text":"` + strings.Repeat("x", 2<<20) + `"}],"usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"cached_tokens":50}}}`
	var scanner jsonCallScanner
	for i := range payload {
		scanner.feed([]byte(payload[i : i+1]))
	}
	if !scanner.complete() || scanner.meta.Model != "target" || scanner.meta.Usage != (callUsage{100, 20, -1, 50}) {
		t.Fatalf("fragmented metadata: %+v", scanner.meta)
	}
	if scanner.length != 0 {
		t.Fatal("retained content buffer")
	}
	var overflow jsonCallScanner
	overflow.feed([]byte(`{"output":` + strings.Repeat("[", 100) + `0` + strings.Repeat("]", 100) + `}`))
	if !overflow.invalid {
		t.Fatal("unbounded nesting accepted")
	}
}

func TestCallLogsWindowPaginationAndLateCompletion(t *testing.T) {
	var store callStore
	info := callEntry{APIID: "a", APIName: "A", Model: "m", Result: "running", Usage: emptyCallUsage()}
	old := store.start(info, time.Now())
	for range callLogCapacity + 3 {
		trace := store.start(info, time.Now())
		trace.setStatus(200, "", "")
		trace.finish("completed", "")
	}
	old.finish("interrupted", "")
	first := store.page(callLogQuery{})
	if first.Summary.Retained != callLogCapacity || first.Summary.Active != 0 || first.Summary.Completed != callLogCapacity || first.Entries[0].ID != callLogCapacity+4 {
		t.Fatalf("wrong retained window: %+v", first.Summary)
	}
	second := store.page(callLogQuery{Before: first.NextBefore})
	if len(second.Entries) != callLogPageSize || second.Entries[0].ID >= first.Entries[len(first.Entries)-1].ID {
		t.Fatal("duplicate cursor page")
	}
	if len(store.page(callLogQuery{Model: "missing"}).Entries) != 0 || len(store.page(callLogQuery{Result: "failed"}).Entries) != 0 {
		t.Fatal("filters ignored")
	}
}

func TestCallLogsWebSocketCountsResponsesNotConnectionAndKeepsUsage(t *testing.T) {
	upgrader := websocket.Upgrader{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for i := range 2 {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			id := "first"
			if i == 1 {
				id = "second"
			}
			conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"`+id+`"}}`))
			conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"`+id+`","model":"target","usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"cached_tokens":50}}}}`))
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	manager := callLogManager(t, upstream, "backend-api", nil)
	cfg := manager.Status().Config
	cfg.WebSocketEnabled = true
	if err := manager.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	url := "ws://" + manager.Status().ListenAddress + "/work/backend-api/codex/responses"
	conn, _, err := websocket.DefaultDialer.Dial(url, http.Header{"Authorization": {"Bearer client-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for i := range 2 {
		data := `{"type":"response.create","model":"target","input":"private-request"}`
		if i == 1 {
			data = `{"type":"response.create","input":"private-request"}`
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(data)); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if _, _, err := conn.ReadMessage(); err != nil {
				t.Fatal(err)
			}
		}
	}
	page := manager.calls.page(callLogQuery{})
	if len(page.Entries) != 2 || page.Summary.Active != 0 {
		t.Fatalf("connection counted instead of responses: %+v", page)
	}
	for _, entry := range page.Entries {
		if entry.Result != "completed" || entry.Operation != "codex-responses" || entry.Status != 101 || entry.Model != "target" || entry.Usage != (callUsage{100, 20, 120, 50}) {
			t.Fatalf("wrong websocket usage: %+v", entry)
		}
	}
	request, _ := json.Marshal(managementRequest{Method: "POST", Path: "/v0/management/plugins/" + PluginID + "/logs", Body: []byte(`{"result":"completed"}`)})
	encoded, err := (&RPC{Manager: manager}).Handle("management.handle", request)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Result managementResponse `json:"result"`
	}
	if err := json.Unmarshal(encoded, &envelope); err != nil || envelope.Result.StatusCode != 200 {
		t.Fatalf("management logs inaccessible: %s %v", encoded, err)
	}
}

func TestCallLogsDisabledWebSocketDoesNotAffectSummary(t *testing.T) {
	for _, format := range []string{"v1", "backend-api"} {
		t.Run(format, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"model":"target","output":[]}`)
			}))
			defer upstream.Close()
			manager := callLogManager(t, upstream, format, nil)
			cfg := manager.Status().Config
			cfg.WebSocketEnabled = false
			if err := manager.Configure(cfg); err != nil {
				t.Fatal(err)
			}
			path := cfg.APIs[0].BasePath() + "/responses"
			if format == "backend-api" {
				path = cfg.APIs[0].BasePath() + "/codex/responses"
			}
			if response := callLogRequest(t, manager, path, `{"model":"target"}`); response.Code != http.StatusOK {
				t.Fatalf("HTTP call failed: %d %s", response.Code, response.Body)
			}
			before := manager.calls.page(callLogQuery{})
			if len(before.Entries) != 1 || before.Summary.Completed != 1 || before.Summary.Failed != 0 || before.Summary.Finished != 1 {
				t.Fatalf("HTTP success was not counted: %+v", before)
			}
			base := "http://" + manager.Status().ListenAddress + path
			for _, upgrade := range []bool{true, false} {
				var status int
				if upgrade {
					conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(base, "http"), http.Header{"Authorization": {"Bearer client-secret"}})
					if conn != nil {
						conn.Close()
					}
					if err == nil || response == nil {
						t.Fatalf("disabled WebSocket accepted: %v", err)
					}
					status = response.StatusCode
					response.Body.Close()
				} else {
					status, _ = regressionRequest(t, http.MethodGet, base, nil, nil)
				}
				if status != http.StatusForbidden {
					t.Fatalf("disabled endpoint upgrade=%v returned %d", upgrade, status)
				}
				after := manager.calls.page(callLogQuery{})
				if after.Summary != before.Summary || len(after.Entries) != 1 || after.Entries[0].ID != before.Entries[0].ID {
					t.Fatalf("disabled WebSocket changed HTTP logs or success statistics: before=%+v after=%+v", before, after)
				}
			}
		})
	}
}

func TestCallLogsAverageIncludesRetainedFinishedCallsWhenActiveOverflows(t *testing.T) {
	var store callStore
	info := callEntry{APIID: "a", Result: "running", Usage: emptyCallUsage()}
	for range callLogCapacity + 1 {
		store.start(info, time.Now())
	}
	for range 2 {
		trace := store.start(info, time.Now().Add(-time.Second))
		trace.finish("completed", "")
	}
	summary := store.page(callLogQuery{}).Summary
	if summary.Active != callLogCapacity+1 || summary.Finished != 2 || summary.Completed != 2 || summary.AverageMS < 1000 {
		t.Fatalf("active overflow hid retained completed calls: %+v", summary)
	}
}
