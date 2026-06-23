package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jagadeesh/grainlify/backend/internal/api"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/migrate"
	"github.com/stretchr/testify/assert"
)

func getTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DB_URL")
	if dsn == "" {
		// Create a dummy pool that won't connect but compiles and behaves as a valid struct pointer
		ctx := context.Background()
		pool, err := pgxpool.New(ctx, "postgres://localhost:5432/non_existent_db")
		if err != nil {
			t.Fatalf("failed to create dummy pool: %v", err)
		}
		return pool
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}

	// Apply migrations
	if err := migrate.Up(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("migrate.Up: %v", err)
	}

	t.Cleanup(pool.Close)
	return pool
}

func TestAuthRoutesRegistered(t *testing.T) {
	cfg := config.Config{JWTSecret: "secret"}
	app := api.New(cfg, api.Deps{DB: &db.DB{}})

	routes := app.GetRoutes()
	hasNonce := false
	hasVerify := false
	for _, r := range routes {
		if r.Method == "POST" && r.Path == "/auth/nonce" {
			hasNonce = true
		}
		if r.Method == "POST" && r.Path == "/auth/verify" {
			hasVerify = true
		}
	}
	assert.True(t, hasNonce, "POST /auth/nonce route should be registered")
	assert.True(t, hasVerify, "POST /auth/verify route should be registered")
}

