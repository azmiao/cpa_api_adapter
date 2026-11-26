package adapter

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const callLogCapacity = 1000
const callLogPageSize = 50

type tokenCount int64

func (n tokenCount) MarshalJSON() ([]byte, error) {
	if n < 0 {
		return []byte("null"), nil
	}
	return strconv.AppendInt(nil, int64(n), 10), nil
}

type callUsage struct {
	Input  tokenCount `json:"input"`
	Output tokenCount `json:"output"`
	Total  tokenCount `json:"total"`
	Cached tokenCount `json:"cached"`
}

func emptyCallUsage() callUsage { return callUsage{-1, -1, -1, -1} }

type callEntry struct {
	ID            uint64    `json:"id"`
	Time          time.Time `json:"time"`
	APIID         string    `json:"api_id"`
	APIName       string    `json:"api_name"`
	APIPath       string    `json:"api_path"`
	Model         string    `json:"model"`
	TargetModel   string    `json:"target_model"`
	Operation     string    `json:"operation"`
	Method        string    `json:"method"`
	Transport     string    `json:"transport"`
	IP            string    `json:"ip"`
	IPSource      string    `json:"ip_source"`
	Status        int       `json:"status"`
	Result        string    `json:"result"`
	ErrorClass    string    `json:"error_class,omitempty"`
	RetryAfter    string    `json:"retry_after,omitempty"`
	DurationMS    float64   `json:"duration_ms"`
	ResponseBytes int64     `json:"response_bytes"`
	Usage         callUsage `json:"usage"`
}

type callStore struct {
	mu      sync.Mutex
	entries [callLogCapacity]callEntry
	next    uint64
	active  int
}

type callTrace struct {
	mu             sync.Mutex
	store          *callStore
	entry          callEntry
	started        time.Time
	finished       bool
	readFailed     bool
	writeFailed    bool
	observedStream bool
	terminal       bool
}

func boundedLogString(value string) string {
	if len(value) <= 256 {
		return value
	}
	value = value[:256]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return strings.Clone(value)
}

func callInfo(r *http.Request, api APIConfig, sharedPrefix string, generation uint64, index int, suffix string) callEntry {
	name := api.Name
	if name == "" {
		name = "API " + strconv.Itoa(index+1)
	}
	ip, source := "", "remote_addr"
	for _, header := range []string{"X-Forwarded-For", "X-Real-IP"} {
		value, _, _ := strings.Cut(r.Header.Get(header), ",")
		if addr, err := netip.ParseAddr(strings.TrimSpace(value)); err == nil {
			ip, source = addr.String(), strings.ToLower(header)
			break
		}
	}
	if ip == "" {
		ip = r.RemoteAddr
		if host, _, err := net.SplitHostPort(ip); err == nil {
			ip = host
		}
	}
	var apiPath string
	if sharedPrefix == "" {
		apiPath = api.BasePath()
	} else {
		apiPath = "/" + sharedPrefix + api.Prefix + "/" + api.Format
	}
	return callEntry{APIID: strconv.FormatUint(generation, 10) + ":" + strconv.Itoa(index+1), APIName: boundedLogString(name), APIPath: boundedLogString(apiPath), Operation: callOperation(api.Format, suffix), Method: boundedLogString(r.Method), Transport: "http", IP: boundedLogString(ip), IPSource: source, Result: "running", Usage: emptyCallUsage()}
}

func callOperation(format, suffix string) string {
	switch {
	case suffix == "/chat/completions":
		return "completions"
	case suffix == "/completions":
		return "completions (legacy)"
	case suffix == "/codex/responses/compact":
		return "codex-responses/compact"
	case suffix == "/codex/responses":
		return "codex-responses"
	case suffix == "/responses/compact":
		return "responses/compact"
	case suffix == "/responses":
		return "responses"
	case strings.HasPrefix(suffix, "/responses/"):
		return "responses-resource"
	case suffix == "/models" || suffix == "/codex/models":
		return "models"
	default:
		return "other"
	}
}

func (s *callStore) start(info callEntry, started time.Time) *callTrace {
	if started.IsZero() {
		started = time.Now()
	}
	s.mu.Lock()
	s.next++
	info.ID, info.Time = s.next, started
	s.entries[(info.ID-1)%callLogCapacity] = info
	s.active++
	s.mu.Unlock()
	return &callTrace{store: s, entry: info, started: started}
}

func (t *callTrace) publish() {
	t.store.mu.Lock()
	if slot := &t.store.entries[(t.entry.ID-1)%callLogCapacity]; slot.ID == t.entry.ID {
		*slot = t.entry
	}
	t.store.mu.Unlock()
}

func (t *callTrace) setModel(mapping Model) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
	}
	if mapping.ID != "" {
		t.entry.Model = boundedLogString(mapping.ID)
	}
	if mapping.Target != "" {
		t.entry.TargetModel = boundedLogString(mapping.Target)
	}
	t.publish()
}

func (t *callTrace) setRequestModel(model string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished || t.entry.Model != "" || model == "" {
		return
	}
	t.entry.Model, t.entry.TargetModel = model, model
	t.publish()
}

