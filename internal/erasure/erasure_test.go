package erasure

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/cryptox"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

// fakeExternal records what the executor asked of the other services and
// answers with whatever the test set.
type fakeExternal struct {
	mu         sync.Mutex
	revoked    []string
	diditDel   []string
	agentErase []int64
	// agentRetain records the retainInFlight flag of each agent call.
	agentRetain []bool
	diditErr    error
	agentErr    error
	githubErr   error
}

func (f *fakeExternal) RevokeGitHub(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, token)
	return f.githubErr
}

func (f *fakeExternal) DeleteDiditSession(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.diditDel = append(f.diditDel, id)
	return f.diditErr
}

func (f *fakeExternal) EraseAtAgent(_ context.Context, ghID int64, _ string, retainInFlight bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agentErase = append(f.agentErase, ghID)
	f.agentRetain = append(f.agentRetain, retainInFlight)
	// The real agent never refuses an erasure told to retain what is in flight.
	if retainInFlight && errors.Is(f.agentErr, ErrAgentInFlight) {
		return nil
	}
	return f.agentErr
}

// fakeNotifier records what the person was told.
type fakeNotifier struct {
	mu   sync.Mutex
	sent []string
}

func (n *fakeNotifier) Notify(_ context.Context, _ uuid.UUID, _ notifications.Type, title, body string, _ notifications.Link) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, title+": "+body)
}

var testEncKey = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

var ghIDSeq atomic.Int64

func init() { ghIDSeq.Store(time.Now().UnixNano() % 1_000_000_000) }

func exec(t *testing.T, d *db.DB, sql string, args ...any) {
	t.Helper()
	if _, err := d.Pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", strings.TrimSpace(strings.SplitN(sql, "\n", 2)[0]), err)
	}
}

