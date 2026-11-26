package adapter

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httputil"
	"path"
	"strconv"
	"strings"
	"time"
)

func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cfg, generation, ok := m.begin()
	if !ok {
		writeError(w, &apiError{http.StatusServiceUnavailable, "适配器已停止"})
		return
	}
	defer m.active.Done()
	requestPath := r.URL.Path
	if cfg.SharedPrefix != "" {
		relative, rooted := strings.CutPrefix(requestPath, "/")
		var matched bool
		requestPath, matched = strings.CutPrefix(relative, cfg.SharedPrefix)
		if !rooted || !matched || !strings.HasPrefix(requestPath, "/") {
			http.NotFound(w, r)
			return
		}
	}
	var api, routeAPI *APIConfig
	matchedPrefix, apiIndex := -1, -1
	for i := range cfg.APIs {
		candidate := &cfg.APIs[i]
		suffix, matches := strings.CutPrefix(requestPath, candidate.Prefix)
		formatPath := "/v1/"
		if candidate.Format == "backend-api" {
			formatPath = "/backend-api/"
		}
		if !matches || !strings.HasPrefix(suffix, formatPath) || len(candidate.Prefix) < matchedPrefix {
			continue
		}
		if len(candidate.Prefix) > matchedPrefix {
			matchedPrefix, routeAPI = len(candidate.Prefix), candidate
			api = nil
		}
		if authenticate(r.Header, candidate.APIKey) {
			api, apiIndex = candidate, i
		}
	}
	if matchedPrefix < 0 {
		http.NotFound(w, r)
		return
	}
	suffix := requestPath[matchedPrefix+1+len(routeAPI.Format):]
	if api == nil {
		info := callInfo(r, *routeAPI, cfg.SharedPrefix, generation, 0, suffix)
		info.APIID, info.APIName = "unauthenticated", "未鉴权"
		trace := m.calls.start(info, time.Time{})
		defer trace.finishHTTP(r.Context())
		writeError(callResponseWriter{w, trace}, &apiError{http.StatusUnauthorized, "API Key 无效或不属于此路由"})
		return
	}
	info := callInfo(r, *api, cfg.SharedPrefix, generation, apiIndex, suffix)
	wsRoute := r.Method == http.MethodGet && (api.Format == "v1" && suffix == "/responses" || api.Format == "backend-api" && suffix == "/codex/responses")
	var trace *callTrace
	clean := path.Clean(r.URL.Path) == r.URL.Path
	if !wsRoute || !clean {
		trace = m.calls.start(info, time.Time{})
		defer trace.finishHTTP(r.Context())
		w = callResponseWriter{w, trace}
	}
	if !clean {
		writeError(w, &apiError{http.StatusBadRequest, "请求路径必须是规范路径"})
		return
	}
	r.Header.Set("Authorization", "Bearer "+api.UpstreamAPIKey)
	r.Header.Del("X-Api-Key")
	r.Header.Del("X-Goog-Api-Key")
	if !wsRoute && requestsUpgrade(r.Header) {
		writeError(w, &apiError{http.StatusBadRequest, "此路径不支持协议升级"})
		return
	}
	switch {
	case r.Method == http.MethodGet && (suffix == "/models" || api.Format == "backend-api" && suffix == "/codex/models"):
		c, err := fetchCatalog(r.Context(), *api, r.Header, r.URL.RawQuery)
		if err != nil {
			writeError(w, err)
			return
		}
		body, warnings, err := c.render(*api)
		if err != nil {
			writeError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if len(warnings) > 0 {
			w.Header().Set("X-Adapter-Warning", "unavailable-model-mappings")
			w.Header().Set("X-Adapter-Warning-Count", strconv.Itoa(len(warnings)))
		}
		_, _ = w.Write(body)
	case wsRoute:
		m.proxyWebSocket(w, r, cfg, *api, suffix, generation, info)
	case api.Format == "v1":
		m.proxyResponse(w, r, cfg, *api, suffix, trace)
	case r.Method == http.MethodPost && (suffix == "/codex/responses" || suffix == "/codex/responses/compact"):
		m.proxyResponse(w, r, cfg, *api, suffix, trace)
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

func (m *Manager) proxyResponse(w http.ResponseWriter, r *http.Request, cfg Config, api APIConfig, upstreamPath string, trace *callTrace) {
	limit := int64(cfg.MaxBodyMB) << 20
	if r.ContentLength > limit {
		writeError(w, &apiError{http.StatusRequestEntityTooLarge, "请求体超过配置的大小限制"})
		return
	}
	if r.Body != nil {
		r.Body = &callObservedBody{ReadCloser: r.Body, trace: trace, request: true}
	}
	resourceID := responseResourceID(api, upstreamPath)
	restrictedResource := resourceID != "" && len(api.Mappings) > 0
	directResourceRead := restrictedResource && r.Method == http.MethodGet && upstreamPath == "/responses/"+resourceID
	var mapped Model
	var changed bool
	if restrictedResource && !directResourceRead {
		var err error
		mapped, err = fetchResponseMapping(r, api, resourceID, limit)
		if err != nil {
			writeError(w, err)
			return
		}
		changed = mapped.ID != mapped.Target
	}
	if len(api.Mappings) != 0 && !restrictedResource && (r.Method != http.MethodGet && r.Method != http.MethodDelete && r.Method != http.MethodHead || r.ContentLength != 0) {
		var cleanup func()
		var err error
		mapped, changed, cleanup, err = prepareRequestBody(r, api, limit)
		if err != nil {
			writeError(w, err)
			return
		}
		defer cleanup()
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
	}
	trace.setModel(mapped)
	if api.Format == "v1" && strings.HasPrefix(upstreamPath, "/models/") && len(api.Mappings) > 0 {
		mapping, ok := api.Mapping(strings.TrimPrefix(upstreamPath, "/models/"))
		if !ok {
			writeError(w, &apiError{http.StatusNotFound, "模型未对当前 API 开放"})
			return
		}
		upstreamPath = "/models/" + mapping.Target
		mapped, changed = mapping, mapping.ID != mapping.Target
		trace.setModel(mapped)
	}
	proxy := &httputil.ReverseProxy{
		Transport: localTransport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			u := targetURL(api, "/"+api.Format+upstreamPath, pr.In.URL.RawQuery)
			pr.SetURL(u)
			pr.Out.URL.Path = u.Path
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = u.RawQuery
			pr.Out.Header.Set("Authorization", "Bearer "+api.UpstreamAPIKey)
			pr.Out.Header.Del("X-Api-Key")
			pr.Out.Header.Del("X-Goog-Api-Key")
			if directResourceRead {
				removeConditionalHeaders(pr.Out.Header)
			}
			if changed || directResourceRead {
				pr.Out.Header.Set("Accept-Encoding", "identity")
			}
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			var tooLarge *http.MaxBytesError
			var ae *apiError
			switch {
			case errors.As(err, &tooLarge):
				writeError(w, &apiError{http.StatusRequestEntityTooLarge, "请求体超过配置的大小限制"})
			case errors.As(err, &ae):
				writeError(w, ae)
			default:
				writeError(w, &apiError{http.StatusBadGateway, "无法连接 CPA 推理接口"})
				trace.setErrorClass("connection_error")
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode == http.StatusSwitchingProtocols {
				return &apiError{http.StatusBadGateway, "CPA 在非升级路径返回了协议升级"}
			}
			if directResourceRead && resp.StatusCode >= 300 && resp.StatusCode < 400 {
				return &apiError{http.StatusBadGateway, "CPA 未返回可核验的 Responses 资源"}
			}
			if directResourceRead && resp.StatusCode >= 200 && resp.StatusCode < 300 {
				body, err := readResponseJSON(resp, limit)
				if err != nil {
					return err
				}
				mapping, err := responseMapping(body, api)
				if err != nil {
					return err
				}
				setJSONResponse(resp, restoreJSON(body, mapping))
				observeCallResponse(resp, trace)
				return nil
			}
			if changed && r.Method != http.MethodHead {
				if err := restoreResponse(resp, mapped, limit); err != nil {
					return err
				}
			}
			observeCallResponse(resp, trace)
			return nil
		},
	}
	proxy.ServeHTTP(deadlineResponseWriter{w}, r)
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

func rewriteRequest(r *http.Request, cfg APIConfig, body []byte) ([]byte, Model, bool, error) {
	if len(cfg.Mappings) == 0 {
		return body, Model{}, false, nil
	}
	fields, err := jsonObjectFields(body)
	if err != nil {
		return nil, Model{}, false, &apiError{http.StatusBadRequest, err.Error()}
	}
	for key := range fields {
		if key != "model" && strings.EqualFold(key, "model") {
			return nil, Model{}, false, &apiError{http.StatusBadRequest, "model 字段必须使用规范小写名称"}
		}
	}
	span, ok := fields["model"]
	var id string
	if !ok || json.Unmarshal(body[span.start:span.end], &id) != nil || id == "" {
		return nil, Model{}, false, &apiError{http.StatusBadRequest, "请求必须指定模型"}
	}
	mapping, changed, err := resolveModel(r, cfg, id)
	if err != nil || !changed {
		return body, mapping, changed, err
	}
	value, _ := json.Marshal(mapping.Target)
	return replaceJSONSpan(body, span, value), mapping, true, nil
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
