package erasure

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// resetRecord inserts a kyc_reset_audit row made `age` ago and returns its id.
func resetRecord(t *testing.T, d *db.DB, subject uuid.UUID, session string, age time.Duration) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	var s any
	if session != "" {
		s = session
	}
	if err := d.Pool.QueryRow(context.Background(), `
INSERT INTO kyc_reset_audit (subject_user_id, previous_status, previous_session_id, previous_kyc_data, reason, reason_code, created_at)
VALUES ($1, 'rejected', $2, '{"first_name":"Ada"}', 'blurry', 'document_unreadable', now() - make_interval(secs => $3))
RETURNING id`, subject, s, age.Seconds()).Scan(&id); err != nil {
		t.Fatalf("reset record: %v", err)
	}
	return id
}

func resetRecordExists(t *testing.T, d *db.DB, id uuid.UUID) bool {
	t.Helper()
	return count(t, d, `SELECT count(*) FROM kyc_reset_audit WHERE id = $1`, id) == 1
}

// A reset record is erased 90 days after the reset, for an account that still
// exists as much as for an erased one; its Didit session is deleted with it.
// A recent record stays, and a second pass changes nothing.
func TestRetention_ResetRecordsGoAfterNinetyDays(t *testing.T) {
	d := dbtest.DB(t)
	p := seedPerson(t, d) // an ordinary, live account
	ctx := context.Background()
	day := 24 * time.Hour
	suffix := uuid.NewString()[:8]

	oldWithSession := resetRecord(t, d, p.id, "sess-old-"+suffix, ResetRecordRetention+day)
	oldWithout := resetRecord(t, d, p.id, "", ResetRecordRetention+day)
	recent := resetRecord(t, d, p.id, "sess-recent-"+suffix, ResetRecordRetention-day)
	// Still the person's current session: the old row goes, the session at
	// Didit is left alone.
	exec(t, d, `UPDATE users SET kyc_session_id = $2 WHERE id = $1`, p.id, "sess-current2-"+suffix)
	oldCurrent := resetRecord(t, d, p.id, "sess-current2-"+suffix, ResetRecordRetention+day)

	ext := &fakeExternal{}
	r := NewRetention(d.Pool, ext, time.Hour)
	n, err := r.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n["kyc_reset_audit"] < 3 {
		t.Errorf("erased %d reset records, want at least the 3 old ones", n["kyc_reset_audit"])
	}
	for name, id := range map[string]uuid.UUID{"old with session": oldWithSession, "old without": oldWithout, "old, current session": oldCurrent} {
		if resetRecordExists(t, d, id) {
			t.Errorf("%s: a reset record older than 90 days survived", name)
		}
	}
	if !resetRecordExists(t, d, recent) {
		t.Error("a reset record younger than 90 days was erased")
	}
	var mine []string
	for _, s := range ext.diditDel {
		if strings.HasSuffix(s, suffix) {
			mine = append(mine, s)
		}
	}
	sort.Strings(mine)
	if strings.Join(mine, ",") != "sess-old-"+suffix {
		t.Errorf("Didit deletions = %v, want only the old record's session (not the recent one, not the current one)", mine)
	}

	// Idempotent: nothing more to do, and Didit is not asked again.
	before := len(ext.diditDel)
	if _, err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if !resetRecordExists(t, d, recent) {
		t.Error("second pass erased the recent record")
	}
	for _, s := range ext.diditDel[before:] {
		if strings.HasSuffix(s, suffix) {
			t.Errorf("second pass asked Didit again for %s", s)
		}
	}
}

