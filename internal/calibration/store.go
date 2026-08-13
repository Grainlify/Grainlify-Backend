package calibration

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Storage for the labelling tool.
//
// Two pools, deliberately. Candidates are read from the product database; the
// sample, the snapshots and the labels are written to a local one. The tool
// never writes to production - not because a write would necessarily be
// harmful, but because "it only reads" is a property worth being structural
// rather than remembered.

// LoadCandidates reads the indexed pull requests eligible for sampling.
//
// Read-only, and the only query this tool runs against the product database.
func LoadCandidates(ctx context.Context, source *pgxpool.Pool) ([]Candidate, error) {
	rows, err := source.Query(ctx, `
SELECT pr.id, p.github_full_name, pr.number, COALESCE(pr.merged, false)
FROM github_pull_requests pr
JOIN projects p ON p.id = pr.project_id
WHERE pr.number IS NOT NULL
ORDER BY pr.id
`)
	if err != nil {
		return nil, fmt.Errorf("load candidates: %w", err)
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		var c Candidate
		if err := rows.Scan(&c.PullRequestID, &c.ProjectFullName, &c.Number, &c.Merged); err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate candidates: %w", err)
	}
	return out, nil
}

// AlreadySampled returns every pull request any previous draw took, so a new
// draw can exclude them and the two sets never overlap.
func AlreadySampled(ctx context.Context, local *pgxpool.Pool) (map[uuid.UUID]bool, error) {
	rows, err := local.Query(ctx, `SELECT pull_request_id FROM calibration_sample_prs`)
	if err != nil {
		return nil, fmt.Errorf("load sampled ids: %w", err)
	}
	defer rows.Close()

	out := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// PersistDraw writes a sample and its rows in one transaction.
//
// All or nothing: a half-written sample would have a recorded seed and pool
// that no longer describe the rows beside it, which is worse than no sample.
func PersistDraw(ctx context.Context, local *pgxpool.Pool, name string, d Draw) (uuid.UUID, error) {
	strata, err := json.Marshal(map[string]any{
		"plan":        d.Plan,
		"relaxations": d.Relaxations,
		"examined":    d.Examined,
	})
	if err != nil {
		return uuid.Nil, err
	}

	tx, err := local.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var sampleID uuid.UUID
	err = tx.QueryRow(ctx, `
INSERT INTO calibration_samples (name, seed, candidate_pr_ids, candidate_hash, strata, notes)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING id
`, name, d.Seed, d.CandidateIDs, d.CandidateHash, strata,
		"Three repositories hold 97% of the indexed corpus, so this set measures agreement on Stellopay-shaped work, not cross-project generalisation.",
	).Scan(&sampleID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("insert sample: %w", err)
	}

	for _, s := range d.Selected {
		_, err := tx.Exec(ctx, `
INSERT INTO calibration_sample_prs
  (sample_id, pull_request_id, project_full_name, merged, size_band, held_back)
VALUES ($1, $2, $3, $4, $5, $6)
`, sampleID, s.PullRequestID, s.ProjectFullName, s.Merged, string(s.Band), s.HeldBack)
		if err != nil {
			return uuid.Nil, fmt.Errorf("insert sample pr %s: %w", s.PullRequestID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	return sampleID, nil
}
