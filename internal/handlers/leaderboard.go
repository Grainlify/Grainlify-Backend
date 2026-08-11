package handlers

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/github"
	"github.com/jagadeesh/grainlify/backend/internal/ranking"
)

// DefaultLeaderboardCacheTTL is how long a computed ranking is reused.
//
// A ranking takes no per-user input, so one computation serves every caller
// with the same scope, and merge counts move on the timescale of GitHub
// syncs rather than of page loads. A minute of staleness is invisible to a
// reader and removes the query from the request path entirely.
const DefaultLeaderboardCacheTTL = 60 * time.Second

// leaderboardMaxStale bounds how long a *failing* refresh may keep serving
// the previous ranking. Serving a slightly old leaderboard beats returning
// 500 to every visitor over a transient database blip, but silently serving
// an arbitrarily old one would turn an outage into a correctness problem
// nobody can see.
const leaderboardMaxStale = 10 * time.Minute

// leaderboardScopes caps how many distinct (window, ecosystem) rankings are
// cached at once. The scope space is small and bounded by the UI - two
// windows times the active ecosystems plus "all" - but it is attacker-
// controlled via query string, so it gets a ceiling rather than an
// unbounded map. Past the cap the least-recently-refreshed scope is dropped.
const leaderboardScopes = 64

// leaderboardEntry is one cached ranking, whatever its scope.
type leaderboardScope struct {
	entries    []ranking.Entry
	orgEntries []ranking.OrgEntry
	fetchedAt  time.Time
}

// leaderboardCache holds the most recently computed ranking per scope.
//
// The mutex is held across the refresh query on purpose: it makes concurrent
// misses collapse into a single database round trip instead of a thundering
// herd, which matters most in exactly the situation the cache exists for.
type leaderboardCache struct {
	mu     sync.Mutex
	ttl    time.Duration
	scopes map[string]*leaderboardScope
}

func newLeaderboardCache(ttl time.Duration) *leaderboardCache {
	return &leaderboardCache{ttl: ttl, scopes: map[string]*leaderboardScope{}}
}

func scopeKey(kind string, opts ranking.Options) string {
	return kind + "|" + string(opts.Window) + "|" + opts.Ecosystem
}

// evictLocked drops the oldest scope when the map is full. Called with mu held.
func (lc *leaderboardCache) evictLocked() {
	if len(lc.scopes) < leaderboardScopes {
		return
	}
	var oldestKey string
	var oldest time.Time
	for k, s := range lc.scopes {
		if oldestKey == "" || s.fetchedAt.Before(oldest) {
			oldestKey, oldest = k, s.fetchedAt
		}
	}
	delete(lc.scopes, oldestKey)
}

// contributors returns the contributor ranking for a scope, refreshing it if
// the cached copy has expired. The returned slice is shared by every
// concurrent caller and must be treated as read-only.
func (lc *leaderboardCache) contributors(ctx context.Context, pool db.DBPool, opts ranking.Options) ([]ranking.Entry, error) {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	key := scopeKey("c", opts)
	cached := lc.scopes[key]
	if cached != nil && cached.entries != nil && lc.ttl > 0 && time.Since(cached.fetchedAt) < lc.ttl {
		return cached.entries, nil
	}

	entries, err := ranking.Contributors(ctx, pool, opts, time.Now())
	if err != nil {
		// Prefer a slightly stale ranking over a 500 on a public page, but
		// only within a bounded window - past that the error is the honest
		// answer.
		if cached != nil && cached.entries != nil && time.Since(cached.fetchedAt) < leaderboardMaxStale {
			slog.Warn("leaderboard: refresh failed, serving cached ranking",
				"error", err,
				"age_seconds", int(time.Since(cached.fetchedAt).Seconds()),
			)
			return cached.entries, nil
		}
		return nil, err
	}

	for i := range entries {
		if entries[i].Avatar == "" {
			entries[i].Avatar = github.AvatarURL(entries[i].Username, 200)
		}
	}

	lc.evictLocked()
	lc.scopes[key] = &leaderboardScope{entries: entries, fetchedAt: time.Now()}
	return entries, nil
}

// orgs is the same, for the organisation ranking.
func (lc *leaderboardCache) orgs(ctx context.Context, pool db.DBPool, opts ranking.Options) ([]ranking.OrgEntry, error) {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	key := scopeKey("o", opts)
	cached := lc.scopes[key]
	if cached != nil && cached.orgEntries != nil && lc.ttl > 0 && time.Since(cached.fetchedAt) < lc.ttl {
		return cached.orgEntries, nil
	}

	entries, err := ranking.Orgs(ctx, pool, opts, time.Now())
	if err != nil {
		if cached != nil && cached.orgEntries != nil && time.Since(cached.fetchedAt) < leaderboardMaxStale {
			slog.Warn("leaderboard: org refresh failed, serving cached ranking",
				"error", err,
				"age_seconds", int(time.Since(cached.fetchedAt).Seconds()),
			)
			return cached.orgEntries, nil
		}
		return nil, err
	}

	lc.evictLocked()
	lc.scopes[key] = &leaderboardScope{orgEntries: entries, fetchedAt: time.Now()}
	return entries, nil
}

