package migrate

import (
	"context"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/jagadeesh/grainlify/backend/migrations"
)

func TestUp_BlocksIrreversibleOutsideDev(t *testing.T) {
	pool := setupTestPool(t)
	dropSchemaMigrations(t, pool)

	err := Up(context.Background(), pool, false)
	if err == nil {
		t.Fatal("expected error when irreversible migration is pending and allowIrreversible=false")
	}
	if !strings.Contains(err.Error(), "irreversible") {
		t.Fatalf("error should mention irreversible migration, got: %v", err)
	}
	if !strings.Contains(err.Error(), "7") {
		t.Fatalf("error should mention migration 7, got: %v", err)
	}
}

func TestUp_AllowsIrreversibleWithFlag(t *testing.T) {
	pool := setupTestPool(t)
	dropSchemaMigrations(t, pool)

	err := Up(context.Background(), pool, true)
	if err != nil {
		t.Fatalf("expected success when allowIrreversible=true, got: %v", err)
	}
}

func TestUp_NonIrreversibleRunsUnaffected(t *testing.T) {
	pool := setupTestPool(t)
	dropSchemaMigrations(t, pool)

	insertSchemaMigrations(t, pool, 7, false)

	err := Up(context.Background(), pool, false)
	if err != nil {
		t.Fatalf("expected non-irreversible migrations to run unaffected, got: %v", err)
	}
}

func TestUp_ReturnsErrorForNilPool(t *testing.T) {
	err := Up(context.Background(), nil, false)
	if err == nil {
		t.Fatal("expected error for nil pool")
	}
	if !strings.Contains(err.Error(), "nil") {
		t.Fatalf("error should mention nil pool, got: %v", err)
	}
}

func TestIrreversibleVersions_NonExistentMarker(t *testing.T) {
	versions, err := irreversibleVersions()
	if err != nil {
		t.Fatalf("irreversibleVersions: %v", err)
	}
	if versions[9999] {
		t.Error("expected version 9999 to not be in irreversible set")
	}
}

func TestCollectPendingVersions_AtLatest(t *testing.T) {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("iofs.New: %v", err)
	}
	pending, err := collectPendingVersions(src, 999, nil)
	if err != nil {
		t.Fatalf("collectPendingVersions: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected no pending versions, got %v", pending)
	}
}

func TestIsIrreversible_Zero(t *testing.T) {
	ok, err := isIrreversible(0)
	if err != nil {
		t.Fatalf("isIrreversible(0): %v", err)
	}
	if ok {
		t.Error("expected version 0 to not be irreversible")
	}
}
