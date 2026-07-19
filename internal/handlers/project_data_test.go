package handlers

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
)

type projectDataTestPool struct {
	queryRowErr error
}

func (p projectDataTestPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (p projectDataTestPool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, nil
}

func (p projectDataTestPool) QueryRow(context.Context, string, ...any) pgx.Row {
	return projectDataTestRow{err: p.queryRowErr}
}

func (p projectDataTestPool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return nil, nil
}

func (p projectDataTestPool) Ping(context.Context) error { return nil }
func (p projectDataTestPool) Close()                     {}
func (p projectDataTestPool) Config() *pgxpool.Config    { return nil }

type projectDataTestRow struct {
	err error
}

func (r projectDataTestRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) > 0 {
		if exists, ok := dest[0].(*bool); ok {
			*exists = false
		}
	}
	return nil
}

func TestProjectDataIssuesUnknownProjectReturnsNotFound(t *testing.T) {
	h := NewProjectDataHandler(&db.DB{Pool: projectDataTestPool{}})
	projectID := uuid.New()

	app := fiber.New()
	app.Get("/projects/:id/issues", func(c *fiber.Ctx) error {
		c.Locals(auth.LocalUserID, uuid.New().String())
		return h.Issues()(c)
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/projects/"+projectID.String()+"/issues", nil))
	if err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusNotFound {
		t.Fatalf("expected status %d, got %d", fiber.StatusNotFound, resp.StatusCode)
	}

	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}
	if !strings.EqualFold(body.Error, "project_not_found") {
		t.Fatalf("expected project_not_found error code, got %q", body.Error)
	}
}
