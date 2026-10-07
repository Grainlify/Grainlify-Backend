package handlers

import (
	"errors"
	"testing"

	"github.com/jagadeesh/grainlify/backend/internal/github"
)

func TestRepoRefusedAsPrivate(t *testing.T) {
	zero := 0
	reset := int64(1789739713)
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"404 is a private or missing repo", &github.GitHubAPIError{StatusCode: 404, Message: "Not Found"}, true},
		// The production case: the shared unauthenticated quota ran out and a
		// public, listed repo was reported as private to every caller.
		{"rate-limit 403 with headers", &github.GitHubAPIError{StatusCode: 403, Message: "API rate limit exceeded for 208.77.244.66.", RateLimitRemaining: &zero, RateLimitResetUnix: &reset}, false},
		{"rate-limit 403 without headers", &github.GitHubAPIError{StatusCode: 403, Message: "You have exceeded a secondary rate limit"}, false},
		{"other 403 stays refused", &github.GitHubAPIError{StatusCode: 403, Message: "Resource protected by organization SAML enforcement"}, true},
		{"5xx is not privacy", &github.GitHubAPIError{StatusCode: 502, Message: "Bad Gateway"}, false},
		{"network error is not privacy", errors.New("dial tcp: i/o timeout"), false},
		{"wrapped 404 still refused", errors.Join(errors.New("ctx"), &github.GitHubAPIError{StatusCode: 404}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := repoRefusedAsPrivate(tc.err); got != tc.want {
				t.Fatalf("repoRefusedAsPrivate(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
