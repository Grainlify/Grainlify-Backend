// Package github — issue comment mutations.
//
// Both CreateIssueComment and DeleteIssueComment route through c.HTTP, which is
// always backed by RateLimitTransport (see NewClient in api.go).  The transport
// transparently retries 403/429 rate-limit responses — honoring Retry-After for
// secondary limits and X-RateLimit-Reset for primary limits — up to
// DefaultMaxRetries times before returning the final response to the caller.
//
// Rate-limit response classification:
//
//   - Secondary rate limit: 403 or 429 with a Retry-After header present.
//     The transport retries these and, when retries are exhausted, the
//     functions here surface ErrSecondaryRateLimit so callers can distinguish
//     "throttled" from "auth failure" (both may appear as 403 from GitHub).
//
//   - Primary rate limit: 403 or 429 with X-RateLimit-Remaining: 0 (no
//     Retry-After).  The transport retries these too; on exhaustion the raw
//     4xx response is returned as a non-rate-limit error.
//
//   - Auth/permission failure: bare 403 with no rate-limit headers.  The
//     transport does not retry these; checkCommentRateLimit delegates to
//     parseGitHubAPIError.
//
//   - 5xx (GitHub outage): not retried by RateLimitTransport.  Callers are
//     responsible for higher-level retry on server errors.
//
// Callers that need to distinguish throttling from other errors:
//
//	comment, err := client.CreateIssueComment(ctx, tok, "owner/repo", 42, "body")
//	if github.IsSecondaryRateLimited(err) {
//	    // back off or queue for later; Retry-After value is in err.Error()
//	}
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrSecondaryRateLimit is returned by CreateIssueComment and DeleteIssueComment
// when GitHub responds with a secondary rate limit (403 + Retry-After) and all
// transport-layer retries have been exhausted.  Callers should back off and
// retry later; the Retry-After value, when available, is included in the message.
//
// Use IsSecondaryRateLimited to test for this condition without comparing
// the sentinel directly.
var ErrSecondaryRateLimit = errors.New("github secondary rate limit exhausted")

// IsSecondaryRateLimited reports whether err (or any error in its chain) is a
// secondary-rate-limit exhaustion.  Safe to call with nil.
func IsSecondaryRateLimited(err error) bool {
	return err != nil && errors.Is(err, ErrSecondaryRateLimit)
}

// secondaryRateLimitError wraps ErrSecondaryRateLimit with the Retry-After
// value from the response so callers can log or surface a meaningful message.
type secondaryRateLimitError struct {
	retryAfter string // raw Retry-After header value; may be empty
}

func (e *secondaryRateLimitError) Error() string {
	if e.retryAfter != "" {
		return fmt.Sprintf("github secondary rate limit exhausted (Retry-After: %s)", e.retryAfter)
	}
	return "github secondary rate limit exhausted"
}

// Is makes errors.Is(err, ErrSecondaryRateLimit) work for wrapped instances.
func (e *secondaryRateLimitError) Is(target error) bool {
	return target == ErrSecondaryRateLimit
}

// checkCommentRateLimit inspects a non-2xx response and returns an appropriate
// error.  For secondary rate limits (403/429 + Retry-After) it returns a
// secondaryRateLimitError so IsSecondaryRateLimited can detect it.  For all
// other error statuses it delegates to parseGitHubAPIError.
func checkCommentRateLimit(resp *http.Response) error {
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		if ra := strings.TrimSpace(resp.Header.Get("Retry-After")); ra != "" {
			return &secondaryRateLimitError{retryAfter: ra}
		}
	}
	return parseGitHubAPIError(resp)
}

type issueCommentCreateResponse struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CreateIssueComment posts a new comment on a GitHub issue.
//
// The call is routed through c.HTTP, which is backed by RateLimitTransport.
// The transport retries up to DefaultMaxRetries times on rate-limit responses,
// sleeping according to Retry-After (secondary limit) or X-RateLimit-Reset
// (primary limit).  If retries are exhausted and the final response is a
// secondary rate limit (403/429 + Retry-After), ErrSecondaryRateLimit is
// returned so callers can distinguish throttling from auth failures.
func (c *Client) CreateIssueComment(ctx context.Context, accessToken string, fullName string, issueNumber int, body string) (IssueComment, error) {
	owner, repo, err := splitFullName(fullName)
	if err != nil {
		return IssueComment{}, err
	}
	if strings.TrimSpace(accessToken) == "" {
		return IssueComment{}, fmt.Errorf("missing github access token")
	}
	if issueNumber <= 0 {
		return IssueComment{}, fmt.Errorf("invalid issue number")
	}
	if strings.TrimSpace(body) == "" {
		return IssueComment{}, fmt.Errorf("comment body is required")
	}

	u := "https://api.github.com/repos/" +
		url.PathEscape(owner) + "/" +
		url.PathEscape(repo) +
		"/issues/" + fmt.Sprintf("%d", issueNumber) + "/comments"

	payload := map[string]string{"body": body}
	b, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return IssueComment{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}

	// c.HTTP is backed by RateLimitTransport; rate-limit retries are transparent.
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return IssueComment{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return IssueComment{}, checkCommentRateLimit(resp)
	}

	var out issueCommentCreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return IssueComment{}, err
	}
	if out.ID == 0 {
		return IssueComment{}, fmt.Errorf("invalid github comment response")
	}

	return IssueComment{
		ID:   out.ID,
		Body: out.Body,
		User: struct {
			Login string `json:"login"`
		}{Login: out.User.Login},
		HTMLURL:   out.HTMLURL,
		CreatedAt: out.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: out.UpdatedAt.UTC().Format(time.RFC3339),
	}, nil
}

// DeleteIssueComment deletes a comment on a GitHub issue.
//
// The accessToken must belong to the comment author or a repo admin.  Like
// CreateIssueComment, the request routes through RateLimitTransport so
// rate-limit retries are handled transparently.  ErrSecondaryRateLimit is
// returned if all retries are exhausted on a secondary-limit response.
func (c *Client) DeleteIssueComment(ctx context.Context, accessToken string, fullName string, commentID int64) error {
	owner, repo, err := splitFullName(fullName)
	if err != nil {
		return err
	}
	if strings.TrimSpace(accessToken) == "" {
		return fmt.Errorf("missing github access token")
	}
	if commentID <= 0 {
		return fmt.Errorf("invalid comment id")
	}

	u := "https://api.github.com/repos/" +
		url.PathEscape(owner) + "/" +
		url.PathEscape(repo) +
		"/issues/comments/" + fmt.Sprintf("%d", commentID)

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}

	// c.HTTP is backed by RateLimitTransport; rate-limit retries are transparent.
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return checkCommentRateLimit(resp)
	}
	return nil
}