func (t *callTrace) setStatus(status int, retryAfter, transport string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	changed := false
	if t.entry.Status == 0 && (status >= 200 || status == http.StatusSwitchingProtocols) {
		t.entry.Status = status
		changed = true
	}
	if retryAfter != "" && t.entry.RetryAfter != retryAfter {
		t.entry.RetryAfter = boundedLogString(retryAfter)
		changed = true
	}
	if transport != "" && t.entry.Transport != transport {
		t.entry.Transport = transport
		changed = true
	}
	if changed {
		t.publish()
	}
}

func (t *callTrace) setErrorClass(errorClass string) {
	t.mu.Lock()
	t.entry.ErrorClass = errorClass
	t.mu.Unlock()
}

func (t *callTrace) addBytes(n int, err error) {
	t.mu.Lock()
	t.entry.ResponseBytes += int64(n)
	t.writeFailed = t.writeFailed || err != nil
	t.mu.Unlock()
}

func (t *callTrace) markReadFailed() {
	t.mu.Lock()
	t.readFailed = true
	t.mu.Unlock()
}

func (t *callTrace) markTerminal() {
	t.mu.Lock()
	t.terminal = true
	t.mu.Unlock()
}

func logErrorClass(status int) string {
	switch {
	case status == 401:
		return "unauthorized"
	case status == 403:
		return "forbidden"
	case status == 404:
		return "not_found"
	case status == 413:
		return "body_too_large"
	case status == 429:
		return "rate_limited"
	case status >= 500:
		return "upstream_error"
	case status >= 400:
		return "request_rejected"
	default:
		return ""
	}
}

func safeErrorCode(code string) string {
	if code == "" || len(code) > 64 {
		return "upstream_error"
	}
	for _, c := range code {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return "upstream_error"
		}
	}
	return code
}

func (t *callTrace) observe(meta jsonCallMetadata) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
	}
	if meta.Model != "" && t.entry.Model == "" {
		t.entry.Model = meta.Model
	}
	if meta.Model != "" && t.entry.TargetModel == "" {
		t.entry.TargetModel = meta.Model
	}
	for _, pair := range [][2]*tokenCount{{&t.entry.Usage.Input, &meta.Usage.Input}, {&t.entry.Usage.Output, &meta.Usage.Output}, {&t.entry.Usage.Total, &meta.Usage.Total}, {&t.entry.Usage.Cached, &meta.Usage.Cached}} {
		if *pair[1] >= 0 {
			*pair[0] = *pair[1]
		}
	}
	if t.entry.Usage.Total < 0 && t.entry.Usage.Input >= 0 && t.entry.Usage.Output >= 0 && t.entry.Usage.Input <= tokenCount((1<<63)-1)-t.entry.Usage.Output {
		t.entry.Usage.Total = t.entry.Usage.Input + t.entry.Usage.Output
	}
	switch {
	case meta.Type == "response.completed" || meta.Status == "completed":
		t.terminal = true
	case meta.Type == "response.failed" || meta.Status == "failed" || meta.Type == "error" || meta.HasError:
		t.terminal, t.entry.Result, t.entry.ErrorClass = true, "failed", safeErrorCode(meta.Code)
	case meta.Type == "response.incomplete" || meta.Status == "incomplete":
		t.terminal, t.entry.Result, t.entry.ErrorClass = true, "incomplete", "upstream_incomplete"
	}
}

func (t *callTrace) finish(result, errorClass string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
	}
	t.finished = true
	if t.entry.Result == "running" {
		t.entry.Result = result
	}
	if errorClass != "" && t.entry.ErrorClass == "" {
		t.entry.ErrorClass = errorClass
	}
	t.entry.DurationMS = float64(time.Since(t.started)) / float64(time.Millisecond)
	t.store.mu.Lock()
	if slot := &t.store.entries[(t.entry.ID-1)%callLogCapacity]; slot.ID == t.entry.ID {
		*slot = t.entry
	}
	t.store.active--
	t.store.mu.Unlock()
}

func (t *callTrace) finishHTTP(ctx context.Context) {
	t.mu.Lock()
	result, errorClass := "completed", logErrorClass(t.entry.Status)
	if t.entry.Status >= 400 {
		result = "failed"
	}
	if t.entry.Status < 400 && (ctx.Err() != nil || t.readFailed || t.writeFailed) {
		result, errorClass = "interrupted", "stream_interrupted"
	} else if t.entry.Status < 400 && t.entry.Transport == "sse" && !t.terminal {
		if t.observedStream {
			result, errorClass = "interrupted", "missing_terminal_event"
		} else {
			result, errorClass = "unconfirmed", "metadata_unavailable"
		}
	}
	t.mu.Unlock()
	t.finish(result, errorClass)
}

type callResponseWriter struct {
	http.ResponseWriter
	trace *callTrace
}

func (w callResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w callResponseWriter) WriteHeader(status int) {
	w.trace.setStatus(status, "", "")
	w.ResponseWriter.WriteHeader(status)
}
func (w callResponseWriter) Write(body []byte) (int, error) {
	w.trace.setStatus(http.StatusOK, "", "")
	n, err := w.ResponseWriter.Write(body)
	w.trace.addBytes(n, err)
	return n, err
}

