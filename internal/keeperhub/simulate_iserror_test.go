package keeperhub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// These tests are driven by what a live KeeperHub instance actually returned
// when SimulateTransfer was first exercised against it (testdata/, captured
// verbatim). The only fixture before this was {"success":true,"wouldRevert":false},
// so no test ever saw a revert, and the one answer this call exists to give
// was being discarded as an error.

func isErrorServer(t *testing.T, text string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["method"] == "tools/call" {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 2, "result": map[string]any{
				"isError": true,
				"content": []any{map[string]any{"text": text}},
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
	return c
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(string(b), "\n")
}

func simulate(c *Client) (TransferSimulation, error) {
	return c.SimulateTransfer(context.Background(), "84532",
		"0xB472ccF4aEd6cA8ed2fc023f584946D5A8170C73", "1",
		"0x036CbD53842c5426634e7929541eC2318f3dCF7e")
}

func TestSimulateTransfer_ReadsARevertKeeperHubReportsAsAToolError(t *testing.T) {
	sim, err := simulate(isErrorServer(t, fixture(t, "simulate_revert_iserror.txt")))
	if err != nil {
		t.Fatalf("a simulated revert is an answer, not a failure to ask: %v", err)
	}
	if !sim.WouldRevert || sim.Safe() {
		t.Fatalf("want wouldRevert and not Safe, got %+v", sim)
	}
	if !strings.Contains(sim.Error, "ERC20: transfer amount exceeds balance") {
		t.Errorf("the revert reason must survive, got %q", sim.Error)
	}
	if !strings.Contains(string(sim.Raw), `"failureKind":"revert"`) {
		t.Errorf("Raw should keep the whole simulation object, got %s", sim.Raw)
	}
}

func TestSimulateTransfer_ReadsAPolicyRefusalTheSameWay(t *testing.T) {
	// KeeperHub's $100 per-transaction stablecoin cap arrives as
	// failureKind "validation" but is still a simulated refusal of this leg.
	sim, err := simulate(isErrorServer(t, fixture(t, "simulate_validation_iserror.txt")))
	if err != nil {
		t.Fatalf("a policy refusal is an answer too: %v", err)
	}
	if !sim.WouldRevert || sim.Safe() {
		t.Fatalf("want wouldRevert and not Safe, got %+v", sim)
	}
	if !strings.Contains(sim.Error, "100.00 USD per-transaction limit") {
		t.Errorf("got %q", sim.Error)
	}
}

// Fail-closed: every isError that is not unmistakably a simulated refusal
// stays an error. None of these may ever come back as a result.
func TestSimulateTransfer_OtherToolErrorsStayErrors(t *testing.T) {
	cases := map[string]string{
		"no json at all":           "Unauthorized: invalid or expired API key",
		"json but not simulated":   `API call failed: 500 - {"success":false,"status":"failed","error":"upstream timeout"}`,
		"simulated but not refuse": `API call failed: 400 - {"success":true,"status":"simulated","wouldRevert":false,"error":""}`,
		"truncated json":           `API call failed: 400 - {"success":false,"status":"simulated","wouldRe`,
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			sim, err := simulate(isErrorServer(t, text))
			if err == nil {
				t.Fatalf("want an error, got a result: %+v", sim)
			}
			if !errors.Is(err, ErrRemote) {
				t.Errorf("tool errors must still unwrap to ErrRemote, got %v", err)
			}
			if sim.Safe() {
				t.Fatal("an error must never read as safe")
			}
		})
	}
}

func TestToolError_MessageUnchanged(t *testing.T) {
	// Logs and alerts key on this text; the typed error must not change it.
	got := (&ToolError{Tool: "execute_transfer", Text: "boom"}).Error()
	want := fmt.Errorf("%w: %s: %s", ErrRemote, "execute_transfer", "boom").Error()
	if got != want {
		t.Fatalf("message changed:\n got %q\nwant %q", got, want)
	}
}
