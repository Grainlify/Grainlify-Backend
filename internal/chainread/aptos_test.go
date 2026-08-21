package chainread

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

const mod = "0x1b419fe2b8c2a694eda8398af4bb6f6980915f9e3ed856b3b0fb4f26597f22c9"

func fakeNode(t *testing.T, status int, body string, capture *string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture != nil {
			b := make([]byte, r.ContentLength)
			r.Body.Read(b)
			*capture = string(b)
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL)
}

func TestClaimDeadline_ParsesAQuotedU64(t *testing.T) {
	// Move u64 comes back as a JSON STRING. 2^64 does not fit a float64, so a
	// node returning a bare number would itself be the anomaly.
	c := fakeNode(t, 200, `["1836345600"]`, nil)
	got, err := c.ClaimDeadline(context.Background(), mod, "0xesc")
	if err != nil {
		t.Fatal(err)
	}
	if got != 1836345600 {
		t.Fatalf("deadline = %d", got)
	}
}

func TestClaimDeadline_RefusesAnUnquotedNumber(t *testing.T) {
	c := fakeNode(t, 200, `[1836345600]`, nil)
	if _, err := c.ClaimDeadline(context.Background(), mod, "0xesc"); !errors.Is(err, ErrBadReply) {
		t.Fatalf("an unquoted u64 was accepted: %v", err)
	}
}

// A u64 beyond float64's exact range must survive. This is the reason the node
// quotes it, and the reason widening to a float would be the wrong "fix".
func TestClaimDeadline_SurvivesAValueFloat64WouldRound(t *testing.T) {
	c := fakeNode(t, 200, `["9007199254740993"]`, nil) // 2^53 + 1
	got, err := c.ClaimDeadline(context.Background(), mod, "0xesc")
	if err != nil {
		t.Fatal(err)
	}
	if got != 9007199254740993 {
		t.Fatalf("deadline = %d; a float64 round trip would give 9007199254740992", got)
	}
}

func TestIsClaimed_ReadsABool(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{{`[true]`, true}, {`[false]`, false}} {
		c := fakeNode(t, 200, tc.body, nil)
		got, err := c.IsClaimed(context.Background(), mod, "0xesc", "0xleaf")
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("IsClaimed(%s) = %v", tc.body, got)
		}
	}
}

// The upstream status and a bounded body must survive a NODE failure, or a
// precise upstream error becomes "the node said no".
//
// This used to assert ErrNodeFailed against a Move abort body, which encoded the
// very conflation that was later split out: an abort is the contract answering,
// not the node failing. The abort case is TestView_AMoveAbortIsNotANodeFailure.
func TestView_KeepsTheUpstreamStatusAndBody(t *testing.T) {
	c := fakeNode(t, 502, `{"message":"bad gateway from upstream pool"}`, nil)
	_, err := c.IsClaimed(context.Background(), mod, "0xesc", "0xleaf")
	if !errors.Is(err, ErrNodeFailed) {
		t.Fatalf("want ErrNodeFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), "bad gateway") {
		t.Errorf("the upstream status and body were discarded: %v", err)
	}
}

func TestView_SendsTheFunctionAndArguments(t *testing.T) {
	var sent string
	c := fakeNode(t, 200, `[false]`, &sent)
	if _, err := c.IsClaimed(context.Background(), mod, "0xesc", "0xleaf"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"::escrow::is_claimed", "0xesc", "0xleaf"} {
		if !strings.Contains(sent, want) {
			t.Errorf("request body missing %q: %s", want, sent)
		}
	}
}

// An unset endpoint variable must be an error naming it, never a fallback to a
// public node - which would work in development and mislead in production.
func TestEndpointFor_RefusesRatherThanFallingBack(t *testing.T) {
	if _, err := EndpointFor(""); !errors.Is(err, ErrNoEndpoint) {
		t.Errorf("empty ref: %v", err)
	}
	if _, err := EndpointFor("DEFINITELY_NOT_SET_ANYWHERE"); !errors.Is(err, ErrNoEndpoint) {
		t.Errorf("unset var: %v", err)
	} else if !strings.Contains(err.Error(), "DEFINITELY_NOT_SET_ANYWHERE") {
		t.Errorf("the error does not name the variable: %v", err)
	}
	os.Setenv("CHAINREAD_TEST_URL", "https://node.example")
	defer os.Unsetenv("CHAINREAD_TEST_URL")
	if got, err := EndpointFor("CHAINREAD_TEST_URL"); err != nil || got != "https://node.example" {
		t.Errorf("EndpointFor = %q, %v", got, err)
	}
}

func TestView_RefusesAnEmptyResult(t *testing.T) {
	c := fakeNode(t, 200, `[]`, nil)
	if _, err := c.ClaimDeadline(context.Background(), mod, "0xesc"); !errors.Is(err, ErrBadReply) {
		t.Fatalf("an empty view result was accepted: %v", err)
	}
}

// A Move abort and an unreachable node both arrive as a non-200. They are
// opposite facts and must not share a name: one means the escrow does not exist,
// the other means check the network.
func TestView_AMoveAbortIsNotANodeFailure(t *testing.T) {
	c := fakeNode(t, 400, `{"message":"Move abort in 0x1b41::escrow: E_NOT_INITIALISED(0x2): ","vm_error_code":4016}`, nil)
	_, err := c.ClaimDeadline(context.Background(), mod, "0xnope")
	if !errors.Is(err, ErrContractAborted) {
		t.Fatalf("want ErrContractAborted, got %v", err)
	}
	if errors.Is(err, ErrNodeFailed) {
		t.Error("a contract abort was also reported as the node being unreachable")
	}
	if !strings.Contains(err.Error(), "E_NOT_INITIALISED(0x2)") {
		t.Errorf("the abort code was discarded: %v", err)
	}
}

func TestView_ARealNodeFailureIsNotAnAbort(t *testing.T) {
	c := fakeNode(t, 503, `upstream connect error`, nil)
	_, err := c.ClaimDeadline(context.Background(), mod, "0xesc")
	if !errors.Is(err, ErrNodeFailed) {
		t.Fatalf("want ErrNodeFailed, got %v", err)
	}
	if errors.Is(err, ErrContractAborted) {
		t.Error("a node failure was reported as a contract abort")
	}
}

func TestAptosBalance_ParsesQuotedAndBare(t *testing.T) {
	for _, body := range []string{`977072000`, `"977072000"`} {
		c := fakeNode(t, 200, body, nil)
		got, err := c.AptosBalance(context.Background(), "0xabc")
		if err != nil || got != 977072000 {
			t.Errorf("body %s: got %d, err %v", body, got, err)
		}
	}
}

// A funded account whose balance cannot be read must ERROR, never report zero.
// On the sponsorship path a false zero refuses every claim while the money sits
// there - the opposite of the failure the balance floor exists to prevent.
func TestAptosBalance_NeverReportsZeroOnFailure(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{404, `{"message":"Resource not found"}`},
		{200, `not-a-number`},
		{503, `upstream down`},
	} {
		c := fakeNode(t, tc.status, tc.body, nil)
		got, err := c.AptosBalance(context.Background(), "0xabc")
		if err == nil {
			t.Errorf("status %d body %q: accepted, returning %d", tc.status, tc.body, got)
		}
		if got != 0 {
			continue
		}
		// Zero WITH an error is fine; zero without one is the hazard.
		if err == nil {
			t.Errorf("status %d: reported a zero balance with no error", tc.status)
		}
	}
}
