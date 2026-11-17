package adapter

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
)

func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cfg, generation, ok := m.begin()
	if !ok {
		writeError(w, &apiError{http.StatusServiceUnavailable, "适配器已停止"})
		return
	}
	defer m.active.Done()
	base := cfg.BasePath()
	switch {
	case r.Method == http.MethodGet && (r.URL.Path == base+"/codex/models" || r.URL.Path == base+"/models"):
		c, err := fetchCatalog(r.Context(), cfg, r.Header, r.URL.RawQuery)
		if err != nil {
			writeError(w, err)
			return
		}
		body, _, err := c.render(cfg)
		if err != nil {
			writeError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(body)
	case r.Method == http.MethodGet && r.URL.Path == base+"/codex/responses":
		m.proxyWebSocket(w, r, cfg, generation)
	case r.Method == http.MethodPost && (r.URL.Path == base+"/codex/responses" || r.URL.Path == base+"/codex/responses/compact"):
		m.proxyResponse(w, r, cfg)
	default:
		http.NotFound(w, r)
	}
}

func (m *Manager) proxyResponse(w http.ResponseWriter, r *http.Request, cfg Config) {
	limit := int64(cfg.MaxBodyMB) << 20
	if r.ContentLength > limit {
		writeError(w, &apiError{http.StatusRequestEntityTooLarge, "请求体超过配置的大小限制"})
		return
	}
	var mapped Model
	var changed bool
	if len(cfg.Mappings) != 0 {
		body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
		if err != nil {
			writeError(w, &apiError{http.StatusBadRequest, "请求体读取失败"})
			return
		}
		if int64(len(body)) > limit {
			writeError(w, &apiError{http.StatusRequestEntityTooLarge, "请求体超过配置的大小限制"})
			return
		}
		body, mapped, changed, err = rewriteRequest(r, cfg, body)
		if err != nil {
			writeError(w, err)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Del("Content-Length")
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
	}
	upstreamPath := strings.TrimPrefix(r.URL.Path, cfg.BasePath())
	proxy := &httputil.ReverseProxy{
		Transport:     localTransport,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			u := targetURL(cfg, "/backend-api"+upstreamPath, pr.In.URL.RawQuery)
			pr.SetURL(u)
			pr.Out.URL.Path = u.Path
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = u.RawQuery
			if changed {
				pr.Out.Header.Set("Accept-Encoding", "identity")
			}
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			writeError(w, &apiError{http.StatusBadGateway, "无法连接 CPA 推理接口"})
		},
	}
	if changed {
		proxy.ModifyResponse = func(resp *http.Response) error { return restoreResponse(resp, mapped) }
	}
	proxy.ServeHTTP(w, r)
}

func rewriteRequest(r *http.Request, cfg Config, body []byte) ([]byte, Model, bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return body, Model{}, false, nil
	}
	object := make(map[string]json.RawMessage)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return body, Model{}, false, nil
		}
		key, ok := keyToken.(string)
		if !ok {
			return body, Model{}, false, nil
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return body, Model{}, false, nil
		}
		if _, exists := object[key]; exists {
			return nil, Model{}, false, &apiError{http.StatusBadRequest, "请求包含重复的 JSON 字段"}
		}
		object[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return body, Model{}, false, nil
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return body, Model{}, false, nil
	}
	var id string
	if json.Unmarshal(object["model"], &id) != nil {
		return body, Model{}, false, nil
	}
	mapping, ok := cfg.Mapping(id)
	if !ok {
		return body, Model{}, false, nil
	}
	if mapping.ID == mapping.Target {
		return body, mapping, false, nil
	}
	c, err := fetchCatalog(r.Context(), cfg, r.Header, r.URL.RawQuery)
	if err != nil {
		return nil, Model{}, false, err
	}
	if err := c.validateMapping(mapping); err != nil {
		return nil, Model{}, false, err
	}
	object["model"], _ = json.Marshal(mapping.Target)
	updated, err := json.Marshal(object)
	if err != nil {
		return nil, Model{}, false, &apiError{http.StatusBadRequest, "无法改写模型 ID"}
	}
	return updated, mapping, true, nil
}

func restoreJSON(body []byte, mapping Model) []byte {
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil || object == nil {
		return body
	}
	changed := replaceModel(object, mapping)
	var nested map[string]json.RawMessage
	if json.Unmarshal(object["response"], &nested) == nil && nested != nil && replaceModel(nested, mapping) {
		object["response"], _ = json.Marshal(nested)
		changed = true
	}
	if !changed {
		return body
	}
	updated, err := json.Marshal(object)
	if err != nil {
		return body
	}
	return updated
}

func replaceModel(object map[string]json.RawMessage, mapping Model) bool {
	var model string
	if json.Unmarshal(object["model"], &model) != nil || model != mapping.Target {
		return false
	}
	object["model"], _ = json.Marshal(mapping.ID)
	return true
}

func restoreResponse(resp *http.Response, mapping Model) error {
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return nil
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(contentType, "text/event-stream") {
		original := resp.Body
		reader, writer := io.Pipe()
		resp.Body = reader
		resp.ContentLength = -1
		resp.Header.Del("Content-Length")
		resp.Header.Del("ETag")
		go func() {
			defer original.Close()
			defer writer.Close()
			lines := bufio.NewReader(original)
			for {
				line, err := lines.ReadBytes('\n')
				if len(line) > 0 {
					line = restoreSSELine(line, mapping)
					if _, writeErr := writer.Write(line); writeErr != nil {
						return
					}
				}
				if err != nil {
					if err != io.EOF {
						_ = writer.CloseWithError(err)
					}
					return
				}
			}
		}()
	} else if strings.Contains(contentType, "application/json") {
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		body = restoreJSON(body, mapping)
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
		resp.Header.Del("ETag")
	}
	return nil
}

func restoreSSELine(line []byte, mapping Model) []byte {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return line
	}
	prefix := 5
	if len(line) > prefix && line[prefix] == ' ' {
		prefix++
	}
	end := len(line)
	for end > prefix && (line[end-1] == '\n' || line[end-1] == '\r') {
		end--
	}
	payload := restoreJSON(line[prefix:end], mapping)
	if bytes.Equal(payload, line[prefix:end]) {
		return line
	}
	out := make([]byte, 0, prefix+len(payload)+len(line)-end)
	out = append(out, line[:prefix]...)
	out = append(out, payload...)
	return append(out, line[end:]...)
}

func mappingErrorEvent(err error) []byte {
	status := http.StatusBadGateway
	message := "模型路由失败"
	if ae, ok := err.(*apiError); ok {
		status = ae.Status
		message = ae.Message
	}
	body, _ := json.Marshal(map[string]any{"type": "error", "status": status, "error": map[string]string{"type": "adapter_error", "message": message}})
	return body
}