func count(t *testing.T, d *db.DB, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := d.Pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// person is everything seeded for one account, so assertions can name it.
type person struct {
	id        uuid.UUID
	ghID      int64
	login     string
	token     string
	wallet    string
	payout    string
	sessions  []string
	projectID uuid.UUID
	settleID  uuid.UUID
}

// seedPerson creates an account with data in every kind of table erasure
// touches or keeps. Cleanup removes it however the test ends.
func seedPerson(t *testing.T, d *db.DB) person {
	t.Helper()
	ctx := context.Background()
	p := person{
		id:     uuid.New(),
		ghID:   ghIDSeq.Add(1),
		token:  "gho_" + uuid.NewString(),
		wallet: "0x" + strings.Repeat("a", 32) + uuid.NewString()[:8],
		payout: "0x" + strings.ReplaceAll(uuid.NewString(), "-", "")[:32] + "bbbbbbbb",
	}
	p.login = "erasure-test-" + uuid.NewString()[:8]
	p.sessions = []string{"sess-current-" + p.login, "sess-reset-" + p.login, "sess-alert-" + p.login}

	exec(t, d, `INSERT INTO users (id, role, display_name, github_user_id, first_name, last_name, location, bio,
	   telegram, twitter, discord, payout_contact_email, kyc_status, kyc_session_id, kyc_verified_at, kyc_data, referral_code)
	 VALUES ($1, 'admin', 'Ada L', $2, 'Ada', 'Lovelace', 'London', 'bio', '@ada', '@ada', 'ada#1', 'ada@pay.example',
	   'verified', $3, now(), '{"id_verification":{"first_name":"Ada","document_number":"X123"}}', $4)`,
		p.id, p.ghID, p.sessions[0], "REF"+p.login)
	t.Cleanup(func() { cleanupPerson(d, p) })
	exec(t, d, `UPDATE users SET email = 'ada@example.com' WHERE id = $1`, p.id)

	key, _ := cryptox.KeyFromB64(testEncKey)
	blob, _ := cryptox.EncryptAESGCM(key, []byte(p.token))
	exec(t, d, `INSERT INTO github_accounts (user_id, github_user_id, login, access_token, avatar_url) VALUES ($1,$2,$3,$4,'https://a/x.png')`,
		p.id, p.ghID, p.login, blob)
	exec(t, d, `INSERT INTO wallets (user_id, wallet_type, address) VALUES ($1, 'evm', $2)`, p.id, p.wallet)
	exec(t, d, `INSERT INTO contributor_addresses (user_id, chain_id, address, verified_nonce, chain_family) VALUES ($1,'evm:8453',$2,'n','evm')`, p.id, p.payout)
	exec(t, d, `INSERT INTO auth_nonces (wallet_type, address, nonce, expires_at) VALUES ('evm', $1, $2, now() + interval '1 hour')`, p.wallet, "nonce-"+p.login)
	exec(t, d, `INSERT INTO notifications (user_id, type, title) VALUES ($1, 'kyc_reset', 'hello')`, p.id)
	exec(t, d, `INSERT INTO notification_preferences (user_id, type) VALUES ($1, 'kyc_reset')`, p.id)
	exec(t, d, `INSERT INTO terms_acceptances (user_id, version) VALUES ($1, '2026-10-02')`, p.id)
	exec(t, d, `INSERT INTO kyc_reset_audit (subject_user_id, previous_status, previous_session_id, previous_kyc_data, reason, reason_code, note)
	   VALUES ($1, 'rejected', $2, '{"first_name":"Ada"}', 'Ada''s document was blurry', 'document_unreadable', 'called Ada')`, p.id, p.sessions[1])
	exec(t, d, `INSERT INTO kyc_review_alerts (session_id, user_id, source) VALUES ($1, $2, 'webhook')`, p.sessions[2], p.id)
	exec(t, d, `INSERT INTO social_follow_submissions (user_id, linkedin_screenshot, x_screenshot) VALUES ($1, 'data:image/png;base64,AA', 'data:image/png;base64,BB')`, p.id)
	exec(t, d, `INSERT INTO org_ratings (user_id, org_login, rating, comment) VALUES ($1, 'some-org', 4, 'nice')`, p.id)
	exec(t, d, `INSERT INTO support_requests (user_id, category, message) VALUES ($1, 'help', 'my name is Ada')`, p.id)

	if err := d.Pool.QueryRow(ctx, `INSERT INTO projects (owner_user_id, github_full_name) VALUES ($1, $2) RETURNING id`,
		p.id, "erasure-org/"+p.login).Scan(&p.projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	exec(t, d, `INSERT INTO issue_applications (user_id, project_id, issue_number, github_login) VALUES ($1, $2, 1, $3)`, p.id, p.projectID, p.login)

	// A payout already made: settled and released. Kept.
	if err := d.Pool.QueryRow(ctx, `INSERT INTO settlements (pool_usdc, total_weight, unit_value_usdc, released_at) VALUES (100, 1, 100, now()) RETURNING id`).
		Scan(&p.settleID); err != nil {
		t.Fatalf("settlement: %v", err)
	}
	exec(t, d, `INSERT INTO settlement_lines (settlement_id, user_id, raw_weight, multiplier, effective_weight, usdc_amount, amount_minor)
	   VALUES ($1, $2, 1, 1, 1, 100, 100000000)`, p.settleID, p.id)
	return p
}

func cleanupPerson(d *db.DB, p person) {
	ctx := context.Background()
	for _, q := range []string{
		`DELETE FROM account_deletion_requests WHERE user_id = $1`,
		`DELETE FROM settlement_holds WHERE user_id = $1`,
		`DELETE FROM settlement_lines WHERE user_id = $1`,
		`DELETE FROM issue_applications WHERE user_id = $1`,
		`DELETE FROM kyc_reset_audit WHERE subject_user_id = $1`,
		`DELETE FROM referrals WHERE referrer_user_id = $1 OR referred_user_id = $1`,
		`DELETE FROM founding_members WHERE user_id = $1`,
		`DELETE FROM projects WHERE owner_user_id = $1`,
		`DELETE FROM auth_nonces WHERE address = '` + p.wallet + `'`,
		`DELETE FROM users WHERE id = $1`,
	} {
		if strings.Contains(q, "$1") {
			_, _ = d.Pool.Exec(ctx, q, p.id)
		} else {
			_, _ = d.Pool.Exec(ctx, q)
		}
	}
	if p.settleID != uuid.Nil {
		_, _ = d.Pool.Exec(ctx, `DELETE FROM settlements WHERE id = $1`, p.settleID)
	}
}

// newExecutor returns an executor whose clock is moved forward by after, and
// which only sees the given accounts.
func newExecutor(d *db.DB, ext External, after time.Duration, scope ...uuid.UUID) *Executor {
	e := NewExecutor(d.Pool, ext, nil, testEncKey, time.Minute)
	e.now = func() time.Time { return time.Now().Add(after) }
	e.scope = scope
	return e
}

func requestRow(t *testing.T, d *db.DB, uid uuid.UUID) (status string, hold *string, attempts int, erased, retained, external []byte) {
	t.Helper()
	if err := d.Pool.QueryRow(context.Background(), `
SELECT status, hold_reason, attempts, erased, retained, external FROM account_deletion_requests
WHERE user_id = $1 ORDER BY requested_at DESC LIMIT 1`, uid).Scan(&status, &hold, &attempts, &erased, &retained, &external); err != nil {
		t.Fatalf("read request: %v", err)
	}
	return
}

func eventKinds(t *testing.T, d *db.DB, uid uuid.UUID) []string {
	t.Helper()
	rows, err := d.Pool.Query(context.Background(), `
SELECT e.kind FROM account_deletion_events e JOIN account_deletion_requests r ON r.id = e.request_id
WHERE r.user_id = $1 ORDER BY e.at, e.id`, uid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		_ = rows.Scan(&k)
		out = append(out, k)
	}
	return out
}

func TestSchedule_RecordsTheRequestWithAGracePeriod(t *testing.T) {
	d := dbtest.DB(t)
	p := seedPerson(t, d)
	now := time.Now().UTC().Truncate(time.Second)

	req, created, err := Schedule(context.Background(), d.Pool, p.id, now)
	if err != nil || !created {
		t.Fatalf("Schedule = %v created=%v", err, created)
	}
	if req.Status != "scheduled" {
		t.Errorf("status = %s", req.Status)
	}
	if got := req.ExecuteAfter.Sub(req.RequestedAt); got != GracePeriod {
		t.Errorf("grace = %s, want %s", got, GracePeriod)
	}
	if k := eventKinds(t, d, p.id); len(k) != 1 || k[0] != "requested" {
		t.Errorf("events = %v, want [requested]", k)
	}
	// Recording the request touches nothing else.
	if n := count(t, d, `SELECT count(*) FROM github_accounts WHERE user_id = $1`, p.id); n != 1 {
		t.Errorf("github account gone at request time")
	}
}

// A second press while a request is open is the same request: one row, one
// clock, not a reset of the grace period.
func TestSchedule_IsIdempotent(t *testing.T) {
	d := dbtest.DB(t)
	p := seedPerson(t, d)
	first, _, err := Schedule(context.Background(), d.Pool, p.id, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	second, created, err := Schedule(context.Background(), d.Pool, p.id, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if created || second.ID != first.ID || !second.ExecuteAfter.Equal(first.ExecuteAfter) {
		t.Errorf("second press: created=%v id %s vs %s, execute_after %s vs %s",
			created, second.ID, first.ID, second.ExecuteAfter, first.ExecuteAfter)
	}
	if n := count(t, d, `SELECT count(*) FROM account_deletion_requests WHERE user_id = $1`, p.id); n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
}

func TestCancel_DuringTheGracePeriodLeavesEverything(t *testing.T) {
	d := dbtest.DB(t)
	p := seedPerson(t, d)
	ext := &fakeExternal{}
	if _, _, err := Schedule(context.Background(), d.Pool, p.id, time.Now()); err != nil {
		t.Fatal(err)
	}
	req, err := Cancel(context.Background(), d.Pool, p.id, time.Now())
	if err != nil || req.Status != "cancelled" || req.CancelledAt == nil {
		t.Fatalf("Cancel = %+v, %v", req, err)
	}

	// Even with the clock past the grace period, a cancelled request is never run.
	if _, err := newExecutor(d, ext, GracePeriod+time.Hour, p.id).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := count(t, d, `SELECT count(*) FROM github_accounts WHERE user_id = $1`, p.id); n != 1 {
		t.Error("a cancelled request erased the GitHub link")
	}
	if len(ext.revoked)+len(ext.diditDel)+len(ext.agentErase) != 0 {
		t.Errorf("a cancelled request called other services: %+v", ext)
	}
	if _, err := Cancel(context.Background(), d.Pool, p.id, time.Now()); !errors.Is(err, ErrNoOpenRequest) {
		t.Errorf("second cancel = %v, want ErrNoOpenRequest", err)
	}
	if k := eventKinds(t, d, p.id); strings.Join(k, ",") != "requested,cancelled" {
		t.Errorf("events = %v", k)
	}

	// And a new request after a cancellation starts a fresh clock.
	_, created, err := Schedule(context.Background(), d.Pool, p.id, time.Now())
	if err != nil || !created {
		t.Errorf("request after cancel: created=%v err=%v", created, err)
	}
}

func TestExecutor_DoesNothingBeforeTheGracePeriodEnds(t *testing.T) {
	d := dbtest.DB(t)
	p := seedPerson(t, d)
	ext := &fakeExternal{}
	if _, _, err := Schedule(context.Background(), d.Pool, p.id, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := newExecutor(d, ext, GracePeriod-time.Hour, p.id).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st, _, _, _, _, _ := requestRow(t, d, p.id); st != "scheduled" {
		t.Errorf("status = %s, want scheduled", st)
	}
	if n := count(t, d, `SELECT count(*) FROM users WHERE id = $1 AND erased_at IS NULL AND first_name = 'Ada'`, p.id); n != 1 {
		t.Error("account touched inside the grace period")
	}
}

// The main test: what is erased is erased, what is kept is kept exactly, and
// every other service was asked.
func TestExecutor_ErasesAndKeepsExactlyWhatThePolicySays(t *testing.T) {
	d := dbtest.DB(t)
	p := seedPerson(t, d)
	other := seedPerson(t, d)
	ext := &fakeExternal{}
	ctx := context.Background()

	if _, _, err := Schedule(ctx, d.Pool, p.id, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := newExecutor(d, ext, GracePeriod+time.Minute, p.id).RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// Request completed and recorded.
	st, _, attempts, erasedJSON, retainedJSON, externalJSON := requestRow(t, d, p.id)
	if st != "completed" || attempts != 1 {
		t.Fatalf("status = %s attempts = %d", st, attempts)
	}
	var erased map[string]int64
	_ = json.Unmarshal(erasedJSON, &erased)
	for _, k := range []string{"github_accounts", "wallets", "contributor_addresses", "auth_nonces", "notifications",
		"terms_acceptances", "kyc_review_alerts", "kyc_reset_audit", "issue_applications", "social_follow_submissions",
		"org_ratings", "support_requests", "users"} {
		if erased[k] < 1 {
			t.Errorf("erased[%s] = %d, want >= 1", k, erased[k])
		}
	}
	var retained []Item
	_ = json.Unmarshal(retainedJSON, &retained)
	if len(retained) != len(Retained) {
		t.Errorf("retained list has %d items, want the %d in the policy", len(retained), len(Retained))
	}
	var external map[string]StepResult
	_ = json.Unmarshal(externalJSON, &external)
	for _, k := range []string{"github", "didit", "agent"} {
		if external[k].Outcome != "done" {
			t.Errorf("external[%s] = %+v, want done", k, external[k])
		}
	}
	if k := eventKinds(t, d, p.id); strings.Join(k, ",") != "requested,completed" {
		t.Errorf("events = %v", k)
	}

	// Other services were asked, with the right identifiers.
	if len(ext.revoked) != 1 || ext.revoked[0] != p.token {
		t.Errorf("GitHub revoke called with %v, want the decrypted token", ext.revoked)
	}
	gotSessions := append([]string(nil), ext.diditDel...)
	sort.Strings(gotSessions)
	wantSessions := append([]string(nil), p.sessions...)
	sort.Strings(wantSessions)
	if strings.Join(gotSessions, ",") != strings.Join(wantSessions, ",") {
		t.Errorf("Didit deletions = %v, want every session we held: %v", gotSessions, wantSessions)
	}
	if len(ext.agentErase) != 1 || ext.agentErase[0] != p.ghID {
		t.Errorf("agent erase = %v, want [%d]", ext.agentErase, p.ghID)
	}

	// Erased.
	var erasedAt *time.Time
	var role string
	var first, last, display, bio, tg, kycStatus, kycSession, payoutEmail, email, refCode *string
	var kycData []byte
	var ghID *int64
	if err := d.Pool.QueryRow(ctx, `SELECT erased_at, role, first_name, last_name, display_name, bio, telegram, kyc_status,
	   kyc_session_id, kyc_data, payout_contact_email, github_user_id, email, referral_code FROM users WHERE id = $1`, p.id).
		Scan(&erasedAt, &role, &first, &last, &display, &bio, &tg, &kycStatus, &kycSession, &kycData, &payoutEmail, &ghID, &email, &refCode); err != nil {
		t.Fatalf("read tombstone: %v", err)
	}
	if erasedAt == nil {
		t.Error("erased_at not set")
	}
	if role != "contributor" {
		t.Errorf("role = %s; an erased admin must hold no privilege", role)
	}
	for name, v := range map[string]*string{"first_name": first, "last_name": last, "display_name": display, "bio": bio,
		"telegram": tg, "kyc_status": kycStatus, "kyc_session_id": kycSession, "payout_contact_email": payoutEmail,
		"email": email, "referral_code": refCode} {
		if v != nil {
			t.Errorf("users.%s = %q, want NULL", name, *v)
		}
	}
	if kycData != nil || ghID != nil {
		t.Errorf("kyc_data or github_user_id left on the tombstone")
	}
	for table, q := range map[string]string{
		"github_accounts":           `SELECT count(*) FROM github_accounts WHERE user_id = $1`,
		"wallets":                   `SELECT count(*) FROM wallets WHERE user_id = $1`,
		"contributor_addresses":     `SELECT count(*) FROM contributor_addresses WHERE user_id = $1`,
		"notifications":             `SELECT count(*) FROM notifications WHERE user_id = $1`,
		"notification_preferences":  `SELECT count(*) FROM notification_preferences WHERE user_id = $1`,
		"terms_acceptances":         `SELECT count(*) FROM terms_acceptances WHERE user_id = $1`,
		"kyc_review_alerts":         `SELECT count(*) FROM kyc_review_alerts WHERE user_id = $1`,
		"issue_applications":        `SELECT count(*) FROM issue_applications WHERE user_id = $1`,
		"social_follow_submissions": `SELECT count(*) FROM social_follow_submissions WHERE user_id = $1`,
		"org_ratings":               `SELECT count(*) FROM org_ratings WHERE user_id = $1`,
		"support_requests":          `SELECT count(*) FROM support_requests WHERE user_id = $1`,
		"kyc_reset_audit copies":    `SELECT count(*) FROM kyc_reset_audit WHERE subject_user_id = $1 AND (previous_kyc_data IS NOT NULL OR previous_session_id IS NOT NULL OR reason IS NOT NULL OR note IS NOT NULL)`,
	} {
		if n := count(t, d, q, p.id); n != 0 {
			t.Errorf("%s: %d rows left", table, n)
		}
	}
	if n := count(t, d, `SELECT count(*) FROM auth_nonces WHERE address = $1`, p.wallet); n != 0 {
		t.Errorf("auth_nonces for the sign-in wallet left: %d", n)
	}

	// Kept, exactly.
	if n := count(t, d, `SELECT count(*) FROM settlement_lines WHERE user_id = $1 AND amount_minor = 100000000`, p.id); n != 1 {
		t.Error("the record of a payout already made was not kept")
	}
	if n := count(t, d, `SELECT count(*) FROM kyc_reset_audit WHERE subject_user_id = $1 AND reason_code = 'document_unreadable' AND previous_status = 'rejected'`, p.id); n != 1 {
		t.Error("the reset audit row (what was decided) was not kept")
	}
	if n := count(t, d, `SELECT count(*) FROM projects WHERE id = $1`, p.projectID); n != 1 {
		t.Error("the listed project was removed; it belongs to the repository")
	}
	if n := count(t, d, `SELECT count(*) FROM account_deletion_requests WHERE user_id = $1`, p.id); n != 1 {
		t.Error("the deletion record itself is missing")
	}

	// Somebody else's account is untouched.
	if n := count(t, d, `SELECT count(*) FROM users WHERE id = $1 AND erased_at IS NULL AND first_name = 'Ada'`, other.id); n != 1 {
		t.Error("another person's profile was touched")
	}
	if n := count(t, d, `SELECT count(*) FROM github_accounts WHERE user_id = $1`, other.id); n != 1 {
		t.Error("another person's GitHub link was touched")
	}
	if n := count(t, d, `SELECT count(*) FROM kyc_reset_audit WHERE subject_user_id = $1 AND previous_kyc_data IS NOT NULL`, other.id); n != 1 {
		t.Error("another person's reset audit copy was touched")
	}
}

// Running again finds nothing to do, and asking again is refused rather than
// starting a second erasure of an empty record.
func TestExecutor_IsIdempotent(t *testing.T) {
	d := dbtest.DB(t)
	p := seedPerson(t, d)
	ext := &fakeExternal{}
	ctx := context.Background()
	if _, _, err := Schedule(ctx, d.Pool, p.id, time.Now()); err != nil {
		t.Fatal(err)
	}
	e := newExecutor(d, ext, GracePeriod+time.Minute, p.id)
	if _, err := e.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := e.RunOnce(ctx); err != nil || n != 0 {
		t.Errorf("second pass examined %d (err %v), want 0", n, err)
	}
	if len(ext.revoked) != 1 || len(ext.agentErase) != 1 {
		t.Errorf("other services called again: %+v", ext)
	}
	if _, _, err := Schedule(ctx, d.Pool, p.id, time.Now()); !errors.Is(err, ErrAlreadyErased) {
		t.Errorf("request after erasure = %v, want ErrAlreadyErased", err)
	}
	if _, err := Cancel(ctx, d.Pool, p.id, time.Now()); !errors.Is(err, ErrNoOpenRequest) {
		t.Errorf("cancel after erasure = %v, want ErrNoOpenRequest", err)
	}
}

// Money still owed holds the erasure, touches nothing, and the erasure
// proceeds by itself once the money has gone out.
func TestExecutor_HoldsWhileMoneyIsInFlight(t *testing.T) {
	d := dbtest.DB(t)
	p := seedPerson(t, d)
	ext := &fakeExternal{}
	ctx := context.Background()
	exec(t, d, `INSERT INTO settlement_holds (user_id, origin_settlement_id, amount_minor, reason) VALUES ($1, $2, 5, 'no_address')`, p.id, p.settleID)
	if _, _, err := Schedule(ctx, d.Pool, p.id, time.Now()); err != nil {
		t.Fatal(err)
	}
	e := newExecutor(d, ext, GracePeriod+time.Minute, p.id)
	if _, err := e.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	st, hold, _, _, _, _ := requestRow(t, d, p.id)
	if st != "held" || hold == nil || !strings.Contains(*hold, "held back") {
		t.Fatalf("status = %s hold = %v, want held with the reason", st, hold)
	}
	if n := count(t, d, `SELECT count(*) FROM contributor_addresses WHERE user_id = $1`, p.id); n != 1 {
		t.Error("payout address erased while money was still owed")
	}
	if len(ext.revoked)+len(ext.diditDel)+len(ext.agentErase) != 0 {
		t.Errorf("a held request called other services: %+v", ext)
	}

	// Released: the next pass after the re-check time completes it.
	exec(t, d, `UPDATE settlement_holds SET released_at = now(), released_in_settlement_id = $2 WHERE user_id = $1`, p.id, p.settleID)
	e.now = func() time.Time { return time.Now().Add(GracePeriod + HoldRecheck + time.Hour) }
	if _, err := e.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _, _, _, _, _ := requestRow(t, d, p.id); st != "completed" {
		t.Errorf("status after release = %s, want completed", st)
	}
	if k := eventKinds(t, d, p.id); strings.Join(k, ",") != "requested,held,completed" {
		t.Errorf("events = %v", k)
	}
}

func TestExecutor_AgentWorkInProgressHolds(t *testing.T) {
	d := dbtest.DB(t)
	p := seedPerson(t, d)
	ext := &fakeExternal{agentErr: ErrAgentInFlight}
	ctx := context.Background()
	if _, _, err := Schedule(ctx, d.Pool, p.id, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := newExecutor(d, ext, GracePeriod+time.Minute, p.id).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if st, hold, _, _, _, _ := requestRow(t, d, p.id); st != "held" || hold == nil || !strings.Contains(*hold, "bounty") {
		t.Errorf("status = %s hold = %v", st, hold)
	}
	// The agent is asked first precisely so a refusal comes before anything
	// irreversible elsewhere.
	if len(ext.revoked) != 0 || len(ext.diditDel) != 0 {
		t.Errorf("GitHub or Didit called before the agent's refusal: %+v", ext)
	}
}

// A hold lasts MaxHold at most, counted from when the erasure became due. Until
// then nothing is touched and the person is told once, with the date; at the
// limit the erasure goes ahead and the record of the money in flight is kept,
// on the empty account record, so it can still be paid or resolved.
func TestExecutor_HoldLastsAtMostMaxHold(t *testing.T) {
	d := dbtest.DB(t)
	p := seedPerson(t, d)
	ext := &fakeExternal{}
	told := &fakeNotifier{}
	ctx := context.Background()
	exec(t, d, `INSERT INTO settlement_holds (user_id, origin_settlement_id, amount_minor, reason) VALUES ($1, $2, 5, 'no_address')`, p.id, p.settleID)
	req, _, err := Schedule(ctx, d.Pool, p.id, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	until := req.ExecuteAfter.Add(MaxHold)

	e := newExecutor(d, ext, GracePeriod+time.Minute, p.id)
	e.notif = told
	// Passes through the month: the first holds, the rest re-check.
	for _, at := range []time.Duration{GracePeriod + time.Minute, GracePeriod + 10*24*time.Hour, GracePeriod + MaxHold - time.Hour} {
		e.now = func() time.Time { return req.ExecuteAfter.Add(at - GracePeriod) }
		if _, err := e.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		st, hold, _, _, _, _ := requestRow(t, d, p.id)
		if st != "held" {
			t.Fatalf("at due+%s: status = %s, want held", at-GracePeriod, st)
		}
		if hold == nil || !strings.Contains(*hold, readableDate(until)) {
			t.Errorf("hold reason does not give the date the hold ends (%s): %v", readableDate(until), hold)
		}
	}
	if n := count(t, d, `SELECT count(*) FROM users WHERE id = $1 AND erased_at IS NULL AND first_name = 'Ada'`, p.id); n != 1 {
		t.Fatal("account touched while held inside the limit")
	}
	if len(told.sent) != 1 {
		t.Fatalf("notifications while held = %d, want exactly 1: %v", len(told.sent), told.sent)
	}
	if !strings.Contains(told.sent[0], readableDate(until)) || !strings.Contains(told.sent[0], "keep only the record of the payment") {
		t.Errorf("the notification must give the date and say the payment record is kept: %s", told.sent[0])
	}
	if len(ext.agentErase) != 0 {
		t.Errorf("other services called while held: %+v", ext)
	}

	// The limit: it goes ahead with the money still in flight.
	e.now = func() time.Time { return until.Add(time.Minute) }
	if _, err := e.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	st, _, _, _, retainedJSON, _ := requestRow(t, d, p.id)
	if st != "completed" {
		t.Fatalf("status at the limit = %s, want completed", st)
	}
	if n := count(t, d, `SELECT count(*) FROM users WHERE id = $1 AND erased_at IS NOT NULL AND first_name IS NULL`, p.id); n != 1 {
		t.Error("account not erased at the limit")
	}
	if n := count(t, d, `SELECT count(*) FROM settlement_holds WHERE user_id = $1 AND amount_minor = 5 AND released_at IS NULL`, p.id); n != 1 {
		t.Error("the record of the money still in flight was not kept on the tombstone")
	}
	if len(ext.agentRetain) != 1 || !ext.agentRetain[0] {
		t.Errorf("agent asked with retainInFlight = %v, want [true] at the limit", ext.agentRetain)
	}
	var retained []Item
	_ = json.Unmarshal(retainedJSON, &retained)
	last := retained[len(retained)-1]
	if len(retained) != len(Retained)+1 || !strings.Contains(last.What, "held back") {
		t.Errorf("retained list should end with the money in flight: %+v", last)
	}
	if k := eventKinds(t, d, p.id); strings.Join(k, ",") != "requested,held,hold_limit_reached,completed" {
		t.Errorf("events = %v", k)
	}
	if len(told.sent) != 1 {
		t.Errorf("notifications = %d after completion, want still 1", len(told.sent))
	}
}

// The agent's refusal is a hold like ours, with the same limit: past it the
// agent is asked to erase while retaining what its own money in flight needs.
func TestExecutor_AgentHoldEndsAtTheLimitToo(t *testing.T) {
	d := dbtest.DB(t)
	p := seedPerson(t, d)
	ext := &fakeExternal{agentErr: ErrAgentInFlight}
	ctx := context.Background()
	req, _, err := Schedule(ctx, d.Pool, p.id, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e := newExecutor(d, ext, GracePeriod+time.Minute, p.id)
	if _, err := e.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _, _, _, _, _ := requestRow(t, d, p.id); st != "held" {
		t.Fatalf("status = %s, want held by the agent", st)
	}
	e.now = func() time.Time { return req.ExecuteAfter.Add(MaxHold + time.Minute) }
	if _, err := e.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	st, _, _, _, retainedJSON, externalJSON := requestRow(t, d, p.id)
	if st != "completed" {
		t.Fatalf("status = %s, want completed at the limit", st)
	}
	if fmt.Sprint(ext.agentRetain) != "[false true]" {
		t.Errorf("agent retainInFlight per call = %v, want [false true]", ext.agentRetain)
	}
	var external map[string]StepResult
	_ = json.Unmarshal(externalJSON, &external)
	if external["agent"].Outcome != "done" {
		t.Errorf("agent step = %+v, want done", external["agent"])
	}
	var retained []Item
	_ = json.Unmarshal(retainedJSON, &retained)
	if last := retained[len(retained)-1]; !strings.Contains(last.What, "bounty") {
		t.Errorf("retained list should name the agent's money in flight: %+v", last)
	}
}

// A service that cannot be reached defers the erasure; after the attempt limit
// it goes ahead and records what is left to do by hand.
func TestExecutor_UnreachableServiceDefersThenRecords(t *testing.T) {
	d := dbtest.DB(t)
	p := seedPerson(t, d)
	ext := &fakeExternal{diditErr: errors.New("didit: status 503")}
	ctx := context.Background()
	if _, _, err := Schedule(ctx, d.Pool, p.id, time.Now()); err != nil {
		t.Fatal(err)
	}
	e := newExecutor(d, ext, GracePeriod+time.Minute, p.id)
	for i := 1; i < MaxExternalAttempts; i++ {
		if _, err := e.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		st, _, attempts, _, _, _ := requestRow(t, d, p.id)
		if st != "scheduled" || attempts != i {
			t.Fatalf("pass %d: status = %s attempts = %d, want deferred", i, st, attempts)
		}
		if n := count(t, d, `SELECT count(*) FROM users WHERE id = $1 AND erased_at IS NULL`, p.id); n != 1 {
			t.Fatalf("pass %d: erased although a step failed", i)
		}
		// Not before the retry time...
		if n, _ := e.RunOnce(ctx); n != 0 {
			t.Fatalf("pass %d: retried before RetryAfter", i)
		}
		// ...and then after it.
		later := e.now().Add(RetryAfter + time.Minute)
		e.now = func() time.Time { return later }
	}
	if _, err := e.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	st, _, _, _, retainedJSON, externalJSON := requestRow(t, d, p.id)
	if st != "completed" {
		t.Fatalf("status = %s after %d attempts, want completed", st, MaxExternalAttempts)
	}
	var external map[string]StepResult
	_ = json.Unmarshal(externalJSON, &external)
	if external["didit"].Outcome != "failed" || len(external["didit"].Pending) != len(p.sessions) {
		t.Errorf("didit = %+v, want failed with every session id kept for follow-up", external["didit"])
	}
	if len(external["github"].Pending) != 0 || len(external["agent"].Pending) != 0 {
		t.Errorf("identifiers kept for steps that succeeded: %+v", external)
	}
	var retained []Item
	_ = json.Unmarshal(retainedJSON, &retained)
	if len(retained) != len(Retained)+1 || !strings.Contains(retained[len(retained)-1].What, "didit") {
		t.Errorf("retained list should add the unfinished Didit step: %+v", retained[len(retained)-1:])
	}
}

// Every table with a foreign key into users is either erased by a step or
// listed as kept with a reason. A new table must be one or the other.
func TestErase_EveryUserTableIsDecided(t *testing.T) {
	d := dbtest.DB(t)
	rows, err := d.Pool.Query(context.Background(), `
SELECT DISTINCT tc.table_name
FROM information_schema.table_constraints tc
JOIN information_schema.constraint_column_usage ccu
  ON ccu.constraint_name = tc.constraint_name AND ccu.table_schema = tc.table_schema
WHERE tc.constraint_type = 'FOREIGN KEY' AND ccu.table_name = 'users' AND tc.table_schema = 'public'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	steps := map[string]bool{}
	for _, s := range eraseSteps {
		steps[s.name] = true
	}
	var undecided []string
	for rows.Next() {
		var table string
		_ = rows.Scan(&table)
		if !steps[table] && keptTables[table] == "" {
			undecided = append(undecided, table)
		}
	}
	if len(undecided) > 0 {
		t.Errorf("tables referencing users with no erasure decision: %v\n"+
			"add an erase step (internal/erasure/erase.go) or a keptTables entry with the reason", undecided)
	}
}
