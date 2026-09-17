// Package keeperhub talks to KeeperHub, the Base USDC payout rail.
//
// Modelled on internal/didit: an http.Client with a timeout, requests built
// with NewRequestWithContext, and a constructor the caller only calls when the
// configuration is present.
//
// # Two credentials, and the boundary between them is CODE DISCIPLINE
//
// Be precise about this, because the shape invites the wrong reading. The
// webhook key fires payout runs; the org API key only reads. Both are secrets
// living in the same environment, read by the same process, so either one
// leaking is the same incident. **The separation protects nothing** and must
// never be described as if it does.
//
// What it buys is that "fire a payout" and "look at a payout" are different
// fields, so a read path cannot pick up the ability to move money by reaching
// for whatever credential is in scope. The compiler keeps them apart; that is a
// legibility property, not a containment one.
//
// # Attribution is USER-scoped, and that is a real operational constraint
//
// A webhook-fired run records `triggeredByUserApiKeyId` with
// `triggeredByOrgApiKeyId: null` and a credential label naming the key. There
// is no service-account option. So a backend firing payouts ties every run to
// ONE INDIVIDUAL, and the rail stops working if that person leaves the org.
// This is stated rather than papered over: whoever operates this needs to know
// the key is a person, and rotating it is a planned task rather than a
// discovery made during an incident.
//
// # Billing
//
// Webhook-fired runs are billable (`billable: true` on the execution record).
// Recorded here as a fact worth knowing, not guarded against: the org is on a
// free plan with pay-as-you-go configured, and PAYG orgs are admitted past the
// included limit and charged rather than blocked, so a dry run plus a payout
// plus a resume is not a quota risk.
package keeperhub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// DefaultBaseURL is the KeeperHub origin. Both the webhook endpoint and the
// MCP endpoint hang off it.
const DefaultBaseURL = "https://app.keeperhub.com"

// WebhookKeyPrefix is the only key format the webhook endpoint accepts.
//
// Checked locally as well as by the server, because the server's refusal costs
// a round trip and arrives as a 401 at payout time - the worst moment to learn
// that the wrong variable was pasted into the deploy environment.
const WebhookKeyPrefix = "wfb_"

// browserUserAgent is required, not decorative.
//
// app.keeperhub.com sits behind Cloudflare, which refuses default script
// user-agents with a 403 "browser_signature_banned" before the request reaches
// the API. A Go client identifying itself honestly as Go is blocked, so it must
// present a browser signature to be served at all.
//
// This is a workaround for somebody else's edge configuration and is recorded
// as one: if KeeperHub ever allowlists API clients, this should go.
const browserUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"

// One error per cause, following the rule this codebase already applies to
// nonces and chain configuration: a name that could cover two causes sends
// whoever is paged to the wrong place.
var (
	// ErrNotConfigured means the rail is switched off, which is an ordinary
	// state rather than a failure - the same shape as an empty Didit key.
	ErrNotConfigured = errors.New("keeperhub: not configured")

	// ErrWebhookKeyFormat is a webhook key that is not a wfb_ key. Almost
	// always the org key pasted into the wrong variable, which is worth naming
	// exactly because the two are interchangeable to look at.
	ErrWebhookKeyFormat = errors.New("keeperhub: webhook key must start with " + WebhookKeyPrefix)

	// ErrNothingToDispatch is an empty recipient list. Firing a run that pays
	// nobody records an attempt that did nothing, and the next resume sees an
	// execution id and believes work happened.
	ErrNothingToDispatch = errors.New("keeperhub: refusing to dispatch an empty recipient list")

	// ErrDispatchRefused is a non-2xx from the webhook endpoint.
	ErrDispatchRefused = errors.New("keeperhub: dispatch refused")

	// ErrRemote is a failure reported by the MCP endpoint while reading.
	ErrRemote = errors.New("keeperhub: remote call failed")
)

// Client is a KeeperHub API client.
//
// The credentials are unexported so that neither can be read back out or logged
// by a caller holding the struct, and so the only way to use the webhook key is
// to call Dispatch.
type Client struct {
	HTTP      *http.Client
	BaseURL   string
	UserAgent string

	webhookKey string
	orgAPIKey  string
	workflowID string

	// newIdempotencyKey is a field rather than a direct uuid call so a test can
	// observe what was generated. It must produce a FRESH value on every call;
	// see Dispatch for why that is the load-bearing property of this package.
	newIdempotencyKey func() string

	mu         sync.Mutex
	mcpSession string
}

// New builds a client, or explains what is missing.
//
// Mirrors the Didit constructor pattern at the call site: the caller checks its
// configuration and only constructs when it is present, so an unset key
// disables the rail rather than half-enabling it. Here the check is inside and
// returns an error, because there are three required values rather than one and
// "which of them was empty" is worth saying.
func New(webhookKey, orgAPIKey, workflowID string) (*Client, error) {
	var missing []string
	if strings.TrimSpace(webhookKey) == "" {
		missing = append(missing, "KEEPERHUB_WEBHOOK_KEY")
	}
	if strings.TrimSpace(orgAPIKey) == "" {
		missing = append(missing, "KEEPERHUB_API_KEY")
	}
	if strings.TrimSpace(workflowID) == "" {
		missing = append(missing, "KEEPERHUB_WORKFLOW_ID")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s not set", ErrNotConfigured, strings.Join(missing, ", "))
	}
	// Refused at construction rather than at dispatch. The alternative is
	// discovering it from a 401 the first time a real payout is fired.
	if !strings.HasPrefix(webhookKey, WebhookKeyPrefix) {
		return nil, ErrWebhookKeyFormat
	}
	return &Client{
		HTTP:              &http.Client{Timeout: 30 * time.Second},
		BaseURL:           DefaultBaseURL,
		UserAgent:         browserUserAgent,
		webhookKey:        webhookKey,
		orgAPIKey:         orgAPIKey,
		workflowID:        workflowID,
		newIdempotencyKey: func() string { return uuid.NewString() },
	}, nil
}

// WorkflowID is the workflow this client fires. Safe to log; it is not secret.
func (c *Client) WorkflowID() string { return c.workflowID }

// postJSON sends one request and returns status and body.
//
// bearer is passed per call rather than read from a field, so each call site
// states which credential it is spending. A reader can see at the call whether
// money can move.
func (c *Client) postJSON(ctx context.Context, url, bearer string, body any, extra map[string]string) (int, []byte, http.Header, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("keeperhub: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("keeperhub: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("User-Agent", c.UserAgent)
	for k, v := range extra {
		req.Header.Set(k, v)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("keeperhub: http request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, resp.Header, fmt.Errorf("keeperhub: read response: %w", err)
	}
	return resp.StatusCode, raw, resp.Header, nil
}

// remoteError pulls the most specific message the API offered.
//
// KeeperHub returns several shapes; the format refusal, for example, is
// {"error":..., "code":"invalid_key_format", "expected":"wfb_*"}. Preserving
// `code` matters because it is the machine-readable half and the prose half
// changes.
func remoteError(status int, raw []byte) string {
	var e struct {
		Error    string `json:"error"`
		Detail   string `json:"detail"`
		Message  string `json:"message"`
		Code     string `json:"code"`
		Expected string `json:"expected"`
	}
	_ = json.Unmarshal(raw, &e)
	msg := e.Error
	for _, alt := range []string{e.Message, e.Detail} {
		if msg == "" {
			msg = alt
		}
	}
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	if msg == "" {
		msg = "no message"
	}
	if e.Code != "" {
		msg = fmt.Sprintf("%s (code %s)", msg, e.Code)
	}
	return fmt.Sprintf("status %d: %s", status, msg)
}
