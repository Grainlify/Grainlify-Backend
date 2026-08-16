// Package ranking is the single definition of what a Grainlify contribution
// is worth and how contributors and organisations are ordered by it.
//
// It exists because the ranking used to be written out four times - the
// public leaderboard, two different queries on the user profile, and the org
// profile badge - and the four had already drifted apart:
//
//   - The leaderboard grouped contributors by LOWER(login); the first profile
//     query compared logins case-sensitively, so a contributor whose commits
//     carried two spellings ranked differently on their own profile than on
//     the board.
//   - The first profile query ranked only logins present in github_accounts
//     joined to users - i.e. only contributors who had signed up - so an
//     unregistered contributor above you on the public board did not exist in
//     the ranking your badge was computed from, and every badge below them
//     was one place too good.
//   - None of the four excluded bots.
//
// Every one of those is the same bug: a ranking rule expressed more than
// once. So the rule is expressed here exactly once, as SQL, and every caller
// asks this package rather than writing its own. Adding a criterion means
// editing rankedContributors below and nothing else.
//
// # Deliberate non-dependency
//
// Nothing under internal/hackathon may import this package. Leaderboard
// standing is presentational and must never become an input to draw weights,
// maintainer-pool scoring, application outcomes, or payouts - a rank that
// pays would turn a public vanity metric into a farming target. That rule is
// enforced by TestHackathonDoesNotImportRanking rather than by memory.
package ranking

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// DefaultSeasonWindow is how far back the default ("season") board looks.
//
// An all-time cumulative count is uncatchable: whoever was present when the
// first projects were onboarded holds the top of the board permanently, and
// a contributor arriving later correctly concludes that rank is not
// something available to them. A rolling window makes the board reflect who
// is active now, which is the only question it is useful for. All-time
// remains available as an explicit secondary view.
//
// 90 days rather than "the current GrainHack": the window has to be
// well-defined when no event is running, and tying the public board's
// definition to hackathon state would also create exactly the ranking ->
// hackathon coupling this package's guard test exists to prevent.
const DefaultSeasonWindow = 90 * 24 * time.Hour

// maxRows caps a materialised ranking so a pathological dataset cannot
// exhaust memory. Well above any realistic contributor count.
const maxRows = 50000

// Window selects the time range a ranking covers.
type Window string

const (
	// WindowSeason is the default: merges within DefaultSeasonWindow.
	WindowSeason Window = "season"
	// WindowAllTime ranks every merge ever recorded.
	WindowAllTime Window = "all"
)

// ParseWindow maps a query-parameter value to a Window, defaulting to
// season. An unrecognised value is not an error: this is a public,
// unauthenticated page and a typo in a shared URL should show the default
// board rather than a 400.
func ParseWindow(s string) Window {
	if s == string(WindowAllTime) {
		return WindowAllTime
	}
	return WindowSeason
}

// Since returns the inclusive lower bound for a window, or nil for all-time.
// Passed to SQL as a nullable timestamptz so one query serves both.
func (w Window) Since(now time.Time) *time.Time {
	if w == WindowAllTime {
		return nil
	}
	t := now.Add(-DefaultSeasonWindow)
	return &t
}

// Options scopes a ranking. The zero value is the default board: season
// window, all ecosystems.
type Options struct {
	Window Window
	// Ecosystem is an ecosystems.slug. Empty means every ecosystem.
	Ecosystem string
}

func (o Options) ecosystem() *string {
	if o.Ecosystem == "" {
		return nil
	}
	return &o.Ecosystem
}

