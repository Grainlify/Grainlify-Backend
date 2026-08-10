package handlers

import (
	"context"
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
	"github.com/jagadeesh/grainlify/backend/internal/github"
)

type OrgRatingsHandler struct {
	cfg config.Config
	db  *db.DB
}

func NewOrgRatingsHandler(cfg config.Config, d *db.DB) *OrgRatingsHandler {
	return &OrgRatingsHandler{cfg: cfg, db: d}
}

// "Org" has no first-class table anywhere in this schema - every helper
// below identifies one purely by SPLIT_PART(github_full_name, '/', 1),
// matched case-insensitively, matching the same convention already used
// throughout internal/handlers/user_profile.go and projects_public.go.

// hasEligibleMergedPR reports whether githubLogin has had at least one PR
// merged into a verified, non-deleted project under orgLogin.
func hasEligibleMergedPR(ctx context.Context, pool db.DBPool, githubLogin, orgLogin string) (bool, error) {
	var exists bool
	err := pool.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM github_pull_requests pr
  JOIN projects p ON p.id = pr.project_id
  WHERE LOWER(pr.author_login) = LOWER($1) AND pr.merged = true
    AND LOWER(SPLIT_PART(p.github_full_name, '/', 1)) = LOWER($2)
    AND p.status = 'verified' AND p.deleted_at IS NULL
)
`, githubLogin, orgLogin).Scan(&exists)
	return exists, err
}

// isOrgOwner reports whether userID owns at least one project under
// orgLogin - excluded from rating eligibility so an org's own maintainer
// can't trivially self-merge a PR and rate their own org.
func isOrgOwner(ctx context.Context, pool db.DBPool, userID uuid.UUID, orgLogin string) (bool, error) {
	var exists bool
	err := pool.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM projects
  WHERE owner_user_id = $1 AND LOWER(SPLIT_PART(github_full_name, '/', 1)) = LOWER($2)
)
`, userID, orgLogin).Scan(&exists)
	return exists, err
}

// canRateOrg composes the two checks above into the single eligibility rule
// both GET .../ratings/me and POST .../ratings enforce.
func canRateOrg(ctx context.Context, pool db.DBPool, userID uuid.UUID, githubLogin, orgLogin string) (bool, error) {
	eligible, err := hasEligibleMergedPR(ctx, pool, githubLogin, orgLogin)
	if err != nil || !eligible {
		return false, err
	}
	owner, err := isOrgOwner(ctx, pool, userID, orgLogin)
	if err != nil {
		return false, err
	}
	return !owner, nil
}

// orgRankPosition returns this org's 1-indexed rank among all orgs, ranked
// by deduped contributor count across their verified, non-deleted repos.
// Returns (nil, nil) if the org has no ranked contributors at all - not an
// error, just "unranked" (mirrors user_profile.go's own rank-lookup
// not-found handling).
func orgRankPosition(ctx context.Context, pool db.DBPool, orgLogin string) (*int, error) {
	var position *int
	err := pool.QueryRow(ctx, `
WITH org_contributors AS (
  SELECT SPLIT_PART(p.github_full_name, '/', 1) AS org_login,
         COUNT(DISTINCT LOWER(contributions.author_login)) AS contributors
  FROM (
    SELECT project_id, author_login FROM github_issues WHERE author_login IS NOT NULL AND author_login != ''
    UNION
    SELECT project_id, author_login FROM github_pull_requests WHERE author_login IS NOT NULL AND author_login != ''
  ) contributions
  JOIN projects p ON p.id = contributions.project_id
  WHERE p.status = 'verified' AND p.deleted_at IS NULL
  GROUP BY SPLIT_PART(p.github_full_name, '/', 1)
),
ranked AS (
  SELECT org_login, contributors,
         ROW_NUMBER() OVER (ORDER BY contributors DESC, org_login ASC) AS position
  FROM org_contributors
)
SELECT position FROM ranked WHERE LOWER(org_login) = LOWER($1)
`, orgLogin).Scan(&position)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return position, err
}

