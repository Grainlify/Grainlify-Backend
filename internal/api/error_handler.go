package api

import (
	"errors"
	"log/slog"
	"runtime/debug"
	"strings"

	"github.com/gofiber/fiber/v2"
)

// ErrorEnvelope is the standard JSON error response shape used across the API.
type ErrorEnvelope struct {
	Error     string `json:"error"`
	Message   string `json:"message,omitempty"`
	RequestID string `json:"request_id"`
}

// JSONErrorHandler returns a Fiber ErrorHandler that maps handler errors and
// recovered panics into the standard JSON error envelope. Server-side details
// for 5xx responses are logged only and never returned to clients.
func JSONErrorHandler() fiber.ErrorHandler {
	return func(c *fiber.Ctx, err error) error {
		status, code, message, logErr := resolveAPIError(err)
		if logErr != nil {
			slog.Error("request failed",
				"request_id", requestIDFromCtx(c),
				"method", c.Method(),
				"path", c.Path(),
				"status", status,
				"error", logErr.Error(),
			)
		}
		return WriteErrorEnvelope(c, status, code, message)
	}
}

// WriteErrorEnvelope writes a JSON error response with the standard envelope.
// Optional extra fields (e.g. path for 404) are merged into the response body.
func WriteErrorEnvelope(c *fiber.Ctx, status int, code, message string, extra ...fiber.Map) error {
	body := fiber.Map{
		"error":      code,
		"request_id": requestIDFromCtx(c),
	}
	if message != "" {
		body["message"] = message
	}
	for _, fields := range extra {
		for key, value := range fields {
			body[key] = value
		}
	}
	return c.Status(status).JSON(body)
}

func requestIDFromCtx(c *fiber.Ctx) string {
	if id, ok := c.Locals("requestid").(string); ok {
		return id
	}
	return ""
}

func resolveAPIError(err error) (status int, code, message string, logErr error) {
	var fiberErr *fiber.Error
	if errors.As(err, &fiberErr) {
		status = fiberErr.Code
		if status >= fiber.StatusInternalServerError {
			return status, "internal_server_error", opaqueServerMessage, err
		}
		code = errorCodeFromFiberMessage(fiberErr.Message)
		if code == "" {
			code = defaultErrorCodeForStatus(status)
		}
		return status, code, fiberErr.Message, nil
	}

	return fiber.StatusInternalServerError, "internal_server_error", opaqueServerMessage, err
}

const opaqueServerMessage = "An unexpected error occurred"

func errorCodeFromFiberMessage(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}
	for _, r := range msg {
		if (r >= 'a' && r <= 'z') || r == '_' {
			continue
		}
		return ""
	}
	return msg
}

func defaultErrorCodeForStatus(status int) string {
	switch status {
	case fiber.StatusBadRequest:
		return "bad_request"
	case fiber.StatusUnauthorized:
		return "unauthorized"
	case fiber.StatusForbidden:
		return "forbidden"
	case fiber.StatusNotFound:
		return "not_found"
	case fiber.StatusMethodNotAllowed:
		return "method_not_allowed"
	case fiber.StatusConflict:
		return "conflict"
	case fiber.StatusUnprocessableEntity:
		return "unprocessable_entity"
	case fiber.StatusTooManyRequests:
		return "too_many_requests"
	case fiber.StatusRequestEntityTooLarge:
		return "request_entity_too_large"
	default:
		return "request_failed"
	}
}

// PanicStackTraceHandler logs panic stack traces server-side at error level.
// Panic details are never included in client responses.
func PanicStackTraceHandler(c *fiber.Ctx, e interface{}) {
	slog.Error("panic recovered",
		"request_id", requestIDFromCtx(c),
		"method", c.Method(),
		"path", c.Path(),
		"panic", e,
		"stack", string(debug.Stack()),
	)
}
