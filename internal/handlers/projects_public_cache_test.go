package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

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
	p := Pagination{Page: 1, Limit: 50, Offset: 0}
	total := 100
	data := []fiber.Map{{"id": "1"}, {"id": "2"}}

	result := buildPaginatedResponse("projects", data, p, total)

	jsonBytes, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("failed to marshal pagination response: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &parsed); err != nil {
		t.Fatalf("failed to unmarshal pagination response: %v", err)
	}

	// Verify structure
	if parsed["data_key"] != "projects" {
		t.Errorf("expected data_key='projects', got %v", parsed["data_key"])
	}

	pagination, ok := parsed["pagination"].(map[string]interface{})
	if !ok {
		t.Fatal("pagination field missing or wrong type")
	}

	if pagination["total"] != float64(100) {
		t.Errorf("expected total=100, got %v", pagination["total"])
	}

	if pagination["limit"] != float64(50) {
		t.Errorf("expected limit=50, got %v", pagination["limit"])
	}
}
