# API Endpoints

## HTTP rate limiting

The API now applies configurable rate limits at the HTTP layer for abuse-prone routes.

### Limit groups

- Auth and webhook routes (`/auth/*`, `/webhooks/*`) use the stricter auth/webhook limit.
- Public read-only routes (`/projects*`, `/leaderboard`, `/stats/landing`, `/ecosystems`, `/profile/public`, `/open-source-week/events`) use the public limit.
- Requests are bucketed per client IP by default. If a request contains a valid bearer token, the limiter falls back to the authenticated user ID for that request instead of the IP.

### Default limits

- Auth/webhook: 60 requests/minute per bucket
- Public read routes: 300 requests/minute per bucket

### Environment variables

- `RATE_LIMIT_AUTH_PER_MIN`: per-minute limit for auth/webhook routes.
- `RATE_LIMIT_PUBLIC_PER_MIN`: per-minute limit for public read routes.
- `TRUSTED_PROXIES`: comma-separated list of trusted proxy IPs or CIDRs allowed to supply `X-Forwarded-For` values.

### Response behavior

When a limit is exceeded, the API returns `429 Too Many Requests` with the standard JSON error envelope:

```json
{
  "error": "too_many_requests",
  "message": "rate limit exceeded",
  "request_id": "..."
}
```

### Security notes

- Forwarded client IPs are only trusted when the immediate remote peer matches a configured trusted proxy.
- Untrusted `X-Forwarded-For` values are ignored to prevent spoofing.
- Local loopback/test requests may still use forwarded headers so the behavior is easy to exercise in development.
