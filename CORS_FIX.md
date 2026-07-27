# CORS Origin Policy

## Problem

The API enables `AllowCredentials: true`, so the CORS origin allowlist is the primary protection against cross-site credential abuse. Previously, `internal/api/api.go` unconditionally allowed:

- Any origin ending in `.vercel.app`
- Any origin ending in `.0xo.in`
- All `localhost` / `127.0.0.1` ports

Because anyone can deploy to `*.vercel.app`, credentialed requests from attacker-controlled preview URLs were accepted.

## Policy (after fix)

Origin matching is centralized in `BuildCORSOriginPolicy` / `CORSOriginPolicy.Allows` (`internal/api/cors.go`).

| Rule | When allowed |
|------|----------------|
| `CORS_ORIGINS` (comma-separated) | Always |
| `FRONTEND_BASE_URL` (exact origin) | Always |
| `localhost` / `127.0.0.1` | Only when `APP_ENV=dev` |
| `*.vercel.app` and `*.0xo.in` wildcards | Only when `CORS_ALLOW_PREVIEW=true` |

**Production default:** explicit `CORS_ORIGINS` + `FRONTEND_BASE_URL` only. Wildcards and localhost are **off**.

## Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `APP_ENV` | `dev` | Set to `production` in deployed environments |
| `CORS_ORIGINS` | _(empty)_ | Comma-separated list of allowed origins |
| `FRONTEND_BASE_URL` | _(empty)_ | Primary frontend origin (also allowed for CORS) |
| `CORS_ALLOW_PREVIEW` | `false` | Set to `true` to allow `*.vercel.app` and `*.0xo.in` |

## Examples

### Local development

```env
APP_ENV=dev
FRONTEND_BASE_URL=http://localhost:5173
CORS_ORIGINS=http://localhost:5173
```

Localhost origins on any port are allowed automatically in `dev`.

### Production

```env
APP_ENV=production
FRONTEND_BASE_URL=https://grainlify.0xo.in
CORS_ORIGINS=https://grainlify.0xo.in,https://api.grainlify.0xo.in
CORS_ALLOW_PREVIEW=false
```

### Preview deployments (optional)

```env
APP_ENV=production
CORS_ALLOW_PREVIEW=true
CORS_ORIGINS=https://grainlify.0xo.in
```

Only enable `CORS_ALLOW_PREVIEW` when preview frontends genuinely need credentialed API access.

## Security notes

- With `AllowCredentials: true`, default to **deny**; only explicitly configured origins are trusted.
- Wildcard suffix checks require a leading dot (e.g. `.vercel.app`) to reduce subdomain-spoof risk.
- Origins are normalized by trimming whitespace and trailing slashes before matching.
