package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// TestProjectsPublicHandler_CacheInvalidation_OnEcosystemChange verifies that
// cache is invalidated when an ecosystem is created/updated/deleted.
func TestProjectsPublicHandler_CacheInvalidation_OnEcosystemChange(t *testing.T) {
	// Create mock handlers
	stopCh := make(chan struct{})
	defer close(stopCh)
	
	cache := NewProjectsCache(10*time.Second, stopCh)
	projectsPublic := newProjectsPublicHandler(config.Config{}, &db.DB{}, cache)
	
	ecosystemsAdmin := &EcosystemsAdminHandler{}
	ecosystemsAdmin.SetCacheInvalidator(projectsPublic.InvalidateAll)

	// Populate cache
	cache.Set("list:?ecosystem=starknet", []byte(`{"projects":[]}`))
	cache.Set("recommended:?limit=8", []byte(`{"projects":[]}`))
	cache.Set("filters:", []byte(`{"languages":[]}`))

	if cache.Len() != 3 {
		t.Fatalf("expected 3 cached entries, got %d", cache.Len())
	}

	// Simulate ecosystem change (call the invalidator)
	ecosystemsAdmin.onEcosystemChanged()

	// Cache should be empty
	if cache.Len() != 0 {
		t.Errorf("expected cache to be cleared after ecosystem change, got %d entries", cache.Len())
	}
}

// TestProjectsPublicHandler_CacheInvalidation_OnProjectUpdate verifies that
// cache is invalidated when a project is updated.
func TestProjectsPublicHandler_CacheInvalidation_OnProjectUpdate(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)
	
	cache := NewProjectsCache(10*time.Second, stopCh)
	projectsPublic := newProjectsPublicHandler(config.Config{}, &db.DB{}, cache)
	
	projects := &ProjectsHandler{}
	projects.SetCacheInvalidator(projectsPublic.InvalidateProject)

	projectID := uuid.New().String()

	// Populate cache
	cache.Set("project:"+projectID, []byte(`{"id":"` + projectID + `"}`))
	cache.Set("list:?ecosystem=starknet", []byte(`{"projects":[]}`))
	cache.Set("recommended:?limit=8", []byte(`{"projects":[]}`))

	if cache.Len() != 3 {
		t.Fatalf("expected 3 cached entries, got %d", cache.Len())
	}

	// Simulate project update
	projects.onProjectChanged(projectID)

	// All caches should be invalidated (project detail + list variants)
	if cache.Len() != 0 {
		t.Errorf("expected cache to be cleared after project update, got %d entries", cache.Len())
	}
}

// TestProjectsPublicHandler_CacheInvalidation_OnBatchSync verifies that
// cache is fully invalidated when GitHub App syncs multiple repos.
func TestProjectsPublicHandler_CacheInvalidation_OnBatchSync(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)
	
	cache := NewProjectsCache(10*time.Second, stopCh)
	projectsPublic := newProjectsPublicHandler(config.Config{}, &db.DB{}, cache)
	
	ghApp := &GitHubAppHandler{}
	ghApp.SetCacheInvalidator(projectsPublic.InvalidateProject)

	// Populate cache
	cache.Set("list:?ecosystem=ethereum", []byte(`{"projects":[]}`))
	cache.Set("recommended:", []byte(`{"projects":[]}`))

	if cache.Len() != 2 {
		t.Fatalf("expected 2 cached entries, got %d", cache.Len())
	}

	// Simulate batch sync (empty string signals invalidate all)
	ghApp.onProjectChanged("")

	// Cache should be empty
	if cache.Len() != 0 {
		t.Errorf("expected cache to be cleared after batch sync, got %d entries", cache.Len())
	}
}

