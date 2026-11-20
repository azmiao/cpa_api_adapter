package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string { return e.Message }

type catalog struct {
	payload map[string]json.RawMessage
	models  []map[string]json.RawMessage
	byID    map[string][]map[string]json.RawMessage
	listKey string
	idKey   string
}

const missingModelWarning = "目标模型 %q 暂未出现在 CPA 目录中；保留已配置模型，实际可用性由 CPA 推理接口判断"

var modelObjectJSON = json.RawMessage(`"model"`)

var localTransport = &http.Transport{
	Proxy:                 nil,
	DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	TLSHandshakeTimeout:   10 * time.Second,
	ResponseHeaderTimeout: upstreamHeaderTimeout,
	ExpectContinueTimeout: time.Second,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          64,
	MaxIdleConnsPerHost:   16,
	IdleConnTimeout:       90 * time.Second,
}
var catalogClient = &http.Client{Transport: localTransport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func targetURL(cfg APIConfig, suffix string, query string) *url.URL {
	u, _ := url.Parse(cfg.Upstream)
	u.Path = strings.TrimRight(u.Path, "/") + suffix
	u.RawPath = ""
	u.RawQuery = query
	return u
}

func fetchCatalog(ctx context.Context, cfg APIConfig, headers http.Header, query string) (*catalog, error) {
	q, err := url.ParseQuery(query)
	if err != nil {
		return nil, &apiError{http.StatusBadRequest, "查询参数无效"}
	}
	if cfg.Format == "backend-api" {
		if q.Get("client_version") == "" {
			q.Set("client_version", "0.159.0")
		}
	} else {
		q.Del("client_version")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL(cfg, "/v1/models", q.Encode()).String(), nil)
	if err != nil {
		return nil, &apiError{http.StatusBadGateway, "无法构造 CPA 目录请求"}
	}
	for _, name := range []string{"User-Agent", "Originator", "Version", "Chatgpt-Account-Id"} {
		for _, value := range headers.Values(name) {
			req.Header.Add(name, value)
		}
	}
	req.Header.Set("Authorization", "Bearer "+cfg.UpstreamAPIKey)
	req.Header.Set("Accept", "application/json")
	key := catalogRequestKey(req, cfg)
	return modelCatalogCache.get(ctx, key, func(loadCtx context.Context) (*catalog, error) {
		return loadCatalog(req.WithContext(loadCtx), cfg.Format)
	})
}

func loadCatalog(req *http.Request, format string) (*catalog, error) {
	resp, err := catalogClient.Do(req)
	if err != nil {
		return nil, &apiError{http.StatusBadGateway, "无法连接 CPA 模型目录，请检查内部地址"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 402 {
			return nil, &apiError{resp.StatusCode, "CPA 拒绝了客户端密钥或模型目录访问"}
		}
		return nil, &apiError{http.StatusBadGateway, fmt.Sprintf("CPA 模型目录返回 HTTP %d", resp.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20+1))
	if err != nil || len(body) > 16<<20 {
		return nil, &apiError{http.StatusBadGateway, "CPA 模型目录读取失败或过大"}
	}
	c := &catalog{byID: make(map[string][]map[string]json.RawMessage), listKey: "models", idKey: "slug"}
	if format == "v1" {
		c.listKey, c.idKey = "data", "id"
	}
	if json.Unmarshal(body, &c.payload) != nil || json.Unmarshal(c.payload[c.listKey], &c.models) != nil || c.models == nil {
		return nil, &apiError{http.StatusBadGateway, "CPA 未返回所选格式的模型目录"}
	}
	for _, entry := range c.models {
		var id string
		_ = json.Unmarshal(entry[c.idKey], &id)
		if id != "" {
			c.byID[id] = append(c.byID[id], entry)
		}
	}
	return c, nil
}

// CPA can omit cooling models; only ambiguity, not absence, invalidates a configured route.
func (c *catalog) validateMapping(m Model) error {
	if len(c.byID[m.Target]) > 1 {
		return &apiError{http.StatusNotFound, fmt.Sprintf("目标模型 %q 在 CPA 目录中重复，已拒绝路由", m.Target)}
	}
	if m.ID != m.Target && len(c.byID[m.ID]) != 0 {
		return &apiError{http.StatusConflict, fmt.Sprintf("别名 %q 与 CPA 模型冲突，已拒绝路由", m.ID)}
	}
	return nil
}

func (c *catalog) render(cfg APIConfig) ([]byte, []string, error) {
	models := c.models
	var warnings []string
	if len(cfg.Mappings) > 0 {
		models = make([]map[string]json.RawMessage, 0, len(cfg.Mappings))
		for _, mapping := range cfg.Mappings {
			if err := c.validateMapping(mapping); err != nil {
				if ae, ok := err.(*apiError); ok && ae.Status == http.StatusConflict {
					return nil, nil, err
				}
				warnings = append(warnings, err.Error())
				continue
			}
			matches := c.byID[mapping.Target]
			// Reserve space for the configured capability fields when CPA supplies no metadata.
			capacity := 9
			if len(matches) == 1 {
				capacity = len(matches[0])
			}
			entry := make(map[string]json.RawMessage, capacity)
			if len(matches) == 1 {
				for k, v := range matches[0] {
					entry[k] = v
				}
			} else {
				warnings = append(warnings, fmt.Sprintf(missingModelWarning, mapping.Target))
				if c.idKey == "id" {
					entry["object"] = modelObjectJSON
				}
			}
			mapping.Apply(entry)
			if c.idKey == "id" {
				entry["id"] = entry["slug"]
				delete(entry, "slug")
			} else if len(matches) == 0 && mapping.DisplayName == "" {
				entry["display_name"] = entry["slug"]
			}
			models = append(models, entry)
		}
	}
	b, err := json.Marshal(models)
	if err != nil {
		return nil, nil, err
	}
	payload := make(map[string]json.RawMessage, len(c.payload))
	for key, value := range c.payload {
		payload[key] = value
	}
	payload[c.listKey] = b
	b, err = json.Marshal(payload)
	return b, warnings, err
}

func writeError(w http.ResponseWriter, err error) {
	status, message := http.StatusBadGateway, "适配器处理失败"
	if ae, ok := err.(*apiError); ok {
		status, message = ae.Status, ae.Message
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": message, "type": "adapter_error"}})
}
