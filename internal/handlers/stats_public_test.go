package handlers

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jagadeesh/grainlify/backend/internal/db"
)

type landingStatsMockPool struct {
	queries atomic.Int64
}

func (p *landingStatsMockPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (p *landingStatsMockPool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, nil
}

func (p *landingStatsMockPool) QueryRow(context.Context, string, ...any) pgx.Row {
	p.queries.Add(1)
	return landingStatsMockRow{activeProjects: 7, contributors: 11}
}

func (p *landingStatsMockPool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return nil, nil
}

func (p *landingStatsMockPool) Ping(context.Context) error { return nil }
func (p *landingStatsMockPool) Close()                     {}
func (p *landingStatsMockPool) Config() *pgxpool.Config    { return nil }

type landingStatsMockRow struct {
	activeProjects int64
	contributors   int64
}

func (r landingStatsMockRow) Scan(dest ...any) error {
	*(dest[0].(*int64)) = r.activeProjects
	*(dest[1].(*int64)) = r.contributors
	return nil
}

func TestLandingStatsCacheAvoidsDBWithinTTL(t *testing.T) {
	pool := &landingStatsMockPool{}
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	h := newLandingStatsHandler(&db.DB{Pool: pool}, time.Minute, func() time.Time { return now })

	app := fiber.New()
	app.Get("/stats/landing", h.Get())

	for i := 0; i < 2; i++ {
		res, err := app.Test(httptest.NewRequest("GET", "/stats/landing", nil))
		if err != nil {
			t.Fatalf("request %d failed: %v", i+1, err)
		}
		if res.StatusCode != 200 {
			t.Fatalf("request %d status = %d, want 200", i+1, res.StatusCode)
		}

		var body LandingStatsResponse
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			t.Fatalf("decode response %d: %v", i+1, err)
		}
		if body.ActiveProjects != 7 || body.Contributors != 11 || body.GrantsDistributedUSD != 0 {
			t.Fatalf("unexpected response %d: %+v", i+1, body)
		}
	}

	if got := pool.queries.Load(); got != 1 {
		t.Fatalf("query count = %d, want 1", got)
	}
}

func TestLandingStatsCacheRefreshesAfterTTL(t *testing.T) {
	pool := &landingStatsMockPool{}
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	h := newLandingStatsHandler(&db.DB{Pool: pool}, time.Minute, func() time.Time { return now })

	app := fiber.New()
	app.Get("/stats/landing", h.Get())

	if _, err := app.Test(httptest.NewRequest("GET", "/stats/landing", nil)); err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	now = now.Add(time.Minute)
	if _, err := app.Test(httptest.NewRequest("GET", "/stats/landing", nil)); err != nil {
		t.Fatalf("second request failed: %v", err)
	}

	if got := pool.queries.Load(); got != 2 {
		t.Fatalf("query count = %d, want 2", got)
	}
}