// TestProjectsPublicHandler_ListCacheHit verifies that repeated List requests
// hit the cache instead of executing the fetch function.
func TestProjectsPublicHandler_ListCacheHit(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)
	
	cache := NewProjectsCache(1*time.Second, stopCh)
	projectsPublic := newProjectsPublicHandler(config.Config{}, &db.DB{}, cache)

	app := fiber.New()
	app.Get("/projects", projectsPublic.List())

	// First request (cache miss, will error due to nil DB but that's ok for this test)
	req1 := httptest.NewRequest("GET", "/projects?ecosystem=starknet", nil)
	resp1, _ := app.Test(req1, -1)
	defer resp1.Body.Close()
	body1, _ := io.ReadAll(resp1.Body)

	// Pre-populate cache with a valid response to simulate a successful fetch
	cacheKey := "list:/projects?ecosystem=starknet"
	cachedResponse := []byte(`{"projects":[],"pagination":{"page":1,"limit":50,"total":0},"data_key":"projects"}`)
	cache.Set(cacheKey, cachedResponse)

	// Second request (cache hit)
	req2 := httptest.NewRequest("GET", "/projects?ecosystem=starknet", nil)
	resp2, _ := app.Test(req2, -1)
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)

	// Should get the cached response
	if !bytes.Equal(body2, cachedResponse) {
		t.Errorf("expected cached response, got: %s", body2)
	}

	// Verify it was a cache hit (body should match exactly)
	if bytes.Contains(body1, []byte("cache")) {
		t.Log("First request went through fetch path (expected)")
	}
	if string(body2) == string(cachedResponse) {
		t.Log("Second request hit cache (expected)")
	}
}

// TestProjectsPublicHandler_RecommendedCacheHit verifies that Recommended
// endpoint uses caching.
func TestProjectsPublicHandler_RecommendedCacheHit(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)
	
	cache := NewProjectsCache(1*time.Second, stopCh)
	projectsPublic := newProjectsPublicHandler(config.Config{}, &db.DB{}, cache)

	app := fiber.New()
	app.Get("/projects/recommended", projectsPublic.Recommended())

	// Pre-populate cache
	cacheKey := "recommended:/projects/recommended"
	cachedResponse := []byte(`{"projects":[{"id":"test"}],"pagination":{"page":1,"limit":8,"total":1},"data_key":"projects"}`)
	cache.Set(cacheKey, cachedResponse)

	// Request should hit cache
	req := httptest.NewRequest("GET", "/projects/recommended", nil)
	resp, _ := app.Test(req, -1)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// Should get the cached response
	if !bytes.Equal(body, cachedResponse) {
		t.Errorf("expected cached response, got: %s", body)
	}
}

// TestProjectsPublicHandler_FiltersCacheHit verifies that FilterOptions
// endpoint uses caching.
func TestProjectsPublicHandler_FiltersCacheHit(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)
	
	cache := NewProjectsCache(1*time.Second, stopCh)
	projectsPublic := newProjectsPublicHandler(config.Config{}, &db.DB{}, cache)

	app := fiber.New()
	app.Get("/projects/filters", projectsPublic.FilterOptions())

	// Pre-populate cache
	cacheKey := "filters:/projects/filters"
	cachedResponse := []byte(`{"languages":["Go","Rust"],"categories":["DeFi"],"tags":["blockchain"]}`)
	cache.Set(cacheKey, cachedResponse)

	// Request should hit cache
	req := httptest.NewRequest("GET", "/projects/filters", nil)
	resp, _ := app.Test(req, -1)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// Should get the cached response
	if !bytes.Equal(body, cachedResponse) {
		t.Errorf("expected cached response, got: %s", body)
	}
}

// TestProjectsPublicHandler_CacheDisabled verifies that handler works correctly
// when cache is nil (disabled).
func TestProjectsPublicHandler_CacheDisabled(t *testing.T) {
	projectsPublic := newProjectsPublicHandler(config.Config{}, &db.DB{}, nil)

	app := fiber.New()
	app.Get("/projects", projectsPublic.List())

	// Request with nil cache should not panic
	req := httptest.NewRequest("GET", "/projects?limit=10", nil)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("unexpected error with nil cache: %v", err)
	}
	defer resp.Body.Close()

	// Should return error due to nil DB, but not panic
	if resp.StatusCode != 503 {
		t.Logf("got status %d (expected 503 for nil DB)", resp.StatusCode)
	}
}

