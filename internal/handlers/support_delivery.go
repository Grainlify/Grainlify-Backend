package handlers

import "time"

// The single definition of whether a support request has reached where it
// needs to go.
//
// "Delivered" is category-dependent, and that is load-bearing rather than
// incidental. A KYC request goes to exactly one place - the admin's Telegram
// DM - and is deliberately kept out of both public-ish destinations:
//
//   - not posted to a Telegram topic, because @Grainlify is readable without
//     joining, and a stub there discloses that somebody asked a verification
//     question at a timestamp while telling nobody anything actionable;
//   - not sent to Discord at all, because the same details plus the reporter's
//     GitHub login would otherwise flow there anyway, leaving the privacy of
//     the category resting on a channel permission staying correct. A
//     permission is configuration; this is code.
//
// So a KYC row permanently leaves TWO columns NULL, and a non-KYC row
// permanently leaves one:
//
//	kyc              telegram_admin_dm_delivered_at is the whole delivery
//	                 discord_delivered_at and telegram_delivered_at stay NULL
//	everything else  discord_delivered_at AND telegram_delivered_at
//	                 telegram_admin_dm_delivered_at stays NULL
//
// Expressed here once, because the alternative is re-deriving it in every
// query and admin view - and a rule expressed twice is a rule that disagrees
// with itself. That is not hypothetical in this codebase: the leaderboard and
// the profile held two copies of the ranking rule and disagreed about the same
// contributor.

// supportDiscordDelivered reports whether Discord has nothing left to do.
//
// True for KYC without looking at the timestamp: the request was never sent,
// so there is nothing to wait for. Reading this as "delivered" is the correct
// reading for every caller that matters - a replay must not keep retrying a
// send that is never going to happen - but it is NOT evidence that anything
// was posted, and nothing should present it as such.
func supportDiscordDelivered(category string, discordAt *time.Time) bool {
	if category == "kyc" {
		return true
	}
	return discordAt != nil
}

// supportTelegramDelivered reports whether Telegram has nothing left to do.
func supportTelegramDelivered(category string, topicAt, adminDMAt *time.Time) bool {
	if category == "kyc" {
		return adminDMAt != nil
	}
	return topicAt != nil
}

// supportFullyDelivered reports whether every route this category actually
// uses has landed.
func supportFullyDelivered(category string, discordAt, topicAt, adminDMAt *time.Time) bool {
	return supportDiscordDelivered(category, discordAt) &&
		supportTelegramDelivered(category, topicAt, adminDMAt)
}

// supportUndeliveredPredicate is the SQL form of NOT supportFullyDelivered,
// for the partial index and for any replay query.
//
// Written as one branch per category rather than as three ORed NULL checks,
// because the categories no longer share a shape: KYC waits on one column,
// everything else waits on two. The earlier version could never be false,
// which would have made a replay job redeliver every row on every run for
// ever.
//
// Kept byte-identical to the predicate in
// migrations/000064_support_delivery_semantics.up.sql.
// TestSupportDelivered_SQLAndGoAgree runs both against every combination of
// column states and asserts they return the same answer, rather than trusting
// two copies to stay in step - which is the failure this whole comment block
// exists to prevent.
const supportUndeliveredPredicate = `(category = 'kyc' AND telegram_admin_dm_delivered_at IS NULL)
     OR (category <> 'kyc' AND (discord_delivered_at IS NULL OR telegram_delivered_at IS NULL))`
