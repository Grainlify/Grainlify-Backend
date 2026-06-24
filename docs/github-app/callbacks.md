# GitHub App Installation Callbacks

Canonical guide for configuring and debugging the GitHub App installation callback flow.

## How installation callbacks work

GitHub App installations use the **Callback URL** from your GitHub App settings (not a `redirect_uri` in code):

```
1. User clicks "Install GitHub App" in the frontend
2. Frontend calls POST /auth/github/app/install/start
3. Backend returns an installation URL with state
4. User completes installation on GitHub
5. GitHub redirects to: {CALLBACK_URL}?installation_id=...&state=...
6. Backend handles GET /auth/github/app/install/callback
7. Backend redirects to FRONTEND_BASE_URL (e.g. /dashboard?github_app_installed=true)
```

## Configure the callback URL

1. Go to **GitHub → Settings → Developer settings → GitHub Apps → Grainlify**
2. Open **Identifying and authorizing users**
3. Set **Callback URL** to your backend URL:

**Production:**
```
https://api.your-domain.com/auth/github/app/install/callback
```

**Local development (with HTTPS tunnel):**
```
https://your-tunnel.example.com/auth/github/app/install/callback
```

Requirements:
- Must be **HTTPS** (GitHub rejects HTTP callbacks)
- Path must be exactly `/auth/github/app/install/callback`
- No trailing slash
- Must be reachable from the public internet

4. Click **Update GitHub App** so GitHub verifies the URL.

## Post-installation settings

In **Post Installation**:

| Setting | Recommended value |
|---------|-------------------|
| Setup URL (optional) | **Leave empty** (or match the callback URL exactly) |
| Redirect on update | Checked |

If Setup URL points elsewhere, GitHub may redirect there instead of your callback.

## Allow other users to install

If GitHub reports the app can only be installed by the creator:

1. Open GitHub App settings → **Where can this GitHub App be installed?**
2. Change from **Only on this account** to **Any account**
3. Click **Update GitHub App**

A private (non-marketplace) app can still be installable by any account when this setting is enabled.

## Environment variables

```bash
# Where users land after a successful installation
FRONTEND_BASE_URL=https://your-frontend.example.com

# Public backend URL (must match the callback host)
PUBLIC_BASE_URL=https://api.your-domain.com
```

## Troubleshooting

### No callback received after installation

1. Confirm **Callback URL** is set in GitHub App settings and matches `PUBLIC_BASE_URL`
2. Confirm the backend is running and the tunnel (if local) is active
3. Check backend logs for `GitHub App installation started` and `GitHub App installation callback received`
4. Uninstall the app from the org and try again (GitHub skips redirect if already installed)
5. Ensure **Setup URL** is empty

### Test callback reachability

```bash
curl -I "https://api.your-domain.com/auth/github/app/install/callback?installation_id=test&state=test"
```

A `200`, `302`, or JSON error about invalid state confirms the route is reachable.

### `missing_installation_id` error

Usually means:
- User cancelled installation on GitHub
- Callback URL was opened directly (not from GitHub)
- Setup URL misconfiguration

Complete the installation flow again from the frontend without cancelling.

### Tunnel URL changed

If your dev tunnel URL changes:
1. Update GitHub App **Callback URL**
2. Update `PUBLIC_BASE_URL` in `.env`
3. Click **Update GitHub App**

## Verification checklist

- [ ] Callback URL set in GitHub App settings (HTTPS, correct path)
- [ ] Setup URL empty or matches callback URL
- [ ] App installable on **Any account** (if needed for other users)
- [ ] `PUBLIC_BASE_URL` and `FRONTEND_BASE_URL` configured
- [ ] Backend running; tunnel active for local dev
- [ ] `curl` test to callback URL succeeds
- [ ] Installation logs show callback received

## Related docs

- [GitHub App Setup](setup.md)
- [Private Key Setup](private-key.md)
- [Webhook Secrets](webhooks.md)
- [OAuth Redirects](../oauth/redirects.md)
