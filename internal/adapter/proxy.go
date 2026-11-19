package adapter

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httputil"
	"path"
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
	var api *APIConfig
	for i := range cfg.APIs {
		candidate := &cfg.APIs[i]
		suffix, matches := strings.CutPrefix(r.URL.Path, candidate.Prefix)
		formatPath := "/v1/"
		if candidate.Format == "backend-api" {
			formatPath = "/backend-api/"
		}
		if matches && strings.HasPrefix(suffix, formatPath) && (api == nil || len(candidate.Prefix) > len(api.Prefix)) {
			api = candidate
		}
	}
	if api == nil {
		http.NotFound(w, r)
		return
	}
	if !authenticate(r.Header, api.APIKey) {
		writeError(w, &apiError{http.StatusUnauthorized, "API Key 无效或不属于此路由"})
		return
	}
	if path.Clean(r.URL.Path) != r.URL.Path {
		writeError(w, &apiError{http.StatusBadRequest, "请求路径必须是规范路径"})
		return
	}
	r.Header.Set("Authorization", "Bearer "+api.APIKey)
	r.Header.Del("X-Api-Key")
	r.Header.Del("X-Goog-Api-Key")
	suffix := r.URL.Path[len(api.Prefix)+len(api.Format)+1:]
	switch {
	case r.Method == http.MethodGet && (suffix == "/models" || api.Format == "backend-api" && suffix == "/codex/models"):
		c, err := fetchCatalog(r.Context(), *api, r.Header, r.URL.RawQuery)
		if err != nil {
			writeError(w, err)
			return
		}
		body, _, err := c.render(*api)
		if err != nil {
			writeError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(body)
	case r.Method == http.MethodGet && (api.Format == "v1" && suffix == "/responses" || api.Format == "backend-api" && suffix == "/codex/responses"):
		m.proxyWebSocket(w, r, cfg, *api, generation)
	case api.Format == "v1":
		m.proxyResponse(w, r, cfg, *api)
	case r.Method == http.MethodPost && (suffix == "/codex/responses" || suffix == "/codex/responses/compact"):
		m.proxyResponse(w, r, cfg, *api)
	default:
		http.NotFound(w, r)
	}
}

func authenticate(headers http.Header, expected string) bool {
	found := false
	for _, name := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key"} {
		for _, value := range headers.Values(name) {
			if name == "Authorization" {
				parts := strings.Fields(value)
				if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
					return false
				}
				value = parts[1]
			}
			if subtle.ConstantTimeCompare([]byte(value), []byte(expected)) != 1 {
				return false
			}
			found = true
		}
	}
	return found
}

