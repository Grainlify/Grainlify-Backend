.PHONY: run dev install-air test test-db-create test-db-drop

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

# Build the binary
build:
	@go build -o ./api ./cmd/api

# Local test Postgres (matches CI's service-container database name).
# Assumes a locally-running Postgres reachable with trust auth as the
# current OS user - adjust TEST_DB_URL if your local setup differs.
TEST_DB_URL ?= postgres://$(shell whoami)@localhost:5432/grainlify_test?sslmode=disable

test-db-create:
	@createdb grainlify_test 2>/dev/null || echo "grainlify_test already exists"

test-db-drop:
	@dropdb --if-exists grainlify_test

# Run the full test suite. DB-backed tests self-skip via t.Skip when
# TEST_DB_URL is unset, so plain `go test ./...` also stays green without
# Postgres - this target just points them at a real local database.
#
# -p 1 runs one package's test binary at a time. Several packages share this
# one Postgres database and mutate global state in it directly
# (schema_migrations version/dirty flags, broad TRUNCATEs between tests), so
# Go's default concurrent-per-package test execution would race on that
# shared state without -p 1 serializing it.
test:
	@TEST_DB_URL=$(TEST_DB_URL) go test -count=1 -p 1 -cover ./...


















