package adapter

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"
)

func TestRestoreJSONPreservesOriginalRepresentation(t *testing.T) {
	mapping := Model{ID: "work", Target: "account-a/gpt"}
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "top and nested model",
			body: " {\n\t\"response\": { \"model\" : \"account-a/gpt\", \"id\":\"resp_123\" },\n\t\"model\" : \"account-a/gpt\", \"n\":9007199254740993, \"delta\":\"account-a/gpt\", \"raw\":1.2300e+04 } \n",
			want: " {\n\t\"response\": { \"model\" : \"work\", \"id\":\"resp_123\" },\n\t\"model\" : \"work\", \"n\":9007199254740993, \"delta\":\"account-a/gpt\", \"raw\":1.2300e+04 } \n",
		},
		{
			name: "model objects",
			body: `{"object":"model", "id":"account-a/gpt", "model":"account-a/gpt", "response":{"object":"model","id":"account-a/gpt"}, "items":[{"model":"account-a/gpt"}]}`,
			want: `{"object":"model", "id":"work", "model":"account-a/gpt", "response":{"object":"model","id":"work"}, "items":[{"model":"account-a/gpt"}]}`,
		},
		{
			name: "escaped routing strings",
			body: `{"\u006dodel":"account-a\/gpt", "response":{"mo\u0064el":"account-a\u002fgpt"},"delta":"account-a\/gpt"}`,
			want: `{"\u006dodel":"work", "response":{"mo\u0064el":"work"},"delta":"account-a\/gpt"}`,
		},
		{
			name: "duplicate decoded key",
			body: `{"model":"other", "\u006dodel":"account-a/gpt"}`,
			want: `{"model":"other", "\u006dodel":"account-a/gpt"}`,
		},
		{
			name: "duplicate nested key",
			body: `{"response":{"model":"other", "model":"account-a/gpt"}}`,
			want: `{"response":{"model":"other", "model":"account-a/gpt"}}`,
		},
		{
			name: "invalid complete document",
			body: `{"model":"account-a/gpt"} trailing`,
			want: `{"model":"account-a/gpt"} trailing`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(test.body)
			got := restoreJSON(body, mapping)
			if string(got) != test.want {
				t.Fatalf("restored JSON = %s; want %s", got, test.want)
			}
			if string(body) != test.body {
				t.Fatal("restoration mutated the original input buffer")
			}
		})
	}
}

func TestRestoreJSONDeltaKeepsOriginalBuffer(t *testing.T) {
	body := []byte(`{"type":"response.output_text.delta","delta":"` + strings.Repeat("account-a/gpt ", 8192) + `","n":9007199254740993}`)
	got := restoreJSON(body, Model{ID: "work", Target: "account-a/gpt"})
	if len(got) != len(body) || &got[0] != &body[0] {
		t.Fatal("a delta without model copied or changed its input buffer")
	}
	if allocs := testing.AllocsPerRun(20, func() {
		restoreJSON(body, Model{ID: "work", Target: "account-a/gpt"})
	}); allocs != 0 {
		t.Fatalf("a delta without model allocated %.1f times per restoration", allocs)
	}
	for _, mapping := range []Model{{Target: "account-a/gpt"}, {ID: "account-a/gpt", Target: "account-a/gpt"}} {
		body := []byte(`{"model":"account-a/gpt"}`)
		got := restoreJSON(body, mapping)
		if len(got) != len(body) || &got[0] != &body[0] || !bytes.Equal(got, body) {
			t.Fatal("an empty or identity mapping modified its response")
		}
	}
}

func TestRestoreSSEMultilineCRLFAndUnterminatedEvent(t *testing.T) {
	body := ": account-a/gpt comment\r\nevent: response.completed\r\nid: 42\r\nretry: 1500\r\ndata: { \"model\" : \"account-a/gpt\",\r\ndata:\"response\":{\"model\":\"account-a/gpt\"}, \"n\":9007199254740993,\r\ndata: \"delta\":\"account-a/gpt remains user text\" }\r\n\r\ndata: [DONE]\n\nevent: response.output_text.delta\ndata: {\"model\":\"account-a/gpt\",\"delta\":\"account-a/gpt\"}"
	want := ": account-a/gpt comment\r\nevent: response.completed\r\nid: 42\r\nretry: 1500\r\ndata: { \"model\" : \"work\",\r\ndata:\"response\":{\"model\":\"work\"}, \"n\":9007199254740993,\r\ndata: \"delta\":\"account-a/gpt remains user text\" }\r\n\r\ndata: [DONE]\n\nevent: response.output_text.delta\ndata: {\"model\":\"work\",\"delta\":\"account-a/gpt\"}"
	resp := &http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}
	if err := restoreResponse(resp, Model{ID: "work", Target: "account-a/gpt"}, 4096); err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil || string(got) != want {
		t.Fatalf("restored SSE = %q, %v; want %q", got, err, want)
	}
	if resp.ContentLength != -1 || resp.Header.Get("Content-Length") != "" {
		t.Fatal("stream retained an obsolete content length")
	}
}

