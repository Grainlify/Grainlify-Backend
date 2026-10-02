package handlers

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
	"github.com/jagadeesh/grainlify/backend/internal/didit"
)

// Every path that reads a Didit decision - the /auth/kyc/status poll, the
// webhook, the reconciler - used to be one log line away from writing the
// identity record into the platform's log stream, and the poll did so at Info
// on every request. Logs on the platform cannot be selectively deleted, so the
// only defence is never writing them. This drives each path with a decision
// full of identity fields and asserts none of it reaches the log.

// kycPrivacyDecision is shaped like Didit's v2 decision response: the
// verification blocks sit at the top level (they land in ExtraFields) as well
// as under data/decision.
const kycPrivacyDecision = `{
  "status": "Approved",
  "decision": {"first_name": "Ada", "date_of_birth": "1815-12-10"},
  "data": {"document_number": "X1234567", "last_name": "Lovelace"},
  "id_verification": {
    "first_name": "Ada", "last_name": "Lovelace", "full_name": "Ada Lovelace",
    "date_of_birth": "1815-12-10", "document_number": "X1234567",
    "personal_number": "PN-99887766", "address": "12 St James's Square"
  }
}`

// The field names and the values. A value can turn up without its key (a
// formatted string, a fmt.Sprint of a map), so both are checked.
var kycPrivacyForbidden = []string{
	"document_number", "date_of_birth", "first_name", "last_name", "full_name",
	"personal_number", "id_verification",
	"X1234567", "1815-12-10", "Lovelace", "PN-99887766", "St James",
}

// lockedBuffer is written by slog from the handler's goroutine and read by the
// test, so it is guarded.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// captureSlog routes the default logger into a buffer at Debug, so a field
// demoted rather than removed would still be caught.
func captureSlog(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func assertNoKYCRecordInLogs(t *testing.T, logs string) {
	t.Helper()
	for _, s := range kycPrivacyForbidden {
		if strings.Contains(logs, s) {
			t.Errorf("log output contains %q - the Didit decision record must never be logged", s)
		}
	}
}

func kycPrivacyClient(status int, body string) *didit.Client {
	return &didit.Client{HTTP: &http.Client{Transport: diditTrustTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})}}
}

func kycPrivacyPoll(t *testing.T, client *didit.Client) (logs string, session string) {
	t.Helper()
	d := dbtest.DB(t)
	user, session := diditTrustUser(t, d, "pending")
	buf := captureSlog(t)

	h := &KYCHandler{cfg: config.Config{}, db: d, didit: client}
	app := fiber.New()
	app.Get("/auth/kyc/status", func(c *fiber.Ctx) error {
		c.Locals(auth.LocalUserID, user.String())
		return c.Next()
	}, h.Status())
	diditTrustDo(t, app, "GET", "/auth/kyc/status", nil, nil)
	return buf.String(), session
}

func TestKYCLogs_StatusPollDoesNotLogTheDecision(t *testing.T) {
	logs, session := kycPrivacyPoll(t, kycPrivacyClient(http.StatusOK, kycPrivacyDecision))
	if !strings.Contains(logs, session) {
		t.Fatalf("captured no log lines for session %s - the capture is not wired, so the check below proves nothing", session)
	}
	assertNoKYCRecordInLogs(t, logs)
}

// A decision body that does not parse comes back inside the client's error,
// and the poll logs that error.
func TestKYCLogs_UndecodableDecisionDoesNotLogTheBody(t *testing.T) {
	truncated := kycPrivacyDecision[:len(kycPrivacyDecision)-10]
	logs, session := kycPrivacyPoll(t, kycPrivacyClient(http.StatusOK, truncated))
	if !strings.Contains(logs, session) {
		t.Fatalf("captured no log lines for session %s", session)
	}
	assertNoKYCRecordInLogs(t, logs)
}

func TestKYCLogs_WebhookDoesNotLogTheDecision(t *testing.T) {
	d := dbtest.DB(t)
	const secret = "kyc-privacy-secret"
	_, session := diditTrustUser(t, d, "pending")
	buf := captureSlog(t)

	app := diditTrustApp(d, kycPrivacyClient(http.StatusOK, kycPrivacyDecision), config.Config{DiditWebhookSecret: secret})
	// Didit's own webhook body carries the decision too.
	body := []byte(fmt.Sprintf(`{"event":"status.updated","session_id":%q,"status":"Approved","decision":%s}`,
		session, kycPrivacyDecision))
	resp := diditTrustDo(t, app, "POST", "/webhooks/didit", body, diditTrustSigned(secret, body))
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("response = %d, want 200", resp.StatusCode)
	}
	assertNoKYCRecordInLogs(t, buf.String())
}

func TestKYCLogs_ReconcilerDoesNotLogTheDecision(t *testing.T) {
	d := dbtest.DB(t)
	session := "kyc-privacy-recon-" + uuid.NewString()
	user := reconcilerFxUser(t, d, "pending", &session)
	buf := captureSlog(t)

	decision, err := kycPrivacyClient(http.StatusOK, kycPrivacyDecision).GetSessionDecision(context.Background(), session)
	if err != nil {
		t.Fatalf("build decision: %v", err)
	}
	r := newTestReconciler(d, &fakeDecisions{bySession: map[string]didit.SessionDecisionResponse{session: decision}})
	// reconcileOne rather than reconcileOnce: the test database is shared and
	// never truncated, so this session is not guaranteed a place in a batch.
	r.reconcileOne(context.Background(), reconcileCandidate{userID: user, sessionID: session, stored: "pending"})

	logs := buf.String()
	if !strings.Contains(logs, session) {
		t.Fatalf("captured no log lines for session %s", session)
	}
	assertNoKYCRecordInLogs(t, logs)
}
