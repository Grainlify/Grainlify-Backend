package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// HasCommitBefore reports whether fullName has at least one commit strictly
// before the given time - used for AI-specs.md §2.1's "repo had commits
// before the hackathon was announced" signal. Existence-only: a single
// per_page=1 request against the `until` filter is sufficient, no need to
// paginate or count.
func (c *Client) HasCommitBefore(ctx context.Context, accessToken, fullName string, before time.Time) (bool, error) {
	owner, repo, err := splitFullName(fullName)
	if err != nil {
		return false, err
	}
	u, _ := url.Parse("https://api.github.com/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/commits")
	q := u.Query()
	q.Set("until", before.UTC().Format(time.RFC3339))
	q.Set("per_page", "1")
	u.RawQuery = q.Encode()

	items, err := c.getJSONArray(ctx, accessToken, u.String())
	if err != nil {
		return false, err
	}
	return len(items) > 0, nil
}

// CountCommitsSince counts commits on fullName's default branch since the
// given time, paginated up to capPages pages of 100 - capped rather than
// exhaustive since this only backs an admin-facing signal, not a billing or
// eligibility decision. capped is true if the cap was hit (the real count
// may be higher).
func (c *Client) CountCommitsSince(ctx context.Context, accessToken, fullName string, since time.Time, capPages int) (count int, capped bool, err error) {
	owner, repo, err := splitFullName(fullName)
	if err != nil {
		return 0, false, err
	}
	for page := 1; page <= capPages; page++ {
		u, _ := url.Parse("https://api.github.com/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/commits")
		q := u.Query()
		q.Set("since", since.UTC().Format(time.RFC3339))
		q.Set("per_page", "100")
		q.Set("page", strconv.Itoa(page))
		u.RawQuery = q.Encode()

		items, err := c.getJSONArray(ctx, accessToken, u.String())
		if err != nil {
			return count, false, err
		}
		count += len(items)
		if len(items) < 100 {
			return count, false, nil
		}
		if page == capPages {
			return count, true, nil
		}
	}
	return count, true, nil
}

// CountContributors counts distinct contributors to fullName, paginated up
// to capPages pages of 100. capped is true if the cap was hit.
func (c *Client) CountContributors(ctx context.Context, accessToken, fullName string, capPages int) (count int, capped bool, err error) {
	owner, repo, err := splitFullName(fullName)
	if err != nil {
		return 0, false, err
	}
	for page := 1; page <= capPages; page++ {
		u, _ := url.Parse("https://api.github.com/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/contributors")
		q := u.Query()
		q.Set("per_page", "100")
		q.Set("page", strconv.Itoa(page))
		u.RawQuery = q.Encode()

		items, err := c.getJSONArray(ctx, accessToken, u.String())
		if err != nil {
			return count, false, err
		}
		count += len(items)
		if len(items) < 100 {
			return count, false, nil
		}
		if page == capPages {
			return count, true, nil
		}
	}
	return count, true, nil
}

// PRReview is the subset of a GitHub PR review this package needs.
type PRReview struct {
	SubmittedAt *string `json:"submitted_at"`
}

// ListPRReviews fetches every review left on PR number.
func (c *Client) ListPRReviews(ctx context.Context, accessToken, fullName string, number int) ([]PRReview, error) {
	owner, repo, err := splitFullName(fullName)
	if err != nil {
		return nil, err
	}
	u := "https://api.github.com/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/pulls/" + strconv.Itoa(number) + "/reviews?per_page=100"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
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
		return nil, parseGitHubAPIError(resp)
	}

	var reviews []PRReview
	if err := json.NewDecoder(resp.Body).Decode(&reviews); err != nil {
		return nil, err
	}
	return reviews, nil
}

// MedianTimeToFirstReviewHours samples up to sampleSize recently-updated PRs
// (any state) and returns the median time from PR creation to its first
// review, in hours - AI-specs.md §2.1's "median time-to-first-review on
// recent PRs" signal, a proxy for maintainer responsiveness. Returns nil
// (not an error) if no sampled PR has any review yet - that's a valid
// "no data" outcome, not a failure.
func (c *Client) MedianTimeToFirstReviewHours(ctx context.Context, accessToken, fullName string, sampleSize int) (*float64, error) {
	prs, err := c.ListPRsPage(ctx, accessToken, fullName, 1)
	if err != nil {
		return nil, err
	}
	if len(prs) > sampleSize {
		prs = prs[:sampleSize]
	}

	var hours []float64
	for _, pr := range prs {
		if pr.CreatedAt == nil {
			continue
		}
		created, err := time.Parse(time.RFC3339, *pr.CreatedAt)
		if err != nil {
			continue
		}
		reviews, err := c.ListPRReviews(ctx, accessToken, fullName, pr.Number)
		if err != nil || len(reviews) == 0 {
			continue
		}
		var first *time.Time
		for _, r := range reviews {
			if r.SubmittedAt == nil {
				continue
			}
			t, err := time.Parse(time.RFC3339, *r.SubmittedAt)
			if err != nil {
				continue
			}
			if first == nil || t.Before(*first) {
				first = &t
			}
		}
		if first == nil {
			continue
		}
		hours = append(hours, first.Sub(created).Hours())
	}

	if len(hours) == 0 {
		return nil, nil
	}
	sort.Float64s(hours)
	mid := len(hours) / 2
	var median float64
	if len(hours)%2 == 0 {
		median = (hours[mid-1] + hours[mid]) / 2
	} else {
		median = hours[mid]
	}
	return &median, nil
}

// getJSONArray is a small shared helper for the count/existence endpoints
// above, which only ever need "how many items came back", not typed fields.
func (c *Client) getJSONArray(ctx context.Context, accessToken, u string) ([]json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(accessToken) != "" {
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
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, parseGitHubAPIError(resp)
	}

	var items []json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, err
	}
	return items, nil
}
