package handlers

import (
	"context"
	"errors"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

const ServiceName = "grainlify-api"

// BuildInfo contains build-time metadata injected via -ldflags.
type BuildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
}

// NewHealth returns a handler that reports service health including
// build metadata, current uptime, and service name.
func NewHealth(build BuildInfo) fiber.Handler {
	return NewHealthWithDB(build, nil)
}

// NewHealthWithDB returns a health handler that also distinguishes database
// unavailability when a database dependency is provided.
func NewHealthWithDB(build BuildInfo, d *db.DB) fiber.Handler {
	startTime := time.Now()
	return func(c *fiber.Ctx) error {
		statusCode := fiber.StatusOK
		body := fiber.Map{
			"ok":         true,
			"service":    ServiceName,
			"version":    build.Version,
			"commit":     build.Commit,
			"build_time": build.BuildTime,
			"uptime":     time.Since(startTime).String(),
		}

		if d != nil && d.Pool != nil {
			ctx, cancel := context.WithTimeout(c.Context(), 1*time.Second)
			defer cancel()

			if err := d.Ping(ctx); err != nil {
				statusCode = fiber.StatusServiceUnavailable
				body["ok"] = false
				if errors.Is(err, db.ErrDBUnavailable) {
					body["database"] = "unavailable"
				} else {
					body["database"] = "error"
				}
			}
		}

		return c.Status(statusCode).JSON(body)
	}
}
