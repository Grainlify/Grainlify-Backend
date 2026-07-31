package handlers_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

type leaderboardRowsErrPool struct {
	rows       pgx.Rows
	queryCalls int
}

func (p *leaderboardRowsErrPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("unexpected Exec")
}

func (p *leaderboardRowsErrPool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	p.queryCalls++
	return p.rows, nil
}

func (p *leaderboardRowsErrPool) QueryRow(context.Context, string, ...any) pgx.Row {
	return leaderboardErrorRow{}
}

func (p *leaderboardRowsErrPool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return nil, errors.New("unexpected BeginTx")
}

func (p *leaderboardRowsErrPool) Ping(context.Context) error { return nil }
func (p *leaderboardRowsErrPool) Close()                     {}
func (p *leaderboardRowsErrPool) Config() *pgxpool.Config    { return nil }

type leaderboardErrorRows struct{ err error }

func (r leaderboardErrorRows) Close()                                       {}
func (r leaderboardErrorRows) Err() error                                   { return r.err }
func (r leaderboardErrorRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r leaderboardErrorRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r leaderboardErrorRows) Next() bool                                   { return false }
func (r leaderboardErrorRows) Scan(...any) error                            { return errors.New("unexpected Scan") }
func (r leaderboardErrorRows) Values() ([]any, error)                       { return nil, errors.New("unexpected Values") }
func (r leaderboardErrorRows) RawValues() [][]byte                          { return nil }
func (r leaderboardErrorRows) Conn() *pgx.Conn                              { return nil }

type leaderboardErrorRow struct{}

func (leaderboardErrorRow) Scan(...any) error { return errors.New("unexpected QueryRow") }

func TestLeaderboard_ReturnsErrorWhenRowsIterationFails(t *testing.T) {
	pool := &leaderboardRowsErrPool{rows: leaderboardErrorRows{err: errors.New("stream interrupted")}}
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Get("/leaderboard", handlers.NewLeaderboardHandler(&db.DB{Pool: pool}).Leaderboard())

	resp, err := app.Test(httptest.NewRequest("GET", "/leaderboard?limit=10&offset=0", nil))
	require.NoError(t, err)
	require.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)
	require.Equal(t, 1, pool.queryCalls)
}
