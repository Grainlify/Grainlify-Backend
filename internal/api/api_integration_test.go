package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jagadeesh/grainlify/backend/internal/api"
	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockDBPool is a stub implementation of db.DBPool interface used for integration tests.
type mockDBPool struct{}

// Exec implements db.DBPool.
func (m *mockDBPool) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

// Query implements db.DBPool.
func (m *mockDBPool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return &mockRows{}, nil
}

// QueryRow implements db.DBPool.
func (m *mockDBPool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return &mockRow{}
}

// BeginTx implements db.DBPool.
func (m *mockDBPool) BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error) {
	return &mockTx{}, nil
}

// Ping implements db.DBPool.
func (m *mockDBPool) Ping(ctx context.Context) error {
	return nil
}

// Close implements db.DBPool.
func (m *mockDBPool) Close() {}

// Config implements db.DBPool.
func (m *mockDBPool) Config() *pgxpool.Config {
	return nil
}

// mockTx is a stub implementation of pgx.Tx interface used for transaction mocking.
type mockTx struct{}

// Begin implements pgx.Tx.
func (m *mockTx) Begin(ctx context.Context) (pgx.Tx, error) { return m, nil }

// Commit implements pgx.Tx.
func (m *mockTx) Commit(ctx context.Context) error { return nil }

// Rollback implements pgx.Tx.
func (m *mockTx) Rollback(ctx context.Context) error { return nil }

// CopyFrom implements pgx.Tx.
func (m *mockTx) CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error) {
	return 0, nil
}

// SendBatch implements pgx.Tx.
func (m *mockTx) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults { return nil }

// LargeObjects implements pgx.Tx.
func (m *mockTx) LargeObjects() pgx.LargeObjects { return pgx.LargeObjects{} }

// Prepare implements pgx.Tx.
func (m *mockTx) Prepare(ctx context.Context, name, sql string) (*pgconn.StatementDescription, error) {
	return nil, nil
}

// Exec implements pgx.Tx.
func (m *mockTx) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

// Query implements pgx.Tx.
func (m *mockTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return &mockRows{}, nil
}

// QueryRow implements pgx.Tx.
func (m *mockTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return &mockRow{}
}

// Conn implements pgx.Tx.
func (m *mockTx) Conn() *pgx.Conn { return nil }

// mockRow is a stub implementation of pgx.Row interface that always returns pgx.ErrNoRows.
type mockRow struct{}

// Scan implements pgx.Row.
func (m *mockRow) Scan(dest ...any) error {
	return pgx.ErrNoRows
}

// mockRows is a stub implementation of pgx.Rows interface that has no rows.
type mockRows struct{}

// Close implements pgx.Rows.
func (m *mockRows) Close() {}

// Err implements pgx.Rows.
func (m *mockRows) Err() error { return nil }

// CommandTag implements pgx.Rows.
func (m *mockRows) CommandTag() pgconn.CommandTag { return pgconn.CommandTag{} }

// FieldDescriptions implements pgx.Rows.
func (m *mockRows) FieldDescriptions() []pgconn.FieldDescription { return nil }

// Next implements pgx.Rows.
func (m *mockRows) Next() bool { return false }

// Scan implements pgx.Rows.
func (m *mockRows) Scan(dest ...any) error { return nil }

// Values implements pgx.Rows.
func (m *mockRows) Values() ([]any, error) { return nil, nil }

// RawValues implements pgx.Rows.
func (m *mockRows) RawValues() [][]byte { return nil }

// Conn implements pgx.Rows.
func (m *mockRows) Conn() *pgx.Conn { return nil }

// mockBus is a stub implementation of bus.Bus interface used for in-memory message publishing.
type mockBus struct{}

// Publish implements bus.Bus.
func (b *mockBus) Publish(ctx context.Context, subject string, data []byte) error {
	return nil
}

// Status implements bus.Bus.
func (b *mockBus) Status() string {
	return "CONNECTED"
}

// Close implements bus.Bus.
func (b *mockBus) Close() {}