// TestProjectsPublicHandler_TTLBoundary verifies cache behavior near TTL expiry.
func TestProjectsPublicHandler_TTLBoundary(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)
	
	ttl := 100 * time.Millisecond
	cache := NewProjectsCache(ttl, stopCh)
	projectsPublic := newProjectsPublicHandler(config.Config{}, &db.DB{}, cache)

	app := fiber.New()
	app.Get("/projects", projectsPublic.List())

	// Pre-populate cache
	cacheKey := "list:/projects"
	cachedResponse := []byte(`{"projects":[]}`)
	cache.Set(cacheKey, cachedResponse)

	// Immediate request should hit
	req1 := httptest.NewRequest("GET", "/projects", nil)
	resp1, _ := app.Test(req1, -1)
	defer resp1.Body.Close()
	body1, _ := io.ReadAll(resp1.Body)

	if !bytes.Equal(body1, cachedResponse) {
		t.Error("expected cache hit immediately after Set")
	}

	// Wait for TTL to expire
	time.Sleep(ttl + 20*time.Millisecond)

	// Request after TTL should miss (will error with nil DB, but that's expected)
	req2 := httptest.NewRequest("GET", "/projects", nil)
	resp2, _ := app.Test(req2, -1)
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)

	// Should get error response (not cached response) after TTL expiry
	if bytes.Equal(body2, cachedResponse) {
		t.Error("expected cache miss after TTL expiry, but got cached response")
	}
}

// TestPaginationResponse_Structure verifies the pagination response structure
// is correctly serialized for caching.
func TestPaginationResponse_Structure(t *testing.T) {
	p := PaginationParams{Limit: 50, Offset: 0}
	total := 100
	data := []fiber.Map{{"id": "1"}, {"id": "2"}}

	result := PaginatedResponse("projects", data, p, total)

	jsonBytes, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("failed to marshal pagination response: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &parsed); err != nil {
		t.Fatalf("failed to unmarshal pagination response: %v", err)
	}

	// PaginatedResponse returns a flat envelope, not a nested "pagination"
	// object with a separate "data_key" discriminator -- see its doc
	// comment: {"<itemsKey>": [...], "limit": N, "offset": N, "total": N,
	// "has_more": bool}. This flat shape is what every real caller
	// (List/Recommended/etc.) and every other passing pagination test in
	// this package (e.g. TestProjectsPagination_Integration) actually
	// decodes, so this test asserts that real shape rather than a
	// different, unused envelope.
	if _, ok := parsed["projects"]; !ok {
		t.Errorf("expected a top-level %q items key, got keys: %v", "projects", parsed)
	}

	if parsed["total"] != float64(100) {
		t.Errorf("expected total=100, got %v", parsed["total"])
	}

	if parsed["limit"] != float64(50) {
		t.Errorf("expected limit=50, got %v", parsed["limit"])
	}

	if parsed["offset"] != float64(0) {
		t.Errorf("expected offset=0, got %v", parsed["offset"])
	}

	if parsed["has_more"] != true {
		t.Errorf("expected has_more=true (offset 0 + limit 50 < total 100), got %v", parsed["has_more"])
	}
}

// ---------------------------------------------------------------------------
// Get() caching — issue #298: Get() previously bypassed h.cache entirely,
// unlike List/Recommended/FilterOptions.
// ---------------------------------------------------------------------------

// panicIfCalledDBPool implements db.DBPool with every method panicking, so a
// test can affirmatively prove a code path never reaches the database
// (stronger than just checking the response body, since a bug that
// accidentally re-fetched but happened to return the same bytes wouldn't be
// caught by a body comparison alone).
type panicIfCalledDBPool struct{}

func (panicIfCalledDBPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	panic("Exec should not be called on a cache hit")
}
func (panicIfCalledDBPool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	panic("Query should not be called on a cache hit")
}
func (panicIfCalledDBPool) QueryRow(context.Context, string, ...any) pgx.Row {
	panic("QueryRow should not be called on a cache hit")
}
func (panicIfCalledDBPool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	panic("BeginTx should not be called on a cache hit")
}
func (panicIfCalledDBPool) Ping(context.Context) error { return nil }
func (panicIfCalledDBPool) Close()                     {}
func (panicIfCalledDBPool) Config() *pgxpool.Config    { return nil }

// TestProjectsPublicHandler_GetCacheHit verifies that a cached project detail
// response is served without ever touching the database, using a DBPool that
// panics if called at all — a cache-hit bug that still happened to return the
// right bytes (e.g. by re-fetching) would be caught here, unlike a body-only
// comparison.
func TestProjectsPublicHandler_GetCacheHit(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)

	cache := NewProjectsCache(10*time.Second, stopCh)
	projectsPublic := newProjectsPublicHandler(config.Config{}, &db.DB{Pool: panicIfCalledDBPool{}}, cache)

	app := fiber.New()
	app.Get("/projects/:id", projectsPublic.Get())

	projectID := uuid.New().String()
	cacheKey := "project:" + projectID
	cachedResponse := []byte(`{"id":"` + projectID + `","github_full_name":"acme/widget"}`)
	cache.Set(cacheKey, cachedResponse)

	req := httptest.NewRequest("GET", "/projects/"+projectID, nil)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != fiber.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if !bytes.Equal(body, cachedResponse) {
		t.Errorf("expected cached response, got: %s", body)
	}
}

