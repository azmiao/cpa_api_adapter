package adapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const PluginID = "cpa-api-adapter"

type Config struct {
	SocketPath string      `json:"socket_path" yaml:"socket_path"`
	SocketMode string      `json:"socket_mode" yaml:"socket_mode"`
	MaxBodyMB  int         `json:"max_body_mb" yaml:"max_body_mb"`
	APIs       []APIConfig `json:"apis" yaml:"apis"`
}

type APIConfig struct {
	Name          string  `json:"name" yaml:"name"`
	APIKey        string  `json:"api_key" yaml:"api_key"`
	Upstream      string  `json:"upstream" yaml:"upstream"`
	Prefix        string  `json:"prefix" yaml:"prefix"`
	Format        string  `json:"format" yaml:"format"`
	PublicBaseURL string  `json:"public_base_url" yaml:"public_base_url"`
	Mappings      []Model `json:"mappings" yaml:"mappings"`
}

type Model struct {
	ID               string   `json:"id" yaml:"id"`
	Target           string   `json:"target" yaml:"target"`
	DisplayName      string   `json:"display_name,omitempty" yaml:"display_name,omitempty"`
	ContextWindow    int      `json:"context_window,omitempty" yaml:"context_window,omitempty"`
	MaxContextWindow int      `json:"max_context_window,omitempty" yaml:"max_context_window,omitempty"`
	MaxTokens        int      `json:"max_tokens,omitempty" yaml:"max_tokens,omitempty"`
	InputModalities  []string `json:"input_modalities,omitempty" yaml:"input_modalities,omitempty"`
	ReasoningLevels  []string `json:"reasoning_levels,omitempty" yaml:"reasoning_levels,omitempty"`
	DefaultReasoning string   `json:"default_reasoning,omitempty" yaml:"default_reasoning,omitempty"`
}

func DefaultConfig() Config {
	return Config{SocketPath: filepath.Join(os.TempDir(), "cpa-api-adapter.sock"), SocketMode: "0660", MaxBodyMB: 32, APIs: []APIConfig{}}
}

func ParseConfig(data []byte) (Config, error) {
	cfg := DefaultConfig()
	if len(data) != 0 {
		wire := struct {
			Config   `yaml:",inline"`
			Enabled  *bool `yaml:"enabled"`
			Priority *int  `yaml:"priority"`
		}{Config: cfg}
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)
		if err := decoder.Decode(&wire); err != nil {
			return Config{}, fmt.Errorf("配置解析失败，请使用 apis 保存每个 API 的配置: %w", err)
		}
		cfg = wire.Config
	}
	return cfg, cfg.Validate()
}

func (c Config) Validate() error {
	if !filepath.IsAbs(c.SocketPath) || strings.ContainsRune(c.SocketPath, 0) {
		return fmt.Errorf("套接字路径必须是绝对路径")
	}
	if c.SocketMode != "0600" && c.SocketMode != "0660" && c.SocketMode != "0666" {
		return fmt.Errorf("套接字权限只能是 0600、0660 或 0666")
	}
	if c.MaxBodyMB < 1 || c.MaxBodyMB > 512 {
		return fmt.Errorf("请求大小限制必须在 1 到 512 MB 之间")
	}
	keys, prefixes := map[string]bool{}, map[string]bool{}
	for i, api := range c.APIs {
		if err := api.Validate(); err != nil {
			return fmt.Errorf("第 %d 个 API: %w", i+1, err)
		}
		if keys[api.APIKey] || prefixes[api.Prefix] {
			return fmt.Errorf("第 %d 个 API 的密钥或路由前缀重复", i+1)
		}
		keys[api.APIKey], prefixes[api.Prefix] = true, true
	}
	return nil
}

