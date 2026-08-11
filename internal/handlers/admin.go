package handlers

import (
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

func (h *AdminHandler) ListUsers() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}

		rows, err := h.db.Pool.Query(c.Context(), `
SELECT id, role, github_user_id, created_at, updated_at
FROM users
ORDER BY created_at DESC
LIMIT 50
`)
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

		return c.Status(fiber.StatusOK).JSON(fiber.Map{"users": out})
	}
}

type setRoleRequest struct {
	Role string `json:"role"`
	// Optional context, recorded on the audit row. Not required: a role change
	// with no stated reason is still far better attributed than the bare
	// UPDATE this replaced.
	Reason string `json:"reason"`
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
		actorID, ok := adminID(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		// Read the old role in the same statement that sets the new one, so
		// the recorded before/after pair cannot be a stale read racing another
		// change. "Alice promoted Bob from contributor to admin" is the useful
		// fact; "Alice changed Bob's role" is not, and a demotion matters as
		// much as a promotion.
		var oldRole string
		err = h.db.Pool.QueryRow(c.Context(), `
UPDATE users SET role = $2, updated_at = now()
WHERE id = $1
RETURNING (SELECT role FROM users WHERE id = $1)
`, userID, role).Scan(&oldRole)
		if errors.Is(err, pgx.ErrNoRows) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "user_not_found"})
		}
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "role_update_failed"})
		}

		if oldRole != role {
			slog.Warn("admin role change",
				"actor_user_id", actorID, "subject_user_id", userID,
				"old_role", oldRole, "new_role", role)
			h.recordRoleAudit(c, userID, oldRole, role, "admin_action", actorID, strings.TrimSpace(req.Reason))
		}
		return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true, "old_role": oldRole, "new_role": role})
	}
}

// BootstrapAdmin grants the FIRST admin on a fresh install, and nothing else.
//
// It used to promote any authenticated user who presented the shared
// ADMIN_BOOTSTRAP_TOKEN, permanently, with no approval and no record. That is
// a self-service admin grant: one environment secret, no rotation, no
// revocation, readable by anyone with deploy access, and it survived rotating
// the token because the role landed on the user row.
//
// Now it refuses as soon as any admin exists. After that point every further
// admin is granted by an existing admin through SetUserRole, which is
// attributed. Recovering a lost sole-admin account is a database write by
// whoever has deploy access - deliberately not a second permanent credential
// existing only for that case.
//
// Both outcomes are recorded. A refusal means somebody holds the token and
// tried to use it, which is worth knowing. A success is the origin record for
// the first admin - without it, the very first grant is the one action nobody
// can trace.
func (h *AdminHandler) BootstrapAdmin() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		if h.cfg.AdminBootstrapToken == "" {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "bootstrap_not_configured"})
		}
		if h.cfg.JWTSecret == "" {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "jwt_not_configured"})
		}
		sub, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(sub)
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		headerToken := strings.TrimSpace(c.Get("X-Admin-Bootstrap-Token"))
		configToken := strings.TrimSpace(h.cfg.AdminBootstrapToken)
		if subtle.ConstantTimeCompare([]byte(headerToken), []byte(configToken)) != 1 {
			slog.Warn("admin bootstrap: rejected, bad token", "user_id", userID, "remote_ip", c.IP())
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_bootstrap_token"})
		}

		// The gate. Once anybody holds admin, bootstrap is closed for good and
		// further grants go through an admin's explicit, attributed decision.
		var adminCount int
		if err := h.db.Pool.QueryRow(c.Context(),
			`SELECT count(*)::int FROM users WHERE role = 'admin'`).Scan(&adminCount); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "bootstrap_failed"})
		}
		if adminCount > 0 {
			// A correct token presented after bootstrap has closed means
			// somebody has the shared secret and tried to use it. That is the
			// signal worth having, so it is logged loudly and recorded.
			slog.Warn("admin bootstrap: refused, an admin already exists",
				"user_id", userID, "remote_ip", c.IP(), "existing_admins", adminCount)
			h.recordRoleAudit(c, userID, "", "", "bootstrap", userID,
				"refused: bootstrap is closed because an admin already exists")
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
				"error":   "bootstrap_closed",
				"message": "An admin already exists. Ask an existing admin to grant your access.",
			})
		}

		var currentRole string
		if err := h.db.Pool.QueryRow(c.Context(), `SELECT role FROM users WHERE id = $1`, userID).Scan(&currentRole); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "user_not_found"})
			}
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "bootstrap_failed"})
		}

		if _, err := h.db.Pool.Exec(c.Context(),
			`UPDATE users SET role = 'admin', updated_at = now() WHERE id = $1`, userID); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "bootstrap_failed"})
		}

		// The origin record for the first admin. actor == subject, because
		// there was nobody else to authorise it - which is precisely what a
		// reader of this row later needs to be able to see.
		slog.Warn("admin bootstrap: first admin granted", "user_id", userID, "remote_ip", c.IP())
		h.recordRoleAudit(c, userID, currentRole, "admin", "bootstrap", userID,
			"first admin granted via bootstrap token on an install with no admin")

		jwtToken, err := auth.IssueJWT(h.cfg.JWTSecret, userID, "admin", "", "", 60*time.Minute)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "token_issue_failed"})
		}
		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"ok":    true,
			"token": jwtToken,
			"role":  "admin",
		})
	}
}

// recordRoleAudit writes one row to admin_role_audit.
//
// Best-effort and never fails the caller: the role change itself is already
// committed, and refusing the response afterwards would leave the caller
// believing it had not happened. A missing audit row is logged loudly so it
// is visible rather than silent.
//
// oldRole is empty for a refused bootstrap, where no change occurred - the row
// exists to record the attempt, not a transition.
func (h *AdminHandler) recordRoleAudit(c *fiber.Ctx, subjectID uuid.UUID, oldRole, newRole, source string, actorID uuid.UUID, note string) {
	if h.db == nil || h.db.Pool == nil {
		return
	}
	if _, err := h.db.Pool.Exec(c.Context(), `
INSERT INTO admin_role_audit (subject_user_id, old_role, new_role, source, actor_user_id, note)
VALUES ($1, NULLIF($2, ''), $3, $4, $5, NULLIF($6, ''))
`, subjectID, oldRole, newRole, source, actorID, note); err != nil {
		slog.Error("admin role audit write failed",
			"subject_user_id", subjectID, "source", source, "error", err)
	}
}
