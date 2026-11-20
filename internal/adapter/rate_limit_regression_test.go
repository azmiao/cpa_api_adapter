package adapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const rateLimitHTTPBody = "{\"error\":{\"type\":\"rate_limit_error\",\"code\":\"rate_limit_exceeded\",\"message\":\"CPA quota reset pending\",\"retry_after\":17},\"request_id\":\"cpa-limit-17\"}\n"
const rateLimitWSBody = `{"type":"error","status":429,"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"CPA quota reset pending","retry_after":17},"request_id":"cpa-limit-17"}`

// Each client owns its request and response; failures are returned to the test
// goroutine rather than terminating a concurrent consumer with t.Fatal.
type rateLimitResult struct {
	status  int
	headers http.Header
	body    []byte
	err     error
}

func rateLimitRequest(client *http.Client, method, target, body string) rateLimitResult {
	req, err := http.NewRequest(method, target, strings.NewReader(body))
	if err != nil {
		return rateLimitResult{err: err}
	}
	req.Header.Set("Authorization", "Bearer client-secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return rateLimitResult{err: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return rateLimitResult{resp.StatusCode, resp.Header.Clone(), data, err}
}

func rateLimitCatalogKeys(format string) (string, string) {
	if format == "v1" {
		return "data", "id"
	}
	return "models", "slug"
}

func assertRateLimitCatalog(t *testing.T, result rateLimitResult, format string, missing bool) {
	t.Helper()
	if result.err != nil || result.status != http.StatusOK {
		t.Fatalf("catalog failed: status=%d body=%s err=%v", result.status, result.body, result.err)
	}
	listKey, idKey := rateLimitCatalogKeys(format)
	var payload map[string]json.RawMessage
	var models []map[string]any
	if err := json.Unmarshal(result.body, &payload); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload[listKey], &models); err != nil {
		t.Fatal(err)
	}
	entries := make(map[string]map[string]any)
	for _, model := range models {
		id, ok := model[idKey].(string)
		if !ok || entries[id] != nil {
			t.Fatalf("invalid or duplicate catalog ID: %s", result.body)
		}
		entries[id] = model
	}
	for _, id := range []string{"visible", "minimal", "listed"} {
		if entries[id] == nil {
			t.Fatalf("configured ID %q disappeared from catalog: %s", id, result.body)
		}
	}
	for id := range entries {
		if id != "visible" && id != "minimal" && id != "listed" {
			t.Fatalf("non-whitelisted model %q leaked: %s", id, result.body)
		}
	}
	for key, want := range map[string]any{
		"display_name": "Configured model", "context_window": float64(8192),
		"max_context_window": float64(16384), "max_tokens": float64(2048),
		"input_modalities":           []any{"text", "image"},
		"supported_reasoning_levels": []any{map[string]any{"effort": "low", "description": "low"}, map[string]any{"effort": "high", "description": "high"}},
		"default_reasoning_level":    "high",
	} {
		if !reflect.DeepEqual(entries["visible"][key], want) {
			t.Fatalf("configured capability %s changed: got=%v want=%v catalog=%s", key, entries["visible"][key], want, result.body)
		}
	}
	if entries["listed"]["context_window"] != float64(32768) || !reflect.DeepEqual(entries["listed"]["input_modalities"], []any{"text"}) {
		t.Fatalf("available CPA metadata was not preserved: %s", result.body)
	}
	if result.headers.Get("Cache-Control") != "no-store" {
		t.Fatalf("catalog became client-cacheable: %v", result.headers)
	}
	if !missing {
		if entries["minimal"]["context_window"] != float64(65536) || entries["minimal"]["source_metadata"] == nil || entries["visible"]["source_metadata"] == nil {
			t.Fatalf("initial CPA metadata missing: %s", result.body)
		}
		if result.headers.Get("X-Adapter-Warning") != "" || result.headers.Get("X-Adapter-Warning-Count") != "" {
			t.Fatalf("available models were warned about: %v", result.headers)
		}
		return
	}
	for _, id := range []string{"visible", "minimal"} {
		for _, key := range []string{"source_metadata", "sourcemetadata", "provider", "owned_by"} {
			if _, ok := entries[id][key]; ok {
				t.Fatalf("missing target retained or invented upstream %s for %s: %s", key, id, result.body)
			}
		}
	}
	for _, key := range []string{"context_window", "max_context_window", "max_tokens", "input_modalities", "supported_reasoning_levels", "default_reasoning_level"} {
		if _, ok := entries["minimal"][key]; ok {
			t.Fatalf("missing target invented an unconfigured %s: %s", key, result.body)
		}
	}
	if result.headers.Get("X-Adapter-Warning") != "unavailable-model-mappings" || result.headers.Get("X-Adapter-Warning-Count") != "2" {
		t.Fatalf("missing catalog warnings lost: %v", result.headers)
	}
}

