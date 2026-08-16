package handlers

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/github"
)

// Resolving is_fork for projects indexed before the column existed.
//
// This ships WITH the ranking exclusion rather than after it, because a stored
// column nothing reads is a fix that has not happened - and here the gap has
// teeth: until every row is resolved, a fork sits in the leaderboard's
// eligible set exactly as it did before.
//
// It runs at startup and does nothing on a resolved database, so it is safe to
// leave in place: after the first successful pass every project already
// carries a value, and every write path sets one at creation.
//
// Tokens come from the installation each project was indexed under, so no new
// credential is introduced. A project whose installation has gone away cannot
// be resolved and is reported rather than guessed at - guessing false there
// would silently re-open the vector for precisely the repos we know least
// about.

type ForkBackfiller struct {
	db  *db.DB
	cfg config.Config
	// fetch reports whether a repo is a fork. Injected so the test does not
	// need GitHub.
	fetch func(ctx context.Context, token, fullName string) (bool, error)
	// token resolves an installation token. Injected for the same reason - and
	// because its FAILURE path is the one that matters most: an installation
	// we cannot read must leave rows unknown rather than assume them safe.
	token func(ctx context.Context, installationID string) (string, error)
}

func NewForkBackfiller(cfg config.Config, d *db.DB) *ForkBackfiller {
	client := github.NewClient()
	return &ForkBackfiller{
		db:  d,
		cfg: cfg,
		fetch: func(ctx context.Context, token, fullName string) (bool, error) {
			repo, err := client.GetRepo(ctx, token, fullName)
			if err != nil {
				return false, err
			}
			return repo.Fork, nil
		},
	}
}

// Run resolves every project with an unknown fork state.
func (b *ForkBackfiller) Run(ctx context.Context) {
	if b.db == nil || b.db.Pool == nil {
		slog.Warn("fork backfill: no database, skipping")
		return
	}

	pending, err := b.pendingByInstallation(ctx)
	if err != nil {
		slog.Error("fork backfill: could not list projects", "error", err)
		return
	}
	if len(pending) == 0 {
		return // Already resolved. The normal case after the first run.
	}

	getToken := b.token
	if getToken == nil {
		appClient, err := github.NewGitHubAppClient(b.cfg.GitHubAppID, b.cfg.GitHubAppPrivateKey)
		if err != nil {
			slog.Error("fork backfill: GitHub App not configured, fork state cannot be resolved",
				"error", err,
				"note", "until this is resolved, forks remain eligible for ranking")
			return
		}
		getToken = appClient.GetInstallationToken
	}

	var resolved, forks, failed int
	for installationID, projects := range pending {
		token, err := getToken(ctx, installationID)
		if err != nil {
			// The installation is gone or the key is wrong. Either way these
			// rows stay unknown rather than being assumed safe.
			slog.Error("fork backfill: no installation token, projects left unresolved",
				"installation_id", installationID, "projects", len(projects), "error", err)
			failed += len(projects)
			continue
		}
		for _, p := range projects {
			isFork, err := b.fetch(ctx, token, p.fullName)
			if err != nil {
				slog.Warn("fork backfill: could not fetch repo",
					"repo", p.fullName, "error", err)
				failed++
				continue
			}
			if _, err := b.db.Pool.Exec(ctx, `
UPDATE projects SET is_fork = $2, fork_checked_at = now(), updated_at = now() WHERE id = $1
`, p.id, isFork); err != nil {
				slog.Error("fork backfill: update failed", "repo", p.fullName, "error", err)
				failed++
				continue
			}
			resolved++
			if isFork {
				forks++
				slog.Info("fork backfill: project is a fork, now excluded from ranking",
					"repo", p.fullName, "project_id", p.id)
			}
		}
	}

	slog.Info("fork backfill complete", "resolved", resolved, "forks_found", forks, "unresolved", failed)
	b.reportRemainingUnknowns(ctx)
}

type pendingProject struct {
	id       uuid.UUID
	fullName string
}

func (b *ForkBackfiller) pendingByInstallation(ctx context.Context) (map[string][]pendingProject, error) {
	rows, err := b.db.Pool.Query(ctx, `
SELECT id, github_full_name, COALESCE(github_app_installation_id, '')
FROM projects
WHERE is_fork IS NULL AND github_app_installation_id IS NOT NULL
ORDER BY github_app_installation_id, github_full_name
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string][]pendingProject{}
	for rows.Next() {
		var p pendingProject
		var installationID string
		if err := rows.Scan(&p.id, &p.fullName, &installationID); err != nil {
			return nil, err
		}
		if installationID == "" {
			continue
		}
		out[installationID] = append(out[installationID], p)
	}
	return out, rows.Err()
}

// reportRemainingUnknowns is the loud half of the permissive default.
//
// An unresolved project still counts toward ranking, so "how many are
// unresolved" is the size of the hole, and it must be visible rather than
// inferable.
func (b *ForkBackfiller) reportRemainingUnknowns(ctx context.Context) {
	var n int
	if err := b.db.Pool.QueryRow(ctx, `
SELECT count(*) FROM projects
WHERE is_fork IS NULL AND deleted_at IS NULL AND status = 'verified'
`).Scan(&n); err != nil {
		return
	}
	if n > 0 {
		slog.Error("fork backfill: LIVE VERIFIED PROJECTS WITH UNKNOWN FORK STATE - these still count toward ranking",
			"count", n)
	}
}