func TestNonceValidation(t *testing.T) {
	pool := getTestPool(t)
	cfg := config.Config{JWTSecret: "secret"}
	app := api.New(cfg, api.Deps{DB: &db.DB{Pool: pool}})

	tests := []struct {
		name       string
		reqBody    map[string]any
		wantStatus int
		wantError  string
	}{
		{
			name: "Valid EVM address (should pass validation)",
			reqBody: map[string]any{
				"wallet_type": "evm",
				"address":     "0xabcdefabcdef1234567890123456789012345678",
			},
			// If DB connection fails it returns 500, but NOT 400 (validation error)
			wantStatus: http.StatusOK, // or 500 if DB not running
		},
		{
			name: "Valid Stellar address (should pass validation)",
			reqBody: map[string]any{
				"wallet_type": "stellar_ed25519",
				"address":     "GBXQTRFRPQLBNDNCD7SWHC26N6N5YZ23J25E34N1E2X4Q3X2E4C2C2C2",
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "Missing wallet_type",
			reqBody: map[string]any{
				"address": "0xabcdefabcdef1234567890123456789012345678",
			},
			wantStatus: http.StatusBadRequest,
			wantError:  "invalid_wallet_type",
		},
		{
			name: "Invalid wallet_type",
			reqBody: map[string]any{
				"wallet_type": "bitcoin",
				"address":     "0xabcdefabcdef1234567890123456789012345678",
			},
			wantStatus: http.StatusBadRequest,
			wantError:  "invalid_wallet_type",
		},
		{
			name: "Oversized wallet_type",
			reqBody: map[string]any{
				"wallet_type": strings.Repeat("a", 60),
				"address":     "0xabcdefabcdef1234567890123456789012345678",
			},
			wantStatus: http.StatusBadRequest,
			wantError:  "invalid_wallet_type",
		},
		{
			name: "Missing address",
			reqBody: map[string]any{
				"wallet_type": "evm",
			},
			wantStatus: http.StatusBadRequest,
			wantError:  "invalid_address",
		},
		{
			name: "Oversized address",
			reqBody: map[string]any{
				"wallet_type": "evm",
				"address":     strings.Repeat("a", 130),
			},
			wantStatus: http.StatusBadRequest,
			wantError:  "invalid_address",
		},
		{
			name: "Malformed EVM address - not hex",
			reqBody: map[string]any{
				"wallet_type": "evm",
				"address":     "0xabcdefabcdef123456789012345678901234567g",
			},
			wantStatus: http.StatusBadRequest,
			wantError:  "invalid_address",
		},
		{
			name: "Malformed EVM address - wrong length",
			reqBody: map[string]any{
				"wallet_type": "evm",
				"address":     "0xabc",
			},
			wantStatus: http.StatusBadRequest,
			wantError:  "invalid_address",
		},
		{
			name: "Malformed Stellar address - invalid character",
			reqBody: map[string]any{
				"wallet_type": "stellar_ed25519",
				"address":     "GBXQTRFRPQLBNDNCD7SWHC26N6N5YZ23J25E34N1E2X4Q3X2E4C2C2C2$",
			},
			wantStatus: http.StatusBadRequest,
			wantError:  "invalid_address",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := json.Marshal(tt.reqBody)
			assert.NoError(t, err)

			req := httptest.NewRequest(http.MethodPost, "/auth/nonce", bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")

			resp, err := app.Test(req)
			assert.NoError(t, err)
			defer resp.Body.Close()

			if tt.wantStatus == http.StatusOK {
				// If TEST_DB_URL is not set, a dummy pool will result in 500 Internal Server Error,
				// which means it passed early handler input validation.
				if os.Getenv("TEST_DB_URL") == "" {
					assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
					var body map[string]string
					err := json.NewDecoder(resp.Body).Decode(&body)
					assert.NoError(t, err)
					assert.Equal(t, "nonce_create_failed", body["error"])
				} else {
					assert.Equal(t, http.StatusOK, resp.StatusCode)
				}
			} else {
				assert.Equal(t, tt.wantStatus, resp.StatusCode)
				var body map[string]string
				err := json.NewDecoder(resp.Body).Decode(&body)
				assert.NoError(t, err)
				assert.Equal(t, tt.wantError, body["error"])
			}
		})
	}
}

func TestVerifyValidation(t *testing.T) {
	pool := getTestPool(t)
	cfg := config.Config{JWTSecret: "secret"}
	app := api.New(cfg, api.Deps{DB: &db.DB{Pool: pool}})

	tests := []struct {
		name       string
		reqBody    map[string]any
		wantStatus int
		wantError  string
	}{
		{
			name: "Missing nonce or signature",
			reqBody: map[string]any{
				"wallet_type": "evm",
				"address":     "0xabcdefabcdef1234567890123456789012345678",
			},
			wantStatus: http.StatusBadRequest,
			wantError:  "missing_nonce_or_signature",
		},
		{
			name: "Oversized nonce",
			reqBody: map[string]any{
				"wallet_type": "evm",
				"address":     "0xabcdefabcdef1234567890123456789012345678",
				"nonce":       strings.Repeat("a", 130),
				"signature":   "0x" + strings.Repeat("a", 130),
			},
			wantStatus: http.StatusBadRequest,
			wantError:  "invalid_nonce",
		},
		{
			name: "Malformed nonce",
			reqBody: map[string]any{
				"wallet_type": "evm",
				"address":     "0xabcdefabcdef1234567890123456789012345678",
				"nonce":       "nonce-123$",
				"signature":   "0x" + strings.Repeat("a", 130),
			},
			wantStatus: http.StatusBadRequest,
			wantError:  "invalid_nonce",
		},
		{
			name: "Oversized signature",
			reqBody: map[string]any{
				"wallet_type": "evm",
				"address":     "0xabcdefabcdef1234567890123456789012345678",
				"nonce":       "nonce-123",
				"signature":   "0x" + strings.Repeat("a", 260),
			},
			wantStatus: http.StatusBadRequest,
			wantError:  "invalid_signature",
		},
		{
			name: "Malformed EVM signature - wrong length",
			reqBody: map[string]any{
				"wallet_type": "evm",
				"address":     "0xabcdefabcdef1234567890123456789012345678",
				"nonce":       "nonce-123",
				"signature":   "0xabcdef",
			},
			wantStatus: http.StatusBadRequest,
			wantError:  "invalid_signature",
		},
		{
			name: "Malformed EVM signature - not hex",
			reqBody: map[string]any{
				"wallet_type": "evm",
				"address":     "0xabcdefabcdef1234567890123456789012345678",
				"nonce":       "nonce-123",
				"signature":   "0x" + strings.Repeat("g", 130),
			},
			wantStatus: http.StatusBadRequest,
			wantError:  "invalid_signature",
		},
		{
			name: "Stellar Ed25519 missing Public Key",
			reqBody: map[string]any{
				"wallet_type": "stellar_ed25519",
				"address":     "GBXQTRFRPQLBNDNCD7SWHC26N6N5YZ23J25E34N1E2X4Q3X2E4C2C2C2",
				"nonce":       "nonce-123",
				"signature":   "0x" + strings.Repeat("a", 128),
			},
			wantStatus: http.StatusBadRequest,
			wantError:  "invalid_public_key",
		},
		{
			name: "Stellar Ed25519 oversized Public Key",
			reqBody: map[string]any{
				"wallet_type": "stellar_ed25519",
				"address":     "GBXQTRFRPQLBNDNCD7SWHC26N6N5YZ23J25E34N1E2X4Q3X2E4C2C2C2",
				"nonce":       "nonce-123",
				"signature":   "0x" + strings.Repeat("a", 128),
				"public_key":  "0x" + strings.Repeat("a", 260),
			},
			wantStatus: http.StatusBadRequest,
			wantError:  "invalid_public_key",
		},
		{
			name: "Stellar Ed25519 malformed Public Key - wrong length",
			reqBody: map[string]any{
				"wallet_type": "stellar_ed25519",
				"address":     "GBXQTRFRPQLBNDNCD7SWHC26N6N5YZ23J25E34N1E2X4Q3X2E4C2C2C2",
				"nonce":       "nonce-123",
				"signature":   "0x" + strings.Repeat("a", 128),
				"public_key":  "0xabcdef",
			},
			wantStatus: http.StatusBadRequest,
			wantError:  "invalid_public_key",
		},
		{
			name: "Stellar Ed25519 malformed Public Key - not hex",
			reqBody: map[string]any{
				"wallet_type": "stellar_ed25519",
				"address":     "GBXQTRFRPQLBNDNCD7SWHC26N6N5YZ23J25E34N1E2X4Q3X2E4C2C2C2",
				"nonce":       "nonce-123",
				"signature":   "0x" + strings.Repeat("a", 128),
				"public_key":  "0x" + strings.Repeat("g", 64),
			},
			wantStatus: http.StatusBadRequest,
			wantError:  "invalid_public_key",
		},
		{
			name: "EVM with invalid optional Public Key",
			reqBody: map[string]any{
				"wallet_type": "evm",
				"address":     "0xabcdefabcdef1234567890123456789012345678",
				"nonce":       "nonce-123",
				"signature":   "0x" + strings.Repeat("a", 130),
				"public_key":  "invalid-chars$",
			},
			wantStatus: http.StatusBadRequest,
			wantError:  "invalid_public_key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := json.Marshal(tt.reqBody)
			assert.NoError(t, err)

			req := httptest.NewRequest(http.MethodPost, "/auth/verify", bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")

			resp, err := app.Test(req)
			assert.NoError(t, err)
			defer resp.Body.Close()

			assert.Equal(t, tt.wantStatus, resp.StatusCode)
			var body map[string]string
			err = json.NewDecoder(resp.Body).Decode(&body)
			assert.NoError(t, err)
			assert.Equal(t, tt.wantError, body["error"])
		})
	}
}
