package handlers

import (
	"sync"
	"testing"
	"time"
)

// TestProjectsCache_Basic verifies basic cache Get/Set behavior.
func TestProjectsCache_Basic(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)
	cache := NewProjectsCache(100*time.Millisecond, stopCh)

	key := "test:key"
	route := "list"
	body := []byte(`{"test":"data"}`)

	// Miss on first lookup
	if got, ok := cache.Get(key, route); ok {
		t.Errorf("expected cache miss, got hit with: %s", got)
	}

	// Set and verify hit
	cache.Set(key, body)
	if got, ok := cache.Get(key, route); !ok || string(got) != string(body) {
		t.Errorf("expected cache hit with %s, got ok=%v, body=%s", body, ok, got)
	}
}

// TestProjectsCache_TTLExpiry verifies that entries expire after TTL.
func TestProjectsCache_TTLExpiry(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)
	ttl := 50 * time.Millisecond
	cache := NewProjectsCache(ttl, stopCh)

	key := "test:expiry"
	route := "list"
	body := []byte(`{"expires":"soon"}`)

	cache.Set(key, body)
	
	// Should hit immediately
	if _, ok := cache.Get(key, route); !ok {
		t.Error("expected cache hit immediately after Set")
	}

	// Wait for TTL to expire
	time.Sleep(ttl + 10*time.Millisecond)

	// Should miss after expiry
	if _, ok := cache.Get(key, route); ok {
		t.Error("expected cache miss after TTL expiry")
	}
}

// TestProjectsCache_InvalidateAll verifies that InvalidateAll clears all entries.
func TestProjectsCache_InvalidateAll(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)
	cache := NewProjectsCache(10*time.Second, stopCh)

	// Set multiple entries
	cache.Set("list:a", []byte("a"))
	cache.Set("list:b", []byte("b"))
	cache.Set("recommended:c", []byte("c"))

	if cache.Len() != 3 {
		t.Errorf("expected 3 entries, got %d", cache.Len())
	}

	// Invalidate all
	cache.InvalidateAll()

	if cache.Len() != 0 {
		t.Errorf("expected 0 entries after InvalidateAll, got %d", cache.Len())
	}

	// Verify all are gone
	if _, ok := cache.Get("list:a", "list"); ok {
		t.Error("expected miss after InvalidateAll")
	}
}

// TestProjectsCache_InvalidateProject verifies selective invalidation.
func TestProjectsCache_InvalidateProject(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)
	cache := NewProjectsCache(10*time.Second, stopCh)

	projectID := "abc-123"
	cache.Set("project:"+projectID, []byte("detail"))
	cache.Set("list:filter1", []byte("list1"))
	cache.Set("recommended:top", []byte("rec"))
	cache.Set("filters:all", []byte("filters"))

	// Invalidate one project
	cache.InvalidateProject(projectID)

	// Project detail and all list variants should be gone
	if _, ok := cache.Get("project:"+projectID, "detail"); ok {
		t.Error("expected project detail to be invalidated")
	}
	if _, ok := cache.Get("list:filter1", "list"); ok {
		t.Error("expected list cache to be invalidated")
	}
	if _, ok := cache.Get("recommended:top", "recommended"); ok {
		t.Error("expected recommended cache to be invalidated")
	}
	if _, ok := cache.Get("filters:all", "filters"); ok {
		t.Error("expected filters cache to be invalidated")
	}
}

// TestProjectsCache_StampedeProtection verifies that concurrent cache misses
// result in only one DB fetch (singleflight behavior).
func TestProjectsCache_StampedeProtection(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)
	cache := NewProjectsCache(1*time.Second, stopCh)

	key := "test:stampede"
	route := "list"
	fetchCount := 0
	var mu sync.Mutex

	fetch := func() ([]byte, error) {
		mu.Lock()
		fetchCount++
		mu.Unlock()
		time.Sleep(50 * time.Millisecond) // Simulate slow DB query
		return []byte(`{"result":"slow"}`), nil
	}

	// Launch 10 concurrent fetches
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = cache.Do(key, route, fetch)
		}()
	}
	wg.Wait()

	// Should have fetched exactly once despite 10 concurrent requests
	mu.Lock()
	count := fetchCount
	mu.Unlock()

	if count != 1 {
		t.Errorf("expected exactly 1 fetch due to stampede protection, got %d", count)
	}

	// Verify the result is cached
	if _, ok := cache.Get(key, route); !ok {
		t.Error("expected result to be cached after Do()")
	}
}