func TestRateLimitSurvivesCatalogExpiryAcrossConcurrentClients(t *testing.T) {
	for _, format := range []string{"v1", "backend-api"} {
		t.Run(format, func(t *testing.T) {
			var clock atomic.Int64
			clock.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
			catalogTestCache(t, 15*time.Second, func() time.Time { return time.Unix(0, clock.Load()) })
			var missing atomic.Bool
			var catalogCalls, inferenceCalls atomic.Int32
			type forwardedRequest struct{ path, body string }
			forwarded := make(chan forwardedRequest, 32)
			paths := []string{"/responses", "/chat/completions"}
			if format == "backend-api" {
				paths = []string{"/codex/responses", "/codex/responses/compact"}
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer upstream-secret" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("X-Goog-Api-Key") != "" {
					http.Error(w, "wrong upstream identity", http.StatusUnauthorized)
					return
				}
				if r.URL.Path == "/v1/models" {
					catalogCalls.Add(1)
					listKey, idKey := rateLimitCatalogKeys(format)
					models := []map[string]any{
						{idKey: "hidden", "context_window": 999999},
						{idKey: "account/listed", "context_window": 32768, "input_modalities": []string{"text"}},
					}
					if !missing.Load() {
						models = append(models,
							map[string]any{idKey: "account/target", "context_window": 131072, "source_metadata": map[string]string{"provider": "old-provider"}, "provider": "old-provider"},
							map[string]any{idKey: "account/minimal", "context_window": 65536, "input_modalities": []string{"audio"}, "source_metadata": map[string]string{"provider": "old-provider"}},
						)
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{listKey: models})
					return
				}
				if r.Method != http.MethodPost || r.URL.Path != "/"+format+paths[0] && r.URL.Path != "/"+format+paths[1] {
					http.NotFound(w, r)
					return
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, "body read failed", http.StatusBadRequest)
					return
				}
				inferenceCalls.Add(1)
				forwarded <- forwardedRequest{r.URL.Path, string(body)}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "17")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, rateLimitHTTPBody)
			}))
			defer upstream.Close()
			base := regressionProxy(t, upstream, format, []Model{
				{ID: "visible", Target: "account/target", DisplayName: "Configured model", ContextWindow: 8192, MaxContextWindow: 16384, MaxTokens: 2048, InputModalities: []string{"text", "image"}, ReasoningLevels: []string{"low", "high"}, DefaultReasoning: "high"},
				{ID: "minimal", Target: "account/minimal"},
				{ID: "listed", Target: "account/listed"},
			}, 1)
			client := &http.Client{Timeout: 5 * time.Second}
			assertRateLimitCatalog(t, rateLimitRequest(client, http.MethodGet, base+"/models", ""), format, false)
			assert429 := func(result rateLimitResult) {
				t.Helper()
				if result.err != nil || result.status != http.StatusTooManyRequests || !bytes.Equal(result.body, []byte(rateLimitHTTPBody)) || result.headers.Get("Retry-After") != "17" {
					t.Fatalf("CPA rate limit replaced: status=%d headers=%v body=%s err=%v", result.status, result.headers, result.body, result.err)
				}
			}
			assert429(rateLimitRequest(client, http.MethodPost, base+paths[0], `{"model":"visible","client":"initial","stream":true}`))
			first := <-forwarded
			if first.path != "/"+format+paths[0] || first.body != `{"model":"account/target","client":"initial","stream":true}` || catalogCalls.Load() != 1 {
				t.Fatalf("initial alias route or cache failed: request=%+v catalogCalls=%d", first, catalogCalls.Load())
			}
			missing.Store(true)
			clock.Add(int64(15 * time.Second))
			const clients = 12
			start := make(chan struct{})
			results := make(chan rateLimitResult, clients)
			for i := range clients {
				go func(i int) {
					<-start
					child := &http.Client{Timeout: 5 * time.Second}
					results <- rateLimitRequest(child, http.MethodPost, base+paths[i%len(paths)], fmt.Sprintf(`{"model":"visible","client":"child-%d","stream":true}`, i))
				}(i)
			}
			close(start)
			for range clients {
				assert429(<-results)
			}
			seen := make(map[string]bool)
			for range clients {
				request := <-forwarded
				var payload struct{ Model, Client string }
				if err := json.Unmarshal([]byte(request.body), &payload); err != nil {
					t.Fatal(err)
				}
				i, err := strconv.Atoi(strings.TrimPrefix(payload.Client, "child-"))
				if err != nil || i < 0 || i >= clients || seen[payload.Client] || payload.Model != "account/target" || request.path != "/"+format+paths[i%len(paths)] || request.body != fmt.Sprintf(`{"model":"account/target","client":"child-%d","stream":true}`, i) {
					t.Fatalf("concurrent alias route corrupted: %+v", request)
				}
				seen[payload.Client] = true
			}
			assertRateLimitCatalog(t, rateLimitRequest(client, http.MethodGet, base+"/models", ""), format, true)
			if format == "backend-api" {
				assertRateLimitCatalog(t, rateLimitRequest(client, http.MethodGet, base+"/codex/models", ""), format, true)
			}
			for _, id := range []string{"account/target", "hidden", "not-configured"} {
				result := rateLimitRequest(client, http.MethodPost, base+paths[0], fmt.Sprintf(`{"model":%q}`, id))
				var payload struct {
					Error struct{ Type string } `json:"error"`
				}
				if result.err != nil || result.status != http.StatusNotFound || json.Unmarshal(result.body, &payload) != nil || payload.Error.Type != "adapter_error" {
					t.Fatalf("whitelist stopped rejecting %q: status=%d body=%s err=%v", id, result.status, result.body, result.err)
				}
			}
			if inferenceCalls.Load() != clients+1 || catalogCalls.Load() != 2 {
				t.Fatalf("missing targets changed inference or cache boundaries: inference=%d catalog=%d", inferenceCalls.Load(), catalogCalls.Load())
			}
		})
	}
}

