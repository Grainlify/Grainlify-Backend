package erasure

import (
	"context"
	"crypto/ed25519"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
	"github.com/jagadeesh/grainlify/backend/internal/grainhack"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

// GrainHack results statements meet erasure and retention: a statement line
// is a payout record, kept when the account is erased (its login is inside a
// signed document) and erased five years after the payment, through the one
// narrow way past the statements' immutability trigger (GH010).

const (
	ghTestRecipient = "DQNUbSmmakWcVKcdRXraNSgWdabZs5dMJyTVPFv21fNL"
	ghTxBase        = "VERv8NMvzbJMEkV8xnrLkEaWRtSz9CosKDYjCJjBRnbJLgp8uirBgmQpjKhoR4tjF3ZpRzrFmBV6UjKdiSZkQUW"
)

type ghWinner struct {
	userID uuid.UUID
	ghID   int64
	login  string
}

type ghWorld struct {
	t      *testing.T
	d      *db.DB
	svc    *grainhack.Service
	hid    uuid.UUID
	admin  uuid.UUID
	people []ghWinner // 1, 2, 3 and 4 USDC; the third is not verified
}

func resetGrainHackTables(t *testing.T, d *db.DB) {
	t.Helper()
	if _, err := d.Pool.Exec(context.Background(), `
		TRUNCATE grainhack_notices, grainhack_payment_reports, grainhack_results_statement_lines,
		         grainhack_results_statements, grainhack_broadcast_notices, grainhack_results_retention_log`); err != nil {
		t.Fatalf("reset grainhack: %v", err)
	}
}

// seedGrainHack is a settled event with a 10 USDC contributor pool and four
// winners earning 1, 2, 3 and 4 USDC; the third has not verified their
// identity, so their line is held.
func seedGrainHack(t *testing.T) *ghWorld {
	t.Helper()
	d := dbtest.DB(t)
	resetGrainHackTables(t, d)
	ctx := context.Background()
	w := &ghWorld{t: t, d: d, admin: uuid.New()}
	var project, users = uuid.Nil, []uuid.UUID{w.admin}
	exec(t, d, `INSERT INTO users (id, role) VALUES ($1, 'admin')`, w.admin)
	if err := d.Pool.QueryRow(ctx, `INSERT INTO projects (owner_user_id, github_full_name) VALUES ($1,$2) RETURNING id`,
		w.admin, "erasure-gh/"+uuid.NewString()[:8]).Scan(&project); err != nil {
		t.Fatalf("project: %v", err)
	}
	if err := d.Pool.QueryRow(ctx, `
		INSERT INTO hackathons (name, contributor_prize_pool, phase, appeals_closed_at, ends_at)
		VALUES ($1, 10, 'settled', now(), now() - interval '7 days') RETURNING id`,
		"Erasure GrainHack "+uuid.NewString()[:8]).Scan(&w.hid); err != nil {
		t.Fatalf("hackathon: %v", err)
	}
	exec(t, d, `INSERT INTO hackathon_config_settings (hackathon_id, key, value) VALUES ($1, 'judging_shadow_mode', 'false')`, w.hid)
	exec(t, d, `INSERT INTO hackathon_payout_runs (hackathon_id, contributor_prize_pool, total_units, unit_value) VALUES ($1, 10, 10, 1)`, w.hid)
	for i, units := range []int{1, 2, 3, 4} {
		kyc := "verified"
		if i == 2 {
			kyc = "pending"
		}
		p := ghWinner{userID: uuid.New(), ghID: ghIDSeq.Add(1), login: "gh-erasure-" + uuid.NewString()[:8]}
		exec(t, d, `INSERT INTO users (id, role, kyc_status, github_user_id) VALUES ($1, 'contributor', $2, $3)`, p.userID, kyc, p.ghID)
		exec(t, d, `INSERT INTO github_accounts (user_id, github_user_id, login, access_token) VALUES ($1, $2, $3, '\x00')`, p.userID, p.ghID, p.login)
		exec(t, d, `INSERT INTO hackathon_verdicts (hackathon_id, project_id, pr_number, user_id, github_login, final_bucket, units, curve_multiplier)
		  VALUES ($1, $2, $3, $4, $5, 'accepted', $6, 1)`, w.hid, project, i+1, p.userID, p.login, units)
		w.people = append(w.people, p)
		users = append(users, p.userID)
	}
	w.svc = &grainhack.Service{Pool: d.Pool, Key: ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)),
		Network: grainhack.NetworkSolanaDevnet, Notify: notifications.New(d, nil, ""), Now: time.Now}
	t.Cleanup(func() {
		resetGrainHackTables(t, d)
		for _, q := range []string{
			`DELETE FROM hackathons WHERE id = $1`,
		} {
			_, _ = d.Pool.Exec(ctx, q, w.hid)
		}
		_, _ = d.Pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, project)
		for _, u := range users {
			_, _ = d.Pool.Exec(ctx, `DELETE FROM notifications WHERE user_id = $1`, u)
			_, _ = d.Pool.Exec(ctx, `DELETE FROM github_accounts WHERE user_id = $1`, u)
			_, _ = d.Pool.Exec(ctx, `DELETE FROM account_deletion_requests WHERE user_id = $1`, u)
			_, _ = d.Pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, u)
		}
	})
	return w
}

