package hackathon

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/chain"
	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// ErrDrawCommitNotReady means an issue's draw-seed commit is missing or not
// yet confirmed on its chain.
//
// §5.3: if the commit has not confirmed before the window closes, do not run
// the draw - extend the window and alert. A draw without a confirmed prior
// commit provides no guarantee at all, and running it anyway silently
// converts a verifiable draw into a trusted one. Nobody outside can tell the
// difference afterwards, which is exactly why it must be refused here rather
// than flagged later.
var ErrDrawCommitNotReady = errors.New("draw-seed commit is not confirmed on chain")

// DrawSeedRecord is the stored commit-reveal state for one issue.
type DrawSeedRecord struct {
	IssueID    uuid.UUID
	ChainID    string
	CommitTx   string
	Committed  bool
	CommitHash []byte
	RevealedTx string
}

// CheckDrawCommit decides whether an issue's draw may run.
//
// Returns nil when the issue is not on a chain at all: an event with no chain
// pools has nothing to commit to and must keep drawing exactly as before.
// That is the majority case, and making the on-chain guarantee optional is
// what lets it be adopted per event rather than all at once.
func CheckDrawCommit(
	ctx context.Context,
	pool db.DBPool,
	reg *chain.Registry,
	hackathonID, issueID uuid.UUID,
	issueNumber int,
) error {
	if reg == nil {
		return nil
	}

	var chainID string
	if err := pool.QueryRow(ctx, `
SELECT COALESCE(chain_id, '') FROM hackathon_issues WHERE id = $1
`, issueID).Scan(&chainID); err != nil {
		return fmt.Errorf("hackathon.CheckDrawCommit: load issue chain: %w", err)
	}
	if chainID == "" {
		return nil // not an on-chain event
	}

	var (
		contributorPool int64
		maintainerPool  int64
		decimals        int32
		minConf         int
	)
	if err := pool.QueryRow(ctx, `
SELECT p.contributor_pool::bigint, p.maintainer_pool::bigint, p.asset_decimals,
       COALESCE(c.min_confirmations, 1)
FROM hackathon_chain_pools p
LEFT JOIN chain_configs c ON c.chain_id = p.chain_id
WHERE p.hackathon_id = $1 AND p.chain_id = $2
`, hackathonID, chainID).Scan(&contributorPool, &maintainerPool, &decimals, &minConf); err != nil {
		return fmt.Errorf("hackathon.CheckDrawCommit: load chain pool: %w", err)
	}

	// The commit transaction recorded for this issue, if any.
	var commitTx string
	var state string
	err := pool.QueryRow(ctx, `
SELECT COALESCE(tx_hash, ''), state
FROM chain_commitments
WHERE hackathon_id = $1 AND chain_id = $2 AND kind = 'draw_commit' AND subject_ref = $3
`, hackathonID, chainID, issueRef(issueID)).Scan(&commitTx, &state)
	if err != nil {
		// No row at all is the clearest case of "no commit was made".
		return fmt.Errorf("%w: no draw-seed commit recorded for issue #%d on %s",
			ErrDrawCommitNotReady, issueNumber, chainID)
	}

	chainPool := chain.Pool{
		HackathonID:      hackathonID.String(),
		ChainID:          chainID,
		ContributorPool:  chain.NewAmount(contributorPool, decimals),
		MaintainerPool:   chain.NewAmount(maintainerPool, decimals),
		EscrowRef:        chain.EscrowRef{ChainID: chainID, HackathonID: hackathonID.String()},
		MinConfirmations: minConf,
	}

	if err := chain.GuardDrawCommitConfirmed(ctx, reg, chainPool, issueRef(issueID), commitTx); err != nil {
		return fmt.Errorf("%w: %v", ErrDrawCommitNotReady, err)
	}
	return nil
}

// issueRef is the subject a draw commitment is recorded under. One function,
// so the value written at commit time and the value checked at draw time
// cannot drift apart.
func issueRef(issueID uuid.UUID) string { return issueID.String() }

// ExtendWindowForCommit pushes an issue's application window out so a draw
// withheld for an unconfirmed commit is retried rather than lost.
//
// §5.3 says to extend the window and alert. Extending matters more than it
// sounds: leaving the window closed would strand the issue - it stays due
// forever, drawing nothing, and the contributors who applied never learn why.
func ExtendWindowForCommit(ctx context.Context, pool db.DBPool, issueID uuid.UUID) error {
	_, err := pool.Exec(ctx, `
UPDATE hackathon_issues
SET application_window_closes_at = GREATEST(
      COALESCE(application_window_closes_at, now()), now()
    ) + interval '1 hour',
    updated_at = now()
WHERE id = $1
`, issueID)
	if err != nil {
		return fmt.Errorf("hackathon.ExtendWindowForCommit: %w", err)
	}
	return nil
}