func TestMissingCatalogTargetPreservesCPA404AndMappingConflicts(t *testing.T) {
	for _, format := range []string{"v1", "backend-api"} {
		for _, scenario := range []struct {
			name      string
			ids       []string
			status    int
			catalog   int
			inference int32
		}{
			{"never-listed", []string{"hidden"}, http.StatusNotFound, http.StatusOK, 1},
			{"duplicate-target", []string{"target", "target", "hidden"}, http.StatusNotFound, http.StatusOK, 0},
			{"alias-collision-with-missing-target", []string{"alias", "hidden"}, http.StatusConflict, http.StatusConflict, 0},
		} {
			t.Run(format+"/"+scenario.name, func(t *testing.T) {
				catalogTestCache(t, 15*time.Second, time.Now)
				var inferenceCalls atomic.Int32
				forwarded := make(chan string, 32)
				const unknownBody = `{"error":{"type":"invalid_request_error","code":"model_not_found","message":"CPA does not serve this target"},"request_id":"cpa-404-decision"}`
				suffix := "/responses"
				if format == "backend-api" {
					suffix = "/codex/responses"
				}
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer upstream-secret" {
						http.Error(w, "wrong upstream identity", http.StatusUnauthorized)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					if r.URL.Path == "/v1/models" {
						listKey, idKey := rateLimitCatalogKeys(format)
						models := make([]map[string]string, 0, len(scenario.ids))
						for _, id := range scenario.ids {
							models = append(models, map[string]string{idKey: id})
						}
						_ = json.NewEncoder(w).Encode(map[string]any{listKey: models})
						return
					}
					if r.Method != http.MethodPost || r.URL.Path != "/"+format+suffix {
						http.NotFound(w, r)
						return
					}
					body, _ := io.ReadAll(r.Body)
					inferenceCalls.Add(1)
					forwarded <- string(body)
					var payload struct{ Model string }
					if json.Unmarshal(body, &payload) != nil || payload.Model != "target" {
						http.Error(w, "unexpected model instead of configured target", http.StatusBadRequest)
						return
					}
					w.Header().Set("X-Cpa-Decision", "model-not-found")
					w.WriteHeader(http.StatusNotFound)
					_, _ = io.WriteString(w, unknownBody)
				}))
				defer upstream.Close()
				base := regressionProxy(t, upstream, format, []Model{{ID: "alias", Target: "target"}}, 1)
				client := &http.Client{Timeout: 5 * time.Second}
				catalogResult := rateLimitRequest(client, http.MethodGet, base+"/models", "")
				if catalogResult.err != nil || catalogResult.status != scenario.catalog {
					t.Fatalf("catalog conflict contract changed: status=%d body=%s err=%v", catalogResult.status, catalogResult.body, catalogResult.err)
				}
				if scenario.catalog == http.StatusOK {
					listKey, idKey := rateLimitCatalogKeys(format)
					var payload map[string]json.RawMessage
					var models []map[string]any
					if json.Unmarshal(catalogResult.body, &payload) != nil || json.Unmarshal(payload[listKey], &models) != nil {
						t.Fatalf("invalid catalog: %s", catalogResult.body)
					}
					if scenario.inference == 1 {
						if len(models) != 1 || models[0][idKey] != "alias" {
							t.Fatalf("never-listed configured ID disappeared or leaked hidden models: %s", catalogResult.body)
						}
					} else if len(models) != 0 {
						t.Fatalf("ambiguous target was advertised: %s", catalogResult.body)
					}
					if catalogResult.headers.Get("X-Adapter-Warning") != "unavailable-model-mappings" || catalogResult.headers.Get("X-Adapter-Warning-Count") != "1" {
						t.Fatalf("catalog warning lost: %v", catalogResult.headers)
					}
				}
				result := rateLimitRequest(client, http.MethodPost, base+suffix, `{"model":"alias"}`)
				if result.err != nil || result.status != scenario.status || inferenceCalls.Load() != scenario.inference {
					t.Fatalf("mapping boundary changed: status=%d body=%s calls=%d err=%v", result.status, result.body, inferenceCalls.Load(), result.err)
				}
				if scenario.inference == 1 {
					if string(result.body) != unknownBody || result.headers.Get("X-Cpa-Decision") != "model-not-found" || <-forwarded != `{"model":"target"}` {
						t.Fatalf("CPA's actual missing-model decision was replaced: headers=%v body=%s", result.headers, result.body)
					}
				}
				for _, id := range []string{"target", "hidden", "wrong-id"} {
					status, body := regressionRequest(t, http.MethodPost, base+suffix, strings.NewReader(fmt.Sprintf(`{"model":%q}`, id)), nil)
					if status != http.StatusNotFound || inferenceCalls.Load() != scenario.inference {
						t.Fatalf("non-whitelisted ID %q reached inference: status=%d body=%s calls=%d", id, status, body, inferenceCalls.Load())
					}
				}
			})
		}
	}
}

