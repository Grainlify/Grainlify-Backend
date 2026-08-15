package handlers

import "time"

// The single definition of whether a support request has reached where it
// needs to go.
//
// "Delivered" is category-dependent, and that is load-bearing rather than
// incidental. KYC requests go to the admin's DM only - there is deliberately
// no public post, because a stub in a readable group discloses that somebody
// asked a verification question at a timestamp and tells nobody anything
// actionable. Everything else goes to its category topic and never to a DM.
//
// So each category permanently leaves one of the two Telegram columns NULL:
//
//	kyc              telegram_admin_dm_delivered_at is the delivery
//	everything else  telegram_delivered_at is the delivery
//
// Expressed here once, because the alternative is re-deriving it in every
// query and admin view - and a rule expressed twice is a rule that disagrees
// with itself. That is not hypothetical in this codebase: the leaderboard and
// the profile held two copies of the ranking rule and disagreed about the same
// contributor.
func supportTelegramDelivered(category string, topicAt, adminDMAt *time.Time) bool {
	if category == "kyc" {
		return adminDMAt != nil
	}
	return topicAt != nil
}

// supportFullyDelivered reports whether every configured route has landed.
func supportFullyDelivered(category string, discordAt, topicAt, adminDMAt *time.Time) bool {
	return discordAt != nil && supportTelegramDelivered(category, topicAt, adminDMAt)
}

// supportUndeliveredPredicate is the SQL form of NOT supportFullyDelivered,
// for the partial index and for any replay query.
//
// Kept byte-identical to the predicate in
// migrations/000064_support_delivery_semantics.up.sql.
// TestSupportDelivered_SQLAndGoAgree runs both against every combination of
// column states and asserts they return the same answer, rather than trusting
// two copies to stay in step - which is the failure this whole comment block
// exists to prevent.
const supportUndeliveredPredicate = `discord_delivered_at IS NULL
     OR (category = 'kyc' AND telegram_admin_dm_delivered_at IS NULL)
     OR (category <> 'kyc' AND telegram_delivered_at IS NULL)`
