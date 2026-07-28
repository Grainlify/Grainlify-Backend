package auth

import (
	"log/slog"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

const (
	LocalUserID = "user_id"
	LocalRole   = "role"
)

func RequireAuth(jwtSecret string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		h := strings.TrimSpace(c.Get("Authorization"))
		if h == "" || !strings.HasPrefix(strings.ToLower(h), "bearer ") {
			slog.Warn("auth middleware: missing or invalid Authorization header",
				"path", c.Path(),
				"method", c.Method(),
				"header_present", h != "",
				"header_prefix_ok", h != "" && strings.HasPrefix(strings.ToLower(h), "bearer "),
				"request_id", c.Locals("requestid"),
			)
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"error":      "missing_bearer_token",
				"request_id": c.Locals("requestid"),
			})
		}
		token := strings.TrimSpace(h[len("bearer "):])
		if token == "" {
			slog.Warn("auth middleware: empty token after 'bearer ' prefix",
				"path", c.Path(),
				"method", c.Method(),
				"request_id", c.Locals("requestid"),
			)
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"error":      "missing_bearer_token",
				"request_id": c.Locals("requestid"),
			})
		}
		claims, err := ParseJWT(jwtSecret, token)
		if err != nil {
			slog.Warn("auth middleware: JWT parse failed",
				"path", c.Path(),
				"method", c.Method(),
				"error", err,
				"token_length", len(token),
				"request_id", c.Locals("requestid"),
			)
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"error":      "invalid_token",
				"request_id": c.Locals("requestid"),
			})
		}

		c.Locals(LocalUserID, claims.Subject)
		c.Locals(LocalRole, claims.Role)
		return c.Next()
	}
}

func RequireRole(roles ...string) fiber.Handler {
	allowed := map[string]struct{}{}
	for _, r := range roles {
		allowed[r] = struct{}{}
	}
	return func(c *fiber.Ctx) error {
		role, _ := c.Locals(LocalRole).(string)
		if role == "" {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
				"error":      "missing_role",
				"request_id": c.Locals("requestid"),
			})
		}
		if _, ok := allowed[role]; !ok {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
				"error":      "insufficient_role",
				"request_id": c.Locals("requestid"),
			})
		}
		return c.Next()
	}
}

// RequireCurrentRole re-verifies the authenticated user's role against the
// live database instead of trusting the (possibly stale) role claim embedded
// in their JWT. It must run after RequireAuth has populated LocalUserID.
//
// RequireRole alone is a pure, DB-free claims check: once a token is issued
// it stays valid for whatever role it was minted with until it expires,
// even if the user's role in the database changes in the meantime (e.g. an
// admin gets demoted). For privilege-sensitive routes such as /admin/*, a
// demoted user's still-unexpired token must stop granting access
// immediately rather than up to a full token TTL later — so those routes
// use this DB-backed check instead of the plain claims-based RequireRole.
func RequireCurrentRole(pool db.DBPool, roles ...string) fiber.Handler {
	allowed := map[string]struct{}{}
	for _, r := range roles {
		allowed[r] = struct{}{}
	}
	return func(c *fiber.Ctx) error {
		forbidden := func() error {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
				"error":      "insufficient_role",
				"request_id": c.Locals("requestid"),
			})
		}

		userIDStr, _ := c.Locals(LocalUserID).(string)
		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
				"error":      "missing_role",
				"request_id": c.Locals("requestid"),
			})
		}

		if pool == nil {
			return forbidden()
		}

		var currentRole string
		if err := pool.QueryRow(c.Context(), `SELECT role FROM users WHERE id = $1`, userID).Scan(&currentRole); err != nil {
			slog.Warn("auth middleware: failed to load current role for authorization check",
				"path", c.Path(),
				"method", c.Method(),
				"user_id", userID.String(),
				"error", err,
				"request_id", c.Locals("requestid"),
			)
			return forbidden()
		}

		if _, ok := allowed[currentRole]; !ok {
			return forbidden()
		}

		// Keep LocalRole consistent with the authoritative (DB) value for
		// any downstream handler that reads it.
		c.Locals(LocalRole, currentRole)
		return c.Next()
	}
}









