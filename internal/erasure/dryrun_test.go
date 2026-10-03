package erasure

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// retentionTables is every table a retention pass touches.
func retentionTables() []string {
	out := []string{"kyc_reset_audit"}
	for _, s := range payoutRetentionSteps {
		out = append(out, s.name)
	}
	return out
}

// rowsByID is each row of a table as text, by id.
func rowsByID(t *testing.T, d *db.DB, table string) map[string]string {
	t.Helper()
	rows, err := d.Pool.Query(context.Background(), `SELECT id::text, t::text FROM `+table+` t`)
	if err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, row string
		if err := rows.Scan(&id, &row); err != nil {
			t.Fatal(err)
		}
		out[id] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func seedDryRunWorld(t *testing.T, d *db.DB) string {
	t.Helper()
	seedPayoutWorld(t, d)
	p := seedPerson(t, d)
	day := 24 * time.Hour
	suffix := "-dry-" + p.id.String()[:8]
	resetRecord(t, d, p.id, "sess-old"+suffix, ResetRecordRetention+day)
	resetRecord(t, d, p.id, "", ResetRecordRetention+2*day)
	resetRecord(t, d, p.id, "sess-recent"+suffix, ResetRecordRetention-day)
	exec(t, d, `UPDATE users SET kyc_session_id = $2 WHERE id = $1`, p.id, "sess-current"+suffix)
	resetRecord(t, d, p.id, "sess-current"+suffix, ResetRecordRetention+3*day)
	return suffix
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The dry run selects exactly what RunOnce then erases: per table, the rows
// the dry run named are the rows the pass deleted (or updated), no more and no
// fewer, and the Didit sessions it listed are the ones the pass asked Didit to
// delete.
func TestRetentionDryRun_SelectsExactlyWhatRunOnceErases(t *testing.T) {
	d := dbtest.DB(t)
	suffix := seedDryRunWorld(t, d)
	ctx := context.Background()
	ext := &fakeExternal{}
	r := NewRetention(d.Pool, ext, time.Hour)

	rep, err := r.DryRun(ctx)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	before := map[string]map[string]string{}
	for _, table := range retentionTables() {
		before[table] = rowsByID(t, d, table)
	}
	n, err := r.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	var wantSessions []string
	seen := map[string]bool{}
	for _, c := range rep.Categories {
		if c.NotApplicable != "" {
			t.Fatalf("%s: %s", c.Name, c.NotApplicable)
		}
		for _, r := range c.Resets {
			if r.DiditSession != "" {
				wantSessions = append(wantSessions, r.DiditSession)
			}
		}
		for _, tb := range c.Tables {
			seen[tb.Table] = true
			after := rowsByID(t, d, tb.Table)
			acted := map[string]bool{}
			for id, row := range before[tb.Table] {
				a, still := after[id]
				if (tb.Action == "delete" && !still) || (tb.Action == "update" && still && a != row) {
					acted[id] = true
				}
			}
			planned := map[string]bool{}
			for _, id := range tb.ids {
				planned[id] = true
			}
			if got, want := sortedSet(acted), sortedSet(planned); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("%s: the pass %sd %v, the dry run said %v", tb.Table, tb.Action, got, want)
			}
			if tb.Rows != len(tb.ids) || int64(tb.Rows) != n[tb.Table] {
				t.Errorf("%s: dry run reports %d rows, RunOnce %d", tb.Table, tb.Rows, n[tb.Table])
			}
		}
	}
	for _, table := range retentionTables() {
		if !seen[table] {
			t.Errorf("the dry run does not report %s, which the pass touches", table)
		}
	}
	// Not vacuous: the seeded world has something due in each kind of step.
	for _, c := range rep.Categories {
		for _, tb := range c.Tables {
			switch tb.Table {
			case "kyc_reset_audit", "sponsored_claims", "keeperhub_payout_legs", "settlement_lines",
				"hackathon_verdicts", "hackathon_appeals", "hackathon_model_calls", "hackathon_draws", "hackathon_maintainer_payouts":
				if tb.Rows == 0 {
					t.Errorf("%s: nothing selected; the test proves nothing about it", tb.Table)
				}
			}
		}
	}

	sort.Strings(wantSessions)
	got := append([]string(nil), ext.diditDel...)
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(wantSessions, ",") {
		t.Errorf("Didit was asked to delete %v; the dry run listed %v", got, wantSessions)
	}
	var mine []string
	for _, s := range wantSessions {
		if strings.HasSuffix(s, suffix) {
			mine = append(mine, s)
		}
	}
	if strings.Join(mine, ",") != "sess-old"+suffix {
		t.Errorf("dry run Didit sessions of this test = %v, want only the old record's", mine)
	}
}

// fingerprint is a table's row count and a hash of every row.
func fingerprint(t *testing.T, d *db.DB, table string) string {
	t.Helper()
	var n int
	var h string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT count(*), COALESCE(md5(string_agg(t::text, ',' ORDER BY t::text)), '') FROM `+table+` t`).Scan(&n, &h); err != nil {
		t.Fatalf("fingerprint %s: %v", table, err)
	}
	return fmt.Sprintf("%d rows, %s", n, h)
}

// The dry run writes nothing and calls nobody: every table the pass touches,
// and the ones it reads, are row for row the same afterwards, and Didit,
// GitHub and the bounty agent were not called.
func TestRetentionDryRun_WritesNothing(t *testing.T) {
	d := dbtest.DB(t)
	seedDryRunWorld(t, d)
	ctx := context.Background()
	tables := append(retentionTables(), "users", "hackathons", "settlements", "keeperhub_payout_runs")
	before := map[string]string{}
	for _, table := range tables {
		before[table] = fingerprint(t, d, table)
	}

	ext := &fakeExternal{}
	r := NewRetention(d.Pool, ext, time.Hour)
	var due int
	for range 2 {
		rep, err := r.DryRun(ctx)
		if err != nil {
			t.Fatalf("DryRun: %v", err)
		}
		due = 0
		for _, c := range rep.Categories {
			for _, tb := range c.Tables {
				due += tb.Rows
			}
		}
	}
	if due == 0 {
		t.Fatal("the dry run found nothing due; the test proves nothing")
	}
	for _, table := range tables {
		if after := fingerprint(t, d, table); after != before[table] {
			t.Errorf("%s changed during the dry run", table)
		}
	}
	if len(ext.diditDel)+len(ext.revoked)+len(ext.agentErase) != 0 {
		t.Errorf("the dry run called out: didit %v, github %d, agent %v", ext.diditDel, len(ext.revoked), ext.agentErase)
	}
}

// The dry run's transaction is one the database refuses to write in, so a
// mistake in its code cannot become a write.
func TestRetentionDryRun_TransactionIsReadOnly(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	tx, err := beginReadOnly(ctx, d.Pool)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	_, err = tx.Exec(ctx, `DELETE FROM kyc_reset_audit WHERE created_at < now() - interval '90 days'`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "25006" { // read_only_sql_transaction
		t.Fatalf("a delete in the dry run's transaction: err = %v, want read_only_sql_transaction", err)
	}
}

// Against a database from before this release - no users.erased_at, or no
// table at all - the category that needs it is reported as not applicable,
// and the others are still reported.
func TestRetentionDryRun_BeforeThisRelease(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	tx, err := d.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	r := NewRetention(d.Pool, &fakeExternal{}, time.Hour)

	if _, err := tx.Exec(ctx, `ALTER TABLE users RENAME COLUMN erased_at TO erased_at_not_yet`); err != nil {
		t.Fatal(err)
	}
	rep, err := r.dryRunIn(ctx, tx)
	if err != nil {
		t.Fatalf("dry run without users.erased_at: %v", err)
	}
	if c := rep.Categories[0]; c.NotApplicable != "" || len(c.Tables) != 1 {
		t.Errorf("reset records need no erased_at, but: %+v", c)
	}
	if c := rep.Categories[1]; !strings.HasPrefix(c.NotApplicable, "not applicable before this release") ||
		!strings.Contains(c.NotApplicable, "erased_at") || len(c.Tables) != 0 {
		t.Errorf("payout records without erased_at: not_applicable = %q, tables = %v", c.NotApplicable, c.Tables)
	}

	if _, err := tx.Exec(ctx, `ALTER TABLE kyc_reset_audit RENAME TO kyc_reset_audit_not_yet`); err != nil {
		t.Fatal(err)
	}
	rep, err = r.dryRunIn(ctx, tx)
	if err != nil {
		t.Fatalf("dry run without kyc_reset_audit: %v", err)
	}
	if c := rep.Categories[0]; !strings.HasPrefix(c.NotApplicable, "not applicable before this release") {
		t.Errorf("reset records without their table: not_applicable = %q", c.NotApplicable)
	}
}

// Without Didit configured, the pass keeps every record whose session it
// would have to delete, and the dry run says so rather than counting them.
func TestRetentionDryRun_DiditNotConfigured(t *testing.T) {
	d := dbtest.DB(t)
	seedDryRunWorld(t, d)
	ctx := context.Background()
	r := NewRetention(d.Pool, &Services{}, time.Hour)

	rep, err := r.DryRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.DiditConfigured {
		t.Error("Services without a Didit client reported as configured")
	}
	c := rep.Categories[0]
	kept := map[string]bool{}
	for _, rr := range c.Resets {
		if rr.DiditSession != "" {
			t.Errorf("listed session %s for Didit, which is not configured", rr.DiditSession)
		}
		if rr.Outcome == ResetOutcomeNotConfigured {
			kept[rr.AuditID] = true
		}
	}
	if len(kept) == 0 {
		t.Fatal("no record reported as kept for want of Didit")
	}
	before := rowsByID(t, d, "kyc_reset_audit")
	n, _ := r.RunOnce(ctx)
	after := rowsByID(t, d, "kyc_reset_audit")
	for id := range kept {
		if _, ok := after[id]; !ok {
			t.Errorf("record %s erased although the dry run said it is kept", id)
		}
	}
	gone := 0
	for id := range before {
		if _, ok := after[id]; !ok {
			gone++
		}
	}
	if int64(gone) != n["kyc_reset_audit"] || gone != c.Tables[0].Rows {
		t.Errorf("erased %d reset records (RunOnce says %d); the dry run said %d", gone, n["kyc_reset_audit"], c.Tables[0].Rows)
	}
}