// TestProjectsCache_ZeroTTL verifies that cache is disabled when TTL is zero.
func TestProjectsCache_ZeroTTL(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)
	cache := NewProjectsCache(0, stopCh)

	key := "test:disabled"
	route := "list"
	body := []byte(`{"disabled":"true"}`)

	cache.Set(key, body)

	// Should always miss when TTL is zero
	if _, ok := cache.Get(key, route); ok {
		t.Error("expected cache miss when TTL is zero (cache disabled)")
	}

	if cache.Len() != 0 {
		t.Errorf("expected 0 entries when cache is disabled, got %d", cache.Len())
	}
}

// TestProjectsCache_Concurrent verifies thread-safety under concurrent access.
func TestProjectsCache_Concurrent(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)
	cache := NewProjectsCache(1*time.Second, stopCh)

	var wg sync.WaitGroup
	ops := 100

	// Concurrent writes
	for i := 0; i < ops; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := "key:" + string(rune(n%10))
			cache.Set(key, []byte("value"))
		}(i)
	}

	// Concurrent reads
	for i := 0; i < ops; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := "key:" + string(rune(n%10))
			_, _ = cache.Get(key, "list")
		}(i)
	}

	// Concurrent invalidations
	for i := 0; i < ops/10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if n%2 == 0 {
				cache.InvalidateAll()
			} else {
				cache.InvalidateProject("proj-" + string(rune(n)))
			}
		}(i)
	}

	wg.Wait()

	// Should not panic or deadlock
	t.Log("concurrent operations completed without deadlock")
}

// TestProjectsCache_EvictionLoop verifies that the background eviction goroutine
// cleans up expired entries periodically.
func TestProjectsCache_EvictionLoop(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)
	ttl := 50 * time.Millisecond
	cache := NewProjectsCache(ttl, stopCh)

	// Insert multiple entries
	for i := 0; i < 10; i++ {
		cache.Set("key:"+string(rune(i)), []byte("value"))
	}

	if cache.Len() != 10 {
		t.Errorf("expected 10 entries, got %d", cache.Len())
	}

	// Wait for TTL + eviction interval
	time.Sleep(ttl + 100*time.Millisecond)

	// Eviction loop should have cleaned up expired entries
	if cache.Len() != 0 {
		t.Errorf("expected 0 entries after eviction loop, got %d", cache.Len())
	}
}

// TestProjectsCache_FetchError verifies that fetch errors are not cached.
func TestProjectsCache_FetchError(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)
	cache := NewProjectsCache(1*time.Second, stopCh)

	key := "test:error"
	route := "list"
	fetchCount := 0

	fetch := func() ([]byte, error) {
		fetchCount++
		return nil, &testError{"intentional error"}
	}

	// First call should error
	_, err := cache.Do(key, route, fetch)
	if err == nil {
		t.Error("expected error from fetch")
	}

	// Second call should also fetch (error not cached)
	_, err = cache.Do(key, route, fetch)
	if err == nil {
		t.Error("expected error from second fetch")
	}

	if fetchCount != 2 {
		t.Errorf("expected 2 fetches (errors not cached), got %d", fetchCount)
	}

	// Cache should be empty
	if cache.Len() != 0 {
		t.Errorf("expected 0 entries (errors not cached), got %d", cache.Len())
	}
}

type testError struct {
	msg string
}

func (e *testError) Error() string {
	return e.msg
}
