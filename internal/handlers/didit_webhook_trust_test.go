package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
	"github.com/jagadeesh/grainlify/backend/internal/didit"
)

// The GET callback is a browser redirect: whoever holds the link chooses its
// query string. These tests pin down that the status in that query string is
// never what decides somebody's KYC status - the decision fetched from
// Didit's API is, and when that fetch cannot happen nothing changes.
//
// Verification is what unlocks GrainHack and Founding Pool payouts, so a
// fallback that applies "status=Approved" from the URL whenever Didit is slow
// or unconfigured is a fail-open on a payment gate.

// diditTrustTransport stands in for the network under a real *didit.Client.
// The handler holds the concrete client and didit.BaseURL is a package const,
// so the transport is the one seam that reaches it without changing the type.
type diditTrustTransport func(*http.Request) (*http.Response, error)

func (f diditTrustTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// diditTrustUnreachable is a client whose every call fails as a network
// outage would.
func diditTrustUnreachable() *didit.Client {
	return &didit.Client{HTTP: &http.Client{Transport: diditTrustTransport(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: connection refused")
	})}}
}

// diditTrustDeciding is a client whose decision endpoint answers status.
func diditTrustDeciding(status string) *didit.Client {
	return &didit.Client{HTTP: &http.Client{Transport: diditTrustTransport(func(r *http.Request) (*http.Response, error) {
		body := fmt.Sprintf(`{"status":%q,"decision":{},"data":{}}`, status)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})}}
}

func diditTrustApp(d *db.DB, client *didit.Client, cfg config.Config) *fiber.App {
	h := &DiditWebhookHandler{cfg: cfg, db: d, didit: client}
	app := fiber.New()
	app.Get("/webhooks/didit", h.Receive())
	app.Post("/webhooks/didit", h.Receive())
	return app
}

func diditTrustDo(t *testing.T, app *fiber.App, method, target string, body []byte, headers map[string]string) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = strings.NewReader(string(body))
	}
	req := httptest.NewRequest(method, target, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func diditTrustSigned(secret string, body []byte) map[string]string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return map[string]string{
		"X-Signature": hex.EncodeToString(mac.Sum(nil)),
		"X-Timestamp": strconv.FormatInt(time.Now().Unix(), 10),
	}
}

func diditTrustUser(t *testing.T, d *db.DB, status string) (uuid.UUID, string) {
	t.Helper()
	session := "didit-trust-" + uuid.NewString()
	return reconcilerFxUser(t, d, status, &session), session
}

func TestDiditCallbackGET_URLStatusIsNotTrustedWhenDiditIsUnreachable(t *testing.T) {
	d := dbtest.DB(t)
	user, session := diditTrustUser(t, d, "pending")
	app := diditTrustApp(d, diditTrustUnreachable(), config.Config{FrontendBaseURL: "https://app.example.com"})

	resp := diditTrustDo(t, app, "GET", "/webhooks/didit?verificationSessionId="+session+"&status=Approved", nil, nil)

	if got, _ := readKYC(t, d, user); got != "pending" {
		t.Fatalf("kyc_status = %q, want unchanged %q - a status in the callback URL must never be applied", got, "pending")
	}
	// The browser still has to land somewhere: the same redirect back to the
	// app, which then polls /auth/kyc/status for the real answer.
	if resp.StatusCode != fiber.StatusFound {
		t.Fatalf("response = %d, want %d redirect back to the app", resp.StatusCode, fiber.StatusFound)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "https://app.example.com?") {
		t.Errorf("Location = %q, want a redirect to the frontend", loc)
	}
}

func TestDiditCallbackGET_NoDiditClientChangesNothing(t *testing.T) {
	d := dbtest.DB(t)
	user, session := diditTrustUser(t, d, "pending")
	app := diditTrustApp(d, nil, config.Config{})

	diditTrustDo(t, app, "GET", "/webhooks/didit?verificationSessionId="+session+"&status=Approved", nil, nil)

	if got, _ := readKYC(t, d, user); got != "pending" {
		t.Fatalf("kyc_status = %q, want unchanged %q with no Didit client configured", got, "pending")
	}
}

func TestDiditCallbackGET_UsesTheAPIDecisionNotTheQueryStatus(t *testing.T) {
	d := dbtest.DB(t)
	user, session := diditTrustUser(t, d, "pending")
	app := diditTrustApp(d, diditTrustDeciding("Declined"), config.Config{})

	diditTrustDo(t, app, "GET", "/webhooks/didit?verificationSessionId="+session+"&status=Approved", nil, nil)

	if got, _ := readKYC(t, d, user); got != "rejected" {
		t.Fatalf("kyc_status = %q, want %q from Didit's API - the query said Approved", got, "rejected")
	}
}

func TestDiditWebhookPOST_SignedButDiditUnreachableChangesNothing(t *testing.T) {
	d := dbtest.DB(t)
	const secret = "didit-trust-secret"
	user, session := diditTrustUser(t, d, "pending")
	app := diditTrustApp(d, diditTrustUnreachable(), config.Config{DiditWebhookSecret: secret})

	body := []byte(fmt.Sprintf(`{"event":"status.updated","session_id":%q,"status":"Approved"}`, session))
	resp := diditTrustDo(t, app, "POST", "/webhooks/didit", body, diditTrustSigned(secret, body))

	if got, _ := readKYC(t, d, user); got != "pending" {
		t.Fatalf("kyc_status = %q, want unchanged %q when Didit's API cannot be reached", got, "pending")
	}
	// Non-2xx so Didit redelivers; the reconciler is the backstop after that.
	if resp.StatusCode < 500 {
		t.Errorf("response = %d, want a 5xx so Didit retries the delivery", resp.StatusCode)
	}
}

func TestDiditWebhookPOST_SignedWithNoDiditClientChangesNothing(t *testing.T) {
	d := dbtest.DB(t)
	const secret = "didit-trust-secret"
	user, session := diditTrustUser(t, d, "pending")
	app := diditTrustApp(d, nil, config.Config{DiditWebhookSecret: secret})

	body := []byte(fmt.Sprintf(`{"event":"status.updated","session_id":%q,"status":"Approved"}`, session))
	diditTrustDo(t, app, "POST", "/webhooks/didit", body, diditTrustSigned(secret, body))

	if got, _ := readKYC(t, d, user); got != "pending" {
		t.Fatalf("kyc_status = %q, want unchanged %q with no Didit client configured", got, "pending")
	}
}

func TestDiditWebhookPOST_SignedUsesTheAPIDecisionNotTheBodyStatus(t *testing.T) {
	d := dbtest.DB(t)
	const secret = "didit-trust-secret"
	user, session := diditTrustUser(t, d, "pending")
	app := diditTrustApp(d, diditTrustDeciding("Approved"), config.Config{DiditWebhookSecret: secret})

	body := []byte(fmt.Sprintf(`{"event":"status.updated","session_id":%q,"status":"Declined"}`, session))
	resp := diditTrustDo(t, app, "POST", "/webhooks/didit", body, diditTrustSigned(secret, body))

	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("response = %d, want 200", resp.StatusCode)
	}
	if got, _ := readKYC(t, d, user); got != "verified" {
		t.Fatalf("kyc_status = %q, want %q from Didit's API", got, "verified")
	}
	var verifiedAtSet bool
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT kyc_verified_at IS NOT NULL FROM users WHERE id = $1`, user).Scan(&verifiedAtSet); err != nil {
		t.Fatalf("read kyc_verified_at: %v", err)
	}
	if !verifiedAtSet {
		t.Error("kyc_verified_at = nil, want a timestamp once Didit says approved")
	}
}
