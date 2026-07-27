# Railway Deployment Guide for Grainlify Backend

This guide walks you through deploying the Grainlify Go backend to Railway.

## Prerequisites

1. **Railway Account**: Sign up at [railway.app](https://railway.app)
2. **GitHub Repository**: Your code should be in a GitHub repository
3. **Railway CLI** (optional): Install for local management
   ```bash
   npm i -g @railway/cli
   ```

---

## Step 1: Create a New Railway Project

1. Go to [railway.app](https://railway.app) and sign in
2. Click **"New Project"**
3. Select **"Deploy from GitHub repo"**
4. Choose your repository (`Grainlify-Backend`)
5. Railway will detect it's a Go project

---

## Step 2: Add PostgreSQL Database

1. In your Railway project, click **"+ New"**
2. Select **"Database"** → **"Add PostgreSQL"**
3. Railway will create a PostgreSQL instance
4. **Copy the connection string** from the database service variables:
   - It will be in the format: `postgresql://postgres:password@hostname:port/railway`
   - This is your `DB_URL`

---

## Step 3: Add Redis (Optional but Recommended)

1. Click **"+ New"** → **"Database"** → **"Add Redis"**
2. Copy the Redis connection URL from the service variables
3. This will be your `REDIS_URL` (if you're using Redis)

---

## Step 4: Add NATS (Optional)

If you're using NATS for the event bus:

1. Click **"+ New"** → **"Database"** → **"Add NATS"**
2. Or use Railway's NATS service
3. Copy the NATS connection URL

---

## Step 5: Configure Environment Variables

In your Railway service (the Go API service), go to **"Variables"** and add the following:

### Required Variables

```bash
# Database
DB_URL=postgresql://postgres:password@hostname:port/railway
AUTO_MIGRATE=true

# JWT Authentication
JWT_SECRET=your-super-secret-jwt-key-min-32-chars-long

# GitHub OAuth
GITHUB_OAUTH_CLIENT_ID=your_github_oauth_client_id
GITHUB_OAUTH_CLIENT_SECRET=your_github_oauth_client_secret
GITHUB_OAUTH_REDIRECT_URL=https://your-railway-app.railway.app/auth/github/callback
GITHUB_LOGIN_REDIRECT_URL=https://your-railway-app.railway.app/auth/github/callback
GITHUB_LOGIN_SUCCESS_REDIRECT_URL=https://your-frontend-domain.com
GITHUB_OAUTH_SUCCESS_REDIRECT_URL=https://your-frontend-domain.com

# GitHub Webhooks
GITHUB_WEBHOOK_SECRET=your_github_webhook_secret_string

# Public Base URL (your Railway app URL)
PUBLIC_BASE_URL=https://your-railway-app.railway.app

# Token Encryption
TOKEN_ENC_KEY_B64=your-base64-encoded-32-byte-key

# Admin Bootstrap
ADMIN_BOOTSTRAP_TOKEN=your-secure-bootstrap-token

# Didit KYC (if using)
DIDIT_API_KEY=your_didit_api_key
DIDIT_WORKFLOW_ID=your_didit_workflow_id
DIDIT_WEBHOOK_SECRET=your_didit_webhook_secret

# NATS (if using)
NATS_URL=nats://your-nats-url

# App Configuration
APP_ENV=production
LOG_LEVEL=info
PORT=8080
```

### How to Generate Required Secrets

#### JWT_SECRET
```bash
openssl rand -base64 32
```

#### TOKEN_ENC_KEY_B64 (32 bytes, base64 encoded)
```bash
openssl rand -base64 32
```

#### ADMIN_BOOTSTRAP_TOKEN
```bash
openssl rand -hex 32
```

#### GITHUB_WEBHOOK_SECRET
```bash
openssl rand -hex 32
```

---

## Step 6: Configure Build Settings

Railway will auto-detect Go from your `go.mod` file. Configure the following:

1. Go to your service → **"Settings"** → **"Build & Deploy"**
2. **Root Directory**: Set to `backend` (since your backend code is in the `backend` folder)
3. Railway will automatically:
   - Detect Go from `go.mod`
   - Run `go build` to build your application
   - Use the main package in `cmd/api/main.go`
   - Start with `./api` (or the binary name)

**Note**: Railway automatically sets the `PORT` environment variable. Your code already handles this via:
```go
port := getEnv("PORT", "8080")
httpAddr = ":" + port
```

The `railway.json` file in the repo is optional - Railway will auto-detect Go projects.

---

## Step 7: Update GitHub OAuth Settings

1. Go to GitHub → **Settings** → **Developer settings** → **OAuth Apps**
2. Edit your OAuth app
3. Update **Authorization callback URL** to:
   ```
   https://your-railway-app.railway.app/auth/github/callback
   ```
4. Save changes

---

## Step 8: Update Webhook URLs

### GitHub Webhooks
For each project repository:
1. Go to repository → **Settings** → **Webhooks**
2. Update webhook URL to:
   ```
   https://your-railway-app.railway.app/webhooks/github
   ```
3. Update webhook secret to match `GITHUB_WEBHOOK_SECRET`

### Didit Webhooks
1. In Didit dashboard, update webhook URL to:
   ```
   https://your-railway-app.railway.app/webhooks/didit
   ```

---

## Step 9: Deploy

1. Railway will automatically deploy when you push to your main branch
2. Or click **"Deploy"** in the Railway dashboard
3. Check the **"Deployments"** tab for build logs
4. Once deployed, your API will be available at:
   ```
   https://your-railway-app.railway.app
   ```

---

## Step 10: Run Database Migrations

The repository has three deployment entrypoints:

- API: `go run ./cmd/api` / a binary built from `./cmd/api`
- Worker: `go run ./cmd/worker` / a binary built from `./cmd/worker`
- Migration job: `go run ./cmd/migrate` / a binary built from `./cmd/migrate`

Migrations can run automatically from the API process when `AUTO_MIGRATE=true` is
set. On startup, `cmd/api/main.go` checks whether migrations are needed and then
calls `internal/migrate.Up` before starting the HTTP server. For production, prefer
`AUTO_MIGRATE=false` on long-running API replicas and run migrations explicitly as
a one-off job so that rollout order is intentional and observable.

To run manually (if needed):

1. Install Railway CLI: `npm i -g @railway/cli`
2. Login: `railway login`
3. Link project: `railway link`
4. Run migrations with the current Railway environment:
   ```bash
   railway run go run ./cmd/migrate
   ```

`cmd/migrate/main.go` loads the same environment/configuration as the app, opens a
PostgreSQL connection from `DB_URL`, and calls `migrate.Up` with a 60-second outer
startup context. The migration implementation uses embedded files from
`migrations/`, the `schema_migrations` table, a small startup jitter, and retry
loops for PostgreSQL migration-lock contention. It treats `migrate.ErrNoChange` as
success, so rerunning the command is safe when the database is already current.

---

## Migration ordering

Use an **expand → deploy → contract** migration strategy for zero-downtime
deployments:

1. **Expand first**: apply only backward-compatible schema changes before the new
   API and worker binaries are rolled out. Safe examples include adding nullable
   columns, adding tables, creating indexes concurrently where applicable, or
   adding code paths that tolerate both old and new shapes.
2. **Deploy binaries second**: deploy the new `cmd/api` service and the matching
   `cmd/worker` service after the schema can support both old and new code.
3. **Contract later**: remove columns, tighten constraints, rename fields without
   compatibility shims, or drop tables only after every old API/worker instance is
   stopped and a separate release has proven the new code no longer depends on the
   old schema.

For zero-downtime deploys, every migration that runs before or during rollout must
be backward compatible with the previous API and worker binaries. This matters
because Railway may have old and new replicas running at the same time during a
deploy, and a rollback may put the previous binary back against the already-migrated
database. If a schema change cannot be made backward compatible, schedule downtime,
stop the API and worker services, take a database backup, apply the migration, then
start the new binaries.

Recommended production order:

1. Confirm the release's migrations are backward compatible with the currently
   deployed `cmd/api` and `cmd/worker` binaries.
2. Run `railway run go run ./cmd/migrate` once and verify the log line
   `migrations applied` or `migrations up to date, no changes needed`.
3. Deploy the API service built from `./cmd/api`.
4. Deploy the worker service built from `./cmd/worker`.
5. Verify `/health`, `/ready`, worker logs, and any release-specific smoke tests.

---

## Rollback

Use this procedure when a deploy must be reverted. The default rollback is a
**binary rollback**, not a destructive database rollback. Do not run down migrations
in production unless an incident commander explicitly approves it and a current
database backup has been captured.

1. **Triage and freeze changes**
   - Stop additional deploys and pause non-essential manual writes or admin jobs.
   - Identify the bad deployment and the last known-good commit/deployment for the
     API and worker services.
   - Check whether the bad release ran migrations with
     `railway run go run ./cmd/migrate` or API `AUTO_MIGRATE=true`.

2. **Roll back long-running binaries**
   - In Railway, redeploy the last known-good deployment for the API service, or
     push/redeploy the last known-good commit for the service built from
     `./cmd/api`.
   - Redeploy the matching last known-good worker deployment for the service built
     from `./cmd/worker`. Keep API and worker versions paired unless the incident
     plan says otherwise.
   - If the worker is causing data corruption or retry storms, temporarily scale
     the worker service to zero replicas before redeploying it.

3. **Leave compatible migrations in place**
   - If the release followed the backward-compatible migration rules above, leave
     the database at its current schema version. The previous binaries must be able
     to run against that schema.
   - Verify that the rolled-back API starts with `AUTO_MIGRATE=false` unless you
     intentionally want startup to call `migrate.Up` again. Rerunning `cmd/migrate`
     is safe when there is no change, but it should not be part of an emergency
     rollback unless needed.

4. **Only consider database rollback for incompatible or destructive changes**
   - Capture a fresh backup/snapshot first.
   - Review the relevant `migrations/*.down.sql` files and the behavior in
     `internal/migrate/migrate.go` before running any downward migration.
   - Prefer a forward hotfix migration that restores compatibility over running
     down migrations on production data.
   - If a down migration is approved, run it from controlled operational tooling or
     a purpose-built recovery command. The current `cmd/migrate` entrypoint only
     runs `migrate.Up`; it does not expose a CLI flag for `Down` or `Steps`.

5. **Verify rollback**
   - Check API health: `curl https://your-railway-app.railway.app/health`.
   - Check database readiness: `curl https://your-railway-app.railway.app/ready`.
   - Check API and worker logs for startup, migration, database, and NATS errors.
   - Confirm user-facing functionality and queued background work have recovered.

---

## Step 11: Verify Deployment

1. **Health Check**:
   ```bash
   curl https://your-railway-app.railway.app/health
   ```
   Should return: `{"ok":true,"service":"grainlify-api"}`

2. **Readiness Check**:
   ```bash
   curl https://your-railway-app.railway.app/ready
   ```
   Should return: `{"ok":true,"db":"connected"}`

3. **Test API Endpoint**:
   ```bash
   curl https://your-railway-app.railway.app/ecosystems
   ```

---

## Step 12: Set Up Custom Domain (Optional)

1. In Railway, go to your service → **"Settings"** → **"Networking"**
2. Click **"Generate Domain"** or **"Add Custom Domain"**
3. Follow the DNS configuration instructions
4. Update `PUBLIC_BASE_URL` to your custom domain

---

## Step 13: Worker Service {#worker-service}

The background worker is a **separate Railway service** built from the same
repository. It handles GitHub webhook ingestion (via NATS) and the periodic
sync-jobs loop.

### Why a separate service?

- The API server does **not** run background jobs when `NATS_URL` is set —
  it logs "NATS configured (use external worker)" and skips the in-process
  runner.
- A separate service can be scaled, restarted, and monitored independently.
- Multiple worker replicas share NATS queue-group load without duplicate
  processing.

### Create the worker service

1. In your Railway project, click **"+ New"** → **"GitHub Repo"** → select the
   same repository.
2. In the new service's **"Settings" → "Build & Deploy"**:
   - **Build Command**: `go build -o ./worker ./cmd/worker`
   - **Start Command**: `./worker`
3. Go to **"Variables"** and add at minimum:

```bash
# Required
DB_URL=<same PostgreSQL URL as the API service>
NATS_URL=<your NATS URL>
APP_ENV=production
LOG_LEVEL=info

# Needed by syncjobs to decrypt GitHub OAuth tokens
TOKEN_ENC_KEY_B64=<same value as API service>

# Needed by the GitHub API client used inside syncjobs
GITHUB_APP_ID=<same as API service>
GITHUB_APP_SLUG=<same as API service>
GITHUB_APP_PRIVATE_KEY=<same as API service>
GITHUB_WEBHOOK_SECRET=<same as API service>
```

4. Click **"Deploy"**.

> **Tip**: Use Railway's [shared variables](https://docs.railway.app/guides/variables#shared-variables)
> or a reference variable (`${{api.DB_URL}}`) to avoid duplicating secrets
> across services.

### Verify the worker is running

Check the service logs for:

```
=== Grainlify Worker Starting ===
github webhook consumer subscribed
starting syncjobs worker
=== Grainlify Worker Running ===
```

### Scaling

To run multiple replicas, increase the service's instance count in
**Settings → Replicas**. Workers use NATS queue groups and PostgreSQL
`FOR UPDATE SKIP LOCKED`, so replicas are safe to run concurrently.

### Graceful shutdown

Railway sends `SIGTERM` before terminating a container. The worker drains
in-flight jobs for up to 10 seconds, then exits cleanly. No additional
configuration is required.

---

## Environment Variables Reference

### Database
- `DB_URL` - PostgreSQL connection string (from Railway PostgreSQL service)
- `AUTO_MIGRATE` - Set to `true` to auto-run migrations on startup

### Authentication
- `JWT_SECRET` - Secret key for JWT token signing (min 32 chars)
- `TOKEN_ENC_KEY_B64` - Base64-encoded 32-byte key for encrypting GitHub tokens

### GitHub OAuth
- `GITHUB_OAUTH_CLIENT_ID` - GitHub OAuth App Client ID
- `GITHUB_OAUTH_CLIENT_SECRET` - GitHub OAuth App Client Secret
- `GITHUB_OAUTH_REDIRECT_URL` - OAuth callback URL (usually same as login redirect)
- `GITHUB_LOGIN_REDIRECT_URL` - Login callback URL
- `GITHUB_LOGIN_SUCCESS_REDIRECT_URL` - Frontend URL after successful login
- `GITHUB_OAUTH_SUCCESS_REDIRECT_URL` - Frontend URL after OAuth linking

### GitHub Webhooks
- `GITHUB_WEBHOOK_SECRET` - Secret for verifying GitHub webhook signatures
- `PUBLIC_BASE_URL` - Your Railway app URL (for webhook endpoints)

### Admin
- `ADMIN_BOOTSTRAP_TOKEN` - Token for bootstrapping first admin user

### Didit KYC
- `DIDIT_API_KEY` - Didit API key
- `DIDIT_WORKFLOW_ID` - Didit workflow ID
- `DIDIT_WEBHOOK_SECRET` - Secret for Didit webhooks

### Infrastructure
- `NATS_URL` - NATS connection URL (if using NATS)
- `REDIS_URL` - Redis connection URL (if using Redis)

### App Config
- `APP_ENV` - Environment (`production`, `staging`, `dev`)
- `LOG_LEVEL` - Logging level (`debug`, `info`, `warn`, `error`)
- `PORT` - Port to listen on (**Railway automatically sets this** - don't set manually)

---

## Troubleshooting

### Build Fails

1. Check build logs in Railway dashboard
2. Ensure Go version is compatible (check `go.mod`)
3. Verify all dependencies are in `go.mod`

### Database Connection Fails

1. Verify `DB_URL` is correct (from Railway PostgreSQL service)
2. Check database service is running
3. Ensure `AUTO_MIGRATE=true` if migrations haven't run

### OAuth Redirect Errors

1. Verify `GITHUB_OAUTH_REDIRECT_URL` matches GitHub OAuth app settings
2. Ensure `PUBLIC_BASE_URL` is set correctly
3. Check that URLs use `https://` (not `http://`)

### Webhook Not Receiving Events

1. Verify `PUBLIC_BASE_URL` is set correctly
2. Check webhook URL in GitHub/Didit matches: `{PUBLIC_BASE_URL}/webhooks/{service}`
3. Verify webhook secret matches
4. Check Railway logs for webhook delivery errors

### CORS Errors

1. Update CORS configuration in `internal/api/api.go` to include your frontend domain
2. Ensure `AllowCredentials: true` is set if using cookies

### High Memory Usage

1. Railway free tier has memory limits
2. Consider upgrading plan or optimizing code
3. Check for memory leaks in handlers

### Container Stops Unexpectedly

If your container stops after running successfully (receives SIGTERM and shuts down gracefully):

1. **Railway Free Tier Auto-Sleep**: Railway's free tier automatically puts containers to sleep after periods of inactivity to save resources. This is expected behavior.
   - Containers will wake up automatically when they receive a request
   - First request after sleep may take longer (cold start)
   - Solution: Upgrade to Developer plan ($20/month) to prevent auto-sleep

2. **Health Check Configuration**: Configure health checks in Railway dashboard:
   - Go to your service → **Settings** → **Health Checks**
   - Set **Health Check Path** to: `/health`
   - Set **Health Check Interval** to: `30` seconds
   - Set **Health Check Timeout** to: `10` seconds
   - This helps Railway know your service is healthy

3. **Deployment Restart**: Railway may restart containers during deployments or updates
   - Check **Deployments** tab in Railway dashboard
   - Look for recent deployments that might have triggered a restart

4. **Resource Limits**: Check if you're hitting memory or CPU limits
   - View **Metrics** tab in Railway dashboard
   - Free tier has 512MB RAM limit per service
   - Consider upgrading if consistently hitting limits

5. **Manual Stop**: Check if container was manually stopped
   - Review Railway dashboard activity logs
   - Verify no manual stop/restart was triggered

**Note**: The application handles SIGTERM gracefully, so shutdown logs showing "shutdown signal received" and "shutdown complete" are normal when Railway stops the container.

---

## Railway CLI Commands

```bash
# Login
railway login

# Link to project
railway link

# View logs
railway logs

# Run command in Railway environment
railway run go run ./cmd/migrate

# Open service in browser
railway open

# View variables
railway variables
```

---

## Cost Estimation

- **Free Tier**: $5/month credit
  - 500 hours of usage
  - 512MB RAM per service
  - 1GB storage
  - Suitable for development/testing

- **Developer Plan**: $20/month
  - $5 credit included
  - Better performance
  - More resources

- **Team Plan**: Custom pricing
  - For production workloads

**Note**: PostgreSQL, Redis, and NATS are separate services with their own costs.

---

## Production Checklist

- [ ] All environment variables set
- [ ] Database migrations run successfully
- [ ] GitHub OAuth app configured with correct callback URL
- [ ] Webhook URLs updated in GitHub/Didit
- [ ] `PUBLIC_BASE_URL` set to production domain
- [ ] `APP_ENV=production` set
- [ ] `LOG_LEVEL=info` or `warn` (not `debug`)
- [ ] Custom domain configured (if using)
- [ ] Health checks passing
- [ ] CORS configured for frontend domain
- [ ] Admin user bootstrapped
- [ ] Monitoring/logging set up (optional)

---

## Support

- Railway Docs: https://docs.railway.app
- Railway Discord: https://discord.gg/railway
- Railway Status: https://status.railway.app

---

**Last Updated**: 2025-12-31

