package handlers

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/github"
)

// Filling in description and topics for projects indexed before the sync
// stored them.
//
// InstallationRepository decoded Description and Topics all along; the sync's
// INSERT never listed description, so every project created before that fix
// has none. Third field found decoded and thrown away, after fork and
// owner.type - and the reason nothing caught it is that no test compares the
// INSERT's column list against what the struct parses.
//
// A boot-time backfill rather than a script, for the same reason as the fork
// one: a script somebody has to remember to run is a gate that can be skipped,
// and the 106 rows this exists for have been waiting since January.
//
// WHAT IT DOES NOT DO: it never touches needs_metadata. That flag is set by a
// human opening the setup form, and it is the only signal of intent anywhere
// in the pipeline - everything arrives through "All repositories" and nobody
// selects anything. Flipping it here would put projects nobody chose in front
// of contributors, which is the case that ended with a maintainer uninstalling
// rather than have 30 swept-in repositories listed under his name. Filling the
// fields only means the form has less to ask for.
type MetadataBackfiller struct {
	db  *db.DB
	cfg config.Config
	// Injected for the test; nil uses the real client.
	fetch func(ctx context.Context, token, fullName string) (description string, topics []string, err error)
	token func(ctx context.Context, installationID string) (string, error)
}

func NewMetadataBackfiller(cfg config.Config, d *db.DB) *MetadataBackfiller {
	client := github.NewClient()
	return &MetadataBackfiller{
		db:  d,
		cfg: cfg,
		fetch: func(ctx context.Context, token, fullName string) (string, []string, error) {
			repo, err := client.GetRepo(ctx, token, fullName)
			if err != nil {
				return "", nil, err
			}
			return repo.Description, nil, nil
		},
	}
}

type pendingMetadata struct {
	id       uuid.UUID
	fullName string
}

// Run fills in what it can. A no-op once every project has a description.
func (b *MetadataBackfiller) Run(ctx context.Context) {
	if b.db == nil || b.db.Pool == nil {
		slog.Warn("metadata backfill: no database, skipping")
		return
	}

	byInstallation, err := b.pending(ctx)
	if err != nil {
		slog.Error("metadata backfill: could not list projects", "error", err)
		return
	}
	if len(byInstallation) == 0 {
		return
	}

	getToken := b.token
	if getToken == nil {
		appClient, err := github.NewGitHubAppClient(b.cfg.GitHubAppID, b.cfg.GitHubAppPrivateKey)
		if err != nil {
			slog.Error("metadata backfill: GitHub App not configured", "error", err)
			return
		}
		getToken = appClient.GetInstallationToken
	}

	var filled, failed int
	for installationID, projects := range byInstallation {
		token, err := getToken(ctx, installationID)
		if err != nil {
			// Unreadable installation: leave the rows alone rather than write
			// an empty description over nothing. Same principle as the fork
			// backfill - an unknown stays unknown.
			slog.Warn("metadata backfill: no installation token",
				"installation_id", installationID, "projects", len(projects), "error", err)
			failed += len(projects)
			continue
		}
		for _, p := range projects {
			description, topics, err := b.fetch(ctx, token, p.fullName)
			if err != nil {
				slog.Warn("metadata backfill: could not fetch repo", "repo", p.fullName, "error", err)
				failed++
				continue
			}
			if description == "" && len(topics) == 0 {
				// GitHub has nothing to give for this repo. Not a failure, and
				// not something a retry will change.
				continue
			}
			var topicsJSON []byte
			if len(topics) > 0 {
				topicsJSON, _ = json.Marshal(topics)
			}
			if _, err := b.db.Pool.Exec(ctx, `
UPDATE projects
SET description = COALESCE(NULLIF($2, ''), description),
    tags = CASE WHEN $3::jsonb IS NOT NULL AND $3::jsonb::text NOT IN ('[]','null')
                THEN $3::jsonb ELSE tags END,
    updated_at = now()
WHERE id = $1
`, p.id, description, topicsJSON); err != nil {
				slog.Error("metadata backfill: update failed", "repo", p.fullName, "error", err)
				failed++
				continue
			}
			filled++
		}
	}
	slog.Info("metadata backfill complete", "filled", filled, "unresolved", failed)
}

func (b *MetadataBackfiller) pending(ctx context.Context) (map[string][]pendingMetadata, error) {
	rows, err := b.db.Pool.Query(ctx, `
SELECT id, github_full_name, COALESCE(github_app_installation_id, '')
FROM projects
WHERE deleted_at IS NULL
  AND (description IS NULL OR description = '')
  AND github_app_installation_id IS NOT NULL
ORDER BY github_app_installation_id, github_full_name
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string][]pendingMetadata{}
	for rows.Next() {
		var p pendingMetadata
		var installationID string
		if err := rows.Scan(&p.id, &p.fullName, &installationID); err != nil {
			return nil, err
		}
		if installationID != "" {
			out[installationID] = append(out[installationID], p)
		}
	}
	return out, rows.Err()
}
