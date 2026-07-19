package handlers

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/jagadeesh/grainlify/backend/internal/bus"
	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// healthStatus is a per-dependency readiness result.
type healthStatus struct {
	Name   string `json:"name"`
	Ready  bool   `json:"ready"`
	Status string `json:"status"`
}

// NewReady returns a handler that checks both database and (when configured) NATS
// connectivity. It returns 200 only when all configured dependencies are healthy;
// otherwise 503 with a per-dependency breakdown.
func NewReady(d *db.DB, b bus.Bus) fiber.Handler {
	return func(c *fiber.Ctx) error {
		statusCode := fiber.StatusOK
		var deps []healthStatus

		// Check database.
		dbStatus := healthStatus{Name: "database"}
		if d == nil || d.Pool == nil {
			dbStatus.Ready = false
			dbStatus.Status = "not_configured"
			statusCode = fiber.StatusServiceUnavailable
		} else {
			ctx, cancel := context.WithTimeout(c.Context(), 1*time.Second)
			defer cancel()

			if err := d.Ping(ctx); err != nil {
				dbStatus.Ready = false
				dbStatus.Status = "unreachable"
				statusCode = fiber.StatusServiceUnavailable
			} else {
				dbStatus.Ready = true
				dbStatus.Status = "ok"
			}
		}
		deps = append(deps, dbStatus)

		// Check NATS bus (only when configured).
		natsStatus := healthStatus{Name: "nats"}
		if b != nil {
			s := b.Status()
			if s == "CONNECTED" || s == "RECONNECTING" {
				natsStatus.Ready = true
				natsStatus.Status = s
			} else {
				natsStatus.Ready = false
				natsStatus.Status = s
				statusCode = fiber.StatusServiceUnavailable
			}
		} else {
			natsStatus.Ready = true
			natsStatus.Status = "not_configured"
		}
		deps = append(deps, natsStatus)

		return c.Status(statusCode).JSON(fiber.Map{
			"ok":   statusCode == fiber.StatusOK,
			"deps": deps,
		})
	}
}
