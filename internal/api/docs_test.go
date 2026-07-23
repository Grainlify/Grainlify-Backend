package api_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/jagadeesh/grainlify/backend/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegisterDocsRoutes_DocsEndpoint(t *testing.T) {
	app := fiber.New()
	api.RegisterDocsRoutes(app)

	req := httptest.NewRequest(http.MethodGet, "/docs", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/html; charset=utf-8", resp.Header.Get("Content-Type"))

	bodyBytes, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	html := string(bodyBytes)

	// CDN asset URLs to check
	cssURL := "https://cdnjs.cloudflare.com/ajax/libs/swagger-ui/5.17.14/swagger-ui.min.css"
	bundleURL := "https://cdnjs.cloudflare.com/ajax/libs/swagger-ui/5.17.14/swagger-ui-bundle.min.js"
	presetURL := "https://cdnjs.cloudflare.com/ajax/libs/swagger-ui/5.17.14/swagger-ui-standalone-preset.min.js"

	// Expected SRI hashes
	expectedCSSHash := "sha384-+R8fx41ke61Yq4+j30C8Uh8fbNUEGy8Le9lzVmeMfiUNd+H3q8gN7qW9LM3rbesj"
	expectedBundleHash := "sha384-5zikP7B6JemcHcJalIiFMnUirmSVBr3lZ85ClVPXO7mamWuqUixgyCMUPcKKfZLk"
	expectedPresetHash := "sha384-bdybAUYvjHTEAqNQoVuYIl37JlT41Ppz216cwp0ulSS1vqDsEPMLAmg8E79VeBq4"

	// Assert presence of assets in HTML
	assert.Contains(t, html, cssURL)
	assert.Contains(t, html, bundleURL)
	assert.Contains(t, html, presetURL)

	// Assert presence of integrity and crossorigin attributes
	assert.Contains(t, html, `integrity="`+expectedCSSHash+`"`)
	assert.Contains(t, html, `integrity="`+expectedBundleHash+`"`)
	assert.Contains(t, html, `integrity="`+expectedPresetHash+`"`)
	assert.Contains(t, html, `crossorigin="anonymous"`)

	// Verify each tag structure specifically
	cssTagSnippet := `<link rel="stylesheet" href="` + cssURL + `" integrity="` + expectedCSSHash + `" crossorigin="anonymous" />`
	bundleTagSnippet := `<script src="` + bundleURL + `" integrity="` + expectedBundleHash + `" crossorigin="anonymous"></script>`
	presetTagSnippet := `<script src="` + presetURL + `" integrity="` + expectedPresetHash + `" crossorigin="anonymous"></script>`

	assert.Contains(t, html, cssTagSnippet)
	assert.Contains(t, html, bundleTagSnippet)
	assert.Contains(t, html, presetTagSnippet)
}

func TestRegisterDocsRoutes_OpenAPIEndpoint(t *testing.T) {
	app := fiber.New()
	api.RegisterDocsRoutes(app)

	req := httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/yaml; charset=utf-8", resp.Header.Get("Content-Type"))

	bodyBytes, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.NotEmpty(t, bodyBytes)
	assert.True(t, strings.Contains(string(bodyBytes), "openapi:"))
}
