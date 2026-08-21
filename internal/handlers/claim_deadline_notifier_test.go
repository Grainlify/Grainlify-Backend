package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

func mustUUID(t *testing.T) uuid.UUID { t.Helper(); return uuid.New() }

func seedDeadlineUser(t *testing.T, d *db.DB) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO users (role, display_name) VALUES ('contributor',$1) RETURNING id`,
		"deadline-"+uuid.NewString()).Scan(&id); err != nil {
		t.Fatalf("seedDeadlineUser: %v", err)
	}
	return id
}

// The milestone is the most urgent one currently in window, not every missed
// one. A job that starts late must not fire three messages at once.
func TestMilestoneFor_PicksTheMostUrgentApplicable(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	day := int64(86400)
	at := func(days int64) int64 { return now.Unix() + days*day }

	cases := []struct {
		daysOut int64
		want    claimDeadlineMilestone
		due     bool
	}{
		{30, "", false}, // nothing yet
		{14, milestoneT14, true},
		{10, milestoneT14, true}, // T-14 is due and overdue; T-7 is not yet applicable
		{7, milestoneT7, true},
		{5, milestoneT7, true},
		{3, milestoneT3, true},
		{1, milestoneT3, true}, // the last reminder, not a fourth on the final day
		{0, milestonePassed, true},
		{-5, milestonePassed, true},
	}
	for _, c := range cases {
		got, due := milestoneFor(at(c.daysOut), now)
		if due != c.due || got != c.want {
			t.Errorf("%+d days: got (%q,%v), want (%q,%v)", c.daysOut, got, due, c.want, c.due)
		}
	}
}

// An extension re-opens the milestones, because the dedup key carries the
// deadline. Asserted at the key level: the same milestone against a new
// deadline is a different row, so it is not suppressed.
func TestClaimDeadlineDedupKey_AnExtensionIsANewRow(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	n := &ClaimDeadlineNotifier{db: d}
	c := deadlineCandidate{settlementID: mustUUID(t), userID: seedDeadlineUser(t, d), amountMinor: 250_000}

	const original, extended = int64(1_800_000_000), int64(1_802_592_000)

	n.recordSent(ctx, c, milestoneT3, original)
	if sent, _ := n.alreadySent(ctx, c, milestoneT3, original); !sent {
		t.Fatal("the reminder just recorded does not read as sent")
	}
	// Same settlement, same user, same milestone - new deadline.
	if sent, _ := n.alreadySent(ctx, c, milestoneT3, extended); sent {
		t.Error("an extended deadline reads as already reminded; the new window would be silent")
	}
	n.recordSent(ctx, c, milestoneT3, extended)
	if sent, _ := n.alreadySent(ctx, c, milestoneT3, extended); !sent {
		t.Error("the reminder for the extended window did not record")
	}
}
