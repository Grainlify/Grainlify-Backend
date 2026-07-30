package handlers_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

func TestMeReadsLinkedGitHubProfileFromDatabase(t *testing.T) {
	userID := uuid.New()
	p := &mockDBPool{}
	p.queryRowFn = func(_ context.Context, sql string, _ ...any) pgx.Row {
		if strings.Contains(sql, "FROM users") {
			return mockRow{values: make([]any, 11)}
		}
		return mockRow{values: []any{"octocat", "https://example.test/avatar.png"}}
	}

	h := handlers.NewAuthHandler(config.Config{}, &db.DB{Pool: p})
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Get("/me", func(c *fiber.Ctx) error {
		c.Locals(auth.LocalUserID, userID.String())
		c.Locals(auth.LocalRole, "contributor")
		return h.Me()(c)
	})

	res, err := app.Test(httptest.NewRequest(http.MethodGet, "/me", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.StatusCode)
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), `"login":"octocat"`)
}
