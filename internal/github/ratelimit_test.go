package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type staticTransport struct {
	status int
	header http.Header
	body   string
}

func (s staticTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: s.status, Header: s.header, Body: io.NopCloser(strings.NewReader(s.body)), Request: r}, nil
}

// The list calls the sync worker makes said only "status 403". 43,708 jobs
// failed with that string, and nothing in it said whether the token was
// refused or the budget was spent.
func TestListCalls_FailWithAPIError(t *testing.T) {
	tr := staticTransport{status: http.StatusForbidden, header: http.Header{
		"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {"1786990000"},
	}, body: `{"message":"API rate limit exceeded for user ID 1."}`}
	c := &Client{HTTP: &http.Client{Transport: tr}}
	ctx := context.Background()

	calls := map[string]error{}
	_, calls["list issues"] = c.ListIssuesPage(ctx, "tok", "octo/repo", 1)
	_, calls["list prs"] = c.ListPRsPage(ctx, "tok", "octo/repo", 1)
	_, calls["list issue comments"] = c.ListIssueComments(ctx, "tok", "octo/repo", 7)
	for call, err := range calls {
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Errorf("%s: error %v is not an *APIError", call, err)
			continue
		}
		if apiErr.Call != call || apiErr.Status != 403 || apiErr.RateLimitRemaining != "0" || !strings.Contains(apiErr.Body, "rate limit") {
			t.Errorf("%s: %+v", call, apiErr)
		}
		// Existing log searches for "github list issues failed: status 403" still match.
		if !strings.HasPrefix(err.Error(), "github "+call+" failed: status 403") {
			t.Errorf("%s: message %q changed shape", call, err.Error())
		}
		if strings.Contains(err.Error(), "tok") {
			t.Errorf("%s: message contains the token", call)
		}
	}
}

func TestRateLimitedUntil(t *testing.T) {
	now := time.Unix(1_786_990_000, 0)
	cases := []struct {
		name   string
		err    *APIError
		wantOK bool
		want   time.Duration // from now
	}{
		{"primary limit waits for the reset", &APIError{Status: 403, RateLimitRemaining: "0", RateLimitReset: "1786991200"}, true, 1205 * time.Second},
		{"reset already passed retries in a minute", &APIError{Status: 403, RateLimitRemaining: "0", RateLimitReset: "1786980000"}, true, time.Minute},
		{"retry-after wins", &APIError{Status: 403, RetryAfter: "90", RateLimitRemaining: "12"}, true, 90 * time.Second},
		{"429 without headers", &APIError{Status: 429}, true, time.Minute},
		{"secondary limit by message", &APIError{Status: 403, RateLimitRemaining: "4000", Body: `{"message":"You have exceeded a secondary rate limit."}`}, true, time.Minute},
		{"reset far ahead is capped at an hour", &APIError{Status: 403, RateLimitRemaining: "0", RateLimitReset: "1787990000"}, true, time.Hour},
		{"403 for access is not a rate limit", &APIError{Status: 403, RateLimitRemaining: "4999", Body: `{"message":"Resource not accessible by integration"}`}, false, 0},
		{"401 is not a rate limit", &APIError{Status: 401, RateLimitRemaining: "0"}, false, 0},
		{"500 is not a rate limit", &APIError{Status: 500}, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.err.RateLimitedUntil(now)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got.Sub(now) != tc.want {
				t.Errorf("retry in %v, want %v", got.Sub(now), tc.want)
			}
		})
	}
}
