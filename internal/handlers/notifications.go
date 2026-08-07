package handlers

import (
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

type NotificationsHandler struct {
	db *db.DB
}

func NewNotificationsHandler(d *db.DB) *NotificationsHandler {
	return &NotificationsHandler{db: d}
}

func (h *NotificationsHandler) userID(c *fiber.Ctx) (uuid.UUID, bool) {
	idStr, _ := c.Locals(auth.LocalUserID).(string)
	id, err := uuid.Parse(idStr)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

type notificationDTO struct {
	ID        uuid.UUID  `json:"id"`
	Type      string     `json:"type"`
	Title     string     `json:"title"`
	Body      string     `json:"body,omitempty"`
	LinkPath  string     `json:"link_path,omitempty"`
	ReadAt    *time.Time `json:"read_at"`
	CreatedAt time.Time  `json:"created_at"`
}

// List handles GET /notifications?limit=&offset=&unread_only=
func (h *NotificationsHandler) List() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		userID, ok := h.userID(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		limit := c.QueryInt("limit", 20)
		if limit <= 0 || limit > 100 {
			limit = 20
		}
		offset := c.QueryInt("offset", 0)
		if offset < 0 {
			offset = 0
		}
		unreadOnly := c.Query("unread_only") == "true"

		query := `
SELECT id, type, title, COALESCE(body, ''), COALESCE(link_path, ''), read_at, created_at
FROM notifications
WHERE user_id = $1`
		if unreadOnly {
			query += ` AND read_at IS NULL`
		}
		query += ` ORDER BY created_at DESC LIMIT $2 OFFSET $3`

		rows, err := h.db.Pool.Query(c.Context(), query, userID, limit, offset)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "list_failed"})
		}
		defer rows.Close()

		items := []notificationDTO{}
		for rows.Next() {
			var n notificationDTO
			if err := rows.Scan(&n.ID, &n.Type, &n.Title, &n.Body, &n.LinkPath, &n.ReadAt, &n.CreatedAt); err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "list_scan_failed"})
			}
			items = append(items, n)
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{"notifications": items})
	}
}

// UnreadCount handles GET /notifications/unread-count
func (h *NotificationsHandler) UnreadCount() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		userID, ok := h.userID(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		var count int
		err := h.db.Pool.QueryRow(c.Context(), `
SELECT count(*) FROM notifications WHERE user_id = $1 AND read_at IS NULL
`, userID).Scan(&count)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "count_failed"})
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{"count": count})
	}
}

// MarkRead handles POST /notifications/:id/read
func (h *NotificationsHandler) MarkRead() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		userID, ok := h.userID(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}
		notifID, err := uuid.Parse(c.Params("id"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_notification_id"})
		}

		tag, err := h.db.Pool.Exec(c.Context(), `
UPDATE notifications SET read_at = now() WHERE id = $1 AND user_id = $2 AND read_at IS NULL
`, notifID, userID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "mark_read_failed"})
		}
		if tag.RowsAffected() == 0 {
			// Either it doesn't exist, isn't this user's, or was already
			// read - all three are fine to report as success (idempotent).
			return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true})
		}
		return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true})
	}
}

// MarkAllRead handles POST /notifications/read-all
func (h *NotificationsHandler) MarkAllRead() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		userID, ok := h.userID(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		if _, err := h.db.Pool.Exec(c.Context(), `
UPDATE notifications SET read_at = now() WHERE user_id = $1 AND read_at IS NULL
`, userID); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "mark_all_read_failed"})
		}
		return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true})
	}
}

type preferenceDTO struct {
	Type  string `json:"type"`
	InApp bool   `json:"in_app"`
	Email bool   `json:"email"`
}

// GetPreferences handles GET /notifications/preferences
func (h *NotificationsHandler) GetPreferences() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		userID, ok := h.userID(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		rows, err := h.db.Pool.Query(c.Context(), `
SELECT type, in_app, email FROM notification_preferences WHERE user_id = $1
`, userID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "preferences_lookup_failed"})
		}
		saved := map[string]preferenceDTO{}
		for rows.Next() {
			var p preferenceDTO
			if err := rows.Scan(&p.Type, &p.InApp, &p.Email); err != nil {
				rows.Close()
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "preferences_scan_failed"})
			}
			saved[p.Type] = p
		}
		rows.Close()

		// Every known type is always returned, defaulting to both channels
		// enabled when the user has no saved row for it.
		out := make([]preferenceDTO, 0, len(notifications.AllTypes))
		for _, t := range notifications.AllTypes {
			if p, found := saved[string(t)]; found {
				out = append(out, p)
			} else {
				out = append(out, preferenceDTO{Type: string(t), InApp: true, Email: true})
			}
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{"preferences": out})
	}
}

// UpdatePreferences handles PUT /notifications/preferences
func (h *NotificationsHandler) UpdatePreferences() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		userID, ok := h.userID(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		var req struct {
			Preferences []preferenceDTO `json:"preferences"`
		}
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_body"})
		}

		for _, p := range req.Preferences {
			if !notifications.Type(p.Type).Valid() {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_notification_type", "type": p.Type})
			}
		}

		tx, err := h.db.Pool.BeginTx(c.Context(), pgx.TxOptions{})
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "preferences_update_failed"})
		}
		defer tx.Rollback(c.Context())

		for _, p := range req.Preferences {
			if _, err := tx.Exec(c.Context(), `
INSERT INTO notification_preferences (user_id, type, in_app, email, updated_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (user_id, type) DO UPDATE SET in_app = $3, email = $4, updated_at = now()
`, userID, p.Type, p.InApp, p.Email); err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "preferences_update_failed"})
			}
		}

		if err := tx.Commit(c.Context()); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "preferences_update_failed"})
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true})
	}
}