type LeaderboardHandler struct {
	db    *db.DB
	cache *leaderboardCache
}

func NewLeaderboardHandler(d *db.DB) *LeaderboardHandler {
	return NewLeaderboardHandlerWithCacheTTL(d, DefaultLeaderboardCacheTTL)
}

// NewLeaderboardHandlerWithCacheTTL builds a handler with an explicit cache
// lifetime. A ttl of zero disables caching, which is what a test wants when
// it seeds rows and immediately asserts on them.
func NewLeaderboardHandlerWithCacheTTL(d *db.DB, ttl time.Duration) *LeaderboardHandler {
	return &LeaderboardHandler{db: d, cache: newLeaderboardCache(ttl)}
}

// leaderboardPaging reads and clamps limit/offset.
func leaderboardPaging(c *fiber.Ctx) (limit, offset int) {
	limit = c.QueryInt("limit", 10)
	if limit < 1 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	offset = c.QueryInt("offset", 0)
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// leaderboardOptions reads the scope query params shared by both boards.
func leaderboardOptions(c *fiber.Ctx) ranking.Options {
	return ranking.Options{
		Window:    ranking.ParseWindow(c.Query("window")),
		Ecosystem: c.Query("ecosystem"),
	}
}

// Leaderboard returns contributors ranked by merged pull requests in
// verified projects. Ranking is computed once per cache period per scope and
// paginated in memory, so an offset costs nothing.
//
// Query parameters:
//   - limit, offset: paging (limit clamped to 1..100)
//   - window: "season" (default, last 90 days) or "all"
//   - ecosystem: an ecosystems.slug; omitted means every ecosystem
func (h *LeaderboardHandler) Leaderboard() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}

		limit, offset := leaderboardPaging(c)
		opts := leaderboardOptions(c)

		entries, err := h.cache.contributors(c.Context(), h.db.Pool, opts)
		if err != nil {
			slog.Error("failed to fetch leaderboard", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "leaderboard_fetch_failed"})
		}

		// Always return an array, even when the offset is past the end.
		leaderboard := []fiber.Map{}
		for i := offset; i < len(entries) && i < offset+limit; i++ {
			e := entries[i]
			rankTier := GetRankTier(e.Rank)

			leaderboard = append(leaderboard, fiber.Map{
				"rank":           e.Rank,
				"rank_tier":      string(rankTier),
				"rank_tier_name": GetRankTierDisplayName(rankTier),
				"username":       e.Username,
				"avatar":         e.Avatar,
				"user_id":        e.UserID,
				"merged_prs":     e.MergedPRs,
				"ecosystems":     e.Ecosystems,
				// score is the ranked quantity under whatever the current
				// definition is; merged_prs names the specific event so a
				// client can label the column honestly.
				"score": e.MergedPRs,
			})
		}

		return c.Status(fiber.StatusOK).JSON(leaderboard)
	}
}

// orgActivityLabel buckets an open-issue count into the badge the projects
// table shows. Thresholds carried over unchanged from the browser-side
// version this replaces, so the badge does not silently change meaning at the
// same time as the ranking under it does.
func orgActivityLabel(openIssues int) string {
	switch {
	case openIssues > 10:
		return "Very High"
	case openIssues > 5:
		return "High"
	case openIssues > 2:
		return "Medium"
	default:
		return "Low"
	}
}

// LeaderboardProjects returns organisations ranked by how many distinct
// contributors landed a merged pull request in their verified repos.
//
// This used to be assembled in the browser from /projects/recommended: the
// top 50 repos by contributor count, grouped by owner, with each repo's
// contributor count SUMmed. That made orgs outside the top-50 sample
// invisible and double-counted anyone active in two repos of the same org.
// Ranking here, over the full set and with COUNT(DISTINCT contributor), is
// the answer the column always claimed to show.
//
// Takes the same limit/offset/window/ecosystem parameters as Leaderboard.
func (h *LeaderboardHandler) LeaderboardProjects() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}

		limit, offset := leaderboardPaging(c)
		opts := leaderboardOptions(c)

		entries, err := h.cache.orgs(c.Context(), h.db.Pool, opts)
		if err != nil {
			slog.Error("failed to fetch project leaderboard", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "leaderboard_fetch_failed"})
		}

		out := []fiber.Map{}
		for i := offset; i < len(entries) && i < offset+limit; i++ {
			e := entries[i]
			out = append(out, fiber.Map{
				"rank":         e.Rank,
				"name":         e.OrgLogin,
				"logo":         github.AvatarURL(e.OrgLogin, 200),
				"contributors": e.Contributors,
				"merged_prs":   e.MergedPRs,
				"open_issues":  e.OpenIssues,
				"activity":     orgActivityLabel(e.OpenIssues),
				"ecosystems":   e.Ecosystems,
				"score":        e.Contributors,
			})
		}

		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"projects": out,
			"total":    len(entries),
			"limit":    limit,
			"offset":   offset,
		})
	}
}
