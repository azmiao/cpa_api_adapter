package adapter

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

func restoreJSON(body []byte, mapping Model) []byte {
	spans, count := responseJSONSpans(body, mapping)
	if count == 0 {
		return body
	}
	replacement, _ := json.Marshal(mapping.ID)
	return replaceResponseSpans(body, spans[:count], replacement)
}

type responseFields struct {
	object   jsonSpan
	model    jsonSpan
	id       jsonSpan
	response jsonSpan
}

// Inspect only routing fields before allocating the duplicate-key maps. Most
// streaming deltas have no model and keep their original backing buffer.
func responseJSONSpans(body []byte, mapping Model) ([2]jsonSpan, int) {
	var spans [2]jsonSpan
	if mapping.ID == "" || mapping.ID == mapping.Target || !json.Valid(body) {
		return spans, 0
	}
	fields := scanResponseFields(body)
	_, topChanged := responseModelSpan(body, fields, mapping.Target)
	var nestedChanged bool
	if fields.response.end != 0 {
		nested := body[fields.response.start:fields.response.end]
		_, nestedChanged = responseModelSpan(nested, scanResponseFields(nested), mapping.Target)
	}
	if !topChanged && !nestedChanged {
		return spans, 0
	}
	validated, err := jsonObjectFields(body)
	if err != nil {
		return spans, 0
	}
	fields = responseFieldsFromMap(validated)
	count := 0
	if topChanged {
		if span, changed := responseModelSpan(body, fields, mapping.Target); changed {
			spans[count] = span
			count++
		}
	}
	if nestedChanged {
		nested := body[fields.response.start:fields.response.end]
		validated, err := jsonObjectFields(nested)
		if err != nil {
			return spans, 0
		}
		if span, changed := responseModelSpan(nested, responseFieldsFromMap(validated), mapping.Target); changed {
			span.start += fields.response.start
			span.end += fields.response.start
			spans[count] = span
			count++
		}
	}
	if count == 2 && spans[0].start > spans[1].start {
		spans[0], spans[1] = spans[1], spans[0]
	}
	return spans, count
}

func responseFieldsFromMap(fields map[string]jsonSpan) responseFields {
	return responseFields{object: fields["object"], model: fields["model"], id: fields["id"], response: fields["response"]}
}

func responseModelSpan(body []byte, fields responseFields, target string) (jsonSpan, bool) {
	span := fields.model
	if responseStringIs(body[fields.object.start:fields.object.end], "model") {
		span = fields.id
	}
	return span, responseStringIs(body[span.start:span.end], target)
}

func responseStringIs(value []byte, expected string) bool {
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return false
	}
	if len(value) == len(expected)+2 && string(value[1:len(value)-1]) == expected {
		return true
	}
	if bytes.IndexByte(value, '\\') < 0 {
		return false
	}
	var decoded string
	return json.Unmarshal(value, &decoded) == nil && decoded == expected
}

// All callers validate the complete JSON first, so values can be skipped
// without decoding or copying large strings, arrays, and nested objects.
func scanResponseFields(body []byte) responseFields {
	var fields responseFields
	at := skipJSONSpace(body, 0)
	if at == len(body) || body[at] != '{' {
		return fields
	}
	at++
	for {
		at = skipJSONSpace(body, at)
		if body[at] == '}' {
			return fields
		}
		keyStart := at
		keyEnd := skipJSONString(body, at)
		at = skipJSONSpace(body, keyEnd) + 1 // Colon.
		at = skipJSONSpace(body, at)
		span := jsonSpan{start: at, end: skipJSONValue(body, at)}
		key := body[keyStart+1 : keyEnd-1]
		switch string(key) {
		case "model":
			fields.model = span
		case "object":
			fields.object = span
		case "id":
			fields.id = span
		case "response":
			fields.response = span
		default:
			if bytes.IndexByte(key, '\\') >= 0 {
				var decoded string
				if json.Unmarshal(body[keyStart:keyEnd], &decoded) == nil {
					switch decoded {
					case "model":
						fields.model = span
					case "object":
						fields.object = span
					case "id":
						fields.id = span
					case "response":
						fields.response = span
					}
				}
			}
		}
		at = skipJSONSpace(body, span.end)
		if body[at] == '}' {
			return fields
		}
		at++ // Comma.
	}
}

func replaceResponseSpans(body []byte, spans []jsonSpan, replacement []byte) []byte {
	size := len(body)
	for _, span := range spans {
		size += len(replacement) - (span.end - span.start)
	}
	updated := make([]byte, 0, size)
	at := 0
	for _, span := range spans {
		updated = append(updated, body[at:span.start]...)
		updated = append(updated, replacement...)
		at = span.end
	}
	return append(updated, body[at:]...)
}

