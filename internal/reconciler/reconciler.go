// Package reconciler makes the database agree with the chain, in that direction
// only.
//
// # What it may and may not do
//
// It writes StatePaid, and it is the only thing that does. It writes it only
// after reading chain state, never after submitting something and assuming.
//
// When the chain and the database disagree in a way it cannot resolve, it
// **alerts and stops**. It does not correct, retry, or re-pay. There is no human
// present at 3am to consult, so the machine must not pick a direction - see
// trap 10 in docs/VERIFICATION-TRAPS.md. The failure mode of guessing here is
// paying somebody twice.
//
// # Polling is the system; triggers are an optimisation
//
// Everything here must be correct with polling alone. A trigger may schedule a
// pass the poller would have made anyway; it may never be the code path that
// performs a transition. That is what makes a missed trigger a delay rather than
// a lost payment, and it is asserted by running the whole suite with triggers
// disabled and expecting identical end state.
//
// Claims are polling-only by nature: the contributor submits them, so there is
// nothing for us to be triggered by.
package reconciler

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/chainops"
	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// ClaimReader answers whether one leaf has been claimed on chain.
//
// Per-leaf and stateless, deliberately. An event stream would be cheaper at
// scale and introduces a cursor - state that can be silently wrong, where wrong
// means an unnoticed claim. At the sizes this system will see for a long while
// the cost difference is nothing and the failure mode is the whole cost.
//
// # Replacing this at scale
//
// Write an EventClaimReader that ingests Claimed events into a local table and
// answers from it. Nothing in this package changes: same interface, same call
// site, same tests.
//
// **Add a reader; do not replace this one.** On any cursor gap or restart the
// event reader should fall back to a per-leaf read for the affected leaves,
// which keeps "cannot miss a claim" a property of the system rather than of the
// fast path. That is only possible while this implementation still exists.
type ClaimReader interface {
	IsClaimed(ctx context.Context, escrow string, leaf []byte) (bool, error)
}

// RootReader answers what root an escrow actually published.
//
// Separate from ClaimReader because it is read once per event rather than once
// per leaf, and because a disagreement here stops reconciliation for the whole
// event rather than one row.
type RootReader interface {
	PublishedRoot(ctx context.Context, escrow string) ([]byte, bool, error)
}

// Alerter delivers a disagreement to a human. Implemented by the support sink.
type Alerter interface {
	AlertPayout(ctx context.Context, subject, message string) error
}

// Outstanding is a leaf we believe is owed and not yet settled.
type Outstanding struct {
	OpID     uuid.UUID
	EventRef uuid.UUID
	Escrow   string
	Leaf     []byte
	State    chainops.State
}

var (
	// ErrRootMismatch stops an event. Every proof being served for it may be
	// invalid, so this is not a per-leaf problem.
	ErrRootMismatch = errors.New("the published root is not the root we recorded")
)

// Reconciler holds no chain knowledge of its own.
type Reconciler struct {
	pool   db.DBPool
	claims ClaimReader
	roots  RootReader
	alert  Alerter
}

func New(pool db.DBPool, claims ClaimReader, roots RootReader, alert Alerter) *Reconciler {
	return &Reconciler{pool: pool, claims: claims, roots: roots, alert: alert}
}

// ReconcileClaims settles every outstanding leaf it can, and alerts on the ones
// it cannot.
//
// Returns the number promoted to paid. An error means the pass could not
// complete, not that a disagreement was found - a disagreement is alerted and
// skipped, so one bad leaf never stops the others being settled.
func (r *Reconciler) ReconcileClaims(ctx context.Context, event uuid.UUID) (int, error) {
	if err := r.checkRoot(ctx, event); err != nil {
		return 0, err
	}

	rows, err := r.outstanding(ctx, event)
	if err != nil {
		return 0, err
	}

	promoted := 0
	for _, o := range rows {
		claimed, err := r.claims.IsClaimed(ctx, o.Escrow, o.Leaf)
		if err != nil {
			// A read failure is not a disagreement. Leave the row alone and try
			// again next pass; alerting here would fire on every network blip.
			slog.Warn("reconciler: could not read claim state",
				"event", event, "leaf", hex.EncodeToString(o.Leaf), "error", err)
			continue
		}

		switch {
		case claimed && o.State != chainops.StatePaid:
			if err := r.markPaid(ctx, o); err != nil {
				return promoted, err
			}
			promoted++

		case !claimed && o.State == chainops.StatePaid:
			// We recorded a payment the chain does not have. This is the one
			// that must never be auto-corrected: reverting the row is a guess
			// about which side is wrong, and re-paying is how a guess becomes a
			// double payment.
			r.raise(ctx, "paid_but_unclaimed", hex.EncodeToString(o.Leaf), event,
				fmt.Sprintf("Row says paid, chain says unclaimed.\nEscrow %s\nLeaf %s\n\n"+
					"Nobody has been paid twice - the risk is that somebody has not been paid "+
					"at all. Their leaf is still claimable and their claim still works.\n\n"+
					"Check the recorded transaction on the explorer before acting, and do not "+
					"re-run anything until you know which case it is.",
					o.Escrow, hex.EncodeToString(o.Leaf)))
		}
	}
	return promoted, nil
}

