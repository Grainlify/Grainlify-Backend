package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// mockFlappyDBPool embeds db.DBPool and overrides only Ping, so it satisfies
// the interface without needing a real Postgres connection.
type mockFlappyDBPool struct {
	db.DBPool
	pingErr error
}

func (m *mockFlappyDBPool) Ping(ctx context.Context) error {
	return m.pingErr
}

func readyStatus(t *testing.T, d *db.DB) (int, map[string]any) {
	t.Helper()
	app := fiber.New()
	app.Get("/ready", handlers.Ready(d))

	resp, err := app.Test(httptest.NewRequest("GET", "/ready", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp.StatusCode, body
}

func TestReady_NilDB(t *testing.T) {
	status, body := readyStatus(t, nil)
	if status != fiber.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", status, fiber.StatusServiceUnavailable)
	}
	if body["reason"] != "db_not_configured" {
		t.Errorf("reason = %v, want db_not_configured", body["reason"])
	}
}

func TestReady_NilPool(t *testing.T) {
	status, body := readyStatus(t, &db.DB{Pool: nil})
	if status != fiber.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", status, fiber.StatusServiceUnavailable)
	}
	if body["reason"] != "db_not_configured" {
		t.Errorf("reason = %v, want db_not_configured", body["reason"])
	}
}

func TestReady_PingFails(t *testing.T) {
	pool := &mockFlappyDBPool{pingErr: fmt.Errorf("connection refused")}
	status, body := readyStatus(t, &db.DB{Pool: pool})
	if status != fiber.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", status, fiber.StatusServiceUnavailable)
	}
	if body["reason"] != "db_unreachable" {
		t.Errorf("reason = %v, want db_unreachable", body["reason"])
	}
}

func TestReady_Healthy(t *testing.T) {
	d := testDB(t)
	status, body := readyStatus(t, d)
	if status != fiber.StatusOK {
		t.Errorf("status = %d, want %d", status, fiber.StatusOK)
	}
	if body["ok"] != true {
		t.Errorf("ok = %v, want true", body["ok"])
	}
}

// Ensure mockFlappyDBPool actually satisfies db.DBPool at compile time.
var _ db.DBPool = (*mockFlappyDBPool)(nil)
