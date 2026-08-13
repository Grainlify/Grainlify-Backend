package handlers

import (
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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


// recordRoleAudit writes one row to admin_role_audit.
//
// Best-effort and never fails the caller: the role change itself is already
// committed, and refusing the response afterwards would leave the caller
// believing it had not happened. A missing audit row is logged loudly so it
// is visible rather than silent.
//
// oldRole is empty for a refused bootstrap, where no change occurred - the row
// exists to record the attempt, not a transition.
// Admin bootstrap was removed.
//
// It granted the first admin on a fresh install to whoever presented the
// shared ADMIN_BOOTSTRAP_TOKEN. It already refused once any admin existed, so
// it was closed in practice - but "closed" depended on the admin count staying
// above zero, which meant an environment variable could reopen a permanent
// grant path. A door that is shut is not the same as a door that is gone.
//
// The recovery path is the database, deliberately and solely: a direct
// UPDATE on users.role by whoever holds database access. That is verified
// end to end in break_glass_test.go, including the part that surprises people
// - the holder must sign in again, because a stale token whose claim
// disagrees with the stored role is refused rather than silently upgraded.
//
// The cost, accepted: a genuinely fresh environment has no in-product way to
// create its first admin and needs a database insert. That is the stated
// policy, not an oversight.
//
// Every further admin is granted by an existing admin through SetUserRole,
// which is attributed and audited.

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
