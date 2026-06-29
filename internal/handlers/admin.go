package handlers

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
)

type AdminHandler struct {
	cfg config.Config
	db  *db.DB
}

func NewAdminHandler(cfg config.Config, d *db.DB) *AdminHandler {
	return &AdminHandler{cfg: cfg, db: d}
}

// ListUsers returns users ordered by creation date, newest first.
//
// Query parameters:
//   - limit: max results (default 50, max 200)
//   - offset: pagination offset (default 0)
//
// The response keeps the backward-compatible "users" array and adds
// limit/offset/total metadata consistent with the other paginated endpoints.
func (h *AdminHandler) ListUsers() fiber.Handler {
	return func(c *fiber.Ctx) error {
		p, err := ParsePagination(c, 50, 200)
		if err != nil {
			// response already written by ParsePagination on error
			return nil
		}

		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}

		rows, err := h.db.Pool.Query(c.Context(), `
SELECT id, role, github_user_id, created_at, updated_at
FROM users
ORDER BY created_at DESC
LIMIT $1 OFFSET $2
`, p.Limit, p.Offset)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "users_list_failed"})
		}
		defer rows.Close()

		var out []fiber.Map
		for rows.Next() {
			var id uuid.UUID
			var role string
			var ghID *int64
			var createdAt, updatedAt time.Time
			if err := rows.Scan(&id, &role, &ghID, &createdAt, &updatedAt); err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "users_list_failed"})
			}
			out = append(out, fiber.Map{
				"id":             id.String(),
				"role":           role,
				"github_user_id": ghID,
				"created_at":     createdAt,
				"updated_at":     updatedAt,
			})
		}

		var total int
		if err := h.db.Pool.QueryRow(c.Context(), `SELECT COUNT(*) FROM users`).Scan(&total); err != nil {
			total = len(out)
		}

		return c.Status(fiber.StatusOK).JSON(PaginatedResponse("users", out, p, total))
	}
}

type setRoleRequest struct {
	Role string `json:"role"`
}

func (h *AdminHandler) SetUserRole() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		userID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_user_id"})
		}
		var req setRoleRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_json"})
		}
		role := strings.TrimSpace(req.Role)
		if role != "contributor" && role != "maintainer" && role != "admin" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_role"})
		}
		ct, err := h.db.Pool.Exec(c.Context(), `
UPDATE users SET role = $2, updated_at = now()
WHERE id = $1
`, userID, role)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "user_not_found"})
			}
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "role_update_failed"})
		}
		if ct.RowsAffected() == 0 {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "user_not_found"})
		}
		return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true})
	}
}

// secureCompare compares two strings in constant time using SHA-256 hashing.
// Hashing the inputs first ensures that the comparison is done on fixed-length buffers,
// preventing timing attacks on string length and preventing panics due to length mismatch.
// It returns 1 if the strings are equal, and 0 otherwise.
func secureCompare(a, b string) int {
	aHash := sha256.Sum256([]byte(a))
	bHash := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(aHash[:], bHash[:])
}

// BootstrapAdmin promotes the currently authenticated user to admin if they know the bootstrap token.
// This allows any authenticated user with the correct bootstrap token to become an admin.
//
// Rules:
// - Requires ADMIN_BOOTSTRAP_TOKEN header match
// - If user is already an admin, returns a fresh JWT token
// - Otherwise, promotes the user to admin and returns a fresh JWT with the updated role
func (h *AdminHandler) BootstrapAdmin() fiber.Handler {
	return func(c *fiber.Ctx) error {
		// Optionally restrict bootstrap to APP_ENV=dev
		if h.cfg.Env != "dev" {
			slog.Warn("Admin bootstrap blocked: endpoint is only allowed in dev environment", "env", h.cfg.Env)
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "bootstrap_disabled_in_env"})
		}

		// Reject bootstrap if configured token is empty (disabling the endpoint)
		if h.cfg.AdminBootstrapToken == "" {
			slog.Warn("Admin bootstrap blocked: bootstrap token is empty (endpoint disabled)")
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "bootstrap_not_configured"})
		}

		// Reject bootstrap if configured token is shorter than 32 characters
		const minTokenLen = 32
		if len(h.cfg.AdminBootstrapToken) < minTokenLen {
			slog.Warn("Admin bootstrap blocked: configured bootstrap token is too short/weak", "length", len(h.cfg.AdminBootstrapToken))
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "bootstrap_token_too_weak"})
		}

		if h.cfg.JWTSecret == "" {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "jwt_not_configured"})
		}

		sub, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(sub)
		if err != nil {
			slog.Warn("Admin bootstrap failed: invalid user ID in context", "raw_user_id", sub)
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		headerToken := strings.TrimSpace(c.Get("X-Admin-Bootstrap-Token"))
		configToken := strings.TrimSpace(h.cfg.AdminBootstrapToken)

		if secureCompare(headerToken, configToken) != 1 {
			slog.Warn("Admin bootstrap failed: token mismatch", "user_id", userID)
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "invalid_bootstrap_token"})
		}

		// DB configured check (only needed for DB operations)
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}

		var currentRole string
		if err := h.db.Pool.QueryRow(c.Context(), `SELECT role FROM users WHERE id = $1`, userID).Scan(&currentRole); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				slog.Warn("Admin bootstrap failed: user not found in database", "user_id", userID)
				return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "user_not_found"})
			}
			slog.Error("Admin bootstrap failed: database error checking user", "user_id", userID, "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "bootstrap_failed"})
		}

		// If user is already an admin, no need to update
		if currentRole == "admin" {
			jwtToken, err := auth.IssueJWT(h.cfg.JWTSecret, userID, "admin", "", "", 60*time.Minute)
			if err != nil {
				slog.Error("Admin bootstrap failed: JWT issuance failed for existing admin", "user_id", userID, "error", err)
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "token_issue_failed"})
			}
			slog.Info("Admin bootstrap successful: user already admin", "user_id", userID)
			return c.Status(fiber.StatusOK).JSON(fiber.Map{
				"ok":    true,
				"token": jwtToken,
				"role":  "admin",
			})
		}

		// Promote user to admin if they have the correct bootstrap token
		_, err = h.db.Pool.Exec(c.Context(), `UPDATE users SET role = 'admin', updated_at = now() WHERE id = $1`, userID)
		if err != nil {
			slog.Error("Admin bootstrap failed: database error updating user role", "user_id", userID, "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "bootstrap_failed"})
		}

		jwtToken, err := auth.IssueJWT(h.cfg.JWTSecret, userID, "admin", "", "", 60*time.Minute)
		if err != nil {
			slog.Error("Admin bootstrap failed: JWT issuance failed after promotion", "user_id", userID, "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "token_issue_failed"})
		}

		slog.Info("Admin bootstrap successful: user promoted to admin", "user_id", userID)
		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"ok":    true,
			"token": jwtToken,
			"role":  "admin",
		})
	}
}




