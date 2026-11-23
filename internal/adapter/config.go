package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const PluginID = "cpa-api-adapter"

const defaultListenAddress = "127.0.0.1:18318"

var errMissingUpstreamAPIKey = errors.New("CPA 上游 API Key 必填；请填写新的私有 CPA Key，并从 CPA 删除已分发的旧 Key")

type Config struct {
	ListenMode       string      `json:"listen_mode" yaml:"listen_mode"`
	ListenAddress    string      `json:"listen_address" yaml:"listen_address"`
	SocketPath       string      `json:"socket_path" yaml:"socket_path"`
	SocketMode       string      `json:"socket_mode" yaml:"socket_mode"`
	MaxBodyMB        int         `json:"max_body_mb" yaml:"max_body_mb"`
	WebSocketEnabled bool        `json:"websocket_enabled" yaml:"websocket_enabled"`
	APIs             []APIConfig `json:"apis" yaml:"apis"`
}

type APIConfig struct {
	Name           string  `json:"name" yaml:"name"`
	APIKey         string  `json:"api_key" yaml:"api_key"`
	UpstreamAPIKey string  `json:"upstream_api_key" yaml:"upstream_api_key"`
	Upstream       string  `json:"upstream" yaml:"upstream"`
	Prefix         string  `json:"prefix" yaml:"prefix"`
	Format         string  `json:"format" yaml:"format"`
	Mappings       []Model `json:"mappings" yaml:"mappings"`
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
	return Config{ListenMode: "unix", ListenAddress: defaultListenAddress, SocketPath: filepath.Join(os.TempDir(), "cpa-api-adapter.sock"), SocketMode: "0660", MaxBodyMB: 32, APIs: []APIConfig{}}
}

func ParseConfig(data []byte) (Config, error) {
	cfg := DefaultConfig()
	if len(data) != 0 {
		wire := struct {
			Config   `yaml:",inline"`
			Enabled  *bool     `yaml:"enabled"`
			Priority *int      `yaml:"priority"`
			Store    yaml.Node `yaml:"store"`
		}{Config: cfg}
		normalized, err := migrateLegacyConfig(data)
		if err != nil {
			return Config{}, fmt.Errorf("配置解析失败，请使用 apis 保存每个 API 的配置: %w", err)
		}
		decoder := yaml.NewDecoder(bytes.NewReader(normalized))
		decoder.KnownFields(true)
		if err := decoder.Decode(&wire); err != nil {
			return Config{}, fmt.Errorf("配置解析失败，请使用 apis 保存每个 API 的配置: %w", err)
		}
		cfg = wire.Config
	}
	return cfg, cfg.Validate()
}

func migrateLegacyConfig(data []byte) ([]byte, error) {
	if !bytes.Contains(data, []byte("public_base_url")) {
		return data, nil
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	if len(document.Content) == 0 || document.Content[0].Kind != yaml.MappingNode {
		return data, nil
	}
	root := document.Content[0]
	migrated := false
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value != "apis" {
			continue
		}
		apis := root.Content[i+1]
		if apis.Kind == yaml.AliasNode {
			apis = apis.Alias
		}
		if apis.Kind != yaml.SequenceNode {
			continue
		}
		for _, api := range apis.Content {
			if api.Kind == yaml.AliasNode {
				api = api.Alias
			}
			if api.Kind != yaml.MappingNode {
				continue
			}
			for j := 0; j < len(api.Content); j += 2 {
				if api.Content[j].Value == "public_base_url" {
					api.Content = append(api.Content[:j], api.Content[j+2:]...)
					migrated = true
					break
				}
			}
		}
	}
	if !migrated {
		return data, nil
	}
	return yaml.Marshal(&document)
}

func (c Config) Validate() error {
	switch c.ListenMode {
	case "", "unix":
		if !filepath.IsAbs(c.SocketPath) || strings.ContainsRune(c.SocketPath, 0) {
			return fmt.Errorf("套接字路径必须是绝对路径")
		}
		if c.SocketMode != "0600" && c.SocketMode != "0660" && c.SocketMode != "0666" {
			return fmt.Errorf("套接字权限只能是 0600、0660 或 0666")
		}
	case "tcp":
		address, err := netip.ParseAddrPort(c.ListenAddress)
		if err != nil || address.Port() == 0 || address.Addr().Zone() != "" {
			return fmt.Errorf("TCP 监听地址必须是 IP:端口，例如 127.0.0.1:18318 或 [::1]:18318，端口范围为 1 到 65535")
		}
	default:
		return fmt.Errorf("监听方式只能是 unix 或 tcp")
	}
	if c.MaxBodyMB < 1 || c.MaxBodyMB > 512 {
		return fmt.Errorf("请求大小限制必须在 1 到 512 MB 之间")
	}
	identities := make(map[[3]string]bool, len(c.APIs))
	upstreamKeys := make(map[string]struct{}, len(c.APIs))
	for i, api := range c.APIs {
		if err := api.Validate(); err != nil {
			return fmt.Errorf("第 %d 个 API: %w", i+1, err)
		}
		identity := [3]string{api.Prefix, api.Format, api.APIKey}
		if identities[identity] {
			return fmt.Errorf("第 %d 个 API 的路由前缀、出口格式和 API Key 组合重复", i+1)
		}
		identities[identity] = true
		upstreamKeys[api.UpstreamAPIKey] = struct{}{}
	}
	for i, api := range c.APIs {
		if _, exists := upstreamKeys[api.APIKey]; exists {
			return fmt.Errorf("第 %d 个 API 的插件 API Key 不能作为任何 API 的 CPA 上游 Key", i+1)
		}
	}
	return nil
}

func (c Config) listenEndpoint() (string, string) {
	if c.ListenMode == "tcp" {
		address, _ := netip.ParseAddrPort(c.ListenAddress)
		address = netip.AddrPortFrom(address.Addr().Unmap(), address.Port())
		return "tcp", address.String()
	}
	return "unix", c.SocketPath
}

func (c APIConfig) Validate() error {
	if c.APIKey == "" || strings.ContainsAny(c.APIKey, " \r\n\t") {
		return fmt.Errorf("插件 API Key 必填且不能包含空白")
	}
	if c.UpstreamAPIKey == "" {
		return errMissingUpstreamAPIKey
	}
	if strings.ContainsAny(c.UpstreamAPIKey, " \r\n\t") {
		return fmt.Errorf("CPA 上游 API Key 不能包含空白")
	}
	if c.UpstreamAPIKey == c.APIKey {
		return fmt.Errorf("CPA 上游 API Key 不能与插件 API Key 相同；请使用未分发给用户的私有 CPA Key")
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
