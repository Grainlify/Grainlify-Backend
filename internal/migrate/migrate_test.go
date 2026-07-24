package migrate

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jagadeesh/grainlify/backend/migrations"
)

func TestGetLatestMigrationVersion_ReturnsHighest(t *testing.T) {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("iofs.New: %v", err)
	}

	v, err := getLatestMigrationVersion(src)
	if err != nil {
		t.Fatalf("getLatestMigrationVersion: %v", err)
	}

	want := uint(32)
	if v != want {
		t.Fatalf("got version %d, want %d", v, want)
	}
}

func TestMigrationVersions_AreContiguous(t *testing.T) {
	var versions []uint
	err := fs.WalkDir(migrations.FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".up.sql") {
			return nil
		}
		parts := strings.SplitN(path, "_", 2)
		if len(parts) < 2 {
			return nil
		}
		v, err := strconv.ParseUint(parts[0], 10, 64)
		if err != nil {
			return nil
		}
		versions = append(versions, uint(v))
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}

	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
	if len(versions) == 0 {
		t.Fatal("no .up.sql migration files found")
	}

	for i, v := range versions {
		want := uint(i + 1)
		if v != want {
			t.Fatalf("migration at position %d has version %d, want %d — gap or numbering error", i, v, want)
		}
	}
}

func TestIrreversibleVersions_ContainsKnownMarkers(t *testing.T) {
	versions, err := irreversibleVersions()
	if err != nil {
		t.Fatalf("irreversibleVersions: %v", err)
	}
	if len(versions) == 0 {
		t.Fatal("expected at least one irreversible marker")
	}
	if !versions[7] {
		t.Error("expected version 7 to be marked irreversible (000007_remove_chain_from_projects)")
	}
}

func TestIsIrreversible_KnownVersion(t *testing.T) {
	ok, err := isIrreversible(7)
	if err != nil {
		t.Fatalf("isIrreversible(7): %v", err)
	}
	if !ok {
		t.Error("expected version 7 to be irreversible")
	}
}

func TestIsIrreversible_UnknownVersion(t *testing.T) {
	ok, err := isIrreversible(1)
	if err != nil {
		t.Fatalf("isIrreversible(1): %v", err)
	}
	if ok {
		t.Error("expected version 1 to not be irreversible")
	}
}

func TestCollectPendingVersions_FromVersion(t *testing.T) {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("iofs.New: %v", err)
	}
	pending, err := collectPendingVersions(src, 5, nil)
	if err != nil {
		t.Fatalf("collectPendingVersions: %v", err)
	}
	if len(pending) == 0 {
		t.Fatal("expected pending versions after version 5")
	}
	if pending[0] != 6 {
		t.Fatalf("first pending version: got %d, want 6", pending[0])
	}
}

func TestCollectPendingVersions_FromNilVersion(t *testing.T) {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("iofs.New: %v", err)
	}
	pending, err := collectPendingVersions(src, 0, migrate.ErrNilVersion)
	if err != nil {
		t.Fatalf("collectPendingVersions: %v", err)
	}
	if len(pending) == 0 {
		t.Fatal("expected pending versions when no version applied")
	}
	if pending[0] != 1 {
		t.Fatalf("first pending version: got %d, want 1", pending[0])
	}
}

func TestCollectPendingVersions_UnknownStateReturnsNil(t *testing.T) {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("iofs.New: %v", err)
	}
	pending, err := collectPendingVersions(src, 0, fmt.Errorf("some error"))
	if err != nil {
		t.Fatalf("collectPendingVersions: %v", err)
	}
	if pending != nil {
		t.Fatal("expected nil pending when version state is unknown")
	}
}

func TestNeedsMigration_NilPool(t *testing.T) {
	needs, err := NeedsMigration(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for nil pool")
	}
	if needs {
		t.Fatal("expected false for nil pool")
	}
}

func setupTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DB_URL")
	if dsn == "" {
		t.Skip("TEST_DB_URL not set — skipping DB integration tests")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func dropSchemaMigrations(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `DROP TABLE IF EXISTS schema_migrations`)
	if err != nil {
		t.Fatalf("drop schema_migrations: %v", err)
	}
}

