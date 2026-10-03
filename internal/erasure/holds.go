package erasure

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// MoneyInFlight lists the reasons money may still be on its way to this
// person, in words the settings screen shows as-is. Empty means none.
//
// Erasure deletes payout addresses and the account the person would sign in
// with to collect a claim. Doing that while a payout is still owed would turn
// "delete my data" into "forfeit my money", which nobody asked for. So the
// erasure waits ("held") and re-checks on every pass, and proceeds by itself
// once each of these has cleared - or once it has waited MaxHold, keeping the
// records of whatever is still in flight.
//
// Each check errs towards holding. A hold costs a delay the person can see
// and ask about; an erasure that strands a payout cannot be undone.
func MoneyInFlight(ctx context.Context, pool Pool, userID uuid.UUID) ([]string, error) {
	checks := []struct {
		reason string
		query  string
	}{
		{
			// The points programme is frozen and none were ever made, but a
			// pending one would still be money owed.
			"a points redemption request that has not been decided",
			`SELECT EXISTS (SELECT 1 FROM redemptions WHERE user_id = $1 AND status = 'pending')`,
		},
		{
			"a payout amount held back from an earlier settlement",
			`SELECT EXISTS (SELECT 1 FROM settlement_holds WHERE user_id = $1 AND released_at IS NULL)`,
		},
		{
			"a payout that has been computed but not yet released",
			`SELECT EXISTS (
			   SELECT 1 FROM settlement_lines l JOIN settlements s ON s.id = l.settlement_id
			   WHERE l.user_id = $1 AND l.amount_minor > 0 AND s.released_at IS NULL)`,
		},
		{
			"a payout transfer that has not been confirmed",
			`SELECT EXISTS (SELECT 1 FROM keeperhub_payout_legs
			   WHERE user_id = $1 AND status IN ('pending', 'dispatched', 'unknown'))`,
		},
		{
			// claim_leaves has no user_id by design; the address history is the
			// join, as /me/claims does it. A leaf is treated as collected only
			// when our sponsored-claim path recorded it - a claim made directly
			// on-chain is invisible here, so it holds, and the person can ask.
			"a published payout claim that has not been collected",
			`SELECT EXISTS (
			   SELECT 1
			   FROM claim_leaves l
			   JOIN payout_event_roots r ON r.settlement_id = l.settlement_id
			   JOIN contributor_addresses ca
			     ON lower(ca.address) = lower(l.claim_address) AND ca.chain_id = r.chain_id
			   WHERE ca.user_id = $1
			     AND r.published_tx IS NOT NULL
			     AND NOT EXISTS (
			       SELECT 1 FROM sponsored_claims sc
			       WHERE sc.user_id = $1 AND sc.leaf_hash = l.leaf_hash
			         AND sc.outcome IN ('submitted', 'refused_already_claimed')))`,
		},
		{
			// Work in progress can still earn a payout, and the assignment is
			// how it would be matched to the person.
			"a GrainHack issue you are still assigned to",
			`SELECT EXISTS (SELECT 1 FROM hackathon_assignments
			   WHERE user_id = $1 AND status IN ('active', 'pr_submitted'))`,
		},
	}

	var reasons []string
	for _, c := range checks {
		var held bool
		if err := pool.QueryRow(ctx, c.query, userID).Scan(&held); err != nil {
			return nil, fmt.Errorf("erasure: hold check %q: %w", c.reason, err)
		}
		if held {
			reasons = append(reasons, c.reason)
		}
	}
	return reasons, nil
}

// holdMessage is the sentence stored in hold_reason, shown as-is on the
// settings screen. until is when the hold ends whatever happens.
func holdMessage(reasons []string, until time.Time) string {
	return "Money may still be on its way to you (" + strings.Join(reasons, "; ") + "). " +
		"We will erase your account as soon as it has been paid, and on " + readableDate(until) +
		" at the latest: if it has not been paid by then, we erase your account anyway and keep the record of " +
		"the payment, so it can still be paid or resolved through support. If you would rather give it up now, contact support."
}

// readableDate is how dates are written in what the person reads.
func readableDate(t time.Time) string { return t.UTC().Format("2 January 2006") }
