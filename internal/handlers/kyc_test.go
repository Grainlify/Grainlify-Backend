package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
)

func TestMapDiditStatusTerminalDecisions(t *testing.T) {
	tests := []struct {
		name        string
		diditStatus string
		want        string
	}{
		{name: "approved", diditStatus: "approved", want: "verified"},
		{name: "verified", diditStatus: "verified", want: "verified"},
		{name: "rejected", diditStatus: "rejected", want: "rejected"},
		{name: "declined", diditStatus: "declined", want: "rejected"},
		{name: "expired", diditStatus: "expired", want: "expired"},
		{name: "unknown fails closed", diditStatus: "ambiguous_provider_status", want: "not_started"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mapDiditStatus(tt.diditStatus); got != tt.want {
				t.Fatalf("mapDiditStatus(%q) = %q, want %q", tt.diditStatus, got, tt.want)
			}
		})
	}
}

type mockRow struct {
	values []interface{}
	err    error
}

func (r *mockRow) Scan(dest ...interface{}) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dest {
		if i >= len(r.values) {
			break
		}
		val := r.values[i]
		if val == nil {
			continue
		}
		
		destVal := reflect.ValueOf(d)
		if destVal.Kind() != reflect.Ptr {
			return fmt.Errorf("scan destination is not a pointer")
		}
		
		srcVal := reflect.ValueOf(val)
		
		if destVal.Elem().Kind() == reflect.Ptr && srcVal.Kind() == reflect.Ptr {
			destVal.Elem().Set(srcVal)
			continue
		}
		
		if destVal.Elem().Kind() == reflect.Ptr && srcVal.Kind() != reflect.Ptr {
			newVal := reflect.New(srcVal.Type())
			newVal.Elem().Set(srcVal)
			destVal.Elem().Set(newVal)
			continue
		}
		
		if destVal.Elem().Kind() != reflect.Ptr && srcVal.Kind() == reflect.Ptr {
			destVal.Elem().Set(srcVal.Elem())
			continue
		}
		
		destVal.Elem().Set(srcVal)
	}
	return nil
}

type mockDBPool struct {
	queryRowFunc func(sql string, args ...any) pgx.Row
	execFunc     func(sql string, args ...any) (pgconn.CommandTag, error)
}

func (m *mockDBPool) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	if m.execFunc != nil {
		return m.execFunc(sql, arguments...)
	}
	return pgconn.CommandTag{}, nil
}
func (m *mockDBPool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return nil, nil
}
func (m *mockDBPool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if m.queryRowFunc != nil {
		return m.queryRowFunc(sql, args...)
	}
	return &mockRow{}
}
func (m *mockDBPool) BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error) {
	return nil, nil
}
func (m *mockDBPool) Ping(ctx context.Context) error {
	return nil
}
func (m *mockDBPool) Close() {}
func (m *mockDBPool) Config() *pgxpool.Config {
	return nil
}

func TestKYCHandlerLoggingRedaction(t *testing.T) {
	// Setup log capturing
	var buf bytes.Buffer
	origLogger := slog.Default()
	captureHandler := slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})
	slog.SetDefault(slog.New(captureHandler))
	defer slog.SetDefault(origLogger)

	// Create mock server for Didit API
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		
		if strings.Contains(r.URL.Path, "error-session") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error": "bad_request", "message": "SensitiveErrorMessageForKYC_first_name=ShouldBeRedacted"}`))
			return
		}
		
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"status": "verified",
			"decision": {
				"first_name": "JohnKYCName",
				"last_name": "DoeKYCName",
				"document_number": "12345KYCDoc"
			},
			"data": {
				"address": "123KYCAddressSt",
				"date_of_birth": "1995-05-05"
			},
			"extra_field": "KYCSensitiveValue"
		}`))
	}))
	defer server.Close()

	t.Run("Success_Path_Redacts_PII", func(t *testing.T) {
		buf.Reset()

		mockPool := &mockDBPool{
			queryRowFunc: func(sql string, args ...any) pgx.Row {
				status := "pending"
				sessionID := "valid-session"
				var verifiedAt *time.Time = nil
				kycData := []byte(`{}`)
				return &mockRow{
					values: []interface{}{&status, &sessionID, verifiedAt, kycData},
				}
			},
			execFunc: func(sql string, args ...any) (pgconn.CommandTag, error) {
				return pgconn.CommandTag{}, nil
			},
		}

		cfg := config.Config{
			DiditAPIKey: "mock-api-key",
		}
		h := NewKYCHandler(cfg, &db.DB{Pool: mockPool})
		h.didit.BaseURL = server.URL

		app := fiber.New()
		app.Use(func(c *fiber.Ctx) error {
			c.Locals("user_id", uuid.New().String())
			return c.Next()
		})
		app.Get("/kyc/status", h.Status())

		req := httptest.NewRequest("GET", "/kyc/status", nil)
		resp, err := app.Test(req, -1)
		if err != nil {
			t.Fatalf("failed request: %v", err)
		}
		defer resp.Body.Close()

		logOutput := buf.String()

		// Assert sensitive values never appear in raw form in the logs
		sensitiveValues := []string{"JohnKYCName", "DoeKYCName", "12345KYCDoc", "123KYCAddressSt", "1995-05-05"}
		for _, val := range sensitiveValues {
			if strings.Contains(logOutput, val) {
				t.Errorf("found sensitive value %q in log output: %s", val, logOutput)
			}
		}

		if !strings.Contains(logOutput, "fetched didit status") {
			t.Errorf("expected 'fetched didit status' log to exist, got: %s", logOutput)
		}
		
		if !strings.Contains(logOutput, "[REDACTED]") {
			t.Errorf("expected '[REDACTED]' placeholder to exist in log, got: %s", logOutput)
		}
	})

	t.Run("Error_Path_Redacts_PII", func(t *testing.T) {
		buf.Reset()

		mockPool := &mockDBPool{
			queryRowFunc: func(sql string, args ...any) pgx.Row {
				status := "pending"
				sessionID := "error-session"
				var verifiedAt *time.Time = nil
				kycData := []byte(`{}`)
				return &mockRow{
					values: []interface{}{&status, &sessionID, verifiedAt, kycData},
				}
			},
			execFunc: func(sql string, args ...any) (pgconn.CommandTag, error) {
				return pgconn.CommandTag{}, nil
			},
		}

		cfg := config.Config{
			DiditAPIKey: "mock-api-key",
		}
		h := NewKYCHandler(cfg, &db.DB{Pool: mockPool})
		h.didit.BaseURL = server.URL

		app := fiber.New()
		app.Use(func(c *fiber.Ctx) error {
			c.Locals("user_id", uuid.New().String())
			return c.Next()
		})
		app.Get("/kyc/status", h.Status())

		req := httptest.NewRequest("GET", "/kyc/status", nil)
		resp, err := app.Test(req, -1)
		if err != nil {
			t.Fatalf("failed request: %v", err)
		}
		defer resp.Body.Close()

		logOutput := buf.String()

		if strings.Contains(logOutput, "ShouldBeRedacted") {
			t.Errorf("found sensitive error value 'ShouldBeRedacted' in log output: %s", logOutput)
		}

		if !strings.Contains(logOutput, "didit api call failed") {
			t.Errorf("expected 'didit api call failed' log to exist, got: %s", logOutput)
		}

		if !strings.Contains(logOutput, "[REDACTED]") {
			t.Errorf("expected '[REDACTED]' placeholder to exist in log, got: %s", logOutput)
		}
	})
}
