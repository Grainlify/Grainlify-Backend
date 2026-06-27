package migrate

import (
	"context"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

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

	want := uint(30)
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
