package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// What a failed GitHub call has to be able to tell us.
//
// These exist because signup broke in production and the only evidence was the
// string "github_user_fetch_failed" plus a 401 in the access log. The upstream
// status, GitHub's own message and the rate-limit budget were all read and
// discarded, so a revoked credential, an exhausted rate limit and a GitHub
// outage were the same log line - which is to say, no line at all.
//
// Each assertion below is one thing we could not answer during that incident.

func TestGetUser_ErrorCarriesStatusBodyAndRateLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "1786990000")
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit"}`))
	}))
	defer server.Close()

	orig := userAPIURL
	userAPIURL = server.URL
	t.Cleanup(func() { userAPIURL = orig })

	_, err := (&Client{HTTP: server.Client(), UserAgent: "patchwork-backend"}).
		GetUser(context.Background(), "tok")
	if err == nil {
		t.Fatal("GetUser() error = nil, want error for 403")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is %T, want *APIError so callers can inspect it", err)
	}
	if apiErr.Status != http.StatusForbidden {
		t.Errorf("Status = %d, want 403", apiErr.Status)
	}
	if apiErr.RateLimitRemaining != "0" {
		t.Errorf("RateLimitRemaining = %q, want %q - this is the field that separates a rate limit from every other 403",
			apiErr.RateLimitRemaining, "0")
	}
	if apiErr.RetryAfter != "60" {
		t.Errorf("RetryAfter = %q, want %q", apiErr.RetryAfter, "60")
	}

	// The rendered message is what actually reaches the log, so assert on it
	// rather than on the struct alone.
	msg := err.Error()
	for _, want := range []string{"GET /user", "403", "secondary rate limit", "ratelimit_remaining: 0", "retry_after: 60"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q\ngot: %s", want, msg)
		}
	}
}

// A 1KB cap: an error path must not let a remote host write an unbounded
// string into our logs.
func TestGetUser_ErrorBodyIsBounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(strings.Repeat("x", 100_000)))
	}))
	defer server.Close()

	orig := userAPIURL
	userAPIURL = server.URL
	t.Cleanup(func() { userAPIURL = orig })

	_, err := (&Client{HTTP: server.Client()}).GetUser(context.Background(), "tok")
	if err == nil {
		t.Fatal("GetUser() error = nil, want error for 500")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is %T, want *APIError", err)
	}
	if len(apiErr.Body) > 1024 {
		t.Errorf("Body length = %d, want <= 1024", len(apiErr.Body))
	}
}

// GitHub answers a refused exchange with HTTP 200 and an error payload. Before
// this, every one of those became "empty token" - so a rotated client secret
// (incorrect_client_credentials) and a double-submitted code
// (bad_verification_code) were indistinguishable, though only one is an
// incident.
func TestExchangeCode_SurfacesRefusalReasonFrom200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"error":"incorrect_client_credentials","error_description":"The client_id and/or client_secret passed are incorrect."}`))
	}))
	defer server.Close()

	orig := tokenEndpoint
	tokenEndpoint = server.URL
	t.Cleanup(func() { tokenEndpoint = orig })

	_, err := ExchangeCode(context.Background(), "code", OAuthConfig{
		ClientID: "id", ClientSecret: "secret", RedirectURL: "https://example.test/cb",
	})
	if err == nil {
		t.Fatal("ExchangeCode() error = nil, want error when GitHub returns an error payload")
	}
	if !strings.Contains(err.Error(), "incorrect_client_credentials") {
		t.Errorf("error = %q, want it to name the refusal reason", err)
	}
}

// The one thing this must never do. The token endpoint's SUCCESS body contains
// the access token, so no error from ExchangeCode may ever quote its body.
func TestExchangeCode_ErrorNeverContainsTheToken(t *testing.T) {
	const secretToken = "gho_thisMustNeverAppearInAnError"

	// A 200 carrying a token but no token_type, and a non-2xx carrying one
	// anyway - both plausible ways a body could leak into an error string.
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"non-2xx with a token in the body", http.StatusInternalServerError, `{"access_token":"` + secretToken + `"}`},
		{"200 with a token and an error", http.StatusOK, `{"access_token":"` + secretToken + `","error":"some_error"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			orig := tokenEndpoint
			tokenEndpoint = server.URL
			t.Cleanup(func() { tokenEndpoint = orig })

			_, err := ExchangeCode(context.Background(), "code", OAuthConfig{
				ClientID: "id", ClientSecret: "secret", RedirectURL: "https://example.test/cb",
			})
			if err == nil {
				return // nothing rendered, nothing to leak
			}
			if strings.Contains(err.Error(), secretToken) {
				t.Fatalf("access token leaked into error message: %s", err)
			}
		})
	}
}