func (c APIConfig) Validate() error {
	if c.APIKey == "" || strings.ContainsAny(c.APIKey, " \r\n\t") {
		return fmt.Errorf("API Key 必填且不能包含空白")
	}
	u, err := url.Parse(c.Upstream)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("CPA 内部地址必须是无账号、查询参数和片段的 HTTP(S) 地址")
	}
	if c.Prefix == "" || c.Prefix[0] != '/' || c.Prefix == "/" || path.Clean(c.Prefix) != c.Prefix || strings.ContainsAny(c.Prefix, "?#% \\\r\n\t") {
		return fmt.Errorf("路由前缀必填，必须是非根的规范绝对路径，例如 /work")
	}
	if c.Format != "v1" && c.Format != "backend-api" {
		return fmt.Errorf("出口格式只能是 v1 或 backend-api")
	}
	upstreamPath := strings.TrimRight(u.Path, "/")
	if upstreamPath == c.Prefix || strings.HasPrefix(upstreamPath, c.Prefix+"/") {
		return fmt.Errorf("CPA 内部地址不能指向适配路由，否则可能循环转发")
	}
	if c.PublicBaseURL != "" {
		p, err := url.Parse(c.PublicBaseURL)
		if err != nil || p.Host == "" || p.User != nil || p.RawQuery != "" || p.Fragment != "" || (p.Scheme != "http" && p.Scheme != "https") || !strings.HasSuffix(strings.TrimRight(p.Path, "/"), c.BasePath()) {
			return fmt.Errorf("对外地址必须是以 %s 结尾的 HTTP(S) 地址", c.BasePath())
		}
	}
	seen := map[string]bool{}
	for i, m := range c.Mappings {
		if strings.TrimSpace(m.ID) != m.ID || strings.TrimSpace(m.Target) != m.Target || m.ID == "" || m.Target == "" || strings.ContainsAny(m.ID+m.Target, "\r\n\t ") {
			return fmt.Errorf("第 %d 条映射的模型 ID 和目标不能为空或包含空白", i+1)
		}
		if seen[m.ID] {
			return fmt.Errorf("模型 ID %q 重复", m.ID)
		}
		seen[m.ID] = true
		if m.ContextWindow < 0 || m.MaxContextWindow < 0 || m.MaxTokens < 0 {
			return fmt.Errorf("模型 %q 的 token 数不能为负数", m.ID)
		}
		for _, v := range m.InputModalities {
			if v != "text" && v != "image" {
				return fmt.Errorf("模型 %q 的输入类型无效", m.ID)
			}
		}
		levels := map[string]bool{}
		for _, v := range m.ReasoningLevels {
			switch v {
			case "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
			default:
				return fmt.Errorf("模型 %q 的推理等级无效", m.ID)
			}
			if levels[v] {
				return fmt.Errorf("模型 %q 的推理等级重复", m.ID)
			}
			levels[v] = true
		}
		if m.DefaultReasoning != "" && !levels[m.DefaultReasoning] {
			return fmt.Errorf("模型 %q 的默认推理等级必须包含在支持的等级中", m.ID)
		}
	}
	return nil
}

func (c APIConfig) BasePath() string { return c.Prefix + "/" + c.Format }

func (c APIConfig) Mapping(id string) (Model, bool) {
	for _, m := range c.Mappings {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

func (m Model) Apply(entry map[string]json.RawMessage) {
	set := func(key string, v any) { b, _ := json.Marshal(v); entry[key] = b }
	set("slug", m.ID)
	if m.DisplayName != "" {
		set("display_name", m.DisplayName)
	}
	if m.ContextWindow > 0 {
		set("context_window", m.ContextWindow)
	}
	if m.MaxContextWindow > 0 {
		set("max_context_window", m.MaxContextWindow)
	}
	if m.MaxTokens > 0 {
		set("max_tokens", m.MaxTokens)
	}
	if len(m.InputModalities) > 0 {
		set("input_modalities", m.InputModalities)
	}
	if len(m.ReasoningLevels) > 0 {
		levels := make([]map[string]string, 0, len(m.ReasoningLevels))
		for _, level := range m.ReasoningLevels {
			levels = append(levels, map[string]string{"effort": level, "description": level})
		}
		set("supported_reasoning_levels", levels)
		def := m.DefaultReasoning
		if def == "" {
			def = m.ReasoningLevels[0]
		}
		set("default_reasoning_level", def)
	}
}
