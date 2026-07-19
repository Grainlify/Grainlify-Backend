package api

import (
"errors"
"log/slog"
"runtime/debug"
"strings"

"github.com/gofiber/fiber/v2"

"github.com/jagadeesh/grainlify/backend/internal/httpx"
)

// ErrorEnvelope is the standard JSON error response shape used across the API.
// This mirrors httpx.ErrorEnvelope field-for-field; both packages must stay
// in sync so every endpoint returns the same shape. See
// docs/reference/api-endpoints.md for the documented contract.
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
return status, string(httpx.CodeInternal), opaqueServerMessage, err
}
code = errorCodeFromFiberMessage(fiberErr.Message)
if code == "" {
code = defaultErrorCodeForStatus(status)
}
return status, code, fiberErr.Message, nil
}

return fiber.StatusInternalServerError, string(httpx.CodeInternal), opaqueServerMessage, err
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

// defaultErrorCodeForStatus maps an HTTP status to its category code, using
// the shared taxonomy defined in internal/httpx so this package and httpx
// never drift into two different sets of codes for the same statuses.
func defaultErrorCodeForStatus(status int) string {
if status == fiber.StatusTeapot {
return "request_failed"
}
return string(httpx.DefaultCodeForStatus(status))
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