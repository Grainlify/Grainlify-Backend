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
  <link rel="stylesheet" href="https://cdnjs.cloudflare.com/ajax/libs/swagger-ui/5.17.14/swagger-ui.min.css" integrity="sha384-+R8fx41ke61Yq4+j30C8Uh8fbNUEGy8Le9lzVmeMfiUNd+H3q8gN7qW9LM3rbesj" crossorigin="anonymous" />
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://cdnjs.cloudflare.com/ajax/libs/swagger-ui/5.17.14/swagger-ui-bundle.min.js" integrity="sha384-5zikP7B6JemcHcJalIiFMnUirmSVBr3lZ85ClVPXO7mamWuqUixgyCMUPcKKfZLk" crossorigin="anonymous"></script>
  <script src="https://cdnjs.cloudflare.com/ajax/libs/swagger-ui/5.17.14/swagger-ui-standalone-preset.min.js" integrity="sha384-bdybAUYvjHTEAqNQoVuYIl37JlT41Ppz216cwp0ulSS1vqDsEPMLAmg8E79VeBq4" crossorigin="anonymous"></script>
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
