package keeperhub

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

var oneLeg = []Recipient{{Address: "0x1111111111111111111111111111111111111111", AmountMinor: "1", LegID: "leg"}}

func TestDispatch_ClassifiesEveryStatus(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   DispatchClass
		why    string
	}{
		{400, `{"error":"bad payload"}`, DispatchRejected, "validation precedes any run"},
		{401, `{"error":"Invalid API key format.","code":"invalid_key_format"}`, DispatchRejected, "key refused"},
		{402, `{"error":"payment required","executionId":"exec-402"}`, DispatchRejected, "row created, run never started"},
		{403, `{"error":"forbidden"}`, DispatchRejected, "key not permitted"},
		{404, `{"error":"not found"}`, DispatchRejected, "no such workflow"},
		{410, `{"error":"Workflow is disabled"}`, DispatchRejected, "observed on a disabled workflow"},
		{422, `{"error":"unprocessable"}`, DispatchRejected, "payload refused"},
		{429, `{"error":"slow down"}`, DispatchRejected, "rate limited before admission"},

		{409, `{"error":"in progress","code":"idempotency_in_progress","retryable":true}`, DispatchIndeterminate,
			"THE TRAP: the first request under this key is still running and may still pay"},
		{409, `{"error":"conflict","code":"idempotency_conflict","retryable":false}`, DispatchIndeterminate,
			"a key already bound to some request"},
		{408, `{"error":"request timeout"}`, DispatchIndeterminate, "the server may have processed it"},
		{500, `{"error":"boom"}`, DispatchIndeterminate, "server-side failure"},
		{502, ``, DispatchIndeterminate, "gateway"},
		{503, ``, DispatchIndeterminate, "unavailable"},
		{418, `{"error":"teapot"}`, DispatchIndeterminate, "unrecognised defaults to the manual check"},
	} {
		c, _ := newServer(t, tc.status, tc.body, "")
		ack, err := c.Dispatch(context.Background(), oneLeg)
		var de *DispatchError
		if !errors.As(err, &de) {
			t.Fatalf("%d: err = %v, want a *DispatchError", tc.status, err)
		}
		if de.Class != tc.want {
			t.Errorf("%d (%s): class = %s, want %s - %s", tc.status, de.Code, de.Class, tc.want, tc.why)
		}
		if de.HTTPStatus != tc.status {
			t.Errorf("%d: HTTPStatus = %d", tc.status, de.HTTPStatus)
		}
		if IsRejected(err) != (tc.want == DispatchRejected) {
			t.Errorf("%d: IsRejected = %v", tc.status, IsRejected(err))
		}
		if tc.want == DispatchRejected && !errors.Is(err, ErrDispatchRefused) {
			t.Errorf("%d: a rejection does not match ErrDispatchRefused", tc.status)
		}
		if tc.want == DispatchIndeterminate && !errors.Is(err, ErrDispatchIndeterminate) {
			t.Errorf("%d: indeterminate does not match ErrDispatchIndeterminate", tc.status)
		}
		if tc.status == 402 {
			// An execution id on a rejection. It is NOT evidence of dispatch.
			if de.ExecutionID != "exec-402" || ack.ExecutionID != "exec-402" {
				t.Errorf("402: the created execution id was not surfaced: %q / %q", de.ExecutionID, ack.ExecutionID)
			}
		}
		if ack.IdempotencyKey == "" {
			t.Errorf("%d: the attempt's key was not returned", tc.status)
		}
	}
}

// A name that does not resolve: the request never left.
func TestDispatch_DNSFailureIsRejected(t *testing.T) {
	c, err := New("wfb_placeholder_not_a_real_key", "kh_placeholder_not_a_real_key", "wf")
	if err != nil {
		t.Fatal(err)
	}
	c.BaseURL = "http://keeperhub-does-not-exist.invalid"
	_, derr := c.Dispatch(context.Background(), oneLeg)
	if !IsRejected(derr) {
		t.Fatalf("err = %v, want rejected - a DNS failure happens before anything is sent", derr)
	}
}

// Nothing listening: the connection was refused, so nothing was sent.
func TestDispatch_ConnectionRefusedIsRejected(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	c, _ := New("wfb_placeholder_not_a_real_key", "kh_placeholder_not_a_real_key", "wf")
	c.BaseURL = "http://" + addr
	_, derr := c.Dispatch(context.Background(), oneLeg)
	if !IsRejected(derr) {
		t.Fatalf("err = %v, want rejected - a refused connection sends nothing", derr)
	}
}

// A timeout after the request was sent: it may have been received and run.
func TestDispatch_TimeoutIsIndeterminate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)
	c, _ := New("wfb_placeholder_not_a_real_key", "kh_placeholder_not_a_real_key", "wf")
	c.BaseURL = srv.URL
	c.HTTP.Timeout = 50 * time.Millisecond

	_, derr := c.Dispatch(context.Background(), oneLeg)
	if IsRejected(derr) || !errors.Is(derr, ErrDispatchIndeterminate) {
		t.Fatalf("err = %v, want indeterminate - a timed-out request may have been processed", derr)
	}
}

// Errors this package did not classify can never make legs resumable.
func TestIsRejected_IsFalseForForeignErrors(t *testing.T) {
	if IsRejected(errors.New("anything")) || IsRejected(nil) {
		t.Fatal("an unclassified error was treated as a definitive rejection")
	}
}
