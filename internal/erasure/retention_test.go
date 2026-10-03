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
