package founding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// OnVerified is called when a user's identity verification completes.
//
// Two things happen, in this order and for different reasons:
//
//  1. The user gets their permanent wave assignment. This is the actual
//     reward for joining early - a multiplier and a badge - and it is worth
//     nothing unless they go on to ship something, which is precisely the
//     farming resistance wanted.
//  2. Their referrer, if any, earns verify-only referral shares, subject to
//     the cap.
//
// The verification share itself is deliberately tiny. At parity with a merged
// pull request, most of the pool would flow to people who did nothing but
// create an account, starving the contributors the programme exists to
// attract.
//
// Best-effort: never fails the caller. Identity verification must succeed
// whatever happens here, exactly as referral completion already does.
func OnVerified(ctx context.Context, pool db.DBPool, userID uuid.UUID, cfg map[string]string) {
	if pool == nil {
		return
	}

	if _, err := AssignWave(ctx, pool, userID, cfg); err != nil {
		slog.Warn("founding: wave assignment failed", "user_id", userID, "error", err)
		return
	}

	if _, err := Grant(ctx, pool, userID,
		atofOr(cfg["founding_share_verified_account"], 0.1),
		ReasonVerifiedAccount, &userID, cfg); err != nil {
		slog.Warn("founding: verified-account share failed", "user_id", userID, "error", err)
	}

	referrer, referralID, ok, err := referrerOf(ctx, pool, userID)
	if err != nil {
		slog.Warn("founding: referrer lookup failed", "user_id", userID, "error", err)
		return
	}
	if !ok {
		return
	}

	granted, err := Grant(ctx, pool, referrer,
		atofOr(cfg["founding_share_referral_verified"], 0.5),
		ReasonReferralVerified, &referralID, cfg)
	switch {
	case errors.Is(err, ErrReferralCapReached):
		// The cap doing its job is not a failure. Logged at info so the
		// number can be observed working rather than inferred.
		slog.Info("founding: verify-only referral cap reached", "referrer_user_id", referrer)
	case err != nil:
		slog.Warn("founding: referral-verified share failed", "referrer_user_id", referrer, "error", err)
	case granted > 0:
		slog.Info("founding: referral-verified shares granted", "referrer_user_id", referrer, "shares", granted)
	}
}

// OnMergedPR is called when a contributor's pull request is merged during the
// first GrainHack.
//
// Grants the contributor their merged-PR shares and, separately, grants their
// referrer the referral-merged-PR shares. The referrer's side is uncapped on
// purpose: unlike a signup it cannot be faked cheaply, so it is exactly the
// referral behaviour worth paying for without limit.
//
// sourceRef must identify the merged PR uniquely (the verdict id is the
// natural choice) so a retry cannot pay twice.
func OnMergedPR(ctx context.Context, pool db.DBPool, userID uuid.UUID, sourceRef uuid.UUID, cfg map[string]string) {
	if pool == nil {
		return
	}

	if _, err := Grant(ctx, pool, userID,
		atofOr(cfg["founding_share_merged_pr"], 5),
		ReasonMergedPR, &sourceRef, cfg); err != nil {
		slog.Warn("founding: merged-PR share failed", "user_id", userID, "error", err)
	}

	referrer, _, ok, err := referrerOf(ctx, pool, userID)
	if err != nil || !ok {
		if err != nil {
			slog.Warn("founding: referrer lookup failed", "user_id", userID, "error", err)
		}
		return
	}
	if _, err := Grant(ctx, pool, referrer,
		atofOr(cfg["founding_share_referral_merged_pr"], 5),
		ReasonReferralMergedPR, &sourceRef, cfg); err != nil {
		slog.Warn("founding: referral-merged-PR share failed", "referrer_user_id", referrer, "error", err)
	}
}

// referrerOf returns who referred userID, and the referral row's id.
//
// Reads the referral regardless of its status. The points programme's
// completion state is a fact about a retired system; what matters here is
// only that the referral relationship exists.
func referrerOf(ctx context.Context, pool db.DBPool, userID uuid.UUID) (uuid.UUID, uuid.UUID, bool, error) {
	var referrer, referralID uuid.UUID
	err := pool.QueryRow(ctx, `
SELECT referrer_user_id, id FROM referrals WHERE referred_user_id = $1
`, userID).Scan(&referrer, &referralID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, uuid.Nil, false, fmt.Errorf("founding.referrerOf: %w", err)
	}
	return referrer, referralID, true, nil
}
