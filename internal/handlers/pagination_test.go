package handlers_test

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePagination_Defaults(t *testing.T) {
	app := fiber.New()
	app.Get("/test", func(c *fiber.Ctx) error {
		p, err := handlers.ParsePagination(c, 20, 100)
		require.NoError(t, err)
		assert.Equal(t, 20, p.Limit)
		assert.Equal(t, 0, p.Offset)
		return c.SendString("ok")
	})

	req := httptest.NewRequest("GET", "/test", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
}

func TestParsePagination_ExplicitValues(t *testing.T) {
	app := fiber.New()
	app.Get("/test", func(c *fiber.Ctx) error {
		p, err := handlers.ParsePagination(c, 20, 100)
		require.NoError(t, err)
		assert.Equal(t, 30, p.Limit)
		assert.Equal(t, 10, p.Offset)
		return c.SendString("ok")
	})

	req := httptest.NewRequest("GET", "/test?limit=30&offset=10", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
}

func TestParsePagination_LimitClampedToMax(t *testing.T) {
	app := fiber.New()
	app.Get("/test", func(c *fiber.Ctx) error {
		p, err := handlers.ParsePagination(c, 20, 100)
		require.NoError(t, err)
		assert.Equal(t, 100, p.Limit)
		assert.Equal(t, 0, p.Offset)
		return c.SendString("ok")
	})

	req := httptest.NewRequest("GET", "/test?limit=999", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
}

func TestParsePagination_LimitDefaultsOnZero(t *testing.T) {
	app := fiber.New()
	app.Get("/test", func(c *fiber.Ctx) error {
		p, err := handlers.ParsePagination(c, 20, 100)
		require.NoError(t, err)
		assert.Equal(t, 20, p.Limit)
		assert.Equal(t, 0, p.Offset)
		return c.SendString("ok")
	})

	req := httptest.NewRequest("GET", "/test?limit=0", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
}

func TestParsePagination_LimitDefaultsOnNegative(t *testing.T) {
	app := fiber.New()
	app.Get("/test", func(c *fiber.Ctx) error {
		p, err := handlers.ParsePagination(c, 20, 100)
		require.NoError(t, err)
		assert.Equal(t, 20, p.Limit)
		assert.Equal(t, 0, p.Offset)
		return c.SendString("ok")
	})

	req := httptest.NewRequest("GET", "/test?limit=-5", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
}

func TestParsePagination_RejectsNegativeOffset(t *testing.T) {
	app := fiber.New()
	app.Get("/test", func(c *fiber.Ctx) error {
		_, err := handlers.ParsePagination(c, 20, 100)
		// ParsePagination writes the 400 response itself and returns a
		// non-nil sentinel so callers stop processing. The response is
		// already committed, so return nil to avoid overwriting it.
		if err != nil {
			return nil
		}
		return nil
	})

	req := httptest.NewRequest("GET", "/test?offset=-1", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 400, resp.StatusCode)
}

func TestParsePagination_LimitIsOne(t *testing.T) {
	app := fiber.New()
	app.Get("/test", func(c *fiber.Ctx) error {
		p, err := handlers.ParsePagination(c, 20, 100)
		require.NoError(t, err)
		assert.Equal(t, 1, p.Limit)
		assert.Equal(t, 0, p.Offset)
		return c.SendString("ok")
	})

	req := httptest.NewRequest("GET", "/test?limit=1", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
}

func TestParsePagination_LimitEqualsMax(t *testing.T) {
	app := fiber.New()
	app.Get("/test", func(c *fiber.Ctx) error {
		p, err := handlers.ParsePagination(c, 20, 100)
		require.NoError(t, err)
		assert.Equal(t, 100, p.Limit)
		assert.Equal(t, 0, p.Offset)
		return c.SendString("ok")
	})

	req := httptest.NewRequest("GET", "/test?limit=100", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
}

func TestParsePagination_CustomDefaults(t *testing.T) {
	app := fiber.New()
	app.Get("/test", func(c *fiber.Ctx) error {
		p, err := handlers.ParsePagination(c, 50, 200)
		require.NoError(t, err)
		assert.Equal(t, 50, p.Limit)
		assert.Equal(t, 0, p.Offset)
		return c.SendString("ok")
	})

	req := httptest.NewRequest("GET", "/test", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
}

func TestPaginatedResponse_Shape(t *testing.T) {
	items := []string{"a", "b", "c"}
	p := handlers.PaginationParams{Limit: 10, Offset: 0}
	resp := handlers.PaginatedResponse("items", items, p, 100)

	assert.Equal(t, items, resp["items"])
	assert.Equal(t, 10, resp["limit"])
	assert.Equal(t, 0, resp["offset"])
	assert.Equal(t, 100, resp["total"])
	assert.True(t, resp["has_more"].(bool))
}

func TestPaginatedResponse_NilItems(t *testing.T) {
	p := handlers.PaginationParams{Limit: 20, Offset: 5}
	resp := handlers.PaginatedResponse("leaderboard", nil, p, 0)

	assert.NotNil(t, resp["leaderboard"])
	assert.Empty(t, resp["leaderboard"])
	assert.Equal(t, 20, resp["limit"])
	assert.Equal(t, 5, resp["offset"])
	assert.Equal(t, 0, resp["total"])
	assert.False(t, resp["has_more"].(bool))
}

func TestPaginatedResponse_CustomItemsKey(t *testing.T) {
	items := []int{1, 2}
	p := handlers.PaginationParams{Limit: 5, Offset: 1}
	resp := handlers.PaginatedResponse("projects", items, p, 42)

	assert.Equal(t, items, resp["projects"])
	assert.Equal(t, 5, resp["limit"])
	assert.Equal(t, 1, resp["offset"])
	assert.Equal(t, 42, resp["total"])
	assert.NotContains(t, resp, "items") // custom key, not "items"
	assert.True(t, resp["has_more"].(bool))
}

func TestPaginatedResponse_RoundTrip(t *testing.T) {
	// Simulate the full cycle: parse pagination params, fetch, build response
	app := fiber.New()
	app.Get("/test", func(c *fiber.Ctx) error {
		p, err := handlers.ParsePagination(c, 20, 100)
		require.NoError(t, err)

		items := []fiber.Map{
			{"id": 1, "name": "foo"},
			{"id": 2, "name": "bar"},
		}
		return c.JSON(handlers.PaginatedResponse("items", items, p, 42))
	})

	req := httptest.NewRequest("GET", "/test?limit=10&offset=5", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
}

func TestParsePagination_ZeroOffset(t *testing.T) {
	app := fiber.New()
	app.Get("/test", func(c *fiber.Ctx) error {
		p, err := handlers.ParsePagination(c, 20, 100)
		require.NoError(t, err)
		assert.Equal(t, 20, p.Limit)
		assert.Equal(t, 0, p.Offset)
		return c.SendString("ok")
	})

	req := httptest.NewRequest("GET", "/test?offset=0", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
}

func TestParsePagination_LargeOffset(t *testing.T) {
	app := fiber.New()
	app.Get("/test", func(c *fiber.Ctx) error {
		p, err := handlers.ParsePagination(c, 20, 100)
		require.NoError(t, err)
		assert.Equal(t, 20, p.Limit)
		assert.Equal(t, 1000, p.Offset)
		return c.SendString("ok")
	})

	req := httptest.NewRequest("GET", "/test?limit=20&offset=1000", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
}
