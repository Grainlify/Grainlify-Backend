package notifications

// Type identifies a category of notification. A user has independent
// in-app/email preferences per Type (internal/notifications service.go).
type Type string

const (
	// TypeIssueAssigned fires when a maintainer assigns a contributor to an
	// issue (internal/handlers/issue_applications.go Assign()).
	TypeIssueAssigned Type = "issue_assigned"
	// TypeIssueApplicationSubmitted fires when a contributor applies to an
	// issue, notifying the project owner (Apply()).
	TypeIssueApplicationSubmitted Type = "issue_application_submitted"
	// TypeIssueApplicationRejected fires when a maintainer rejects an
	// application (Reject()).
	TypeIssueApplicationRejected Type = "issue_application_rejected"
	// TypeIssueApplicationReceived confirms to the CONTRIBUTOR that their own
	// application was recorded and is waiting on the maintainer (Apply()).
	//
	// It exists because the applicant side was silent. Applying notified the
	// maintainer and told the applicant nothing, so the first message a
	// contributor ever received about their own application was its rejection
	// - 13 of the 14 people with an open application had never had a single
	// notification about it. A refusal arriving out of a silence reads as a
	// system that was never listening.
	TypeIssueApplicationReceived Type = "issue_application_received"
	// TypePRMerged fires when a pull request authored by a linked Grainlify
	// user transitions to merged (internal/ingest/github_webhook.go).
	TypePRMerged Type = "pr_merged"
	// TypeRewardReceived is defined for forward compatibility with the
	// on-chain payout system (internal/chain) - nothing publishes it yet,
	// since no payout path has ever run. The preference toggle exists in the
	// UI so it's ready once payouts ship.
	TypeRewardReceived Type = "reward_received"
	// TypeReferralCompleted fires when someone a user referred finishes
	// GitHub signup + KYC verification (internal/handlers/referrals.go
	// maybeCompleteReferral). It no longer credits points - the fixed-rate
	// programme is retired - and the referral now counts toward the Founding
	// Contributor Pool instead.
	TypeReferralCompleted Type = "referral_completed"
	// TypeSocialFollowCompleted carries every social-follow decision:
	// approved, rejected, or revoked (internal/handlers/social_follow.go
	// decide). The stored value keeps its original name because it is
	// persisted on existing notification rows and read by preference
	// filters; the name is narrower than what it now covers.
	//
	// Revocation is the reason this fires on more than approval. Eligibility
	// disappearing silently, and surfacing only when the pool is shared out,
	// is how a defensible decision comes to look arbitrary.
	TypeSocialFollowCompleted Type = "social_follow_completed"

	// An admin cleared a contributor's KYC status so they can verify again.
	// Worth telling them: a refused verification is otherwise terminal in the
	// UI, so somebody who was stuck has no way to discover that the door has
	// been reopened.
	TypeKYCReset Type = "kyc_reset"

	// TypeFoundingPosition tells somebody where they stand in the Founding
	// Contributor Pool: that they hold a position, that it is permanent, and
	// what it still needs.
	//
	// Its own type rather than a borrowed one. social_follow_completed means "a
	// decision was made about your proof", and sending this under it would make
	// the message mutable by a preference that means something else, and
	// miscategorised in a table whose link paths we have already had to repair
	// once. Its own type is also the only way it is mutable at all: the
	// preferences API iterates AllTypes, so a type missing from that list
	// produces a notification nobody can turn off.
	TypeFoundingPosition Type = "founding_position"
	// TypeRedemptionPaid fires when an admin marks a points->USDC redemption
	// request as paid (internal/handlers/redemptions.go).
	TypeRedemptionPaid Type = "redemption_paid"
	// TypeRedemptionRejected fires when an admin rejects a redemption
	// request; the spent points are refunded at the same time.
	TypeRedemptionRejected Type = "redemption_rejected"
	// TypeGrainHackIssueCapExceeded fires when a maintainer labels an issue
	// for GrainHack but the org has already hit max_issues_per_org for that
	// hackathon (internal/hackathon/intake.go SyncIssueLabel).
	TypeGrainHackIssueCapExceeded Type = "grainhack_issue_cap_exceeded"
	// TypeGrainHackApplicationAccepted fires when an admin accepts a
	// project's GrainHack application (internal/handlers/admin_hackathon_applications.go).
	TypeGrainHackApplicationAccepted Type = "grainhack_application_accepted"
	// TypeGrainHackApplicationReviewed fires when an admin rejects a
	// project's GrainHack application or requests more info on it.
	TypeGrainHackApplicationReviewed Type = "grainhack_application_reviewed"
	// TypeGrainHackAssigned fires when a contributor wins the weighted draw
	// for a GrainHack issue (internal/hackathon/runner.go).
	TypeGrainHackAssigned Type = "grainhack_assigned"
	// TypeGrainHackAssignmentReleased fires when an assignment is
	// auto-released for going stale, which also records an abandon
	// (AI-specs.md §4.6).
	TypeGrainHackAssignmentReleased Type = "grainhack_assignment_released"
	// TypeGrainHackEventEnding warns a contributor still holding an open
	// assignment that the event ends soon - sent *before* ends_at, per
	// AI-specs.md §13's second open question.
	TypeGrainHackEventEnding Type = "grainhack_event_ending"
)

// AllTypes is the canonical list iterated by the preferences API. Keep in
// sync with the Type constants above.
var AllTypes = []Type{
	TypeIssueAssigned,
	TypeIssueApplicationSubmitted,
	TypeIssueApplicationRejected,
	TypeIssueApplicationReceived,
	TypePRMerged,
	TypeRewardReceived,
	TypeReferralCompleted,
	TypeSocialFollowCompleted,
	TypeKYCReset,
	TypeFoundingPosition,
	TypeRedemptionPaid,
	TypeRedemptionRejected,
	TypeGrainHackIssueCapExceeded,
	TypeGrainHackApplicationAccepted,
	TypeGrainHackApplicationReviewed,
	TypeGrainHackAssigned,
	TypeGrainHackAssignmentReleased,
	TypeGrainHackEventEnding,
}

func (t Type) Valid() bool {
	for _, v := range AllTypes {
		if v == t {
			return true
		}
	}
	return false
}
