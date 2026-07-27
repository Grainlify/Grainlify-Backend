package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

type failingHealthPool struct {
	err error
}

func (p failingHealthPool) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, p.err
}
func (p failingHealthPool) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, p.err
}
func (p failingHealthPool) QueryRow(context.Context, string, ...any) pgx.Row { return nil }
func (p failingHealthPool) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return nil, p.err
}
func (p failingHealthPool) Ping(context.Context) error { return p.err }
func (p failingHealthPool) Close()                     {}
func (p failingHealthPool) Config() *pgxpool.Config    { return nil }

func TestHealthReportsGrainlifyServiceName(t *testing.T) {
	app := fiber.New()
	app.Get("/health", handlers.NewHealth(handlers.BuildInfo{
		Version: "1.0.0", Commit: "abc123", BuildTime: "2025-01-01T00:00:00Z",
	}))

	resp, err := app.Test(httptest.NewRequest("GET", "/health", nil))
	if err != nil {
		t.Fatalf("app.Test returned error: %v", err)
	}
	defer resp.Body.Close()

	var body struct {
		OK        bool   `json:"ok"`
		Service   string `json:"service"`
		Version   string `json:"version"`
		Commit    string `json:"commit"`
		BuildTime string `json:"build_time"`
		Uptime    string `json:"uptime"`
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
	if body.Version != "1.0.0" {
		t.Fatalf("health version = %q, want %q", body.Version, "1.0.0")
	}
	if body.Commit != "abc123" {
		t.Fatalf("health commit = %q, want %q", body.Commit, "abc123")
	}
	if body.BuildTime != "2025-01-01T00:00:00Z" {
		t.Fatalf("health build_time = %q, want %q", body.BuildTime, "2025-01-01T00:00:00Z")
	}
	if body.Uptime == "" {
		t.Fatal("health uptime is empty, want a duration string")
	}
}

func TestHealthUptimeIsRecent(t *testing.T) {
	app := fiber.New()
	start := time.Now()
	app.Get("/health", handlers.NewHealth(handlers.BuildInfo{
		Version: "dev", Commit: "none", BuildTime: "unknown",
	}))

	// Give a tiny bit of wall-clock time so uptime > 0.
	time.Sleep(5 * time.Millisecond)

	resp, err := app.Test(httptest.NewRequest("GET", "/health", nil))
	if err != nil {
		t.Fatalf("app.Test returned error: %v", err)
	}
	defer resp.Body.Close()

	var body struct {
		Uptime string `json:"uptime"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode health response: %v", err)
	}

	d, err := time.ParseDuration(body.Uptime)
	if err != nil {
		t.Fatalf("uptime %q is not a valid duration: %v", body.Uptime, err)
	}
	if d <= 0 {
		t.Fatalf("uptime %v should be positive", d)
	}
	if d > time.Minute {
		t.Fatalf("uptime %v is unreasonably large; test took < 1m", d)
	}
	_ = start // used to verify monotonic relationship conceptually
}

func TestHealthBuildInfoDefaults(t *testing.T) {
	app := fiber.New()
	app.Get("/health", handlers.NewHealth(handlers.BuildInfo{}))

	resp, err := app.Test(httptest.NewRequest("GET", "/health", nil))
	if err != nil {
		t.Fatalf("app.Test returned error: %v", err)
	}
	defer resp.Body.Close()

	var body struct {
		Version   string `json:"version"`
		Commit    string `json:"commit"`
		BuildTime string `json:"build_time"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode health response: %v", err)
	}

	if body.Version != "" {
		t.Fatalf("expected empty version when not provided, got %q", body.Version)
	}
	if body.Commit != "" {
		t.Fatalf("expected empty commit when not provided, got %q", body.Commit)
	}
	if body.BuildTime != "" {
		t.Fatalf("expected empty build_time when not provided, got %q", body.BuildTime)
	}
}

func TestHealthStatusCode(t *testing.T) {
	app := fiber.New()
	app.Get("/health", handlers.NewHealth(handlers.BuildInfo{}))

	resp, err := app.Test(httptest.NewRequest("GET", "/health", nil))
	if err != nil {
		t.Fatalf("app.Test returned error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestHealthResponseContainsOnlyExpectedFields(t *testing.T) {
	app := fiber.New()
	app.Get("/health", handlers.NewHealth(handlers.BuildInfo{
		Version: "2.0.0", Commit: "def456", BuildTime: "2025-06-01T00:00:00Z",
	}))

	resp, err := app.Test(httptest.NewRequest("GET", "/health", nil))
	if err != nil {
		t.Fatalf("app.Test returned error: %v", err)
	}
	defer resp.Body.Close()

	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode health response: %v", err)
	}

	expectedKeys := []string{"ok", "service", "version", "commit", "build_time", "uptime"}
	for _, k := range expectedKeys {
		if _, ok := raw[k]; !ok {
			t.Errorf("missing expected key %q in health response", k)
		}
	}
	// Ensure no unexpected keys leak secrets.
	for k := range raw {
		found := false
		for _, exp := range expectedKeys {
			if k == exp {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("unexpected key %q in health response (possible info leak)", k)
		}
	}
}

func TestHealthDoesNotExposeSensitiveData(t *testing.T) {
	app := fiber.New()
	app.Get("/health", handlers.NewHealth(handlers.BuildInfo{}))

	resp, err := app.Test(httptest.NewRequest("GET", "/health", nil))
	if err != nil {
		t.Fatalf("app.Test returned error: %v", err)
	}
	defer resp.Body.Close()

	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode health response: %v", err)
	}

	sensitiveKeys := []string{"db_url", "nats_url", "jwt_secret", "password", "token", "secret"}
	bodyStr := strings.ToLower(strings.Join(func() []string {
		var vals []string
		for k, v := range raw {
			vals = append(vals, k, fmt.Sprintf("%v", v))
		}
		return vals
	}(), " "))
	for _, sk := range sensitiveKeys {
		if strings.Contains(bodyStr, sk) {
			t.Errorf("response may leak sensitive key matching %q", sk)
		}
	}
}

func TestHealthReportsDatabaseUnavailableDistinctly(t *testing.T) {
	app := fiber.New()
	app.Get("/health", handlers.NewHealthWithDB(handlers.BuildInfo{}, &db.DB{Pool: failingHealthPool{err: fmt.Errorf("dial tcp: connection refused")}}))

	resp, err := app.Test(httptest.NewRequest("GET", "/health", nil))
	if err != nil {
		t.Fatalf("app.Test returned error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}

	var body struct {
		OK       bool   `json:"ok"`
		Database string `json:"database"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if body.OK {
		t.Fatal("health response ok = true, want false")
	}
	if body.Database != "unavailable" {
		t.Fatalf("database status = %q, want %q", body.Database, "unavailable")
	}
}