// Until Didit has deleted the session, the record naming it stays: erasing it
// first would leave the session at Didit with nothing left that can find it.
func TestRetention_ResetRecordWaitsForDidit(t *testing.T) {
	d := dbtest.DB(t)
	p := seedPerson(t, d)
	ctx := context.Background()
	id := resetRecord(t, d, p.id, "sess-flaky-"+uuid.NewString()[:8], ResetRecordRetention+time.Hour)

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"Didit unreachable", errors.New("didit: status 503")},
		{"Didit not configured", ErrNotConfigured},
	} {
		_, _ = NewRetention(d.Pool, &fakeExternal{diditErr: tc.err}, time.Hour).RunOnce(ctx)
		if !resetRecordExists(t, d, id) {
			t.Fatalf("%s: the record was erased although its Didit session was not", tc.name)
		}
	}
	if _, err := NewRetention(d.Pool, &fakeExternal{}, time.Hour).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if resetRecordExists(t, d, id) {
		t.Error("the record stayed after Didit deleted the session")
	}
}

// payoutWorld is one erased account (tomb) and one live account (live) with
// payout records old and recent, finished and open, on both rails and in
// GrainHack. Every id an assertion needs is named.
type payoutWorld struct {
	tomb, live                                   uuid.UUID
	sOld, sRecent, sOpen                         uuid.UUID
	hOld, hFailed, hOpen                         uuid.UUID
	project                                      uuid.UUID
	tombLegOld, tombLegFailed, tombLegPending    uuid.UUID
	liveLegOld                                   uuid.UUID
	tombVerdictOld, tombVerdictOpen, liveVerdict uuid.UUID
	tombAssignOld, liveAssign                    uuid.UUID
	draw                                         uuid.UUID
}

