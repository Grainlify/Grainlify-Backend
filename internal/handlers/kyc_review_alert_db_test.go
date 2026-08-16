package handlers

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// countingSink records deliveries instead of sending them.
type countingSink struct {
	mu       sync.Mutex
	messages []string
	fail     bool
}

func (s *countingSink) Name() string                 { return "telegram" }
func (s *countingSink) Configured() bool             { return true }
func (s *countingSink) Handles(category string) bool { return true }
func (s *countingSink) Deliver(ctx context.Context, r SupportRequest) (SupportDeliveryResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, r.Message)
	if s.fail {
		return SupportDeliveryResult{}, context.DeadlineExceeded
	}
	return SupportDeliveryResult{}, nil
}
func (s *countingSink) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.messages) }

// One alert per session, whatever arrives and however often.
//
// A notification that repeats gets muted, and a muted alert is the unwatched
// queue this change exists to remove. The claim is a primary key rather than a
// check-then-write, so a webhook and a sweep racing on the same session cannot
// both win.
func TestAlertAdminOfKYCReview_SendsOncePerSession(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()

	var userID uuid.UUID
	if err := d.Pool.QueryRow(ctx, `
INSERT INTO users (role, display_name, github_user_id) VALUES ('contributor','alert-test',$1) RETURNING id
`, int64(uuid.New().ID())).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	session := "sess-" + uuid.NewString()
	sink := &countingSink{}

	alertAdminOfKYCReview(ctx, d, sink, userID, session, "webhook")
	if sink.count() != 1 {
		t.Fatalf("first alert sent %d messages, want 1", sink.count())
	}

	// A redelivery from Didit, and the sweep finding the same row.
	alertAdminOfKYCReview(ctx, d, sink, userID, session, "webhook")
	alertAdminOfKYCReview(ctx, d, sink, userID, session, "sweep")
	if sink.count() != 1 {
		t.Errorf("session alerted %d times; repeated alerts get muted", sink.count())
	}

	// A genuinely different session is a genuinely different review.
	alertAdminOfKYCReview(ctx, d, sink, userID, "sess-"+uuid.NewString(), "webhook")
	if sink.count() != 2 {
		t.Errorf("a second session produced %d total alerts, want 2 - keying on the user would silence it", sink.count())
	}

	var source string
	if err := d.Pool.QueryRow(ctx, `SELECT source FROM kyc_review_alerts WHERE session_id = $1`, session).Scan(&source); err != nil {
		t.Fatalf("no claim row: %v", err)
	}
	if source != "webhook" {
		t.Errorf("source = %q, want the winner recorded", source)
	}
}

// Concurrency is the normal case: the webhook and a sweep tick can land
// together. Check-then-write would let both through.
func TestAlertAdminOfKYCReview_ConcurrentCallersProduceOneMessage(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()

	var userID uuid.UUID
	if err := d.Pool.QueryRow(ctx, `
INSERT INTO users (role, display_name, github_user_id) VALUES ('contributor','race-test',$1) RETURNING id
`, int64(uuid.New().ID())).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	session := "sess-" + uuid.NewString()
	sink := &countingSink{}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); alertAdminOfKYCReview(ctx, d, sink, userID, session, "webhook") }()
	}
	wg.Wait()

	if sink.count() != 1 {
		t.Errorf("8 concurrent callers sent %d messages, want exactly 1", sink.count())
	}
}

// A failed send keeps the claim.
//
// Releasing it would turn one delivery failure into an alert that retries on
// every sweep - which is the repeating notification the design is avoiding.
// The failure is logged loudly instead.
func TestAlertAdminOfKYCReview_FailedSendDoesNotRetryForever(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()

	var userID uuid.UUID
	if err := d.Pool.QueryRow(ctx, `
INSERT INTO users (role, display_name, github_user_id) VALUES ('contributor','fail-test',$1) RETURNING id
`, int64(uuid.New().ID())).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	session := "sess-" + uuid.NewString()
	sink := &countingSink{fail: true}

	alertAdminOfKYCReview(ctx, d, sink, userID, session, "webhook")
	alertAdminOfKYCReview(ctx, d, sink, userID, session, "sweep")

	if sink.count() != 1 {
		t.Errorf("a failed send was attempted %d times; it must not repeat every sweep", sink.count())
	}
}
