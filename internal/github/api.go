package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type Client struct {
	HTTP      *http.Client
	UserAgent string
}

// clientTimeout bounds the *entire* round trip performed by http.Client,
// including every retry the composed transports perform internally — Go
// derives the context deadline used by both transports' retry-sleep selects
// from this same Timeout. It must therefore cover their realistic combined
// worst case, not just a single attempt:
//
//   - RateLimitTransport: up to DefaultMaxRetries (3) waits, each capped at
//     DefaultMaxWait (60s) ⇒ up to 180s honoring a real X-RateLimit-Reset.
//   - TransientRetryTransport: a 429/403 response isn't in its own retry set
//     (only 5xx/network errors are), so it does not multiply that 180s —
//     it makes one pass-through call to RateLimitTransport per its own
//     attempt, and its 30s MaxElapsed budget self-limits further attempts
//     once a single inner call has already run that long.
//
// 210s covers the 180s worst case with headroom for actual request latency
// and TransientRetryTransport's own smaller-scale retries, while still
// failing a genuinely hung connection well short of "forever".
const clientTimeout = 210 * time.Second

// NewClient returns a GitHub API client with two layers of automatic retry:
//
//  1. RateLimitTransport — retries 403/429 rate-limit responses, honoring the
//     X-RateLimit-Reset and Retry-After headers returned by GitHub.
//
//  2. TransientRetryTransport — retries transient 5xx server errors and
//     network-level failures (connection reset, timeout, etc.) with jittered
//     exponential back-off. Only idempotent requests (GET/HEAD/OPTIONS) are
//     retried by default; non-idempotent requests require the caller to set the
//     "X-Retry-Non-Idempotent: true" request header to opt in explicitly.
//
// The two transports are composed so that each handles its own concern:
//
//	http.DefaultTransport
//	  └─ RateLimitTransport           (inner — rate-limit signals)
//	       └─ TransientRetryTransport (outer — 5xx / network errors)
//
// See clientTimeout for why the client-level Timeout is 210s rather than a
// short fixed value: a short timeout would silently truncate the retry
// budgets above before they can do what their own doc comments describe.
// Callers with their own, tighter time budget should derive a shorter
// context.Context and pass it through (every method in this package accepts
// one) rather than relying on the client-level Timeout for that.
func NewClient() *Client {
	rateLimitTransport := NewRateLimitTransport(nil)
	transientTransport := NewTransientRetryTransport(rateLimitTransport)
	return &Client{
		HTTP: &http.Client{
			Timeout:   clientTimeout,
			Transport: transientTransport,
		},
		UserAgent: "grainlify-backend",
	}
}

type User struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	AvatarURL string `json:"avatar_url"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	Location  string `json:"location"`
	Bio       string `json:"bio"`
	Blog      string `json:"blog"` // Website URL
}

type Email struct {
	Email      string `json:"email"`
	Primary    bool   `json:"primary"`
	Verified   bool   `json:"verified"`
	Visibility string `json:"visibility"`
}

func (c *Client) GetUser(ctx context.Context, accessToken string) (User, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		return User{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return User{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return User{}, fmt.Errorf("github /user failed: status %d", resp.StatusCode)
	}

	var u User
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return User{}, err
	}
	if u.ID == 0 || u.Login == "" {
		return User{}, fmt.Errorf("invalid github user response")
	}
	return u, nil
}

// GetUserEmails fetches the user's email addresses from GitHub
// Requires user:email scope
func (c *Client) GetUserEmails(ctx context.Context, accessToken string) ([]Email, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user/emails", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("github /user/emails failed: status %d", resp.StatusCode)
	}

	var emails []Email
	if err := json.NewDecoder(resp.Body).Decode(&emails); err != nil {
		return nil, err
	}
	return emails, nil
}

// GetPrimaryEmail gets the primary email from the user's emails list
func (c *Client) GetPrimaryEmail(ctx context.Context, accessToken string) (string, error) {
	emails, err := c.GetUserEmails(ctx, accessToken)
	if err != nil {
		return "", err
	}

	// Find primary email
	for _, email := range emails {
		if email.Primary && email.Verified {
			return email.Email, nil
		}
	}

	// If no primary verified email, return first verified email
	for _, email := range emails {
		if email.Verified {
			return email.Email, nil
		}
	}

	// If no verified email, return first email
	if len(emails) > 0 {
		return emails[0].Email, nil
	}

	return "", fmt.Errorf("no email found")
}