// Summary handles GET /orgs/:login - aggregate stats, rank, and average
// rating for one org. Public, no auth required.
func (h *OrgRatingsHandler) Summary() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		orgLogin := strings.TrimSpace(c.Params("login"))
		if orgLogin == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_org_login"})
		}

		var repoCount, totalStars int
		err := h.db.Pool.QueryRow(c.Context(), `
SELECT COUNT(*), COALESCE(SUM(stars_count), 0)
FROM projects
WHERE status = 'verified' AND deleted_at IS NULL
  AND LOWER(SPLIT_PART(github_full_name, '/', 1)) = LOWER($1)
`, orgLogin).Scan(&repoCount, &totalStars)
		if err != nil {
			slog.Error("org summary: repo lookup failed", "error", err, "org_login", orgLogin)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "org_lookup_failed"})
		}
		if repoCount == 0 {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "org_not_found"})
		}

		var contributorCount int
		_ = h.db.Pool.QueryRow(c.Context(), `
SELECT COUNT(DISTINCT LOWER(contributions.author_login))
FROM (
  SELECT project_id, author_login FROM github_issues WHERE author_login IS NOT NULL AND author_login != ''
  UNION
  SELECT project_id, author_login FROM github_pull_requests WHERE author_login IS NOT NULL AND author_login != ''
) contributions
JOIN projects p ON p.id = contributions.project_id
WHERE p.status = 'verified' AND p.deleted_at IS NULL
  AND LOWER(SPLIT_PART(p.github_full_name, '/', 1)) = LOWER($1)
`, orgLogin).Scan(&contributorCount)

		var mergedPRCount int
		_ = h.db.Pool.QueryRow(c.Context(), `
SELECT COUNT(*) FROM github_pull_requests pr
JOIN projects p ON p.id = pr.project_id
WHERE pr.merged = true AND p.status = 'verified' AND p.deleted_at IS NULL
  AND LOWER(SPLIT_PART(p.github_full_name, '/', 1)) = LOWER($1)
`, orgLogin).Scan(&mergedPRCount)

		position, err := orgRankPosition(c.Context(), h.db.Pool, orgLogin)
		if err != nil {
			slog.Warn("org summary: rank lookup failed", "error", err, "org_login", orgLogin)
		}
		tier := RankTierUnranked
		tierName := GetRankTierDisplayName(tier)
		tierColor := GetRankTierColor(tier)
		if position != nil {
			tier = GetRankTier(*position)
			tierName = GetRankTierDisplayName(tier)
			tierColor = GetRankTierColor(tier)
		}

		var avgRating *float64
		var ratingsCount int
		_ = h.db.Pool.QueryRow(c.Context(), `
SELECT AVG(rating)::float8, COUNT(*) FROM org_ratings WHERE LOWER(org_login) = LOWER($1)
`, orgLogin).Scan(&avgRating, &ratingsCount)

		return c.JSON(fiber.Map{
			"login":              orgLogin,
			"avatar_url":         github.AvatarURL(orgLogin, 200),
			"repo_count":         repoCount,
			"stars_count":        totalStars,
			"contributors_count": contributorCount,
			"merged_prs_count":   mergedPRCount,
			"rank_position":      position,
			"rank_tier":          string(tier),
			"rank_tier_name":     tierName,
			"rank_tier_color":    tierColor,
			"average_rating":     avgRating,
			"ratings_count":      ratingsCount,
		})
	}
}

type orgActivityWeekDTO struct {
	WeekStart    string `json:"week_start"` // "2026-07-27", Monday-start ISO week
	IssuesOpened int    `json:"issues_opened"`
	PRsMerged    int    `json:"prs_merged"`
}

const orgActivityWeeks = 12

