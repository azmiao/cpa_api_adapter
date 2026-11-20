package adapter

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func listenerCatalog(t *testing.T, cfg Config) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+cfg.ListenAddress+"/work/backend-api/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIs[0].APIKey)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated listener failed: %d %s %v", resp.StatusCode, body, err)
	}
	return string(body)
}

func TestInvalidReconfigureAndValidationKeepAuthenticatedListener(t *testing.T) {
	catalogTestCache(t, time.Minute, time.Now)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer upstream-secret" {
			http.Error(w, "wrong key", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"models":[{"slug":"available"}]}`)
	}))
	defer upstream.Close()
	cfg := testConfig()
	cfg.ListenMode, cfg.ListenAddress = "tcp", tcpTestAddress(t)
	cfg.APIs[0].Upstream = upstream.URL
	manager := NewManager()
	defer manager.Stop()
	if err := manager.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	rpc := &RPC{Manager: manager}
	assertPreserved := func() {
		t.Helper()
		status := manager.Status()
		if !status.Running || status.ListenAddress != cfg.ListenAddress || status.Error != "" || !reflect.DeepEqual(status.Config, cfg) {
			t.Fatalf("bad input changed running configuration: %+v", status)
		}
		assertListenerServing(t, "tcp", cfg.ListenAddress)
		if !strings.Contains(listenerCatalog(t, cfg), `"slug":"available"`) {
			t.Fatal("old credentials no longer reached the original catalog")
		}
	}
	bad := cfg
	bad.MaxBodyMB = 0
	if err := manager.Configure(bad); err == nil {
		t.Fatal("invalid direct configuration was accepted")
	}
	assertPreserved()
	missing := cfg
	missing.APIs = append([]APIConfig(nil), cfg.APIs...)
	missing.APIs[0].UpstreamAPIKey = ""
	data, err := json.Marshal(missing)
	if err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(lifecycleRequest{ConfigYAML: data})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rpc.Handle("plugin.reconfigure", request); err == nil {
		t.Fatal("missing private upstream key replaced a running listener")
	}
	assertPreserved()
	if _, err := rpc.Handle("plugin.register", request); err == nil {
		t.Fatal("legacy registration replaced a running authenticated configuration")
	}
	assertPreserved()
	for _, data := range []string{
		"max_body_mb: not-a-number\n",
		"listen_mode: tcp\nlisten_address: invalid\n",
		"unknown: true\n",
		"enabled: definitely\n",
		"apis: [{api_key: key, upstream_api_key: private-key, upstream: http://127.0.0.1, prefix: /work, format: v1, typo: 1}]\n",
	} {
		request, err := json.Marshal(lifecycleRequest{ConfigYAML: []byte(data)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rpc.Handle("plugin.reconfigure", request); err == nil {
			t.Fatalf("invalid lifecycle input was accepted: %s", data)
		}
		assertPreserved()
	}
	for _, body := range []string{
		`{"max_body_mb":`,
		`{} {}`,
		`{"max_body_mb":0}`,
		`{"max_body_mb":"wrong"}`,
		`null`,
		`[]`,
		`{"listen_mod":"tcp"}`,
		`{"enabled":"wrong"}`,
		`{"apis":[{"api_key":"key","upstream_api_key":"private-key","upstream":"http://127.0.0.1","prefix":"/work","format":"v1","typo":1}]}`,
	} {
		response := rpc.handleManagement(managementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + PluginID + "/validate", Body: []byte(body)})
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("management accepted invalid data: %s => %d %s", body, response.StatusCode, response.Body)
		}
		assertPreserved()
	}
	for _, body := range []string{`{}`, `{"enabled":true,"priority":1,"store":{"release-tag":"vtest","repository":"https://example.com"}}`} {
		response := rpc.handleManagement(managementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + PluginID + "/validate", Body: []byte(body)})
		if response.StatusCode != http.StatusOK {
			t.Fatalf("management did not use defaults/host metadata contract: %s => %d %s", body, response.StatusCode, response.Body)
		}
		assertPreserved()
	}
}

func TestReconfigurationAndStopInvalidateCatalogIncludingOldFlights(t *testing.T) {
	catalogTestCache(t, time.Minute, time.Now)
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	releaseOld := sync.OnceFunc(func() { close(release) })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-release:
				_, _ = io.WriteString(w, `{"models":[{"slug":"old"}]}`)
			case <-r.Context().Done():
			}
			return
		}
		_, _ = io.WriteString(w, `{"models":[{"slug":"new"}]}`)
	}))
	defer upstream.Close()
	defer releaseOld()
	cfg := testConfig()
	cfg.ListenMode, cfg.ListenAddress = "tcp", tcpTestAddress(t)
	cfg.APIs[0].Upstream = upstream.URL
	manager := NewManager()
	defer manager.Stop()
	if err := manager.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	oldResult := make(chan error, 1)
	go func() {
		req, err := http.NewRequest(http.MethodGet, "http://"+cfg.ListenAddress+"/work/backend-api/models", nil)
		if err != nil {
			oldResult <- err
			return
		}
		req.Header.Set("Authorization", "Bearer client-secret")
		resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
		if err != nil {
			oldResult <- err
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err == nil && (resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"slug":"old"`)) {
			err = fmt.Errorf("in-flight old generation lost its response: %d %s", resp.StatusCode, body)
		}
		oldResult <- err
	}()
	awaitCatalogSignal(t, started)
	cfg.MaxBodyMB++
	if err := manager.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listenerCatalog(t, cfg), `"slug":"new"`) || calls.Load() != 2 {
		t.Fatal("new configuration reused an old-generation directory request")
	}
	releaseOld()
	if err := <-oldResult; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listenerCatalog(t, cfg), `"slug":"new"`) || calls.Load() != 2 {
		t.Fatal("old in-flight directory replaced the new generation's cached catalog")
	}
	manager.Stop()
	if err := manager.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listenerCatalog(t, cfg), `"slug":"new"`) || calls.Load() != 3 {
		t.Fatal("Stop retained a cached directory across restart")
	}
}
