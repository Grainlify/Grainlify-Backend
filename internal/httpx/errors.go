// Package httpx provides shared HTTP helpers for Fiber handlers.
package httpx

import (
	"github.com/gofiber/fiber/v2"
)

// ErrorEnvelope is the standard JSON error response body.
//
// Every error response has the shape:
//
//	{
//	  "error": {
//	    "code":       "<machine_readable_code>",
//	    "message":    "<human_readable_message>",
//	    "request_id": "<value of X-Request-Id header>"
//	  }
//	}
//
// 5xx responses must use opaque codes/messages; raw internal errors must
// never be placed in "message" – log them instead.
type ErrorEnvelope struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail holds the fields nested under the "error" key.
type ErrorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// RespondError writes a consistent JSON error envelope to the Fiber context.
// It echoes the requestid local (set by the requestid middleware) into every
// error response so that support teams can correlate client-visible errors
// with server logs.
//
// Usage:
//
//	return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_json", "request body must be valid JSON")
func RespondError(c *fiber.Ctx, status int, code, message string) error {
	requestID := ""
	if rid, ok := c.Locals("requestid").(string); ok {
		requestID = rid
	}
	return c.Status(status).JSON(ErrorEnvelope{
		Error: ErrorDetail{
			Code:      code,
			Message:   message,
			RequestID: requestID,
		},
	})
}
