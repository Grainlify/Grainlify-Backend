package handlers_test

import (
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/assert"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// newSyncJobsApp wires JobsForProject behind a fiber app using a DB with no
// pool. Pagination parameters are validated before the DB availability check,
// so valid params fall through to the db check (503) while out-of-range params
// are rejected up front (400) without requiring a live database.
func newSyncJobsApp() *fiber.App {
	h := handlers.NewSyncHandler(&db.DB{})
	app := fiber.New()
	app.Get("/projects/:id/sync/jobs", h.JobsForProject())
	return app
}

const syncTestProjectPath = "/projects/11111111-1111-1111-1111-111111111111/sync/jobs"

func TestSyncJobsForProject_DefaultParamsAccepted(t *testing.T) {
	app := newSyncJobsApp()
	status, errMsg := decodeError(t, app, syncTestProjectPath)
	assert.Equal(t, fiber.StatusServiceUnavailable, status)
	assert.Equal(t, "db_not_configured", errMsg)
}

func TestSyncJobsForProject_BoundedParamsAccepted(t *testing.T) {
	app := newSyncJobsApp()
	status, errMsg := decodeError(t, app, syncTestProjectPath+"?limit=10&offset=5")
	assert.Equal(t, fiber.StatusServiceUnavailable, status)
	assert.Equal(t, "db_not_configured", errMsg)
}

func TestSyncJobsForProject_LimitAboveMaxAccepted(t *testing.T) {
	app := newSyncJobsApp()
	status, errMsg := decodeError(t, app, syncTestProjectPath+"?limit=10000")
	assert.Equal(t, fiber.StatusServiceUnavailable, status)
	assert.Equal(t, "db_not_configured", errMsg)
}

func TestSyncJobsForProject_ZeroLimitAccepted(t *testing.T) {
	app := newSyncJobsApp()
	status, errMsg := decodeError(t, app, syncTestProjectPath+"?limit=0")
	assert.Equal(t, fiber.StatusServiceUnavailable, status)
	assert.Equal(t, "db_not_configured", errMsg)
}

func TestSyncJobsForProject_NegativeOffsetRejected(t *testing.T) {
	app := newSyncJobsApp()
	status, errMsg := decodeError(t, app, syncTestProjectPath+"?offset=-3")
	assert.Equal(t, fiber.StatusBadRequest, status)
	assert.Equal(t, "offset must be non-negative", errMsg)
}
