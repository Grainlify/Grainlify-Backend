package notifications_test

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
	"github.com/jagadeesh/grainlify/backend/internal/useremail"
)

type recordingMailer struct {
	mu   sync.Mutex
	sent []string // addresses
}

func (m *recordingMailer) Send(_ context.Context, to, _, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, to)
	return nil
}

func (m *recordingMailer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

func userWithEmail(t *testing.T, d *db.DB, address string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := d.Pool.Exec(context.Background(),
		`INSERT INTO users (id, role) VALUES ($1,'contributor')`, id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d.Pool.Exec(context.Background(), `DELETE FROM notifications WHERE user_id=$1`, id)
		d.Pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, id)
	})
	if address != "" {
		if _, err := useremail.Capture(context.Background(), d.Pool, id, address); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// The address finally being on file is the whole point: every notification
// email this product has ever tried to send was skipped for want of one.
func TestNotify_EmailsTheStoredAddress(t *testing.T) {
	d := dbtest.DB(t)
	m := &recordingMailer{}
	s := notifications.New(d, m, "https://grainlify.com")
	uid := userWithEmail(t, d, "ada@example.com")

	s.Notify(context.Background(), uid, notifications.TypeBountyDrawWon,
		"You won the draw", "The 25 USDC bounty is yours.", notifications.BountiesLink())

	if m.count() != 1 {
		t.Fatalf("sent %d emails, want 1", m.count())
	}
	if m.sent[0] != "ada@example.com" {
		t.Errorf("sent to %q", m.sent[0])
	}
}

// The master switch means "no email from Grainlify at all", and it is read
// inside sendEmail rather than at the call site so a new caller cannot forget
// it. The in-app notification must still be stored: turning email off is not
// turning notifications off.
func TestNotify_MasterSwitchStopsEmailButNotInApp(t *testing.T) {
	d := dbtest.DB(t)
	m := &recordingMailer{}
	s := notifications.New(d, m, "https://grainlify.com")
	uid := userWithEmail(t, d, "ada@example.com")

	if err := useremail.SetEnabled(context.Background(), d.Pool, uid, false); err != nil {
		t.Fatal(err)
	}
	s.Notify(context.Background(), uid, notifications.TypeBountyPaid,
		"You've been paid", "25 USDC has been sent.", notifications.BountiesLink())

	if m.count() != 0 {
		t.Errorf("sent %d emails with the master switch off", m.count())
	}
	var inApp int
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM notifications WHERE user_id = $1`, uid).Scan(&inApp); err != nil {
		t.Fatal(err)
	}
	if inApp != 1 {
		t.Errorf("stored %d in-app notifications, want 1 - turning email off must not turn notifications off", inApp)
	}
}

// Somebody who removed their address gets no email and no error.
func TestNotify_NoAddressIsQuiet(t *testing.T) {
	d := dbtest.DB(t)
	m := &recordingMailer{}
	s := notifications.New(d, m, "https://grainlify.com")
	uid := userWithEmail(t, d, "ada@example.com")

	if err := useremail.Remove(context.Background(), d.Pool, uid); err != nil {
		t.Fatal(err)
	}
	s.Notify(context.Background(), uid, notifications.TypeBountyDrawLost,
		"The draw went to someone else", "Applying again costs you nothing.", notifications.BountiesLink())

	if m.count() != 0 {
		t.Errorf("sent %d emails after the address was removed", m.count())
	}
}
