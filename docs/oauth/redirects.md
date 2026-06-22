# OAuth Redirect Debugging

Guide for diagnosing OAuth login redirects (user lands on wrong URL, localhost instead of production, etc.).

## Expected flow

1. Frontend calls `GET /auth/github/login/start?redirect=<frontend-origin>`
2. Backend stores `redirect_uri` in `oauth_states` and redirects to GitHub
3. GitHub redirects to the static callback: `/auth/github/login/callback`
4. Backend reads `redirect_uri` from state and redirects user to frontend with JWT

## Step 1: Verify migration applied

The `redirect_uri` column must exist in `oauth_states` (migration `000024`):

```sql
SELECT column_name
FROM information_schema.columns
WHERE table_name = 'oauth_states' AND column_name = 'redirect_uri';
```

If missing, run migrations:

```bash
go run ./cmd/migrate
```

Or set `AUTO_MIGRATE=true` and restart the API.

## Step 2: Check backend logs

**On login start:**
```
OAuth login start - received redirect parameter redirect=https://your-frontend.example.com
OAuth login start - stored redirect_uri in state redirect_uri=...
```

**On callback:**
```
OAuth callback - retrieved redirect_uri from state redirect_uri=...
OAuth redirect - redirecting user final_redirect_url=.../auth/callback?token=...
```

| Log message | Likely cause |
|-------------|--------------|
| `no redirect_uri in state` | Frontend did not pass `redirect` parameter |
| `redirect_uri_not_allowed` | Origin not in `CORS_ORIGINS` / `FRONTEND_BASE_URL` |
| `no redirect URL configured` | Missing `FRONTEND_BASE_URL` fallback |

## Step 3: Verify frontend passes redirect

In browser DevTools → Network, the login request should include:

```
/auth/github/login/start?redirect=https%3A%2F%2Fyour-frontend.example.com
```

## Step 4: Verify database storage

```sql
SELECT state, kind, redirect_uri, expires_at, created_at
FROM oauth_states
WHERE kind = 'github_login'
ORDER BY created_at DESC
LIMIT 5;
```

`redirect_uri` should contain the frontend URL, not `NULL`.

## Step 5: GitHub OAuth App settings

| Setting | Value |
|---------|-------|
| Homepage URL | Production frontend URL |
| Authorization callback URL | `https://api.your-domain.com/auth/github/login/callback` |

The backend redirect should override Homepage URL, but callback URL must match GitHub app registration.

## Environment variables

```bash
FRONTEND_BASE_URL=https://your-frontend.example.com
GITHUB_OAUTH_REDIRECT_URL=https://api.your-domain.com/auth/github/login/callback
GITHUB_OAUTH_SUCCESS_REDIRECT_URL=https://your-frontend.example.com
```

See also [Environment Redirect URLs](env-redirect-urls.md) and [Multi-Environment Setup](multi-env-setup.md).

## Quick checklist

- [ ] Migration `000024` applied
- [ ] Frontend passes `redirect` query parameter
- [ ] Backend logs show `redirect_uri` stored and retrieved
- [ ] `FRONTEND_BASE_URL` set as fallback
- [ ] GitHub OAuth callback URL matches backend
- [ ] Production domain allowed in CORS / frontend config

## Related docs

- [OAuth 2.0 Spec Compliance](spec-compliance.md) — state parameter and CSRF design
- [OAuth App Settings](app-settings.md)
- [GitHub App Callbacks](../github-app/callbacks.md) — separate from OAuth login flow