func TestWebSocketRateLimitSurvivesMissingCatalog(t *testing.T) {
	for _, format := range []string{"v1", "backend-api"} {
		t.Run(format, func(t *testing.T) {
			var clock atomic.Int64
			clock.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
			catalogTestCache(t, 15*time.Second, func() time.Time { return time.Unix(0, clock.Load()) })
			f := newWSRegressionFixture(t, false, format)
			assertCreate429 := func(kind int, requestID string) {
				t.Helper()
				wsRegressionWrite(t, f.client, kind, fmt.Sprintf(`{"type":"response.create","model":"alias-a","request_id":%q}`, requestID))
				f.expectUpstreamBytes(t, kind, fmt.Sprintf(`{"type":"response.create","model":"target-a","request_id":%q}`, requestID))
				wsRegressionWrite(t, f.upstream, kind, rateLimitWSBody)
				frame := f.readClient(t)
				if frame.kind != kind || !bytes.Equal(frame.data, []byte(rateLimitWSBody)) {
					t.Fatalf("CPA rate-limit frame replaced by adapter error: kind=%d body=%s", frame.kind, frame.data)
				}
			}
			assertCreate429(websocket.TextMessage, "initial")
			if f.catalogCalls.Load() != 1 {
				t.Fatalf("initial create did not consult CPA catalog: %d", f.catalogCalls.Load())
			}
			f.catalogMissing.Store(true)
			clock.Add(int64(15 * time.Second))
			wsRegressionWrite(t, f.client, websocket.TextMessage, `{"type":"response.create","model":"target-a"}`)
			f.expectLocalError(t, http.StatusNotFound)
			assertCreate429(websocket.TextMessage, "after-expiry-text")
			assertCreate429(websocket.BinaryMessage, "after-expiry-binary")
			if f.catalogCalls.Load() != 2 {
				t.Fatalf("WebSocket did not refresh and reuse the missing-target catalog: %d", f.catalogCalls.Load())
			}
		})
	}
}

