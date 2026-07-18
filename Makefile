.PHONY: run dev install-air lint build build-worker run-worker build-prod test openapi-validate openapi-sync

# Install air for live reload
install-air:
	@echo "Installing air..."
	@go install github.com/air-verse/air@latest
	@echo "Air installed! Make sure ~/go/bin (or $${GOPATH}/bin) is in your PATH"

# Run with air (auto-reload on file changes)
dev:
	@if command -v air > /dev/null; then \
		air; \
	else \
		echo "Air not found. Installing..."; \
		$(MAKE) install-air; \
		echo "Please add ~/go/bin to your PATH or run: export PATH=\$$PATH:~/go/bin"; \
		echo "Then run 'make dev' again"; \
	fi

# Run without air (standard go run)
run:
	@go run ./cmd/api

# Build the API binary
build:
	@go build -o ./api ./cmd/api

# Build the binary with version metadata injected via ldflags.
build-prod:
	@go build -ldflags="\
		-X main.Version=$$(git describe --tags --always 2>/dev/null || echo dev) \
		-X main.Commit=$$(git rev-parse --short HEAD) \
		-X main.BuildTime=$$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
		-o ./api ./cmd/api

# Build the worker binary
build-worker:
	@go build -o ./worker ./cmd/worker

# Run the worker (requires DB_URL and NATS_URL in env / .env)
run-worker:
	@go run ./cmd/worker

# Run static analysis with the pinned golangci-lint configuration.
lint:
	@golangci-lint run ./...

# Run unit tests with race detection; skips tests that require live network/DB.
test:
	@go test -race -short ./...

# Validate the OpenAPI spec against the OpenAPI 3.x schema.
openapi-validate:
	@go test -run TestOpenAPISpecValid ./internal/api/ -v

# Sync the root-level openapi.yaml mirror with the canonical copy.
openapi-sync:
	@cp internal/api/openapi.yaml openapi.yaml 2>/dev/null || copy /y internal\api\openapi.yaml openapi.yaml >nul
	@echo "openapi.yaml synced"


















