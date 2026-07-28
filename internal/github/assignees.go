package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/gofiber/fiber/v2"
)

// issueAssigneesResponse decodes the subset of GitHub's issue-assignees
// endpoint response AddIssueAssignees cares about: the resulting assignees
// GitHub actually applied.
type issueAssigneesResponse struct {
	Assignees []struct {
		Login string `json:"login"`
	} `json:"assignees"`
}

// AddIssueAssignees adds assignees to a GitHub issue and returns the logins
// GitHub actually applied, decoded from the response body. Requires repo
// write permission (maintainer).
//
// GitHub silently drops a login from the resulting assignees array — rather
// than returning an error — when it isn't a valid assignee (not a
// collaborator, insufficient permissions, doesn't exist). A nil error here
// only means the request itself succeeded; callers MUST check the returned
// list against what they requested before treating an assignment as applied.
func (c *Client) AddIssueAssignees(ctx context.Context, accessToken string, fullName string, issueNumber int, logins []string) ([]string, error) {
	if issueNumber <= 0 || len(logins) == 0 {
		return nil, fmt.Errorf("invalid issue number or assignees")
	}
	owner, repo, err := splitFullName(fullName)
	if err != nil {
		return nil, err
	}

	u := "https://api.github.com/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/issues/" + fmt.Sprintf("%d", issueNumber) + "/assignees"
	payload := map[string][]string{"assignees": logins}
	b, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		ghErr := parseGitHubAPIError(resp)
		if apiErr, ok := ghErr.(*GitHubAPIError); ok {
			if apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 {
				return nil, fiber.NewError(apiErr.StatusCode, apiErr.Message)
			}
		}
		return nil, ghErr
	}

	var decoded issueAssigneesResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode assignees response: %w", err)
	}
	applied := make([]string, 0, len(decoded.Assignees))
	for _, a := range decoded.Assignees {
		applied = append(applied, a.Login)
	}
	return applied, nil
}

// RemoveIssueAssignees removes assignees from a GitHub issue. Requires repo write permission.
func (c *Client) RemoveIssueAssignees(ctx context.Context, accessToken string, fullName string, issueNumber int, logins []string) error {
	if issueNumber <= 0 || len(logins) == 0 {
		return fmt.Errorf("invalid issue number or assignees")
	}
	owner, repo, err := splitFullName(fullName)
	if err != nil {
		return err
	}

	u := "https://api.github.com/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/issues/" + fmt.Sprintf("%d", issueNumber) + "/assignees"
	payload := map[string][]string{"assignees": logins}
	b, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		ghErr := parseGitHubAPIError(resp)
		if apiErr, ok := ghErr.(*GitHubAPIError); ok {
			if apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 {
				return fiber.NewError(apiErr.StatusCode, apiErr.Message)
			}
		}
		return ghErr
	}
	return nil
}