func TestRestoreSSEHTTPFramesCompleteEventsPromptly(t *testing.T) {
	completeFirst := make(chan struct{})
	releaseSecond := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\r\n")
		w.(http.Flusher).Flush()
		select {
		case <-completeFirst:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "data: {\"model\":\"account-a/gpt\"}\r\n\r\n")
		w.(http.Flusher).Flush()
		select {
		case <-releaseSecond:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(upstreamURL)
	proxy.FlushInterval = -1
	proxy.ModifyResponse = func(resp *http.Response) error {
		return restoreResponse(resp, Model{ID: "work", Target: "account-a/gpt"}, 4096)
	}
	downstream := httptest.NewServer(proxy)
	defer downstream.Close()
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(downstream.URL, "http://"), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := fmt.Fprintf(conn, "GET /stream HTTP/1.1\r\nHost: adapter\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, " 200 ") {
		t.Fatalf("HTTP status = %q, %v", status, err)
	}
	headers, err := textproto.NewReader(reader).ReadMIMEHeader()
	if err != nil || headers.Get("Transfer-Encoding") != "chunked" {
		t.Fatalf("stream headers = %v, %v", headers, err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	partial, err := reader.ReadString('\n')
	var timeout net.Error
	if partial != "" || !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("an unfinished event was flushed as an HTTP frame: %q, %v", partial, err)
	}
	close(completeFirst)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	first := readResponseRegressionChunk(t, reader)
	if want := "event: response.completed\r\ndata: {\"model\":\"work\"}\r\n\r\n"; string(first) != want {
		t.Fatalf("first HTTP frame = %q; want one whole event %q", first, want)
	}
	// The upstream is still waiting to send its next event. Receiving the
	// first frame above proves events are not batched behind later output.
	close(releaseSecond)
	second := readResponseRegressionChunk(t, reader)
	if string(second) != "data: [DONE]\n\n" {
		t.Fatalf("second HTTP frame = %q", second)
	}
}

func readResponseRegressionChunk(t *testing.T, reader *bufio.Reader) []byte {
	t.Helper()
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read HTTP chunk length: %v", err)
	}
	size, err := strconv.ParseUint(strings.TrimSpace(line), 16, 32)
	if err != nil || size == 0 {
		t.Fatalf("HTTP chunk length = %q, %v", line, err)
	}
	frame := make([]byte, int(size)+2)
	if _, err := io.ReadFull(reader, frame); err != nil {
		t.Fatalf("read HTTP chunk: %v", err)
	}
	if !bytes.Equal(frame[len(frame)-2:], []byte("\r\n")) {
		t.Fatalf("invalid HTTP chunk terminator: %q", frame)
	}
	return frame[:len(frame)-2]
}

func TestResponseJSONSizeLimitsAndSourceClosure(t *testing.T) {
	for _, encoding := range []string{"identity", "gzip"} {
		t.Run(encoding, func(t *testing.T) {
			body := []byte(`{"model":"account-a/gpt","delta":"` + strings.Repeat("a", 256) + `"}`)
			if encoding == "gzip" {
				body = responseRegressionGzip(t, body)
			}
			original := &responseRegressionBody{Reader: bytes.NewReader(body)}
			resp := &http.Response{Header: http.Header{"Content-Encoding": {encoding}}, Body: original, ContentLength: -1}
			if _, err := readResponseJSON(resp, 64); err == nil || !strings.Contains(err.Error(), "size limit") {
				t.Fatalf("oversized %s JSON did not fail: %v", encoding, err)
			}
			if original.closes.Load() != 1 {
				t.Fatal("JSON source was not closed exactly once")
			}
		})
	}
	reader := strings.NewReader(strings.Repeat("a", 256))
	original := &responseRegressionBody{Reader: reader}
	resp := &http.Response{Header: make(http.Header), Body: original, ContentLength: 256}
	if _, err := readResponseJSON(resp, 64); err == nil || reader.Len() != 256 || original.closes.Load() != 1 {
		t.Fatalf("declared oversized JSON was read or left open: %v", err)
	}
}

func TestReadResponseJSONExactLengthAndUnknownCompatibility(t *testing.T) {
	body := []byte(`{"model":"account-a/gpt","delta":"` + strings.Repeat("x", 128<<10) + `"}`)
	for _, test := range []struct {
		name   string
		length int64
		fail   string
	}{
		{"known exact", int64(len(body)), ""},
		{"known short", int64(len(body)) + 1, "shorter than Content-Length"},
		{"known long", int64(len(body)) - 1, "longer than Content-Length"},
		{"unknown", -1, ""},
		{"default zero", 0, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := &responseRegressionBody{Reader: bytes.NewReader(body)}
			resp := &http.Response{Header: make(http.Header), Body: original, ContentLength: test.length}
			got, err := readResponseJSON(resp, int64(len(body))+1024)
			if original.closes.Load() != 1 {
				t.Fatal("response source was not closed exactly once")
			}
			if test.fail != "" {
				if err == nil || !strings.Contains(err.Error(), test.fail) {
					t.Fatalf("length mismatch error = %v; want %q", err, test.fail)
				}
				result := httptest.NewRecorder()
				writeError(result, err)
				if result.Code != http.StatusBadGateway {
					t.Fatalf("upstream length failure returned HTTP %d", result.Code)
				}
				return
			}
			if err != nil || !bytes.Equal(got, body) {
				t.Fatalf("response body mismatch: %v", err)
			}
		})
	}
}

