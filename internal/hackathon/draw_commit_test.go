package hackathon

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/chain"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// dcFxOnChainIssue sets up a live hackathon on one chain with a published
// issue whose window has closed and one applicant waiting - i.e. an issue
// runDueDraws will pick up on its next tick.
func dcFxOnChainIssue(t *testing.T, pool db.DBPool, chainID string) (hackathonID, issueID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	ciFxChainPool(t, pool, hackathonID, chainID)

	issueID = fxPublishedIssue(t, pool, hackathonID, projectID, 8100, "standard")
	if _, err := pool.Exec(ctx, `
UPDATE hackathon_issues
SET chain_id = $2,
    application_window_opens_at  = now() - interval '2 hours',
    application_window_closes_at = now() - interval '1 minute'
WHERE id = $1`, issueID, chainID); err != nil {
		t.Fatalf("close window: %v", err)
	}

	user := fxUser(t, pool)
	if _, err := pool.Exec(ctx, `
INSERT INTO hackathon_issue_applications (hackathon_id, hackathon_issue_id, user_id, github_login, status)
VALUES ($1, $2, $3, 'applicant', 'applied')`, hackathonID, issueID, user); err != nil {
		t.Fatalf("seed applicant: %v", err)
	}
	return hackathonID, issueID
}

func dcCountDraws(t *testing.T, pool db.DBPool, issueID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*)::int FROM hackathon_draws WHERE hackathon_issue_id = $1 AND NOT is_simulation`,
		issueID).Scan(&n); err != nil {
		t.Fatalf("count draws: %v", err)
	}
	return n
}

// A due draw whose seed commit has not confirmed must not run - driven
// through runDueDraws, the real scheduling path, rather than by calling the
// guard directly.
//
// Calling the guard would prove only that the guard works. The failure this
// is protecting against is the guard existing and never being reached, which
// is the same class as a config key nothing reads: it looks like protection
// and is not.
func TestRunDueDraws_WithholdsTheDrawUntilTheCommitConfirms(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	pool := d.Pool

	adapter := chain.NewMockAdapter("soroban", 6)
	reg := chain.NewRegistry()
	reg.Register(adapter)

	hackathonID, issueID := dcFxOnChainIssue(t, pool, "soroban")
	runner := NewAssignmentRunner(pool, nil, nil, nil).WithChains(reg)

	// --- No commit recorded at all -----------------------------------------
	if err := runner.runDueDraws(ctx); err != nil {
		t.Fatalf("runDueDraws: %v", err)
	}
	if n := dcCountDraws(t, pool, issueID); n != 0 {
		t.Fatalf("a draw ran with no seed commit recorded (%d draws); it would be unverifiable and indistinguishable from an honest one", n)
	}

	// The window was extended rather than left closed, so the issue is
	// retried instead of stranded.
	var closesAt time.Time
	if err := pool.QueryRow(ctx,
		`SELECT application_window_closes_at FROM hackathon_issues WHERE id = $1`, issueID).Scan(&closesAt); err != nil {
		t.Fatalf("read window: %v", err)
	}
	if !closesAt.After(time.Now()) {
		t.Errorf("window closes at %v, which is not in the future - the issue would stay due forever, drawing nothing", closesAt)
	}

	// --- Commit submitted but unconfirmed ----------------------------------
	var commitHash [32]byte
	tx, err := adapter.BuildCommitDrawSeed(ctx, chain.EscrowRef{ChainID: "soroban", HackathonID: hackathonID.String()},
		issueID.String(), commitHash)
	if err != nil {
		t.Fatalf("build commit: %v", err)
	}
	txHash := string(tx.Payload)
	if _, err := pool.Exec(ctx, `
INSERT INTO chain_commitments (hackathon_id, chain_id, kind, subject_ref, value, tx_hash, submitted_at, state)
VALUES ($1, 'soroban', 'draw_commit', $2, $3, $4, now(), 'submitted')`,
		hackathonID, issueID.String(), commitHash[:], txHash); err != nil {
		t.Fatalf("record commit: %v", err)
	}
	// Re-open the window so the issue is due again.
	if _, err := pool.Exec(ctx,
		`UPDATE hackathon_issues SET application_window_closes_at = now() - interval '1 minute' WHERE id = $1`, issueID); err != nil {
		t.Fatalf("reclose window: %v", err)
	}

	if err := runner.runDueDraws(ctx); err != nil {
		t.Fatalf("runDueDraws: %v", err)
	}
	if n := dcCountDraws(t, pool, issueID); n != 0 {
		t.Fatalf("a draw ran on an unconfirmed commit (%d draws)", n)
	}

	// --- Commit confirmed: the draw may now run ----------------------------
	adapter.Confirm(txHash, 5)
	if _, err := pool.Exec(ctx,
		`UPDATE hackathon_issues SET application_window_closes_at = now() - interval '1 minute' WHERE id = $1`, issueID); err != nil {
		t.Fatalf("reclose window: %v", err)
	}
	if err := runner.runDueDraws(ctx); err != nil {
		t.Fatalf("runDueDraws: %v", err)
	}
	if n := dcCountDraws(t, pool, issueID); n == 0 {
		t.Error("the draw did not run even with a confirmed commit; the guard is refusing valid draws")
	}
}

// An event with no chain pools must draw exactly as before. The on-chain
// guarantee is adopted per event, so making it mandatory would break every
// existing one.
func TestRunDueDraws_UnchainedEventsDrawAsBefore(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	pool := d.Pool

	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	_ = hackathonID
	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 8200, "standard")
	if _, err := pool.Exec(ctx, `
UPDATE hackathon_issues
SET application_window_opens_at  = now() - interval '2 hours',
    application_window_closes_at = now() - interval '1 minute'
WHERE id = $1`, issueID); err != nil {
		t.Fatalf("close window: %v", err)
	}
	user := fxUser(t, pool)
	if _, err := pool.Exec(ctx, `
INSERT INTO hackathon_issue_applications (hackathon_id, hackathon_issue_id, user_id, github_login, status)
VALUES ($1, $2, $3, 'applicant', 'applied')`, hackathonID, issueID, user); err != nil {
		t.Fatalf("seed applicant: %v", err)
	}

	// A registry is attached, but this event runs no chains.
	reg := chain.NewRegistry()
	reg.Register(chain.NewMockAdapter("soroban", 6))
	runner := NewAssignmentRunner(pool, nil, nil, nil).WithChains(reg)

	if err := runner.runDueDraws(ctx); err != nil {
		t.Fatalf("runDueDraws: %v", err)
	}
	if n := dcCountDraws(t, pool, issueID); n == 0 {
		t.Error("an unchained event's draw was withheld; the on-chain guard must not apply to events with no chain pools")
	}
}
