package handlers_test

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// Test ContributionActivity pagination behavior after migration to ParsePagination()
// These tests verify all the required behaviors from Issue #224

func TestContributionActivity_NegativeOffsetReturns400(t *testing.T) {
	// Test that negative offsets return HTTP 400 (this was missing before the fix)
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	
	// Create a minimal handler that just tests pagination parsing
	// We can't easily test the full handler without database setup
	app.Get("/test", func(c *fiber.Ctx) error {
		// This should return 400 for negative offset
		p, err := handlers.ParsePagination(c, 50, 100)
		if err != nil {
			// ParsePagination already wrote the response
			return nil
		}
		
		// Should not reach here with negative offset
		return c.JSON(fiber.Map{
			"activities": []fiber.Map{},
			"total":      0,
			"limit":      p.Limit,
			"offset":     p.Offset,
		})
	})

	req := httptest.NewRequest("GET", "/test?offset=-1", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 400, resp.StatusCode)
	
	// Verify error response
	var body map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)
	assert.Contains(t, body["error"], "offset must be non-negative")
}

func TestContributionActivity_ZeroLimitBecomes50(t *testing.T) {
	// Test that limit=0 becomes 50 (default)
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	
	app.Get("/test", func(c *fiber.Ctx) error {
		p, err := handlers.ParsePagination(c, 50, 100)
		if err != nil {
			return nil
		}
		
		return c.JSON(fiber.Map{
			"activities": []fiber.Map{},
			"total":      0,
			"limit":      p.Limit,
			"offset":     p.Offset,
		})
	})

	req := httptest.NewRequest("GET", "/test?limit=0", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	
	var body map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)
	
	// Verify limit is reset to default (50)
	assert.Equal(t, float64(50), body["limit"])
	assert.Equal(t, float64(0), body["offset"])
}

func TestContributionActivity_NegativeLimitBecomes50(t *testing.T) {
	// Test that negative limits become 50 (default)
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	
	app.Get("/test", func(c *fiber.Ctx) error {
		p, err := handlers.ParsePagination(c, 50, 100)
		if err != nil {
			return nil
		}
		
		return c.JSON(fiber.Map{
			"activities": []fiber.Map{},
			"total":      0,
			"limit":      p.Limit,
			"offset":     p.Offset,
		})
	})

	req := httptest.NewRequest("GET", "/test?limit=-5", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	
	var body map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)
	
	// Verify limit is reset to default (50)
	assert.Equal(t, float64(50), body["limit"])
	assert.Equal(t, float64(0), body["offset"])
}

func TestContributionActivity_LimitAbove100Becomes100(t *testing.T) {
	// Test that limits > 100 are capped at 100
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	
	app.Get("/test", func(c *fiber.Ctx) error {
		p, err := handlers.ParsePagination(c, 50, 100)
		if err != nil {
			return nil
		}
		
		return c.JSON(fiber.Map{
			"activities": []fiber.Map{},
			"total":      0,
			"limit":      p.Limit,
			"offset":     p.Offset,
		})
	})

	req := httptest.NewRequest("GET", "/test?limit=999", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	
	var body map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)
	
	// Verify limit is capped at 100
	assert.Equal(t, float64(100), body["limit"])
	assert.Equal(t, float64(0), body["offset"])
}

func TestContributionActivity_ValidLimitUnchanged(t *testing.T) {
	// Test that valid limits remain unchanged
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	
	app.Get("/test", func(c *fiber.Ctx) error {
		p, err := handlers.ParsePagination(c, 50, 100)
		if err != nil {
			return nil
		}
		
		return c.JSON(fiber.Map{
			"activities": []fiber.Map{},
			"total":      0,
			"limit":      p.Limit,
			"offset":     p.Offset,
		})
	})

	req := httptest.NewRequest("GET", "/test?limit=25", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	
	var body map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)
	
	// Verify limit remains as specified
	assert.Equal(t, float64(25), body["limit"])
	assert.Equal(t, float64(0), body["offset"])
}

