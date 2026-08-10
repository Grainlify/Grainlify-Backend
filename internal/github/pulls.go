package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// pullsAPIBase is a var so tests can point it at an httptest.Server,
// matching this package's convention.
var pullsAPIBase = "https://api.github.com/repos/"

// PRFile is one file's contribution to a pull request.
type PRFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Changes   int    `json:"changes"`
	// Patch is absent for binary files and omitted by GitHub on very large
	// diffs, which is why the caller must fall back to Additions/Deletions
	// rather than reading a missing patch as an empty change.
	Patch string `json:"patch"`
}

// prFilesPageSize is GitHub's maximum for this endpoint.
const prFilesPageSize = 100

// maxPRFilePages caps how much of a very large PR is fetched.
//
// A PR touching more than 300 files is not one a judge is going to assess
// meaningfully anyway, and the diff would blow past any sensible context
// window. Truncation is reported rather than hidden: a verdict computed
// from a partial diff has to be visibly partial.
const maxPRFilePages = 3

// ListPRFiles fetches a pull request's changed files.
//
// Returns truncated=true when the PR has more files than the page cap, so
// the caller can record that the diff stats it derives are incomplete
// instead of silently under-reporting a large change.
func (c *Client) ListPRFiles(ctx context.Context, accessToken, fullName string, number int) (files []PRFile, truncated bool, err error) {
	owner, repo, err := splitFullName(fullName)
	if err != nil {
		return nil, false, err
	}

	for page := 1; page <= maxPRFilePages; page++ {
		u := fmt.Sprintf("%s%s/%s/pulls/%d/files?per_page=%d&page=%d",
			pullsAPIBase, url.PathEscape(owner), url.PathEscape(repo), number, prFilesPageSize, page)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, false, err
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
			return nil, false, err
		}
		if resp.StatusCode != http.StatusOK {
			apiErr := parseGitHubAPIError(resp)
			resp.Body.Close()
			return nil, false, apiErr
		}
		var page1 []PRFile
		decodeErr := json.NewDecoder(resp.Body).Decode(&page1)
		resp.Body.Close()
		if decodeErr != nil {
			return nil, false, fmt.Errorf("github.ListPRFiles: decode: %w", decodeErr)
		}

		files = append(files, page1...)
		if len(page1) < prFilesPageSize {
			return files, false, nil
		}
		if page == maxPRFilePages {
			return files, true, nil
		}
	}
	return files, false, nil
}

// IsRepoCollaboratorAdmin reports whether login has admin permission on the
// repo, backing §2.4's "not a repo admin or org owner" condition.
//
// Returns (false, error) when permission can't be read - the caller must not
// read an unreadable permission as "not an admin", since that is the
// direction that lets a maintainer collect on their own org's issues.
func (c *Client) IsRepoCollaboratorAdmin(ctx context.Context, accessToken, fullName, login string) (bool, error) {
	owner, repo, err := splitFullName(fullName)
	if err != nil {
		return false, err
	}
	u := pullsAPIBase + url.PathEscape(owner) + "/" + url.PathEscape(repo) +
		"/collaborators/" + url.PathEscape(login) + "/permission"

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
	// 404 means "not a collaborator at all", which is a real answer.
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, parseGitHubAPIError(resp)
	}
	var payload struct {
		Permission string `json:"permission"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return false, fmt.Errorf("github.IsRepoCollaboratorAdmin: decode: %w", err)
	}
	return payload.Permission == "admin", nil
}

// RepoAdmins returns the set of logins with admin permission on a repo,
// lowercased for case-insensitive comparison.
//
// Fetched once per repo per judging run rather than per PR: §2.4's "not a
// repo admin or org owner" condition applies to every PR in the repo, and a
// per-PR permission check would be one round trip per submission for an
// answer that doesn't change between them.
//
// Returns an error rather than an empty set when the list can't be read. An
// unreadable permission list must never be mistaken for "nobody is an
// admin" - that is the direction that lets a maintainer's second account
// collect on their own org's issues, which is the collusion path §2.3
// exists to close.
func (c *Client) RepoAdmins(ctx context.Context, accessToken, fullName string) (map[string]bool, error) {
	owner, repo, err := splitFullName(fullName)
	if err != nil {
		return nil, err
	}
	admins := map[string]bool{}

	for page := 1; page <= 5; page++ {
		u := fmt.Sprintf("%s%s/%s/collaborators?permission=admin&per_page=100&page=%d",
			pullsAPIBase, url.PathEscape(owner), url.PathEscape(repo), page)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
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
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			apiErr := parseGitHubAPIError(resp)
			resp.Body.Close()
			return nil, apiErr
		}
		var batch []struct {
			Login string `json:"login"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&batch)
		resp.Body.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("github.RepoAdmins: decode: %w", decodeErr)
		}
		for _, u := range batch {
			admins[strings.ToLower(u.Login)] = true
		}
		if len(batch) < 100 {
			break
		}
	}
	return admins, nil
}

// CommitCIStatus reports whether CI passed for a commit.
//
// Returns nil for "unknown", deliberately distinct from false: a repo with
// no CI configured, or one whose checks this token cannot see, must not have
// its contributors rejected for a failure that was never observed (see
// hackathon.Prefilter, which treats nil as not-failing).
//
// Consults both APIs because repos use either - Check Runs is what GitHub
// Actions reports to, while the older commit-status API is still what many
// external CI services use. A failure on either is a failure; success
// requires at least one of them to have actually run.
func (c *Client) CommitCIStatus(ctx context.Context, accessToken, fullName, sha string) (*bool, error) {
	if strings.TrimSpace(sha) == "" {
		return nil, nil
	}
	owner, repo, err := splitFullName(fullName)
	if err != nil {
		return nil, err
	}
	base := pullsAPIBase + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/commits/" + url.PathEscape(sha)

	get := func(path string, out any) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return err
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
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return parseGitHubAPIError(resp)
		}
		return json.NewDecoder(resp.Body).Decode(out)
	}

	sawSuccess := false
	failed := false
	passed := true

	var checks struct {
		CheckRuns []struct {
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if err := get("/check-runs?per_page=100", &checks); err == nil {
		for _, r := range checks.CheckRuns {
			switch r.Conclusion {
			case "failure", "timed_out", "cancelled", "action_required", "startup_failure":
				return &failed, nil
			case "success", "neutral", "skipped":
				sawSuccess = true
			}
		}
	}

	var combined struct {
		State string `json:"state"`
	}
	if err := get("/status", &combined); err == nil {
		switch combined.State {
		case "failure", "error":
			return &failed, nil
		case "success":
			sawSuccess = true
		}
	}

	if sawSuccess {
		return &passed, nil
	}
	// Nothing ran, or nothing we could see. Unknown, not failing.
	return nil, nil
}
