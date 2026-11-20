package adapter

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDualKeyConfigurationRejectsMissingAndExposedUpstreamCredentials(t *testing.T) {
	for _, item := range []struct {
		name   string
		change func(*Config)
	}{
		{"missing", func(cfg *Config) { cfg.APIs[0].UpstreamAPIKey = "" }},
		{"whitespace", func(cfg *Config) { cfg.APIs[0].UpstreamAPIKey = "private key" }},
		{"shared", func(cfg *Config) { cfg.APIs[0].UpstreamAPIKey = cfg.APIs[0].APIKey }},
		{"client_exposes_other_upstream", func(cfg *Config) { cfg.APIs[1].APIKey = cfg.APIs[0].UpstreamAPIKey }},
		{"upstream_exposed_by_other_client", func(cfg *Config) { cfg.APIs[1].UpstreamAPIKey = cfg.APIs[0].APIKey }},
	} {
		t.Run(item.name, func(t *testing.T) {
			cfg := testConfig()
			other := cfg.APIs[0]
			other.APIKey, other.UpstreamAPIKey, other.Prefix = "second-client", "second-upstream", "/second"
			cfg.APIs = append(cfg.APIs, other)
			item.change(&cfg)
			for _, encode := range []func(any) ([]byte, error){json.Marshal, yaml.Marshal} {
				data, err := encode(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := ParseConfig(data); err == nil {
					t.Fatal("unsafe dual-key configuration was accepted")
				}
			}
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			response := (&RPC{Manager: NewManager()}).handleManagement(managementRequest{
				Method: http.MethodPost, Path: "/v0/management/plugins/cpa-api-adapter/validate", Body: data,
			})
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("unsafe credentials could be saved through management: %d", response.StatusCode)
			}
		})
	}
}

func TestLegacyKeyRegistrationIsEditableButDoesNotListenUntilMigrated(t *testing.T) {
	for _, format := range []struct {
		name   string
		encode func(any) ([]byte, error)
	}{{"json", json.Marshal}, {"yaml", yaml.Marshal}} {
		t.Run(format.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer upstream-secret" {
					http.Error(w, "private CPA credentials required", http.StatusUnauthorized)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"models":[{"slug":"migrated-model"}]}`)
			}))
			defer upstream.Close()
			cfg := testConfig()
			cfg.ListenMode, cfg.ListenAddress = "tcp", tcpTestAddress(t)
			cfg.APIs[0].Upstream = upstream.URL
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var legacy map[string]any
			if err := json.Unmarshal(data, &legacy); err != nil {
				t.Fatal(err)
			}
			delete(legacy["apis"].([]any)[0].(map[string]any), "upstream_api_key")
			data, err = format.encode(legacy)
			if err != nil {
				t.Fatal(err)
			}
			manager := NewManager()
			defer manager.Stop()
			rpc := &RPC{Manager: manager}
			request, err := json.Marshal(lifecycleRequest{ConfigYAML: data})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := rpc.Handle("plugin.register", request); err != nil {
				t.Fatalf("legacy configuration could not expose its migration management page: %v", err)
			}
			status := manager.Status()
			if status.Running || status.Error == "" || status.Config.APIs[0].APIKey != "client-secret" || status.Config.APIs[0].UpstreamAPIKey != "" {
				t.Fatal("legacy credentials were reused or lost instead of requiring migration")
			}
			probe, err := net.Listen("tcp", cfg.ListenAddress)
			if err != nil {
				t.Fatalf("legacy configuration unexpectedly opened its API listener: %v", err)
			}
			probe.Close()
			page := rpc.handleManagement(managementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/cpa-api-adapter/index.html"})
			if page.StatusCode != http.StatusOK {
				t.Fatalf("migration management page is inaccessible: %d", page.StatusCode)
			}
			data, err = format.encode(cfg)
			if err != nil {
				t.Fatal(err)
			}
			request, err = json.Marshal(lifecycleRequest{ConfigYAML: data})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := rpc.Handle("plugin.reconfigure", request); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(listenerCatalog(t, cfg), `"slug":"migrated-model"`) {
				t.Fatal("migrated listener did not authenticate with the private upstream key")
			}
		})
	}
}
