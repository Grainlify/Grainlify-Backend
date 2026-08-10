package handlers

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/github"
)

// DefaultLeaderboardCacheTTL is how long a computed ranking is reused.
//
// The leaderboard is global - it takes no per-user or per-request input - so
// one computation serves every caller, and contribution counts move on the
// timescale of GitHub syncs rather than of page loads. A minute of staleness
// is invisible to a reader and removes the query from the request path
// entirely.
const DefaultLeaderboardCacheTTL = 60 * time.Second

// leaderboardMaxStale bounds how long a *failing* refresh may keep serving the
// previous ranking. Serving a slightly old leaderboard beats returning 500 to
// every visitor over a transient database blip, but silently serving an
// arbitrarily old one would turn an outage into a correctness problem nobody
// can see.
const leaderboardMaxStale = 10 * time.Minute

// leaderboardMaxRows caps the materialised ranking so a pathological dataset
// cannot exhaust memory. Well above any realistic contributor count (~531 in
// production at the time of writing); crossing it is logged rather than
// silently truncating the tail.
const leaderboardMaxRows = 50000

// leaderboardQuery computes the entire ranking in one pass.
//
// It replaces a version that ran six correlated subqueries *per contributor*,
// each with a LOWER(author_login) predicate no index could serve. Because the
// ORDER BY sorts on a computed count, every contributor had to be evaluated
// before LIMIT/OFFSET could discard any of them - so each page cost a full
// O(contributors x contributions) evaluation and paging was flat at ~6.5s per
// page regardless of offset.
//
// Two correctness fixes come with the rewrite:
//
//  1. Contributors are grouped by LOWER(login). The old query took a
//     case-*sensitive* DISTINCT over logins but counted case-insensitively, so
//     a contributor appearing as both "Alice" and "alice" produced two rows
//     that each reported the combined total. Reproduced in the test database
//     ("assigned-bob"); latent in production.
//
//  2. The github_accounts join is a LATERAL ... LIMIT 1 rather than a plain
//     LEFT JOIN. github_accounts has no unique constraint on login and the
//     test database currently holds 50 login groups with more than one row;
//     a plain join fans out and emits one leaderboard row per duplicate
//     account.
const leaderboardQuery = `
WITH contributions AS (
    SELECT LOWER(i.author_login) AS login_key,
           i.author_login        AS login_raw,
           p.ecosystem_id        AS ecosystem_id
    FROM github_issues i
    JOIN projects p ON p.id = i.project_id
    WHERE p.status = 'verified'
      AND i.author_login IS NOT NULL
      AND i.author_login <> ''

    UNION ALL

    SELECT LOWER(pr.author_login),
           pr.author_login,
           p.ecosystem_id
    FROM github_pull_requests pr
    JOIN projects p ON p.id = pr.project_id
    WHERE p.status = 'verified'
      AND pr.author_login IS NOT NULL
      AND pr.author_login <> ''
),
totals AS (
    SELECT c.login_key,
           MIN(c.login_raw) AS login_raw,
           COUNT(*)         AS contribution_count,
           COALESCE(
               ARRAY_AGG(DISTINCT e.name ORDER BY e.name)
                   FILTER (WHERE e.name IS NOT NULL),
               ARRAY[]::TEXT[]
           ) AS ecosystems
    FROM contributions c
    LEFT JOIN ecosystems e
           ON e.id = c.ecosystem_id
          AND e.status = 'active'
    GROUP BY c.login_key
)
SELECT COALESCE(acct.login, t.login_raw) AS username,
       COALESCE(acct.avatar_url, '')     AS avatar_url,
       COALESCE(acct.user_id::text, '')  AS user_id,
       t.contribution_count,
       t.ecosystems
FROM totals t
LEFT JOIN LATERAL (
    SELECT ga.login, ga.avatar_url, ga.user_id
    FROM github_accounts ga
    WHERE LOWER(ga.login) = t.login_key
    ORDER BY ga.created_at DESC, ga.id DESC
    LIMIT 1
) acct ON TRUE
ORDER BY t.contribution_count DESC, t.login_key ASC
LIMIT $1
`

// leaderboardEntry is one ranked contributor, as computed by leaderboardQuery.
// Rank is deliberately not stored: it is the index into the ranked slice, so
// it cannot drift out of sync with the ordering.
type leaderboardEntry struct {
	Username      string
	Avatar        string
	UserID        string
	Contributions int
	Ecosystems    []string
}

