package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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

func TestConnectWrapsPingFailureAsDBUnavailable(t *testing.T) {
	dsn := "postgresql://secret_user:secret_password@127.0.0.1:1/grainlify?sslmode=disable"
	_, err := Connect(context.Background(), dsn, PoolConfig{MaxConns: 1})
	if err == nil {
		t.Fatal("Connect returned nil error, want database unavailable error")
	}
	if !errors.Is(err, ErrDBUnavailable) {
		t.Fatalf("errors.Is(err, ErrDBUnavailable) = false; err = %v", err)
	}

	var unavailable *DBUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("errors.As(err, *DBUnavailableError) = false; err = %T", err)
	}
	if unavailable.Op == "" {
		t.Fatal("DBUnavailableError.Op is empty")
	}
	if strings.Contains(err.Error(), "secret_password") || strings.Contains(err.Error(), dsn) {
		t.Fatalf("database unavailable error leaked DSN credentials: %q", err.Error())
	}
}

type mockTx struct {
	pgx.Tx
	commitCalled   bool
	rollbackCalled bool
	commitErr      error
	rollbackErr    error
}

func (m *mockTx) Commit(ctx context.Context) error {
	m.commitCalled = true
	return m.commitErr
}

func (m *mockTx) Rollback(ctx context.Context) error {
	m.rollbackCalled = true
	return m.rollbackErr
}

type mockPool struct {
	DBPool
	tx       *mockTx
	beginErr error
	pingErr  error
	closed   bool
}

func (m *mockPool) BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error) {
	if m.beginErr != nil {
		return nil, m.beginErr
	}
	return m.tx, nil
}

func (m *mockPool) Ping(ctx context.Context) error {
	return m.pingErr
}

func (m *mockPool) Close() {
	m.closed = true
}

func TestWithTx_CommitOnSuccess(t *testing.T) {
	tx := &mockTx{}
	pool := &mockPool{tx: tx}
	err := WithTx(context.Background(), pool, func(t pgx.Tx) error {
		return nil
	})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !tx.commitCalled {
		t.Error("expected Commit to be called")
	}
	if tx.rollbackCalled {
		t.Error("expected Rollback NOT to be called")
	}
}

func TestWithTx_RollbackOnError(t *testing.T) {
	tx := &mockTx{}
	pool := &mockPool{tx: tx}
	expectedErr := errors.New("something went wrong")
	err := WithTx(context.Background(), pool, func(t pgx.Tx) error {
		return expectedErr
	})
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected %v, got %v", expectedErr, err)
	}
	if tx.commitCalled {
		t.Error("expected Commit NOT to be called")
	}
	if !tx.rollbackCalled {
		t.Error("expected Rollback to be called")
	}
}

func TestWithTx_PanicTriggersRollback(t *testing.T) {
	tx := &mockTx{}
	pool := &mockPool{tx: tx}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic to be re-raised")
		}
		if r != "boom" {
			t.Fatalf("expected panic value 'boom', got %v", r)
		}
		if tx.commitCalled {
			t.Error("expected Commit NOT to be called on panic")
		}
		if !tx.rollbackCalled {
			t.Error("expected Rollback to be called on panic")
		}
	}()

	_ = WithTx(context.Background(), pool, func(t pgx.Tx) error {
		panic("boom")
	})
}

