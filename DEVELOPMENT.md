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
