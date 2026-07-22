package handlers

import (
	"sync"
	"time"

	"github.com/jagadeesh/grainlify/backend/internal/metrics"
)

// projectsCacheEntry holds one serialised JSON response together with its
// pre-computed expiry time.
type projectsCacheEntry struct {
	body      []byte
	expiresAt time.Time
}

// ProjectsCache is a concurrency-safe, short-TTL in-memory cache for the
// public projects endpoints (List, Recommended, FilterOptions, Get/:id).
//
// # Design
//
//   - Keys for list-style endpoints (List, Recommended, FilterOptions) are
//     opaque strings built from the full query string so that different filter
//     combinations never collide.
//   - Keys for the detail endpoint (Get/:id) are the project UUID string.
//   - A dedicated "list" namespace and a "project:<id>" namespace keep the two
//     key spaces apart, which lets Invalidate("list") clear all list variants
//     while leaving detail entries untouched, and Invalidate("project:<id>")
//     clear a specific project's detail entry.
//
// # TTL and staleness
//
// Default TTL is 30 s.  An admin edit is reflected within at most TTL seconds
// for the remaining window on a hot entry, or immediately if the write path
// calls InvalidateAll / InvalidateProject(id).  Callers in admin/update handlers
// must call one of those methods to satisfy the "promptly" acceptance criterion.
//
// # Security
//
// All cached keys are derived from public query parameters only — never from
// auth headers or user-specific fields.  This endpoint is intentionally
// unauthenticated and the cached body contains no per-user data, so serving
// the same cached response to multiple callers is safe.
//
// # Stampede protection
//
// A singleflight-style in-flight map (inflight) ensures that concurrent cache
// misses for the same key result in exactly one DB round-trip.  Subsequent
// waiters block on the first fetcher's result channel rather than hammering the
// database simultaneously.
type ProjectsCache struct {
	mu      sync.RWMutex
	entries map[string]projectsCacheEntry
	ttl     time.Duration
	now     func() time.Time // injectable for testing

	// inflight tracks in-progress fetches so concurrent cache misses for the
	// same key share a single DB call (stampede protection).
	inflightMu sync.Mutex
	inflight   map[string]*inflightCall
}

// inflightCall represents one in-progress cache-fill operation.
type inflightCall struct {
	done chan struct{}
	body []byte
	err  error
}

// NewProjectsCache constructs a ProjectsCache with the given TTL and starts a
// background eviction goroutine that stops when stopCh is closed.
func NewProjectsCache(ttl time.Duration, stopCh <-chan struct{}) *ProjectsCache {
	c := &ProjectsCache{
		entries:  make(map[string]projectsCacheEntry),
		ttl:      ttl,
		now:      time.Now,
		inflight: make(map[string]*inflightCall),
	}
	go c.evictLoop(stopCh)
	return c
}

// Get returns the cached body for key, or (nil, false) on a miss / expired entry.
// It records a hit or miss counter against the given route label.
func (c *ProjectsCache) Get(key, route string) ([]byte, bool) {
	if c.ttl <= 0 {
		metrics.ProjectsPublicCache.WithLabelValues(route, "miss").Inc()
		return nil, false
	}

	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()

	if !ok || c.now().After(entry.expiresAt) {
		if ok {
			// lazy eviction
			c.mu.Lock()
			if e, still := c.entries[key]; still && c.now().After(e.expiresAt) {
				delete(c.entries, key)
			}
			c.mu.Unlock()
		}
		metrics.ProjectsPublicCache.WithLabelValues(route, "miss").Inc()
		return nil, false
	}

	metrics.ProjectsPublicCache.WithLabelValues(route, "hit").Inc()
	return entry.body, true
}

// Set stores body under key with a TTL-derived expiry.  No-op when TTL ≤ 0.
func (c *ProjectsCache) Set(key string, body []byte) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	c.entries[key] = projectsCacheEntry{
		body:      body,
		expiresAt: c.now().Add(c.ttl),
	}
	c.mu.Unlock()
}

// Invalidate removes all cache entries whose key has the given prefix.
// Use "list:" to clear all list/filter variants, "project:" to clear all
// detail entries, or a specific "project:<uuid>" to clear one project.
func (c *ProjectsCache) Invalidate(prefix string) {
	c.mu.Lock()
	for k := range c.entries {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(c.entries, k)
		}
	}
	c.mu.Unlock()
}

// InvalidateAll clears every entry — used when a structural change (e.g. an
// ecosystem rename) could affect many list results simultaneously.
func (c *ProjectsCache) InvalidateAll() {
	c.mu.Lock()
	c.entries = make(map[string]projectsCacheEntry)
	c.mu.Unlock()
}

// InvalidateProject removes the cached detail entry for a single project ID,
// plus all list variants (since the project's metadata may appear in lists).
func (c *ProjectsCache) InvalidateProject(projectID string) {
	c.Invalidate("project:" + projectID)
	c.Invalidate("list:")
	c.Invalidate("recommended:")
	c.Invalidate("filters:")
}

// Do executes fetch if key is not cached, deduplicating concurrent calls for
// the same key (stampede protection).  On success the result is stored in the
// cache.  fetch must return the raw JSON bytes to be stored and returned.
func (c *ProjectsCache) Do(key, route string, fetch func() ([]byte, error)) ([]byte, error) {
	if body, ok := c.Get(key, route); ok {
		return body, nil
	}

	// Not in cache — check/register in-flight map.
	c.inflightMu.Lock()
	if call, exists := c.inflight[key]; exists {
		// Another goroutine is already fetching this key; wait for it.
		c.inflightMu.Unlock()
		<-call.done
		return call.body, call.err
	}
	call := &inflightCall{done: make(chan struct{})}
	c.inflight[key] = call
	c.inflightMu.Unlock()

	// This goroutine owns the fetch.
	body, err := fetch()
	call.body, call.err = body, err
	close(call.done)

	c.inflightMu.Lock()
	delete(c.inflight, key)
	c.inflightMu.Unlock()

	if err == nil {
		c.Set(key, body)
	}
	return body, err
}

// Len returns the number of entries currently stored (including expired ones
// not yet swept).
func (c *ProjectsCache) Len() int {
	c.mu.RLock()
	n := len(c.entries)
	c.mu.RUnlock()
	return n
}

// evictLoop periodically removes expired entries.  It runs at TTL/2, minimum 5 s.
func (c *ProjectsCache) evictLoop(stopCh <-chan struct{}) {
	if c.ttl <= 0 {
		return
	}
	interval := c.ttl / 2
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.evictExpired()
		case <-stopCh:
			return
		}
	}
}

func (c *ProjectsCache) evictExpired() {
	now := c.now()
	c.mu.Lock()
	for k, e := range c.entries {
		if now.After(e.expiresAt) {
			delete(c.entries, k)
		}
	}
	c.mu.Unlock()
}
