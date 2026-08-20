package handlers

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

func statusChangeFxUser(t *testing.T, d *db.DB, status string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := d.Pool.QueryRow(context.Background(), `
INSERT INTO users (role, display_name, github_user_id, kyc_status)
VALUES ('contributor', $1, $2, $3) RETURNING id
`, "kyc-change-"+uuid.NewString(), reconcilerFxNextGHID(), status).Scan(&id); err != nil {
		t.Fatalf("statusChangeFxUser: %v", err)
	}
	return id
}

// The constraint that bites: the reconciler re-observes the same 25 sessions
// every five minutes and writes unconditionally. If `changed` were derived from
// the write rather than from the value it replaced, every pass would look like
// a transition and the notifications page would fill with the same message.
func TestApplyKYCStatus_ReportsChangedOnlyOnARealTransition(t *testing.T) {
	d := dbtest.DB(t)
	user := statusChangeFxUser(t, d, "verified")

	prev, changed, err := applyKYCStatus(context.Background(), d, user, "rejected", []byte(`{}`))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if prev != "verified" || !changed {
		t.Fatalf("previous=%q changed=%v, want verified/true", prev, changed)
	}

	// Re-observing the same decision, which is what every subsequent pass does.
	for i := 0; i < 3; i++ {
		prev, changed, err = applyKYCStatus(context.Background(), d, user, "rejected", []byte(`{}`))
		if err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
		if changed {
			t.Fatalf("re-observing an unchanged status reported changed=true on pass %d; "+
				"every reconcile would notify", i)
		}
		if prev != "rejected" {
			t.Errorf("previous=%q, want rejected", prev)
		}
	}
}

// The previous value must come from the write, not from a read taken earlier.
// A caller holding a stale snapshot must still get the truth.
func TestApplyKYCStatus_PreviousComesFromTheWriteNotTheCaller(t *testing.T) {
	d := dbtest.DB(t)
	user := statusChangeFxUser(t, d, "pending")

	// Something else lands first - a webhook, say.
	if _, _, err := applyKYCStatus(context.Background(), d, user, "verified", []byte(`{}`)); err != nil {
		t.Fatalf("first: %v", err)
	}
	// A caller whose snapshot still says "pending" now writes "verified" too.
	prev, changed, err := applyKYCStatus(context.Background(), d, user, "verified", []byte(`{}`))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if changed {
		t.Error("reported a transition that had already happened; the contributor would be told twice")
	}
	if prev != "verified" {
		t.Errorf("previous=%q, want verified - the value must come from the row, not the caller", prev)
	}
}

// kyc_verified_at is stamped on the transition into verified and never
// re-dated. Re-asserted here because applyKYCStatus now owns that statement for
// both paths, and re-dating it is what gave all 37 founding members a
// verification time later than the wave it caused.
func TestApplyKYCStatus_DoesNotRedateVerification(t *testing.T) {
	d := dbtest.DB(t)
	user := statusChangeFxUser(t, d, "pending")

	if _, _, err := applyKYCStatus(context.Background(), d, user, "verified", []byte(`{}`)); err != nil {
		t.Fatalf("first: %v", err)
	}
	var first string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT kyc_verified_at::text FROM users WHERE id = $1`, user).Scan(&first); err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, _, err := applyKYCStatus(context.Background(), d, user, "verified", []byte(`{}`)); err != nil {
		t.Fatalf("second: %v", err)
	}
	var second string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT kyc_verified_at::text FROM users WHERE id = $1`, user).Scan(&second); err != nil {
		t.Fatalf("read: %v", err)
	}
	if first != second {
		t.Errorf("kyc_verified_at moved on re-observation: %s -> %s", first, second)
	}
}

// The copy must not dead-end. A refusal that names no route out is how somebody
// concludes the matter is closed and never asks.
func TestNoticeForKYCStatus_RefusalNamesBothRoutesOut(t *testing.T) {
	n, ok := noticeForKYCStatus("verified", "rejected")
	if !ok {
		t.Fatal("a refusal produced no notice")
	}
	for _, want := range []string{"Billing", "support"} {
		if !contains(n.Body, want) {
			t.Errorf("refusal copy does not mention %q - it dead-ends:\n%s", want, n.Body)
		}
	}
	// The reversal case says what happened, not only what to do next.
	if !contains(n.Body, "no longer approved") {
		t.Errorf("verified -> rejected does not say the approval was withdrawn:\n%s", n.Body)
	}
}

// Transient states are not news. Somebody who just opened the flow does not
// need telling that it started.
func TestNoticeForKYCStatus_SkipsTransientStates(t *testing.T) {
	for _, status := range []string{"pending", "not_started", "", "something_new"} {
		if _, ok := noticeForKYCStatus("", status); ok {
			t.Errorf("status %q produced a notification; it is transient or unknown", status)
		}
	}
	for _, status := range []string{"verified", "rejected", "in_review", "expired"} {
		if _, ok := noticeForKYCStatus("", status); !ok {
			t.Errorf("status %q produced no notification; it is a decision or a dead end", status)
		}
	}
}
