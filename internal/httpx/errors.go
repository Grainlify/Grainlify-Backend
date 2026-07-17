// Package httpx provides shared HTTP helpers for Fiber handlers.
package httpx

import "github.com/gofiber/fiber/v2"

// Code is a stable, machine-readable error code returned to API clients in
// the "error" field of every error response. Codes are part of the public
// API contract described in docs/reference/api-endpoints.md: existing
// values must never be renamed or repurposed, and codes must never leak
// internal implementation detail (SQL errors, stack traces, file paths,
// etc). Handlers may still define specific codes (e.g. "ecosystem_not_found")
// as plain string constants -- they convert to Code automatically -- but
// every 5xx response is always forced to CodeInternal regardless of what is
// passed in, so failure detail never reaches the client.
type Code string

const (
CodeBadRequest         Code = "bad_request"
CodeInvalidJSON        Code = "invalid_json"
CodeValidationFailed   Code = "validation_failed"
CodeUnauthorized       Code = "unauthorized"
CodeForbidden          Code = "forbidden"
CodeNotFound           Code = "not_found"
CodeMethodNotAllowed   Code = "method_not_allowed"
CodeConflict           Code = "conflict"
CodeUnprocessable      Code = "unprocessable_entity"
CodeTooManyRequests    Code = "too_many_requests"
CodeRequestTooLarge    Code = "request_entity_too_large"
CodeServiceUnavailable Code = "service_unavailable"
CodeInternal           Code = "internal_server_error"
)

// genericInternalMessage is the only message ever sent to a client on a 5xx
// response. Real failure detail belongs in server-side logs, never here.
const genericInternalMessage = "An unexpected error occurred"

// ErrorEnvelope is the standard JSON error response body returned by every
// endpoint in the API:
//
//{
//  "error": "<machine_readable_code>",
//  "message": "<human_readable_message>",
//  "request_id": "<value of X-Request-Id header>"
//}
//
// This flat shape matches docs/reference/api-endpoints.md and internal/api's
// ErrorEnvelope. Do not introduce a second, differently-shaped envelope.
type ErrorEnvelope struct {
Error     string `json:"error"`
Message   string `json:"message,omitempty"`
RequestID string `json:"request_id"`
}

// RespondError writes a consistent JSON error envelope to the Fiber context.
// It echoes the requestid local (set by the requestid middleware) into every
// error response so support teams can correlate client-visible errors with
// server logs.
//
// Every 5xx status is automatically forced to the opaque CodeInternal /
// genericInternalMessage pair -- the code and message arguments passed in
// are ignored in that case -- so a handler can never accidentally leak a raw
// error (SQL error, stack trace, etc.) to a client. Log the real error
// server-side (slog) before calling this.
//
// Usage:
//
//return httpx.RespondError(c, fiber.StatusBadRequest, "invalid_json", "request body must be valid JSON")
func RespondError(c *fiber.Ctx, status int, code Code, message string) error {
if status >= fiber.StatusInternalServerError {
code = CodeInternal
message = genericInternalMessage
}
requestID := ""
if rid, ok := c.Locals("requestid").(string); ok {
requestID = rid
}
return c.Status(status).JSON(ErrorEnvelope{
Error:     string(code),
Message:   message,
RequestID: requestID,
})
}

// DefaultCodeForStatus returns the category code conventionally associated
// with an HTTP status, for cases where a handler only has a status (e.g.
// mapping a framework-level error) rather than an explicit Code.
func DefaultCodeForStatus(status int) Code {
switch status {
case fiber.StatusBadRequest:
return CodeBadRequest
case fiber.StatusUnauthorized:
return CodeUnauthorized
case fiber.StatusForbidden:
return CodeForbidden
case fiber.StatusNotFound:
return CodeNotFound
case fiber.StatusMethodNotAllowed:
return CodeMethodNotAllowed
case fiber.StatusConflict:
return CodeConflict
case fiber.StatusUnprocessableEntity:
return CodeUnprocessable
case fiber.StatusTooManyRequests:
return CodeTooManyRequests
case fiber.StatusRequestEntityTooLarge:
return CodeRequestTooLarge
case fiber.StatusServiceUnavailable:
return CodeServiceUnavailable
default:
return CodeInternal
}
}
