package payout

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/payoutaddr"
)

// Resolve joins each entitlement to its GitHub login and live payout address and
// decides its outcome. It writes nothing.
//
// Both joins are LEFT joins on purpose: a missing login and a missing address are
// outcomes to record, not rows to lose. The whole point of the classification is
// that somebody who earned an amount they cannot receive stays visible.
//
// github_accounts.access_token is deliberately not selected. It lives in that
// table and has no business in a settlement query.
func Resolve(ctx context.Context, pool db.DBPool, s Settlement) ([]Member, error) {
	if s.ChainID == "" {
		return nil, fmt.Errorf("payout.Resolve: chain id is empty")
	}
	// Refused here, so an unreconcilable settlement cannot even produce a report
	// somebody might read and approve.
	if err := validate(s); err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(s.Entitlements))
	for _, e := range s.Entitlements {
		ids = append(ids, e.UserID)
	}

	type joined struct {
		login string
		addr  string
	}
	found := make(map[uuid.UUID]joined, len(ids))

	rows, err := pool.Query(ctx, `
		SELECT u.id,
		       COALESCE(g.login, ''),
		       COALESCE(a.address, '')
		FROM unnest($1::uuid[]) AS u(id)
		LEFT JOIN github_accounts g ON g.user_id = u.id
		LEFT JOIN contributor_addresses a
		       ON a.user_id = u.id AND a.chain_id = $2 AND a.superseded_at IS NULL`,
		ids, s.ChainID)
	if err != nil {
		return nil, fmt.Errorf("payout.Resolve: join identities and addresses: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var j joined
		if err := rows.Scan(&id, &j.login, &j.addr); err != nil {
			return nil, fmt.Errorf("payout.Resolve: scan: %w", err)
		}
		found[id] = j
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]Member, 0, len(s.Entitlements))
	for _, e := range s.Entitlements {
		amt := e.AmountMinor
		if amt == nil {
			amt = new(big.Int)
		}
		j := found[e.UserID]

		m := Member{
			UserID:      e.UserID,
			GitHubLogin: j.login,
			AmountMinor: new(big.Int).Set(amt),
			Reason:      e.IneligibleReason,
		}

		// Canonicalise here rather than trusting what is stored. The column has
		// a format check, but a value read back is still a value crossing a
		// boundary, and this is the last point before it is committed to a leaf.
		if j.addr != "" {
			c, err := payoutaddr.Validate(j.addr)
			if err != nil {
				// A stored address that no longer validates is not a reason to
				// pay somebody at it. Treat it as absent and say so.
				m.ClaimAddress = ""
			} else {
				m.ClaimAddress = c
			}
		}

		switch {
		case amt.Sign() <= 0 && e.IneligibleReason != "":
			m.Outcome = OutcomeIneligible
		case amt.Sign() <= 0:
			m.Outcome = OutcomeNoShares
		case m.GitHubLogin == "":
			m.Outcome = OutcomeNoGitHubAccount
		case m.ClaimAddress == "":
			m.Outcome = OutcomeNoAddress
		default:
			m.Outcome = OutcomePayable
		}
		out = append(out, m)
	}

	// Deterministic order, so the digest below is stable and two runs of the
	// report are diffable.
	sort.Slice(out, func(i, j int) bool { return out[i].UserID.String() < out[j].UserID.String() })
	return out, nil
}

// ErrDoesNotReconcile is returned when the entitlements cannot be paid from the
// pool they claim to come from.
var ErrDoesNotReconcile = errors.New("settlement does not reconcile against its pool")

// validate refuses a settlement whose arithmetic cannot be true before any of it
// reaches a tree.
//
// The dangerous direction is allocation EXCEEDING the pool. The escrow is funded
// with the leaf total and `publish_root` asserts total == funded_total, so an
// over-allocated settlement does not fail at publication - it fails by asking us
// to fund more than the event holds, and whoever is funding finds out by looking
// at their balance.
//
// This is also the check that catches a second rounding. Amounts must come from
// effective_units through apportion, which is the only place a fraction becomes
// an integer; feeding an already-rounded figure back through it produces totals
// that are plausible and wrong. Wrong by a few minor units per person is exactly
// what a sum against the pool detects and what reading the numbers does not.
func validate(s Settlement) error {
	if s.PoolMinor == nil {
		return fmt.Errorf("%w: pool is not set", ErrDoesNotReconcile)
	}
	if s.PoolMinor.Sign() < 0 {
		return fmt.Errorf("%w: pool is negative (%s)", ErrDoesNotReconcile, s.PoolMinor)
	}
	total := new(big.Int)
	for _, e := range s.Entitlements {
		if e.AmountMinor == nil {
			continue
		}
		if e.AmountMinor.Sign() < 0 {
			return fmt.Errorf("%w: %s is allocated a negative amount (%s)",
				ErrDoesNotReconcile, e.UserID, e.AmountMinor)
		}
		total.Add(total, e.AmountMinor)
	}
	if total.Cmp(s.PoolMinor) > 0 {
		over := new(big.Int).Sub(total, s.PoolMinor)
		return fmt.Errorf("%w: entitlements total %s minor units against a pool of %s, "+
			"over by %s. Amounts must come from effective_units through apportion, which is "+
			"the only place a fraction becomes an integer - an already-rounded figure fed back "+
			"through it produces exactly this.",
			ErrDoesNotReconcile, total, s.PoolMinor, over)
	}
	return nil
}
