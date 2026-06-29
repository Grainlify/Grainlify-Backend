# Development Guide

## Logging Guidelines

To maintain security and reduce noise in production, follow these logging guidelines:
1. **Never log secrets or PII at INFO level.** This includes tokens (`SOROBAN_SOURCE_SECRET`, OAuth tokens, JWT secrets), emails, and KYC decision data.
2. **Use `slog.Debug` for verbose data.** Large payloads (like webhook bodies) and full header dumps must be logged at `Debug` level, not `Info`.
3. **Redact sensitive fields.** When logging maps or structs that contain addresses or amounts, use the `logger.RedactMap` helper to sanitize the output before logging at `Info`.

Example:
```go
import "github.com/jagadeesh/grainlify/backend/internal/logger"

redactedArgs := logger.RedactMap(args)
slog.Info("interaction occurred", "args", redactedArgs) // Safe for INFO
slog.Debug("interaction detailed", "args", args)        // Safe for DEBUG
```

## Running the Backend Server

### Option 1: Auto-reload with Air (Recommended for Development) ⚡

The server will **automatically restart** when you make changes to any `.go` file.

```bash
# Quick start - recommended (handles PATH and installation automatically)
./run-dev.sh

# Or directly with air (if already installed)
air

# Or using make
make dev
```

**What gets watched:**
- All `.go` files in `cmd/`, `internal/`, and root
- Automatically excludes: `tmp/`, `vendor/`, `testdata/`, `migrations/`, `.git/`, test files
- Restarts within 1 second of file changes

**First time setup:**
```bash
# Install air
go install github.com/air-verse/air@latest

# Add to PATH (add to ~/.zshrc or ~/.bashrc)
export PATH=$PATH:$HOME/go/bin
```

### Option 2: Standard Go Run (No Auto-reload)

```bash
go run ./cmd/api

# Or using make
make run
```

## Installing Air

If `air` is not found, install it:

```bash
go install github.com/air-verse/air@latest
```

Make sure `~/go/bin` is in your PATH. Add this to your `~/.zshrc` or `~/.bashrc`:

```bash
export PATH=$PATH:$HOME/go/bin
```

## Configuration

Air configuration is in `.air.toml`. It watches for changes in:
- All `.go` files
- Excludes `tmp/`, `vendor/`, `testdata/`, `migrations/`, `.git/`
- Excludes `*_test.go` files

## Build Commands

```bash
# Build binary
make build
# or
go build -o ./api ./cmd/api

# Run migrations
go run ./cmd/migrate

# Run worker
go run ./cmd/worker
```

## Running Tests

### Unit tests (no database required)

```bash
go test ./internal/handlers/... ./internal/ingest/...
```

The handler tests (`internal/handlers`) are pure unit tests with a mock bus — no external dependencies.

### HTTP Integration Tests (no database required) 🔌

We have comprehensive HTTP integration tests covering the assembled Fiber app (routing, middleware like requestid, CORS, recover, rate limiters, auth/role gates, and response shapes) without requiring a database.

Run the API integration tests:

```bash
go test -v ./internal/api/... -race
```

These integration tests drive the app via `app.Test(httptest.NewRequest(...))` using a mock database seam (`db.DBPool`) and a mock message bus. The test covers:
- **Public endpoints**: (e.g. `/health`, `/projects`) returning successful statuses.
- **Route Precedence**: Explicitly asserting that specific routes (like `/projects/mine` and `/projects/pending-setup`) resolve before parameterized routes (like `/projects/:id`).
- **Auth and Role Gates**: Asserting that `RequireAuth` and `RequireRole` block invalid/missing tokens (with 401 Unauthorized) and insufficient roles (with 403 Forbidden).
- **Error Responses**: Asserting that error envelopes use the standard structure (`ErrorEnvelope`) and include `request_id` for traceability.

### Integration tests (requires PostgreSQL)

DB integration tests in `internal/ingest` are gated behind the `TEST_DB_URL` environment variable.  
When the variable is absent the tests are **skipped automatically** — they never fail in CI unless you opt in.

Set `TEST_DB_URL` to a throwaway Postgres database:

```bash
export TEST_DB_URL="postgres://user:pass@localhost:5432/grainlify_test?sslmode=disable"
go test ./internal/ingest/...
```

The test harness calls `migrate.Up` automatically, so the target database only needs to exist (it does not need pre-created tables).  
Each test cleans up the rows it inserts via `t.Cleanup`, so the schema stays clean between runs.

> **CI**: add `TEST_DB_URL` as a secret/environment variable in your pipeline to enable DB integration tests.

## Running Lint

CI runs `golangci-lint` with the pinned version in `.github/workflows/ci.yml`.

Install the same version locally:

```bash
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
```

Then run:

```bash
golangci-lint run ./...
# or
make lint
```

The lint configuration is in `.golangci.yml`. Existing legacy findings are explicitly excluded there so new changes can be checked without forcing a broad cleanup in the first linting PR.
