package handlers_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/handlers"
)

// authHandlerTestJWTSecret is the HMAC secret used to sign/verify tokens
// throughout this file's tests.
const authHandlerTestJWTSecret = "auth-handler-test-secret-do-not-use-in-prod"

// authHandlerTestWalletAddress returns a unique-enough fake address so
// repeated runs against a persistent Postgres don't collide on the
// wallets(wallet_type, address) unique constraint.
func authHandlerTestWalletAddress() string {
	return "0x" + uuid.NewString()
}

// authHandlerCreateUser drives the same nonce-create/consume flow the real
// POST /auth/verify endpoint uses to provision a user, without needing an
// actual wallet signature: ConsumeNonceAndUpsertUser itself does not check
// signatures -- that happens one layer up, in AuthHandler.Verify, via
// auth.VerifySignature.
func authHandlerCreateUser(t *testing.T, d *db.DB) auth.VerifyResult {
	t.Helper()
	ctx := context.Background()
	address := authHandlerTestWalletAddress()

	n, err := auth.CreateNonce(ctx, d.Pool, auth.WalletTypeEVM, address, 10*time.Minute)
	if err != nil {
		t.Fatalf("CreateNonce: %v", err)
	}
	res, err := auth.ConsumeNonceAndUpsertUser(ctx, d.Pool, auth.WalletTypeEVM, address, n.Nonce, "")
	if err != nil {
		t.Fatalf("ConsumeNonceAndUpsertUser: %v", err)
	}
	return res
}

func TestAuthHandler_Me(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: authHandlerTestJWTSecret}
	h := handlers.NewAuthHandler(cfg, d)

	app := fiber.New()
	app.Get("/me", auth.RequireAuth(cfg.JWTSecret), h.Me())

	t.Run("missing auth header returns 401", func(t *testing.T) {
		resp, err := app.Test(httptest.NewRequest("GET", "/me", nil))
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want %d", resp.StatusCode, fiber.StatusUnauthorized)
		}
	})

	t.Run("invalid token returns 401", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/me", nil)
		req.Header.Set("Authorization", "Bearer not-a-real-token")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want %d", resp.StatusCode, fiber.StatusUnauthorized)
		}
	})

	t.Run("valid token returns the user's profile", func(t *testing.T) {
		res := authHandlerCreateUser(t, d)
		token, err := auth.IssueJWT(cfg.JWTSecret, res.User.ID, res.User.Role, res.Wallet.WalletType, res.Wallet.Address, time.Hour)
		if err != nil {
			t.Fatalf("IssueJWT: %v", err)
		}

		req := httptest.NewRequest("GET", "/me", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusOK)
		}

		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if body["id"] != res.User.ID.String() {
			t.Errorf("id = %v, want %v", body["id"], res.User.ID.String())
		}
		if body["role"] != res.User.Role {
			t.Errorf("role = %v, want %v", body["role"], res.User.Role)
		}
		if _, present := body["github"]; present {
			t.Errorf("github = %v, want absent for a user with no linked GitHub account", body["github"])
		}
	})
}

func TestAuthHandler_ResyncGitHubProfile(t *testing.T) {
	d := testDB(t)
	cfg := config.Config{JWTSecret: authHandlerTestJWTSecret}
	h := handlers.NewAuthHandler(cfg, d)

	app := fiber.New()
	app.Post("/me/github/resync", auth.RequireAuth(cfg.JWTSecret), h.ResyncGitHubProfile())

	t.Run("missing auth header returns 401", func(t *testing.T) {
		resp, err := app.Test(httptest.NewRequest("POST", "/me/github/resync", nil))
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want %d", resp.StatusCode, fiber.StatusUnauthorized)
		}
	})

	t.Run("valid auth without a linked github account returns 404", func(t *testing.T) {
		res := authHandlerCreateUser(t, d)
		token, err := auth.IssueJWT(cfg.JWTSecret, res.User.ID, res.User.Role, res.Wallet.WalletType, res.Wallet.Address, time.Hour)
		if err != nil {
			t.Fatalf("IssueJWT: %v", err)
		}

		req := httptest.NewRequest("POST", "/me/github/resync", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != fiber.StatusNotFound {
			t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusNotFound)
		}

		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if body["error"] != "github_not_linked" {
			t.Errorf("error = %v, want github_not_linked", body["error"])
		}
	})
}
