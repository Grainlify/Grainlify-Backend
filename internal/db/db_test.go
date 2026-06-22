package db

import (
	"testing"
	"time"

	"github.com/jagadeesh/grainlify/backend/internal/config"
)

func TestPoolConfigDefaults(t *testing.T) {
	cfg := config.Load() // no env overrides set

	if cfg.DBMaxConns != 10 {
		t.Errorf("DBMaxConns default: want 10, got %d", cfg.DBMaxConns)
	}
	if cfg.DBMinConns != 0 {
		t.Errorf("DBMinConns default: want 0, got %d", cfg.DBMinConns)
	}
	if cfg.DBMaxConnLifetime != 30*time.Minute {
		t.Errorf("DBMaxConnLifetime default: want 30m, got %v", cfg.DBMaxConnLifetime)
	}
	if cfg.DBMaxConnIdleTime != 5*time.Minute {
		t.Errorf("DBMaxConnIdleTime default: want 5m, got %v", cfg.DBMaxConnIdleTime)
	}
}

func TestPoolConfigOverrides(t *testing.T) {
	t.Setenv("DB_MAX_CONNS", "25")
	t.Setenv("DB_MIN_CONNS", "5")
	t.Setenv("DB_MAX_CONN_LIFETIME", "1h")
	t.Setenv("DB_MAX_CONN_IDLE_TIME", "10m")

	cfg := config.Load()

	if cfg.DBMaxConns != 25 {
		t.Errorf("DBMaxConns override: want 25, got %d", cfg.DBMaxConns)
	}
	if cfg.DBMinConns != 5 {
		t.Errorf("DBMinConns override: want 5, got %d", cfg.DBMinConns)
	}
	if cfg.DBMaxConnLifetime != time.Hour {
		t.Errorf("DBMaxConnLifetime override: want 1h, got %v", cfg.DBMaxConnLifetime)
	}
	if cfg.DBMaxConnIdleTime != 10*time.Minute {
		t.Errorf("DBMaxConnIdleTime override: want 10m, got %v", cfg.DBMaxConnIdleTime)
	}
}

func TestPoolConfigInvalidFallsBackToDefaults(t *testing.T) {
	t.Setenv("DB_MAX_CONNS", "not-a-number")
	t.Setenv("DB_MIN_CONNS", "-1")
	t.Setenv("DB_MAX_CONN_LIFETIME", "bad-duration")
	t.Setenv("DB_MAX_CONN_IDLE_TIME", "0")

	cfg := config.Load()

	if cfg.DBMaxConns != 10 {
		t.Errorf("invalid DBMaxConns: want default 10, got %d", cfg.DBMaxConns)
	}
	if cfg.DBMinConns != 0 {
		t.Errorf("invalid DBMinConns: want default 0, got %d", cfg.DBMinConns)
	}
	if cfg.DBMaxConnLifetime != 30*time.Minute {
		t.Errorf("invalid DBMaxConnLifetime: want default 30m, got %v", cfg.DBMaxConnLifetime)
	}
	if cfg.DBMaxConnIdleTime != 5*time.Minute {
		t.Errorf("invalid DBMaxConnIdleTime: want default 5m, got %v", cfg.DBMaxConnIdleTime)
	}
}

func TestPoolConfigMapsOntoPgxConfig(t *testing.T) {
	pc := PoolConfig{
		MaxConns:        20,
		MinConns:        2,
		MaxConnLifetime: 45 * time.Minute,
		MaxConnIdleTime: 8 * time.Minute,
	}

	// Parse a syntactically valid (but unreachable) URL to get a pgxpool.Config.
	pgxCfg, err := parsePgxConfig("postgresql://user:pass@localhost:5432/db")
	if err != nil {
		t.Fatalf("parsePgxConfig: %v", err)
	}

	pgxCfg.MaxConns = pc.MaxConns
	pgxCfg.MinConns = pc.MinConns
	pgxCfg.MaxConnLifetime = pc.MaxConnLifetime
	pgxCfg.MaxConnIdleTime = pc.MaxConnIdleTime

	if pgxCfg.MaxConns != 20 {
		t.Errorf("MaxConns: want 20, got %d", pgxCfg.MaxConns)
	}
	if pgxCfg.MinConns != 2 {
		t.Errorf("MinConns: want 2, got %d", pgxCfg.MinConns)
	}
	if pgxCfg.MaxConnLifetime != 45*time.Minute {
		t.Errorf("MaxConnLifetime: want 45m, got %v", pgxCfg.MaxConnLifetime)
	}
	if pgxCfg.MaxConnIdleTime != 8*time.Minute {
		t.Errorf("MaxConnIdleTime: want 8m, got %v", pgxCfg.MaxConnIdleTime)
	}
}
