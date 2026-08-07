package handlers_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/migrate"
)

// testDB returns a db.DB backed by a live pool from TEST_DB_URL, migrated to
// the current schema. Tests that need a real Postgres call this first and
// get skipped (not failed) when TEST_DB_URL isn't set, so `go test ./...`
// stays green on a machine with no local Postgres running.
func testDB(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DB_URL")
	if dsn == "" {
		t.Skip("TEST_DB_URL not set; skipping test that needs a live database")
	}

	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := migrate.Up(context.Background(), pool); err != nil {
		t.Fatalf("migrate.Up: %v", err)
	}

	return &db.DB{Pool: pool}
}
