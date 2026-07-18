# OpenAPI Specification

The Grainlify API uses a hand-written OpenAPI 3.1.0 specification to document all endpoints, request/response schemas, authentication, and server information.

## Spec Locations

| Location | Purpose |
|----------|---------|
| `internal/api/openapi.yaml` | **Canonical spec** — embedded at compile time via `//go:embed` in `internal/api/docs.go` |
| `openapi.yaml` | Root-level convenience copy for external tooling |

The canonical spec lives inside the Go module so it's embedded automatically. The root-level copy is a mirror — keep it in sync when updating the canonical spec.

## Viewing the Spec

When the API is running:

- **Swagger UI**: `http://localhost:8080/docs`
- **Raw YAML**: `http://localhost:8080/openapi.yaml`

Start the dev server with `make dev`.

## Validation

### Schema validation (new)

A Go test validates the spec against the OpenAPI 3.x schema using `github.com/getkin/kin-openapi`:

```bash
go test -run TestOpenAPISpecValid ./internal/api/ -v
```

This test loads `openapi.yaml`, parses it, and checks that it complies with the OpenAPI specification. It catches:
- Broken `$ref` targets
- Invalid schema definitions
- Missing required fields
- Structural inconsistencies

The test runs as part of the full test suite (`go test ./...` or `make test`), so it is automatically checked in CI.

### Route-coverage validation (existing)

`TestAllRoutesDocumented` in `internal/api/openapi_test.go` verifies that every route registered in `api.go` has a corresponding path entry in the spec. Update the `registeredRoutes` list in that file whenever routes are added or removed.

## Updating the Spec

1. Edit `internal/api/openapi.yaml`
2. Run validation: `go test -run TestOpenAPI ./internal/api/ -v`
3. Sync the root mirror: `Copy-Item internal/api/openapi.yaml openapi.yaml` (or use `make openapi-sync`)
4. Commit both copies

## Notes

- The spec is maintained by hand rather than generated from Go types, giving full control over descriptions, examples, and documentation quality.
- The `//go:embed` directive in `docs.go` embeds the file at compile time — if the spec doesn't exist, the build will fail.
- When adding a new route, add it to both `api.go` and `openapi.yaml`, and add an entry to `registeredRoutes` in `openapi_test.go`.
