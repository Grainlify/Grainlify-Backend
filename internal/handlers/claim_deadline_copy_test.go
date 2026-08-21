package handlers

import (
	"strings"
	"testing"
)

// Nothing happens on a date, so no message may say it does.
//
// sweep_unclaimed needs now >= claim_deadline AND an admin signer. There is no
// scheduler. "We may return unclaimed funds after 12 March" is true; "your
// payout will be returned on 12 March" is not, and the second is what a writer
// produces without thinking about it.
func TestClaimDeadlineCopy_NeverStatesADateOnWhichSomethingHappens(t *testing.T) {
	for _, m := range []claimDeadlineMilestone{milestoneT14, milestoneT7, milestoneT3, milestonePassed} {
		n := noticeForDeadline(m, "12 March 2028", "12.50", "USDC")
		body := n.Body
		// Assertions only. "automatically on" was in this list and flagged
		// "Nothing happens automatically on that date" - a sentence that DENIES
		// the event, which is exactly the copy we want. A substring test cannot
		// see negation, so the list has to contain phrases that are assertions
		// whatever precedes them.
		for _, forbidden := range []string{
			"will be returned on",
			"will be swept",
			"is returned on",
			"funds are returned on",
		} {
			if strings.Contains(strings.ToLower(body), forbidden) {
				t.Errorf("%s says %q, which asserts an event on a date: nothing happens on that date\n%s",
					m, forbidden, body)
			}
		}
	}
}

// Every message names a route out, and the deadline is never presented as final
// because extend_deadline exists and is the documented remedy.
func TestClaimDeadlineCopy_EveryMessageSaysTheDateCanMove(t *testing.T) {
	for _, m := range []claimDeadlineMilestone{milestoneT14, milestoneT7, milestoneT3, milestonePassed} {
		n := noticeForDeadline(m, "12 March 2028", "12.50", "USDC")
		if !strings.Contains(strings.ToLower(n.Body), "extend") {
			t.Errorf("%s does not mention extending the window, so the date reads as final:\n%s", m, n.Body)
		}
	}
}

// The post-deadline message is the one that reaches somebody who has already
// failed. It has to say three things, in this order: the money is still there,
// it was not taken, and asking is normal.
func TestClaimDeadlineCopy_ThePassedMessageDoesNotReadAsAnEpitaph(t *testing.T) {
	n := noticeForDeadline(milestonePassed, "12 March 2028", "12.50", "USDC")
	// Title and body together: "still waiting" is the title, and a reader meets
	// both. Checking only the body tested a smaller thing than the name claims.
	body := strings.ToLower(n.Title + "\n" + n.Body)

	for _, want := range []string{"still waiting", "has not been returned", "still sitting", "not too late"} {
		if !strings.Contains(body, want) {
			t.Errorf("post-deadline message is missing %q:\n%s", want, n.Body)
		}
	}
	// It must not apologise or imply fault. Somebody who missed a window while
	// installing their first wallet did not do anything wrong.
	for _, forbidden := range []string{"unfortunately", "you failed", "you missed your chance", "sorry"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("post-deadline message contains %q, which reads as blame or as an epitaph", forbidden)
		}
	}
}

// The three-day message is the last one, so it has to say start now rather than
// on the day - the whole reason it is not sent on the final day.
func TestClaimDeadlineCopy_TheLastReminderSaysStartNow(t *testing.T) {
	n := noticeForDeadline(milestoneT3, "12 March 2028", "12.50", "USDC")
	if !strings.Contains(strings.ToLower(n.Body), "start now rather than on the day") {
		t.Errorf("the final reminder does not tell somebody to start now:\n%s", n.Body)
	}
}

// The schedule is three milestones and the last is not the final day. A change
// here should be a decision, not a drift.
func TestClaimDeadlineSchedule_LastReminderIsNotTheFinalDay(t *testing.T) {
	if len(claimDeadlineSchedule) != 3 {
		t.Fatalf("schedule has %d milestones, want 3", len(claimDeadlineSchedule))
	}
	last := claimDeadlineSchedule[len(claimDeadlineSchedule)-1]
	if last.days < 2 {
		t.Errorf("last reminder is %d days out; a final-day message is only actionable by "+
			"somebody who can finish within hours, which is not this population", last.days)
	}
	for i := 1; i < len(claimDeadlineSchedule); i++ {
		if claimDeadlineSchedule[i].days >= claimDeadlineSchedule[i-1].days {
			t.Errorf("schedule is not strictly decreasing at %d", i)
		}
	}
}
