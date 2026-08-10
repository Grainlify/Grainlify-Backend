package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// publicUserAPIBase is a var (not a const) so tests can point it at an
// httptest.Server, matching userAPIURL's pattern in api.go.
var publicUserAPIBase = "https://api.github.com/users/"

// orgAPIBase is a var for the same reason.
var orgAPIBase = "https://api.github.com/orgs/"

// PublicUser is the subset of GitHub's public user profile the GrainHack
// hard gates (AI-specs.md §4.1) and Layer 2 evidence (§4.3) need. Distinct
// from User (api.go), which models the *authenticated* /user response and
// carries neither CreatedAt nor Type.
type PublicUser struct {
	ID          int64     `json:"id"`
	Login       string    `json:"login"`
	Type        string    `json:"type"` // "User" | "Bot" | "Organization"
	CreatedAt   time.Time `json:"created_at"`
	PublicRepos int       `json:"public_repos"`
}

// GetPublicUser fetches GET /users/{login}. accessToken may be any valid
// token (installation or user) - the endpoint is public, but authenticating
// raises the rate limit from 60/hr to 5000/hr.
func (c *Client) GetPublicUser(ctx context.Context, accessToken, login string) (PublicUser, error) {
	if login == "" {
		return PublicUser{}, fmt.Errorf("github.GetPublicUser: empty login")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, publicUserAPIBase+url.PathEscape(login), nil)
	if err != nil {
		return PublicUser{}, err
	}
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return PublicUser{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return PublicUser{}, parseGitHubAPIError(resp)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return PublicUser{}, err
	}
	var u PublicUser
	if err := json.Unmarshal(body, &u); err != nil {
		return PublicUser{}, fmt.Errorf("github.GetPublicUser: decode: %w", err)
	}
	return u, nil
}

// searchAPIBase is a var for the same reason as the bases above.
var searchAPIBase = "https://api.github.com/search/commits"

// UserHasPublicActivityBefore reports whether login authored at least
// minCommits public commits before `before`, via the commit search API.
//
// This backs AI-specs.md §3.6's require_pre_announcement_commit, which the
// spec is explicit about: "Existence check, not a volume check." It asks
// only whether the account existed as a real contributor before the event
// was announced - the anti-sybil signal - and deliberately does not reward
// higher counts anywhere downstream.
func (c *Client) UserHasPublicActivityBefore(ctx context.Context, accessToken, login string, before time.Time, minCommits int) (bool, error) {
	if login == "" {
		return false, fmt.Errorf("github.UserHasPublicActivityBefore: empty login")
	}
	if minCommits < 1 {
		return true, nil
	}
	q := fmt.Sprintf("author:%s author-date:<%s", login, before.UTC().Format("2006-01-02"))
	u := searchAPIBase + "?q=" + url.QueryEscape(q) + "&per_page=1"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, parseGitHubAPIError(resp)
	}
	var payload struct {
		TotalCount int `json:"total_count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return false, fmt.Errorf("github.UserHasPublicActivityBefore: decode: %w", err)
	}
	return payload.TotalCount >= minCommits, nil
}

// IsOrgMember reports whether login is a *public or private* member of org,
// via GET /orgs/{org}/members/{username}: 204 = member, 404 = not, 302 =
// the token can't see private membership.
//
// The 302 case is why this returns (bool, error) rather than just bool: a
// redirect means "cannot determine", and AI-specs.md §4.1's block_org_members
// gate is a real anti-collusion control - treating "unknown" as "not a
// member" would silently open the exact hole the gate exists to close. The
// caller decides how to handle an indeterminate answer.
func (c *Client) IsOrgMember(ctx context.Context, accessToken, org, login string) (bool, error) {
	if org == "" || login == "" {
		return false, fmt.Errorf("github.IsOrgMember: empty org or login")
	}
	u := orgAPIBase + url.PathEscape(org) + "/members/" + url.PathEscape(login)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}

	// Don't auto-follow the 302: the redirect itself is the signal.
	client := *c.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNoContent:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	case http.StatusFound:
		return false, fmt.Errorf("github.IsOrgMember: membership of %q in %q is not visible to this token", login, org)
	default:
		return false, parseGitHubAPIError(resp)
	}
}
