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
	// TypePRMerged fires when a pull request authored by a linked Grainlify
	// user transitions to merged (internal/ingest/github_webhook.go).
	TypePRMerged Type = "pr_merged"
	// TypeRewardReceived is defined for forward compatibility with the
	// on-chain payout system (internal/soroban) - nothing publishes it yet,
	// since that system isn't wired to any request flow today. The
	// preference toggle exists in the UI so it's ready once payouts ship.
	TypeRewardReceived Type = "reward_received"
	// TypeReferralCompleted fires when someone a user referred finishes
	// GitHub signup + KYC verification, crediting the referrer with points
	// (internal/handlers/referrals.go maybeCompleteReferral).
	TypeReferralCompleted Type = "referral_completed"
	// TypeSocialFollowCompleted fires once a user's proof submissions for
	// every social-follow platform have been approved
	// (internal/handlers/social_follow.go maybeCompleteSocialFollow).
	TypeSocialFollowCompleted Type = "social_follow_completed"
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
	TypePRMerged,
	TypeRewardReceived,
	TypeReferralCompleted,
	TypeSocialFollowCompleted,
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
