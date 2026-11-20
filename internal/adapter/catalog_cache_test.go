package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Observing Done lets tests wait until each consumer is waiting without sleeps
// or assertions about cache internals.
type catalogWaitingContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *catalogWaitingContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func catalogContext(ctx context.Context) *catalogWaitingContext {
	return &catalogWaitingContext{Context: ctx, waiting: make(chan struct{})}
}

func awaitCatalogSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("catalog operation did not reach expected state")
	}
}

func catalogTestCache(t *testing.T, ttl time.Duration, now func() time.Time) {
	t.Helper()
	previous := modelCatalogCache
	modelCatalogCache = newCatalogCache(ttl, 1<<20, 32, 32, now)
	t.Cleanup(func() { modelCatalogCache = previous })
}

func catalogConfig(server *httptest.Server) APIConfig {
	return APIConfig{APIKey: "client-secret", Upstream: server.URL, Prefix: "/work", Format: "backend-api"}
}

func TestCatalogConcurrentConsumersShareFetch(t *testing.T) {
	catalogTestCache(t, time.Minute, time.Now)
	var calls atomic.Int32
	release := make(chan struct{})
	releaseFetch := sync.OnceFunc(func() { close(release) })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-release:
			_, _ = io.WriteString(w, `{"models":[{"slug":"available"}]}`)
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	defer releaseFetch()
	results := make(chan error, 16)
	contexts := make([]*catalogWaitingContext, 16)
	for i := range contexts {
		contexts[i] = catalogContext(context.Background())
		go func(ctx context.Context) {
			_, err := fetchCatalog(ctx, catalogConfig(upstream), nil, "")
			results <- err
		}(contexts[i])
	}
	for _, ctx := range contexts {
		awaitCatalogSignal(t, ctx.waiting)
	}
	releaseFetch()
	for range contexts {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent directory consumers caused %d upstream requests", calls.Load())
	}
}

func TestCatalogCancellationDoesNotCancelActiveConsumer(t *testing.T) {
	catalogTestCache(t, time.Minute, time.Now)
	var calls atomic.Int32
	started, canceled := make(chan struct{}), make(chan struct{})
	release := make(chan struct{})
	releaseFetch := sync.OnceFunc(func() { close(release) })
	markStarted := sync.OnceFunc(func() { close(started) })
	markCanceled := sync.OnceFunc(func() { close(canceled) })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		markStarted()
		select {
		case <-release:
			_, _ = io.WriteString(w, `{"models":[{"slug":"available"}]}`)
		case <-r.Context().Done():
			markCanceled()
		}
	}))
	defer upstream.Close()
	defer releaseFetch()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	leader, follower := catalogContext(ctx), catalogContext(context.Background())
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { _, err := fetchCatalog(leader, catalogConfig(upstream), nil, ""); first <- err }()
	awaitCatalogSignal(t, started)
	go func() { _, err := fetchCatalog(follower, catalogConfig(upstream), nil, ""); second <- err }()
	awaitCatalogSignal(t, follower.waiting)
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled consumer returned %v", err)
	}
	select {
	case <-canceled:
		t.Fatal("canceling the first consumer canceled an active consumer's fetch")
	default:
	}
	releaseFetch()
	if err := <-second; err != nil || calls.Load() != 1 {
		t.Fatalf("active consumer lost shared fetch: %v, requests=%d", err, calls.Load())
	}
}

func TestCatalogAllConsumersCancelUpstreamAndAllowRetry(t *testing.T) {
	catalogTestCache(t, time.Minute, time.Now)
	var calls atomic.Int32
	started, canceled := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
			<-r.Context().Done()
			close(canceled)
			return
		}
		_, _ = io.WriteString(w, `{"models":[{"slug":"retry"}]}`)
	}))
	defer upstream.Close()
	results := make(chan error, 2)
	var cancels []context.CancelFunc
	for range 2 {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cancels = append(cancels, cancel)
		waiting := catalogContext(ctx)
		go func() { _, err := fetchCatalog(waiting, catalogConfig(upstream), nil, ""); results <- err }()
		awaitCatalogSignal(t, waiting.waiting)
	}
	awaitCatalogSignal(t, started)
	for _, cancel := range cancels {
		cancel()
	}
	for range 2 {
		if err := <-results; !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled consumer returned %v", err)
		}
	}
	awaitCatalogSignal(t, canceled)
	if _, err := fetchCatalog(context.Background(), catalogConfig(upstream), nil, ""); err != nil || calls.Load() != 2 {
		t.Fatalf("all-consumer cancellation prevented a fresh retry: %v, requests=%d", err, calls.Load())
	}
}

