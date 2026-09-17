package migrate

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestNonPooledConnConfig_StripsPoolerFromNeonHost(t *testing.T) {
	// A deliberately FAKE host. The only part the code under test cares about is
	// Neon's "-pooler." separator, which nonPooledConnConfig finds and strips by
	// plain string replacement - nothing resolves the name - so everything else
	// is a placeholder on the reserved .invalid domain. Never put a real
	// endpoint here: this repository is public. No password either - the code
	// under test never reads it, and a user:password URI is exactly what secret
	// scanners look for.
	cfg, err := pgx.ParseConfig("postgres://user@ep-fake-endpoint-000000-pooler.region.example.invalid:5432/grainlify?sslmode=require")
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}

	got := nonPooledConnConfig(cfg)

	want := "ep-fake-endpoint-000000.region.example.invalid"
	if got.Host != want {
		t.Errorf("Host = %q, want %q", got.Host, want)
	}
	// Everything else about the connection must be preserved.
	if got.Database != cfg.Database || got.User != cfg.User {
		t.Errorf("derived config changed database/user: got %+v, from %+v", got, cfg)
	}
}

func TestNonPooledConnConfig_LeavesNonPooledHostUnchanged(t *testing.T) {
	cfg, err := pgx.ParseConfig("postgres://user@localhost:5432/grainlify_test?sslmode=disable")
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}

	got := nonPooledConnConfig(cfg)

	if got != cfg {
		t.Errorf("expected the exact same config back for a non-pooled host, got a different pointer/value")
	}
	if got.Host != "localhost" {
		t.Errorf("Host = %q, want %q", got.Host, "localhost")
	}
}

func TestNonPooledConnConfig_OnlyStripsPoolerHostSegment(t *testing.T) {
	// A hostname that merely contains "pooler" without the exact
	// "-pooler." separator (Neon's actual convention) must not be mangled.
	cfg, err := pgx.ParseConfig("postgres://user@my-pooler-db.example.com:5432/app?sslmode=disable")
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}

	got := nonPooledConnConfig(cfg)

	if got.Host != "my-pooler-db.example.com" {
		t.Errorf("Host = %q, want unchanged %q", got.Host, "my-pooler-db.example.com")
	}
}
