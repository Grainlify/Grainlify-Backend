// Package dbtest provides a shared real-Postgres test database helper for
// packages that need one (internal/handlers, internal/ingest,
// internal/syncjobs). Not a _test.go file so it can be imported as a normal
// package dependency from each package's own _test.go files.
package dbtest

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/migrate"
)

// DB returns a db.DB backed by a live pool from TEST_DB_URL, migrated to the
// current schema. Tests that need a real Postgres call this first and get
// skipped (not failed) when TEST_DB_URL isn't set, so `go test ./...` stays
// green on a machine with no local Postgres running.
func DB(t *testing.T) *db.DB {
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

// FreshDB is DB, but in a database of its own, created for this test and
// dropped after it. For tests whose subject reads the whole of a table - a
// queue worker claiming "any pending job" - and so would act on rows other
// tests left in the shared database.
func FreshDB(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DB_URL")
	if dsn == "" {
		t.Skip("TEST_DB_URL not set; skipping test that needs a live database")
	}
	ctx := context.Background()

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse TEST_DB_URL: %v", err)
	}
	admin, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer admin.Close()

	name := "fresh_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}

	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pgxpool.New(%s): %v", name, err)
	}
	t.Cleanup(func() {
		pool.Close()
		if a, err := pgxpool.New(context.Background(), dsn); err == nil {
			_, _ = a.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			a.Close()
		}
	})
	if err := migrate.Up(ctx, pool); err != nil {
		t.Fatalf("migrate.Up: %v", err)
	}
	return &db.DB{Pool: pool}
}
