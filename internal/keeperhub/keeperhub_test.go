package keeperhub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These tests never reach the network and contain no real credentials. The key
// strings below are obvious placeholders; a real wfb_/kh_ key must never appear
// in a fixture, a log or this repository.
const (
	testWebhookKey = "wfb_placeholder_not_a_real_key"
	testOrgKey     = "kh_placeholder_not_a_real_key"
	testWorkflowID = "wf_probe"
)

// recorded is a capture of what the fake server saw, so a test can assert on
// the request rather than only on the reply.
type recorded struct {
	path            string
	authorization   string
	userAgent       string
	idempotencyKeys []string
	bodies          []map[string]any
}

// newServer stands up a fake KeeperHub serving both the webhook endpoint and
// the MCP endpoint, which is how the real origin is laid out.
func newServer(t *testing.T, dispatchStatus int, dispatchBody string, mcpText string) (*Client, *recorded) {
	t.Helper()
	rec := &recorded{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)

		switch {
		case strings.HasSuffix(r.URL.Path, "/webhook"):
			rec.path = r.URL.Path
			rec.authorization = r.Header.Get("Authorization")
			rec.userAgent = r.Header.Get("User-Agent")
			rec.idempotencyKeys = append(rec.idempotencyKeys, r.Header.Get("Idempotency-Key"))
			rec.bodies = append(rec.bodies, body)
			w.WriteHeader(dispatchStatus)
			fmt.Fprint(w, dispatchBody)

		case r.URL.Path == "/mcp":
			w.Header().Set("Mcp-Session-Id", "session-1")
			if body["method"] == "initialize" {
				fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05"}}`)
				return
			}
			if body["method"] == "notifications/initialized" {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			// A tools/call reply carries its payload as text inside content[0].
			env := map[string]any{"jsonrpc": "2.0", "id": 2, "result": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": mcpText}},
			}}
			_ = json.NewEncoder(w).Encode(env)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := New(testWebhookKey, testOrgKey, testWorkflowID)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.BaseURL = srv.URL
	return c, rec
}

func TestNew_NamesEveryMissingValue(t *testing.T) {
	_, err := New("", "", "")
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
	for _, want := range []string{"KEEPERHUB_WEBHOOK_KEY", "KEEPERHUB_API_KEY", "KEEPERHUB_WORKFLOW_ID"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %s: %v", want, err)
		}
	}
}

// The org key and the webhook key are interchangeable to look at, and pasting
// the wrong one into the deploy environment is the likeliest configuration
// mistake here. Caught at construction rather than as a 401 at payout time.
func TestNew_RefusesAKeyThatIsNotAWebhookKey(t *testing.T) {
	if _, err := New(testOrgKey, testOrgKey, testWorkflowID); !errors.Is(err, ErrWebhookKeyFormat) {
		t.Fatalf("err = %v, want ErrWebhookKeyFormat", err)
	}
}