// TestAPIIntegration exercises the assembled Fiber application from internal/api/api.go
// via table-driven HTTP requests, verifying routing order precedence, error envelope formats,
// and authentication/role auth gates.
func TestAPIIntegration(t *testing.T) {
	jwtSecret := "integration-test-secret-key-123456"
	cfg := config.Config{
		JWTSecret:    jwtSecret,
		MaxBodyBytes: 1024 * 1024,
	}

	mockDB := &db.DB{Pool: &mockDBPool{}}
	busInstance := &mockBus{}
	buildInfo := handlers.BuildInfo{
		Version:   "1.0.0-test",
		Commit:    "abc1234",
		BuildTime: "2026-06-28",
	}

	app := api.New(cfg, api.Deps{DB: mockDB, Bus: busInstance}, buildInfo)

	// Helper to generate valid JWT tokens for test requests
	generateToken := func(role string) string {
		token, err := auth.IssueJWT(jwtSecret, uuid.New(), role, auth.WalletTypeEVM, "0x1234567890123456789012345678901234567890", 1*time.Hour)
		require.NoError(t, err)
		return "Bearer " + token
	}

	validUserToken := generateToken("user")
	validAdminToken := generateToken("admin")
	invalidToken := "Bearer invalid-token-sig"

	tests := []struct {
		name           string
		method         string
		path           string
		authHeader     string
		expectedStatus int
		verifyResponse func(t *testing.T, body []byte, headers http.Header)
	}{
		{
			name:           "Public endpoint: /health returns 200",
			method:         "GET",
			path:           "/health",
			expectedStatus: http.StatusOK,
			verifyResponse: func(t *testing.T, body []byte, headers http.Header) {
				var res map[string]any
				err := json.Unmarshal(body, &res)
				require.NoError(t, err)
				assert.Equal(t, true, res["ok"])
				assert.Equal(t, "grainlify-api", res["service"])
				assert.Equal(t, "1.0.0-test", res["version"])
			},
		},
		{
			name:           "Public endpoint: /projects returns 200 with empty projects list",
			method:         "GET",
			path:           "/projects",
			expectedStatus: http.StatusOK,
			verifyResponse: func(t *testing.T, body []byte, headers http.Header) {
				var res map[string]any
				err := json.Unmarshal(body, &res)
				require.NoError(t, err)
				assert.Contains(t, res, "projects")
			},
		},
		{
			name:           "Auth-required endpoint: /profile returns 401 with missing token",
			method:         "GET",
			path:           "/profile",
			authHeader:     "",
			expectedStatus: http.StatusUnauthorized,
			verifyResponse: func(t *testing.T, body []byte, headers http.Header) {
				var res map[string]any
				err := json.Unmarshal(body, &res)
				require.NoError(t, err)
				assert.Equal(t, "missing_bearer_token", res["error"])
				assert.NotEmpty(t, res["request_id"])
			},
		},
		{
			name:           "Auth-required endpoint: /profile returns 401 with invalid token",
			method:         "GET",
			path:           "/profile",
			authHeader:     invalidToken,
			expectedStatus: http.StatusUnauthorized,
			verifyResponse: func(t *testing.T, body []byte, headers http.Header) {
				var res map[string]any
				err := json.Unmarshal(body, &res)
				require.NoError(t, err)
				assert.Equal(t, "invalid_token", res["error"])
				assert.NotEmpty(t, res["request_id"])
			},
		},
		{
			name:           "Auth-required endpoint: /profile returns 200 with valid user token",
			method:         "GET",
			path:           "/profile",
			authHeader:     validUserToken,
			expectedStatus: http.StatusOK,
			verifyResponse: func(t *testing.T, body []byte, headers http.Header) {
				var res map[string]any
				err := json.Unmarshal(body, &res)
				require.NoError(t, err)
				assert.Equal(t, float64(0), res["contributions_count"])
				assert.Empty(t, res["languages"])
			},
		},
		{
			name:           "Admin endpoint: /admin/users returns 401 with missing token",
			method:         "GET",
			path:           "/admin/users",
			authHeader:     "",
			expectedStatus: http.StatusUnauthorized,
			verifyResponse: func(t *testing.T, body []byte, headers http.Header) {
				var res map[string]any
				err := json.Unmarshal(body, &res)
				require.NoError(t, err)
				assert.Equal(t, "missing_bearer_token", res["error"])
			},
		},
		{
			name:           "Admin endpoint: /admin/users returns 403 with user token (non-admin)",
			method:         "GET",
			path:           "/admin/users",
			authHeader:     validUserToken,
			expectedStatus: http.StatusForbidden,
			verifyResponse: func(t *testing.T, body []byte, headers http.Header) {
				var res map[string]any
				err := json.Unmarshal(body, &res)
				require.NoError(t, err)
				assert.Equal(t, "insufficient_role", res["error"])
			},
		},
		{
			name:           "Admin endpoint: /admin/users returns 200 with admin token",
			method:         "GET",
			path:           "/admin/users",
			authHeader:     validAdminToken,
			expectedStatus: http.StatusOK,
			verifyResponse: func(t *testing.T, body []byte, headers http.Header) {
				var res map[string]any
				err := json.Unmarshal(body, &res)
				require.NoError(t, err)
				assert.Contains(t, res, "users")
			},
		},
		{
			name:           "Route precedence: /projects/mine resolves before /projects/:id",
			method:         "GET",
			path:           "/projects/mine",
			authHeader:     validUserToken,
			expectedStatus: http.StatusOK,
			verifyResponse: func(t *testing.T, body []byte, headers http.Header) {
				// If it hit /projects/:id, it would parse "mine" as UUID and return 400 Bad Request.
				// Because /projects/mine is registered first, it resolves here returning 200 OK.
				var res []any
				err := json.Unmarshal(body, &res)
				require.NoError(t, err)
				assert.Empty(t, res)
			},
		},
		{
			name:           "Route precedence: /projects/pending-setup resolves before /projects/:id",
			method:         "GET",
			path:           "/projects/pending-setup",
			authHeader:     validUserToken,
			expectedStatus: http.StatusOK,
			verifyResponse: func(t *testing.T, body []byte, headers http.Header) {
				// If it hit /projects/:id, it would parse "pending-setup" as UUID and return 400 Bad Request.
				// Because /projects/pending-setup is registered first, it resolves here returning 200 OK.
				var res []any
				err := json.Unmarshal(body, &res)
				require.NoError(t, err)
				assert.Empty(t, res)
			},
		},
		{
			name:           "Route precedence check: /projects/:id resolves correctly for valid uuid",
			method:         "GET",
			path:           "/projects/00000000-0000-0000-0000-000000000000",
			expectedStatus: http.StatusNotFound,
			verifyResponse: func(t *testing.T, body []byte, headers http.Header) {
				var res map[string]any
				err := json.Unmarshal(body, &res)
				require.NoError(t, err)
				errObj, ok := res["error"].(map[string]any)
				require.True(t, ok, "expected nested error envelope, got %#v", res["error"])
				assert.Equal(t, "project_not_found", errObj["code"])
			},
		},
		{
			name:           "Error envelope format check: 404 unmatched route includes request_id and uses standard envelope",
			method:         "GET",
			path:           "/non-existent-route-path",
			expectedStatus: http.StatusNotFound,
			verifyResponse: func(t *testing.T, body []byte, headers http.Header) {
				var res map[string]any
				err := json.Unmarshal(body, &res)
				require.NoError(t, err)
				assert.Equal(t, "not_found", res["error"])
				assert.NotEmpty(t, res["request_id"])
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}

			resp, err := app.Test(req, 10000)
			require.NoError(t, err)
			defer resp.Body.Close()

			assert.Equal(t, tt.expectedStatus, resp.StatusCode)

			var bodyBytes []byte
			if resp.Body != nil {
				buf := new(bytes.Buffer)
				_, _ = buf.ReadFrom(resp.Body)
				bodyBytes = buf.Bytes()
			}

			if tt.verifyResponse != nil {
				tt.verifyResponse(t, bodyBytes, resp.Header)
			}
		})
	}
}
