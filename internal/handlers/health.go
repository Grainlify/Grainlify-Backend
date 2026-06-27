package handlers

import (
	"time"

	"github.com/gofiber/fiber/v2"
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
	startTime := time.Now()
	return func(c *fiber.Ctx) error {
		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"ok":         true,
			"service":    ServiceName,
			"version":    build.Version,
			"commit":     build.Commit,
			"build_time": build.BuildTime,
			"uptime":     time.Since(startTime).String(),
		})
	}
}