// Activity handles GET /orgs/:login/activity - issues opened and PRs merged
// per week across the org's verified repos, for the last orgActivityWeeks
// weeks. Public, no auth required. Zero-filled in SQL via generate_series
// (unlike user_profile.go's ContributionCalendar, which zero-fills in Go -
// no existing shared helper for this, and generate_series keeps the Go side
// a plain row scan).
func (h *OrgRatingsHandler) Activity() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		orgLogin := strings.TrimSpace(c.Params("login"))
		if orgLogin == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_org_login"})
		}

		var repoCount int
		if err := h.db.Pool.QueryRow(c.Context(), `
SELECT COUNT(*) FROM projects
WHERE status = 'verified' AND deleted_at IS NULL
  AND LOWER(SPLIT_PART(github_full_name, '/', 1)) = LOWER($1)
`, orgLogin).Scan(&repoCount); err != nil {
			slog.Error("org activity: repo lookup failed", "error", err, "org_login", orgLogin)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "org_lookup_failed"})
		}
		if repoCount == 0 {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "org_not_found"})
		}

		rows, err := h.db.Pool.Query(c.Context(), `
WITH weeks AS (
  SELECT generate_series(
    date_trunc('week', now() - ($2 - 1) * interval '1 week'),
    date_trunc('week', now()),
    interval '1 week'
  )::date AS week_start
),
issues_by_week AS (
  SELECT date_trunc('week', gi.created_at_github)::date AS week_start, COUNT(*) AS n
  FROM github_issues gi
  JOIN projects p ON p.id = gi.project_id
  WHERE LOWER(SPLIT_PART(p.github_full_name, '/', 1)) = LOWER($1)
    AND p.status = 'verified' AND p.deleted_at IS NULL
    AND gi.created_at_github >= date_trunc('week', now() - ($2 - 1) * interval '1 week')
  GROUP BY 1
),
prs_by_week AS (
  SELECT date_trunc('week', gp.merged_at_github)::date AS week_start, COUNT(*) AS n
  FROM github_pull_requests gp
  JOIN projects p ON p.id = gp.project_id
  WHERE LOWER(SPLIT_PART(p.github_full_name, '/', 1)) = LOWER($1)
    AND p.status = 'verified' AND p.deleted_at IS NULL
    AND gp.merged = true
    AND gp.merged_at_github >= date_trunc('week', now() - ($2 - 1) * interval '1 week')
  GROUP BY 1
)
SELECT w.week_start, COALESCE(i.n, 0), COALESCE(m.n, 0)
FROM weeks w
LEFT JOIN issues_by_week i ON i.week_start = w.week_start
LEFT JOIN prs_by_week m ON m.week_start = w.week_start
ORDER BY w.week_start ASC
`, orgLogin, orgActivityWeeks)
		if err != nil {
			slog.Error("org activity: query failed", "error", err, "org_login", orgLogin)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "activity_query_failed"})
		}
		defer rows.Close()

		weeks := []orgActivityWeekDTO{}
		for rows.Next() {
			var weekStart time.Time
			var d orgActivityWeekDTO
			if err := rows.Scan(&weekStart, &d.IssuesOpened, &d.PRsMerged); err != nil {
				slog.Error("org activity: scan failed", "error", err, "org_login", orgLogin)
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "activity_scan_failed"})
			}
			d.WeekStart = weekStart.Format("2006-01-02")
			weeks = append(weeks, d)
		}
		if err := rows.Err(); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "activity_query_failed"})
		}

		return c.JSON(fiber.Map{"weeks": weeks})
	}
}

type orgCalendarDayDTO struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
	Level int    `json:"level"`
}

const orgCalendarDays = 365

