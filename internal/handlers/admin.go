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
		if errors.Is(err, pgx.ErrNoRows) || ct.RowsAffected() == 0 {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "user_not_found"})
		}
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "role_update_failed"})
		}
		return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true})
	}
}

// BootstrapAdmin promotes the currently authenticated user to admin if they know the bootstrap token.
// This allows any authenticated user with the correct bootstrap token to become an admin.
//
// Rules:
// - Requires ADMIN_BOOTSTRAP_TOKEN header match (using SHA-256 and subtle.ConstantTimeCompare)
// - Requires the configured token to be at least 32 characters long, otherwise the endpoint is disabled
// - If user is already an admin, returns a fresh JWT token
// - Otherwise, promotes the user to admin and returns a fresh JWT with the updated role
// - Logs success/failure audit entries without the token values
func (h *AdminHandler) BootstrapAdmin() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		
		configToken := strings.TrimSpace(h.cfg.AdminBootstrapToken)
		if len(configToken) < 32 {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "bootstrap_not_configured"})
		}
		
		if h.cfg.JWTSecret == "" {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "jwt_not_configured"})
		}
		
		sub, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(sub)
		if err != nil {
			slog.Warn("admin bootstrap failed: invalid user id in context", "raw_user_id", sub)
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		headerToken := strings.TrimSpace(c.Get("X-Admin-Bootstrap-Token"))
		h1 := sha256.Sum256([]byte(headerToken))
		h2 := sha256.Sum256([]byte(configToken))
		if subtle.ConstantTimeCompare(h1[:], h2[:]) != 1 {
			slog.Warn("admin bootstrap failed: invalid bootstrap token", "user_id", userID.String())
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_bootstrap_token"})
		}

		var currentRole string
		if err := h.db.Pool.QueryRow(c.Context(), `SELECT role FROM users WHERE id = $1`, userID).Scan(&currentRole); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				slog.Warn("admin bootstrap failed: user not found in database", "user_id", userID.String())
				return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "user_not_found"})
			}
			slog.Error("admin bootstrap failed: database query error", "user_id", userID.String(), "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "bootstrap_failed"})
		}

		// If user is already an admin, no need to update
		if currentRole == "admin" {
			jwtToken, err := auth.IssueJWT(h.cfg.JWTSecret, userID, "admin", "", "", 60*time.Minute)
			if err != nil {
				slog.Error("admin bootstrap failed: token issue failed", "user_id", userID.String(), "error", err)
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "token_issue_failed"})
			}
			slog.Info("admin bootstrap succeeded: user is already admin", "user_id", userID.String())
			return c.Status(fiber.StatusOK).JSON(fiber.Map{
				"ok":    true,
				"token": jwtToken,
				"role":  "admin",
			})
		}

		// Promote user to admin if they have the correct bootstrap token
		_, err = h.db.Pool.Exec(c.Context(), `UPDATE users SET role = 'admin', updated_at = now() WHERE id = $1`, userID)
		if err != nil {
			slog.Error("admin bootstrap failed: database promotion failed", "user_id", userID.String(), "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "bootstrap_failed"})
		}

		jwtToken, err := auth.IssueJWT(h.cfg.JWTSecret, userID, "admin", "", "", 60*time.Minute)
		if err != nil {
			slog.Error("admin bootstrap failed: token issue failed after promotion", "user_id", userID.String(), "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "token_issue_failed"})
		}
		
		slog.Info("admin bootstrap succeeded: user promoted to admin", "user_id", userID.String())
		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"ok":    true,
			"token": jwtToken,
			"role":  "admin",
		})
	}
}




