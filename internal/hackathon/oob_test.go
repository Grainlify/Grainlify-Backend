package hackathon

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// The default has to stay "revert", or activating this gate would silently
// switch off enforcement for every project that never configured it.
func TestAutoRevertOOBAssignment_DefaultsToReverting(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	_, projectID, _ := fxLiveHackathon(t, d.Pool)

	if !AutoRevertOOBAssignment(ctx, d.Pool, projectID) {
		t.Error("auto_revert_oob_assignment defaulted to off; it ships as true and nothing here changed it")
	}
}

// The switch has to actually stop the GitHub write. It was seeded, shown in
// the admin UI, and read by nothing - an admin could turn it off, see it off,
// and Grainlify would keep removing assignees and commenting on repositories
// it does not own.
func TestAutoRevertOOBAssignment_PerHackathonOverrideTurnsItOff(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	hackathonID, projectID, owner := fxLiveHackathon(t, d.Pool)

	if _, err := d.Pool.Exec(ctx, `
INSERT INTO hackathon_config_settings (hackathon_id, key, value, updated_by)
VALUES ($1, 'auto_revert_oob_assignment', 'false', $2)
`, hackathonID, owner); err != nil {
		t.Fatalf("set override: %v", err)
	}

	if AutoRevertOOBAssignment(ctx, d.Pool, projectID) {
		t.Error("a per-hackathon override of 'false' did not turn off the revert")
	}
}

// A global default of false applies to a project carrying the literal
// "GrainHack" label without belonging to any hackathon - those are protected
// by EffectiveGrainHackLabels, so they reach this gate too.
func TestAutoRevertOOBAssignment_GlobalDefaultAppliesWithoutAHackathon(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	owner := fxUser(t, d.Pool)
	projectID := fxProject(t, d.Pool, owner, "")

	if !AutoRevertOOBAssignment(ctx, d.Pool, projectID) {
		t.Fatal("expected the shipped global default (true) for a project with no hackathon")
	}

	if _, err := d.Pool.Exec(ctx, `
INSERT INTO hackathon_config_settings (hackathon_id, key, value, updated_by)
VALUES (NULL, 'auto_revert_oob_assignment', 'false', $1)
ON CONFLICT (key) WHERE hackathon_id IS NULL DO UPDATE SET value = 'false'
`, owner); err != nil {
		t.Fatalf("set global default: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Pool.Exec(context.Background(),
			`UPDATE hackathon_config_settings SET value = 'true' WHERE hackathon_id IS NULL AND key = 'auto_revert_oob_assignment'`)
	})

	if AutoRevertOOBAssignment(ctx, d.Pool, projectID) {
		t.Error("a global default of 'false' did not turn off the revert")
	}
}

// Fails closed. The two directions are not symmetric: not reverting lapses
// for one sync tick, while wrongly reverting writes to someone else's
// repository and notifies a contributor that they were removed.
func TestAutoRevertOOBAssignment_FailsClosedOnError(t *testing.T) {
	d := dbtest.DB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // every query on this context now fails

	if AutoRevertOOBAssignment(ctx, d.Pool, uuid.New()) {
		t.Error("returned true when the config could not be read; it must fail closed and leave the repository alone")
	}
}

