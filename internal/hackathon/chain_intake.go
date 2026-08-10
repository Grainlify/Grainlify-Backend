package hackathon

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// Errors callers distinguish rather than string-match.
var (
	ErrChainRequired    = errors.New("this event runs on chains, so an issue must be tagged with one")
	ErrChainNotInEvent  = errors.New("that chain is not one this event runs on")
	ErrChainRetagLocked = errors.New("the issue cannot be re-tagged: applications are already bound to its current chain")
)

// EventChains returns the chain ids an event runs on, from
// hackathon_chain_pools - the source of truth (§6).
//
// An empty result means the event is not on-chain at all, which is the
// majority case and must keep working exactly as before.
func EventChains(ctx context.Context, pool db.DBPool, hackathonID uuid.UUID) ([]string, error) {
	rows, err := pool.Query(ctx, `
SELECT chain_id FROM hackathon_chain_pools WHERE hackathon_id = $1 ORDER BY chain_id
`, hackathonID)
	if err != nil {
		return nil, fmt.Errorf("hackathon.EventChains: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ValidateIssueChain checks an issue's chain tag against the event.
//
// Two rules from §6:
//
//   - An event that runs chains requires every issue to carry one. An issue
//     with no chain has no pool to be paid from, so accepting it would take
//     someone's work with nothing behind it.
//   - The chain must be one the event actually runs. A typo naming a chain
//     with no escrow is the same failure wearing a different hat.
//
// An event with no chain pools accepts an empty chain and rejects a non-empty
// one, so a stray tag cannot imply a pool that was never funded.
func ValidateIssueChain(ctx context.Context, pool db.DBPool, hackathonID uuid.UUID, chainID string) error {
	chains, err := EventChains(ctx, pool, hackathonID)
	if err != nil {
		return err
	}
	chainID = strings.TrimSpace(chainID)

	if len(chains) == 0 {
		if chainID != "" {
			return fmt.Errorf("%w: this event runs no chains, but the issue is tagged %q", ErrChainNotInEvent, chainID)
		}
		return nil
	}
	if chainID == "" {
		return fmt.Errorf("%w: tag it with one of %s", ErrChainRequired, strings.Join(chains, ", "))
	}
	for _, c := range chains {
		if strings.EqualFold(c, chainID) {
			return nil
		}
	}
	return fmt.Errorf("%w: %q is not among %s", ErrChainNotInEvent, chainID, strings.Join(chains, ", "))
}

// RetagIssueChain changes an issue's chain, refusing once any application
// exists.
//
// §11-#2: only before any application exists. Existing applications carry a
// copy of the chain they were made under and are already bound to that pool -
// re-tagging afterwards would leave applications pointing at one pool and the
// issue at another, and no correct answer for which one pays.
func RetagIssueChain(ctx context.Context, pool db.DBPool, issueID uuid.UUID, chainID string) error {
	var hackathonID uuid.UUID
	var issueNumber int
	var projectID uuid.UUID
	if err := pool.QueryRow(ctx, `
SELECT hackathon_id, project_id, issue_number FROM hackathon_issues WHERE id = $1
`, issueID).Scan(&hackathonID, &projectID, &issueNumber); err != nil {
		return fmt.Errorf("hackathon.RetagIssueChain: load issue: %w", err)
	}

	if err := ValidateIssueChain(ctx, pool, hackathonID, chainID); err != nil {
		return err
	}

	var applications int
	if err := pool.QueryRow(ctx, `
SELECT count(*)::int FROM hackathon_issue_applications WHERE hackathon_issue_id = $1
`, issueID).Scan(&applications); err != nil {
		return fmt.Errorf("hackathon.RetagIssueChain: count applications: %w", err)
	}
	if applications > 0 {
		return fmt.Errorf("%w: %d application(s) exist for issue #%d", ErrChainRetagLocked, applications, issueNumber)
	}

	if _, err := pool.Exec(ctx, `
UPDATE hackathon_issues SET chain_id = NULLIF($2, ''), updated_at = now() WHERE id = $1
`, issueID, strings.TrimSpace(chainID)); err != nil {
		return fmt.Errorf("hackathon.RetagIssueChain: %w", err)
	}
	return nil
}

// UntaggedIssuesInChainEvents returns issues that belong to an event running
// chain pools but carry no chain.
//
// chain_id is nullable because legacy rows predate chains and there is no
// correct value to invent for them. This is the check that covers the actual
// hazard the missing constraint leaves open - a *new* issue slipping through
// null - and it is independent of whether legacy cleanup has happened.
func UntaggedIssuesInChainEvents(ctx context.Context, pool db.DBPool) ([]string, error) {
	rows, err := pool.Query(ctx, `
SELECT hi.id::text
FROM hackathon_issues hi
WHERE hi.chain_id IS NULL
  AND EXISTS (SELECT 1 FROM hackathon_chain_pools p WHERE p.hackathon_id = hi.hackathon_id)
ORDER BY hi.id
`)
	if err != nil {
		return nil, fmt.Errorf("hackathon.UntaggedIssuesInChainEvents: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
