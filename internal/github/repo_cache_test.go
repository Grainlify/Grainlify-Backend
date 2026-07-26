package github

// Tests for RepoCache and GetRepoWithCache.
//
// All tests are self-contained — no network, no database, no real GitHub token.
// The injectable `now` clock on RepoCache lets us simulate TTL expiry and
// window boundaries without sleeping, making the suite fast and deterministic.
//
// Edge cases covered:
//   - Cache hit within TTL (no extra API call)
//   - Cache miss (first fetch stored, second fetch served from cache)
//   - TTL expiry mid-burst (entry evicted, fresh fetch performed)
//   - Revoked repo access is reflected at the exact TTL boundary
//   - Bypass flag forces a fresh fetch and overwrites the cached entry
//   - Permission-bearing entries are scoped by access token
//   - Repo renamed/transferred between fetches (new name misses, old name still hits until TTL)
//   - Nil cache → falls through to GetRepo directly
//   - TTL==0 → caching disabled, every call is a live fetch
//   - Invalidate removes a specific entry without touching others
//   - Concurrent same-key cache fills share one fetch
//   - Concurrent reads are safe (no data race under -race)
//   - Key is normalised: "Owner/Repo" and "owner/repo" share the same bucket
//   - Eviction sweep removes only expired entries

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── test helpers ──────────────────────────────────────────────────────────────

// newTestCache builds a RepoCache with an injectable clock and no background
// goroutine; eviction is exercised explicitly in unit tests.
func newTestCache(ttl time.Duration, nowFn func() time.Time) *RepoCache {
	return &RepoCache{
		entries: make(map[string]cacheEntry),
		ttl:     ttl,
		now:     nowFn,
	}
}

func newRepoCacheTestClient(handler http.Handler) *Client {
	return &Client{
		HTTP: &http.Client{
			Timeout: 2 * time.Second,
			Transport: repoCacheRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				return rec.Result(), nil
			}),
		},
		UserAgent: "test",
	}
}

type repoCacheRoundTripFunc func(*http.Request) (*http.Response, error)