// Calendar handles GET /orgs/:login/calendar - a GitHub-style contribution
// heatmap aggregated across the org's verified repos for the last
// orgCalendarDays days. Public, no auth required. Mirrors
// UserProfileHandler.ContributionCalendar's semantics exactly (issues
// opened + PRs opened, both via created_at_github) so "contributions" means
// the same thing on a user's own profile and an org's - just zero-filled in
// SQL via generate_series (this handler's own established convention, see
// Activity() above) instead of ContributionCalendar's Go-side day-walk, and
// reusing that same file's calculateContributionLevel for the quartile
// levels rather than re-deriving it.
func (h *OrgRatingsHandler) Calendar() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		orgLogin := strings.TrimSpace(c.Params("login"))
		if orgLogin == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_org_login"})
		}

		var repoCount int
		if err := h.db.Pool.QueryRow(c.Context(), `
SELECT COUNT(*) FROM projects
WHERE status = 'verified' AND deleted_at IS NULL
  AND LOWER(SPLIT_PART(github_full_name, '/', 1)) = LOWER($1)
`, orgLogin).Scan(&repoCount); err != nil {
			slog.Error("org calendar: repo lookup failed", "error", err, "org_login", orgLogin)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "org_lookup_failed"})
		}
		if repoCount == 0 {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "org_not_found"})
		}

		rows, err := h.db.Pool.Query(c.Context(), `
WITH days AS (
  SELECT generate_series(
    (now() - ($2 - 1) * interval '1 day')::date,
    now()::date,
    interval '1 day'
  ) AS day
),
daily_counts AS (
  SELECT DATE(contribution_date) AS day, COUNT(*) AS n
  FROM (
    SELECT gi.created_at_github AS contribution_date
    FROM github_issues gi
    JOIN projects p ON p.id = gi.project_id
    WHERE LOWER(SPLIT_PART(p.github_full_name, '/', 1)) = LOWER($1)
      AND p.status = 'verified' AND p.deleted_at IS NULL
      AND gi.created_at_github >= (now() - ($2 - 1) * interval '1 day')
    UNION ALL
    SELECT gp.created_at_github AS contribution_date
    FROM github_pull_requests gp
    JOIN projects p ON p.id = gp.project_id
    WHERE LOWER(SPLIT_PART(p.github_full_name, '/', 1)) = LOWER($1)
      AND p.status = 'verified' AND p.deleted_at IS NULL
      AND gp.created_at_github >= (now() - ($2 - 1) * interval '1 day')
  ) contributions
  GROUP BY 1
)
SELECT d.day, COALESCE(dc.n, 0)::int
FROM days d
LEFT JOIN daily_counts dc ON dc.day = d.day
ORDER BY d.day ASC
`, orgLogin, orgCalendarDays)
		if err != nil {
			slog.Error("org calendar: query failed", "error", err, "org_login", orgLogin)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "calendar_query_failed"})
		}
		defer rows.Close()

		type rawDay struct {
			date  time.Time
			count int
		}
		var raw []rawDay
		maxCount := 0
		total := 0
		for rows.Next() {
			var d rawDay
			if err := rows.Scan(&d.date, &d.count); err != nil {
				slog.Error("org calendar: scan failed", "error", err, "org_login", orgLogin)
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "calendar_scan_failed"})
			}
			raw = append(raw, d)
			total += d.count
			if d.count > maxCount {
				maxCount = d.count
			}
		}
		if err := rows.Err(); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "calendar_query_failed"})
		}

		calendar := make([]orgCalendarDayDTO, 0, len(raw))
		for _, d := range raw {
			calendar = append(calendar, orgCalendarDayDTO{
				Date:  d.date.Format("2006-01-02"),
				Count: d.count,
				Level: calculateContributionLevel(d.count, maxCount),
			})
		}

		return c.JSON(fiber.Map{"calendar": calendar, "total": total})
	}
}

