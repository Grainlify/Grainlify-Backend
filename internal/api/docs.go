package api

import (
_ "embed"

"github.com/gofiber/fiber/v2"
)

//go:embed openapi.yaml
var openapiSpec []byte

// RegisterDocsRoutes registers the /openapi.yaml and /docs routes on the app.
// The Swagger UI is served via CDN so no additional dependencies are required.
func RegisterDocsRoutes(app *fiber.App) {
// Serve raw OpenAPI spec
app.Get("/openapi.yaml", func(c *fiber.Ctx) error {
c.Set("Content-Type", "application/yaml; charset=utf-8")
return c.Send(openapiSpec)
})

// Serve Swagger UI
app.Get("/docs", func(c *fiber.Ctx) error {
c.Set("Content-Type", "text/html; charset=utf-8")
return c.SendString(swaggerUIHTML)
})
}

const swaggerUIHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1.0" />
  <title>Grainlify API Docs</title>
  <link rel="stylesheet" href="https://cdnjs.cloudflare.com/ajax/libs/swagger-ui/5.17.14/swagger-ui.min.css" />
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://cdnjs.cloudflare.com/ajax/libs/swagger-ui/5.17.14/swagger-ui-bundle.min.js"></script>
  <script src="https://cdnjs.cloudflare.com/ajax/libs/swagger-ui/5.17.14/swagger-ui-standalone-preset.min.js"></script>
  <script>
    window.onload = function () {
      SwaggerUIBundle({
        url: "/openapi.yaml",
        dom_id: "#swagger-ui",
        presets: [SwaggerUIBundle.presets.apis, SwaggerUIStandalonePreset],
        layout: "StandaloneLayout",
        deepLinking: true,
        tryItOutEnabled: true,
      });
    };
  </script>
</body>
</html>`
