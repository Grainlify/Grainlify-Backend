package api_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"gopkg.in/yaml.v3"
)

// specPaths parses openapi.yaml and returns all documented paths.
func specPaths(t *testing.T) map[string]struct{} {
t.Helper()
data, err := os.ReadFile("openapi.yaml")
if err != nil {
t.Fatalf("failed to read openapi.yaml: %v", err)
}

var spec struct {
Paths map[string]interface{} `yaml:"paths"`
}
if err := yaml.Unmarshal(data, &spec); err != nil {
t.Fatalf("failed to parse openapi.yaml: %v", err)
}

paths := make(map[string]struct{}, len(spec.Paths))
for p := range spec.Paths {
// Normalise Fiber :param style to OpenAPI {param} style
paths[p] = struct{}{}
}
return paths
}

// TestOpenAPISpecParses verifies the YAML is valid and contains paths.
func TestOpenAPISpecParses(t *testing.T) {
paths := specPaths(t)
if len(paths) == 0 {
t.Fatal("openapi.yaml has no paths defined")
}
t.Logf("openapi.yaml documents %d paths", len(paths))
}

// TestOpenAPISpecVersion verifies openapi version field is 3.x.
func TestOpenAPISpecVersion(t *testing.T) {
data, err := os.ReadFile("openapi.yaml")
if err != nil {
t.Fatalf("failed to read openapi.yaml: %v", err)
}

var spec struct {
OpenAPI string `yaml:"openapi"`
Info    struct {
Title   string `yaml:"title"`
Version string `yaml:"version"`
} `yaml:"info"`
}
if err := yaml.Unmarshal(data, &spec); err != nil {
t.Fatalf("failed to parse openapi.yaml: %v", err)
}

if !strings.HasPrefix(spec.OpenAPI, "3.") {
t.Errorf("expected OpenAPI 3.x, got %q", spec.OpenAPI)
}
if spec.Info.Title == "" {
t.Error("openapi.yaml info.title is empty")
}
if spec.Info.Version == "" {
t.Error("openapi.yaml info.version is empty")
}
	t.Logf("spec: OpenAPI %s — %s %s", spec.OpenAPI, spec.Info.Title, spec.Info.Version)
}

// TestOpenAPISpecValid validates the spec against the OpenAPI 3.x schema
// using kin-openapi. This catches structural errors like broken $ref targets,
// invalid schemas, missing required fields, etc.
func TestOpenAPISpecValid(t *testing.T) {
	data, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatalf("failed to read openapi.yaml: %v", err)
	}

	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = false

	doc, err := loader.LoadFromData(data)
	if err != nil {
		t.Fatalf("failed to load openapi.yaml: %v", err)
	}

	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("openapi.yaml validation failed: %v", err)
	}
	t.Logf("openapi.yaml is a valid OpenAPI %s document", doc.OpenAPI)
}

// registeredRoutes is the canonical list of routes from internal/api/api.go.
// Update this list whenever a route is added or removed in api.go.
var registeredRoutes = []struct {
method string
path   string
}{
{"GET", "/"},
{"GET", "/health"},
{"GET", "/ready"},
{"POST", "/auth/nonce"},
{"POST", "/auth/verify"},
{"GET", "/auth/github/login/start"},
{"GET", "/auth/github/login/callback"},
{"POST", "/auth/github/start"},
{"GET", "/auth/github/callback"},
{"GET", "/auth/github/status"},
{"POST", "/auth/github/app/install/start"},
{"GET", "/auth/github/app/install/callback"},
{"POST", "/auth/kyc/start"},
{"GET", "/auth/kyc/status"},
{"GET", "/me"},
{"POST", "/me/github/resync"},
{"GET", "/profile"},
{"GET", "/profile/public"},
{"GET", "/profile/calendar"},
{"GET", "/profile/activity"},
{"GET", "/profile/projects"},
{"GET", "/profile/projects-led"},
{"PUT", "/profile/update"},
{"PUT", "/profile/avatar"},
{"GET", "/ecosystems"},
{"GET", "/ecosystems/{id}"},
{"GET", "/leaderboard"},
{"GET", "/stats/landing"},
{"GET", "/open-source-week/events"},
{"GET", "/open-source-week/events/{id}"},
{"GET", "/projects"},
{"POST", "/projects"},
{"GET", "/projects/recommended"},
{"GET", "/projects/filters"},
{"GET", "/projects/mine"},
{"GET", "/projects/pending-setup"},
{"GET", "/projects/{id}"},
{"PUT", "/projects/{id}/metadata"},
{"GET", "/projects/{id}/issues/public"},
{"GET", "/projects/{id}/prs/public"},
{"POST", "/projects/{id}/verify"},
{"POST", "/projects/{id}/sync"},
{"GET", "/projects/{id}/sync/jobs"},
{"GET", "/projects/{id}/issues"},
{"GET", "/projects/{id}/prs"},
{"GET", "/projects/{id}/events"},
{"POST", "/projects/{id}/issues/{number}/apply"},
{"POST", "/projects/{id}/issues/{number}/bot-comment"},
{"POST", "/projects/{id}/issues/{number}/withdraw"},
{"POST", "/projects/{id}/issues/{number}/assign"},
{"POST", "/projects/{id}/issues/{number}/unassign"},
{"POST", "/projects/{id}/issues/{number}/reject"},
{"POST", "/admin/bootstrap"},
{"GET", "/admin/users"},
{"PUT", "/admin/users/{id}/role"},
{"GET", "/admin/ecosystems"},
{"POST", "/admin/ecosystems"},
{"GET", "/admin/ecosystems/{id}"},
{"PUT", "/admin/ecosystems/{id}"},
{"DELETE", "/admin/ecosystems/{id}"},
{"GET", "/admin/open-source-week/events"},
{"POST", "/admin/open-source-week/events"},
{"DELETE", "/admin/open-source-week/events/{id}"},
{"POST", "/webhooks/github"},
{"GET", "/webhooks/didit"},
{"POST", "/webhooks/didit"},
{"GET", "/docs"},
{"GET", "/openapi.yaml"},
}

// TestAllRoutesDocumented checks every registered route has an entry in openapi.yaml.
func TestAllRoutesDocumented(t *testing.T) {
specPaths := specPaths(t)

missing := []string{}
for _, r := range registeredRoutes {
if _, ok := specPaths[r.path]; !ok {
missing = append(missing, r.method+" "+r.path)
}
}

if len(missing) > 0 {
t.Errorf("routes registered in api.go but missing from openapi.yaml:\n  %s",
strings.Join(missing, "\n  "))
}
}

// TestDocsRoutesServed verifies /docs and /openapi.yaml are in the spec.
func TestDocsRoutesServed(t *testing.T) {
paths := specPaths(t)
for _, p := range []string{"/docs", "/openapi.yaml"} {
if _, ok := paths[p]; !ok {
t.Errorf("expected %s in openapi.yaml paths", p)
}
}
}