func (f repoCacheRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func writeRepoResponse(w http.ResponseWriter, id int64, fullName string, private, admin, push, pull bool) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"id":%d,"full_name":%q,"owner":{"id":1,"login":"owner","avatar_url":"https://example.test/avatar.png"},"html_url":"https://github.com/%s","private":%t,"permissions":{"admin":%t,"push":%t,"pull":%t}}`,
		id, fullName, fullName, private, admin, push, pull)
}

// ── RepoCache unit tests ──────────────────────────────────────────────────────

func TestRepoCache_GetMissOnEmpty(t *testing.T) {
	c := newTestCache(time.Minute, time.Now)
	_, ok := c.Get("owner/repo")
	if ok {
		t.Fatal("expected miss on empty cache, got hit")
	}
}

func TestRepoCache_SetThenGetHit(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	c := newTestCache(time.Minute, func() time.Time { return now })

	repo := Repo{ID: 1, FullName: "owner/repo"}
	c.set("owner/repo", repo)

	got, ok := c.Get("owner/repo")
	if !ok {
		t.Fatal("expected hit, got miss")
	}
	if got.ID != 1 {
		t.Fatalf("got repo ID %d, want 1", got.ID)
	}
}

func TestRepoCache_ExpiredEntryIsMiss(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	c := newTestCache(time.Minute, func() time.Time { return now })

	c.set("owner/repo", Repo{ID: 2, FullName: "owner/repo"})

	// Advance clock past TTL.
	now = now.Add(time.Minute + time.Second)
	_, ok := c.Get("owner/repo")
	if ok {
		t.Fatal("expected miss after TTL expiry, got hit")
	}
}

func TestRepoCache_ExpiredEntryIsEvictedLazily(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	c := newTestCache(time.Minute, func() time.Time { return now })

	c.set("owner/repo", Repo{ID: 3, FullName: "owner/repo"})
	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1 after set", c.Len())
	}

	// Expire and trigger lazy eviction via Get.
	now = now.Add(time.Minute + time.Second)
	c.Get("owner/repo")

	if c.Len() != 0 {
		t.Fatalf("Len = %d, want 0 after lazy eviction", c.Len())
	}
}

func TestRepoCache_ExactTTLBoundary(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	now := base
	c := newTestCache(60*time.Second, func() time.Time { return now })

	c.set("owner/repo", Repo{ID: 4, FullName: "owner/repo"})

	now = base.Add(60*time.Second - time.Nanosecond)
	if _, ok := c.Get("owner/repo"); !ok {
		t.Fatal("expected hit immediately before TTL boundary, got miss")
	}

	now = base.Add(60 * time.Second)
	if _, ok := c.Get("owner/repo"); ok {
		t.Fatal("expected miss exactly at TTL boundary, got hit")
	}
}

func TestRepoCache_KeyNormalisationCaseInsensitive(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	c := newTestCache(time.Minute, func() time.Time { return now })

	c.set("Owner/Repo", Repo{ID: 5, FullName: "owner/repo"})

	// Lower-case lookup must hit.
	if _, ok := c.Get("owner/repo"); !ok {
		t.Fatal("expected hit with lower-case key, got miss")
	}
	// Mixed-case lookup must also hit.
	if _, ok := c.Get("OWNER/REPO"); !ok {
		t.Fatal("expected hit with upper-case key, got miss")
	}
}

func TestRepoCache_InvalidateRemovesEntry(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	c := newTestCache(time.Minute, func() time.Time { return now })

	c.set("owner/a", Repo{ID: 6, FullName: "owner/a"})
	c.set("owner/b", Repo{ID: 7, FullName: "owner/b"})

	c.Invalidate("owner/a")

	if _, ok := c.Get("owner/a"); ok {
		t.Fatal("expected miss after Invalidate, got hit")
	}
	if _, ok := c.Get("owner/b"); !ok {
		t.Fatal("sibling entry should still be a hit after Invalidate of different key")
	}
}

func TestRepoCache_InvalidateRemovesTokenScopedEntries(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	c := newTestCache(time.Minute, func() time.Time { return now })

	c.set(repoCacheScopedKey("token-a", "owner/a"), Repo{ID: 61, FullName: "owner/a"})
	c.set(repoCacheScopedKey("token-b", "owner/a"), Repo{ID: 62, FullName: "owner/a"})
	c.set(repoCacheScopedKey("token-a", "owner/b"), Repo{ID: 63, FullName: "owner/b"})

	c.Invalidate("owner/a")

	if _, ok := c.Get(repoCacheScopedKey("token-a", "owner/a")); ok {
		t.Fatal("expected token-a owner/a entry to be invalidated")
	}
	if _, ok := c.Get(repoCacheScopedKey("token-b", "owner/a")); ok {
		t.Fatal("expected token-b owner/a entry to be invalidated")
	}
	if _, ok := c.Get(repoCacheScopedKey("token-a", "owner/b")); !ok {
		t.Fatal("sibling repo should remain cached")
	}
}

func TestRepoCache_InvalidateNonExistentIsNoOp(t *testing.T) {
	c := newTestCache(time.Minute, time.Now)
	// Must not panic.
	c.Invalidate("owner/nonexistent")
}

func TestRepoCache_TTLZeroDisablesCache(t *testing.T) {
	c := newTestCache(0, time.Now)
	c.set("owner/repo", Repo{ID: 8, FullName: "owner/repo"})
	if _, ok := c.Get("owner/repo"); ok {
		t.Fatal("expected cache disabled (TTL=0), but got hit")
	}
	if c.Len() != 0 {
		t.Fatalf("Len = %d, want 0 when TTL=0", c.Len())
	}
}

func TestRepoCache_EvictExpiredSweep(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	c := newTestCache(time.Minute, func() time.Time { return now })

	c.set("owner/old", Repo{ID: 9, FullName: "owner/old"})
	c.set("owner/fresh", Repo{ID: 10, FullName: "owner/fresh"})

	// Advance past TTL for "owner/old" only by re-inserting "owner/fresh"
	// with the advanced clock.
	now = now.Add(time.Minute + time.Second)
	// Re-insert "owner/fresh" at the new time so it has a future expiry.
	c.set("owner/fresh", Repo{ID: 10, FullName: "owner/fresh"})

	c.evictExpired()

	if c.Len() != 1 {
		t.Fatalf("Len = %d after sweep, want 1 (only fresh entry)", c.Len())
	}
	if _, ok := c.Get("owner/fresh"); !ok {
		t.Fatal("fresh entry should survive the sweep")
	}
}

func TestRepoCache_ConcurrentReadsAreSafe(t *testing.T) {
	now := time.Unix(3_000_000, 0)
	c := newTestCache(time.Minute, func() time.Time { return now })
	c.set("owner/repo", Repo{ID: 11, FullName: "owner/repo"})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Get("owner/repo")
		}()
	}
	wg.Wait() // -race will report any data race
}

func TestRepoCache_ConcurrentWritesAreSafe(t *testing.T) {
	c := newTestCache(time.Minute, time.Now)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.set(fmt.Sprintf("owner/repo%d", i), Repo{ID: int64(i), FullName: fmt.Sprintf("owner/repo%d", i)})
		}()
	}
	wg.Wait()
}

// ── GetRepoWithCache integration tests ───────────────────────────────────────

func TestGetRepoWithCache_NilCacheFallsThrough(t *testing.T) {
	var calls int64
	client := newRepoCacheTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		writeRepoResponse(w, 1, "owner/repo", false, false, true, true)
	}))
	for i := 0; i < 2; i++ {
		got, err := client.GetRepoWithCache(context.Background(), "token", "owner/repo", nil, false)
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i+1, err)
		}
		if got.ID != 1 {
			t.Fatalf("call %d: got repo ID %d, want 1", i+1, got.ID)
		}
	}
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Fatalf("nil cache made %d API calls, want 2", got)
	}
}

func TestGetRepoWithCache_TTLZeroFallsThrough(t *testing.T) {
	var calls int64
	client := newRepoCacheTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		writeRepoResponse(w, 1, "owner/repo", false, false, true, true)
	}))

	zeroTTLCache := newTestCache(0, time.Now)

	for i := 0; i < 2; i++ {
		if _, err := client.GetRepoWithCache(context.Background(), "token", "owner/repo", zeroTTLCache, false); err != nil {
			t.Fatalf("call %d: unexpected error: %v", i+1, err)
		}
	}
	if zeroTTLCache.Len() != 0 {
		t.Fatalf("TTL=0: Len = %d after GetRepoWithCache, want 0", zeroTTLCache.Len())
	}
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Fatalf("TTL=0 made %d API calls, want 2", got)
	}
}

func TestGetRepoWithCache_HitServesFromCacheWithoutAPICall(t *testing.T) {
	now := time.Unix(5_000_000, 0)
	cache := newTestCache(time.Minute, func() time.Time { return now })

	token := "token-hit"
	cached := Repo{ID: 42, FullName: "owner/cached-repo"}
	cache.set(repoCacheScopedKey(token, "owner/cached-repo"), cached)

	var apiCalls int64
	client := newRepoCacheTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&apiCalls, 1)
		w.WriteHeader(http.StatusInternalServerError) // fail loudly if reached
	}))

	got, err := client.GetRepoWithCache(context.Background(), token, "owner/cached-repo", cache, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != 42 {
		t.Fatalf("got repo ID %d, want 42", got.ID)
	}
	if got := atomic.LoadInt64(&apiCalls); got != 0 {
		t.Fatalf("expected 0 API calls on cache hit, got %d", got)
	}
}

func TestGetRepoWithCache_MissFetchesAndPopulatesCache(t *testing.T) {
	now := time.Unix(6_000_000, 0)
	cache := newTestCache(time.Minute, func() time.Time { return now })

	var apiCalls int64
	client := newRepoCacheTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&apiCalls, 1)
		writeRepoResponse(w, 7, "owner/miss-repo", false, false, true, true)
	}))

	got, err := client.GetRepoWithCache(context.Background(), "token-miss", "owner/miss-repo", cache, false)
	if err != nil {
		t.Fatalf("unexpected error on cache miss: %v", err)
	}
	if got.ID != 7 {
		t.Fatalf("got ID %d, want 7", got.ID)
	}
	if got := atomic.LoadInt64(&apiCalls); got != 1 {
		t.Fatalf("expected 1 API call after miss, got %d", got)
	}

	got, err = client.GetRepoWithCache(context.Background(), "token-miss", "owner/miss-repo", cache, false)
	if err != nil {
		t.Fatalf("unexpected error on cache hit: %v", err)
	}
	if got.ID != 7 {
		t.Fatalf("got ID %d, want 7", got.ID)
	}
	if got := atomic.LoadInt64(&apiCalls); got != 1 {
		t.Fatalf("expected second call to hit cache; API calls = %d, want 1", got)
	}
}

func TestGetRepoWithCache_BypassForcesAPICall(t *testing.T) {
	now := time.Unix(7_000_000, 0)
	cache := newTestCache(time.Minute, func() time.Time { return now })

	var apiCalls int64
	client := newRepoCacheTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := atomic.AddInt64(&apiCalls, 1)
		writeRepoResponse(w, call, "owner/bypass-repo", call > 1, false, true, true)
	}))

	got, err := client.GetRepoWithCache(context.Background(), "token-bypass", "owner/bypass-repo", cache, false)
	if err != nil {
		t.Fatalf("initial miss: %v", err)
	}
	if got.ID != 1 || got.Private {
		t.Fatalf("initial repo = {ID:%d Private:%t}, want {ID:1 Private:false}", got.ID, got.Private)
	}

	got, err = client.GetRepoWithCache(context.Background(), "token-bypass", "owner/bypass-repo", cache, false)
	if err != nil {
		t.Fatalf("cached hit: %v", err)
	}
	if got.ID != 1 || got.Private {
		t.Fatalf("cached repo = {ID:%d Private:%t}, want stale ID 1 before bypass", got.ID, got.Private)
	}

	got, err = client.GetRepoWithCache(context.Background(), "token-bypass", "owner/bypass-repo", cache, true)
	if err != nil {
		t.Fatalf("bypass refresh: %v", err)
	}
	if got.ID != 2 || !got.Private {
		t.Fatalf("bypass repo = {ID:%d Private:%t}, want refreshed private repo", got.ID, got.Private)
	}

	got, err = client.GetRepoWithCache(context.Background(), "token-bypass", "owner/bypass-repo", cache, false)
	if err != nil {
		t.Fatalf("post-bypass cached hit: %v", err)
	}
	if got.ID != 2 || !got.Private {
		t.Fatalf("post-bypass repo = {ID:%d Private:%t}, want refreshed cached repo", got.ID, got.Private)
	}
	if got := atomic.LoadInt64(&apiCalls); got != 2 {
		t.Fatalf("API calls = %d, want 2 (miss + bypass)", got)
	}
}

func TestGetRepoWithCache_RevokedAccessReflectedAtTTLExpiry(t *testing.T) {
	base := time.Unix(7_100_000, 0)
	now := base
	ttl := 30 * time.Second
	cache := newTestCache(ttl, func() time.Time { return now })

	var calls int64
	var revoked atomic.Bool
	client := newRepoCacheTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		if revoked.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		writeRepoResponse(w, 70, "owner/revoked-repo", false, false, true, true)
	}))

	got, err := client.GetRepoWithCache(context.Background(), "token-revoked", "owner/revoked-repo", cache, false)
	if err != nil {
		t.Fatalf("initial authorized lookup: %v", err)
	}
	if !got.Permissions.Push {
		t.Fatal("initial lookup should have push permission")
	}

	revoked.Store(true)
	now = base.Add(ttl - time.Nanosecond)
	got, err = client.GetRepoWithCache(context.Background(), "token-revoked", "owner/revoked-repo", cache, false)
	if err != nil {
		t.Fatalf("cached lookup before TTL expiry: %v", err)
	}
	if !got.Permissions.Push {
		t.Fatal("expected cached permission immediately before TTL expiry")
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("API calls before TTL expiry = %d, want 1", got)
	}

	now = base.Add(ttl)
	got, err = client.GetRepoWithCache(context.Background(), "token-revoked", "owner/revoked-repo", cache, false)
	if err == nil {
		t.Fatalf("lookup at TTL expiry returned stale repo with push=%t, want GitHub revocation error", got.Permissions.Push)
	}
	apiErr, ok := err.(*GitHubAPIError)
	if !ok || apiErr.StatusCode != http.StatusNotFound {
		t.Fatalf("lookup at TTL expiry error = %T(%v), want 404 GitHubAPIError", err, err)
	}
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Fatalf("API calls after TTL expiry = %d, want 2", got)
	}
	if cache.Len() != 0 {
		t.Fatalf("cache Len = %d after revoked refresh failed, want stale entry evicted", cache.Len())
	}
}

func TestGetRepoWithCache_PermissionsAreScopedByAccessToken(t *testing.T) {
	now := time.Unix(7_200_000, 0)
	cache := newTestCache(time.Minute, func() time.Time { return now })

	var calls int64
	client := newRepoCacheTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		switch r.Header.Get("Authorization") {
		case "Bearer admin-token":
			writeRepoResponse(w, 81, "owner/scoped-repo", false, true, true, true)
		case "Bearer readonly-token":
			writeRepoResponse(w, 82, "owner/scoped-repo", false, false, false, true)
		default:
			t.Errorf("unexpected Authorization header %q", r.Header.Get("Authorization"))
			http.Error(w, "unexpected authorization header", http.StatusUnauthorized)
		}
	}))

	adminRepo, err := client.GetRepoWithCache(context.Background(), "admin-token", "owner/scoped-repo", cache, false)
	if err != nil {
		t.Fatalf("admin lookup: %v", err)
	}
	if !adminRepo.Permissions.Push {
		t.Fatal("admin token should have push permission")
	}

	readOnlyRepo, err := client.GetRepoWithCache(context.Background(), "readonly-token", "owner/scoped-repo", cache, false)
	if err != nil {
		t.Fatalf("readonly lookup: %v", err)
	}
	if readOnlyRepo.Permissions.Push {
		t.Fatal("readonly token received cached push permission from another token")
	}

	adminRepo, err = client.GetRepoWithCache(context.Background(), "admin-token", "owner/scoped-repo", cache, false)
	if err != nil {
		t.Fatalf("second admin lookup: %v", err)
	}
	if !adminRepo.Permissions.Push {
		t.Fatal("admin token should still hit its own scoped permission entry")
	}
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Fatalf("API calls = %d, want 2 token-scoped misses", got)
	}
}

func TestGetRepoWithCache_ConcurrentSameKeyMissesShareFetch(t *testing.T) {
	now := time.Unix(7_300_000, 0)
	cache := newTestCache(time.Minute, func() time.Time { return now })

	var calls int64
	client := newRepoCacheTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		time.Sleep(25 * time.Millisecond)
		writeRepoResponse(w, 90, "owner/concurrent-repo", false, false, true, true)
	}))

	const goroutines = 20
	start := make(chan struct{})
	errs := make(chan error, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, err := client.GetRepoWithCache(context.Background(), "token-concurrent", "owner/concurrent-repo", cache, false)
			if err != nil {
				errs <- err
				return
			}
			if got.ID != 90 || !got.Permissions.Push {
				errs <- fmt.Errorf("got repo ID=%d push=%t, want ID=90 push=true", got.ID, got.Permissions.Push)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		return
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("concurrent same-key misses made %d API calls, want 1", got)
	}
	if cache.Len() != 1 {
		t.Fatalf("cache Len = %d after concurrent fill, want 1", cache.Len())
	}
}

func TestGetRepoWithCache_ExpiryMidBurst(t *testing.T) {
	now := time.Unix(8_000_000, 0)
	cache := newTestCache(30*time.Second, func() time.Time { return now })

	cache.set("owner/expiry-repo", Repo{ID: 10, FullName: "owner/expiry-repo"})

	// Hit within TTL.
	if _, ok := cache.Get("owner/expiry-repo"); !ok {
		t.Fatal("expected hit within TTL")
	}

	// Advance past TTL — mid-burst expiry.
	now = now.Add(31 * time.Second)
	if _, ok := cache.Get("owner/expiry-repo"); ok {
		t.Fatal("expected miss after TTL expiry mid-burst")
	}
	// Entry should have been lazily removed.
	if cache.Len() != 0 {
		t.Fatalf("Len = %d, want 0 after lazy eviction", cache.Len())
	}
}

func TestGetRepoWithCache_RenamedRepoMisses(t *testing.T) {
	// Simulates a repo transferred from "old-owner/repo" to "new-owner/repo".
	// The old name should still hit (if within TTL); the new name should miss.
	now := time.Unix(9_000_000, 0)
	cache := newTestCache(time.Minute, func() time.Time { return now })

	cache.set("old-owner/repo", Repo{ID: 20, FullName: "old-owner/repo"})

	if _, ok := cache.Get("old-owner/repo"); !ok {
		t.Fatal("expected hit for old name")
	}
	if _, ok := cache.Get("new-owner/repo"); ok {
		t.Fatal("expected miss for new (renamed) name — cache key is name-based")
	}
}

func TestRepoCache_LenCountsAllEntries(t *testing.T) {
	now := time.Unix(10_000_000, 0)
	cache := newTestCache(time.Minute, func() time.Time { return now })

	for i := 0; i < 5; i++ {
		cache.set(fmt.Sprintf("owner/repo%d", i), Repo{ID: int64(i), FullName: fmt.Sprintf("owner/repo%d", i)})
	}
	if cache.Len() != 5 {
		t.Fatalf("Len = %d, want 5", cache.Len())
	}
}

func TestRepoCache_NewRepoCacheStartsEmpty(t *testing.T) {
	stopCh := make(chan struct{})
	close(stopCh)
	c := NewRepoCache(time.Minute, stopCh)
	if c.Len() != 0 {
		t.Fatalf("new cache Len = %d, want 0", c.Len())
	}
}
