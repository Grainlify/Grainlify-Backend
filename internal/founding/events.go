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
		if errors.Is(err, ErrNotEligibleForWave) {
			// Not a failure. They verified without an approved social-follow
			// submission, so they hold no position yet; approving them later
			// calls OnSocialFollowApproved, which assigns one then. Info
			// rather than warn precisely so this does not read as breakage in
			// the logs - it is the gate working.
			slog.Info("founding: no wave yet, awaiting social follow approval", "user_id", userID)
		} else {
			slog.Warn("founding: wave assignment failed", "user_id", userID, "error", err)
			return
		}
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

// GrantMergedPRShares awards merged-PR shares for a whole event, once, at
// appeals close.
//
// **Two conditions, both required: the pull request is merged, and the final
// post-appeal verdict accepted it.** Neither alone is sound:
//
//   - Merge alone is a maintainer-controllable signal. A maintainer can merge
//     anything, and the maintainer-pool scoring already excludes every metric
//     a maintainer can move by themselves for exactly this reason.
//   - Verdict alone is not final. An appeal can change a bucket, so granting
//     at verdict time would mean building a clawback path - and clawing back
//     a share somebody has already been shown is precisely the problem the
//     appeals recompute exists to avoid.
//
// Hooked to the existing appeals-close recompute rather than to a new event,
// because that is the one moment at which nothing underneath can still move:
// every appeal has a human answer, the buckets are final, and the pool
// arithmetic has been redone. One grant, at the point of no further change.
//
// Safe to re-run. The unique index on (user, reason, source) makes a repeat a
// no-op, which matters because the recompute can be retried and because a
// partial failure here is best-effort - a missed grant is recoverable by
// running this again, since the source data is the verdict rows themselves.
//
// The consequence, which is documented rather than hidden: **merged-PR shares
// do not appear until after appeals close.** That is correct - nobody's share
// should be visible while it can still move - but it does mean a contributor
// sees a merged pull request and no shares for it until the window shuts.
func GrantMergedPRShares(ctx context.Context, pool db.DBPool, hackathonID uuid.UUID, cfg map[string]string) (int, error) {
	rows, err := pool.Query(ctx, `
SELECT v.id, v.user_id
FROM hackathon_verdicts v
JOIN github_pull_requests pr
  ON pr.project_id = v.project_id AND pr.number = v.pr_number
WHERE v.hackathon_id = $1
  AND pr.merged_at_github IS NOT NULL
  AND v.final_bucket IS NOT NULL
  AND v.final_bucket <> 'rejected'
  AND v.user_id IS NOT NULL
ORDER BY v.id
`, hackathonID)
	if err != nil {
		return 0, fmt.Errorf("founding.GrantMergedPRShares: load verdicts: %w", err)
	}
	defer rows.Close()

	type award struct {
		verdictID uuid.UUID
		userID    uuid.UUID
	}
	var awards []award
	for rows.Next() {
		var a award
		if err := rows.Scan(&a.verdictID, &a.userID); err != nil {
			return 0, fmt.Errorf("founding.GrantMergedPRShares: scan: %w", err)
		}
		awards = append(awards, a)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	granted := 0
	for _, a := range awards {
		// The verdict id is the source ref on both sides, so a contributor's
		// share and their referrer's share for the same pull request are each
		// recorded once and cannot double on a re-run.
		OnMergedPR(ctx, pool, a.userID, a.verdictID, cfg)
		granted++
	}
	return granted, nil
}

// OnMergedPR grants the shares for one accepted, merged pull request.
//
// Called by GrantMergedPRShares at appeals close rather than directly from a
// merge or a verdict - see that function for why both conditions are needed.
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

// OnSocialFollowApproved is called when an admin approves a social-follow
// submission. It is the other half of the gate.
//
// Wave assignment now needs two facts - the account is verified, and the
// submission is approved - and they arrive in either order from two unrelated
// events. Whichever happens second does the work:
//
//	verify then approve   OnVerified refuses (ErrNotEligibleForWave); this
//	                      assigns
//	approve then verify   this refuses (not verified); OnVerified assigns
//
// Without this, only verification could ever assign, so anybody approved after
// verifying would be stranded permanently by the gate that was meant to admit
// them. Twelve people are currently approved and unverified, waiting on
// exactly this path.
//
// Idempotent for free, because it converges on AssignWave: MembershipFor
// short-circuits a second call, the transaction-scoped advisory lock
// serialises a verify and an approve landing simultaneously, and the sequence
// is still allocated as max+1 under that lock, so it stays gapless.
//
// Deliberately does NOT grant shares. Only the wave is gated; an unapproved
// contributor still accrues the verification share, which settles to zero.
// Gating Grant is a separate policy question and is not decided here.
//
// Best-effort: an approval must succeed whatever happens to the assignment.
func OnSocialFollowApproved(ctx context.Context, pool db.DBPool, userID uuid.UUID, cfg map[string]string) {
	if pool == nil {
		return
	}

	// Assignment is for verified accounts. Checked here rather than inside
	// AssignWave because it is this event's precondition, not the gate's: the
	// gate is about approval, and conflating the two would mean a config flag
	// that says "social follow not required" also stopped requiring
	// verification.
	var status string
	if err := pool.QueryRow(ctx, `
SELECT COALESCE(kyc_status, '') FROM users WHERE id = $1
`, userID).Scan(&status); err != nil {
		slog.Warn("founding: could not read verification status after approval", "user_id", userID, "error", err)
		return
	}
	if status != "verified" {
		slog.Info("founding: approved but not yet verified, no wave assigned",
			"user_id", userID, "kyc_status", status)
		return
	}

	m, err := AssignWave(ctx, pool, userID, cfg)
	if err != nil {
		if errors.Is(err, ErrNotEligibleForWave) {
			// Should not happen - this runs after an approval commits - but a
			// revoke racing the approval could produce it, and a wrong log
			// line is worse than a surprising one.
			slog.Warn("founding: approval did not confer eligibility", "user_id", userID, "error", err)
			return
		}
		slog.Warn("founding: wave assignment after approval failed", "user_id", userID, "error", err)
		return
	}
	slog.Info("founding: wave assigned on social follow approval",
		"user_id", userID, "sequence_number", m.Sequence, "wave", m.Wave)
}