type callObservedBody struct {
	io.ReadCloser
	trace   *callTrace
	parser  jsonCallScanner
	sse     *sseCallScanner
	request bool
	ended   bool
}

func (b *callObservedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.sse != nil {
		b.sse.feed(p[:n], b.trace)
	} else {
		b.parser.feed(p[:n])
	}
	if b.request && b.parser.meta.Model != "" {
		b.trace.setRequestModel(b.parser.meta.Model)
	}
	if err != nil && !b.ended {
		b.ended = true
		if err != io.EOF {
			b.trace.markReadFailed()
		}
		if !b.request {
			if b.sse != nil {
				b.sse.end(b.trace)
			} else if b.parser.complete() {
				b.trace.observe(b.parser.meta)
			}
		}
	}
	return n, err
}

func observeCallResponse(resp *http.Response, trace *callTrace) {
	contentType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	transport := "http"
	if contentType == "text/event-stream" {
		transport = "sse"
	}
	trace.setStatus(resp.StatusCode, resp.Header.Get("Retry-After"), transport)
	encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding"))
	if resp.Body == nil || encoding != "" && !strings.EqualFold(encoding, "identity") {
		return
	}
	if transport != "sse" && contentType != "application/json" && !strings.HasSuffix(contentType, "+json") {
		return
	}
	body := &callObservedBody{ReadCloser: resp.Body, trace: trace}
	if transport == "sse" {
		body.sse = &sseCallScanner{}
		trace.mu.Lock()
		trace.observedStream = true
		trace.mu.Unlock()
	}
	resp.Body = body
}

type callLogQuery struct {
	Before uint64 `json:"before"`
	API    string `json:"api"`
	Model  string `json:"model"`
	Result string `json:"result"`
}

type callLogChoice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type callLogSummary struct {
	Retained  int     `json:"retained"`
	Active    int     `json:"active"`
	Finished  int     `json:"finished"`
	Completed int     `json:"completed"`
	Failed    int     `json:"failed"`
	Limited   int     `json:"limited"`
	AverageMS float64 `json:"average_ms"`
}
type callLogPage struct {
	Entries    []callEntry     `json:"entries"`
	NextBefore uint64          `json:"next_before"`
	APIs       []callLogChoice `json:"apis"`
	Models     []string        `json:"models"`
	Summary    callLogSummary  `json:"summary"`
}

func (s *callStore) page(query callLogQuery) callLogPage {
	out := callLogPage{Entries: make([]callEntry, 0, callLogPageSize), APIs: []callLogChoice{}, Models: []string{}}
	seenAPIs, seenModels := make(map[string]bool), make(map[string]bool)
	s.mu.Lock()
	defer s.mu.Unlock()
	out.Summary.Active = s.active
	var duration float64
	var finished int
	for i := 0; i < callLogCapacity && uint64(i) < s.next; i++ {
		id := s.next - uint64(i)
		entry := s.entries[(id-1)%callLogCapacity]
		if entry.ID != id {
			continue
		}
		out.Summary.Retained++
		if !seenAPIs[entry.APIID] {
			seenAPIs[entry.APIID] = true
			out.APIs = append(out.APIs, callLogChoice{entry.APIID, entry.APIName + " · " + entry.APIPath})
		}
		if entry.Model != "" && !seenModels[entry.Model] {
			seenModels[entry.Model] = true
			out.Models = append(out.Models, entry.Model)
		}
		if entry.Result == "completed" {
			out.Summary.Completed++
		}
		if entry.Result == "failed" || entry.Result == "incomplete" || entry.Result == "interrupted" {
			out.Summary.Failed++
		}
		if entry.Status == 429 || entry.ErrorClass == "rate_limit_exceeded" || entry.ErrorClass == "rate_limited" {
			out.Summary.Limited++
		}
		if entry.Result != "running" {
			duration += entry.DurationMS
			finished++
		}
		if query.Before != 0 && id > query.Before || query.API != "" && query.API != entry.APIID || query.Model != "" && query.Model != entry.Model || query.Result != "" && query.Result != entry.Result {
			continue
		}
		if len(out.Entries) == callLogPageSize {
			out.NextBefore = out.Entries[len(out.Entries)-1].ID - 1
			continue
		}
		if entry.Result == "running" {
			entry.DurationMS = float64(time.Since(entry.Time)) / float64(time.Millisecond)
		}
		out.Entries = append(out.Entries, entry)
	}
	out.Summary.Finished = finished
	if finished > 0 {
		out.Summary.AverageMS = duration / float64(finished)
	}
	return out
}

func decodeCallLogQuery(body []byte) (callLogQuery, bool) {
	var query callLogQuery
	if len(body) > 4096 || json.Unmarshal(body, &query) != nil || len(query.API) > 256 || len(query.Model) > 256 {
		return query, false
	}
	switch query.Result {
	case "", "running", "completed", "failed", "incomplete", "interrupted", "unconfirmed":
		return query, true
	default:
		return query, false
	}
}
