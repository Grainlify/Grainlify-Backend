package api_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/gofiber/fiber/v2/middleware/requestid"
	"github.com/jagadeesh/grainlify/backend/internal/api"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testApp(t *testing.T, routes func(*fiber.App)) *fiber.App {
	t.Helper()

	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		ErrorHandler:          api.JSONErrorHandler(),
	})
	app.Use(requestid.New())
	app.Use(recover.New(recover.Config{
		EnableStackTrace:  true,
		StackTraceHandler: api.PanicStackTraceHandler,
	}))
	routes(app)
	return app
}

func decodeErrorBody(t *testing.T, resp *http.Response) map[string]interface{} {
	t.Helper()
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var decoded map[string]interface{}
	require.NoError(t, json.Unmarshal(body, &decoded))
	return decoded
}

func TestJSONErrorHandler_GenericErrorReturnsOpaque500(t *testing.T) {
	app := testApp(t, func(app *fiber.App) {
		app.Get("/boom", func(c *fiber.Ctx) error {
			return fiber.NewError(fiber.StatusInternalServerError, "database connection leaked: secret-host")
		})
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/boom", nil))
	require.NoError(t, err)

	assert.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, fiber.MIMEApplicationJSON, resp.Header.Get(fiber.HeaderContentType))

	body := decodeErrorBody(t, resp)
	assert.Equal(t, "internal_server_error", body["error"])
	assert.Equal(t, "An unexpected error occurred", body["message"])
	assert.NotEmpty(t, body["request_id"])
	assert.NotContains(t, body, "database")
	assert.NotContains(t, body, "secret-host")
}

func TestJSONErrorHandler_FiberErrorPreserves4xxStatus(t *testing.T) {
	app := testApp(t, func(app *fiber.App) {
		app.Get("/missing", func(c *fiber.Ctx) error {
			return fiber.NewError(fiber.StatusNotFound, "not_found")
		})
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/missing", nil))
	require.NoError(t, err)

	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)

	body := decodeErrorBody(t, resp)
	assert.Equal(t, "not_found", body["error"])
	assert.Equal(t, "not_found", body["message"])
	assert.NotEmpty(t, body["request_id"])
}

func TestJSONErrorHandler_GenericGoErrorMapsTo500(t *testing.T) {
	app := testApp(t, func(app *fiber.App) {
		app.Get("/fail", func(c *fiber.Ctx) error {
			return assert.AnError
		})
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/fail", nil))
	require.NoError(t, err)

	assert.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)

	body := decodeErrorBody(t, resp)
	assert.Equal(t, "internal_server_error", body["error"])
	assert.Equal(t, "An unexpected error occurred", body["message"])
	assert.NotEmpty(t, body["request_id"])
	assert.NotContains(t, strings.ToLower(body["message"].(string)), "assert")
}

func TestJSONErrorHandler_PanicReturnsOpaque500(t *testing.T) {
	app := testApp(t, func(app *fiber.App) {
		app.Get("/panic", func(c *fiber.Ctx) error {
			panic("super secret internal state")
		})
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/panic", nil))
	require.NoError(t, err)

	assert.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)

	body := decodeErrorBody(t, resp)
	assert.Equal(t, "internal_server_error", body["error"])
	assert.Equal(t, "An unexpected error occurred", body["message"])
	assert.NotEmpty(t, body["request_id"])
	assert.NotContains(t, body, "super secret")
	assert.NotContains(t, body, "stack")
}

func TestJSONErrorHandler_CustomFiberStatusPreserved(t *testing.T) {
	app := testApp(t, func(app *fiber.App) {
		app.Get("/teapot", func(c *fiber.Ctx) error {
			return fiber.NewError(fiber.StatusTeapot, "short_and_stout")
		})
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/teapot", nil))
	require.NoError(t, err)

	assert.Equal(t, fiber.StatusTeapot, resp.StatusCode)

	body := decodeErrorBody(t, resp)
	assert.Equal(t, "short_and_stout", body["error"])
	assert.Equal(t, "short_and_stout", body["message"])
}

func TestAppNotFoundUsesErrorEnvelope(t *testing.T) {
	app := api.New(config.Config{}, api.Deps{})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/definitely-missing-route", nil))
	require.NoError(t, err)

	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)

	body := decodeErrorBody(t, resp)
	assert.Equal(t, "not_found", body["error"])
	assert.Equal(t, "/definitely-missing-route", body["path"])
	assert.NotEmpty(t, body["request_id"])
}

func TestRecoverStackTraceHandlerLogsAtErrorLevel(t *testing.T) {
	var logs []string
	original := slog.Default()
	handler := slog.NewTextHandler(&captureWriter{lines: &logs}, &slog.HandlerOptions{Level: slog.LevelDebug})
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(original) })

	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		ErrorHandler:          api.JSONErrorHandler(),
	})
	app.Use(requestid.New())
	app.Use(recover.New(recover.Config{
		EnableStackTrace:  true,
		StackTraceHandler: api.PanicStackTraceHandler,
	}))
	app.Get("/panic", func(c *fiber.Ctx) error {
		panic("logged panic detail")
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/panic", nil))
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusInternalServerError, resp.StatusCode)

	require.NotEmpty(t, logs)
	combined := strings.Join(logs, "\n")
	assert.Contains(t, combined, "panic recovered")
	assert.Contains(t, combined, "logged panic detail")
	assert.Contains(t, combined, "stack=")
}

type captureWriter struct {
	lines *[]string
}

func (w *captureWriter) Write(p []byte) (int, error) {
	*w.lines = append(*w.lines, string(p))
	return len(p), nil
}