func TestCatalogSeparatesEffectiveQueryKeysAndForwardedHeaders(t *testing.T) {
	catalogTestCache(t, time.Minute, time.Now)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stamp := fmt.Sprint(calls.Add(1))
		_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{map[string]string{"slug": stamp}}, "data": []any{map[string]string{"id": stamp}}})
	}))
	defer upstream.Close()
	cfg := catalogConfig(upstream)
	headers := http.Header{"Authorization": {"Bearer client-secret"}, "Chatgpt-Account-Id": {"account-a"}, "User-Agent": {"client-a"}, "Originator": {"origin-a"}, "Version": {"version-a"}}
	fetch := func(cfg APIConfig, headers http.Header, query string) string {
		t.Helper()
		c, err := fetchCatalog(context.Background(), cfg, headers, query)
		if err != nil {
			t.Fatal(err)
		}
		var id string
		if err := json.Unmarshal(c.models[0][c.idKey], &id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	baseline := fetch(cfg, headers, "alpha=1&beta=2")
	if fetch(cfg, headers, "beta=2&alpha=1&client_version=0.159.0") != baseline {
		t.Fatal("equivalent normalized queries did not share a catalog")
	}
	for _, name := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Chatgpt-Account-Id", "User-Agent", "Originator", "Version"} {
		t.Run(name, func(t *testing.T) {
			changed := headers.Clone()
			changed.Set(name, "different")
			id := fetch(cfg, changed, "alpha=1&beta=2")
			if id == baseline || fetch(cfg, changed, "beta=2&alpha=1") != id {
				t.Fatal("different forwarded credentials/client identity leaked or failed to reuse its own catalog")
			}
		})
	}
	changed := cfg
	changed.APIKey = "another-key"
	if fetch(changed, headers, "alpha=1&beta=2") == baseline {
		t.Fatal("different configured keys shared a catalog")
	}
	if fetch(cfg, headers, "alpha=1&beta=3") == baseline {
		t.Fatal("distinct effective query returned another query's catalog")
	}
	changed = cfg
	changed.Format = "v1"
	v1 := fetch(changed, headers, "alpha=1&beta=2&client_version=ignored")
	if v1 == baseline || fetch(changed, headers, "beta=2&alpha=1") != v1 {
		t.Fatal("format isolation or effective client_version normalization failed")
	}
	changed = cfg
	changed.Upstream += "/other"
	if fetch(changed, headers, "alpha=1&beta=2") == baseline {
		t.Fatal("different upstream directory URLs shared a catalog")
	}
}