type responseSource struct {
	reader   io.Reader
	original io.ReadCloser
	gzip     *gzip.Reader
	close    sync.Once
	closeErr error
}

func responseContentEncoding(resp *http.Response) (string, error) {
	encoding := strings.TrimSpace(strings.Join(resp.Header.Values("Content-Encoding"), ","))
	switch {
	case encoding == "", strings.EqualFold(encoding, "identity"):
		return "identity", nil
	case strings.EqualFold(encoding, "gzip"):
		return "gzip", nil
	default:
		return "", fmt.Errorf("unsupported upstream content encoding %q", encoding)
	}
}

func openResponseSource(resp *http.Response) (*responseSource, error) {
	if resp.Body == nil {
		return nil, fmt.Errorf("upstream response has no body")
	}
	source := &responseSource{reader: resp.Body, original: resp.Body}
	encoding, err := responseContentEncoding(resp)
	if err != nil {
		_ = source.closeOriginal()
		return nil, err
	}
	if encoding == "gzip" {
		decoder, err := gzip.NewReader(resp.Body)
		if err != nil {
			_ = source.closeOriginal()
			return nil, fmt.Errorf("decode upstream gzip response: %w", err)
		}
		source.reader, source.gzip = decoder, decoder
		resp.Uncompressed = true
	}
	return source, nil
}

func (source *responseSource) closeOriginal() error {
	source.close.Do(func() { source.closeErr = source.original.Close() })
	return source.closeErr
}

func (source *responseSource) closeDecoder() {
	if source.gzip != nil {
		_ = source.gzip.Close()
	}
}