func (m *Manager) proxyResponse(w http.ResponseWriter, r *http.Request, cfg Config, api APIConfig) {
	limit := int64(cfg.MaxBodyMB) << 20
	if r.ContentLength > limit {
		writeError(w, &apiError{http.StatusRequestEntityTooLarge, "请求体超过配置的大小限制"})
		return
	}
	var mapped Model
	var changed bool
	if len(api.Mappings) != 0 && r.Method != http.MethodGet && r.Method != http.MethodDelete {
		body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
		if err != nil {
			writeError(w, &apiError{http.StatusBadRequest, "请求体读取失败"})
			return
		}
		if int64(len(body)) > limit {
			writeError(w, &apiError{http.StatusRequestEntityTooLarge, "请求体超过配置的大小限制"})
			return
		}
		body, mapped, changed, err = rewriteBody(r, api, body)
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
	upstreamPath := r.URL.Path[len(api.Prefix)+len(api.Format)+1:]
	if api.Format == "v1" && strings.HasPrefix(upstreamPath, "/models/") && len(api.Mappings) > 0 {
		mapping, ok := api.Mapping(strings.TrimPrefix(upstreamPath, "/models/"))
		if !ok {
			writeError(w, &apiError{http.StatusNotFound, "模型未对当前 API 开放"})
			return
		}
		upstreamPath = "/models/" + mapping.Target
		mapped, changed = mapping, mapping.ID != mapping.Target
	}
	proxy := &httputil.ReverseProxy{
		Transport:     localTransport,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			u := targetURL(api, "/"+api.Format+upstreamPath, pr.In.URL.RawQuery)
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

func resolveModel(r *http.Request, cfg APIConfig, id string) (Model, bool, error) {
	mapping, ok := cfg.Mapping(id)
	if !ok {
		return Model{}, false, &apiError{http.StatusNotFound, "模型未对当前 API 开放"}
	}
	if mapping.ID == mapping.Target {
		return mapping, false, nil
	}
	c, err := fetchCatalog(r.Context(), cfg, r.Header, r.URL.RawQuery)
	if err != nil {
		return Model{}, false, err
	}
	if err := c.validateMapping(mapping); err != nil {
		return Model{}, false, err
	}
	return mapping, true, nil
}

func rewriteBody(r *http.Request, cfg APIConfig, body []byte) ([]byte, Model, bool, error) {
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if r.Header.Get("Content-Type") == "" || err == nil && mediaType == "application/json" {
		return rewriteRequest(r, cfg, body)
	}
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return nil, Model{}, false, &apiError{http.StatusUnsupportedMediaType, "模型白名单接口仅支持 JSON 或 multipart/form-data"}
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var id string
	found := false
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, Model{}, false, &apiError{http.StatusBadRequest, "multipart 请求无效"}
		}
		if part.FormName() == "model" {
			if found || part.FileName() != "" {
				return nil, Model{}, false, &apiError{http.StatusBadRequest, "model 字段必须唯一且不能是文件"}
			}
			value, err := io.ReadAll(part)
			if err != nil {
				return nil, Model{}, false, &apiError{http.StatusBadRequest, "model 字段读取失败"}
			}
			id, found = string(value), true
		}
	}
	if !found {
		return nil, Model{}, false, &apiError{http.StatusBadRequest, "请求必须指定模型"}
	}
	mapping, changed, err := resolveModel(r, cfg, id)
	if err != nil || !changed {
		return body, mapping, changed, err
	}
	reader = multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var output bytes.Buffer
	output.Grow(len(body) + len(mapping.Target) - len(mapping.ID))
	writer := multipart.NewWriter(&output)
	if err := writer.SetBoundary(params["boundary"]); err != nil {
		return nil, Model{}, false, &apiError{http.StatusBadRequest, "multipart boundary 无效"}
	}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, Model{}, false, &apiError{http.StatusBadRequest, "multipart 请求无效"}
		}
		destination, err := writer.CreatePart(part.Header)
		if err != nil {
			return nil, Model{}, false, err
		}
		if part.FormName() == "model" {
			_, err = io.WriteString(destination, mapping.Target)
		} else {
			_, err = io.Copy(destination, part)
		}
		if err != nil {
			return nil, Model{}, false, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, Model{}, false, err
	}
	return output.Bytes(), mapping, true, nil
}

func rewriteRequest(r *http.Request, cfg APIConfig, body []byte) ([]byte, Model, bool, error) {
	if len(cfg.Mappings) == 0 {
		return body, Model{}, false, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, Model{}, false, &apiError{http.StatusBadRequest, "请求必须是有效的 JSON 对象"}
	}
	object := make(map[string]json.RawMessage)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, Model{}, false, &apiError{http.StatusBadRequest, "请求 JSON 无效"}
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, Model{}, false, &apiError{http.StatusBadRequest, "请求 JSON 无效"}
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, Model{}, false, &apiError{http.StatusBadRequest, "请求 JSON 无效"}
		}
		if _, exists := object[key]; exists {
			return nil, Model{}, false, &apiError{http.StatusBadRequest, "请求包含重复的 JSON 字段"}
		}
		object[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, Model{}, false, &apiError{http.StatusBadRequest, "请求 JSON 无效"}
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, Model{}, false, &apiError{http.StatusBadRequest, "请求 JSON 无效"}
	}
	var id string
	if json.Unmarshal(object["model"], &id) != nil || id == "" {
		return nil, Model{}, false, &apiError{http.StatusBadRequest, "请求必须指定模型"}
	}
	mapping, changed, err := resolveModel(r, cfg, id)
	if err != nil || !changed {
		return body, mapping, changed, err
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
	key := "model"
	var kind string
	if json.Unmarshal(object["object"], &kind) == nil && kind == "model" {
		key = "id"
	}
	var model string
	if json.Unmarshal(object[key], &model) != nil || model != mapping.Target {
		return false
	}
	object[key], _ = json.Marshal(mapping.ID)
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
