// Package erasure carries out a person's request to delete their account.
//
// # The shape
//
// A request is recorded at once and executed after a grace period
// (GracePeriod), by code, without anybody pressing a button. During the grace
// period the person can cancel it and nothing has been touched. After it, the
// executor erases the account in place: the users row becomes an empty
// tombstone that payout records can keep pointing at, and everything that
// identifies the person is deleted or overwritten around it.
//
// Why automatic rather than an admin queue: every step is fixed in code and
// the same for everybody, so an administrator pressing "execute" would add a
// delay and a chance of forgetting, and nothing they could check that the code
// does not. The two judgements that do need care are made by code as well:
// whether money is still on its way to the person (then the erasure waits,
// "held", and proceeds by itself once it has arrived, or after MaxHold at the
// latest), and whether the other services were reached (then it retries, and
// records anything it could not do).
//
// Why a grace period at all: the request is made from a signed-in browser
// session, and a session can be somebody else's - a borrowed laptop, a stolen
// token. Erasure cannot be undone, so it waits long enough for the real owner
// to notice the notification the request sends and cancel.
//
// # What is not deleted, and why
//
// Retained lists every category kept, the reason, and for how long. It is the
// same list the settings screen shows before somebody confirms, and the list
// recorded on the request when it completes - one list, so the promise and
// the record cannot drift apart.
package erasure

import (
	"strings"
	"time"
)

// GracePeriod is how long a request waits before it is carried out. Seven
// days: long enough to notice a request you did not make, short enough that
// "delete my account" still means soon.
const GracePeriod = 7 * 24 * time.Hour

// MaxHold is the longest a due erasure waits for money still on its way to
// the person, counted from when it became due (the end of GracePeriod).
//
// Without a limit a hold could last for ever: a claim made directly on chain
// is invisible to us, so it holds until somebody notices, and a GrainHack
// assignment can sit active for weeks. "Delete my account" cannot mean
// "unless a payment is open". After MaxHold the erasure goes ahead, and the
// records of the money still in flight are kept, attached to the empty account
// record, so it can still be paid or resolved; the person is told when the
// hold begins, with the date it ends.
const MaxHold = 30 * 24 * time.Hour

// ErasedPlaceholder replaces a GitHub login in records that are kept, such as
// a GrainHack verdict: the row still says a verdict was given, and no longer
// says to whom.
const ErasedPlaceholder = "erased-account"

// Item is one line of the policy: what, why, and until when.
type Item struct {
	What  string `json:"what"`
	Why   string `json:"why,omitempty"`
	Until string `json:"until,omitempty"`
}

// Erased is what deletion removes, in the words the settings screen uses.
var Erased = []Item{
	{What: "Your profile: name, bio, location, website, social handles, avatar and email address"},
	{What: "Your GitHub connection, including the access token. We also ask GitHub to revoke Grainlify's access to your account"},
	{What: "Your identity-verification record: the decision Didit returned, its session identifiers, and the copies kept when an administrator reset a verification. We also ask Didit to delete the session and its images"},
	{What: "Wallets you signed in with and payout addresses you registered"},
	{What: "Your applications to issues, GrainHack issues and bounties, and the profile snapshots made to assess them"},
	{What: "Your notifications, notification settings, ratings and comments, social-follow screenshots and support requests"},
	{What: "Your wallet link and bounty applications held by the Grainlify bounty agent"},
	{What: "Your record of accepting these Terms"},
}

// Retained is what deletion keeps, why, and until when.
var Retained = []Item{
	{
		What:  "Records of payouts already made or owed to you: amount, date, transaction id and the address it was sent to",
		Why:   "Needed to account for money paid out, for tax and anti-money-laundering obligations, and to answer disputes. They point at an empty account record, not at your name. When they are erased, each payout round keeps only its total",
		Until: "5 years after the payment, then erased",
	},
	{
		What:  "Anything on a public blockchain: payout transactions and the addresses involved",
		Why:   "A blockchain cannot be edited by anybody, including us",
		Until: "permanently",
	},
	{
		What:  "Public bounty ledger entries for payouts already made",
		Why:   "The ledger is a public record of money paid, and is never edited silently. Your GitHub login is replaced with \"erased account\" where it is shown, and the ledger records publicly that an erasure took place",
		Until: "permanently",
	},
	{
		What:  "GrainHack verdicts, draws, assignments and appeals that decided a payout, with your GitHub login replaced",
		Why:   "They are the basis of payouts to you and to other people, and must still add up",
		Until: "5 years after the payment, then erased",
	},
	{
		What:  "For payouts collected by claim, the published claim list: each address and amount, whose fingerprint was published on the blockchain",
		Why:   "Every claim is checked against it, so it cannot change once published. It holds no account, GitHub login or name, and once the payout records above are erased nothing we hold connects an address in it to you",
		Until: "permanently",
	},
	{
		What:  "A record of identity-verification resets an administrator made: the date, the status before and the reason code, without your verification data",
		Why:   "Fraud prevention: it is the record of what was decided and by whom",
		Until: "90 days after the reset, then erased",
	},
	{
		What:  "Referral links, Founding Contributor Pool membership and shares, tied to the empty account record",
		Why:   "Other people's shares and the pool's published wave counts are computed from them. They hold no personal details once the account is erased, and an erased account is never paid",
		Until: "for the life of the pool",
	},
	{
		What:  "This deletion request: when it was made, what was erased and what was kept",
		Why:   "So we can show that the deletion was carried out. It records counts and reasons, never your data",
		Until: "permanently",
	},
	{
		What: "Your public GitHub activity on repositories listed on Grainlify, which we mirror from GitHub, and comments posted on GitHub on your behalf",
		Why:  "It is public on GitHub and belongs to the repositories. It still counts on the public leaderboard, as it does for people with no Grainlify account",
	},
	{
		What: "Projects you listed as a maintainer",
		Why:  "They belong to the repository, not to the account. Uninstall the Grainlify GitHub App to remove them",
	},
	{
		What:  "Database backups made before the erasure",
		Why:   "Backups cannot be edited row by row. They are deleted on their own schedule",
		Until: "until they age out (see Retention in the Privacy Policy)",
	},
	{
		What:  "Application logs written before the erasure",
		Why:   "Logs cannot be edited. They are deleted on their own schedule",
		Until: "until they age out (see Retention in the Privacy Policy)",
	},
	{
		What: "Support messages already delivered to our team's Telegram and Discord, and the billing profile your browser keeps on your device",
		Why:  "Messages already delivered to those services cannot be withdrawn by us; the browser copy is on your device. We clear it from this browser when you request deletion",
	},
}

// InFlightItem is added to the request's retained list when the erasure went
// ahead after MaxHold with money still in flight. The records it names are
// already kept (keptTables); this says why, in the list the request records.
func InFlightItem(reasons []string) Item {
	return Item{
		What: "Records of the money that was still on its way to you when your deletion went ahead (" + strings.Join(reasons, "; ") + ")",
		Why: "So it can still be paid to you or otherwise resolved. They are attached to the empty account record; " +
			"contact support to collect it",
		Until: "until it is resolved, then as for other payout records",
	}
}
