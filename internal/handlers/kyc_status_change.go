package handlers

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

// applyKYCStatus writes a decision and reports what it replaced.
//
// # One definition, because "did it change" has to mean the same thing twice
//
// The webhook and the reconciler used to carry their own copy of this UPDATE.
// That was defensible while it was only a write - two statements that happen to
// agree. It stopped being defensible the moment a notification hangs off the
// transition, because a notification that fires in one path and not the other,
// or twice for one change, is a worse bug than the drift the duplication risked.
//
// # Why the previous value comes from the statement rather than a read
//
// Both callers had a "previous status" already, and neither was trustworthy:
// the webhook read it in a separate statement and says so in its own comment,
// and the reconciler took it from a queue query up to twenty-five sessions
// earlier. Either can be stale by the time the write lands, and a stale
// previous value produces exactly the two failures worth avoiding - a missed
// notification when it looks unchanged, and a duplicate when two deliveries
// race.
//
// So the previous value is taken inside the same statement, under FOR UPDATE.
// The row is locked, the snapshot is the row as it was, and concurrent writers
// serialise. `changed` is then a fact rather than an inference.
//
// kyc_verified_at is still stamped only on the transition INTO verified, which
// is what stopped every founding member being re-dated by redelivery.
func applyKYCStatus(
	ctx context.Context, d *db.DB, userID uuid.UUID, status string, decisionJSON []byte,
) (previous string, changed bool, err error) {
	err = d.Pool.QueryRow(ctx, `
WITH prev AS (
  SELECT kyc_status AS old_status FROM users WHERE id = $3 FOR UPDATE
)
UPDATE users u
SET kyc_status = $1,
    kyc_data = $2,
    kyc_verified_at = CASE
      WHEN $1 = 'verified' AND u.kyc_status IS DISTINCT FROM 'verified' THEN now()
      ELSE u.kyc_verified_at
    END,
    updated_at = now()
FROM prev
WHERE u.id = $3
RETURNING COALESCE(prev.old_status, '')
`, status, decisionJSON, userID).Scan(&previous)
	if err != nil {
		return "", false, err
	}
	return previous, previous != status, nil
}

// kycStatusNotice is what a contributor is told about one transition.
type kycStatusNotice struct {
	Title string
	Body  string
}

// noticeForKYCStatus returns the message for a transition into status, or false
// when the transition is not worth interrupting somebody for.
//
// # Not every status is news
//
// pending and not_started are transient and self-inflicted - the person just
// opened the verification flow, so telling them it has started is noise. The
// four below are decisions or dead ends, which are the states somebody cannot
// discover by continuing what they were already doing.
//
// # Why the refusal copy is the longest
//
// verified -> rejected is the worst message this system sends anybody, and the
// damage is not the bad news. It is the person who reads it, concludes the
// matter is closed, and never asks. So it names both routes out: a retry, which
// is genuinely available because canStartNewKYCSession admits "rejected", and a
// human, for the case where the decision is wrong.
//
// The same rule the superseded-address copy follows: never end on the problem.
func noticeForKYCStatus(previous, status string) (kycStatusNotice, bool) {
	switch status {
	case "verified":
		return kycStatusNotice{
			Title: "Your identity is verified",
			Body: "Verification is complete, so everything that needed it is now open to you - " +
				"including your place in the Founding Contributor Pool. Nothing further is needed.",
		}, true

	case "rejected":
		body := "Your identity verification was reviewed and not approved. This is not the end of it:\n\n" +
			"You can start a new verification from Billing whenever you are ready. " +
			"The most common reason is a photo the checks could not read - a clearer, uncropped " +
			"picture of the whole document, in good light, resolves most refusals.\n\n" +
			"If you think this decision is wrong, contact support and a person will look at it again. " +
			"Please do ask - we would much rather re-check a decision than have you assume it is settled."
		if previous == "verified" {
			body = "Your identity verification is no longer approved, following a review by our " +
				"verification provider.\n\n" + body
		}
		return kycStatusNotice{Title: "Your identity verification was not approved", Body: body}, true

	case "in_review":
		return kycStatusNotice{
			Title: "Your verification is being reviewed",
			Body: "Someone is looking at your verification by hand. There is nothing for you to do " +
				"and nothing has gone wrong - we will tell you as soon as there is a decision. " +
				"If you have been waiting more than a few days, contact support and we will chase it.",
		}, true

	case "expired":
		return kycStatusNotice{
			Title: "Your verification session expired",
			Body: "The verification link timed out before it was finished, which happens easily and " +
				"costs you nothing. You can start a new one from Billing whenever suits you.",
		}, true
	}
	return kycStatusNotice{}, false
}

// notifyKYCStatusChange tells the contributor, for transitions worth telling
// them about.
//
// Best-effort: the caller's write has already committed and must not be undone
// by a notification failing. A silent failure is still logged, because "nobody
// was told" is the condition this exists to prevent and it should not be
// discoverable only by asking the person.
func notifyKYCStatusChange(
	ctx context.Context, notify *notifications.Service, userID uuid.UUID, previous, status string,
) {
	if notify == nil {
		return
	}
	notice, worth := noticeForKYCStatus(previous, status)
	if !worth {
		return
	}
	res := notify.NotifyInApp(ctx, userID, notifications.TypeKYCStatusChanged,
		notice.Title, notice.Body, notifications.SettingsLink(notifications.SubtabBilling))
	switch {
	case res.Err != nil:
		slog.Error("kyc status notification failed; the contributor was not told their status changed",
			"user_id", userID, "was", previous, "now", status, "error", res.Err)
	case res.Suppressed:
		slog.Info("kyc status notification suppressed by preference",
			"user_id", userID, "was", previous, "now", status)
	}
}
