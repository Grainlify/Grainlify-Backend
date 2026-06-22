package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveAPIError(t *testing.T) {
	t.Run("fiber 4xx snake case message", func(t *testing.T) {
		status, code, message, logErr := resolveAPIError(fiber.NewError(fiber.StatusBadRequest, "invalid_body"))
		assert.Equal(t, fiber.StatusBadRequest, status)
		assert.Equal(t, "invalid_body", code)
		assert.Equal(t, "invalid_body", message)
		assert.Nil(t, logErr)
	})

	t.Run("fiber 4xx non snake case uses status default", func(t *testing.T) {
		status, code, message, logErr := resolveAPIError(fiber.NewError(fiber.StatusForbidden, "Access denied"))
		assert.Equal(t, fiber.StatusForbidden, status)
		assert.Equal(t, "forbidden", code)
		assert.Equal(t, "Access denied", message)
		assert.Nil(t, logErr)
	})

	t.Run("fiber 5xx opaque", func(t *testing.T) {
		status, code, message, logErr := resolveAPIError(fiber.NewError(fiber.StatusInternalServerError, "db exploded"))
		assert.Equal(t, fiber.StatusInternalServerError, status)
		assert.Equal(t, "internal_server_error", code)
		assert.Equal(t, opaqueServerMessage, message)
		assert.Error(t, logErr)
	})

	t.Run("generic error opaque", func(t *testing.T) {
		status, code, message, logErr := resolveAPIError(errors.New("secret"))
		assert.Equal(t, fiber.StatusInternalServerError, status)
		assert.Equal(t, "internal_server_error", code)
		assert.Equal(t, opaqueServerMessage, message)
		assert.Error(t, logErr)
	})
}

func TestErrorCodeFromFiberMessage(t *testing.T) {
	assert.Equal(t, "", errorCodeFromFiberMessage(""))
	assert.Equal(t, "", errorCodeFromFiberMessage("Bad Request"))
	assert.Equal(t, "not_found", errorCodeFromFiberMessage("not_found"))
}

func TestDefaultErrorCodeForStatus(t *testing.T) {
	cases := map[int]string{
		fiber.StatusBadRequest:          "bad_request",
		fiber.StatusUnauthorized:        "unauthorized",
		fiber.StatusForbidden:           "forbidden",
		fiber.StatusNotFound:            "not_found",
		fiber.StatusMethodNotAllowed:    "method_not_allowed",
		fiber.StatusConflict:            "conflict",
		fiber.StatusUnprocessableEntity: "unprocessable_entity",
		fiber.StatusTooManyRequests:     "too_many_requests",
		fiber.StatusTeapot:              "request_failed",
	}
	for status, want := range cases {
		assert.Equal(t, want, defaultErrorCodeForStatus(status), "status %d", status)
	}
}

func TestRequestIDFromCtxEmpty(t *testing.T) {
	app := fiber.New()
	var got string
	app.Get("/", func(c *fiber.Ctx) error {
		got = requestIDFromCtx(c)
		return c.SendStatus(fiber.StatusOK)
	})
	_, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
	require.NoError(t, err)
	assert.Equal(t, "", got)
}
