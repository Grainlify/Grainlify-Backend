package handlers

import (
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

type SearchHandler struct {
	db *db.DB
}

func NewSearchHandler(d *db.DB) *SearchHandler {
	return &SearchHandler{db: d}
}

const searchResultLimit = 8

// Search does a lightweight substring search across verified, publicly
// listed projects, their open issues, and known contributors - backs the
// dashboard's global search (Cmd+K). Same visibility rule as Browse:
// verified, metadata complete, not deleted.
func (h *SearchHandler) Search() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}

		query := strings.TrimSpace(c.Query("q"))
		empty := fiber.Map{"projects": []fiber.Map{}, "issues": []fiber.Map{}, "contributors": []fiber.Map{}}
		if len(query) < 2 {
			return c.Status(fiber.StatusOK).JSON(empty)
		}
		like := "%" + query + "%"
		ctx := c.Context()

		projects := []fiber.Map{}
		if rows, err := h.db.Pool.Query(ctx, `
SELECT p.id, p.github_full_name, p.description, e.name
FROM projects p
LEFT JOIN ecosystems e ON p.ecosystem_id = e.id
WHERE p.status = 'verified' AND p.needs_metadata = false AND p.deleted_at IS NULL
  AND (p.github_full_name ILIKE $1 OR p.description ILIKE $1)
ORDER BY p.stars_count DESC NULLS LAST
LIMIT $2
`, like, searchResultLimit); err == nil {
			for rows.Next() {
				var id, fullName string
				var description, ecosystemName *string
				if rows.Scan(&id, &fullName, &description, &ecosystemName) == nil {
					projects = append(projects, fiber.Map{
						"id":               id,
						"github_full_name": fullName,
						"description":      description,
						"ecosystem_name":   ecosystemName,
					})
				}
			}
			rows.Close()
		}

		issues := []fiber.Map{}
		if rows, err := h.db.Pool.Query(ctx, `
SELECT gi.id, gi.title, gi.number, p.id, p.github_full_name
FROM github_issues gi
INNER JOIN projects p ON gi.project_id = p.id
WHERE p.status = 'verified' AND p.needs_metadata = false AND p.deleted_at IS NULL
  AND gi.state = 'open' AND gi.title ILIKE $1
ORDER BY gi.updated_at_github DESC NULLS LAST
LIMIT $2
`, like, searchResultLimit); err == nil {
			for rows.Next() {
				var id, title, projectID, projectFullName string
				var number int
				if rows.Scan(&id, &title, &number, &projectID, &projectFullName) == nil {
					issues = append(issues, fiber.Map{
						"id":                id,
						"title":             title,
						"number":            number,
						"project_id":        projectID,
						"project_full_name": projectFullName,
					})
				}
			}
			rows.Close()
		}

		contributors := []fiber.Map{}
		if rows, err := h.db.Pool.Query(ctx, `
WITH matched_contributors AS (
  SELECT DISTINCT i.author_login AS login
  FROM github_issues i
  INNER JOIN projects p ON i.project_id = p.id
  WHERE p.status = 'verified' AND i.author_login IS NOT NULL AND i.author_login != ''
    AND i.author_login ILIKE $1
  UNION
  SELECT DISTINCT pr.author_login AS login
  FROM github_pull_requests pr
  INNER JOIN projects p ON pr.project_id = p.id
  WHERE p.status = 'verified' AND pr.author_login IS NOT NULL AND pr.author_login != ''
    AND pr.author_login ILIKE $1
)
SELECT
  mc.login,
  COALESCE(ga.avatar_url, ''),
  COALESCE(u.id::text, ''),
  (
    SELECT COUNT(*) FROM github_issues i INNER JOIN projects p ON i.project_id = p.id
    WHERE LOWER(i.author_login) = LOWER(mc.login) AND p.status = 'verified'
  ) + (
    SELECT COUNT(*) FROM github_pull_requests pr INNER JOIN projects p ON pr.project_id = p.id
    WHERE LOWER(pr.author_login) = LOWER(mc.login) AND p.status = 'verified'
  ) AS contributions
FROM matched_contributors mc
LEFT JOIN github_accounts ga ON LOWER(ga.login) = LOWER(mc.login)
LEFT JOIN users u ON u.id = ga.user_id
ORDER BY contributions DESC
LIMIT $2
`, like, searchResultLimit); err == nil {
			for rows.Next() {
				var login, avatarURL, userID string
				var contributions int
				if rows.Scan(&login, &avatarURL, &userID, &contributions) == nil {
					if avatarURL == "" {
						avatarURL = "https://github.com/" + login + ".png?size=200"
					}
					contributors = append(contributors, fiber.Map{
						"login":         login,
						"user_id":       userID,
						"avatar_url":    avatarURL,
						"contributions": contributions,
					})
				}
			}
			rows.Close()
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"projects":     projects,
			"issues":       issues,
			"contributors": contributors,
		})
	}
}
