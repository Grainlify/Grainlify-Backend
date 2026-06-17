# Development Guide

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
