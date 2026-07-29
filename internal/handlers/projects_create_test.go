package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// Issue #317: Create() must not let a non-owner silently take over an
// existing project via the github_full_name ON CONFLICT upsert.

func newCreateProjectTestApp(h *handlers.ProjectsHandler) *fiber.App {
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Post("/projects", func(c *fiber.Ctx) error {
		c.Locals(auth.LocalUserID, c.Get("X-Test-User-ID"))
		return h.Create()(c)
	})
	return app
}

func seedActiveEcosystemForCreate(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	suffix := uuid.NewString()
	name := "Create Fixture " + suffix
	var id string
	require.NoError(t, pool.QueryRow(context.Background(),
		`INSERT INTO ecosystems (slug, name, status) VALUES ($1, $2, 'active') RETURNING id`,
		"create-fixture-"+suffix, name,
	).Scan(&id))
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM ecosystems WHERE id = $1`, id) })
	return name
}

func seedUserForCreate(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	require.NoError(t, pool.QueryRow(context.Background(),
		`INSERT INTO users (role) VALUES ('maintainer') RETURNING id`,
	).Scan(&id))
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id) })
	return id
}

func postCreateProject(t *testing.T, app *fiber.App, userID, fullName, ecosystemName string) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"github_full_name": fullName,
		"ecosystem_name":   ecosystemName,
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/projects", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User-ID", userID)
	resp, err := app.Test(req, -1)
	require.NoError(t, err)
	return resp
}

func TestCreate_NonOwnerCannotHijackExistingProject(t *testing.T) {
	pool := openTestPool(t)
	ecoName := seedActiveEcosystemForCreate(t, pool)
	owner := seedUserForCreate(t, pool)
	attacker := seedUserForCreate(t, pool)

	fullName := "acme/hijack-target-" + uuid.NewString()
	var projectID string
	require.NoError(t, pool.QueryRow(context.Background(),
		`INSERT INTO projects (owner_user_id, github_full_name, status) VALUES ($1, $2, 'verified') RETURNING id`,
		owner, fullName,
	).Scan(&projectID))
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, projectID) })

	h := handlers.NewProjectsHandler(config.Config{}, &db.DB{Pool: pool})
	app := newCreateProjectTestApp(h)

	resp := postCreateProject(t, app, attacker, fullName, ecoName)
	defer resp.Body.Close()

	require.Equal(t, fiber.StatusConflict, resp.StatusCode,
		"a different user's POST for an already-registered repo must be rejected")

	var envelope struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&envelope))
	require.Equal(t, "project_already_registered", envelope.Error)

	var ownerAfter, statusAfter string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT owner_user_id, status FROM projects WHERE id = $1`, projectID,
	).Scan(&ownerAfter, &statusAfter))
	require.Equal(t, owner, ownerAfter, "owner_user_id must be unchanged after a rejected hijack attempt")
	require.Equal(t, "verified", statusAfter, "status must be unchanged after a rejected hijack attempt")
}

func TestCreate_OriginalOwnerCanReSubmit(t *testing.T) {
	pool := openTestPool(t)
	ecoName := seedActiveEcosystemForCreate(t, pool)
	owner := seedUserForCreate(t, pool)

	fullName := "acme/resubmit-target-" + uuid.NewString()
	var projectID string
	require.NoError(t, pool.QueryRow(context.Background(),
		`INSERT INTO projects (owner_user_id, github_full_name, status) VALUES ($1, $2, 'verified') RETURNING id`,
		owner, fullName,
	).Scan(&projectID))
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, projectID) })

	h := handlers.NewProjectsHandler(config.Config{}, &db.DB{Pool: pool})
	app := newCreateProjectTestApp(h)

	resp := postCreateProject(t, app, owner, fullName, ecoName)
	defer resp.Body.Close()

	require.Equal(t, fiber.StatusCreated, resp.StatusCode,
		"the original owner re-submitting their own project must still succeed")
}
