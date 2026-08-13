package calibration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// GitHub access for the labelling tool.
//
// Deliberately its own small client rather than internal/github: this package
// is not part of the product, and it should not be able to drag product code
// along behind it or grow a reason to be imported by it.

// apiBase is a var so tests can point it at an httptest.Server.
var apiBase = "https://api.github.com"

// PRDetail is what one API call gives us about a pull request. additions,
// deletions and changed_files come from this endpoint directly, which is why
// sizing costs one request rather than paging the whole file list.
type PRDetail struct {
	Number       int    `json:"number"`
	Title        string `json:"title"`
	Body         string `json:"body"`
	Additions    int    `json:"additions"`
	Deletions    int    `json:"deletions"`
	ChangedFiles int    `json:"changed_files"`
	HTMLURL      string `json:"html_url"`
	User         struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

// PRFile is one file's contribution, including its patch. The patch is absent
// for binary files and omitted by GitHub on very large diffs - a caller must
// treat a missing patch as "not shown", never as "no change".
type PRFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Patch     string `json:"patch"`
}

// Issue is the linked issue, fetched so its body can be frozen alongside the
// diff. Issues get edited; a labeller and a later reader must see the same
// acceptance criteria.
type Issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
}

// GitHubClient is a thin authenticated reader.
type GitHubClient struct {
	Token string
	HTTP  *http.Client
}

func NewGitHubClient(token string) *GitHubClient {
	return &GitHubClient{Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

func (g *GitHubClient) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", apiBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "grainlify-calibration")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}

	resp, err := g.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Rate limiting is worth naming rather than surfacing as a generic 403:
	// a draw that dies half way through with "403" reads as a permissions
	// problem and sends someone looking in the wrong place.
	if resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0" {
		return fmt.Errorf("github rate limit exhausted; resets at %s", resp.Header.Get("X-RateLimit-Reset"))
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (g *GitHubClient) GetPR(ctx context.Context, fullName string, number int) (PRDetail, error) {
	var pr PRDetail
	err := g.get(ctx, fmt.Sprintf("/repos/%s/pulls/%d", fullName, number), &pr)
	return pr, err
}

// ListFiles fetches up to maxFiles of a pull request's files.
//
// Truncation is reported, never hidden: a labeller shown a partial diff must
// know it is partial, or they are labelling something other than the change.
func (g *GitHubClient) ListFiles(ctx context.Context, fullName string, number, maxFiles int) (files []PRFile, truncated bool, err error) {
	const perPage = 100
	for page := 1; len(files) < maxFiles; page++ {
		var batch []PRFile
		path := fmt.Sprintf("/repos/%s/pulls/%d/files?per_page=%d&page=%d", fullName, number, perPage, page)
		if err := g.get(ctx, path, &batch); err != nil {
			return files, false, err
		}
		files = append(files, batch...)
		if len(batch) < perPage {
			return files, false, nil
		}
	}
	return files[:maxFiles], true, nil
}

func (g *GitHubClient) GetIssue(ctx context.Context, fullName string, number int) (Issue, error) {
	var is Issue
	err := g.get(ctx, fmt.Sprintf("/repos/%s/issues/%d", fullName, number), &is)
	return is, err
}

// SizeFnFor adapts the client to the sampler's SizeFn.
//
// One request per candidate, and the sampler only spends it on candidates that
// have already passed the cheap constraints - so the draw costs roughly as
// many API calls as it examines, not as many as the corpus holds.
func (g *GitHubClient) SizeFnFor() SizeFn {
	return func(ctx context.Context, c Candidate) (int, error) {
		pr, err := g.GetPR(ctx, c.ProjectFullName, c.Number)
		if err != nil {
			return 0, err
		}
		return pr.Additions + pr.Deletions, nil
	}
}

// LinkedIssueNumber extracts the issue a pull request closes.
//
// GitHub's own closing-keyword set, matched case-insensitively. About one pull
// request in nine references nothing, and that must surface as "no linked
// issue" rather than an empty panel - an empty panel reads as "the criteria
// are missing", which changes how someone labels.
func LinkedIssueNumber(body string) (int, bool) {
	lower := strings.ToLower(body)
	for _, kw := range []string{"closes", "closed", "close", "fixes", "fixed", "fix", "resolves", "resolved", "resolve"} {
		idx := 0
		for {
			i := strings.Index(lower[idx:], kw+" #")
			if i < 0 {
				break
			}
			start := idx + i + len(kw) + 2
			end := start
			for end < len(lower) && lower[end] >= '0' && lower[end] <= '9' {
				end++
			}
			if end > start {
				var n int
				if _, err := fmt.Sscanf(lower[start:end], "%d", &n); err == nil && n > 0 {
					return n, true
				}
			}
			idx = idx + i + len(kw) + 2
			if idx >= len(lower) {
				break
			}
		}
	}
	return 0, false
}