func (w *ghWorld) issue() *grainhack.Issued {
	w.t.Helper()
	is, err := w.svc.Issue(context.Background(), grainhack.IssueRequest{
		HackathonID: w.hid, Pool: grainhack.PoolContributor, ActorID: w.admin, Confirm: true})
	if err != nil {
		w.t.Fatalf("issue: %v", err)
	}
	return is
}

// pay reports winner i paid under the statement, as the agent would.
func (w *ghWorld) pay(is *grainhack.Issued, i int) {
	w.t.Helper()
	p := w.people[i]
	amount := ""
	for _, l := range is.Lines {
		if l.GitHubUserID == p.ghID {
			amount = l.AmountMinor
		}
	}
	if _, err := w.svc.ReportPayment(context.Background(), grainhack.PaymentReport{
		StatementID: is.StatementID.String(), GitHubUserID: p.ghID, AmountMinor: amount, Currency: "USDC",
		Network: is.Network, TxSignature: strconv.Itoa(i+2) + ghTxBase, Recipient: ghTestRecipient,
	}); err != nil {
		w.t.Fatalf("report payment for winner %d: %v", i, err)
	}
}

// erase runs erasure's own database steps for one winner, as the executor
// does once nothing holds it.
func (w *ghWorld) erase(i int) {
	w.t.Helper()
	if _, err := eraseRows(context.Background(), w.d.Pool, w.people[i].userID); err != nil {
		w.t.Fatalf("erase winner %d: %v", i, err)
	}
}

func (w *ghWorld) verifies(is *grainhack.Issued) bool {
	return grainhack.Verify(w.svc.Key.Public().(ed25519.PublicKey), is.Statement, is.Signature)
}

func lineOf(is *grainhack.Issued, ghID int64) *grainhack.IssuedLine {
	for i := range is.Lines {
		if is.Lines[i].GitHubUserID == ghID {
			return &is.Lines[i]
		}
	}
	return nil
}

func hasReason(reasons []string, part string) bool {
	for _, r := range reasons {
		if strings.Contains(r, part) {
			return true
		}
	}
	return false
}

// A GrainHack line not yet paid - payable or held for KYC - holds the
// erasure like any other money in flight; once the agent reports it paid,
// it does not.
func TestGrainHackStatements_UnpaidLineHoldsTheErasure(t *testing.T) {
	w := seedGrainHack(t)
	ctx := context.Background()
	is := w.issue()
	w.pay(is, 0)
	for i, want := range []bool{false, true, true, true} {
		reasons, err := MoneyInFlight(ctx, w.d.Pool, w.people[i].userID)
		if err != nil {
			t.Fatal(err)
		}
		if got := hasReason(reasons, "GrainHack payout in a results statement"); got != want {
			t.Errorf("winner %d: held by the statement = %v, want %v (%v)", i, got, want, reasons)
		}
	}
}

