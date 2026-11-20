package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"net/http"
	"slices"
	"sync"
	"time"
)

// Keys retain only a digest, so client headers and arbitrary queries cannot
// consume unbounded cache memory. Parsed catalogs are immutable after loading.
var modelCatalogCache = newCatalogCache(15*time.Second, 32<<20, 256, 32, time.Now)

type catalogKey [sha256.Size]byte

type catalogEntry struct {
	value   *catalog
	expires time.Time
	bytes   int64
}

type catalogFlight struct {
	done       chan struct{}
	cancel     context.CancelFunc
	generation uint64
	waiters    int
	finished   bool
	value      *catalog
	err        error
}

type catalogCache struct {
	mu            sync.Mutex
	entries       map[catalogKey]catalogEntry
	flights       map[catalogKey]*catalogFlight
	activeFlights int
	generation    uint64
	bytes         int64
	ttl           time.Duration
	maxBytes      int64
	maxEntries    int
	maxFlights    int
	now           func() time.Time
}

func newCatalogCache(ttl time.Duration, maxBytes int64, maxEntries, maxFlights int, now func() time.Time) *catalogCache {
	return &catalogCache{
		entries: make(map[catalogKey]catalogEntry), flights: make(map[catalogKey]*catalogFlight),
		ttl: ttl, maxBytes: maxBytes, maxEntries: maxEntries, maxFlights: maxFlights, now: now,
	}
}

func catalogRequestKey(req *http.Request, cfg APIConfig) catalogKey {
	h := sha256.New()
	var length [8]byte
	add := func(value string) {
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = h.Write(length[:])
		_, _ = h.Write([]byte(value))
	}
	add(req.URL.String())
	add(cfg.Format)
	add(cfg.UpstreamAPIKey)
	names := make([]string, 0, len(req.Header))
	for name := range req.Header {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		add(name)
		binary.BigEndian.PutUint64(length[:], uint64(len(req.Header[name])))
		_, _ = h.Write(length[:])
		for _, value := range req.Header[name] {
			add(value)
		}
	}
	var key catalogKey
	h.Sum(key[:0])
	return key
}

func (c *catalogCache) get(ctx context.Context, key catalogKey, load func(context.Context) (*catalog, error)) (*catalog, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if entry, ok := c.entries[key]; ok {
		if c.now().Before(entry.expires) {
			c.mu.Unlock()
			return entry.value, nil
		}
		c.bytes -= entry.bytes
		delete(c.entries, key)
	}
	flight := c.flights[key]
	if flight == nil {
		if c.activeFlights >= c.maxFlights {
			c.mu.Unlock()
			return nil, &apiError{http.StatusServiceUnavailable, "CPA 模型目录请求过多，请稍后重试"}
		}
		loadCtx, cancel := context.WithCancel(context.Background())
		flight = &catalogFlight{done: make(chan struct{}), cancel: cancel, generation: c.generation}
		c.flights[key] = flight
		c.activeFlights++
		go c.load(loadCtx, key, flight, load)
	}
	flight.waiters++
	c.mu.Unlock()
	select {
	case <-flight.done:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return flight.value, flight.err
	case <-ctx.Done():
		c.mu.Lock()
		flight.waiters--
		if !flight.finished && flight.waiters == 0 {
			if c.flights[key] == flight {
				delete(c.flights, key)
			}
			flight.cancel()
		}
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (c *catalogCache) load(ctx context.Context, key catalogKey, flight *catalogFlight, load func(context.Context) (*catalog, error)) {
	value, err := load(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.activeFlights--
	flight.value, flight.err, flight.finished = value, err, true
	if c.flights[key] == flight {
		delete(c.flights, key)
		if err == nil && flight.waiters > 0 && flight.generation == c.generation {
			c.store(key, value)
		}
	}
	close(flight.done)
	flight.cancel()
}

func (c *catalogCache) store(key catalogKey, value *catalog) {
	bytes := catalogMemory(value)
	if bytes > c.maxBytes || c.maxEntries <= 0 {
		return
	}
	now := c.now()
	for key, entry := range c.entries {
		if !now.Before(entry.expires) {
			c.bytes -= entry.bytes
			delete(c.entries, key)
		}
	}
	for len(c.entries) >= c.maxEntries || c.bytes+bytes > c.maxBytes {
		var oldestKey catalogKey
		var oldest time.Time
		for candidate, entry := range c.entries {
			if oldest.IsZero() || entry.expires.Before(oldest) {
				oldestKey, oldest = candidate, entry.expires
			}
		}
		c.bytes -= c.entries[oldestKey].bytes
		delete(c.entries, oldestKey)
	}
	c.entries[key] = catalogEntry{value: value, expires: now.Add(c.ttl), bytes: bytes}
	c.bytes += bytes
}

// Conservatively account for retained raw buffers, strings, slice capacities,
// maps and entries, including the duplicate decoded model fields.
func catalogMemory(c *catalog) int64 {
	bytes := int64(512 + cap(c.models)*8)
	for key, value := range c.payload {
		bytes += int64(256 + len(key) + cap(value))
	}
	for _, model := range c.models {
		bytes += 256
		for key, value := range model {
			bytes += int64(256 + len(key) + cap(value))
		}
	}
	for id, entries := range c.byID {
		bytes += int64(256 + len(id) + cap(entries)*8)
	}
	return bytes
}

func (c *catalogCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation++
	clear(c.entries)
	clear(c.flights)
	// Old accepted requests retain their slots until loading finishes, so
	// repeated reconfiguration cannot bypass the active-request limit.
	c.bytes = 0
}
