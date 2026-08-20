package handlers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
	"github.com/jagadeesh/grainlify/backend/internal/didit"
)

// fakeDecisions answers whatever the test says Didit currently holds, and
// counts calls so a test can prove a session was NOT asked about.
type fakeDecisions struct {
	bySession map[string]didit.SessionDecisionResponse
	errFor    map[string]error
	calls     map[string]int
}

func (f *fakeDecisions) GetSessionDecision(_ context.Context, sessionID string) (didit.SessionDecisionResponse, error) {
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[sessionID]++
	if err, ok := f.errFor[sessionID]; ok {
		return didit.SessionDecisionResponse{}, err
	}
	return f.bySession[sessionID], nil
}

func reconcilerFxUser(t *testing.T, d *db.DB, status string, sessionID *string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := d.Pool.QueryRow(context.Background(), `
INSERT INTO users (role, display_name, github_user_id, kyc_status, kyc_session_id)
VALUES ('contributor', $1, $2, $3, $4)
RETURNING id
`, "kyc-recon-"+uuid.NewString(), reconcilerFxNextGHID(), status, sessionID).Scan(&id)
	if err != nil {
		t.Fatalf("reconcilerFxUser: %v", err)
	}
	return id
}

// Unique per ROW, not per process.
//
// A package-level counter looked fine and passed once: this database is never
// truncated by design, so run two reused every id run one had inserted and
// every test in the file died on the unique constraint. The convention here is
// unique-per-row for exactly that reason - see seedSocialFollowUser, which
// composes the same two sources.
func reconcilerFxNextGHID() int64 {
	return time.Now().UnixNano() + int64(uuid.New().ID())
}

