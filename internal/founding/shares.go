package founding

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// Share-earning reasons. These are the CHECK constraint's values; keeping
// them as constants means a typo is a compile error rather than a runtime
// constraint violation on a path that only fires in production.
const (
	ReasonVerifiedAccount  = "verified_account"
	ReasonMergedPR         = "merged_pr"
	ReasonReferralVerified = "referral_verified"
	ReasonReferralMergedPR = "referral_merged_pr"
)

// ErrReferralCapReached means this grant would exceed the lifetime ceiling on
// shares from referrals that only verified.
//
// Not an error the caller needs to handle loudly - it is the cap working. It
// is distinguishable so a caller can log it as "expected" rather than as a
// failure.
var ErrReferralCapReached = errors.New("verify-only referral share cap reached")

// Grant records one share-earning event.
//
// Idempotent on (user, reason, source) by unique index: a retried webhook or a
// re-run backfill cannot double somebody's shares, and a duplicate is
// silently a no-op rather than an error, because a retry is not a failure.
//
// Returns the shares actually granted, which may be less than asked for when
// the referral cap partially absorbs it, or zero when it is already full.
func Grant(
	ctx context.Context,
	pool db.DBPool,
	userID uuid.UUID,
	shares float64,
	reason string,
	sourceRef *uuid.UUID,
	cfg map[string]string,
) (float64, error) {
	if shares <= 0 {
		return 0, nil
	}
	if reason == ReasonReferralVerified {
		return grantCapped(ctx, pool, userID, shares, reason, sourceRef, cfg)
	}
	return grantUncapped(ctx, pool, userID, shares, reason, sourceRef)
}

func grantUncapped(
	ctx context.Context,
	pool db.DBPool,
	userID uuid.UUID,
	shares float64,
	reason string,
	sourceRef *uuid.UUID,
) (float64, error) {
	tag, err := pool.Exec(ctx, `
INSERT INTO founding_shares (user_id, shares, reason, source_ref)
VALUES ($1, $2, $3, $4)
ON CONFLICT DO NOTHING
`, userID, shares, reason, sourceRef)
	if err != nil {
		return 0, fmt.Errorf("founding.Grant(%s): %w", reason, err)
	}
	if tag.RowsAffected() == 0 {
		return 0, nil // already recorded
	}
	return shares, nil
}

// grantCapped applies the verify-only referral ceiling.
//
// **This is the number that stops mass signup farming.** Below the cap a
// referral pays for the referred person merely verifying, which is what gives
// somebody a reason to invite people before the event opens. Above it,
// referrals only pay when the referred person actually ships code - which
// cannot be faked cheaply, and is the outcome the pool exists to buy.
//
// The read and the write are one transaction under a per-referrer advisory
// lock. Without it, two referrals completing at the same moment both read a
// total below the cap and both insert, taking the referrer over it - the
// classic check-then-act race, and the one that matters most here because
// referrals arrive in bursts precisely when somebody is farming.
func grantCapped(
	ctx context.Context,
	pool db.DBPool,
	userID uuid.UUID,
	shares float64,
	reason string,
	sourceRef *uuid.UUID,
	cfg map[string]string,
) (float64, error) {
	capShares := atofOr(cfg["founding_referral_verified_cap_shares"], 10)
	if capShares <= 0 {
		return 0, ErrReferralCapReached
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("founding.Grant(%s): begin: %w", reason, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"founding_referral_cap:"+userID.String()); err != nil {
		return 0, fmt.Errorf("founding.Grant(%s): lock: %w", reason, err)
	}

	var earned float64
	if err := tx.QueryRow(ctx, `
SELECT COALESCE(sum(shares), 0)::float8 FROM founding_shares
WHERE user_id = $1 AND reason = $2
`, userID, reason).Scan(&earned); err != nil {
		return 0, fmt.Errorf("founding.Grant(%s): sum: %w", reason, err)
	}

	remaining := capShares - earned
	if remaining <= 0 {
		return 0, ErrReferralCapReached
	}
	// A partial grant rather than an all-or-nothing refusal: the cap is a
	// ceiling on the total, not a filter on individual referrals, so the one
	// that crosses it should still pay out the part that fits.
	granted := shares
	if granted > remaining {
		granted = remaining
	}

	tag, err := tx.Exec(ctx, `
INSERT INTO founding_shares (user_id, shares, reason, source_ref)
VALUES ($1, $2, $3, $4)
ON CONFLICT DO NOTHING
`, userID, granted, reason, sourceRef)
	if err != nil {
		return 0, fmt.Errorf("founding.Grant(%s): insert: %w", reason, err)
	}
	if tag.RowsAffected() == 0 {
		return 0, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("founding.Grant(%s): commit: %w", reason, err)
	}
	return granted, nil
}

// TotalFor returns a user's raw share total, before their wave multiplier.
func TotalFor(ctx context.Context, pool db.DBPool, userID uuid.UUID) (float64, error) {
	var total float64
	err := pool.QueryRow(ctx, `
SELECT COALESCE(sum(shares), 0)::float8 FROM founding_shares WHERE user_id = $1
`, userID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("founding.TotalFor: %w", err)
	}
	return total, nil
}

// Breakdown is a user's raw shares grouped by how they were earned. This is
// what a "why do I have this many shares" question is answered with.
type Breakdown struct {
	VerifiedAccount  float64 `json:"verified_account"`
	MergedPR         float64 `json:"merged_pr"`
	ReferralVerified float64 `json:"referral_verified"`
	ReferralMergedPR float64 `json:"referral_merged_pr"`
	Total            float64 `json:"total"`
}

// BreakdownFor groups a user's shares by reason.
func BreakdownFor(ctx context.Context, pool db.DBPool, userID uuid.UUID) (Breakdown, error) {
	rows, err := pool.Query(ctx, `
SELECT reason, sum(shares)::float8 FROM founding_shares WHERE user_id = $1 GROUP BY reason
`, userID)
	if err != nil {
		return Breakdown{}, fmt.Errorf("founding.BreakdownFor: %w", err)
	}
	defer rows.Close()

	var b Breakdown
	for rows.Next() {
		var reason string
		var sum float64
		if err := rows.Scan(&reason, &sum); err != nil {
			return Breakdown{}, err
		}
		switch reason {
		case ReasonVerifiedAccount:
			b.VerifiedAccount = sum
		case ReasonMergedPR:
			b.MergedPR = sum
		case ReasonReferralVerified:
			b.ReferralVerified = sum
		case ReasonReferralMergedPR:
			b.ReferralMergedPR = sum
		}
		b.Total += sum
	}
	return b, rows.Err()
}

func atofOr(s string, fallback float64) float64 {
	if s == "" {
		return fallback
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fallback
	}
	return v
}

func atoiOr(s string, fallback int) int {
	if s == "" {
		return fallback
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return v
}
