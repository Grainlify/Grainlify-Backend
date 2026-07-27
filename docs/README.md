# Grainlify Backend Documentation

Indexed documentation for setup, GitHub integration, OAuth, deployment, and troubleshooting.

## Setup

| Document | Description |
|----------|-------------|
| [Quick Start](setup/quick-start.md) | Auto-reload dev server with Air |
| [Development Guide](setup/development.md) | Logging guidelines, running tests, dev commands |
| [Database Setup (Docker)](setup/docker-postgres.md) | PostgreSQL via Docker Compose |
| [Quick Database Setup](setup/quick-db-setup.md) | Use an existing Postgres container |
| [CORS Policy](setup/cors.md) | Origin allowlist and preview wildcards |
| [Architecture](setup/architecture.md) | Backend architecture overview |

## GitHub App

| Document | Description |
|----------|-------------|
| [GitHub App Setup](github-app/setup.md) | Full GitHub App configuration |
| [Installation Callbacks](github-app/callbacks.md) | Callback URL setup and debugging (canonical) |
| [Private Key Setup](github-app/private-key.md) | `GITHUB_APP_PRIVATE_KEY` configuration |
| [Private vs Public App](github-app/private-vs-public.md) | Installation visibility settings |
| [Webhook Secrets](github-app/webhooks.md) | `GITHUB_WEBHOOK_SECRET` explained |

## OAuth

| Document | Description |
|----------|-------------|
| [Multi-Environment Setup](oauth/multi-env-setup.md) | OAuth across dev/staging/production |
| [Environment Redirect URLs](oauth/env-redirect-urls.md) | Redirect URL configuration per env |
| [OAuth App Settings](oauth/app-settings.md) | GitHub OAuth app registration |
| [Redirect Debugging](oauth/redirects.md) | Fix wrong redirect / localhost issues (canonical) |
| [OAuth 2.0 Spec Compliance](oauth/spec-compliance.md) | State parameter and CSRF design |

## Deployment

| Document | Description |
|----------|-------------|
| [Railway Deployment](deployment/railway.md) | Deploy to Railway |

## Troubleshooting

| Document | Description |
|----------|-------------|
| [Troubleshooting Guide](troubleshooting/index.md) | Common issues (OAuth, DB, webhooks, deployment) |
| [Sync Issues & PRs](troubleshooting/sync-issues.md) | GitHub rate limits and sync failures |

## Reference

| Document | Description |
|----------|-------------|
| [API Endpoints](reference/api-endpoints.md) | Complete REST API reference |
| [OpenAPI Spec](reference/openapi-spec.md) | OpenAPI 3.1 spec — location, validation, and update guide |

## Interactive API docs

When the API is running:

- Swagger UI: `http://localhost:8080/docs`
- OpenAPI spec: `http://localhost:8080/openapi.yaml`

## Consolidated debug notes

The following root-level debug files were merged into canonical pages:

| Former files | Canonical page |
|--------------|----------------|
| `CALLBACK_URL_SETUP.md`, `GITHUB_APP_CALLBACK_DEBUG.md`, `GITHUB_APP_CALLBACK_FIX.md`, `VERIFY_CALLBACK_URL.md`, `FIX_INSTALLATION_ACCESS.md` | [github-app/callbacks.md](github-app/callbacks.md) |
| `OAUTH_REDIRECT_DEBUG.md` | [oauth/redirects.md](oauth/redirects.md) |

All topical docs now live under `docs/`; the repository root keeps only `README.md` and code.