func TestReadResponseJSONKeepsTerminalReadFailure(t *testing.T) {
	body := `{"model":"account-a/gpt"}`
	reader := iotest.DataErrReader(&responseRegressionFailReader{Reader: strings.NewReader(body)})
	original := &responseRegressionBody{Reader: reader}
	resp := &http.Response{Header: make(http.Header), Body: original, ContentLength: int64(len(body))}
	if _, err := readResponseJSON(resp, 4096); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("a terminal error accompanying the final declared bytes was lost: %v", err)
	}
	if original.closes.Load() != 1 {
		t.Fatal("failed known-length source was not closed exactly once")
	}
}

func TestReadResponseJSONRejectsTruncatedHTTPBody(t *testing.T) {
	body := `{"model":"account-a/gpt"}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)+10))
		_, _ = io.WriteString(w, body)
	}))
	defer upstream.Close()
	resp, err := upstream.Client().Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readResponseJSON(resp, 4096); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated HTTP response was accepted: %v", err)
	}
}

func TestRestoreSSELimitsUnbrokenLinesAndWholeEvents(t *testing.T) {
	for _, body := range []string{
		"data: " + strings.Repeat("a", 8192),
		strings.Repeat(": comment\n", 20) + "\n",
	} {
		original := &responseRegressionBody{Reader: strings.NewReader(body)}
		resp := &http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: original, ContentLength: -1}
		if err := restoreResponse(resp, Model{ID: "work", Target: "account-a/gpt"}, 64); err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err == nil || !strings.Contains(err.Error(), "size limit") || len(got) != 0 {
			t.Fatalf("oversized SSE event = %q, %v", got, err)
		}
		if original.closes.Load() != 1 {
			t.Fatal("oversized SSE source was not closed exactly once")
		}
	}
}

func TestRestoreSSEPropagatesUpstreamFailure(t *testing.T) {
	original := &responseRegressionBody{Reader: &responseRegressionFailReader{Reader: strings.NewReader("data: {\"model\":\"account-a/gpt\"}\n\n")}}
	resp := &http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: original, ContentLength: -1}
	if err := restoreResponse(resp, Model{ID: "work", Target: "account-a/gpt"}, 4096); err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if string(got) != "data: {\"model\":\"work\"}\n\n" || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("upstream stream failure was lost: %q, %v", got, err)
	}
}

func TestRestoreSSECloseInterruptsBlockedUpstream(t *testing.T) {
	original := &responseRegressionBlockingBody{entered: make(chan struct{}), closed: make(chan struct{}), returned: make(chan struct{})}
	t.Cleanup(func() { _ = original.Close() })
	resp := &http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: original, ContentLength: -1}
	if err := restoreResponse(resp, Model{ID: "work", Target: "account-a/gpt"}, 4096); err != nil {
		t.Fatal(err)
	}
	readDone := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(resp.Body)
		readDone <- err
	}()
	select {
	case <-original.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream Read did not start")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- resp.Body.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("closing the downstream body blocked")
	}
	select {
	case <-original.returned:
	case <-time.After(3 * time.Second):
		t.Fatal("downstream close left the upstream Read blocked")
	}
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("a canceled downstream read reported success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("downstream read did not terminate")
	}
	_ = resp.Body.Close()
	if original.closes.Load() != 1 {
		t.Fatal("repeated downstream close closed the original source multiple times")
	}
}

func TestRestoreGzipJSONAndSSE(t *testing.T) {
	for _, test := range []struct {
		name        string
		contentType string
		body        string
		want        string
	}{
		{"JSON", "application/json", ` {"model":"account-a/gpt", "n":9007199254740993,"delta":"account-a/gpt"} `, ` {"model":"work", "n":9007199254740993,"delta":"account-a/gpt"} `},
		{"SSE", "text/event-stream", "data: {\"model\":\"account-a/gpt\",\n" + "data: \"delta\":\"account-a/gpt\"}\n\n", "data: {\"model\":\"work\",\n" + "data: \"delta\":\"account-a/gpt\"}\n\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := responseRegressionGzip(t, []byte(test.body))
			original := &responseRegressionBody{Reader: bytes.NewReader(body)}
			resp := &http.Response{Header: http.Header{
				"Content-Type": {test.contentType}, "Content-Encoding": {"gzip"}, "Content-Length": {strconv.Itoa(len(body))},
				"Etag": {"old"}, "Digest": {"sha-256=old"}, "Content-Digest": {"sha-256=:old:"}, "Content-Md5": {"old"},
			}, Body: original, ContentLength: int64(len(body))}
			if err := restoreResponse(resp, Model{ID: "work", Target: "account-a/gpt"}, 4096); err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil || string(got) != test.want {
				t.Fatalf("gzip %s response = %q, %v", test.name, got, err)
			}
			for _, name := range []string{"Content-Encoding", "ETag", "Digest", "Content-Digest", "Content-MD5"} {
				if resp.Header.Get(name) != "" {
					t.Fatalf("rewritten gzip response retained %s", name)
				}
			}
			if test.name == "JSON" && (resp.ContentLength != int64(len(got)) || resp.Header.Get("Content-Length") != strconv.Itoa(len(got))) {
				t.Fatal("rewritten JSON has an incorrect content length")
			}
			if original.closes.Load() != 1 {
				t.Fatal("gzip source was not closed exactly once")
			}
		})
	}
}

func TestRestoreResponseRejectsUnsupportedEncoding(t *testing.T) {
	for _, contentType := range []string{"application/json", "text/event-stream"} {
		original := &responseRegressionBody{Reader: strings.NewReader(`{"model":"account-a/gpt"}`)}
		resp := &http.Response{Header: http.Header{"Content-Type": {contentType}, "Content-Encoding": {"br"}}, Body: original}
		if err := restoreResponse(resp, Model{ID: "work", Target: "account-a/gpt"}, 4096); err == nil || !strings.Contains(err.Error(), "encoding") {
			t.Fatalf("unsupported %s encoding was silently forwarded: %v", contentType, err)
		}
		if original.closes.Load() != 1 {
			t.Fatal("rejected encoded response was left open")
		}
	}
}

func TestSetJSONResponseRemovesInvalidMetadata(t *testing.T) {
	resp := &http.Response{Header: make(http.Header), Trailer: make(http.Header), TransferEncoding: []string{"chunked"}}
	for _, name := range []string{"ETag", "Last-Modified", "Content-MD5", "Digest", "Content-Digest", "Repr-Digest", "Content-Encoding", "Content-Range", "Accept-Ranges", "Transfer-Encoding", "Trailer"} {
		resp.Header.Set(name, "obsolete")
		resp.Trailer.Set(name, "obsolete")
	}
	resp.Trailer.Set("X-Trace", "keep")
	setJSONResponse(resp, []byte(`{"model":"work"}`))
	for _, name := range []string{"ETag", "Last-Modified", "Content-MD5", "Digest", "Content-Digest", "Repr-Digest", "Content-Encoding", "Content-Range", "Accept-Ranges", "Transfer-Encoding", "Trailer"} {
		if resp.Header.Get(name) != "" || resp.Trailer.Get(name) != "" {
			t.Fatalf("rewritten JSON retained invalid %s metadata", name)
		}
	}
	if resp.Trailer.Get("X-Trace") != "keep" || len(resp.TransferEncoding) != 0 || resp.ContentLength != 16 || resp.Header.Get("Content-Length") != "16" {
		t.Fatalf("replacement response metadata = %+v", resp)
	}
	_ = resp.Body.Close()
}

func responseRegressionGzip(t *testing.T, body []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := gzip.NewWriter(&output)
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

type responseRegressionBody struct {
	io.Reader
	closes atomic.Int32
}

func (body *responseRegressionBody) Close() error {
	body.closes.Add(1)
	return nil
}

type responseRegressionFailReader struct {
	io.Reader
}

func (reader *responseRegressionFailReader) Read(p []byte) (int, error) {
	n, err := reader.Reader.Read(p)
	if err == io.EOF {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

type responseRegressionBlockingBody struct {
	entered    chan struct{}
	closed     chan struct{}
	returned   chan struct{}
	enterOnce  sync.Once
	returnOnce sync.Once
	closeOnce  sync.Once
	closes     atomic.Int32
}

func (body *responseRegressionBlockingBody) Read([]byte) (int, error) {
	body.enterOnce.Do(func() { close(body.entered) })
	<-body.closed
	body.returnOnce.Do(func() { close(body.returned) })
	return 0, io.ErrClosedPipe
}

func (body *responseRegressionBlockingBody) Close() error {
	body.closes.Add(1)
	body.closeOnce.Do(func() { close(body.closed) })
	return nil
}
