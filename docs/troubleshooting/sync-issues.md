# Sync Issues & Pull Requests — Troubleshooting

This document covers the GitHub rate-limit backoff added in `internal/github/ratelimit.go`
and how to diagnose problems when issue/PR syncing stalls or fails.

---

## How rate-limit backoff works

All HTTP calls made through `Client` (returned by `NewClient`) automatically go through
`RateLimitTransport`. The transport:

1. Executes the request normally.
2. If the response is **403** or **429** and indicates a rate limit (see rules below),
   it sleeps for the prescribed duration and retries the request.
3. Repeats up to `MaxRetries` times (default **3**).
4. Always caps the sleep at `MaxWait` (default **60 s**) so no request can hang
   indefinitely, and it respects `context.Context` deadlines and cancellations.

### Rate-limit detection rules

| Signal | Header | Action |
|--------|--------|--------|
| Primary limit | `X-RateLimit-Remaining: 0` on 403/429 | Sleep until `X-RateLimit-Reset` (Unix timestamp) |
| Secondary limit | `Retry-After: <seconds>` on 403/429 | Sleep for that many seconds |
| Auth failure | 403 without either header | **Not retried** — fail immediately |

---

## Configuration

The defaults work for most workloads. To override them, build the transport manually
and pass it to a custom `http.Client`:

```go
tr := github.NewRateLimitTransport(nil)
tr.MaxRetries = 5               // retry up to 5 times
tr.MaxWait    = 2 * time.Minute // cap sleep at 2 minutes

client := &github.Client{
    HTTP: &http.Client{
        Timeout:   30 * time.Second,
        Transport: tr,
    },
    UserAgent: "my-app",
}
```

---

## Common failure scenarios

### Syncing stops with "API rate limit exceeded"

GitHub allows **5 000 requests/hour** for authenticated calls and **60/hour** unauthenticated.
Bulk syncs can exhaust this quickly.

**What happens:** `RateLimitTransport` sleeps until the reset window and retries. If the
reset is more than `MaxWait` seconds away, the sleep is capped and the retry may still fail.
The caller receives the final 403 after all retries are spent.

**Fix:** Increase `MaxWait` or reduce sync frequency. Check `X-RateLimit-Reset` in the
response headers to see when the limit resets.

### Syncing stops with "secondary rate limit"

GitHub triggers this for rapid bursts (e.g. many write requests in quick succession).
The `Retry-After` header tells you how many seconds to wait.

**What happens:** `RateLimitTransport` sleeps for `min(Retry-After, MaxWait)` seconds and
retries automatically.

**Fix:** If you see repeated secondary limits, add deliberate spacing between API calls in
the sync loop.

### Context deadline exceeded during sync

If the total time spent sleeping + retrying exceeds the context deadline the caller set,
`RateLimitTransport` returns `context.DeadlineExceeded` immediately instead of continuing
to sleep.

**Fix:** Increase the context deadline for long-running sync jobs, or reduce `MaxRetries`
so less total time is spent retrying.

### 403 not being retried (auth failure)

A 403 **without** `X-RateLimit-Remaining: 0` or `Retry-After` is treated as an auth error,
not a rate limit. It is returned immediately without retrying.

**Fix:** Verify the GitHub token is valid and has the required scopes (`repo`, `read:user`).

---

## Running the sync tests

```bash
go test ./internal/github/... -v -run TestRateLimit
# or run all github package tests
go test ./internal/github/... -v
```

Test cases cover:

- No retry on 200 OK
- Primary rate limit (403 + `X-RateLimit-Remaining: 0`) → retry → success
- Secondary rate limit (403 + `Retry-After`) → retry → success
- Exhausted retries — final 403 returned to caller
- `MaxWait` cap — large `Retry-After` value is capped
- Context cancellation mid-sleep
- Plain 500 not retried
- Plain 403 (no rate-limit headers) not retried
- Multiple rate limits before eventual success

---

## Security notes

- `RateLimitTransport` never logs the `Authorization` header or any token value.
- Total wait time is always bounded by `MaxWait` (default 60 s), preventing indefinite hangs.
- Context cancellation is honoured during the sleep phase, so upstream timeouts are respected.