func insertSchemaMigrations(t *testing.T, pool *pgxpool.Pool, version uint, dirty bool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version bigint NOT NULL,
			dirty  boolean NOT NULL
		)
	`)
	if err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	_, err = pool.Exec(context.Background(), `
		INSERT INTO schema_migrations (version, dirty) VALUES ($1, $2)
	`, version, dirty)
	if err != nil {
		t.Fatalf("insert schema_migrations: %v", err)
	}
}

func TestNeedsMigration_NoTable(t *testing.T) {
	pool := setupTestPool(t)
	dropSchemaMigrations(t, pool)

	needs, err := NeedsMigration(context.Background(), pool)
	if err != nil {
		t.Fatalf("NeedsMigration: %v", err)
	}
	if !needs {
		t.Fatal("expected true — no schema_migrations table exists")
	}
}

func TestNeedsMigration_BehindLatest(t *testing.T) {
	pool := setupTestPool(t)
	dropSchemaMigrations(t, pool)
	insertSchemaMigrations(t, pool, 5, false)

	needs, err := NeedsMigration(context.Background(), pool)
	if err != nil {
		t.Fatalf("NeedsMigration: %v", err)
	}
	if !needs {
		t.Fatal("expected true — version 5 < latest 28")
	}
}

func TestNeedsMigration_UpToDate(t *testing.T) {
	pool := setupTestPool(t)
	dropSchemaMigrations(t, pool)
	insertSchemaMigrations(t, pool, 28, false)

	needs, err := NeedsMigration(context.Background(), pool)
	if err != nil {
		t.Fatalf("NeedsMigration: %v", err)
	}
	if needs {
		t.Fatal("expected false — version 28 == latest 28")
	}
}

func TestNeedsMigration_Dirty(t *testing.T) {
	pool := setupTestPool(t)
	dropSchemaMigrations(t, pool)
	insertSchemaMigrations(t, pool, 28, true)

	needs, err := NeedsMigration(context.Background(), pool)
	if err != nil {
		t.Fatalf("NeedsMigration: %v", err)
	}
	if !needs {
		t.Fatal("expected true — dirty flag set, regardless of version")
	}
}

// TestUp_CancelledContextAbortsRetryLoop verifies that passing a pre-cancelled (or
// very-short-deadline) context to Up causes the driver-creation retry loop to return
// promptly with the context error, rather than running all 10 attempts × 500 ms = 5 s.
func TestUp_CancelledContextAbortsRetryLoop(t *testing.T) {
	// We need a non-nil pool whose Config().ConnConfig is valid enough for
	// stdlib.OpenDB to succeed but whose resulting *sql.DB will make
	// postgres.WithInstance fail (because there's no real PostgreSQL listening).
	// pgxpool.ParseConfig gives us a valid *pgx.ConnConfig without dialing.
	cfg, err := pgxpool.ParseConfig("postgresql://nouser:nopass@127.0.0.1:1/nodb?connect_timeout=1")
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}

	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		// Some environments reject pool creation without a live server; skip rather than fail.
		t.Skipf("pgxpool.NewWithConfig: %v", err)
	}
	defer pool.Close()

	// Pre-cancel the context before calling Up.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err = Up(ctx, pool, true)
	elapsed := time.Since(start)

	// The function must return quickly (well under one full retry delay of 500 ms).
	const maxAllowed = 400 * time.Millisecond
	if elapsed > maxAllowed {
		t.Errorf("Up with cancelled ctx took %v; expected < %v (retry loop not aborting promptly)", elapsed, maxAllowed)
	}

	// It must return the context error, not some driver or migration error.
	if err == nil {
		t.Fatal("expected an error from Up with cancelled ctx, got nil")
	}
	if err != context.Canceled {
		// Allow wrapped errors too.
		if !strings.Contains(err.Error(), context.Canceled.Error()) {
			t.Errorf("expected context.Canceled (or wrapped), got: %v", err)
		}
	}
}