func seedPayoutWorld(t *testing.T, d *db.DB) payoutWorld {
	t.Helper()
	ctx := context.Background()
	w := payoutWorld{tomb: uuid.New(), live: uuid.New()}
	old := time.Now().AddDate(-PayoutRecordYears, 0, -2)
	recent := time.Now().AddDate(-PayoutRecordYears+1, 0, 0)
	id := func(sql string, args ...any) uuid.UUID {
		t.Helper()
		var out uuid.UUID
		if err := d.Pool.QueryRow(ctx, sql, args...).Scan(&out); err != nil {
			t.Fatalf("%s: %v", strings.TrimSpace(strings.SplitN(sql, "(", 2)[0]), err)
		}
		return out
	}
	leaf := func() []byte { b := make([]byte, 32); copy(b, uuid.New().String()); return b }
	addr := func() string { return "0x" + strings.ReplaceAll(uuid.NewString(), "-", "") + "abcdef12" }

	exec(t, d, `INSERT INTO users (id, role, erased_at) VALUES ($1, 'contributor', now() - interval '1 day'), ($2, 'contributor', NULL)`, w.tomb, w.live)
	t.Cleanup(func() { cleanupPayoutWorld(d, w) })

	// Settlements: released long ago, released recently, not released.
	w.sOld = id(`INSERT INTO settlements (pool_usdc, total_weight, unit_value_usdc, released_at, pool_minor) VALUES (100, 1, 100, $1, 100000000) RETURNING id`, old)
	w.sRecent = id(`INSERT INTO settlements (pool_usdc, total_weight, unit_value_usdc, released_at) VALUES (100, 1, 100, $1) RETURNING id`, recent)
	w.sOpen = id(`INSERT INTO settlements (pool_usdc, total_weight, unit_value_usdc) VALUES (100, 1, 100) RETURNING id`)
	for _, l := range []struct {
		s, u uuid.UUID
	}{{w.sOld, w.tomb}, {w.sRecent, w.tomb}, {w.sOpen, w.tomb}, {w.sOld, w.live}} {
		exec(t, d, `INSERT INTO settlement_lines (settlement_id, user_id, raw_weight, multiplier, effective_weight, usdc_amount, amount_minor) VALUES ($1,$2,1,1,1,50,50000000)`, l.s, l.u)
	}
	exec(t, d, `INSERT INTO settlement_holds (user_id, origin_settlement_id, amount_minor, reason, released_in_settlement_id, released_at) VALUES ($1,$2,5,'no_address',$2,$3)`, w.tomb, w.sOld, old)
	exec(t, d, `INSERT INTO settlement_holds (user_id, origin_settlement_id, amount_minor, reason) VALUES ($1,$2,5,'no_address')`, w.tomb, w.sRecent)
	exec(t, d, `INSERT INTO sponsored_claims (user_id, settlement_id, leaf_hash, outcome, tx_hash, created_at) VALUES ($1,$2,$3,'submitted','0xtx-old',$4)`, w.tomb, w.sOld, leaf(), old)
	exec(t, d, `INSERT INTO sponsored_claims (user_id, settlement_id, leaf_hash, outcome, tx_hash, created_at) VALUES ($1,$2,$3,'submitted','0xtx-recent',$4)`, w.tomb, w.sRecent, leaf(), recent)
	exec(t, d, `INSERT INTO sponsored_claims (user_id, settlement_id, leaf_hash, outcome, tx_hash, created_at) VALUES ($1,$2,$3,'submitted','0xtx-live',$4)`, w.live, w.sOld, leaf(), old)
	exec(t, d, `INSERT INTO redemptions (user_id, points_spent, usdc_amount, stellar_wallet_address, status, reviewed_at, created_at) VALUES ($1, 10, 1, 'GSTELLAR', 'approved', $2, $2)`, w.tomb, old)

	// GrainHack: an event paid long ago, one whose only leg failed with
	// nobody resolving it, and one with a payout still pending.
	w.project = id(`INSERT INTO projects (owner_user_id, github_full_name) VALUES ($1, $2) RETURNING id`, w.live, "retention-org/"+uuid.NewString()[:8])
	event := func(name string) (uuid.UUID, uuid.UUID) {
		h := id(`INSERT INTO hackathons (name, ends_at) VALUES ($1, $2) RETURNING id`, name, old.AddDate(0, -1, 0))
		hpr := id(`INSERT INTO hackathon_payout_runs (hackathon_id, contributor_prize_pool, total_units, unit_value) VALUES ($1, 100, 1, 100) RETURNING id`, h)
		kr := id(`INSERT INTO keeperhub_payout_runs (hackathon_id, pool, chain_id, pool_minor, hackathon_payout_run_id, evm_chain_id) VALUES ($1,'contributor','base',100000000,$2,8453) RETURNING id`, h, hpr)
		return h, kr
	}
	var krOld, krFailed, krOpen uuid.UUID
	w.hOld, krOld = event("retention old")
	w.hFailed, krFailed = event("retention failed")
	w.hOpen, krOpen = event("retention open")
	w.tombLegOld = id(`INSERT INTO keeperhub_payout_legs (run_id, user_id, address, amount_minor, status, tx_hash, confirmed_at, updated_at) VALUES ($1,$2,$3,50,'confirmed','0xleg-old',$4,$4) RETURNING id`, krOld, w.tomb, addr(), old)
	w.liveLegOld = id(`INSERT INTO keeperhub_payout_legs (run_id, user_id, address, amount_minor, status, tx_hash, confirmed_at, updated_at) VALUES ($1,$2,$3,50,'confirmed','0xleg-live',$4,$4) RETURNING id`, krOld, w.live, addr(), old)
	w.tombLegFailed = id(`INSERT INTO keeperhub_payout_legs (run_id, user_id, address, amount_minor, status, updated_at) VALUES ($1,$2,$3,50,'failed',$4) RETURNING id`, krFailed, w.tomb, addr(), old)
	w.tombLegPending = id(`INSERT INTO keeperhub_payout_legs (run_id, user_id, address, amount_minor, status, updated_at) VALUES ($1,$2,$3,50,'pending',$4) RETURNING id`, krOpen, w.tomb, addr(), old)
	exec(t, d, `INSERT INTO keeperhub_payout_exclusions (run_id, user_id, amount_minor, reason, created_at) VALUES ($1,$2,5,'no_address',$3)`, krOld, w.tomb, old)

	issue := func(h uuid.UUID, n int) uuid.UUID {
		return id(`INSERT INTO hackathon_issues (hackathon_id, project_id, issue_number, org_login) VALUES ($1,$2,$3,'retention-org') RETURNING id`, h, w.project, n)
	}
	i1, i2, i3 := issue(w.hOld, 1), issue(w.hOld, 2), issue(w.hOpen, 1)
	assign := func(h, i, u uuid.UUID, n int) uuid.UUID {
		return id(`INSERT INTO hackathon_assignments (hackathon_id, hackathon_issue_id, project_id, issue_number, user_id, github_login, org_login, status)
		  VALUES ($1,$2,$3,$4,$5,$6,'retention-org','completed') RETURNING id`, h, i, w.project, n, u, ErasedPlaceholder)
	}
	w.tombAssignOld = assign(w.hOld, i1, w.tomb, 1)
	w.liveAssign = assign(w.hOld, i2, w.live, 2)
	verdict := func(h, a, u uuid.UUID, pr int) uuid.UUID {
		return id(`INSERT INTO hackathon_verdicts (hackathon_id, assignment_id, project_id, pr_number, user_id, github_login, payout_amount)
		  VALUES ($1,$2,$3,$4,$5,$6,50) RETURNING id`, h, a, w.project, pr, u, ErasedPlaceholder)
	}
	w.tombVerdictOld = verdict(w.hOld, w.tombAssignOld, w.tomb, 1)
	w.liveVerdict = verdict(w.hOld, w.liveAssign, w.live, 2)
	w.tombVerdictOpen = verdict(w.hOpen, assign(w.hOpen, i3, w.tomb, 1), w.tomb, 1)
	exec(t, d, `INSERT INTO hackathon_appeals (hackathon_id, verdict_id, user_id, github_login, reason) VALUES ($1,$2,$3,$4,'[erased]')`, w.hOld, w.tombVerdictOld, w.tomb, ErasedPlaceholder)
	exec(t, d, `INSERT INTO hackathon_model_calls (hackathon_id, verdict_id, stage, provider, model, request) VALUES ($1,$2,'judge','p','m','{"pr":"the diff"}')`, w.hOld, w.tombVerdictOld)
	w.draw = id(`INSERT INTO hackathon_draws (hackathon_id, hackathon_issue_id, seed, pool, pool_size, winner_user_id)
	  VALUES ($1,$2,7,$3::jsonb,2,$4) RETURNING id`, w.hOld, i1,
		`[{"user_id":"`+w.tomb.String()+`","github_login":"`+ErasedPlaceholder+`","tickets":2},{"user_id":"`+w.live.String()+`","github_login":"someone","tickets":1}]`, w.tomb)
	exec(t, d, `INSERT INTO hackathon_maintainer_payouts (hackathon_id, project_id, org_login, maintainer_user_id, holdback_status, holdback_resolved_at, created_at)
	  VALUES ($1,$2,'retention-org',$3,'released',$4,$4)`, w.hOld, w.project, w.tomb, old)
	return w
}

