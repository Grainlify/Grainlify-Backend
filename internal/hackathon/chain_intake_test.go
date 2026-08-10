package hackathon

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

func ciFxChainPool(t *testing.T, pool db.DBPool, hackathonID uuid.UUID, chainID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
INSERT INTO chain_configs (chain_id, enabled, asset, min_confirmations)
VALUES ($1, true, '{"symbol":"USDC","decimals":6}'::jsonb, 1)
ON CONFLICT (chain_id) DO NOTHING`, chainID); err != nil {
		t.Fatalf("chain_configs: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO hackathon_chain_pools (hackathon_id, chain_id, contributor_pool, maintainer_pool, asset_decimals)
VALUES ($1, $2, 30000000000, 5000000000, 6)
ON CONFLICT (hackathon_id, chain_id) DO NOTHING`, hackathonID, chainID); err != nil {
		t.Fatalf("hackathon_chain_pools: %v", err)
	}
}

// The hazard the nullable column leaves open: a new issue entering a
// chain-running event without a chain. It has no pool to be paid from, so
// accepting it takes someone's work with nothing behind it.
//
// This covers that directly and does not depend on legacy rows having been
// cleaned up, which is why it can exist now rather than waiting for the
// NOT NULL constraint.
func TestChainTagging_NoIssueInAChainEventMayLackAChain(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	pool := d.Pool
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	ciFxChainPool(t, pool, hackathonID, "soroban")

	// A correctly tagged issue is fine.
	tagged := fxPublishedIssue(t, pool, hackathonID, projectID, 7001, "standard")
	if _, err := pool.Exec(ctx, `UPDATE hackathon_issues SET chain_id = 'soroban' WHERE id = $1`, tagged); err != nil {
		t.Fatalf("tag issue: %v", err)
	}

	untagged, err := UntaggedIssuesInChainEvents(ctx, pool)
	if err != nil {
		t.Fatalf("UntaggedIssuesInChainEvents: %v", err)
	}
	for _, id := range untagged {
		if id == tagged.String() {
			t.Fatal("a correctly tagged issue was reported as untagged")
		}
	}

	// An untagged issue in the same event must be reported.
	slipped := fxPublishedIssue(t, pool, hackathonID, projectID, 7002, "standard")
	untagged, err = UntaggedIssuesInChainEvents(ctx, pool)
	if err != nil {
		t.Fatalf("UntaggedIssuesInChainEvents: %v", err)
	}
	var found bool
	for _, id := range untagged {
		if id == slipped.String() {
			found = true
		}
	}
	if !found {
		t.Error("an issue with no chain in a chain-running event was not reported; it has no pool to be paid from")
	}
}

func TestValidateIssueChain_RequiresAKnownChainOnlyWhenTheEventRunsThem(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	pool := d.Pool
	hackathonID, _, _ := fxLiveHackathon(t, pool)

	// No chain pools: an empty tag is correct, and a stray tag is not - it
	// would imply a pool that was never funded.
	if err := ValidateIssueChain(ctx, pool, hackathonID, ""); err != nil {
		t.Errorf("an untagged issue in a non-chain event should be fine: %v", err)
	}
	if err := ValidateIssueChain(ctx, pool, hackathonID, "soroban"); !errors.Is(err, ErrChainNotInEvent) {
		t.Errorf("a chain tag on an event with no pools: err = %v, want ErrChainNotInEvent", err)
	}

	ciFxChainPool(t, pool, hackathonID, "soroban")

	if err := ValidateIssueChain(ctx, pool, hackathonID, ""); !errors.Is(err, ErrChainRequired) {
		t.Errorf("untagged issue in a chain event: err = %v, want ErrChainRequired", err)
	}
	if err := ValidateIssueChain(ctx, pool, hackathonID, "flare"); !errors.Is(err, ErrChainNotInEvent) {
		t.Errorf("a chain the event does not run: err = %v, want ErrChainNotInEvent", err)
	}
	if err := ValidateIssueChain(ctx, pool, hackathonID, "SOROBAN"); err != nil {
		t.Errorf("chain matching should be case-insensitive: %v", err)
	}
}

// §11-#2: re-tagging is allowed only before any application exists.
func TestRetagIssueChain_BlockedOnceApplicationsExist(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	pool := d.Pool
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)
	ciFxChainPool(t, pool, hackathonID, "soroban")
	ciFxChainPool(t, pool, hackathonID, "flare")
	_ = projectID

	issueID := fxPublishedIssue(t, pool, hackathonID, projectID, 7100, "standard")
	if err := RetagIssueChain(ctx, pool, issueID, "soroban"); err != nil {
		t.Fatalf("tagging before any application should be allowed: %v", err)
	}
	if err := RetagIssueChain(ctx, pool, issueID, "flare"); err != nil {
		t.Fatalf("re-tagging before any application should be allowed: %v", err)
	}

	// An application binds the issue to its pool.
	user := fxUser(t, pool)
	if _, err := pool.Exec(ctx, `
INSERT INTO hackathon_issue_applications (hackathon_id, hackathon_issue_id, user_id, github_login, status)
VALUES ($1, $2, $3, 'applicant', 'applied')`, hackathonID, issueID, user); err != nil {
		t.Fatalf("seed application: %v", err)
	}

	err := RetagIssueChain(ctx, pool, issueID, "soroban")
	if !errors.Is(err, ErrChainRetagLocked) {
		t.Fatalf("err = %v, want ErrChainRetagLocked - an existing application is already bound to the old pool", err)
	}

	// And the tag did not move.
	var chainID string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(chain_id,'') FROM hackathon_issues WHERE id = $1`, issueID).Scan(&chainID); err != nil {
		t.Fatalf("read chain: %v", err)
	}
	if chainID != "flare" {
		t.Errorf("chain_id = %q after a blocked re-tag, want it unchanged at flare", chainID)
	}
}