type orgReviewDTO struct {
	Rating      int       `json:"rating"`
	Comment     *string   `json:"comment,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	UserID      uuid.UUID `json:"user_id"`
	DisplayName string    `json:"display_name"`
	AvatarURL   string    `json:"avatar_url"`
	GithubLogin string    `json:"github_login,omitempty"`
}

// List handles GET /orgs/:login/ratings - a paginated list of individual
// reviews with reviewer identity. Public, no auth required.
func (h *OrgRatingsHandler) List() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}
		orgLogin := strings.TrimSpace(c.Params("login"))
		if orgLogin == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_org_login"})
		}
		limit := c.QueryInt("limit", 20)
		if limit <= 0 || limit > 100 {
			limit = 20
		}
		offset := c.QueryInt("offset", 0)
		if offset < 0 {
			offset = 0
		}

		rows, err := h.db.Pool.Query(c.Context(), `
SELECT r.rating, r.comment, r.created_at, r.updated_at,
       u.id, COALESCE(u.display_name, ga.login, 'Anonymous'),
       COALESCE(u.avatar_url, ga.avatar_url, ''), COALESCE(ga.login, '')
FROM org_ratings r
JOIN users u ON u.id = r.user_id
LEFT JOIN github_accounts ga ON ga.user_id = u.id
WHERE LOWER(r.org_login) = LOWER($1)
ORDER BY r.created_at DESC
LIMIT $2 OFFSET $3
`, orgLogin, limit, offset)
		if err != nil {
			slog.Error("org ratings list failed", "error", err, "org_login", orgLogin)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "ratings_query_failed"})
		}
		defer rows.Close()

		out := []orgReviewDTO{}
		for rows.Next() {
			var d orgReviewDTO
			if err := rows.Scan(&d.Rating, &d.Comment, &d.CreatedAt, &d.UpdatedAt, &d.UserID, &d.DisplayName, &d.AvatarURL, &d.GithubLogin); err != nil {
				slog.Error("org ratings scan failed", "error", err, "org_login", orgLogin)
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "ratings_scan_failed"})
			}
			out = append(out, d)
		}
		if err := rows.Err(); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "ratings_query_failed"})
		}

		var total int
		_ = h.db.Pool.QueryRow(c.Context(), `SELECT COUNT(*) FROM org_ratings WHERE LOWER(org_login) = LOWER($1)`, orgLogin).Scan(&total)

		return c.JSON(fiber.Map{"ratings": out, "total": total})
	}
}

// MyStatus handles GET /orgs/:login/ratings/me - whether the caller is
// eligible to rate this org, and their existing rating if they've already
// left one. Authenticated.
func (h *OrgRatingsHandler) MyStatus() fiber.Handler {
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

		linked, err := github.GetLinkedAccount(c.Context(), h.db.Pool, userID, h.cfg.TokenEncKeyB64)
		if err != nil {
			// No linked GitHub account is a legitimate ineligible state here
			// (a wallet-only user), not an error - unlike write-path handlers
			// where it's a hard stop.
			return c.JSON(fiber.Map{"eligible": false, "rating": nil})
		}

		eligible, err := canRateOrg(c.Context(), h.db.Pool, userID, linked.Login, orgLogin)
		if err != nil {
			slog.Error("org rating eligibility check failed", "error", err, "org_login", orgLogin)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "eligibility_check_failed"})
		}

		var rating int
		var comment *string
		var createdAt, updatedAt time.Time
		err = h.db.Pool.QueryRow(c.Context(), `
SELECT rating, comment, created_at, updated_at FROM org_ratings
WHERE user_id = $1 AND LOWER(org_login) = LOWER($2)
`, userID, orgLogin).Scan(&rating, &comment, &createdAt, &updatedAt)

		var existing fiber.Map
		if err == nil {
			existing = fiber.Map{"rating": rating, "comment": comment, "created_at": createdAt, "updated_at": updatedAt}
			// A user keeps the ability to edit their own existing rating even
			// if they'd no longer separately qualify (e.g. their only merged
			// PR was later reverted) - eligibility, once earned, isn't revoked
			// out from under an already-submitted review.
			eligible = true
		} else if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("org rating self-lookup failed", "error", err, "org_login", orgLogin)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "rating_lookup_failed"})
		}

		return c.JSON(fiber.Map{"eligible": eligible, "rating": existing})
	}
}

type submitOrgRatingRequest struct {
	Rating  int     `json:"rating"`
	Comment *string `json:"comment"`
}

// Submit handles POST /orgs/:login/ratings - create or update the caller's
// own rating for this org. Authenticated; re-checks eligibility server-side
// regardless of what the client believes.
func (h *OrgRatingsHandler) Submit() fiber.Handler {
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

		var req submitOrgRatingRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_body"})
		}
		if req.Rating < 1 || req.Rating > 5 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid_rating"})
		}

		linked, err := github.GetLinkedAccount(c.Context(), h.db.Pool, userID, h.cfg.TokenEncKeyB64)
		if err != nil {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "github_not_linked"})
		}

		// A user who already has a rating for this org may always edit it,
		// even if the underlying eligibility signal has since changed -
		// mirrors MyStatus()'s same "once earned, not revoked" rule.
		var alreadyRated bool
		_ = h.db.Pool.QueryRow(c.Context(), `
SELECT EXISTS(SELECT 1 FROM org_ratings WHERE user_id = $1 AND LOWER(org_login) = LOWER($2))
`, userID, orgLogin).Scan(&alreadyRated)

		if !alreadyRated {
			eligible, err := canRateOrg(c.Context(), h.db.Pool, userID, linked.Login, orgLogin)
			if err != nil {
				slog.Error("org rating eligibility check failed", "error", err, "org_login", orgLogin)
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "eligibility_check_failed"})
			}
			if !eligible {
				return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "not_eligible"})
			}
		}

		_, err = h.db.Pool.Exec(c.Context(), `
INSERT INTO org_ratings (user_id, org_login, rating, comment, updated_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (user_id, (LOWER(org_login))) DO UPDATE SET
  rating = EXCLUDED.rating,
  comment = EXCLUDED.comment,
  updated_at = now()
`, userID, orgLogin, req.Rating, req.Comment)
		if err != nil {
			slog.Error("failed to submit org rating", "error", err, "user_id", userID, "org_login", orgLogin)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "rating_submit_failed"})
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{"ok": true})
	}
}
