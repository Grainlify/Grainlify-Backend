package handlers

import (
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
)

const orgLinkMaxLength = 200

type OrgLinksHandler struct {
	db *db.DB
}

func NewOrgLinksHandler(d *db.DB) *OrgLinksHandler {
	return &OrgLinksHandler{db: d}
}

type orgLinksDTO struct {
	Telegram *string `json:"telegram"`
	LinkedIn *string `json:"linkedin"`
	WhatsApp *string `json:"whatsapp"`
	Twitter  *string `json:"twitter"`
	Discord  *string `json:"discord"`
}

// Get handles GET /orgs/:login/links - public, no auth required. Returns all
// nil fields (not a 404) when the org has never had links configured - "no
// links yet" is a normal state for any org login, not an error, and this
// endpoint doesn't itself validate the org exists (Summary() already does
// that for whoever is deciding whether to render an org page at all).
func (h *OrgLinksHandler) Get() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		orgLogin := strings.TrimSpace(c.Params("login"))
		if orgLogin == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_org_login"})
		}

		var out orgLinksDTO
		err := h.db.Pool.QueryRow(c.Context(), `
SELECT telegram, linkedin, whatsapp, twitter, discord
FROM org_social_links WHERE LOWER(org_login) = LOWER($1)
`, orgLogin).Scan(&out.Telegram, &out.LinkedIn, &out.WhatsApp, &out.Twitter, &out.Discord)
		if err != nil {
			// No row yet is the common case, not an error - out is already
			// all-nil, exactly what we want to return.
			return c.JSON(out)
		}

		return c.JSON(out)
	}
}

type updateOrgLinksRequest struct {
	Telegram *string `json:"telegram"`
	LinkedIn *string `json:"linkedin"`
	WhatsApp *string `json:"whatsapp"`
	Twitter  *string `json:"twitter"`
	Discord  *string `json:"discord"`
}

// Update handles PUT /orgs/:login/links - authenticated, requires the
// caller to own at least one project under this org (isOrgOwner, shared
// with org_ratings.go). Unlike users.UpdateProfile, a field CAN be cleared:
// a JSON key present with an empty string sets that column to NULL; an
// absent key leaves the existing value untouched; a non-empty string sets
// it (trimmed). This matters here specifically because a maintainer fixing
// a wrong handle needs a way to blank it, not just overwrite it.
func (h *OrgLinksHandler) Update() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		orgLogin := strings.TrimSpace(c.Params("login"))
		if orgLogin == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_org_login"})
		}
		userIDStr, _ := c.Locals(auth.LocalUserID).(string)
		userID, err := uuid.Parse(userIDStr)
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid_user"})
		}

		owner, err := isOrgOwner(c.Context(), h.db.Pool, userID, orgLogin)
		if err != nil {
			slog.Error("org links: ownership check failed", "error", err, "org_login", orgLogin)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "ownership_check_failed"})
		}
		if !owner {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "not_an_org_maintainer"})
		}

		var req updateOrgLinksRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_body"})
		}

		fields := map[string]*string{
			"telegram": req.Telegram,
			"linkedin": req.LinkedIn,
			"whatsapp": req.WhatsApp,
			"twitter":  req.Twitter,
			"discord":  req.Discord,
		}
		setClauses := []string{}
		args := []any{}
		insertCols := []string{"org_login", "updated_by", "updated_at"}
		insertVals := []string{"$1", "$2", "$3"}
		args = append(args, orgLogin, userID, time.Now().UTC())
		for _, col := range []string{"telegram", "linkedin", "whatsapp", "twitter", "discord"} {
			ptr := fields[col]
			if ptr == nil {
				continue // key absent - leave this column untouched
			}
			trimmed := strings.TrimSpace(*ptr)
			if len(trimmed) > orgLinkMaxLength {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "link_too_long", "field": col})
			}
			var val *string
			if trimmed != "" {
				val = &trimmed
			} // empty string -> val stays nil -> column set to NULL
			args = append(args, val)
			setClauses = append(setClauses, col+" = $"+strconv.Itoa(len(args)))
			insertCols = append(insertCols, col)
			insertVals = append(insertVals, "$"+strconv.Itoa(len(args)))
		}
		setClauses = append(setClauses, "updated_by = $2", "updated_at = $3")

		query := "INSERT INTO org_social_links (" + strings.Join(insertCols, ", ") + ") VALUES (" +
			strings.Join(insertVals, ", ") + ") ON CONFLICT ((LOWER(org_login))) DO UPDATE SET " +
			strings.Join(setClauses, ", ")

		if _, err := h.db.Pool.Exec(c.Context(), query, args...); err != nil {
			slog.Error("org links: update failed", "error", err, "org_login", orgLogin)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "update_failed"})
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true})
	}
}
