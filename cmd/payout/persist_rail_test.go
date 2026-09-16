package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
	"github.com/jagadeesh/grainlify/backend/internal/settlement"
)

// What an operator running `payout persist` sees when the event is already being
// paid on KeeperHub: the policy sentence, printed as-is by main, never a
// database exception.
//
// Drives cmdPersist with the test database's pool directly rather than running
// the binary, because the binary reads DB_URL and must never be pointed
// anywhere by a test.
func TestPersistCommand_ExplainsTheOneRailRefusal(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()

	var owner, project, hid, prun, uid uuid.UUID
	must := func(err error, what string) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	must(d.Pool.QueryRow(ctx, `INSERT INTO users DEFAULT VALUES RETURNING id`).Scan(&owner), "owner")
	must(d.Pool.QueryRow(ctx, `INSERT INTO projects (owner_user_id, github_full_name) VALUES ($1,$2) RETURNING id`,
		owner, "acme/cli-"+uuid.NewString()[:8]).Scan(&project), "project")
	must(d.Pool.QueryRow(ctx, `INSERT INTO hackathons (name, contributor_prize_pool) VALUES ($1, 1) RETURNING id`,
		"cli-"+uuid.NewString()[:8]).Scan(&hid), "hackathon")
	must(d.Pool.QueryRow(ctx, `INSERT INTO users DEFAULT VALUES RETURNING id`).Scan(&uid), "user")
	_, err := d.Pool.Exec(ctx, `
		INSERT INTO hackathon_verdicts (hackathon_id, project_id, pr_number, user_id, github_login, final_bucket, units, curve_multiplier)
		VALUES ($1, $2, 1, $3, 'cli-user', 'accepted', 1, 1)`, hid, project, uid)
	must(err, "verdict")
	must(d.Pool.QueryRow(ctx, `
		INSERT INTO hackathon_payout_runs (hackathon_id, contributor_prize_pool, total_units, unit_value)
		VALUES ($1, 1, 1, 1) RETURNING id`, hid).Scan(&prun), "payout run")
	_, err = d.Pool.Exec(ctx, `
		INSERT INTO keeperhub_payout_runs (hackathon_id, pool, chain_id, evm_chain_id, pool_minor, hackathon_payout_run_id, state)
		VALUES ($1, 'contributor', 'base-sepolia', 84532, 1000000, $2, 'planned')`, hid, prun)
	must(err, "keeperhub run")
	t.Cleanup(func() {
		ctx := context.Background()
		d.Pool.Exec(ctx, `DELETE FROM settlements WHERE hackathon_id = $1`, hid)
		d.Pool.Exec(ctx, `DELETE FROM hackathons WHERE id = $1`, hid)
		d.Pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, project)
		d.Pool.Exec(ctx, `DELETE FROM users WHERE id IN ($1, $2)`, owner, uid)
	})

	runErr := cmdPersist(ctx, d, []string{"persist", "--hackathon", hid.String(), "--pool", "contributor"})
	if runErr == nil {
		t.Fatal("payout persist recorded a settlement for an event being paid on KeeperHub")
	}
	var rx *settlement.RailExclusionError
	if !errors.As(runErr, &rx) {
		t.Fatalf("payout persist returned %T: %v - want the typed one-rail refusal", runErr, runErr)
	}

	// main prints exactly runErr; this is that line.
	printed := runErr.Error()
	t.Logf("operator sees: %s", printed)
	for _, raw := range []string{"SQLSTATE", "ERROR:", "KH001", "insert settlement", "keeperhub_payout_runs"} {
		if strings.Contains(printed, raw) {
			t.Errorf("the operator's message leaks %q: %s", raw, printed)
		}
	}
	if !strings.HasPrefix(printed, "refused: hackathon "+hid.String()) {
		t.Errorf("message does not lead with the refusal and the event: %s", printed)
	}
}