func cleanupPayoutWorld(d *db.DB, w payoutWorld) {
	ctx := context.Background()
	for _, q := range []string{
		`DELETE FROM hackathons WHERE id IN ($3, $4, $5)`,
		`DELETE FROM sponsored_claims WHERE user_id IN ($1, $2)`,
		`DELETE FROM settlement_holds WHERE user_id IN ($1, $2)`,
		`DELETE FROM redemptions WHERE user_id IN ($1, $2)`,
		`DELETE FROM settlements WHERE id IN ($6, $7, $8)`,
		`DELETE FROM projects WHERE id = $9`,
		`DELETE FROM users WHERE id IN ($1, $2)`,
	} {
		_, _ = d.Pool.Exec(ctx, q, w.tomb, w.live, w.hOld, w.hFailed, w.hOpen, w.sOld, w.sRecent, w.sOpen, w.project)
	}
}

// An erased account's payout records are erased five years after the
// payment: the rows that tie an amount, an address or a transaction to the
// account go, the round's totals stay. Nothing still open, nothing recent and
// nothing of a live account is touched, and a second pass changes nothing.
func TestRetention_PayoutRecordsOfErasedAccountsGoAfterFiveYears(t *testing.T) {
	d := dbtest.DB(t)
	w := seedPayoutWorld(t, d)
	ctx := context.Background()
	r := NewRetention(d.Pool, &fakeExternal{}, time.Hour)

	check := func(pass int) {
		t.Helper()
		gone := map[string]string{
			"settlement line, released 5+ years ago": `SELECT count(*) FROM settlement_lines WHERE user_id = $1 AND settlement_id = '` + w.sOld.String() + `'`,
			"released hold":                          `SELECT count(*) FROM settlement_holds WHERE user_id = $1 AND released_at IS NOT NULL`,
			"old sponsored claim (tx id)":            `SELECT count(*) FROM sponsored_claims WHERE user_id = $1 AND tx_hash = '0xtx-old'`,
			"confirmed leg (address, tx id)":         `SELECT count(*) FROM keeperhub_payout_legs WHERE id = '` + w.tombLegOld.String() + `' AND user_id = $1`,
			"exclusion":                              `SELECT count(*) FROM keeperhub_payout_exclusions WHERE user_id = $1`,
			"decided redemption (address)":           `SELECT count(*) FROM redemptions WHERE user_id = $1`,
			"GrainHack verdict":                      `SELECT count(*) FROM hackathon_verdicts WHERE id = '` + w.tombVerdictOld.String() + `' AND user_id = $1`,
			"GrainHack appeal":                       `SELECT count(*) FROM hackathon_appeals WHERE user_id = $1`,
			"GrainHack assignment":                   `SELECT count(*) FROM hackathon_assignments WHERE id = '` + w.tombAssignOld.String() + `' AND user_id = $1`,
			"verdict's model calls": `SELECT count(*) FROM hackathon_model_calls m WHERE m.hackathon_id = '` + w.hOld.String() + `'
			   AND NOT EXISTS (SELECT 1 FROM hackathon_verdicts v WHERE v.id = m.verdict_id AND v.user_id <> $1)`,
			"maintainer payout recipient": `SELECT count(*) FROM hackathon_maintainer_payouts WHERE maintainer_user_id = $1`,
			"draw winner":                 `SELECT count(*) FROM hackathon_draws WHERE winner_user_id = $1`,
			"draw entry":                  `SELECT count(*) FROM hackathon_draws WHERE pool::text LIKE '%' || $1::text || '%'`,
		}
		for what, q := range gone {
			if n := count(t, d, q, w.tomb); n != 0 {
				t.Errorf("pass %d: %s of the erased account still there (%d)", pass, what, n)
			}
		}
		kept := map[string]string{
			"settlement line, released 4 years ago":    `SELECT count(*) FROM settlement_lines WHERE user_id = $1 AND settlement_id = '` + w.sRecent.String() + `'`,
			"settlement line, not released":            `SELECT count(*) FROM settlement_lines WHERE user_id = $1 AND settlement_id = '` + w.sOpen.String() + `'`,
			"unreleased hold (money owed)":             `SELECT count(*) FROM settlement_holds WHERE user_id = $1 AND released_at IS NULL`,
			"recent sponsored claim":                   `SELECT count(*) FROM sponsored_claims WHERE user_id = $1 AND tx_hash = '0xtx-recent'`,
			"failed leg nobody resolved (may be owed)": `SELECT count(*) FROM keeperhub_payout_legs WHERE id = '` + w.tombLegFailed.String() + `' AND user_id = $1`,
			"pending leg":                      `SELECT count(*) FROM keeperhub_payout_legs WHERE id = '` + w.tombLegPending.String() + `' AND user_id = $1`,
			"verdict in an event still paying": `SELECT count(*) FROM hackathon_verdicts WHERE id = '` + w.tombVerdictOpen.String() + `' AND user_id = $1`,
		}
		for what, q := range kept {
			if n := count(t, d, q, w.tomb); n != 1 {
				t.Errorf("pass %d: %s was erased (%d left)", pass, what, n)
			}
		}
		live := map[string]string{
			"line":       `SELECT count(*) FROM settlement_lines WHERE user_id = $1`,
			"claim":      `SELECT count(*) FROM sponsored_claims WHERE user_id = $1`,
			"leg":        `SELECT count(*) FROM keeperhub_payout_legs WHERE id = '` + w.liveLegOld.String() + `' AND user_id = $1`,
			"verdict":    `SELECT count(*) FROM hackathon_verdicts WHERE id = '` + w.liveVerdict.String() + `' AND user_id = $1`,
			"assignment": `SELECT count(*) FROM hackathon_assignments WHERE id = '` + w.liveAssign.String() + `' AND user_id = $1`,
			"draw entry": `SELECT count(*) FROM hackathon_draws WHERE pool::text LIKE '%' || $1::text || '%'`,
		}
		for what, q := range live {
			if n := count(t, d, q, w.live); n != 1 {
				t.Errorf("pass %d: the live account's old %s was touched (%d)", pass, what, n)
			}
		}
		// The totals of each round stay.
		if n := count(t, d, `SELECT count(*) FROM settlements WHERE id = $1 AND pool_minor = 100000000`, w.sOld); n != 1 {
			t.Errorf("pass %d: the settlement total went", pass)
		}
		if n := count(t, d, `SELECT count(*) FROM keeperhub_payout_runs WHERE hackathon_id = $1 AND pool_minor = 100000000`, w.hOld); n != 1 {
			t.Errorf("pass %d: the KeeperHub run total went", pass)
		}
		if n := count(t, d, `SELECT count(*) FROM hackathon_maintainer_payouts WHERE hackathon_id = $1 AND maintainer_user_id IS NULL`, w.hOld); n != 1 {
			t.Errorf("pass %d: the maintainer payout itself went", pass)
		}
		var reason *string
		var pool string
		if err := d.Pool.QueryRow(ctx, `SELECT no_winner_reason, pool::text FROM hackathon_draws WHERE id = $1`, w.draw).Scan(&reason, &pool); err != nil {
			t.Fatalf("pass %d: the draw went: %v", pass, err)
		}
		if reason == nil || !strings.Contains(*reason, "deleted their account") {
			t.Errorf("pass %d: a draw whose winner was erased must say why it names no winner: %v", pass, reason)
		}
		if !strings.Contains(pool, "00000000-0000-0000-0000-000000000000") {
			t.Errorf("pass %d: the erased entry should carry the nil id: %s", pass, pool)
		}
	}

	if _, err := r.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	check(1)
	if _, err := r.RunOnce(ctx); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	check(2)
}

// Every payout record erasure keeps, and every GrainHack record it rewrites,
// has a retention step - so "kept for five years, then erased" stays true of a
// table added later.
func TestRetention_EveryKeptPayoutRecordHasAPeriod(t *testing.T) {
	steps := map[string]bool{}
	for _, s := range payoutRetentionSteps {
		steps[s.name] = true
	}
	var missing []string
	for table, why := range keptTables {
		if strings.HasPrefix(why, "payout record") && !steps[table] {
			missing = append(missing, table)
		}
	}
	for _, s := range eraseSteps {
		if strings.HasPrefix(s.name, "hackathon_") && strings.Contains(s.sql, ErasedPlaceholder) && !steps[s.name] {
			missing = append(missing, s.name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("kept payout records with no retention step: %v\nadd one to payoutRetentionSteps (internal/erasure/retention.go)", missing)
	}
}
