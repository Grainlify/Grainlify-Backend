package handlers

import "fmt"

// What a contributor is told about a closing claim window.
//
// Separated from the sender so the wording can be asserted directly, the same
// reason claimAddressCopy.ts is a module on the frontend: a test that reads
// "does this name a route out" is worth having, and the same test through a
// delivery path is not.
//
// # The rule every message follows: never state a date on which something happens
//
// Nothing happens on a date. sweep_unclaimed requires now >= claim_deadline AND
// an admin signer, and migration 000079 states the principle - "the sweep
// deadline is an act rather than a clock: a timer fires while nobody is
// watching". There is no scheduler and no cron.
//
// So "we may return unclaimed funds after 12 March" is true and "your payout
// will be returned on 12 March" is not. The second is the sentence a reader
// writes without thinking about it.
type claimDeadlineMilestone string

const (
	milestoneT14    claimDeadlineMilestone = "t_minus_14"
	milestoneT7     claimDeadlineMilestone = "t_minus_7"
	milestoneT3     claimDeadlineMilestone = "t_minus_3"
	milestonePassed claimDeadlineMilestone = "passed"
)

// claimDeadlineSchedule is every milestone before the deadline, in days.
//
// Fourteen, seven and three, and the last is deliberately not the final day.
// Claiming needs a wallet install, a network switch and a signature, and for
// somebody who has never used a wallet that can fail in ways which take days -
// a lost seed phrase, a wrong network, a device they no longer have. A
// final-day message is only actionable by somebody who can finish within hours,
// which is precisely not this population.
//
// A PRIOR, NOT A MEASUREMENT. No payout has ever run: there are zero claims,
// zero deadlines and zero misses, so nothing here is derived from how long
// people actually take. The closest proxy we now capture is the time from
// `register_now` first being shown to contributor_addresses.verified_at - the
// same task shape - and it should be run before the first publication and these
// numbers revised. Labelled rather than presented as evidence.
var claimDeadlineSchedule = []struct {
	days      int
	milestone claimDeadlineMilestone
}{
	{14, milestoneT14},
	{7, milestoneT7},
	{3, milestoneT3},
}

type claimDeadlineNotice struct {
	Title string
	Body  string
}

// noticeForDeadline builds the message for one milestone.
//
// `on` is the deadline rendered for a human. `amount` and `symbol` are the
// person's own figures, which are theirs to know - §6 forbids publishing a
// per-person number, not telling somebody their own.
func noticeForDeadline(m claimDeadlineMilestone, on, amount, symbol string) claimDeadlineNotice {
	switch m {
	case milestonePassed:
		// The one that reaches somebody who has already failed, and the only
		// one whose truth depends on the sweep NOT having run - which is
		// checked before this is ever called.
		return claimDeadlineNotice{
			Title: "Your payout is still waiting",
			Body: fmt.Sprintf(
				"The claim window for your %s %s closed on %s, and you have not claimed it yet.\n\n"+
					"It has not been returned. Unclaimed payouts come back to Grainlify rather than "+
					"being destroyed, and we have not done that here — your payout is still sitting "+
					"in the escrow with your name on it.\n\n"+
					"Contact us and we will extend the window so you can claim it. This is routine, "+
					"it is not a favour, and it is not too late.",
				amount, symbol, on),
		}
	case milestoneT3:
		return claimDeadlineNotice{
			Title: "A few days left to claim your payout",
			Body: fmt.Sprintf(
				"Your %s %s is waiting, and the claim window closes on %s.\n\n"+
					"If you have not set up a wallet yet, start now rather than on the day — "+
					"installing one and switching network can take longer than you expect, and it is "+
					"much easier to fix a problem with a few days in hand.\n\n"+
					"If you miss it, tell us. We can extend the window, and unclaimed payouts come "+
					"back to us rather than being destroyed.",
				amount, symbol, on),
		}
	default:
		return claimDeadlineNotice{
			Title: "Your payout is ready to claim",
			Body: fmt.Sprintf(
				"Your %s %s is waiting for you. The claim window closes on %s.\n\n"+
					"Claiming needs an Aptos wallet — Petra is the one we have tested end to end. "+
					"If you do not have one yet, that is the part worth doing early: the claim itself "+
					"takes a minute once the wallet exists.\n\n"+
					"After the window closes we may return unclaimed payouts to Grainlify. Nothing "+
					"happens automatically on that date, and if you are late you can ask us to "+
					"extend it.",
				amount, symbol, on),
		}
	}
}