// notFoundRow is a pgx.Row whose Scan always reports pgx.ErrNoRows, so a
// getFetch call resolves to a 404 without needing a real database or any
// GitHub network call — it never reaches the GitHub-enrichment code past the
// project lookup.
type notFoundRow struct{}

func (notFoundRow) Scan(dest ...any) error { return pgx.ErrNoRows }

// countingNotFoundDBPool implements db.DBPool, counting QueryRow calls and
// always reporting "no such project." A small sleep simulates DB latency so
// concurrent callers actually overlap in time, giving the singleflight
// in-flight map something real to deduplicate.
type countingNotFoundDBPool struct {
	queryRowCalls atomic.Int32
}

func (p *countingNotFoundDBPool) QueryRow(context.Context, string, ...any) pgx.Row {
	p.queryRowCalls.Add(1)
	time.Sleep(20 * time.Millisecond)
	return notFoundRow{}
}
func (p *countingNotFoundDBPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	panic("Exec should not be called for a not-found project lookup")
}
func (p *countingNotFoundDBPool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	panic("Query should not be called for a not-found project lookup")
}
func (p *countingNotFoundDBPool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	panic("BeginTx should not be called for a not-found project lookup")
}
func (p *countingNotFoundDBPool) Ping(context.Context) error { return nil }
func (p *countingNotFoundDBPool) Close()                     {}
func (p *countingNotFoundDBPool) Config() *pgxpool.Config    { return nil }

// TestProjectsPublicHandler_GetStampedeProtection verifies concurrent
// requests for the same uncached project ID share a single underlying fetch,
// matching the guarantee ProjectsCache.Do already provides List/Recommended/
// FilterOptions. All N concurrent Get() calls resolve to the DB's
// "not found" result via the exact same singleflight path GitHub enrichment
// would also share, so counting DB QueryRow calls proves the whole getFetch
// closure — DB lookup and GitHub calls alike — ran exactly once.
func TestProjectsPublicHandler_GetStampedeProtection(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)

	cache := NewProjectsCache(10*time.Second, stopCh)
	pool := &countingNotFoundDBPool{}
	projectsPublic := newProjectsPublicHandler(config.Config{}, &db.DB{Pool: pool}, cache)

	app := fiber.New()
	app.Get("/projects/:id", projectsPublic.Get())

	projectID := uuid.New().String()

	const n = 10
	var wg sync.WaitGroup
	statuses := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/projects/"+projectID, nil)
			resp, err := app.Test(req, -1)
			if err != nil {
				t.Errorf("app.Test: %v", err)
				return
			}
			defer resp.Body.Close()
			statuses[i] = resp.StatusCode
		}(i)
	}
	wg.Wait()

	if got := pool.queryRowCalls.Load(); got != 1 {
		t.Errorf("expected exactly 1 DB QueryRow call across %d concurrent requests, got %d", n, got)
	}
	for i, s := range statuses {
		if s != fiber.StatusNotFound {
			t.Errorf("request %d: status = %d, want 404", i, s)
		}
	}
}

// TestProjectsPublicHandler_GetInvalidateProjectEvictsDetailEntry verifies
// InvalidateProject correctly evicts the "project:<id>" detail cache entry
// specifically (not just as a side effect of clearing everything), per issue
// #298's acceptance criteria.
func TestProjectsPublicHandler_GetInvalidateProjectEvictsDetailEntry(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)

	cache := NewProjectsCache(10*time.Second, stopCh)
	projectsPublic := newProjectsPublicHandler(config.Config{}, &db.DB{}, cache)

	projectID := uuid.New().String()
	cacheKey := "project:" + projectID
	cache.Set(cacheKey, []byte(`{"id":"`+projectID+`"}`))

	if _, ok := cache.Get(cacheKey, "get"); !ok {
		t.Fatal("setup: expected detail entry to be cached before invalidation")
	}

	projectsPublic.InvalidateProject(projectID)

	if _, ok := cache.Get(cacheKey, "get"); ok {
		t.Error("expected InvalidateProject to evict the detail cache entry, but it's still cached")
	}
}