func TestWithTx_EdgeCases(t *testing.T) {
	ctx := context.Background()

	// 1. Nil pool / Nil DB
	if err := WithTx(ctx, nil, func(tx pgx.Tx) error { return nil }); err == nil || err.Error() != "db not configured" {
		t.Errorf("expected 'db not configured' for nil pool, got %v", err)
	}
	var nilDB *DB
	if err := nilDB.WithTx(ctx, func(tx pgx.Tx) error { return nil }); err == nil || err.Error() != "db not configured" {
		t.Errorf("expected 'db not configured' for nil DB, got %v", err)
	}

	// 2. Nil function
	tx := &mockTx{}
	pool := &mockPool{tx: tx}
	if err := WithTx(ctx, pool, nil); err == nil || err.Error() != "transaction function cannot be nil" {
		t.Errorf("expected 'transaction function cannot be nil', got %v", err)
	}

	// 3. BeginTx returns error
	poolBeginErr := &mockPool{beginErr: errors.New("cannot begin")}
	if err := WithTx(ctx, poolBeginErr, func(tx pgx.Tx) error { return nil }); err == nil || !strings.Contains(err.Error(), "begin transaction") {
		t.Errorf("expected 'begin transaction' error, got %v", err)
	}

	// 4. Commit returns error
	txCommitErr := &mockTx{commitErr: errors.New("commit conflict")}
	poolCommit := &mockPool{tx: txCommitErr}
	if err := WithTx(ctx, poolCommit, func(tx pgx.Tx) error { return nil }); err == nil || err.Error() != "commit conflict" {
		t.Errorf("expected 'commit conflict' error, got %v", err)
	}
	if !txCommitErr.commitCalled {
		t.Error("expected Commit to have been called")
	}

	// 5. DB method wrapper WithTx
	d := &DB{Pool: pool}
	if err := d.WithTx(ctx, func(tx pgx.Tx) error { return nil }); err != nil {
		t.Errorf("expected success with DB.WithTx, got %v", err)
	}
}

func TestDB_MethodsAndHelpers(t *testing.T) {
	ctx := context.Background()

	// Ping and Close on nil or unconfigured DB
	var nilDB *DB
	if err := nilDB.Ping(ctx); err == nil || err.Error() != "db not configured" {
		t.Errorf("expected 'db not configured' for nilDB.Ping, got %v", err)
	}
	nilDB.Close() // should not panic

	emptyDB := &DB{}
	if err := emptyDB.Ping(ctx); err == nil || err.Error() != "db not configured" {
		t.Errorf("expected 'db not configured' for emptyDB.Ping, got %v", err)
	}
	emptyDB.Close() // should not panic

	// Error formatting and unwrapping
	var nilErr *DBUnavailableError
	if nilErr.Error() != ErrDBUnavailable.Error() {
		t.Errorf("nil DBUnavailableError.Error() = %q", nilErr.Error())
	}
	if nilErr.Unwrap() != nil {
		t.Errorf("nil DBUnavailableError.Unwrap() should be nil")
	}

	errNoOp := &DBUnavailableError{Err: errors.New("sub")}
	if errNoOp.Error() != ErrDBUnavailable.Error() {
		t.Errorf("empty Op Error() = %q", errNoOp.Error())
	}
	if errNoOp.Unwrap() == nil || errNoOp.Unwrap().Error() != "sub" {
		t.Errorf("Unwrap() = %v", errNoOp.Unwrap())
	}

	// maskDBURL edge cases
	if m := maskDBURL("short"); m != "***" {
		t.Errorf("short maskDBURL = %q, want '***'", m)
	}
	if m := maskDBURL("postgresql://no-colon-or-at-symbol-here"); m != "***" {
		t.Errorf("no-at maskDBURL = %q, want '***'", m)
	}

	// Active pool Ping and Close
	mp := &mockPool{}
	validDB := &DB{Pool: mp}
	if err := validDB.Ping(ctx); err != nil {
		t.Errorf("validDB.Ping() error = %v, want nil", err)
	}
	mp.pingErr = errors.New("ping failed")
	if err := validDB.Ping(ctx); err == nil || !strings.Contains(err.Error(), "ping db") {
		t.Errorf("validDB.Ping() with error = %v, want ping db error", err)
	}
	validDB.Close()
	if !mp.closed {
		t.Errorf("expected mockPool to be closed after validDB.Close()")
	}

	// Connect error paths
	if _, err := Connect(ctx, "", PoolConfig{}); err == nil || err.Error() != "DB_URL is required" {
		t.Errorf("expected DB_URL is required, got %v", err)
	}
	if _, err := Connect(ctx, "://invalid-url", PoolConfig{}); err == nil || !strings.Contains(err.Error(), "parse DB_URL") {
		t.Errorf("expected parse DB_URL error, got %v", err)
	}
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Connect(canceledCtx, "postgresql://user:pass@127.0.0.1:5432/grainlify?sslmode=disable", PoolConfig{}); err == nil || !strings.Contains(err.Error(), "connect db") {
		t.Errorf("expected connect db error on canceled context, got %v", err)
	}
}
