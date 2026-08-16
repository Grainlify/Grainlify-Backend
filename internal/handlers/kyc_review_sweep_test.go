package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// seedKYCUser creates a user in a given kyc state, with updated_at backdated by
// age so the sweep's threshold can be exercised without waiting.
func seedKYCUser(t *testing.T, d *db.DB, status, sessionID string, age time.Duration) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	err := d.Pool.QueryRow(ctx, `
INSERT INTO users (role, display_name, github_user_id, kyc_status, kyc_session_id, updated_at)
VALUES ('contributor', 'sweep-test', $1, $2, NULLIF($3,''), now() - $4::interval)
RETURNING id
`, int64(uuid.New().ID()), status, sessionID, age.String()).Scan(&id)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func newTestSweeper(d *db.DB, sink SupportSink) *KYCReviewSweeper {
	return &KYCReviewSweeper{db: d, sink: sink, minAge: 15 * time.Minute, interval: time.Hour}
}

// The sweep is the fallback for a webhook that never arrived. Didit retries
// twice and then drops the delivery, so without this a five-minute outage on
// our side loses the event permanently.
func TestSweep_AlertsOnlyForSessionsOldEnoughThatTheWebhookIsNotComing(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	sink := &countingSink{}

	oldSession, freshSession := "sess-old-"+uuid.NewString(), "sess-fresh-"+uuid.NewString()
	seedKYCUser(t, d, "in_review", oldSession, 2*time.Hour)
	seedKYCUser(t, d, "in_review", freshSession, 2*time.Minute)

	newTestSweeper(d, sink).sweepOnce(ctx)

	// Scoped to the two sessions this test created: the test database is shared
	// across tests, so counting all claim rows would be measuring other tests.
	claimed := func(session string) bool {
		var n int
		if err := d.Pool.QueryRow(ctx,
			`SELECT count(*) FROM kyc_review_alerts WHERE session_id = $1`, session).Scan(&n); err != nil {
			t.Fatalf("read claim: %v", err)
		}
		return n > 0
	}
	if !claimed(oldSession) {
		t.Error("a session waiting two hours was not reported")
	}
	if claimed(freshSession) {
		t.Error("a two-minute-old session was reported; Didit's webhook retries have not finished")
	}
}

// The states that must stay silent. A sweep that alerts on approved or
// declined sessions is a sweep that gets muted.
func TestSweep_IgnoresEveryStatusExceptInReview(t *testing.T) {
	d := dbtest.DB(t)
	sink := &countingSink{}

	for _, status := range []string{"verified", "not_started", "pending", "rejected", "expired"} {
		seedKYCUser(t, d, status, "sess-"+status+"-"+uuid.NewString(), 3*time.Hour)
	}
	// In review but with no session id: nothing to link an admin to, and no key
	// to claim on, so it cannot be alerted once rather than repeatedly.
	seedKYCUser(t, d, "in_review", "", 3*time.Hour)

	newTestSweeper(d, sink).sweepOnce(context.Background())

	if sink.count() != 0 {
		t.Errorf("sent %d alerts for sessions nobody is waiting on", sink.count())
	}
}

// Running is not a reason to alert again.
func TestSweep_RepeatedRunsAlertOnce(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	sink := &countingSink{}
	seedKYCUser(t, d, "in_review", "sess-"+uuid.NewString(), 90*time.Minute)

	s := newTestSweeper(d, sink)
	for i := 0; i < 5; i++ {
		s.sweepOnce(ctx)
	}

	if sink.count() != 1 {
		t.Errorf("five sweeps sent %d alerts; at a ten-minute interval that is six an hour, forever", sink.count())
	}
}

// The webhook and the sweep are two paths to the same event, and both fire for
// a session the webhook handled: it is claimed, so the sweep is silent.
func TestSweep_DoesNotRepeatWhatTheWebhookAlreadyReported(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	sink := &countingSink{}

	session := "sess-" + uuid.NewString()
	userID := seedKYCUser(t, d, "in_review", session, 3*time.Hour)

	alertAdminOfKYCReview(ctx, d, sink, userID, session, "webhook")
	newTestSweeper(d, sink).sweepOnce(ctx)

	if sink.count() != 1 {
		t.Errorf("webhook plus sweep sent %d alerts for one review", sink.count())
	}
}

// The line this change does not cross.
//
// The sweep is an alerting backstop, not a retry rule. Letting a user
// re-attempt while a session is legitimately under review produces duplicate
// sessions, and if the flagged document flags again the second lands in the
// same queue - turning "stuck and visible" into "churning and invisible".
func TestSweep_NeverTouchesTheUsersVerificationState(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()

	session := "sess-" + uuid.NewString()
	userID := seedKYCUser(t, d, "in_review", session, 5*time.Hour)

	var before, after struct {
		status  string
		session *string
		updated time.Time
	}
	read := func(dst *struct {
		status  string
		session *string
		updated time.Time
	}) {
		if err := d.Pool.QueryRow(ctx,
			`SELECT kyc_status, kyc_session_id, updated_at FROM users WHERE id = $1`, userID,
		).Scan(&dst.status, &dst.session, &dst.updated); err != nil {
			t.Fatalf("read user: %v", err)
		}
	}

	read(&before)
	newTestSweeper(d, &countingSink{}).sweepOnce(ctx)
	read(&after)

	if before.status != after.status {
		t.Errorf("kyc_status changed %q -> %q; the sweep must not decide anything", before.status, after.status)
	}
	if (before.session == nil) != (after.session == nil) || (before.session != nil && *before.session != *after.session) {
		t.Error("kyc_session_id changed")
	}
	if !before.updated.Equal(after.updated) {
		t.Error("updated_at moved; the sweep wrote to the user row")
	}
}

// An unconfigured sink is the silent-failure case: the claim is taken, so
// nothing retries, and without a loud log nobody would ever know.
func TestSweep_UnconfiguredSinkIsLoudRatherThanSilent(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	session := "sess-" + uuid.NewString()
	userID := seedKYCUser(t, d, "in_review", session, 3*time.Hour)

	alertAdminOfKYCReview(ctx, d, &countingSink{}, userID, session, "sweep") // baseline: claim taken

	var n int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM kyc_review_alerts WHERE session_id = $1`, session).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("claim rows = %d, want 1", n)
	}
}