// Erasure keeps an erased winner's statement lines exactly as issued: the
// statement still verifies, its payment record stays, and a later statement
// (another winner's KYC clearing) still issues, carrying the erased winner's
// line over unchanged. Nobody is notified at the erased account.
func TestGrainHackStatements_ErasureKeepsLinesIssuableAndVerifiable(t *testing.T) {
	w := seedGrainHack(t)
	ctx := context.Background()
	st1 := w.issue()
	w.pay(st1, 0)
	erased := w.people[0]
	before := lineOf(st1, erased.ghID)

	w.erase(0)

	got, err := w.svc.Get(ctx, st1.StatementID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Statement != st1.Statement || got.Signature != st1.Signature || !w.verifies(got) {
		t.Fatal("the issued statement changed or no longer verifies after the erasure")
	}
	if l := lineOf(got, erased.ghID); l == nil || *l != *before {
		t.Fatalf("the erased winner's line changed: %+v, was %+v", l, before)
	}
	if n := count(t, w.d, `SELECT count(*) FROM grainhack_payment_reports WHERE user_id = $1`, erased.userID); n != 1 {
		t.Errorf("the erased winner's payment record went at erasure (%d)", n)
	}
	if n := count(t, w.d, `SELECT count(*) FROM grainhack_notices WHERE user_id = $1`, erased.userID); n != 0 {
		t.Errorf("the erased winner's notice records are kept (%d)", n)
	}
	if n := count(t, w.d, `SELECT count(*) FROM github_accounts WHERE user_id = $1`, erased.userID); n != 0 {
		t.Fatal("erasure left the GitHub account: this test would not exercise the erased path")
	}
	view, err := w.svc.AdminView(ctx, got)
	if err != nil {
		t.Fatal(err)
	}
	// The erased winner's line is payable and their account has no verified
	// identity any more; that must not read as a status to supersede.
	if view.SupersedeAvailable {
		t.Error("an erased account's line must not make a supersede available")
	}

	// The held winner verifies: the superseding statement issues.
	exec(t, w.d, `UPDATE users SET kyc_status = 'verified' WHERE id = $1`, w.people[2].userID)
	st2 := w.issue()
	if st2.Supersedes == nil || *st2.Supersedes != st1.StatementID || !w.verifies(st2) {
		t.Fatalf("superseding statement: supersedes %v, verifies %v", st2.Supersedes, w.verifies(st2))
	}
	if l := lineOf(st2, erased.ghID); l == nil || l.Login != erased.login || l.Status != grainhack.StatusPayable ||
		l.AmountMinor != before.AmountMinor || l.UserID != erased.userID {
		t.Errorf("the erased winner's line was not carried over unchanged: %+v", l)
	}
	if l := lineOf(st2, w.people[2].ghID); l == nil || l.Status != grainhack.StatusPayable {
		t.Errorf("the verified winner's line: %+v", l)
	}
	view, err = w.svc.AdminView(ctx, st2)
	if err != nil {
		t.Fatal(err)
	}
	if view.SupersedeAvailable {
		t.Error("an erased account's line must not make a supersede available")
	}
	if _, err := w.svc.Issue(ctx, grainhack.IssueRequest{HackathonID: w.hid, Pool: grainhack.PoolContributor, ActorID: w.admin, Confirm: true}); !errors.Is(err, grainhack.ErrNothingToSupersede) {
		t.Errorf("issuing again: %v, want nothing_to_supersede", err)
	}
	if n := count(t, w.d, `SELECT count(*) FROM notifications WHERE user_id = $1`, erased.userID); n != 0 {
		t.Errorf("%d notification(s) sent to the erased account", n)
	}
	// A missing-wallet report naming the erased winner sends nothing either.
	res, err := w.svc.RemindLinkWallet(ctx, grainhack.WalletReport{StatementID: st2.StatementID.String(), GitHubUserIDs: []int64{erased.ghID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Notified) != 0 || len(res.Skipped) != 1 || res.Skipped[0].Reason != "account_erased" {
		t.Errorf("wallet reminder to the erased account: %+v", res)
	}
}

// Five years after the event's last payment, the retention pass erases an
// erased winner's statement lines and payment record, and redacts every
// statement that named them: their login and GitHub id leave the stored
// document, the other winners' lines stay, the signature is emptied, and each
// action is logged. The dry run lists exactly those rows first; nothing of a
// live account and nothing recent is touched; a second pass does nothing.
func TestGrainHackStatements_RetentionAfterFiveYears(t *testing.T) {
	w := seedGrainHack(t)
	ctx := context.Background()
	st1 := w.issue()
	for _, i := range []int{0, 1, 3} {
		w.pay(st1, i)
	}
	exec(t, w.d, `UPDATE users SET kyc_status = 'verified' WHERE id = $1`, w.people[2].userID)
	st2 := w.issue()
	w.pay(st2, 2)
	w.erase(0)
	erased := w.people[0]

	r := NewRetention(w.d.Pool, &fakeExternal{}, time.Hour)
	tables := []string{"grainhack_payment_reports", "grainhack_results_statement_lines", "grainhack_results_statements"}

	// Paid this week: nothing is due.
	n, err := r.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	for _, tb := range tables {
		if n[tb] != 0 {
			t.Errorf("recent payment: %s erased %d", tb, n[tb])
		}
	}

	// Six years on.
	r.now = func() time.Time { return time.Now().AddDate(PayoutRecordYears+1, 0, 0) }
	rep, err := r.DryRun(ctx)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	wantLines := map[string]bool{}
	rows, err := w.d.Pool.Query(ctx, `SELECT id::text FROM grainhack_results_statement_lines WHERE user_id = $1`, erased.userID)
	if err != nil {
		t.Fatal(err)
	}
	ids, _ := pgx.CollectRows(rows, pgx.RowTo[string])
	for _, id := range ids {
		wantLines[id] = true
	}
	want := map[string]struct {
		action string
		ids    map[string]bool
	}{
		"grainhack_results_statement_lines": {"delete", wantLines},
		"grainhack_results_statements":      {"update", map[string]bool{st1.StatementID.String(): true, st2.StatementID.String(): true}},
		"grainhack_payment_reports":         {"delete", nil},
	}
	for _, c := range rep.Categories {
		for _, tb := range c.Tables {
			wt, ok := want[tb.Table]
			if !ok {
				continue
			}
			got := map[string]bool{}
			for _, id := range tb.ids {
				got[id] = true
			}
			if tb.Action != wt.action {
				t.Errorf("dry run: %s action %s, want %s", tb.Table, tb.Action, wt.action)
			}
			if wt.ids != nil && strings.Join(sortedSet(got), ",") != strings.Join(sortedSet(wt.ids), ",") {
				t.Errorf("dry run: %s lists %v, want %v", tb.Table, sortedSet(got), sortedSet(wt.ids))
			}
			if tb.Table == "grainhack_payment_reports" && tb.Rows != 1 {
				t.Errorf("dry run: %d payment reports, want the erased winner's one", tb.Rows)
			}
			delete(want, tb.Table)
		}
	}
	if len(want) != 0 {
		t.Errorf("dry run does not report: %v", want)
	}
	if len(wantLines) != 2 {
		t.Fatalf("the erased winner should have a line in both statements, has %d", len(wantLines))
	}

	n, err = r.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce after five years: %v", err)
	}
	if n["grainhack_results_statement_lines"] != 2 || n["grainhack_results_statements"] != 2 || n["grainhack_payment_reports"] != 1 {
		t.Errorf("RunOnce erased %v", n)
	}
	check := func(pass int) {
		t.Helper()
		if c := count(t, w.d, `SELECT count(*) FROM grainhack_results_statement_lines WHERE user_id = $1`, erased.userID); c != 0 {
			t.Errorf("pass %d: %d line(s) of the erased winner left", pass, c)
		}
		if c := count(t, w.d, `SELECT count(*) FROM grainhack_payment_reports WHERE user_id = $1`, erased.userID); c != 0 {
			t.Errorf("pass %d: the erased winner's payment record is left", pass)
		}
		for _, p := range w.people[1:] {
			if c := count(t, w.d, `SELECT count(*) FROM grainhack_results_statement_lines WHERE user_id = $1`, p.userID); c != 2 {
				t.Errorf("pass %d: a live winner's lines were touched (%d left)", pass, c)
			}
			if c := count(t, w.d, `SELECT count(*) FROM grainhack_payment_reports WHERE user_id = $1`, p.userID); c != 1 {
				t.Errorf("pass %d: a live winner's payment record was touched", pass)
			}
		}
		for _, sid := range []uuid.UUID{st1.StatementID, st2.StatementID} {
			is, err := w.svc.Get(ctx, sid)
			if err != nil {
				t.Fatal(err)
			}
			if is.RedactedAt == nil || is.Signature != "" || w.verifies(is) {
				t.Errorf("pass %d: statement %s not redacted: redacted_at %v, signature %q", pass, sid, is.RedactedAt, is.Signature)
			}
			if strings.Contains(is.Statement, erased.login) || strings.Contains(is.Statement, strconv.FormatInt(erased.ghID, 10)) {
				t.Errorf("pass %d: the redacted statement still names the erased winner: %s", pass, is.Statement)
			}
			for _, p := range w.people[1:] {
				if !strings.Contains(is.Statement, p.login) {
					t.Errorf("pass %d: the redacted statement lost a live winner's line: %s", pass, is.Statement)
				}
			}
			if is.PoolMinor != "10000000" || is.HackathonID != w.hid {
				t.Errorf("pass %d: the statement's own facts changed: %+v", pass, is)
			}
		}
		if c := count(t, w.d, `SELECT count(*) FROM grainhack_results_retention_log WHERE op = 'line_deleted'`); c != 2 {
			t.Errorf("pass %d: %d line deletions logged, want 2", pass, c)
		}
		if c := count(t, w.d, `SELECT count(*) FROM grainhack_results_retention_log WHERE op = 'statement_redacted'`); c != 2 {
			t.Errorf("pass %d: %d redactions logged, want 2", pass, c)
		}
	}
	check(1)
	n, err = r.RunOnce(ctx)
	if err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	for _, tb := range tables {
		if n[tb] != 0 {
			t.Errorf("second pass: %s erased %d", tb, n[tb])
		}
	}
	check(2)
}

// An event with a line still payable and unpaid in its latest statement is
// still paying: none of its records are erased, however old.
func TestGrainHackStatements_RetentionWaitsForAnUnpaidLine(t *testing.T) {
	w := seedGrainHack(t)
	ctx := context.Background()
	st1 := w.issue()
	w.pay(st1, 0)
	w.erase(0)
	r := NewRetention(w.d.Pool, &fakeExternal{}, time.Hour)
	r.now = func() time.Time { return time.Now().AddDate(PayoutRecordYears+1, 0, 0) }
	n, err := r.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n["grainhack_results_statement_lines"] != 0 || n["grainhack_payment_reports"] != 0 {
		t.Errorf("an event still paying had records erased: %v", n)
	}
}

// The way past GH010 is the retention transaction's alone, and narrow there.
func TestGrainHackStatements_NothingElseBypassesGH010(t *testing.T) {
	w := seedGrainHack(t)
	ctx := context.Background()
	st := w.issue()
	w.erase(0)
	erasedLine := w.people[0].userID
	liveLine := w.people[1].userID

	isGH010 := func(err error) bool {
		var pg *pgconn.PgError
		return errors.As(err, &pg) && pg.Code == grainhack.SQLStateImmutable
	}
	// attempt runs setup and then stmt in one transaction, always rolled back.
	attempt := func(setup, stmt string, args ...any) error {
		t.Helper()
		tx, err := w.d.Pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		if setup != "" {
			if _, err := tx.Exec(ctx, setup); err != nil {
				t.Fatalf("setup %q: %v", setup, err)
			}
		}
		_, err = tx.Exec(ctx, stmt, args...)
		return err
	}
	allow := `SELECT set_config('` + statementRetentionSetting + `', txid_current()::text, true)`
	delLine := `DELETE FROM grainhack_results_statement_lines WHERE user_id = $1`

	// The control: the retention transaction's setting deletes an erased line.
	if err := attempt(allow, delLine, erasedLine); err != nil {
		t.Fatalf("the retention bypass itself failed: %v", err)
	}

	refused := map[string]error{
		"no setting":                        attempt("", delLine, erasedLine),
		"another transaction's id":          attempt(`SELECT set_config('`+statementRetentionSetting+`', (txid_current() + 1)::text, true)`, delLine, erasedLine),
		"any other value":                   attempt(`SELECT set_config('`+statementRetentionSetting+`', 'on', true)`, delLine, erasedLine),
		"a live account's line":             attempt(allow, delLine, liveLine),
		"updating a line":                   attempt(allow, `UPDATE grainhack_results_statement_lines SET login = 'x' WHERE user_id = $1`, erasedLine),
		"deleting a statement":              attempt(allow, `DELETE FROM grainhack_results_statements WHERE id = $1`, st.StatementID),
		"redacting with erased lines left":  attempt(allow, `UPDATE grainhack_results_statements SET canonical_json = '{}', signature = '', redacted_at = now() WHERE id = $1`, st.StatementID),
		"changing more than the document":   attempt(allow+`; DELETE FROM grainhack_results_statement_lines WHERE user_id = '`+erasedLine.String()+`'`, `UPDATE grainhack_results_statements SET canonical_json = '{}', signature = '', redacted_at = now(), issued_at = now() WHERE id = $1`, st.StatementID),
		"changing the document unredacted":  attempt(allow+`; DELETE FROM grainhack_results_statement_lines WHERE user_id = '`+erasedLine.String()+`'`, `UPDATE grainhack_results_statements SET canonical_json = '{}' WHERE id = $1`, st.StatementID),
	}
	for what, err := range refused {
		if !isGH010(err) {
			t.Errorf("%s: %v, want GH010", what, err)
		}
	}

	// A setting left on a session, not on the transaction, matches nothing
	// in the next transaction.
	pool, ok := w.d.Pool.(*pgxpool.Pool)
	if !ok {
		t.Fatalf("the test database is a %T, not a pool to take one connection from", w.d.Pool)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT set_config('`+statementRetentionSetting+`', txid_current()::text, false)`); err != nil {
		t.Fatal(err)
	}
	_, err = conn.Exec(ctx, delLine, erasedLine)
	_, _ = conn.Exec(ctx, `RESET `+statementRetentionSetting)
	if !isGH010(err) {
		t.Errorf("a session-wide setting from an earlier transaction: %v, want GH010", err)
	}

	if n := count(t, w.d, `SELECT count(*) FROM grainhack_results_statement_lines WHERE statement_id = $1`, st.StatementID); n != 4 {
		t.Errorf("%d lines left, want all 4", n)
	}
	if n := count(t, w.d, `SELECT count(*) FROM grainhack_results_retention_log`); n != 0 {
		t.Errorf("%d retention log rows after only rolled-back attempts", n)
	}
}
