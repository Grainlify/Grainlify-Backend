package handlers

import "errors"

// The fixed-rate points programme is frozen.
//
// It paid a known amount for an action that costs nothing and produces
// nothing: $1 per verified referral, $5 for following three social accounts,
// uncapped and unfunded. That is the exact thing the GrainHack design exists
// to prevent - a reward you can calculate in advance is a reward you can
// farm - and it needed no code, no merged PR and no skill to collect. The
// liability was also unbounded: at ~$6 per user, ten thousand users is
// $60,000 owed against no revenue.
//
// It is replaced by the Founding Contributor Pool: one fixed pool, shares
// earned overwhelmingly by merged pull requests, share value unknowable until
// settlement.
//
// **Freezing before building the replacement is deliberate.** Two reward
// systems running at once is the worst available state - balances accruing
// under rules that are being retired, against a liability nobody intends to
// honour at the old rate.
//
// Confirmed against production before freezing (2026-08-11): zero
// point_ledger rows, zero referrals, zero social-follow submissions, zero
// redemptions in any status, across 8 users. Nothing accrued and nothing was
// ever paid, so this freeze takes nothing away from anybody.
// A var rather than a const purely so tests can exercise both sides of it -
// the same seam pattern as timeNow elsewhere in this codebase. Nothing
// outside this package can reach it, and nothing inside it writes it except
// the test helper.
var pointsProgrammeFrozen = true

// ErrPointsProgrammeFrozen is returned by the accrual chokepoint. Callers
// distinguish it rather than string-matching.
var ErrPointsProgrammeFrozen = errors.New("the points programme is frozen and no longer grants points")

// guardPointsAccrual refuses to grant points while the programme is frozen.
//
// Applied at insertLedgerEntry - the single place any point_ledger row is
// written - rather than at each of the three grant sites, so a grant path
// added later is frozen by default instead of by remembering.
//
// Only grants are refused. Negative entries (a redemption spend, a
// rejected-redemption refund) stay permitted: refusing a refund would strand
// somebody's balance, which is the opposite of the intent. With no balances
// in existence this is theoretical today, and it is the correct shape anyway.
func guardPointsAccrual(amount int) error {
	if pointsProgrammeFrozen && amount > 0 {
		return ErrPointsProgrammeFrozen
	}
	return nil
}

// pointsRedemptionsFrozen reports whether new redemption requests are
// refused. Separate from the accrual guard because they are separate
// decisions: accrual stops because the rules changed, redemption stops
// because there is no rate left to redeem at.
func pointsRedemptionsFrozen() bool { return pointsProgrammeFrozen }

// pointsGrantAmount is what a completion is worth while the programme is
// frozen: nothing.
//
// The referral and social-follow completions themselves are still recorded.
// That is deliberate and not merely harmless - the Founding Contributor Pool
// pays shares for a referred person verifying, and for a referred person
// shipping a merged PR, so the referral graph and its completion timestamps
// are the source data those shares are computed from. Stopping the *payment*
// must not stop the *record*, or the replacement has nothing to read.
//
// Writing 0 into points_awarded rather than leaving the old constant is what
// keeps the two views of a balance agreeing: /referrals/me sums
// referrals.points_awarded directly, not the ledger, so a completed referral
// carrying a non-zero points_awarded with no matching ledger row would show
// somebody a balance they cannot spend and nobody owes them.
func pointsGrantAmount(configured int) int {
	if pointsProgrammeFrozen {
		return 0
	}
	return configured
}