// §2.3 step 3: the event is recorded whether or not the revert fired. The
// switch controls whether Grainlify writes to someone else's repository, not
// whether it notices - and since this count feeds maintainer-pool
// eligibility, a switch that erased the evidence would be the first thing a
// maintainer gaming the event would turn off.
func TestRecordOOBAssignment_RecordedWhetherOrNotTheRevertFired(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, d.Pool)

	if err := RecordOOBAssignment(ctx, d.Pool, projectID, 101, "reverted-user", true); err != nil {
		t.Fatalf("record reverted: %v", err)
	}
	if err := RecordOOBAssignment(ctx, d.Pool, projectID, 102, "left-in-place", false); err != nil {
		t.Fatalf("record not-reverted: %v", err)
	}

	summaries, err := OOBOrgSummaries(ctx, d.Pool, hackathonID)
	if err != nil {
		t.Fatalf("OOBOrgSummaries: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("got %d org summaries, want 1", len(summaries))
	}
	if summaries[0].Total != 2 {
		t.Errorf("total = %d, want 2 - both events count, reverted or not", summaries[0].Total)
	}
	if summaries[0].DistinctAssignees != 2 {
		t.Errorf("distinct_assignees = %d, want 2", summaries[0].DistinctAssignees)
	}

	// The detail a reviewer needs: which issue, which account, and whether
	// the revert fired.
	events, err := OOBAssignmentsForOrg(ctx, d.Pool, hackathonID, summaries[0].OrgLogin)
	if err != nil {
		t.Fatalf("OOBAssignmentsForOrg: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	byLogin := map[string]OOBAssignment{}
	for _, e := range events {
		byLogin[e.AssignedLogin] = e
	}
	if !byLogin["reverted-user"].Reverted {
		t.Error("the reverted event was not stored as reverted")
	}
	if byLogin["left-in-place"].Reverted {
		t.Error("the un-reverted event was stored as reverted")
	}
	if byLogin["left-in-place"].MaintainerID == nil {
		t.Error("no responsible maintainer recorded; §2.3 records against the maintainer and org")
	}
	if byLogin["left-in-place"].IssueNumber != 102 {
		t.Errorf("issue_number = %d, want 102", byLogin["left-in-place"].IssueNumber)
	}
}

// A standing un-reverted assignment must not inflate the count. The sync
// worker re-reads every issue on every run, so without this the act of
// turning the revert off would walk an innocent org over the threshold on its
// own.
func TestRecordOOBAssignment_ReObservationDoesNotInflateAnUnrevertedEvent(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, d.Pool)

	for i := 0; i < 5; i++ {
		if err := RecordOOBAssignment(ctx, d.Pool, projectID, 200, "squatter", false); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}

	summaries, err := OOBOrgSummaries(ctx, d.Pool, hackathonID)
	if err != nil {
		t.Fatalf("OOBOrgSummaries: %v", err)
	}
	if summaries[0].Total != 1 {
		t.Errorf("total = %d after 5 sightings of one standing assignment, want 1", summaries[0].Total)
	}
	if summaries[0].Flagged {
		t.Error("an org was flagged purely by the sync worker re-reading the same unreverted assignment")
	}
}

// Re-assignment after a revert IS a new violation: the assignee was removed
// from GitHub, so seeing them assigned again means someone assigned them again.
func TestRecordOOBAssignment_ReAssignmentAfterARevertCounts(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, d.Pool)

	for i := 0; i < 3; i++ {
		if err := RecordOOBAssignment(ctx, d.Pool, projectID, 300, "persistent", true); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}

	summaries, err := OOBOrgSummaries(ctx, d.Pool, hackathonID)
	if err != nil {
		t.Fatalf("OOBOrgSummaries: %v", err)
	}
	if summaries[0].Total != 3 {
		t.Errorf("total = %d, want 3 - each re-assignment after a revert is a new violation", summaries[0].Total)
	}
}

// Crossing the threshold flags for review and does nothing else. It must not
// carry a penalty: three out-of-band assignments could be a maintainer who
// has not read the rules or one routing issues to an alt account, and those
// are indistinguishable from here.
func TestOOBOrgSummaries_ThresholdFlagsForReviewWithoutPenalising(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	hackathonID, projectID, _ := fxLiveHackathon(t, d.Pool)

	for i := 0; i < 2; i++ {
		if err := RecordOOBAssignment(ctx, d.Pool, projectID, 400+i, "acct", true); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	summaries, err := OOBOrgSummaries(ctx, d.Pool, hackathonID)
	if err != nil {
		t.Fatalf("OOBOrgSummaries: %v", err)
	}
	if summaries[0].Flagged {
		t.Fatalf("flagged at %d, below the default threshold of 3", summaries[0].Total)
	}
	if summaries[0].Threshold != 3 {
		t.Errorf("threshold = %d, want the shipped default of 3", summaries[0].Threshold)
	}

	if err := RecordOOBAssignment(ctx, d.Pool, projectID, 402, "acct", true); err != nil {
		t.Fatalf("record third: %v", err)
	}
	summaries, err = OOBOrgSummaries(ctx, d.Pool, hackathonID)
	if err != nil {
		t.Fatalf("OOBOrgSummaries: %v", err)
	}
	if !summaries[0].Flagged {
		t.Errorf("total %d did not flag at threshold %d", summaries[0].Total, summaries[0].Threshold)
	}

	// Flagging is advisory. Nothing in the maintainer's standing changed:
	// their accepted application is untouched and no eligibility field moved.
	var appStatus string
	if err := d.Pool.QueryRow(ctx, `
SELECT status FROM hackathon_project_applications WHERE hackathon_id = $1 AND project_id = $2
`, hackathonID, projectID).Scan(&appStatus); err != nil {
		t.Fatalf("read application: %v", err)
	}
	if appStatus != "accepted" {
		t.Errorf("application status = %q after flagging, want it untouched at accepted - flagging must not auto-penalise", appStatus)
	}
}
