# Projects Public Cache

## Overview

The public projects endpoints (`/projects`, `/projects/recommended`, `/projects/filters`) use an in-memory response cache with a 30-second TTL to reduce database load and improve response times for anonymous browse/discover traffic.

## Cached Endpoints

| Endpoint | Route Label | Cache Key Pattern |
|----------|-------------|-------------------|
| `GET /projects` | `list` | `list:` + full query string |
| `GET /projects/recommended` | `recommended` | `recommended:` + full query string |
| `GET /projects/filters` | `filters` | `filters:` + full query string |

**Note:** The project detail endpoint (`GET /projects/:id`) is **not cached** because it makes live GitHub API calls to fetch fresh README content and repo metadata.

## Cache Design

### TTL and Staleness

- **Default TTL:** 30 seconds
- **Staleness bound:** Worst-case 30s for TTL expiry + invalidation latency
- **Invalidation:** Admin/write operations trigger immediate cache clearing

This short TTL balances:
- **Traffic absorption:** Browse page spikes hit cache instead of DB
- **Freshness:** Admin edits reflected within ~30s (or immediately via invalidation hooks)

### Key Namespaces

Cache keys use prefixes to enable selective invalidation:

- `list:*` — All filtered list responses
- `recommended:*` — Top projects by contributors
- `filters:*` — Available filter options (languages, categories, tags)
- `project:<uuid>` — Individual project detail (currently unused, reserved)

### Stampede Protection

The cache uses a **singleflight pattern** (in-flight deduplication) to ensure that concurrent cache misses for the same key result in exactly one DB fetch. Subsequent waiters block on the first fetcher's result channel instead of hammering the database simultaneously.

## Invalidation Hooks

Cache entries are invalidated when underlying data changes:

### Ecosystem Changes (Invalidate All)

Ecosystem create/update/delete operations invalidate **all cached entries** because ecosystem name/slug appear in every project list response.

**Handlers:**
- `admin_ecosystems.go`: `Create()`, `Update()`, `Delete()`

**Hook:** `projectsPublic.InvalidateAll()`

### Project Updates (Invalidate Project + Lists)

Project metadata updates invalidate:
1. The specific project's detail entry (if cached)
2. All list/recommended/filter variants (since the project may appear in lists)

**Handlers:**
- `projects.go`: `UpdateMetadata()`, `Verify()` (after successful verification)
- `github_app.go`: `syncInstallationRepositories()` (batch sync)

**Hook:** `projectsPublic.InvalidateProject(projectID)`

**Special case:** When `projectID` is an empty string, it signals a batch operation (e.g., GitHub App installation sync) and triggers `InvalidateAll()`.

## Metrics

The cache records Prometheus metrics for monitoring:

```
grainlify_projects_public_cache_total{route="list", result="hit"}
grainlify_projects_public_cache_total{route="list", result="miss"}
grainlify_projects_public_cache_total{route="recommended", result="hit"}
grainlify_projects_public_cache_total{route="recommended", result="miss"}
grainlify_projects_public_cache_total{route="filters", result="hit"}
grainlify_projects_public_cache_total{route="filters", result="miss"}
```

**Scrape endpoint:** `/metrics` (requires `METRICS_TOKEN` bearer auth)

### Monitoring Queries

**Cache hit ratio (all routes):**
```promql
sum(rate(grainlify_projects_public_cache_total{result="hit"}[5m]))
/
sum(rate(grainlify_projects_public_cache_total[5m]))
```

**Cache hit ratio by route:**
```promql
sum by (route) (rate(grainlify_projects_public_cache_total{result="hit"}[5m]))
/
sum by (route) (rate(grainlify_projects_public_cache_total[5m]))
```

**Cache miss rate (potential DB load indicator):**
```promql
rate(grainlify_projects_public_cache_total{result="miss"}[5m])
```

## Security

### No Per-User Data

All cached endpoints are **public and unauthenticated**. Cache keys are derived exclusively from public query parameters (ecosystem, language, tags, limit, offset). No auth headers, user IDs, or personalized data are included in cache keys, so serving the same cached response to multiple users is safe.

### DoS via Invalidation

**Threat:** Malicious actor triggers frequent invalidations to bypass cache and overload the database.

**Mitigations:**
1. **Rate limiting:** API-level rate limiting already applied to all requests (see `internal/api/ratelimit.go`)
2. **Restricted access:** Invalidation is triggered only by admin/write operations (authenticated, authorized endpoints)
3. **Stampede protection:** Even if cache is emptied, concurrent requests for the same key share a single DB fetch
4. **Short TTL:** Natural expiry occurs every 30s, so sustained attack would need continuous admin access

### Cache Poisoning

**Threat:** Attacker caches malicious content.

**Mitigations:**
1. **Server-side only:** Cache is populated server-side from DB queries; no user-supplied content is cached directly
2. **Invalidation on writes:** Any data modification (admin or owner) invalidates affected entries
3. **TTL expiry:** All entries auto-expire after 30s

## Configuration

### Disabling the Cache

To disable caching (e.g., for debugging), set the TTL to zero:

```go
// In NewProjectsPublicHandler or via environment variable (if implemented)
cache := NewProjectsCache(0 * time.Second, stopCh)
```

