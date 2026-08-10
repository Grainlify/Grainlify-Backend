# bash rather than sh, for `set -o pipefail` in the test target.
SHELL := /bin/bash

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
#
# -timeout: STOPGAP, not a fix. internal/handlers crossed Go's default 600s
# on 2026-08-10 having climbed 438 -> 494 -> 552 -> 600+ over a few rounds of
# added tests. Raising the ceiling buys a few more rounds and then we are
# back here.
#
# Measured cause: two leaderboard tests are 69% of the suite (489s of 712s).
# They page the whole global leaderboard, whose query is quadratic in a
# contributor count that grows every run because this database is never
# truncated. It is NOT the per-test setup cost, which is the ~223s the other
# 231 tests share between them. See docs/TESTING-DEBT.md. Raise this again
# only alongside fixing those two tests, not instead of it.
#
# Full output is teed to test-output.log because a timeout prints a
# goroutine dump naming the stuck test, and piping the run through tail
# truncates exactly the part worth reading.
TEST_TIMEOUT ?= 30m
TEST_LOG ?= test-output.log

# pipefail is load-bearing: without it the recipe's exit status comes from
# tee, which always succeeds, and a failing suite would report as passing.
test:
	@set -o pipefail; TEST_DB_URL=$(TEST_DB_URL) go test -count=1 -p 1 -cover -timeout $(TEST_TIMEOUT) ./... 2>&1 | tee $(TEST_LOG)
	@echo "full output: $(TEST_LOG)"


















