package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

func responseResourceID(api APIConfig, upstreamPath string) string {
	if api.Format != "v1" {
		return ""
	}
	tail, ok := strings.CutPrefix(upstreamPath, "/responses/")
	if !ok {
		return ""
	}
	id, _, _ := strings.Cut(tail, "/")
	return id
}

func responseMapping(body []byte, api APIConfig) (Model, error) {
	fields, err := jsonObjectFields(body)
	if err != nil {
		return Model{}, &apiError{http.StatusBadGateway, "CPA 未返回有效的 Responses 资源"}
	}
	for key := range fields {
		if key != "model" && strings.EqualFold(key, "model") {
			return Model{}, &apiError{http.StatusBadGateway, "CPA 返回了不明确的资源模型字段"}
		}
	}
	span, ok := fields["model"]
	var model string
	if !ok || json.Unmarshal(body[span.start:span.end], &model) != nil || model == "" {
		return Model{}, &apiError{http.StatusBadGateway, "无法确认 Responses 资源所属模型"}
	}
	for _, mapping := range api.Mappings {
		if mapping.Target == model {
			return mapping, nil
		}
	}
	return Model{}, &apiError{http.StatusNotFound, "资源所属模型未对当前 API 开放"}
}

func fetchResponseMapping(r *http.Request, api APIConfig, id string, limit int64) (Model, error) {
	ctx, cancel := context.WithTimeout(r.Context(), upstreamHeaderTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL(api, "/v1/responses/"+id, "").String(), nil)
	if err != nil {
		return Model{}, &apiError{http.StatusBadGateway, "无法构造 Responses 资源检查请求"}
	}
	req.Header = r.Header.Clone()
	removeHopHeaders(req.Header)
	removeConditionalHeaders(req.Header)
	req.Header.Set("Authorization", "Bearer "+api.UpstreamAPIKey)
	req.Header.Del("X-Api-Key")
	req.Header.Del("X-Goog-Api-Key")
	req.Header.Del("Content-Length")
	req.Header.Del("Content-Type")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := localTransport.RoundTrip(req)
	if err != nil {
		return Model{}, &apiError{http.StatusBadGateway, "无法检查 Responses 资源所属模型"}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		status := http.StatusBadGateway
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			status = resp.StatusCode
		}
		return Model{}, &apiError{status, "CPA 拒绝了 Responses 资源访问"}
	}
	body, err := readResponseJSON(resp, limit)
	if err != nil {
		return Model{}, err
	}
	return responseMapping(body, api)
}
