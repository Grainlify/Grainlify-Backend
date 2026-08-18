# GitHub OAuth App Settings - Production Configuration

## ✅ Domain migration complete (grainlify.0xo.in → grainlify.com)

The values below are live. `grainlify.com` serves the app from Vercel and
`api.grainlify.com` serves the backend through Cloudflare; both hold valid
certificates and the apex sends
`Strict-Transport-Security: max-age=63072000; includeSubDomains`.

**This section previously said the opposite** - that the OAuth callback should
stay on `.0xo.in` until `api.grainlify.com` was confirmed serving. That was
correct when written and became wrong without anyone editing it, which is the
whole hazard of instructions that describe a transition.

The current state, established from the running system rather than from this
file:

- Railway holds `GITHUB_OAUTH_REDIRECT_URL` and `GITHUB_LOGIN_REDIRECT_URL` as
  `https://api.grainlify.com/auth/github/login/callback`, and has **no**
  `.0xo.in` value in any of its 44 variables.
- Sign-ups are completing: new accounts were created today and on each of the
  preceding days. A new account requires a full authorization round trip, which
  GitHub refuses unless `redirect_uri` matches a registered callback URL. So the
  App's registered set already accepts `api.grainlify.com`.
- Because the backend only ever sends the `api.grainlify.com` value, whether the
  old URL also remains registered no longer affects anything.

## ⚠️ Critical Settings

When configuring your GitHub OAuth App for production, these settings are critical:

### 1. Homepage URL

**✅ CORRECT (Production):**
```
https://grainlify.com
```

**❌ WRONG:**
```
http://localhost:5173
https://localhost:5173
```

**Why this matters:**
- GitHub uses Homepage URL as the default landing page after OAuth
- If set to localhost, users will be redirected to localhost even in production
- Should always point to your production frontend domain

### 2. Authorization callback URL

**✅ CORRECT (Backend - Single URL for all environments):**
```
https://api.grainlify.com/auth/github/login/callback
```

**Why this works:**
- This is the callback URL the backend sends as `redirect_uri`, and the only
  one it ever sends. Whether other URLs remain registered in the App from
  earlier domains is not something this file can assert - the App settings
  page is the only place that answers it - and it does not matter while the
  backend sends just this one.
- Works for production, preview deployments, and localhost
- Backend handles redirecting to the correct frontend via the `redirect` parameter

## Complete OAuth App Settings

| Setting | Value | Notes |
|---------|-------|-------|
| **Application name** | Grainlify | Your app name |
| **Homepage URL** | `https://grainlify.com` | Production frontend |
| **Authorization callback URL** | `https://api.grainlify.com/auth/github/login/callback` | Backend callback (never changes) |
| **Application description** | (Optional) | Describe your app |

## How It Works

1. **User clicks "Sign in with GitHub"** on any frontend (production/preview/localhost)
2. **Frontend sends:** `GET /auth/github/login/start?redirect={frontend_origin}`
3. **Backend stores** the `redirect` parameter in database
4. **Backend redirects** to GitHub OAuth
5. **GitHub redirects** to the callback URL (always the same backend URL)
6. **Backend processes** OAuth, retrieves stored `redirect` parameter
7. **Backend redirects** user to: `{redirect}/auth/callback?token={jwt}`

## For Local Development

**Option 1: Separate OAuth App (Recommended)**
- Create a second OAuth App for local development
- Set Homepage URL to `http://localhost:5173`
- Use different `GITHUB_OAUTH_CLIENT_ID` for local dev

**Option 2: Temporarily Change Homepage URL**
- When developing locally, temporarily change Homepage URL to `http://localhost:5173`
- Remember to change it back before deploying

**Note:** The callback URL can stay the same (production backend) even for local dev, as long as you're using a tunnel or the backend is accessible.

## Security

The backend validates redirect URIs to prevent open redirect attacks. Only these origins are allowed:

- ✅ `localhost` origins (for development)
- ✅ `*.vercel.app` domains (for preview deployments)
- ✅ Origins in `CORS_ORIGINS` env var
- ✅ `FRONTEND_BASE_URL` (if configured)

## Quick Checklist

- [ ] Homepage URL = Production frontend (not localhost)
- [ ] Authorization callback URL = Production backend + `/auth/github/login/callback`
- [ ] Backend `GITHUB_OAUTH_REDIRECT_URL` matches callback URL exactly
- [ ] Test OAuth flow on production
- [ ] Test OAuth flow on preview deployment
- [ ] Verify users are redirected to correct frontend after login