func TestDispatch_SendsTheListAtTheTriggerTopLevel(t *testing.T) {
	c, rec := newServer(t, 200, `{"executionId":"exec-1","status":"running"}`, "")

	ack, err := c.Dispatch(context.Background(), []Recipient{
		{Address: "0x1111111111111111111111111111111111111111", AmountMinor: "111", LegID: "leg-1"},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if ack.ExecutionID != "exec-1" || ack.Status != "running" {
		t.Errorf("ack = %+v", ack)
	}
	if want := "/api/workflows/" + testWorkflowID + "/webhook"; rec.path != want {
		t.Errorf("path = %q, want %q", rec.path, want)
	}
	// The WEBHOOK key, never the org key: this is the call that moves money.
	if rec.authorization != "Bearer "+testWebhookKey {
		t.Errorf("dispatch did not use the webhook key")
	}
	// Cloudflare refuses default script user-agents outright.
	if !strings.HasPrefix(rec.userAgent, "Mozilla/") {
		t.Errorf("User-Agent = %q, want a browser signature", rec.userAgent)
	}
	// Top level, so the workflow reads {{@trigger-1:Trigger.recipients}}.
	list, ok := rec.bodies[0]["recipients"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("recipients not at the top level of the payload: %v", rec.bodies[0])
	}
	first := list[0].(map[string]any)
	if first["amountMinor"] != "111" {
		t.Errorf("amountMinor = %v, want the exact string \"111\"", first["amountMinor"])
	}
}

// The load-bearing property of this package.
//
// The endpoint dedupes on webhook:<workflowId> and replays the ORIGINAL
// executionId for a repeated key. A resume carries a shorter list of only the
// unpaid legs and must be a genuinely new execution - reuse a key across that
// boundary and the resume returns the failed run's id, pays nobody, and is
// recorded as an attempt that happened.
func TestDispatch_GeneratesAFreshIdempotencyKeyPerAttempt(t *testing.T) {
	c, rec := newServer(t, 200, `{"executionId":"exec-1","status":"running"}`, "")
	ctx := context.Background()

	full := []Recipient{
		{Address: "0x1111111111111111111111111111111111111111", AmountMinor: "111"},
		{Address: "0x2222222222222222222222222222222222222222", AmountMinor: "222"},
	}
	// The resume: only the leg that was not paid.
	resume := []Recipient{{Address: "0x2222222222222222222222222222222222222222", AmountMinor: "222"}}

	first, err := c.Dispatch(ctx, full)
	if err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	second, err := c.Dispatch(ctx, resume)
	if err != nil {
		t.Fatalf("resume dispatch: %v", err)
	}

	if len(rec.idempotencyKeys) != 2 {
		t.Fatalf("saw %d dispatches, want 2", len(rec.idempotencyKeys))
	}
	if rec.idempotencyKeys[0] == "" {
		t.Fatal("no Idempotency-Key was sent at all")
	}
	if rec.idempotencyKeys[0] == rec.idempotencyKeys[1] {
		t.Fatalf("the resume reused the first attempt's idempotency key (%s). KeeperHub "+
			"dedupes on webhook:<workflowId> and would replay the original execution, so "+
			"the resume would pay nobody while reporting success",
			rec.idempotencyKeys[0])
	}
	if first.IdempotencyKey == second.IdempotencyKey {
		t.Error("the ack reported the same key for two attempts")
	}
	// Reported back so the attempt can be recorded against the leg.
	if first.IdempotencyKey != rec.idempotencyKeys[0] {
		t.Error("the ack's key is not the one that was sent")
	}
}

// Dispatching nothing produces a real, billable execution that pays nobody, and
// a caller storing that id has recorded work that never existed.
func TestDispatch_RefusesAnEmptyList(t *testing.T) {
	c, rec := newServer(t, 200, `{"executionId":"exec-1","status":"running"}`, "")
	if _, err := c.Dispatch(context.Background(), nil); !errors.Is(err, ErrNothingToDispatch) {
		t.Fatalf("err = %v, want ErrNothingToDispatch", err)
	}
	if len(rec.idempotencyKeys) != 0 {
		t.Error("the empty dispatch was sent to KeeperHub anyway")
	}
}

func TestDispatch_RefusesASuccessWithNothingToPoll(t *testing.T) {
	c, _ := newServer(t, 200, `{"status":"running"}`, "")
	if _, err := c.Dispatch(context.Background(), []Recipient{{Address: "0x1", AmountMinor: "1"}}); !errors.Is(err, ErrDispatchRefused) {
		t.Fatalf("err = %v, want ErrDispatchRefused - a 200 with no executionId leaves "+
			"a run that cannot be followed", err)
	}
}

func TestDispatch_SurfacesTheKeyFormatRefusal(t *testing.T) {
	body := `{"error":"Invalid API key format. Expected a user webhook key starting with wfb_.","code":"invalid_key_format","expected":"wfb_*"}`
	c, _ := newServer(t, 401, body, "")
	_, err := c.Dispatch(context.Background(), []Recipient{{Address: "0x1", AmountMinor: "1"}})
	if !errors.Is(err, ErrDispatchRefused) {
		t.Fatalf("err = %v, want ErrDispatchRefused", err)
	}
	if !strings.Contains(err.Error(), "invalid_key_format") {
		t.Errorf("the machine-readable code was dropped: %v", err)
	}
}

// A real webhook-fired run, recorded. No credentials appear in it.
const recordedExecution = `{
 "logs": {
  "execution": {
   "id": "exec-1",
   "status": "success",
   "startedAt": "2026-09-16T09:38:31.839Z",
   "completedAt": "2026-09-16T09:38:32.739Z",
   "error": null,
   "triggerSource": "webhook",
   "triggeredByCredentialType": "webhook_key",
   "triggeredByUserApiKeyId": "u2a22b6t4ge28w6r7u97k",
   "triggeredByOrgApiKeyId": null,
   "triggeredByCredentialLabel": "wfb_HRFwtO2",
   "billable": true,
   "dispatchKey": null
  },
  "logs": [
   {"nodeId":"step-3","nodeName":"Collect","status":"success","iterationIndex":null,"forEachNodeId":"step-1","output":{"count":3}},
   {"nodeId":"step-2","nodeName":"Pay","status":"success","iterationIndex":0,"forEachNodeId":"step-1","output":{"leg":"a"}},
   {"nodeId":"step-2","nodeName":"Pay","status":"success","iterationIndex":1,"forEachNodeId":"step-1","output":{"leg":"b"}},
   {"nodeId":"step-2","nodeName":"Pay","status":"error","iterationIndex":2,"forEachNodeId":"step-1","error":"insufficient balance","output":null}
  ]
 }
}`

func TestExecution_ReadsPerLegResultsAndWebhookEvidence(t *testing.T) {
	c, _ := newServer(t, 200, "", recordedExecution)

	ex, err := c.Execution(context.Background(), "exec-1")
	if err != nil {
		t.Fatalf("Execution: %v", err)
	}

	// A Collect entry carries forEachNodeId but no iterationIndex and holds the
	// aggregate of the whole loop. Counting it would add a phantom leg.
	if len(ex.Legs) != 3 {
		t.Fatalf("legs = %d, want 3 - the Collect entry is not a leg", len(ex.Legs))
	}
	if ex.Legs[0].IterationIndex != 0 || ex.Legs[2].IterationIndex != 2 {
		t.Errorf("iteration indices not preserved: %+v", ex.Legs)
	}
	// A run can succeed overall while a leg inside it failed. That is the whole
	// reason per-leg results are read rather than the run status alone.
	if ex.Legs[2].Status != "error" || ex.Legs[2].Error != "insufficient balance" {
		t.Errorf("the failed leg was not surfaced: %+v", ex.Legs[2])
	}
	if !ex.Succeeded() {
		t.Error("run status should be success")
	}

	if ex.TriggerSource != "webhook" || ex.CredentialType != "webhook_key" {
		t.Errorf("webhook evidence missing: source=%q type=%q", ex.TriggerSource, ex.CredentialType)
	}
	// Attribution is user-scoped: the rail is tied to one individual.
	if ex.UserAPIKeyID == "" || ex.OrgAPIKeyID != "" {
		t.Errorf("attribution = user %q org %q, want a user key and no org key",
			ex.UserAPIKeyID, ex.OrgAPIKeyID)
	}
	if !ex.Billable {
		t.Error("webhook-fired runs are billable")
	}
	if ex.StartedAt == nil || ex.CompletedAt == nil {
		t.Fatal("timestamps not parsed")
	}
	if !ex.CompletedAt.After(*ex.StartedAt) {
		t.Error("completedAt should be after startedAt")
	}
}

// "running" is an acknowledgement. The run in the probe finished ~0.9s after
// the response returned, and nothing in the envelope said so.
func TestExecution_RunningIsNotTerminal(t *testing.T) {
	c, _ := newServer(t, 200, `{"executionId":"exec-1","status":"running"}`,
		`{"logs":{"execution":{"id":"exec-1","status":"running"},"logs":[]}}`)

	ack, err := c.Dispatch(context.Background(), []Recipient{{Address: "0x1", AmountMinor: "1"}})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if ack.Status != "running" {
		t.Fatalf("status = %q", ack.Status)
	}
	ex, err := c.Execution(context.Background(), ack.ExecutionID)
	if err != nil {
		t.Fatalf("Execution: %v", err)
	}
	if ex.Terminal() {
		t.Error("a running execution reported itself terminal")
	}
	if ex.Succeeded() {
		t.Error("a running execution reported itself successful - nothing may infer " +
			"an outcome from a dispatch acknowledgement")
	}
}

func TestExecution_TerminalStates(t *testing.T) {
	for status, want := range map[string]bool{
		"running": false, "pending": false,
		"success": true, "error": true, "system_error": true,
		"external_error": true, "cancelled": true,
	} {
		if got := (Execution{Status: status}).Terminal(); got != want {
			t.Errorf("Terminal(%q) = %v, want %v", status, got, want)
		}
	}
}

// A simulation can run successfully and still report that the transfer would
// revert. Checking one of the three is how that gets missed.
func TestSimulation_SafeRequiresAllThreeConditions(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    TransferSimulation
		want bool
	}{
		{"clean", TransferSimulation{Success: true}, true},
		{"would revert", TransferSimulation{Success: true, WouldRevert: true}, false},
		{"did not run", TransferSimulation{}, false},
		{"succeeded with an error", TransferSimulation{Success: true, Error: "nope"}, false},
	} {
		if got := tc.s.Safe(); got != tc.want {
			t.Errorf("%s: Safe() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSimulateTransfer_SendsABooleanAndUsesTheOrgKey(t *testing.T) {
	var seenAuth string
	var seenArgs map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		seenAuth = r.Header.Get("Authorization")
		if body["method"] == "tools/call" {
			p := body["params"].(map[string]any)
			seenArgs = p["arguments"].(map[string]any)
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 2, "result": map[string]any{
				"content": []any{map[string]any{"text": `{"success":true,"wouldRevert":false}`}},
			}})
			return
		}
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(srv.Close)

	c, err := New(testWebhookKey, testOrgKey, testWorkflowID)
	if err != nil {
		t.Fatal(err)
	}
	c.BaseURL = srv.URL

	sim, err := c.SimulateTransfer(context.Background(), "8453",
		"0x1111111111111111111111111111111111111111", "1.5",
		"0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913")
	if err != nil {
		t.Fatalf("SimulateTransfer: %v", err)
	}
	if !sim.Safe() {
		t.Error("a clean simulation should be safe")
	}
	// Reading, so the ORG key - never the webhook key.
	if seenAuth != "Bearer "+testOrgKey {
		t.Error("simulation must spend the org key, not the webhook key")
	}
	// A stringified "true" is rejected by the API, and the failure mode of
	// getting this wrong is a call that broadcasts instead of simulating.
	if v, ok := seenArgs["simulate"].(bool); !ok || !v {
		t.Errorf("simulate = %#v, want the JSON boolean true", seenArgs["simulate"])
	}
	// An idempotency key here would dedupe a later real transfer against a dry run.
	if _, present := seenArgs["idempotency_key"]; present {
		t.Error("a simulation must not carry an idempotency key")
	}
}