func TestManagementCatalogWarnsWithoutInventingMissingTargets(t *testing.T) {
	for _, format := range []string{"v1", "backend-api"} {
		t.Run(format, func(t *testing.T) {
			catalogTestCache(t, 15*time.Second, time.Now)
			var catalogCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer upstream-secret" {
					http.Error(w, "unexpected management catalog request", http.StatusUnauthorized)
					return
				}
				catalogCalls.Add(1)
				listKey, _ := rateLimitCatalogKeys(format)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{listKey: []any{}})
			}))
			defer upstream.Close()
			cfg := catalogConfig(upstream)
			cfg.Format = format
			cfg.Mappings = []Model{{ID: "configured-alias", Target: "account/missing", ContextWindow: 8192, InputModalities: []string{"text"}}}
			input, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			response := (&RPC{Manager: NewManager()}).handleManagement(managementRequest{
				Method: http.MethodPost,
				Path:   "/v0/management/plugins/cpa-api-adapter/catalog",
				Body:   input,
			})
			var payload struct {
				Models []struct {
					ID string `json:"id"`
				} `json:"models"`
				Warnings []string `json:"warnings"`
			}
			if response.StatusCode != http.StatusOK || json.Unmarshal(response.Body, &payload) != nil || payload.Models == nil || len(payload.Models) != 0 || len(payload.Warnings) != 1 || !strings.Contains(payload.Warnings[0], "account/missing") || catalogCalls.Load() != 1 {
				t.Fatalf("management catalog fabricated a model or lost the actual absence warning: status=%d calls=%d body=%s", response.StatusCode, catalogCalls.Load(), response.Body)
			}
			if bytes.Contains(response.Body, []byte("client-secret")) || bytes.Contains(response.Body, []byte("upstream-secret")) {
				t.Fatalf("management warning leaked credentials: %s", response.Body)
			}
		})
	}
}
