package auth

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// mwTestJWTSecret is the HMAC secret used to sign/verify tokens throughout
// this file's tests.
const mwTestJWTSecret = "middleware-test-secret-do-not-use-in-prod"

// mwSignClaims signs the given Claims with HS256 using secret. Used to
// hand-craft tokens (e.g. already-expired ones) that IssueJWT can't produce
// directly since IssueJWT clamps non-positive TTLs to a default.
func mwSignClaims(t *testing.T, secret string, claims Claims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign claims: %v", err)
	}
	return signed
}

// mwEchoLocalsHandler is a downstream handler used to observe what
// RequireAuth / RequireRole stored in c.Locals.
func mwEchoLocalsHandler(c *fiber.Ctx) error {
	uid, _ := c.Locals(LocalUserID).(string)
	role, _ := c.Locals(LocalRole).(string)
	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"user_id": uid,
		"role":    role,
	})
}

func TestRequireAuth_MissingOrMalformedHeader(t *testing.T) {
	cases := []struct {
		name      string
		header    string
		setHeader bool
	}{
		{name: "no header at all", setHeader: false},
		{name: "empty header value", header: "", setHeader: true},
		{name: "wrong scheme", header: "Token abc.def.ghi", setHeader: true},
		{name: "bearer with no space or token", header: "Bearer", setHeader: true},
		{name: "bearer with only trailing whitespace", header: "Bearer    ", setHeader: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := fiber.New()
			app.Get("/x", RequireAuth(mwTestJWTSecret), mwEchoLocalsHandler)

			req := httptest.NewRequest("GET", "/x", nil)
			if tc.setHeader {
				req.Header.Set("Authorization", tc.header)
			}
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != fiber.StatusUnauthorized {
				t.Errorf("status = %d, want %d", resp.StatusCode, fiber.StatusUnauthorized)
			}
			var body map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if body["error"] != "missing_bearer_token" {
				t.Errorf("error = %v, want missing_bearer_token", body["error"])
			}
		})
	}
}

func TestRequireAuth_GarbageJWT(t *testing.T) {
	app := fiber.New()
	app.Get("/x", RequireAuth(mwTestJWTSecret), mwEchoLocalsHandler)

	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", "Bearer this.is-not.a-real-jwt")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, fiber.StatusUnauthorized)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["error"] != "invalid_token" {
		t.Errorf("error = %v, want invalid_token", body["error"])
	}
}

func TestRequireAuth_ExpiredJWT(t *testing.T) {
	now := time.Now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   uuid.NewString(),
			IssuedAt:  jwt.NewNumericDate(now.Add(-2 * time.Hour)),
			ExpiresAt: jwt.NewNumericDate(now.Add(-1 * time.Hour)),
		},
		Role: "contributor",
	}
	token := mwSignClaims(t, mwTestJWTSecret, claims)

	app := fiber.New()
	app.Get("/x", RequireAuth(mwTestJWTSecret), mwEchoLocalsHandler)

	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, fiber.StatusUnauthorized)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["error"] != "invalid_token" {
		t.Errorf("error = %v, want invalid_token", body["error"])
	}
}

func TestRequireAuth_ValidJWT(t *testing.T) {
	userID := uuid.New()
	token, err := IssueJWT(mwTestJWTSecret, userID, "maintainer", WalletTypeEVM, "0xabc", time.Hour)
	if err != nil {
		t.Fatalf("IssueJWT: %v", err)
	}

	app := fiber.New()
	app.Get("/x", RequireAuth(mwTestJWTSecret), mwEchoLocalsHandler)

	req := httptest.NewRequest("GET", "/x", nil)
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
	// Assert the middleware actually populated c.Locals with the claims'
	// Subject (as LocalUserID) and Role (as LocalRole).
	if body["user_id"] != userID.String() {
		t.Errorf("user_id = %v, want %v", body["user_id"], userID.String())
	}
	if body["role"] != "maintainer" {
		t.Errorf("role = %v, want maintainer", body["role"])
	}
}

func TestRequireAuth_ValidJWT_LowercaseBearerScheme(t *testing.T) {
	userID := uuid.New()
	token, err := IssueJWT(mwTestJWTSecret, userID, "contributor", WalletTypeEVM, "0xabc", time.Hour)
	if err != nil {
		t.Fatalf("IssueJWT: %v", err)
	}

	app := fiber.New()
	app.Get("/x", RequireAuth(mwTestJWTSecret), mwEchoLocalsHandler)

	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", "bearer "+token) // lowercase scheme should still work
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusOK)
	}
}

func TestRequireRole(t *testing.T) {
	// seedRole mounts a fake upstream handler that seeds c.Locals(LocalRole)
	// the way RequireAuth would, without needing a real JWT -- RequireRole
	// only ever reads from Locals.
	seedRole := func(role string) fiber.Handler {
		return func(c *fiber.Ctx) error {
			if role != "" {
				c.Locals(LocalRole, role)
			}
			return c.Next()
		}
	}

	t.Run("role mismatch returns 403", func(t *testing.T) {
		app := fiber.New()
		app.Get("/admin", seedRole("contributor"), RequireRole("admin"), mwEchoLocalsHandler)

		resp, err := app.Test(httptest.NewRequest("GET", "/admin", nil))
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != fiber.StatusForbidden {
			t.Errorf("status = %d, want %d", resp.StatusCode, fiber.StatusForbidden)
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if body["error"] != "insufficient_role" {
			t.Errorf("error = %v, want insufficient_role", body["error"])
		}
	})

	t.Run("missing role returns 403", func(t *testing.T) {
		app := fiber.New()
		app.Get("/admin", seedRole(""), RequireRole("admin"), mwEchoLocalsHandler)

		resp, err := app.Test(httptest.NewRequest("GET", "/admin", nil))
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != fiber.StatusForbidden {
			t.Errorf("status = %d, want %d", resp.StatusCode, fiber.StatusForbidden)
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if body["error"] != "missing_role" {
			t.Errorf("error = %v, want missing_role", body["error"])
		}
	})

	t.Run("role match reaches next handler", func(t *testing.T) {
		app := fiber.New()
		app.Get("/admin", seedRole("admin"), RequireRole("admin"), mwEchoLocalsHandler)

		resp, err := app.Test(httptest.NewRequest("GET", "/admin", nil))
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != fiber.StatusOK {
			t.Errorf("status = %d, want %d", resp.StatusCode, fiber.StatusOK)
		}
	})

	t.Run("multiple allowed roles, one matches", func(t *testing.T) {
		app := fiber.New()
		app.Get("/staff", seedRole("maintainer"), RequireRole("admin", "maintainer"), mwEchoLocalsHandler)

		resp, err := app.Test(httptest.NewRequest("GET", "/staff", nil))
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != fiber.StatusOK {
			t.Errorf("status = %d, want %d", resp.StatusCode, fiber.StatusOK)
		}
	})

	t.Run("multiple allowed roles, none match", func(t *testing.T) {
		app := fiber.New()
		app.Get("/staff", seedRole("contributor"), RequireRole("admin", "maintainer"), mwEchoLocalsHandler)

		resp, err := app.Test(httptest.NewRequest("GET", "/staff", nil))
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != fiber.StatusForbidden {
			t.Errorf("status = %d, want %d", resp.StatusCode, fiber.StatusForbidden)
		}
	})
}
