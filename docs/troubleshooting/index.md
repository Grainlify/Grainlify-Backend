# Troubleshooting Guide

This guide consolidates common issues and solutions for Grainlify Backend.

## Table of Contents

1. [Development Setup Issues](#development-setup-issues)
2. [GitHub OAuth Issues](#github-oauth-issues)
3. [GitHub App Issues](#github-app-issues)
4. [Callback URL Issues](#callback-url-issues)
5. [Database Issues](#database-issues)
6. [Deployment Issues](#deployment-issues)
7. [Webhook Issues](#webhook-issues)

---

## Development Setup Issues

### Air not found

**Error:** `air: command not found`

**Solution:**
```bash
# Install air
go install github.com/air-verse/air@latest

# Add to PATH (add to ~/.zshrc or ~/.bashrc)
export PATH=$PATH:$HOME/go/bin

# Verify installation
which air
```

### Server not restarting on file changes

**Symptoms:** Changes to `.go` files don't trigger auto-reload

**Solutions:**
- Check that you're editing `.go` files (not just config files)
- Check `tmp/build-errors.log` for build errors
- Make sure you're in the backend directory
- Verify `.air.toml` configuration is correct

### Port already in use

**Error:** `address already in use`

**Solutions:**
```bash
# Change PORT in your .env file
PORT=8081

# Or kill the existing process (macOS/Linux)
lsof -ti:8080 | xargs kill

# Or kill the existing process (Windows)
netstat -ano | findstr :8080
taskkill /PID <PID> /F
```

---

## GitHub OAuth Issues

### Invalid callback URL

**Error:** `redirect_uri_mismatch` or `Invalid callback URL`

**Solutions:**
1. Ensure the callback URL exactly matches what's in GitHub OAuth app settings
2. Check for trailing slashes (remove them)
3. Verify HTTPS is used for production (HTTP only for localhost)
4. Check environment variable: `GITHUB_OAUTH_REDIRECT_URL`

**Common callback URLs:**
- Local: `http://localhost:8080/auth/github/callback`
- Production: `https://your-domain.com/auth/github/callback`

### OAuth redirect loop

**Symptoms:** Infinite redirect between GitHub and backend

**Solutions:**
1. Check `GITHUB_OAUTH_REDIRECT_URL` matches GitHub OAuth app settings
2. Verify `GITHUB_LOGIN_SUCCESS_REDIRECT_URL` points to frontend
3. Check `FRONTEND_BASE_URL` is set correctly
4. Ensure JWT token is being generated and stored

### Missing GitHub OAuth credentials

**Error:** `GITHUB_OAUTH_CLIENT_ID not set` or similar

**Solutions:**
1. Create GitHub OAuth App at https://github.com/settings/developers
2. Set environment variables:
   - `GITHUB_OAUTH_CLIENT_ID`
   - `GITHUB_OAUTH_CLIENT_SECRET`
   - `GITHUB_OAUTH_REDIRECT_URL`
   - `GITHUB_LOGIN_SUCCESS_REDIRECT_URL`

---

## GitHub App Issues

### Installation fails with missing_installation_id

**Error:** `missing_installation_id` in logs

**Solutions:**
1. Verify GitHub App "Setup URL" is either:
   - Empty (recommended - GitHub will use Callback URL)
   - Set to the same callback URL as "Identifying and authorizing users"
2. **Do NOT** set it to frontend URL
3. Check `GITHUB_APP_ID` and `GITHUB_APP_SLUG` are correct
4. Verify private key is properly base64 encoded

### Private key errors

**Error:** `failed to generate JWT` or private key related errors

**Solutions:**
1. Generate a new private key in GitHub App settings
2. Download the `.pem` file (can only be downloaded once)
3. Base64 encode the private key:
   ```bash
   # macOS/Linux
   base64 -i grainlify.private-key.pem
   
   # Or using cat
   cat grainlify.private-key.pem | base64
   ```
4. Set `GITHUB_APP_PRIVATE_KEY` to the base64 string
5. Ensure no line breaks in the environment variable

### Webhook delivery failed

**Error:** Webhook deliveries failing in GitHub App settings

**Solutions:**
1. Verify webhook URL is accessible from internet
2. Check `PUBLIC_BASE_URL` is set correctly
3. Verify `GITHUB_WEBHOOK_SECRET` matches GitHub App settings
4. Ensure backend is running and `/webhooks/github` endpoint exists
5. For local development, use a tunnel (ngrok, loclx)

### Local development with webhooks

**Problem:** GitHub cannot reach localhost for webhooks

**Solution:** Use a tunneling service

**Using loclx (Recommended):**
```bash
# Install and start tunnel
loclx tunnel http --to localhost:8080

# Use the HTTPS URL provided (e.g., https://abc123.loclx.io)
# Update GitHub App webhook URL to: https://abc123.loclx.io/webhooks/github
```

**Using ngrok:**
```bash
# Start tunnel
ngrok http 8080

# Use the HTTPS URL provided
# Update GitHub App webhook URL to: https://abc123.ngrok.io/webhooks/github
```

**Important:**
- Tunnel URL changes each time you restart (free tier)
- Keep tunnel running while testing
- Update webhook URL in GitHub App settings after restart

---

## Callback URL Issues

### Multiple environment callback URLs

**Problem:** Different callback URLs for local, staging, production

**Solution:** Use environment-specific configuration

**Local Development:**
```bash
GITHUB_OAUTH_REDIRECT_URL=http://localhost:8080/auth/github/callback
PUBLIC_BASE_URL=http://localhost:8080
```

**Staging:**
```bash
GITHUB_OAUTH_REDIRECT_URL=https://staging.your-domain.com/auth/github/callback
PUBLIC_BASE_URL=https://staging.your-domain.com
```

**Production:**
```bash
GITHUB_OAUTH_REDIRECT_URL=https://api.your-domain.com/auth/github/callback
PUBLIC_BASE_URL=https://api.your-domain.com
```

### Verifying callback URL configuration

**Checklist:**
- [ ] Callback URL in GitHub OAuth App matches `GITHUB_OAUTH_REDIRECT_URL`
- [ ] Callback URL in GitHub App matches `GITHUB_LOGIN_REDIRECT_URL`
- [ ] `PUBLIC_BASE_URL` is set and accessible
- [ ] No trailing slashes in URLs (unless required)
- [ ] HTTPS used for production, HTTP for localhost

---

## Database Issues

### Database connection failed

**Error:** `connection refused` or `failed to connect to database`

**Solutions:**
1. Verify `DB_URL` is set correctly
2. Check PostgreSQL is running
3. Verify database credentials
4. Check network connectivity
5. For Railway: Copy DB_URL from service variables

### Migration failed

**Error:** Migration errors during startup

**Solutions:**
1. Check migration files in `migrations/` directory
2. Verify database permissions
3. Run migrations manually:
   ```bash
   go run ./cmd/migrate
   ```
4. Check for schema conflicts
5. Set `AUTO_MIGRATE=false` to skip auto-migration

### Database schema out of sync

**Symptoms:** API returns 500 errors related to database

**Solutions:**
1. Run migrations manually:
   ```bash
   go run ./cmd/migrate
   ```
2. Check migration logs for errors
3. Verify all migrations have been applied
4. Check database schema matches migration files

---

## Deployment Issues

### Railway deployment fails

**Error:** Build fails on Railway

**Solutions:**
1. Check build logs in Railway dashboard
2. Ensure Go version is compatible (check `go.mod`)
3. Verify all dependencies are in `go.mod`
4. Check `railway.json` configuration if present
5. Ensure `Root Directory` is set correctly (should be `.` or `backend`)

### Environment variables not set

**Symptoms:** App crashes or returns 500 errors

**Solutions:**
1. Check Railway service → Variables tab
2. Verify all required variables are set:
   - `DB_URL`
   - `JWT_SECRET`
   - `GITHUB_OAUTH_CLIENT_ID`
   - `GITHUB_OAUTH_CLIENT_SECRET`
   - `GITHUB_WEBHOOK_SECRET`
   - `PUBLIC_BASE_URL`
   - `FRONTEND_BASE_URL`
3. Redeploy after adding variables
4. Check Railway logs for missing variable errors

### Container stops unexpectedly

**Symptoms:** Container stops after running successfully

**Possible causes:**
1. **Railway Free Tier Auto-Sleep:** Expected behavior on free tier
   - Containers sleep after inactivity
   - Wake up on first request (cold start)
   - Solution: Upgrade to Developer plan to prevent auto-sleep

2. **Health Check Issues:** Configure health checks
   - Go to service → Settings → Health Checks
   - Set Health Check Path: `/health`
   - Set interval: 30 seconds
   - Set timeout: 10 seconds

3. **Resource Limits:** Hitting memory/CPU limits
   - Check Metrics tab in Railway dashboard
   - Free tier: 512MB RAM limit
   - Consider upgrading if consistently hitting limits

### CORS errors

**Symptoms:** Browser CORS errors when calling API

**Solutions:**
1. Update CORS configuration in `internal/api/api.go`
2. Add frontend domain to allowed origins
3. Ensure `AllowCredentials: true` if using cookies
4. Check preflight requests are handled

---

## Webhook Issues

### Webhook signature verification failed

**Error:** `invalid webhook signature`

**Solutions:**
1. Verify `GITHUB_WEBHOOK_SECRET` matches GitHub App settings
2. Ensure secret is at least 32 characters
3. Regenerate webhook secret in GitHub App if needed
4. Update environment variable and redeploy

### Webhook events not received

**Symptoms:** No webhook events in backend logs

**Solutions:**
1. Check webhook URL in GitHub App settings
2. Verify `PUBLIC_BASE_URL` is set correctly
3. Check webhook delivery logs in GitHub App settings
4. Ensure backend is running and accessible
5. For local development, verify tunnel is running

### Duplicate webhook events

**Symptoms:** Same event processed multiple times

**Solutions:**
1. Check Redis is configured for deduplication
2. Verify webhook deduplication logic is working
3. Check for multiple webhook deliveries in GitHub logs
4. Ensure idempotent event processing

---

## Getting Help

If you're still stuck:

1. **Check logs:** Review application logs for detailed error messages
2. **Verify configuration:** Double-check all environment variables
3. **Test locally:** Reproduce the issue in local development
4. **Check documentation:** Refer to specific setup guides:
   - [Documentation Index](../README.md)
   - [GitHub App Setup](../github-app/setup.md)
   - [Railway Deployment](../deployment/railway.md)
   - [API Endpoints](../reference/api-endpoints.md)
   - [Development Guide](../setup/development.md)

---

**Last Updated:** 2025-01-16