func readResponseJSON(resp *http.Response, limit int64) ([]byte, error) {
	source, err := openResponseSource(resp)
	if err != nil {
		return nil, err
	}
	defer source.closeOriginal()
	defer source.closeDecoder()
	if limit < 0 {
		return nil, fmt.Errorf("invalid upstream JSON size limit")
	}
	if source.gzip == nil && resp.ContentLength > limit {
		return nil, fmt.Errorf("upstream JSON exceeds size limit (%d bytes)", limit)
	}
	// Zero is also the default in manually constructed responses; only a
	// positive identity length is authoritative for exact-size allocation.
	if source.gzip == nil && resp.ContentLength > 0 {
		body := make([]byte, int(resp.ContentLength))
		received := 0
		for received < len(body) {
			n, readErr := source.reader.Read(body[received:])
			received += n
			if readErr == io.EOF {
				if received != len(body) {
					return nil, fmt.Errorf("upstream JSON shorter than Content-Length (%d of %d bytes): %w", received, resp.ContentLength, io.ErrUnexpectedEOF)
				}
				return body, nil
			}
			if readErr != nil {
				return nil, fmt.Errorf("read upstream JSON (%d of %d Content-Length bytes): %w", received, resp.ContentLength, readErr)
			}
		}
		var extra [1]byte
		if n, readErr := io.ReadFull(source.reader, extra[:]); n > 0 {
			return nil, fmt.Errorf("upstream JSON longer than Content-Length (%d bytes)", resp.ContentLength)
		} else if readErr != io.EOF {
			return nil, fmt.Errorf("read upstream JSON: %w", readErr)
		}
		return body, nil
	}
	readLimit := limit
	if readLimit < (1<<63)-1 {
		readLimit++
	}
	body, err := io.ReadAll(io.LimitReader(source.reader, readLimit))
	if err != nil {
		return nil, fmt.Errorf("read upstream JSON: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("upstream JSON exceeds size limit (%d bytes)", limit)
	}
	return body, nil
}

var invalidResponseMetadata = [...]string{
	"Content-Length", "Content-Encoding", "ETag", "Last-Modified", "Content-MD5", "Digest",
	"Content-Digest", "Repr-Digest", "Content-Range", "Accept-Ranges",
	"Transfer-Encoding", "Trailer",
}

func clearResponseMetadata(resp *http.Response) {
	for _, name := range invalidResponseMetadata {
		resp.Header.Del(name)
		resp.Trailer.Del(name)
	}
	resp.TransferEncoding = nil
}

func setJSONResponse(resp *http.Response, body []byte) {
	if resp.Header == nil {
		resp.Header = make(http.Header)
	}
	clearResponseMetadata(resp)
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
}

func restoreResponse(resp *http.Response, mapping Model, limit int64) error {
	if _, err := responseContentEncoding(resp); err != nil {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		return err
	}
	contentType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch {
	case contentType == "text/event-stream":
		if limit < 0 {
			if resp.Body != nil {
				_ = resp.Body.Close()
			}
			return fmt.Errorf("invalid upstream SSE event size limit")
		}
		source, err := openResponseSource(resp)
		if err != nil {
			return err
		}
		reader, writer := io.Pipe()
		resp.Body = &restoredSSEBody{reader: reader, writer: writer, source: source, response: resp, mapping: mapping, limit: limit}
		clearResponseMetadata(resp)
		resp.ContentLength = -1
	case contentType == "application/json", strings.HasSuffix(contentType, "+json"):
		body, err := readResponseJSON(resp, limit)
		if err != nil {
			return err
		}
		setJSONResponse(resp, restoreJSON(body, mapping))
	}
	return nil
}

type restoredSSEBody struct {
	reader   *io.PipeReader
	writer   *io.PipeWriter
	source   *responseSource
	response *http.Response
	mapping  Model
	limit    int64
	start    sync.Once
}

func (body *restoredSSEBody) Read(p []byte) (int, error) {
	body.start.Do(func() { go body.transform() })
	return body.reader.Read(p)
}

func (body *restoredSSEBody) Close() error {
	_ = body.reader.Close()
	err := body.source.closeOriginal()
	body.start.Do(func() {
		body.source.closeDecoder()
		_ = body.writer.Close()
	})
	return err
}

func (body *restoredSSEBody) transform() {
	var streamErr error
	defer func() {
		_ = body.source.closeOriginal()
		body.source.closeDecoder()
		_ = body.writer.CloseWithError(streamErr)
	}()
	// Start on the first downstream Read, after the proxy has inspected headers
	// and declared trailers. Closing the downstream also interrupts a blocked
	// upstream Read, rather than merely closing the pipe's consumer.
	lines := bufio.NewReader(body.source.reader)
	var event []byte
	lineStart := 0
	for {
		fragment, err := lines.ReadSlice('\n')
		if int64(len(fragment)) > body.limit-int64(len(event)) {
			streamErr = fmt.Errorf("upstream SSE event exceeds size limit (%d bytes)", body.limit)
			return
		}
		event = append(event, fragment...)
		if err == nil {
			line := event[lineStart:]
			blank := len(line) == 1 || len(line) == 2 && line[0] == '\r'
			if blank {
				if _, err := body.writer.Write(restoreSSEEvent(event, body.mapping)); err != nil {
					streamErr = err
					return
				}
				event = event[:0]
			}
			lineStart = len(event)
			continue
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != io.EOF {
			streamErr = err
			return
		}
		if len(event) != 0 {
			if _, err := body.writer.Write(restoreSSEEvent(event, body.mapping)); err != nil {
				streamErr = err
				return
			}
		}
		// The original body may have populated integrity trailers at EOF.
		clearResponseMetadata(body.response)
		return
	}
}

func restoreSSEEvent(event []byte, mapping Model) []byte {
	var localSpans [8]jsonSpan
	data := localSpans[:0]
	payloadLength := 0
	for at := 0; at < len(event); {
		end := len(event)
		if newline := bytes.IndexByte(event[at:], '\n'); newline >= 0 {
			end = at + newline + 1
		}
		contentEnd := end
		if contentEnd > at && event[contentEnd-1] == '\n' {
			contentEnd--
		}
		if contentEnd > at && event[contentEnd-1] == '\r' {
			contentEnd--
		}
		line := event[at:contentEnd]
		if bytes.Equal(line, []byte("data")) || bytes.HasPrefix(line, []byte("data:")) {
			start := at + 4
			if start < contentEnd {
				start++ // Colon.
				if start < contentEnd && event[start] == ' ' {
					start++
				}
			}
			data = append(data, jsonSpan{start: start, end: contentEnd})
			payloadLength += contentEnd - start
		}
		at = end
	}
	if len(data) == 0 {
		return event
	}
	payload := event[data[0].start:data[0].end]
	if len(data) > 1 {
		payload = make([]byte, 0, payloadLength+len(data)-1)
		for i, span := range data {
			if i != 0 {
				payload = append(payload, '\n')
			}
			payload = append(payload, event[span.start:span.end]...)
		}
	}
	spans, count := responseJSONSpans(payload, mapping)
	if count == 0 {
		return event
	}
	for i := range count {
		payloadStart := 0
		found := false
		for _, span := range data {
			payloadEnd := payloadStart + span.end - span.start
			if spans[i].start >= payloadStart && spans[i].end <= payloadEnd {
				spans[i].start += span.start - payloadStart
				spans[i].end += span.start - payloadStart
				found = true
				break
			}
			payloadStart = payloadEnd + 1
		}
		if !found {
			return event
		}
	}
	replacement, _ := json.Marshal(mapping.ID)
	return replaceResponseSpans(event, spans[:count], replacement)
}