When TTL is zero, all `Get()` calls return misses and `Set()` becomes a no-op.

### Adjusting TTL

The default 30s TTL is defined in `internal/handlers/projects_public.go`:

```go
const defaultProjectsCacheTTL = 30 * time.Second
```

To change it, modify this constant and redeploy. Consider the tradeoffs:

- **Shorter TTL (e.g., 10s):** Fresher data, more DB queries
- **Longer TTL (e.g., 60s):** Higher hit ratio, longer staleness window

## Testing

### Unit Tests

`internal/handlers/projects_cache_test.go` covers:
- Basic Get/Set behavior
- TTL expiry
- Invalidation (all, by project, by prefix)
- Stampede protection (singleflight)
- Concurrent access (thread-safety)
- Eviction loop
- Error handling (errors not cached)

### Integration Tests

`internal/handlers/projects_public_cache_test.go` covers:
- Cache invalidation hooks (ecosystem, project, batch)
- Cache hits on repeated requests
- TTL boundary behavior
- Cache-disabled (nil cache) operation
- Pagination response structure

### Running Tests

```bash
# Unit tests only (no DB required)
go test -run TestProjectsCache ./internal/handlers/

# Integration tests (requires TEST_DB_URL)
TEST_DB_URL="postgresql://user:pass@localhost/test_db" go test ./internal/handlers/

# All tests with race detection
go test -race ./internal/handlers/
```

## Architecture

### Cache Lifecycle

1. **Initialization:** `NewProjectsPublicHandler()` creates a `ProjectsCache` with 30s TTL and a background eviction goroutine
2. **Request path:**
   - Handler checks cache via `cache.Do(key, route, fetch)`
   - On **hit:** Cached JSON bytes returned, metric recorded
   - On **miss:** `fetch()` executes DB query, result cached + returned, metric recorded
3. **Write path:** Admin/project handlers call invalidation hooks after successful DB writes
4. **Eviction:** Background goroutine sweeps expired entries every 15s (TTL/2, min 5s)
5. **Shutdown:** `stopCh` close signals eviction loop to exit gracefully

### Cache Structure

```go
type ProjectsCache struct {
    entries  map[string]projectsCacheEntry  // key → {body, expiresAt}
    ttl      time.Duration
    inflight map[string]*inflightCall       // stampede protection
    mu       sync.RWMutex                   // protects entries
    inflightMu sync.Mutex                   // protects inflight
}
```

### Concurrency Model

- **Read path:** RLock for cache lookup, short critical section
- **Write path:** Lock for cache insertion/deletion
- **Stampede protection:** Mutex-protected in-flight map + result channels
- **Background eviction:** Periodic Lock to sweep expired entries

## Future Enhancements

### Potential Improvements

1. **Redis backend:** Replace in-memory cache with Redis for multi-instance deployments
2. **Cache warming:** Pre-populate popular filters on startup or after invalidation
3. **Adaptive TTL:** Increase TTL for stable data (e.g., 5min for filter options)
4. **Compression:** Gzip cached JSON bodies to reduce memory footprint
5. **LRU eviction:** Add size limit + LRU policy to prevent unbounded growth
6. **Per-route TTL:** Different TTLs for list (30s) vs. filters (5m)

### Monitoring Recommendations

- **Alert on low hit ratio:** < 50% hit ratio over 5min may indicate cache thrashing
- **Alert on high miss rate:** Sudden spike in misses may indicate invalidation storm or TTL mismatch
- **Dashboard:** Graph hit/miss rates by route, cache size, eviction count

## Troubleshooting

### Symptom: Stale data persists beyond 30s

**Possible causes:**
1. Invalidation hook not called after write operation
2. Cache key mismatch (query params in different order)
3. Clock skew on cache expiry

**Diagnosis:**
- Check logs for `onEcosystemChanged` / `onProjectChanged` calls
- Inspect cache keys: `cacheKey := "list:" + c.OriginalURL()`
- Verify TTL: `defaultProjectsCacheTTL`

### Symptom: High DB load despite cache

**Possible causes:**
1. Low hit ratio (check metrics)
2. Cache stampede (check inflight map size)
3. TTL too short for traffic pattern

**Diagnosis:**
- Query Prometheus: `grainlify_projects_public_cache_total{result="hit"}`
- Increase TTL if freshness requirements allow
- Verify stampede protection is working (only 1 fetch per key per TTL window)

### Symptom: Memory growth

**Possible causes:**
1. Eviction loop not running (stopCh never closed?)
2. Unbounded cache keys (query param explosion)
3. Large response bodies

**Diagnosis:**
- Monitor cache size: `cache.Len()` (not currently exposed as metric, consider adding)
- Check eviction interval: `ttl / 2` (min 5s)
- Inspect cache keys: should have bounded cardinality (not UUIDs, except project:<id>)

## References

- Cache implementation: `internal/handlers/projects_cache.go`
- Handler integration: `internal/handlers/projects_public.go`
- Invalidation hooks: `internal/handlers/admin_ecosystems.go`, `internal/handlers/projects.go`, `internal/handlers/github_app.go`
- Metrics: `internal/metrics/metrics.go`
- API wiring: `internal/api/api.go`
