package handlers_test

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// The endpoint that makes "it is deployed" falsifiable.
func TestVersion_ReportsTheCommitTheEnvironmentSupplies(t *testing.T) {
	t.Setenv("RAILWAY_GIT_COMMIT_SHA", "abc123def456")

	app := fiber.New()
	app.Get("/version", handlers.Version())
	resp, err := app.Test(httptest.NewRequest("GET", "/version", nil))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if got["commit"] != "abc123def456" {
		t.Errorf("commit = %v, want the value from the environment", got["commit"])
	}
	if got["commit_known"] != true {
		t.Errorf("commit_known = %v with a sha present", got["commit_known"])
	}
}

// Unknown must read as unknown. A plausible-looking default here would make
// the deploy check pass while proving nothing - which is precisely the class
// of failure this endpoint was added to remove.
func TestVersion_UnknownIsReportedRatherThanInvented(t *testing.T) {
	for _, k := range []string{"RAILWAY_GIT_COMMIT_SHA", "VERCEL_GIT_COMMIT_SHA", "GIT_COMMIT_SHA", "SOURCE_COMMIT", "COMMIT_SHA"} {
		t.Setenv(k, "")
	}

	app := fiber.New()
	app.Get("/version", handlers.Version())
	resp, _ := app.Test(httptest.NewRequest("GET", "/version", nil))
	body, _ := io.ReadAll(resp.Body)
	var got map[string]any
	_ = json.Unmarshal(body, &got)

	if got["commit"] != "" {
		t.Errorf("commit = %v with nothing configured; it must not invent one", got["commit"])
	}
	if got["commit_known"] != false {
		t.Errorf("commit_known = %v with nothing configured", got["commit_known"])
	}
}