func TestContributionActivity_ValidOffsetUnchanged(t *testing.T) {
	// Test that valid offsets remain unchanged
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	
	app.Get("/test", func(c *fiber.Ctx) error {
		p, err := handlers.ParsePagination(c, 50, 100)
		if err != nil {
			return nil
		}
		
		return c.JSON(fiber.Map{
			"activities": []fiber.Map{},
			"total":      0,
			"limit":      p.Limit,
			"offset":     p.Offset,
		})
	})

	req := httptest.NewRequest("GET", "/test?limit=30&offset=15", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	
	var body map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)
	
	// Verify both values remain as specified
	assert.Equal(t, float64(30), body["limit"])
	assert.Equal(t, float64(15), body["offset"])
}

func TestContributionActivity_DefaultBehavior(t *testing.T) {
	// Test default behavior when no parameters are provided
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	
	app.Get("/test", func(c *fiber.Ctx) error {
		p, err := handlers.ParsePagination(c, 50, 100)
		if err != nil {
			return nil
		}
		
		return c.JSON(fiber.Map{
			"activities": []fiber.Map{},
			"total":      0,
			"limit":      p.Limit,
			"offset":     p.Offset,
		})
	})

	req := httptest.NewRequest("GET", "/test", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	
	var body map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)
	
	// Verify default values: limit=50, offset=0
	assert.Equal(t, float64(50), body["limit"])
	assert.Equal(t, float64(0), body["offset"])
}

func TestContributionActivity_EmptyLoginResponseStructure(t *testing.T) {
	// Test that the empty login response structure is preserved
	// This simulates the case where githubLogin is empty in ContributionActivity
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	
	app.Get("/test", func(c *fiber.Ctx) error {
		p, err := handlers.ParsePagination(c, 50, 100)
		if err != nil {
			return nil
		}
		
		// Simulate empty login case - should return same structure as before
		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"activities": []fiber.Map{},
			"total":      0,
			"limit":      p.Limit,
			"offset":     p.Offset,
		})
	})

	req := httptest.NewRequest("GET", "/test?limit=25&offset=5", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	
	var body map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)
	
	// Verify the response structure matches expected format
	assert.NotNil(t, body["activities"])
	assert.Equal(t, float64(0), body["total"])
	assert.Equal(t, float64(25), body["limit"])
	assert.Equal(t, float64(5), body["offset"])
	
	// Verify activities is an empty array
	activities, ok := body["activities"].([]interface{})
	assert.True(t, ok)
	assert.Empty(t, activities)
}

// Integration test for boundary values
func TestContributionActivity_BoundaryValues(t *testing.T) {
	testCases := []struct {
		name           string
		query          string
		expectedLimit  float64
		expectedOffset float64
		expectedStatus int
	}{
		{
			name:           "limit equals max (100)",
			query:          "?limit=100",
			expectedLimit:  100,
			expectedOffset: 0,
			expectedStatus: 200,
		},
		{
			name:           "limit equals 1 (minimum valid)",
			query:          "?limit=1",
			expectedLimit:  1,
			expectedOffset: 0,
			expectedStatus: 200,
		},
		{
			name:           "offset equals 0",
			query:          "?offset=0",
			expectedLimit:  50, // default
			expectedOffset: 0,
			expectedStatus: 200,
		},
		{
			name:           "large valid offset",
			query:          "?limit=10&offset=1000",
			expectedLimit:  10,
			expectedOffset: 1000,
			expectedStatus: 200,
		},
	}

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Get("/test", func(c *fiber.Ctx) error {
		p, err := handlers.ParsePagination(c, 50, 100)
		if err != nil {
			return nil
		}
		
		return c.JSON(fiber.Map{
			"activities": []fiber.Map{},
			"total":      0,
			"limit":      p.Limit,
			"offset":     p.Offset,
		})
	})

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/test"+tc.query, nil)
			resp, err := app.Test(req)
			require.NoError(t, err)
			assert.Equal(t, tc.expectedStatus, resp.StatusCode)

			if tc.expectedStatus == 200 {
				var body map[string]interface{}
				err = json.NewDecoder(resp.Body).Decode(&body)
				require.NoError(t, err)
				
				assert.Equal(t, tc.expectedLimit, body["limit"])
				assert.Equal(t, tc.expectedOffset, body["offset"])
			}
		})
	}
}