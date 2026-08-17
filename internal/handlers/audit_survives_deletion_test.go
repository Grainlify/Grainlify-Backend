package handlers_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// An audit row must outlive the people it is about.
//
// Two defects these pin, both found by enumerating every foreign key into
// users(id) and neither ever fired, because nothing has ever deleted a user:
//
//   - kyc_reset_audit.actor_user_id was NOT NULL *and* ON DELETE SET NULL,
//     which cannot hold. The delete raised a not-null violation instead of
//     anonymising, so any admin who had performed a reset was undeletable.
//   - Both audit tables CASCADEd on their SUBJECT, so deleting a user
//     destroyed the record of decisions made about them - the audit was
//     complete right up to the moment it would first have been needed.
//
// Written against a real DELETE rather than against information_schema. The
// constraint metadata was self-contradictory and still looked correct in a
// catalogue query; only executing the delete showed which way it resolved.

// deleteUser issues the raw delete these constraints govern. Deliberately not
// routed through any handler: there is no deletion endpoint, and the point is
// what the SCHEMA does when something bypasses the product.
func deleteUser(t *testing.T, id uuid.UUID) {
	t.Helper()
	d := testDB(t)
	if _, err := d.Pool.Exec(t.Context(), `DELETE FROM users WHERE id = $1`, id); err != nil {
		t.Fatalf("DELETE FROM users: %v", err)
	}
}

// seedKYCResetAudit writes one audit row and returns its id.
//
// Returning the id is the point. The first version of these tests inserted a
// FIXED reason string and read the row back with `WHERE reason = $1`. That
// passes on a clean database and rots on a shared one: every run leaves a row
// behind, so by the third run four rows shared the reason, QueryRow returned
// an arbitrary one - typically an earlier run's, already nulled by an earlier
// deletion - and the assertions failed against a row this test never wrote.
//
// It failed intermittently, which is worse than failing: two green runs and
// one red, with no code change between them. Keyed on the primary key it is
// exact, and t.Cleanup keeps the table from growing regardless.
func seedKYCResetAudit(t *testing.T, subject, actor uuid.UUID, prevStatus, reasonCode string) uuid.UUID {
	t.Helper()
	d := testDB(t)
	var id uuid.UUID
	if err := d.Pool.QueryRow(t.Context(), `
INSERT INTO kyc_reset_audit (subject_user_id, previous_status, actor_user_id, reason, reason_code)
VALUES ($1, $2, $3, $4, $5)
RETURNING id
`, subject, prevStatus, actor, "audit survival probe "+uuid.NewString(), reasonCode).Scan(&id); err != nil {
		t.Fatalf("seed audit row: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Pool.Exec(context.WithoutCancel(t.Context()), `DELETE FROM kyc_reset_audit WHERE id = $1`, id)
	})
	return id
}

func TestAuditSurvives_DeletingItsSubject(t *testing.T) {
	d := testDB(t)
	actor := leaderboardSuiteUser(t, d.Pool)
	subject := leaderboardSuiteUser(t, d.Pool)

	auditID := seedKYCResetAudit(t, subject, actor, "rejected", "document_unreadable")

	deleteUser(t, subject)

	// The row is still there, and still says what happened and why.
	var gotReason, gotCode, gotPrev string
	var gotSubject *uuid.UUID
	var gotActor *uuid.UUID
	if err := d.Pool.QueryRow(t.Context(), `
SELECT reason, reason_code, previous_status, subject_user_id, actor_user_id
FROM kyc_reset_audit WHERE id = $1
`, auditID).Scan(&gotReason, &gotCode, &gotPrev, &gotSubject, &gotActor); err != nil {
		t.Fatalf("the audit row was destroyed by deleting its subject - which is the event it exists to record: %v", err)
	}
	if gotSubject != nil {
		t.Errorf("subject_user_id = %v, want NULL after the subject was deleted", gotSubject)
	}
	if gotActor == nil || *gotActor != actor {
		t.Errorf("actor_user_id = %v, want the actor preserved (%v) - only the SUBJECT was deleted", gotActor, actor)
	}
	if gotCode != "document_unreadable" || gotPrev != "rejected" {
		t.Errorf("the record lost its content: code=%q previous_status=%q", gotCode, gotPrev)
	}
}

// The contradiction: this delete used to fail outright with a not-null
// violation, making every admin who had ever reset somebody undeletable.
func TestAuditSurvives_DeletingTheActorWhoPerformedIt(t *testing.T) {
	d := testDB(t)
	actor := leaderboardSuiteUser(t, d.Pool)
	subject := leaderboardSuiteUser(t, d.Pool)

	auditID := seedKYCResetAudit(t, subject, actor, "in_review", "session_expired")

	// Before the fix this line failed: null value in column "actor_user_id"
	// violates not-null constraint.
	deleteUser(t, actor)

	var gotActor, gotSubject *uuid.UUID
	var gotReason string
	if err := d.Pool.QueryRow(t.Context(), `
SELECT actor_user_id, subject_user_id, reason FROM kyc_reset_audit WHERE id = $1
`, auditID).Scan(&gotActor, &gotSubject, &gotReason); err != nil {
		t.Fatalf("audit row missing after deleting its actor: %v", err)
	}
	if gotActor != nil {
		t.Errorf("actor_user_id = %v, want NULL", gotActor)
	}
	if gotSubject == nil || *gotSubject != subject {
		t.Errorf("subject_user_id = %v, want the subject preserved (%v)", gotSubject, subject)
	}
	if gotReason == "" {
		t.Error("reason was cleared; the record lost the thing it exists to hold")
	}
}

// The same property for the role audit, which had the same cascade.
func TestAdminRoleAuditSurvives_DeletingItsSubject(t *testing.T) {
	d := testDB(t)
	actor := leaderboardSuiteUser(t, d.Pool)
	subject := leaderboardSuiteUser(t, d.Pool)

	if _, err := d.Pool.Exec(t.Context(), `
INSERT INTO admin_role_audit (subject_user_id, actor_user_id, old_role, new_role, source)
VALUES ($1, $2, 'contributor', 'admin', 'admin_action')
`, subject, actor); err != nil {
		t.Fatalf("seed role audit: %v", err)
	}

	deleteUser(t, subject)

	var n int
	if err := d.Pool.QueryRow(t.Context(), `
SELECT count(*) FROM admin_role_audit WHERE actor_user_id = $1 AND new_role = 'admin'
`, actor).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 1 {
		t.Errorf("found %d role-audit rows, want 1 - a promotion to admin must remain on record "+
			"after the promoted account is gone", n)
	}
}
