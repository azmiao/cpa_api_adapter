package adapter

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
)

//go:embed index.html
var page []byte

var pluginVersion = "0.4.1"

type RPC struct{ Manager *Manager }

type Envelope struct {
	OK     bool      `json:"ok"`
	Result any       `json:"result,omitempty"`
	Error  *RPCError `json:"error,omitempty"`
}
type RPCError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}
type managementRequest struct {
	Method string `json:"Method"`
	Path   string `json:"Path"`
	Body   []byte `json:"Body"`
}
type managementResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

func (p *RPC) Handle(method string, request []byte) ([]byte, error) {
	var result any
	switch method {
	case "plugin.register", "plugin.reconfigure":
		var req lifecycleRequest
		if len(request) > 0 {
			if err := json.Unmarshal(request, &req); err != nil {
				return nil, err
			}
		}
		cfg, err := ParseConfig(req.ConfigYAML)
		if err != nil {
			return nil, err
		}
		if err = p.Manager.Configure(cfg); err != nil {
			return nil, err
		}
		result = map[string]any{
			"schema_version": 6,
			"metadata":       map[string]any{"Name": "CPA API Adapter", "Version": pluginVersion, "Author": "azmiao", "GitHubRepository": "https://github.com/azmiao/cpa_api_adapter", "ConfigFields": []any{}},
			"capabilities":   map[string]any{"management_api": true},
		}
	case "plugin.quiesce", "plugin.shutdown":
		p.Manager.Stop()
		result = map[string]any{}
	case "management.register":
		result = map[string]any{
			"resources": []any{map[string]any{"Path": "/index.html", "Menu": "API 适配器", "Description": "配置独立 API 路由和模型"}},
			"routes": []any{
				map[string]any{"Method": "GET", "Path": "/plugins/" + PluginID + "/status"},
				map[string]any{"Method": "POST", "Path": "/plugins/" + PluginID + "/validate"},
				map[string]any{"Method": "POST", "Path": "/plugins/" + PluginID + "/catalog"},
			},
		}
	case "management.handle":
		var req managementRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
		result = p.handleManagement(req)
	default:
		return json.Marshal(Envelope{OK: false, Error: &RPCError{Code: "unknown_method", Message: "unknown method: " + method}})
	}
	return json.Marshal(Envelope{OK: true, Result: result})
}

func managementJSON(status int, body any) managementResponse {
	b, err := json.Marshal(body)
	if err != nil {
		status = http.StatusInternalServerError
		b = []byte(`{"error":"response encoding failed"}`)
	}
	return managementResponse{StatusCode: status, Headers: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}, "Cache-Control": []string{"no-store"}}, Body: b}
}

func (p *RPC) handleManagement(req managementRequest) managementResponse {
	if req.Method == http.MethodGet && strings.HasSuffix(req.Path, "/resource/plugins/"+PluginID+"/index.html") {
		return managementResponse{StatusCode: http.StatusOK, Headers: http.Header{
			"Content-Type":            []string{"text/html; charset=utf-8"},
			"Cache-Control":           []string{"no-store"},
			"X-Content-Type-Options":  []string{"nosniff"},
			"Content-Security-Policy": []string{"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; frame-ancestors 'self'"},
		}, Body: page}
	}
	prefix := "/v0/management/plugins/" + PluginID + "/"
	index := strings.LastIndex(req.Path, prefix)
	if index < 0 {
		return managementJSON(http.StatusNotFound, map[string]string{"error": "未找到接口"})
	}
	switch req.Method + " " + req.Path[index+len(prefix):] {
	case "GET status":
		return managementJSON(http.StatusOK, p.Manager.Status())
	case "POST validate":
		if len(req.Body) > 1<<20 {
			return managementJSON(http.StatusRequestEntityTooLarge, map[string]string{"error": "配置过大"})
		}
		if _, err := jsonObjectFields(req.Body); err != nil {
			return managementJSON(http.StatusBadRequest, map[string]string{"error": "配置必须是有效的 JSON 对象"})
		}
		if _, err := ParseConfig(req.Body); err != nil {
			return managementJSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		return managementJSON(http.StatusOK, map[string]bool{"valid": true})
	case "POST catalog":
		var input APIConfig
		if len(req.Body) > 1<<20 || json.Unmarshal(req.Body, &input) != nil {
			return managementJSON(http.StatusBadRequest, map[string]string{"error": "API 配置无效"})
		}
		if err := input.Validate(); err != nil {
			return managementJSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		headers := http.Header{"Authorization": []string{"Bearer " + input.APIKey}}
		c, err := fetchCatalog(context.Background(), input, headers, "")
		if err != nil {
			if ae, ok := err.(*apiError); ok {
				return managementJSON(ae.Status, map[string]string{"error": ae.Message})
			}
			return managementJSON(http.StatusBadGateway, map[string]string{"error": "获取目录失败"})
		}
		var warnings []string
		for _, mapping := range input.Mappings {
			if err := c.validateMapping(mapping); err != nil {
				warnings = append(warnings, err.Error())
			}
		}
		entries := make([]map[string]string, 0, len(c.models))
		for _, model := range c.models {
			var id, name string
			_ = json.Unmarshal(model[c.idKey], &id)
			_ = json.Unmarshal(model["display_name"], &name)
			if id != "" {
				entries = append(entries, map[string]string{"id": id, "name": name})
			}
		}
		return managementJSON(http.StatusOK, map[string]any{"models": entries, "warnings": warnings})
	default:
		return managementJSON(http.StatusNotFound, map[string]string{"error": "未找到管理接口"})
	}
}
