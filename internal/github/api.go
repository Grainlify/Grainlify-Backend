package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	HTTP      *http.Client
	UserAgent string
}

func NewClient() *Client {
	return &Client{
		HTTP:      &http.Client{Timeout: 10 * time.Second},
		UserAgent: "patchwork-backend",
	}
}

// userAPIURL is a var (not a const) so tests can point it at an
// httptest.Server instead of the real GitHub API.
var userAPIURL = "https://api.github.com/user"

// APIError is a non-2xx from the GitHub REST API, with the detail needed to
// tell the causes apart.
//
// "status 403" on its own is four different incidents wearing one name: a
// revoked or expired token, the primary rate limit exhausted, a secondary
// (per-IP) rate limit, and GitHub itself degraded. They need opposite
// responses - rotate a credential, back off, slow the caller down, wait - and
// the bare status distinguishes none of them.
//
// GitHub already says which. The message is in the body, the budget is in
// X-RateLimit-Remaining, and the wait is in Retry-After. This type is only
// here because we were reading all three and throwing them away, which is how
// a signup outage came to be debugged from a string that named the call and
// nothing else.
//
// The access token is never included, in any field. The body is bounded
// because an error path must not be a way to write an unbounded remote string
// into our logs.
type APIError struct {
	// Call is the endpoint in a form a human can search for, e.g. "GET /user".
	Call   string
	Status int
	// Body is GitHub's response, truncated. For an auth failure this is
	// `{"message":"Bad credentials"...}`; for a secondary rate limit it names
	// that specifically. It is the single most useful field and was the one
	// most completely discarded.
	Body string
	// RateLimitRemaining distinguishes "we are out of budget" from every other
	// 403. Empty when GitHub did not send the header.
	RateLimitRemaining string
	// RateLimitReset is a unix timestamp; RetryAfter is seconds. GitHub sends
	// one or the other depending on which limit was hit.
	RateLimitReset string
	RetryAfter     string
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("github %s failed: status %d", e.Call, e.Status)
	if e.Body != "" {
		msg += fmt.Sprintf(", body: %s", e.Body)
	}
	if e.RateLimitRemaining != "" {
		msg += fmt.Sprintf(", ratelimit_remaining: %s", e.RateLimitRemaining)
	}
	if e.RateLimitReset != "" {
		msg += fmt.Sprintf(", ratelimit_reset: %s", e.RateLimitReset)
	}
	if e.RetryAfter != "" {
		msg += fmt.Sprintf(", retry_after: %s", e.RetryAfter)
	}
	return msg
}

// newAPIError captures a failed response. Must be called before the body is
// closed and instead of decoding it.
func newAPIError(call string, resp *http.Response) *APIError {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return &APIError{
		Call:               call,
		Status:             resp.StatusCode,
		Body:               strings.TrimSpace(string(raw)),
		RateLimitRemaining: resp.Header.Get("X-RateLimit-Remaining"),
		RateLimitReset:     resp.Header.Get("X-RateLimit-Reset"),
		RetryAfter:         resp.Header.Get("Retry-After"),
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, userAPIURL, nil)
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
		return User{}, newAPIError("GET /user", resp)
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
		return nil, newAPIError("GET /user/emails", resp)
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
