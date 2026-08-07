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
}

func (t Type) Valid() bool {
	for _, v := range AllTypes {
		if v == t {
			return true
		}
	}
	return false
}
