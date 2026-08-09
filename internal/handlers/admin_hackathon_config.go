package handlers

import (
	"strconv"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/hackathon"
)

type AdminHackathonConfigHandler struct {
	db *db.DB
}

func NewAdminHackathonConfigHandler(d *db.DB) *AdminHackathonConfigHandler {
	return &AdminHackathonConfigHandler{db: d}
}

type configSettingDTO struct {
	Key         string `json:"key"`
	Type        string `json:"type"`
	Section     string `json:"section"`
	Description string `json:"description"`
	ValidRange  string `json:"valid_range,omitempty"`
	Active      bool   `json:"active"`
	Default     string `json:"default"`
	Value       string `json:"value"`
	Overridden  bool   `json:"overridden"` // true if hackathon_id scope has its own row, distinct from global
}

func parseOptionalHackathonID(c *fiber.Ctx) (*uuid.UUID, error) {
	raw := c.Query("hackathon_id")
	if raw == "" {
		return nil, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// List handles GET /admin/hackathon-config?hackathon_id= - every known
// setting (AI-specs.md §3.1-§3.12), grouped by section in the response so
// "a flat list of 40 toggles is unusable" isn't a problem the frontend has
// to solve alone.
func (h *AdminHackathonConfigHandler) List() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		hackathonID, err := parseOptionalHackathonID(c)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_hackathon_id"})
		}

		values, err := hackathon.EffectiveValues(c.Context(), h.db.Pool, hackathonID)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "config_list_failed"})
		}

		overridden := map[string]bool{}
		if hackathonID != nil {
			rows, err := h.db.Pool.Query(c.Context(), `SELECT key FROM hackathon_config_settings WHERE hackathon_id = $1`, *hackathonID)
			if err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "config_list_failed"})
			}
			for rows.Next() {
				var k string
				if err := rows.Scan(&k); err == nil {
					overridden[k] = true
				}
			}
			rows.Close()
		}

		out := make([]configSettingDTO, 0, len(hackathon.Definitions))
		for key, def := range hackathon.Definitions {
			out = append(out, configSettingDTO{
				Key:         key,
				Type:        def.Type,
				Section:     def.Section,
				Description: def.Description,
				ValidRange:  def.ValidRange,
				Active:      def.Active,
				Default:     def.Default,
				Value:       values[key],
				Overridden:  overridden[key],
			})
		}
		return c.Status(fiber.StatusOK).JSON(fiber.Map{"settings": out})
	}
}

type updateConfigSettingRequest struct {
	HackathonID *uuid.UUID `json:"hackathon_id"`
	Key         string     `json:"key"`
	Value       string     `json:"value"`
}

// Update handles PUT /admin/hackathon-config.
func (h *AdminHackathonConfigHandler) Update() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		actorID, ok := adminID(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}
		var req updateConfigSettingRequest
		if err := c.BodyParser(&req); err != nil || req.Key == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_body"})
		}

		if err := hackathon.SetValue(c.Context(), h.db.Pool, req.HackathonID, req.Key, req.Value, actorID); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "config_update_failed", "message": err.Error()})
		}
		return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true})
	}
}

type resetConfigSettingRequest struct {
	HackathonID uuid.UUID `json:"hackathon_id"`
	Key         string    `json:"key"`
}

// Reset handles POST /admin/hackathon-config/reset - deletes a per-hackathon
// override so the key falls back to the global default. Global-scope
// settings have no "reset" (there's nothing above them but the factory
// default, which SetValue can just be called with directly).
func (h *AdminHackathonConfigHandler) Reset() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		actorID, ok := adminID(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}
		var req resetConfigSettingRequest
		if err := c.BodyParser(&req); err != nil || req.Key == "" || req.HackathonID == uuid.Nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_body"})
		}

		if err := hackathon.ResetValue(c.Context(), h.db.Pool, req.HackathonID, req.Key, actorID); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "config_reset_failed", "message": err.Error()})
		}
		return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true})
	}
}

type configAuditEntryDTO struct {
	ID          uuid.UUID  `json:"id"`
	HackathonID *uuid.UUID `json:"hackathon_id"`
	Key         string     `json:"key"`
	OldValue    *string    `json:"old_value"`
	NewValue    *string    `json:"new_value"`
	ActorUserID *uuid.UUID `json:"actor_user_id"`
	ActorLogin  *string    `json:"actor_login"`
	CreatedAt   string     `json:"created_at"`
}

// Audit handles GET /admin/hackathon-config/audit?key=&hackathon_id=&limit=&offset=
// - "why did this contributor get $110" must be answerable months later
// (AI-specs.md §3.1), including for phase transitions (key='phase').
func (h *AdminHackathonConfigHandler) Audit() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		limit := c.QueryInt("limit", 100)
		if limit <= 0 || limit > 500 {
			limit = 100
		}
		offset := c.QueryInt("offset", 0)
		if offset < 0 {
			offset = 0
		}

		query := `
SELECT ca.id, ca.hackathon_id, ca.key, ca.old_value, ca.new_value, ca.actor_user_id, ga.login, ca.created_at::text
FROM config_audit ca
LEFT JOIN github_accounts ga ON ga.user_id = ca.actor_user_id
WHERE 1=1
`
		args := []any{}
		if key := c.Query("key"); key != "" {
			args = append(args, key)
			query += ` AND ca.key = $` + strconv.Itoa(len(args))
		}
		if hid := c.Query("hackathon_id"); hid != "" {
			id, err := uuid.Parse(hid)
			if err != nil {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_hackathon_id"})
			}
			args = append(args, id)
			query += ` AND ca.hackathon_id = $` + strconv.Itoa(len(args))
		}
		args = append(args, limit, offset)
		query += ` ORDER BY ca.created_at DESC LIMIT $` + strconv.Itoa(len(args)-1) + ` OFFSET $` + strconv.Itoa(len(args))

		rows, err := h.db.Pool.Query(c.Context(), query, args...)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "audit_list_failed"})
		}
		defer rows.Close()

		out := []configAuditEntryDTO{}
		for rows.Next() {
			var e configAuditEntryDTO
			if err := rows.Scan(&e.ID, &e.HackathonID, &e.Key, &e.OldValue, &e.NewValue, &e.ActorUserID, &e.ActorLogin, &e.CreatedAt); err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "audit_scan_failed"})
			}
			out = append(out, e)
		}
		return c.Status(fiber.StatusOK).JSON(fiber.Map{"entries": out})
	}
}