// leaderboardCache holds the most recently computed ranking.
//
// The mutex is held across the refresh query on purpose: it makes concurrent
// misses collapse into a single database round trip instead of a thundering
// herd, which matters most in exactly the situation the cache exists for.
type leaderboardCache struct {
	mu        sync.Mutex
	ttl       time.Duration
	entries   []leaderboardEntry
	fetchedAt time.Time
}

// get returns the ranking, refreshing it if the cached copy has expired.
//
// The returned slice is shared by every concurrent caller and must be treated
// as read-only.
func (lc *leaderboardCache) get(ctx context.Context, pool db.DBPool) ([]leaderboardEntry, error) {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	if lc.entries != nil && lc.ttl > 0 && time.Since(lc.fetchedAt) < lc.ttl {
		return lc.entries, nil
	}

	entries, err := fetchLeaderboard(ctx, pool)
	if err != nil {
		// Prefer a slightly stale ranking over a 500 on a public page, but
		// only within a bounded window - past that the error is the honest
		// answer.
		if lc.entries != nil && time.Since(lc.fetchedAt) < leaderboardMaxStale {
			slog.Warn("leaderboard: refresh failed, serving cached ranking",
				"error", err,
				"age_seconds", int(time.Since(lc.fetchedAt).Seconds()),
			)
			return lc.entries, nil
		}
		return nil, err
	}

	lc.entries = entries
	lc.fetchedAt = time.Now()
	return lc.entries, nil
}

func fetchLeaderboard(ctx context.Context, pool db.DBPool) ([]leaderboardEntry, error) {
	rows, err := pool.Query(ctx, leaderboardQuery, leaderboardMaxRows)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	entries := make([]leaderboardEntry, 0, 128)
	for rows.Next() {
		var e leaderboardEntry
		if err := rows.Scan(&e.Username, &e.Avatar, &e.UserID, &e.Contributions, &e.Ecosystems); err != nil {
			return nil, err
		}
		if e.Avatar == "" {
			e.Avatar = github.AvatarURL(e.Username, 200)
		}
		if e.Ecosystems == nil {
			e.Ecosystems = []string{}
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(entries) == leaderboardMaxRows {
		slog.Warn("leaderboard: ranking hit the row cap, tail is not ranked",
			"cap", leaderboardMaxRows,
		)
	}
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
// lifetime. A ttl of zero disables caching, which is what a test wants when it
// seeds rows and immediately asserts on them.
func NewLeaderboardHandlerWithCacheTTL(d *db.DB, ttl time.Duration) *LeaderboardHandler {
	return &LeaderboardHandler{db: d, cache: &leaderboardCache{ttl: ttl}}
}

// Leaderboard returns top contributors ranked by contributions in verified
// projects. Ranking is computed once per cache period and paginated in memory,
// so an offset costs nothing.
func (h *LeaderboardHandler) Leaderboard() fiber.Handler {
	return func(c *fiber.Ctx) error {
		if h.db == nil || h.db.Pool == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "db_not_configured"})
		}

		limit := c.QueryInt("limit", 10)
		if limit < 1 {
			limit = 10
		}
		if limit > 100 {
			limit = 100
		}
		offset := c.QueryInt("offset", 0)
		if offset < 0 {
			offset = 0
		}

		entries, err := h.cache.get(c.Context(), h.db.Pool)
		if err != nil {
			slog.Error("failed to fetch leaderboard", "error", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "leaderboard_fetch_failed"})
		}

		// Always return an array, even when the offset is past the end.
		leaderboard := []fiber.Map{}
		for i := offset; i < len(entries) && i < offset+limit; i++ {
			e := entries[i]
			rank := i + 1
			rankTier := GetRankTier(rank)

			leaderboard = append(leaderboard, fiber.Map{
				"rank":           rank,
				"rank_tier":      string(rankTier),
				"rank_tier_name": GetRankTierDisplayName(rankTier),
				"username":       e.Username,
				"avatar":         e.Avatar,
				"user_id":        e.UserID,
				"contributions":  e.Contributions,
				"ecosystems":     e.Ecosystems,
				// Trend needs historical snapshots, which are not collected
				// yet; score mirrors the contribution count until then.
				"score":      e.Contributions,
				"trend":      "same",
				"trendValue": 0,
			})
		}

		return c.Status(fiber.StatusOK).JSON(leaderboard)
	}
}
