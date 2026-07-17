# feat: Add liveness (`/health`) and readiness (`/ready`) probes for production orchestration

## Summary

Adds two distinct health-check endpoints so orchestrators (Kubernetes, Nomad, etc.) can accurately determine whether the service is alive vs. ready to serve traffic. Liveness checks only the process itself; readiness verifies that both Postgres and NATS are reachable.

## Changes

### `internal/handlers/health.go` — Liveness probe (`GET /health`)

- Returns **200 OK** unconditionally with build metadata, service name, and uptime.
- Does **not** contact any external dependency — a crash-looping process that is still alive will pass liveness.
- Response shape:

```json
{
  "ok":         true,
  "service":    "grainlify-api",
  "version":    "v1.0.0",
  "commit":     "abc123",
  "build_time": "2025-01-15T10:00:00Z",
  "uptime":     "5m23.456s"
}
```

### `internal/handlers/ready.go` — Readiness probe (`GET /ready`)

- Pings the Postgres connection pool via `db.Ping(ctx)` with a **1-second timeout**.
- Checks NATS connection status via `bus.Status()` — accepts `CONNECTED` or `RECONNECTING`.
- Returns **200 OK** only when every configured dependency is healthy.
- Returns **503 Service Unavailable** with a per-dependency breakdown otherwise.
- Gracefully handles unconfigured dependencies (nil DB or nil Bus) without crashing.
- Response shape (healthy):

```json
{
  "ok": true,
  "deps": [
    { "name": "database", "ready": true,  "status": "ok" },
    { "name": "nats",     "ready": true,  "status": "CONNECTED" }
  ]
}
```

- Response shape (unhealthy):

```json
{
  "ok": false,
  "deps": [
    { "name": "database", "ready": false, "status": "unreachable" },
    { "name": "nats",     "ready": false, "status": "DISCONNECTED" }
  ]
}
```

### Route registration (`internal/api/api.go`)

```
GET /health  → handlers.NewHealth(build)
GET /ready   → handlers.NewReady(deps.DB, deps.Bus)
```

Both routes are registered before authentication middleware — no token required.

## Security

- Neither endpoint leaks connection strings, stack traces, or environment variables.
- A dedicated test (`TestHealthDoesNotExposeSensitiveData` / `TestReadyDoesNotExposeSecrets`) asserts the response body is scanned for substrings like `db_url`, `nats_url`, `password`, `secret`, `token`, `jwt` and fails if any are found.

## Tests

### `internal/handlers/health_test.go` (6 tests)

| Test | Coverage |
|---|---|
| `TestHealthReportsGrainlifyServiceName` | Service name, version, commit, build_time, uptime are present in response |
| `TestHealthUptimeIsRecent` | Uptime string is positive and under 1 minute |
| `TestHealthBuildInfoDefaults` | Empty `BuildInfo` produces empty strings (not `null`) |
| `TestHealthStatusCode` | Always returns 200 |
| `TestHealthResponseContainsOnlyExpectedFields` | Response contains exactly `ok`, `service`, `version`, `commit`, `build_time`, `uptime` — no extra fields |
| `TestHealthDoesNotExposeSensitiveData` | No sensitive substrings in response body |

### `internal/handlers/ready_test.go` (8 tests)

| Test | Scenario | Expected Status |
|---|---|---|
| `TestReadyBothHealthy` | DB pings OK, NATS CONNECTED | 200 |
| `TestReadyDBNotConfigured` | DB is nil, NATS CONNECTED | 503 |
| `TestReadyNATSDisconnected` | DB pool exists but nil pool (no-op ping), NATS DISCONNECTED | 503 |
| `TestReadyNATSNotConfigured` | DB healthy, Bus is nil | 200 |
| `TestReadyBothUnavailable` | DB nil, NATS DISCONNECTED | 503 |
| `TestReadyResponseStructure` | Both nil — validates JSON shape (`ok`, `deps[].name/ready/status`) | 503 |
| `TestReadyDoesNotExposeSecrets` | Scans response body for sensitive substrings | Pass |
| `mockBusReady` compile-time check | `var _ bus.Bus = (*mockBusReady)(nil)` | Compiles |

### Mocking strategy

- **Database**: Uses `&db.DB{}` (nil pool — returns `not_configured`) for failing scenarios, or a real `pgxpool` from `TEST_DB_URL` env var (skipped when unset) for healthy scenarios.
- **NATS**: `mockBusReady` struct with a mutex-protected `status` field implements `bus.Bus` at compile time.

## Usage with Kubernetes

```yaml
livenessProbe:
  httpGet:
    path: /health
    port: 8080
  initialDelaySeconds: 5
  periodSeconds: 15

readinessProbe:
  httpGet:
    path: /ready
    port: 8080
  initialDelaySeconds: 5
  periodSeconds: 10
  failureThreshold: 3
```

## Files changed

- `internal/handlers/health.go` — New liveness handler
- `internal/handlers/health_test.go` — 6 tests for liveness
- `internal/handlers/ready.go` — New readiness handler with dependency pings
- `internal/handlers/ready_test.go` — 8 tests for readiness
- `internal/api/api.go` — Route registration for `/health` and `/ready`

Closes #<issue-number>