// rankedContributors is THE definition of the contributor ranking.
//
// What counts, and why:
//
//   - A merged pull request in a verified project. Nothing else. The previous
//     definition counted authored issues and authored pull requests equally
//     and never read merge status at all, which meant the board could be
//     topped by opening pull requests and never landing them, and that filing
//     an issue scored the same as shipping a fix. On this platform issues are
//     largely authored by maintainers scoping work, so counting them also
//     ranked maintainers for creating the work rather than doing it. Merged
//     PRs are the one event that is unambiguously delivered contribution.
//
//   - Merge is detected as (merged OR merged_at_github IS NOT NULL), not
//     merged alone. The `merged` column is false on every row synced through
//     the repo-list path, because GitHub's "list pull requests" response has
//     no `merged` field - only `merged_at` - so it silently unmarshalled to
//     false. At the time of writing 1299 production rows carry a merge
//     timestamp and 0 have merged = true. That sync bug is fixed separately,
//     but the historical rows are already written, so the predicate accepts
//     either signal and does not depend on a backfill having run.
//
//   - merge_commit_sha is deliberately NOT a merge signal. GitHub populates
//     it with a test-merge commit for open and closed-unmerged PRs too.
//
//   - Bots are excluded by login suffix. GitHub App accounts always end in
//     "[bot]"; production currently has dependabot[bot], vercel[bot],
//     github-actions[bot], and grantfox-oss[bot] - the last being Grainlify's
//     own app, which was ranking against the humans it exists to serve.
//
// Forks are not eligible, anywhere.
//
// A fork is a repository the contributor controls. They can open a pull
// request against it and merge it themselves, with nobody reviewing anything.
// Ranking counts merged pull requests precisely because a merge means somebody
// else accepted the work - so counting merges inside a fork converts the one
// number the platform claims is unfarmable into a number anyone can mint on
// demand: install the App on all repositories, fork any repo, merge into it.
//
// Written once and referenced by every query below rather than retyped. The
// eligibility predicate is the thing an unrelated change is most likely to
// "simplify", and TestEligibilityPredicateAppliesEverywhere reads this file to
// assert no verified-project gate exists without it.
//
// COALESCE treats an undetermined project as eligible. That is deliberate and
// it is the weaker of two bad options: making unknown ineligible empties the
// leaderboard for every contributor the moment the column is added and before
// the backfill has run. Every write path sets is_fork at creation and the
// backfill resolves existing rows, so unknown is a transient state rather than
// a resting one - and BackfillForks logs loudly if any live verified project
// is still unknown after it runs.
const notAFork = "AND COALESCE(p.is_fork, FALSE) = FALSE"

// notAForkP2 is the same predicate for the p2 alias used by the org open-issue
// subquery. Two spellings, one meaning - the test below checks both.
const notAForkP2 = "AND COALESCE(p2.is_fork, FALSE) = FALSE"

// Placeholders are fixed so every caller shares them: $1 = window start
// (nullable timestamptz), $2 = ecosystem slug (nullable text).
const rankedContributors = `
WITH merges AS (
    SELECT LOWER(pr.author_login) AS login_key,
           pr.author_login        AS login_raw,
           p.ecosystem_id         AS ecosystem_id
    FROM github_pull_requests pr
    JOIN projects p ON p.id = pr.project_id
    WHERE p.status = 'verified'
      AND p.deleted_at IS NULL
      ` + notAFork + `
      AND (pr.merged = TRUE OR pr.merged_at_github IS NOT NULL)
      AND pr.author_login IS NOT NULL
      AND pr.author_login <> ''
      AND pr.author_login NOT LIKE '%[bot]'
      AND (
        $1::timestamptz IS NULL
        OR COALESCE(pr.merged_at_github, pr.closed_at_github) >= $1::timestamptz
      )
      AND (
        $2::text IS NULL
        OR EXISTS (
            SELECT 1 FROM ecosystems e
            WHERE e.id = p.ecosystem_id
              AND e.status = 'active'
              AND e.slug = $2::text
        )
      )
),
totals AS (
    SELECT m.login_key,
           MIN(m.login_raw) AS login_raw,
           COUNT(*)         AS merged_prs,
           COALESCE(
               ARRAY_AGG(DISTINCT e.name ORDER BY e.name)
                   FILTER (WHERE e.name IS NOT NULL),
               ARRAY[]::TEXT[]
           ) AS ecosystems
    FROM merges m
    LEFT JOIN ecosystems e
           ON e.id = m.ecosystem_id
          AND e.status = 'active'
    GROUP BY m.login_key
),
ranked AS (
    SELECT t.*,
           ROW_NUMBER() OVER (
               ORDER BY t.merged_prs DESC, t.login_key ASC
           ) AS rank
    FROM totals t
)
`