func TestCatalogExpiredDataIsNotReturned(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	catalogTestCache(t, time.Minute, func() time.Time { return time.Unix(0, clock.Load()) })
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"models":[{"slug":"version-%d"}]}`, calls.Add(1))
	}))
	defer upstream.Close()
	cfg := catalogConfig(upstream)
	fetch := func() string {
		t.Helper()
		c, err := fetchCatalog(context.Background(), cfg, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		body, _, err := c.render(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	before := fetch()
	if fetch() != before || calls.Load() != 1 {
		t.Fatal("fresh catalog was not reused")
	}
	clock.Add(int64(time.Minute))
	if fetch() == before || calls.Load() != 2 {
		t.Fatal("expired catalog was returned instead of fetching current models")
	}
}

func TestCachedCatalogConcurrentWhitelistsDoNotContaminate(t *testing.T) {
	catalogTestCache(t, time.Minute, time.Now)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"models":[{"slug":"first"},{"slug":"second"}],"value":9007199254740993}`)
	}))
	defer upstream.Close()
	cfg := catalogConfig(upstream)
	c, err := fetchCatalog(context.Background(), cfg, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 3)
	for _, mappings := range [][]Model{nil, {{ID: "alias-a", Target: "first"}}, {{ID: "alias-b", Target: "second"}}} {
		go func(mappings []Model) {
			local := cfg
			local.Mappings = mappings
			for range 40 {
				body, warnings, err := c.render(local)
				var payload struct{ Models []struct{ Slug string } }
				if err != nil || len(warnings) != 0 || json.Unmarshal(body, &payload) != nil || !strings.Contains(string(body), `9007199254740993`) {
					results <- fmt.Errorf("render failed: %s %v %v", body, warnings, err)
					return
				}
				if len(mappings) == 0 && (len(payload.Models) != 2 || payload.Models[0].Slug != "first" || payload.Models[1].Slug != "second") || len(mappings) > 0 && (len(payload.Models) != 1 || payload.Models[0].Slug != mappings[0].ID) {
					results <- fmt.Errorf("whitelist polluted by another consumer: %s", body)
					return
				}
			}
			results <- nil
		}(mappings)
	}
	for range 3 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	cached, err := fetchCatalog(context.Background(), cfg, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := cached.render(cfg)
	if err != nil || !strings.Contains(string(body), `"slug":"first"`) || !strings.Contains(string(body), `"slug":"second"`) || calls.Load() != 1 {
		t.Fatalf("cached upstream catalog was mutated: %s %v, requests=%d", body, err, calls.Load())
	}
}

func TestCatalogSaturationPreservesSharedConsumersAndReleasesSlots(t *testing.T) {
	previous := modelCatalogCache
	modelCatalogCache = newCatalogCache(time.Minute, 1<<20, 32, 2, time.Now)
	t.Cleanup(func() { modelCatalogCache = previous })
	var calls atomic.Int32
	startedA, startedB := make(chan struct{}), make(chan struct{})
	releaseA, releaseB := make(chan struct{}), make(chan struct{})
	finishA := sync.OnceFunc(func() { close(releaseA) })
	finishB := sync.OnceFunc(func() { close(releaseB) })
	markA := sync.OnceFunc(func() { close(startedA) })
	markB := sync.OnceFunc(func() { close(startedB) })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var release <-chan struct{}
		switch r.URL.Query().Get("scope") {
		case "a":
			markA()
			release = releaseA
		case "b":
			markB()
			release = releaseB
		}
		if release != nil {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		_, _ = io.WriteString(w, `{"models":[{"slug":"available"}]}`)
	}))
	defer upstream.Close()
	defer finishA()
	defer finishB()
	cfg := catalogConfig(upstream)
	first, second, follower := make(chan error, 1), make(chan error, 1), make(chan error, 1)
	start := func(scope string, result chan<- error) *catalogWaitingContext {
		waiting := catalogContext(context.Background())
		go func() { _, err := fetchCatalog(waiting, cfg, nil, "scope="+scope); result <- err }()
		return waiting
	}
	start("a", first)
	awaitCatalogSignal(t, startedA)
	start("b", second)
	awaitCatalogSignal(t, startedB)
	shared := start("a", follower)
	awaitCatalogSignal(t, shared.waiting)
	assertSaturated := func() {
		t.Helper()
		_, err := fetchCatalog(context.Background(), cfg, nil, "scope=overflow")
		var ae *apiError
		if !errors.As(err, &ae) || ae.Status != http.StatusServiceUnavailable || calls.Load() != 2 {
			t.Fatalf("saturated cache admitted an extra upstream request: %v, requests=%d", err, calls.Load())
		}
	}
	assertSaturated()
	modelCatalogCache.clear()
	assertSaturated()
	finishA()
	for _, result := range []<-chan error{first, follower} {
		if err := <-result; err != nil {
			t.Fatalf("saturation disrupted an accepted/shared consumer: %v", err)
		}
	}
	if _, err := fetchCatalog(context.Background(), cfg, nil, "scope=overflow"); err != nil || calls.Load() != 3 {
		t.Fatalf("finished request did not release capacity: %v, requests=%d", err, calls.Load())
	}
	finishB()
	if err := <-second; err != nil {
		t.Fatal(err)
	}
}
