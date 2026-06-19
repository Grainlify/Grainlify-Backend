package handlers_test

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

func TestHealthReportsGrainlifyServiceName(t *testing.T) {
	app := fiber.New()
	app.Get("/health", handlers.Health())

	resp, err := app.Test(httptest.NewRequest("GET", "/health", nil))
	if err != nil {
		t.Fatalf("app.Test returned error: %v", err)
	}
	defer resp.Body.Close()

	var body struct {
		OK      bool   `json:"ok"`
		Service string `json:"service"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode health response: %v", err)
	}

	if !body.OK {
		t.Fatal("health response ok = false, want true")
	}
	if body.Service != handlers.ServiceName {
		t.Fatalf("health service = %q, want %q", body.Service, handlers.ServiceName)
	}
}
