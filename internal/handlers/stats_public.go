package handlers

import (
	"log/slog"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/metrics"
)

const landingStatsCacheTTL = 45 * time.Second

type LandingStatsHandler struct {
	db *db.DB

	cacheMu sync.RWMutex
	cache   landingStatsCache
	ttl     time.Duration
	now     func() time.Time
}

type landingStatsCache struct {
	resp      LandingStatsResponse
	expiresAt time.Time
	ok        bool
}

func NewLandingStatsHandler(d *db.DB) *LandingStatsHandler {
	return newLandingStatsHandler(d, landingStatsCacheTTL, time.Now)
}

func newLandingStatsHandler(d *db.DB, ttl time.Duration, now func() time.Time) *LandingStatsHandler {
	if now == nil {
		now = time.Now
	}
	return &LandingStatsHandler{db: d, ttl: ttl, now: now}
}

type LandingStatsResponse struct {
	ActiveProjects       int64 `json:"active_projects"`
	Contributors         int64 `json:"contributors"`
	GrantsDistributedUSD int64 `json:"grants_distributed_usd"`
}

// Get returns high-level landing page stats.
//
// Notes:
//   - Active projects are verified projects that aren't soft-deleted.
//   - Contributors are distinct GitHub author logins across issues/PRs in verified projects.
//   - Grants distributed is currently 0 (no payouts table implemented yet).
//   - Results are cached in-process for 45 seconds to bound staleness while reducing
//     repeated aggregate queries on this public endpoint. Each backend process keeps
//     its own cache; there is no cross-process sharing or invalidation.
func (h *LandingStatsHandler) Get() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}

		now := h.now()
		if resp, ok := h.cached(now); ok {
			metrics.LandingStatsCache.WithLabelValues("hit").Inc()
			return c.Status(fiber.StatusOK).JSON(resp)
		}
		metrics.LandingStatsCache.WithLabelValues("miss").Inc()

		resp, err := h.fetch(c)
		if err != nil {
			slog.Error("failed to fetch landing stats", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "stats_fetch_failed"})
		}

		h.store(resp, now.Add(h.ttl))
		return c.Status(fiber.StatusOK).JSON(resp)
	}
}

func (h *LandingStatsHandler) cached(now time.Time) (LandingStatsResponse, bool) {
	h.cacheMu.RLock()
	defer h.cacheMu.RUnlock()
	if !h.cache.ok || !now.Before(h.cache.expiresAt) {
		return LandingStatsResponse{}, false
	}
	return h.cache.resp, true
}

func (h *LandingStatsHandler) store(resp LandingStatsResponse, expiresAt time.Time) {
	h.cacheMu.Lock()
	defer h.cacheMu.Unlock()
	h.cache = landingStatsCache{resp: resp, expiresAt: expiresAt, ok: true}
}

func (h *LandingStatsHandler) fetch(c *fiber.Ctx) (LandingStatsResponse, error) {
	var resp LandingStatsResponse
	err := h.db.Pool.QueryRow(c.Context(), `
WITH verified_projects AS (
  SELECT id
  FROM projects
  WHERE status = 'verified' AND deleted_at IS NULL
),
all_contributors AS (
  SELECT gi.author_login AS login
  FROM github_issues gi
  INNER JOIN verified_projects vp ON vp.id = gi.project_id
  WHERE gi.author_login IS NOT NULL AND gi.author_login != ''
  UNION
  SELECT gpr.author_login AS login
  FROM github_pull_requests gpr
  INNER JOIN verified_projects vp ON vp.id = gpr.project_id
  WHERE gpr.author_login IS NOT NULL AND gpr.author_login != ''
)
SELECT
  (SELECT COUNT(*) FROM verified_projects) AS active_projects,
  (SELECT COUNT(DISTINCT LOWER(login)) FROM all_contributors) AS contributors
`).Scan(&resp.ActiveProjects, &resp.Contributors)
	if err != nil {
		return LandingStatsResponse{}, err
	}

	// No payouts/grants table exists yet in the schema.
	resp.GrantsDistributedUSD = 0
	return resp, nil
}
