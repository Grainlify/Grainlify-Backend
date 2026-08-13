package calibration

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// Storage for the labelling tool.
//
// Two pools, deliberately. Candidates are read from the product database; the
// sample, the snapshots and the labels are written to a local one. The tool
// never writes to production - not because a write would necessarily be
// harmful, but because "it only reads" is a property worth being structural
// rather than remembered.

// nullIfEmpty keeps the column NULL for the original framing rather than
// storing an empty string, so "no framing recorded" and "framed as the whole
// contribution" stay distinguishable.
func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func pct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(int(1000*float64(n)/float64(total))) / 10
}

// LoadCandidates reads the indexed pull requests eligible for sampling.
//
// Read-only, and the only query this tool runs against the product database.
func LoadCandidates(ctx context.Context, source db.DBPool) ([]Candidate, error) {
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

// RecordedPool returns the candidate ids one earlier draw recorded.
//
// A later draw runs against THIS list rather than a fresh query, so the two
// sets come from the same population. The corpus grows as sync runs; drawing
// set-2 from today's corpus would mean the two sets were drawn from different
// populations, and any difference between them would be partly that.
func RecordedPool(ctx context.Context, local db.DBPool, sampleName string) ([]uuid.UUID, string, error) {
	var ids []uuid.UUID
	var hash string
	err := local.QueryRow(ctx, `
SELECT candidate_pr_ids, candidate_hash FROM calibration_samples WHERE name = $1
`, sampleName).Scan(&ids, &hash)
	return ids, hash, err
}

// AlreadySampled returns every pull request any previous draw took, so a new
// draw can exclude them and the two sets never overlap.
func AlreadySampled(ctx context.Context, local db.DBPool) (map[uuid.UUID]bool, error) {
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
func PersistDraw(ctx context.Context, local db.DBPool, name string, d Draw, framing string) (uuid.UUID, error) {
	unmerged := 0
	for _, s := range d.Selected {
		if !s.Merged {
			unmerged++
		}
	}
	// The mix is recorded next to the corpus it was drawn from, so an
	// agreement rate read later sits beside what it was measured against.
	strata, err := json.Marshal(map[string]any{
		"plan":        d.Plan,
		"relaxations": d.Relaxations,
		"examined":    d.Examined,
		"mix": map[string]any{
			"total":               len(d.Selected),
			"unmerged":            unmerged,
			"merged":              len(d.Selected) - unmerged,
			"unmerged_pct":        pct(unmerged, len(d.Selected)),
			"corpus_total":        d.CorpusTotal,
			"corpus_unmerged":     d.CorpusUnmerged,
			"corpus_unmerged_pct": pct(d.CorpusUnmerged, d.CorpusTotal),
			"note":                "Unmerged is deliberately over-sampled against the corpus proportion: that is where human and model disagree. An agreement rate from this set is not comparable to one measured on a proportional sample.",
		},
	})
	if err != nil {
		return uuid.Nil, err
	}

	tx, err := local.BeginTx(ctx, pgx.TxOptions{})
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
  (sample_id, pull_request_id, pr_number, project_full_name, merged, size_band, held_back, labelling_framing)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
`, sampleID, s.PullRequestID, s.Number, s.ProjectFullName, s.Merged, string(s.Band), s.HeldBack, nullIfEmpty(framing))
		if err != nil {
			return uuid.Nil, fmt.Errorf("insert sample pr %s: %w", s.PullRequestID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	return sampleID, nil
}