func readKYC(t *testing.T, d *db.DB, id uuid.UUID) (status string, reconciledSet bool) {
	t.Helper()
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT COALESCE(kyc_status,''), kyc_reconciled_at IS NOT NULL FROM users WHERE id = $1`,
		id).Scan(&status, &reconciledSet); err != nil {
		t.Fatalf("readKYC: %v", err)
	}
	return
}

func newTestReconciler(d *db.DB, f *fakeDecisions) *KYCStatusReconciler {
	return &KYCStatusReconciler{db: d, didit: f, batch: 50}
}

// The case the whole reconciler exists for: somebody verified with us whom
// Didit has since declined, where the webhook that would have told us never
// arrived.
func TestKYCReconciler_ReversesAVerifiedUserWhenDiditNowDeclines(t *testing.T) {
	d := dbtest.DB(t)
	session := "recon-reversal-" + uuid.NewString()
	user := reconcilerFxUser(t, d, "verified", &session)

	f := &fakeDecisions{bySession: map[string]didit.SessionDecisionResponse{
		session: {Status: "Declined"},
	}}
	newTestReconciler(d, f).reconcileOnce(context.Background())

	got, reconciled := readKYC(t, d, user)
	if got != "rejected" {
		t.Errorf("kyc_status = %q, want rejected - a verified user Didit has declined must not stay verified", got)
	}
	if !reconciled {
		t.Error("kyc_reconciled_at not stamped")
	}
}

// An unrecognised status must never overwrite a real one - the same rule the
// webhook follows. A status we cannot map is a status we do not understand,
// and writing a guess is worse than writing nothing.
func TestKYCReconciler_UnrecognisedStatusLeavesTheRowAlone(t *testing.T) {
	d := dbtest.DB(t)
	session := "recon-unknown-" + uuid.NewString()
	user := reconcilerFxUser(t, d, "verified", &session)

	f := &fakeDecisions{bySession: map[string]didit.SessionDecisionResponse{
		session: {Status: "Something Didit Invented On A Tuesday"},
	}}
	newTestReconciler(d, f).reconcileOnce(context.Background())

	if got, _ := readKYC(t, d, user); got != "verified" {
		t.Errorf("kyc_status = %q, want verified unchanged", got)
	}
}

// The queue must keep rotating when a session permanently fails.
//
// Six sessions answer 403 to our API key today. If the attempt were stamped
// only on success they would sit at the head of a NULLS FIRST queue forever
// and starve every session behind them - the reconciler would run, look busy,
// and re-read the same six rows until somebody noticed.
func TestKYCReconciler_StampsTheAttemptEvenWhenDiditRefuses(t *testing.T) {
	d := dbtest.DB(t)
	session := "recon-403-" + uuid.NewString()
	user := reconcilerFxUser(t, d, "verified", &session)

	f := &fakeDecisions{errFor: map[string]error{
		session: errors.New("didit get decision failed: status 403"),
	}}
	newTestReconciler(d, f).reconcileOnce(context.Background())

	got, reconciled := readKYC(t, d, user)
	if got != "verified" {
		t.Errorf("kyc_status = %q, want verified - a failed read must not change a status", got)
	}
	if !reconciled {
		t.Error("kyc_reconciled_at not stamped after a failed read; this session would block the queue forever")
	}
}

// A contributor an admin has reset must never be pulled back to their old
// status. The guard is structural - the reset nulls kyc_session_id, so they
// leave the queue - and this asserts the structure rather than trusting it:
// if a future reset stops nulling the session id, this fails.
func TestKYCReconciler_NeverUndoesAnAdminReset(t *testing.T) {
	d := dbtest.DB(t)
	session := "recon-reset-" + uuid.NewString()
	user := reconcilerFxUser(t, d, "in_review", &session)

	// Exactly what KYCAdminHandler's reset does.
	if _, err := d.Pool.Exec(context.Background(),
		`UPDATE users SET kyc_status = 'expired', kyc_session_id = NULL WHERE id = $1`, user); err != nil {
		t.Fatalf("simulate reset: %v", err)
	}

	// Didit still holds the old session and still calls it approved.
	f := &fakeDecisions{bySession: map[string]didit.SessionDecisionResponse{
		session: {Status: "Approved"},
	}}
	newTestReconciler(d, f).reconcileOnce(context.Background())

	if got, _ := readKYC(t, d, user); got != "expired" {
		t.Errorf("kyc_status = %q, want expired - the reconciler undid an admin reset", got)
	}
	if f.calls[session] != 0 {
		t.Errorf("the reset session was queried %d times; a reset contributor must leave the queue entirely", f.calls[session])
	}
}

// No change means no write. Cheap to assert and it pins the thing that would
// otherwise re-date every row on every tick.
func TestKYCReconciler_AgreementWritesNothing(t *testing.T) {
	d := dbtest.DB(t)
	session := "recon-agree-" + uuid.NewString()
	user := reconcilerFxUser(t, d, "verified", &session)

	var before string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT updated_at::text FROM users WHERE id = $1`, user).Scan(&before); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}

	f := &fakeDecisions{bySession: map[string]didit.SessionDecisionResponse{
		session: {Status: "Approved"},
	}}
	newTestReconciler(d, f).reconcileOnce(context.Background())

	var after string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT updated_at::text FROM users WHERE id = $1`, user).Scan(&after); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}
	if before != after {
		t.Errorf("updated_at moved (%s -> %s) with no status change", before, after)
	}
}

// Run must reconcile once BEFORE the first tick.
//
// Without this the first pass is a whole interval after boot, and on a
// platform that restarts the process on every deploy each deploy pushes it
// another interval away - a day of frequent deploys can leave the reconciler
// having never completed a pass at all. The sweeper this was written as a
// sibling of has always done a startup pass; the requirement was in its
// comment and was read as description.
//
// The interval here is an hour, so a reconcile that happens at all is the
// startup pass and cannot be the ticker.
func TestKYCReconciler_ReconcilesOnceAtStartupBeforeTheFirstTick(t *testing.T) {
	d := dbtest.DB(t)
	session := "recon-startup-" + uuid.NewString()
	user := reconcilerFxUser(t, d, "verified", &session)

	f := &fakeDecisions{bySession: map[string]didit.SessionDecisionResponse{session: {Status: "Declined"}}}
	r := &KYCStatusReconciler{db: d, didit: f, batch: 500, interval: time.Hour}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	// Poll for the WRITE, not for the lookup.
	//
	// An earlier version of this test signalled on the lookup and cancelled
	// the context as soon as it saw one - which cancelled the very UPDATE it
	// was waiting for, because reconcileOnce does the write on the same
	// context. It failed for a reason that had nothing to do with the
	// behaviour under test. Waiting for the observable outcome has no such
	// race: nothing but the startup pass can produce it inside an hour.
	deadline := time.Now().Add(25 * time.Second)
	for {
		if got, _ := readKYC(t, d, user); got == "rejected" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("status unchanged after 25s with a one-hour tick interval: Run did no startup pass")
		}
		time.Sleep(150 * time.Millisecond)
	}

	cancel()
	<-done
}