// checkRoot refuses to reconcile an event whose on-chain root is not the one we
// recorded.
//
// If those differ, the tree we are serving proofs from is not the tree that was
// published, so every proof is invalid. That is an event-wide problem and the
// pass stops rather than settling leaves against a root we do not understand.
func (r *Reconciler) checkRoot(ctx context.Context, event uuid.UUID) error {
	var escrow string
	var recorded []byte
	err := r.pool.QueryRow(ctx, `
SELECT escrow_address, root
FROM payout_event_roots
WHERE settlement_id = $1
`, event).Scan(&escrow, &recorded)
	if err != nil {
		// No recorded root yet is not a disagreement - nothing has been
		// published, so there is nothing to reconcile against.
		return nil
	}

	onChain, published, err := r.roots.PublishedRoot(ctx, escrow)
	if err != nil {
		return fmt.Errorf("reconciler: read published root: %w", err)
	}
	if !published {
		return nil
	}
	if hex.EncodeToString(onChain) == hex.EncodeToString(recorded) {
		return nil
	}

	r.raise(ctx, "root_mismatch", event.String(), event,
		fmt.Sprintf("Published root does not match the root we recorded.\n"+
			"Escrow   %s\nOn chain %s\nRecorded %s\n\n"+
			"Recompute escrow_address(admin, event_id) first - a wrong escrow address is "+
			"far more likely than a changed root, because a root is write-once.\n\n"+
			"If the escrow is right and the roots genuinely differ, the tree we are serving "+
			"proofs from is not the tree that was published. Stop serving proofs for this "+
			"event immediately.",
			escrow, hex.EncodeToString(onChain), hex.EncodeToString(recorded)))

	return fmt.Errorf("%w: event %s", ErrRootMismatch, event)
}

func (r *Reconciler) outstanding(ctx context.Context, event uuid.UUID) ([]Outstanding, error) {
	rows, err := r.pool.Query(ctx, `
SELECT o.id, o.event_ref, COALESCE(e.escrow_address, ''), o.leaf_hash, o.state
FROM chain_operations o
LEFT JOIN payout_event_roots e ON e.settlement_id = o.event_ref
WHERE o.event_ref = $1 AND o.kind = 'claim'
ORDER BY o.created_at
`, event)
	if err != nil {
		return nil, fmt.Errorf("reconciler: load outstanding leaves: %w", err)
	}
	defer rows.Close()

	var out []Outstanding
	for rows.Next() {
		var o Outstanding
		var state string
		if err := rows.Scan(&o.OpID, &o.EventRef, &o.Escrow, &o.Leaf, &state); err != nil {
			return nil, fmt.Errorf("reconciler: scan: %w", err)
		}
		o.State = chainops.State(state)
		out = append(out, o)
	}
	return out, rows.Err()
}

// markPaid is the only write of StatePaid in this codebase.
func (r *Reconciler) markPaid(ctx context.Context, o Outstanding) error {
	_, err := r.pool.Exec(ctx, `
UPDATE chain_operations SET state = $1 WHERE id = $2
`, string(chainops.StatePaid), o.OpID)
	if err != nil {
		return fmt.Errorf("reconciler: mark paid: %w", err)
	}
	return nil
}

// raise records a disagreement once and tells somebody.
//
// The claim row is written first and the alert only sent if this pass is the one
// that claimed it, so a reconciler running every thirty seconds does not send the
// same alert every thirty seconds. An alert that repeats gets muted, and a muted
// alert is worse than none because it still looks like coverage.
//
// Delivery failure leaves the row claimed, deliberately - the same choice the
// KYC path makes. Re-sending on the next pass would turn a transient sink outage
// into exactly the repetition this exists to prevent.
func (r *Reconciler) raise(ctx context.Context, kind, subject string, event uuid.UUID, detail string) {
	tag, err := r.pool.Exec(ctx, `
INSERT INTO chain_reconcile_alerts (kind, subject, event_ref, detail)
VALUES ($1, $2, $3, $4)
ON CONFLICT (kind, subject) DO NOTHING
`, kind, subject, event, detail)
	if err != nil {
		slog.Error("reconciler: could not record disagreement",
			"kind", kind, "subject", subject, "error", err)
		return
	}
	if tag.RowsAffected() == 0 {
		return // Already alerted for this disagreement.
	}

	if r.alert == nil {
		slog.Error("reconciler: disagreement found and NOBODY WAS TOLD - no alerter configured",
			"kind", kind, "subject", subject, "detail", detail)
		return
	}
	if err := r.alert.AlertPayout(ctx, kind, detail); err != nil {
		slog.Error("reconciler: disagreement recorded but delivery failed",
			"kind", kind, "subject", subject, "error", err)
	}
}
