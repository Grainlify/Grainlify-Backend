package migrate

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestNonPooledConnConfig_StripsPoolerFromNeonHost(t *testing.T) {
	cfg, err := pgx.ParseConfig("postgres://user:pass@ep-purple-lake-a4yl80xp-pooler.us-east-1.aws.neon.tech:5432/grainlify?sslmode=require")
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}

	got := nonPooledConnConfig(cfg)

	want := "ep-purple-lake-a4yl80xp.us-east-1.aws.neon.tech"
	if got.Host != want {
		t.Errorf("Host = %q, want %q", got.Host, want)
	}
	// Everything else about the connection must be preserved.
	if got.Database != cfg.Database || got.User != cfg.User {
		t.Errorf("derived config changed database/user: got %+v, from %+v", got, cfg)
	}
}

func TestNonPooledConnConfig_LeavesNonPooledHostUnchanged(t *testing.T) {
	cfg, err := pgx.ParseConfig("postgres://user:pass@localhost:5432/grainlify_test?sslmode=disable")
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
	cfg, err := pgx.ParseConfig("postgres://user:pass@my-pooler-db.example.com:5432/app?sslmode=disable")
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}

	got := nonPooledConnConfig(cfg)

	if got.Host != "my-pooler-db.example.com" {
		t.Errorf("Host = %q, want unchanged %q", got.Host, "my-pooler-db.example.com")
	}
}