// Entry is one ranked contributor.
type Entry struct {
	Rank       int
	Username   string
	Avatar     string
	UserID     string
	MergedPRs  int
	Ecosystems []string
}

// entriesQuery materialises the whole ranking.
//
// The github_accounts join is a LATERAL ... LIMIT 1 rather than a plain LEFT
// JOIN: github_accounts has no unique constraint on login, so a plain join
// fans out and emits one leaderboard row per duplicate account.
const entriesQuery = rankedContributors + `
SELECT r.rank,
       COALESCE(acct.login, r.login_raw) AS username,
       COALESCE(acct.avatar_url, '')     AS avatar_url,
       COALESCE(acct.user_id::text, '')  AS user_id,
       r.merged_prs,
       r.ecosystems
FROM ranked r
LEFT JOIN LATERAL (
    SELECT ga.login, ga.avatar_url, ga.user_id
    FROM github_accounts ga
    WHERE LOWER(ga.login) = r.login_key
    ORDER BY ga.created_at DESC, ga.id DESC
    LIMIT 1
) acct ON TRUE
ORDER BY r.rank
LIMIT $3
`

// Contributors returns the full ranking, ordered best first.
func Contributors(ctx context.Context, pool db.DBPool, opts Options, now time.Time) ([]Entry, error) {
	rows, err := pool.Query(ctx, entriesQuery, opts.Window.Since(now), opts.ecosystem(), maxRows)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	entries := make([]Entry, 0, 128)
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Rank, &e.Username, &e.Avatar, &e.UserID, &e.MergedPRs, &e.Ecosystems); err != nil {
			return nil, err
		}
		if e.Ecosystems == nil {
			e.Ecosystems = []string{}
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

// positionQuery asks for one contributor's rank.
//
// It wraps the identical rankedContributors CTE rather than re-deriving the
// ordering, which is the whole point of this package: a profile badge and
// the public board cannot disagree, because there is only one ORDER BY. Note
// the filter is applied AFTER ROW_NUMBER() - filtering to the user first
// would rank them 1st every time.
const positionQuery = rankedContributors + `
SELECT r.rank, r.merged_prs
FROM ranked r
WHERE r.login_key = LOWER($3::text)
`

// Position returns a contributor's rank and merged-PR count under the same
// rules as Contributors. A contributor with no qualifying merges is absent
// from the ranking entirely and gets (nil, 0, nil) - not rank 0, which a
// caller could mistake for a real position.
func Position(ctx context.Context, pool db.DBPool, login string, opts Options, now time.Time) (*int, int, error) {
	var rank int
	var mergedPRs int
	err := pool.QueryRow(ctx, positionQuery, opts.Window.Since(now), opts.ecosystem(), login).
		Scan(&rank, &mergedPRs)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	return &rank, mergedPRs, nil
}

// rankedOrgs is the organisation ranking, and it is deliberately built on
// the same merges CTE as the contributor board so the two cannot disagree
// about what a contribution is.
//
// It ranks by COUNT(DISTINCT contributor), which is what the org board
// always claimed to show. The version this replaces was computed in the
// browser: it fetched the top 50 repos, grouped them by owner, and SUMmed
// each repo's contributor count. That was wrong twice over - an org whose
// repos all fell outside the top 50 was invisible, and anyone who
// contributed to two repos in the same org was counted twice, so an org
// could out-rank another on strictly fewer distinct people.
const rankedOrgs = `
WITH merges AS (
    SELECT LOWER(pr.author_login)                        AS login_key,
           SPLIT_PART(p.github_full_name, '/', 1)        AS org_login,
           p.ecosystem_id                                AS ecosystem_id
    FROM github_pull_requests pr
    JOIN projects p ON p.id = pr.project_id
    WHERE p.status = 'verified'
      AND p.deleted_at IS NULL
      ` + notAFork + `
      AND SPLIT_PART(p.github_full_name, '/', 2) <> '.github'
      AND (pr.merged = TRUE OR pr.merged_at_github IS NOT NULL)
      AND pr.author_login IS NOT NULL
      AND pr.author_login <> ''
      AND pr.author_login NOT LIKE '%[bot]'
      AND (
        $1::timestamptz IS NULL
        OR COALESCE(pr.merged_at_github, pr.closed_at_github) >= $1::timestamptz
      )
      AND (
        $2::text IS NULL
        OR EXISTS (
            SELECT 1 FROM ecosystems e
            WHERE e.id = p.ecosystem_id
              AND e.status = 'active'
              AND e.slug = $2::text
        )
      )
),
totals AS (
    SELECT LOWER(m.org_login)                     AS org_key,
           MIN(m.org_login)                       AS org_login,
           COUNT(DISTINCT m.login_key)            AS contributors,
           COUNT(*)                               AS merged_prs,
           COALESCE(
               ARRAY_AGG(DISTINCT e.name ORDER BY e.name)
                   FILTER (WHERE e.name IS NOT NULL),
               ARRAY[]::TEXT[]
           ) AS ecosystems
    FROM merges m
    LEFT JOIN ecosystems e
           ON e.id = m.ecosystem_id
          AND e.status = 'active'
    GROUP BY LOWER(m.org_login)
),
ranked AS (
    SELECT t.*,
           ROW_NUMBER() OVER (
               ORDER BY t.contributors DESC, t.merged_prs DESC, t.org_key ASC
           ) AS rank
    FROM totals t
)
`

// OrgEntry is one ranked organisation.
type OrgEntry struct {
	Rank         int
	OrgLogin     string
	Contributors int
	MergedPRs    int
	OpenIssues   int
	Ecosystems   []string
}

// orgEntriesQuery adds the open-issue count, which drives the "activity"
// badge in the UI. It is not part of the ranking - the ORDER BY does not see
// it - it is presentation carried alongside, and it is computed here only
// because the browser used to compute it from a sample of 50 repos.
const orgEntriesQuery = rankedOrgs + `
SELECT r.rank,
       r.org_login,
       r.contributors,
       r.merged_prs,
       COALESCE((
           SELECT COUNT(*)
           FROM github_issues gi
           JOIN projects p2 ON p2.id = gi.project_id
           WHERE gi.state = 'open'
             AND p2.status = 'verified'
             AND p2.deleted_at IS NULL
             ` + notAForkP2 + `
             AND LOWER(SPLIT_PART(p2.github_full_name, '/', 1)) = r.org_key
       ), 0) AS open_issues,
       r.ecosystems
FROM ranked r
ORDER BY r.rank
LIMIT $3
`

// Orgs returns the organisation ranking, ordered best first.
func Orgs(ctx context.Context, pool db.DBPool, opts Options, now time.Time) ([]OrgEntry, error) {
	rows, err := pool.Query(ctx, orgEntriesQuery, opts.Window.Since(now), opts.ecosystem(), maxRows)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	entries := make([]OrgEntry, 0, 64)
	for rows.Next() {
		var e OrgEntry
		if err := rows.Scan(&e.Rank, &e.OrgLogin, &e.Contributors, &e.MergedPRs, &e.OpenIssues, &e.Ecosystems); err != nil {
			return nil, err
		}
		if e.Ecosystems == nil {
			e.Ecosystems = []string{}
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

const orgPositionQuery = rankedOrgs + `
SELECT r.rank, r.contributors
FROM ranked r
WHERE r.org_key = LOWER($3::text)
`

// OrgPosition returns one organisation's rank under the same rules as Orgs.
func OrgPosition(ctx context.Context, pool db.DBPool, orgLogin string, opts Options, now time.Time) (*int, int, error) {
	var rank, contributors int
	err := pool.QueryRow(ctx, orgPositionQuery, opts.Window.Since(now), opts.ecosystem(), orgLogin).
		Scan(&rank, &contributors)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	return &rank, contributors, nil
}
