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
