package handlers_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/config"
)

// /projects/:id/issues/public must return only OPEN issues.
//
// It had no state predicate at all, and the ordering promoted exactly the
// wrong rows: it sorts by updated_at_github DESC, and closing an issue bumps
// that timestamp, so the most recently closed sorted straight to the top.
// Measured against production before the fix: 42% of returned issues closed,
// 35% of the top ten.
//
// The closed issue below carries the NEWEST timestamp on purpose. A version of
// this test where the closed row sorted last would pass on a build with no
// filter at all, because LIMIT 50 would never reach it - the vacuous shape in
// VERIFICATION-TRAPS.md §1. Here the closed row is the first thing an
// unfiltered query returns, so the assertion cannot be satisfied by luck.
func TestIssuesPublic_ReturnsOnlyOpenIssues(t *testing.T) {
	d := testDB(t)
	owner := projectsFxUser(t, d.Pool)
	app := newProjectsPublicTestApp(config.Config{}, d)
	id := projectsFxInsertProject(t, d.Pool, projectsFxProjectSpec{
		OwnerUserID: owner, Status: "verified", NeedsMetadata: false,
	})

	closedTitle := "closed just now " + uuid.New().String()
	openTitle := "still open " + uuid.New().String()

	seed := func(number int, state, title, updated string) {
		t.Helper()
		if _, err := d.Pool.Exec(context.Background(), `
INSERT INTO github_issues (project_id, github_issue_id, number, state, title, author_login, url, updated_at_github, last_seen_at)
VALUES ($1, $2, $3, $4, $5, 'someone', '', $6::timestamptz, now())
`, id, projectsFxNextGHUserID(), number, state, title, updated); err != nil {
			t.Fatalf("seed issue #%d: %v", number, err)
		}
	}
	// Newest timestamp on the closed one - what closing an issue actually does.
	seed(9001, "closed", closedTitle, "2030-01-01T00:00:00Z")
	seed(9002, "open", openTitle, "2029-01-01T00:00:00Z")

	status, body := projectsFxDoJSON(t, app, "GET", "/projects/"+id.String()+"/issues/public", "", nil)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", status, body)
	}
	var resp struct {
		Issues []map[string]any `json:"issues"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	sawOpen := false
	for _, it := range resp.Issues {
		if it["state"] != "open" {
			t.Errorf("returned an issue in state %v (%v) - a closed issue is not something to recommend",
				it["state"], it["title"])
		}
		if it["title"] == closedTitle {
			t.Error("the closed issue was returned, and it sorted first because closing bumped its timestamp")
		}
		if it["title"] == openTitle {
			sawOpen = true
		}
	}
	// The other half: filtering must not empty the list.
	if !sawOpen {
		t.Errorf("the open issue is missing; got %d issues", len(resp.Issues))
	}
}
