package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func TestRequireAuthRejectsMissingMalformedAndInvalidTokens(t *testing.T) {
	secret := "test-secret"
	app := fiber.New()
	app.Get("/protected", RequireAuth(secret), func(c *fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	tests := []struct {
		name          string
		authorization string
	}{
		{name: "missing"},
		{name: "wrong scheme", authorization: "Basic abc"},
		{name: "empty bearer", authorization: "Bearer   "},
		{name: "invalid token", authorization: "Bearer not-a-token"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/protected", nil)
			if tt.authorization != "" {
				req.Header.Set("Authorization", tt.authorization)
			}

			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("app.Test returned error: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != fiber.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusUnauthorized)
			}
		})
	}
}

func TestRequireAuthSetsUserLocals(t *testing.T) {
	secret := "test-secret"
	userID := uuid.New()
	token, err := IssueJWT(secret, userID, "maintainer", WalletTypeEVM, "0xabc", time.Hour)
	if err != nil {
		t.Fatalf("IssueJWT returned error: %v", err)
	}

	app := fiber.New()
	app.Get("/protected", RequireAuth(secret), func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"user_id": c.Locals(LocalUserID),
			"role":    c.Locals(LocalRole),
		})
	})

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test returned error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusOK)
	}

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}
	if body["user_id"] != userID.String() {
		t.Fatalf("user_id = %q, want %q", body["user_id"], userID.String())
	}
	if body["role"] != "maintainer" {
		t.Fatalf("role = %q, want maintainer", body["role"])
	}
}

func TestRequireRoleAllowsOnlyConfiguredRoles(t *testing.T) {
	app := fiber.New()
	app.Get("/missing", RequireRole("admin"), func(c *fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})
	app.Get("/user", func(c *fiber.Ctx) error {
		c.Locals(LocalRole, "user")
		return RequireRole("admin")(c)
	}, func(c *fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})
	app.Get("/admin", func(c *fiber.Ctx) error {
		c.Locals(LocalRole, "admin")
		return RequireRole("admin")(c)
	}, func(c *fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	tests := []struct {
		path string
		want int
	}{
		{path: "/missing", want: fiber.StatusForbidden},
		{path: "/user", want: fiber.StatusForbidden},
		{path: "/admin", want: fiber.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			resp, err := app.Test(httptest.NewRequest(http.MethodGet, tt.path, nil))
			if err != nil {
				t.Fatalf("app.Test returned error: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tt.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.want)
			}
		})
	}
}

func TestRequireAuthAndRolePreserveAuthenticationAuthorizationStatusCodes(t *testing.T) {
	secret := "test-secret"
	userID := uuid.New()

	adminToken := mustSignTestClaims(t, secret, Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Role: "admin",
	})
	contributorToken := mustSignTestClaims(t, secret, Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Role: "contributor",
	})
	missingRoleToken := mustSignTestClaims(t, secret, Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	})
	expiredAdminToken := mustSignTestClaims(t, secret, Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Hour)),
		},
		Role: "admin",
	})

	app := fiber.New()
	// Mirrors admin-only route protection from internal/api/api.go: RequireAuth on the
	// /admin group followed by RequireRole("admin") on /admin/users.
	app.Get("/admin/users", RequireAuth(secret), RequireRole("admin"), func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"ok": true})
	})

	tests := []struct {
		name          string
		token         string
		wantStatus    int
		wantErrorCode string
	}{
		{
			name:       "correct admin role allowed",
			token:      adminToken,
			wantStatus: fiber.StatusOK,
		},
		{
			name:          "wrong authenticated role denied as forbidden",
			token:         contributorToken,
			wantStatus:    fiber.StatusForbidden,
			wantErrorCode: "insufficient_role",
		},
		{
			name:          "missing authenticated role claim denied as forbidden",
			token:         missingRoleToken,
			wantStatus:    fiber.StatusForbidden,
			wantErrorCode: "missing_role",
		},
		{
			name:          "unauthenticated request denied as unauthorized",
			wantStatus:    fiber.StatusUnauthorized,
			wantErrorCode: "missing_bearer_token",
		},
		{
			name:          "expired otherwise valid token denied as unauthorized",
			token:         expiredAdminToken,
			wantStatus:    fiber.StatusUnauthorized,
			wantErrorCode: "invalid_token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}

			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("app.Test returned error: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}

			if tt.wantErrorCode == "" {
				return
			}
			var body map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("failed to decode response body: %v", err)
			}
			if body["error"] != tt.wantErrorCode {
				t.Fatalf("error = %q, want %q", body["error"], tt.wantErrorCode)
			}
		})
	}
}

func mustSignTestClaims(t *testing.T, secret string, claims Claims) string {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("failed to sign test token: %v", err)
	}
	return token
}
