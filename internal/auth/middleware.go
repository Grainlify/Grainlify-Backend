package auth

import (
	"log/slog"
	"strings"

	"github.com/gofiber/fiber/v2"
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
				"error": "missing_bearer_token",
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
				"error": "missing_bearer_token",
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
				"error": "invalid_token",
			})
		}

		c.Locals(LocalUserID, claims.Subject)
		c.Locals(LocalRole, claims.Role)
		return c.Next()
	}
}
