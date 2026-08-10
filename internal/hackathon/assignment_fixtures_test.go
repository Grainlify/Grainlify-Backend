package hackathon

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// fxLiveHackathon builds the whole "there is a live event with an accepted
// project" backdrop the §4 pipeline needs, so each test states only what it
// is actually about.
func fxLiveHackathon(t *testing.T, pool db.DBPool) (hackathonID, projectID, ownerID uuid.UUID) {
	t.Helper()
	ownerID = fxUser(t, pool)
	projectID = fxProject(t, pool, ownerID, "")
	hackathonID = fxHackathon(t, pool, fxHackathonSpec{Phase: "live"})

	// announced_at is load-bearing for the age gates (§3.2), and the draw
	// needs starts_at/ends_at to bound the event.
	if _, err := pool.Exec(context.Background(), `
UPDATE hackathons
SET announced_at = now() - interval '180 days',
    starts_at = now() - interval '1 day',
    ends_at = now() + interval '30 days'
WHERE id = $1
`, hackathonID); err != nil {
		t.Fatalf("fxLiveHackathon: set dates: %v", err)
	}
	fxAcceptedApplication(t, pool, hackathonID, projectID, ownerID)
	return hackathonID, projectID, ownerID
}

// fxPublishedIssue inserts a published hackathon_issues row with an open
// application window.
func fxPublishedIssue(t *testing.T, pool db.DBPool, hackathonID, projectID uuid.UUID, issueNumber int, tier string) uuid.UUID {
	t.Helper()
	if tier == "" {
		tier = "standard"
	}
	var orgLogin string
	if err := pool.QueryRow(context.Background(),
		`SELECT SPLIT_PART(github_full_name, '/', 1) FROM projects WHERE id = $1`, projectID).Scan(&orgLogin); err != nil {
		t.Fatalf("fxPublishedIssue: org: %v", err)
	}
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
INSERT INTO hackathon_issues
  (hackathon_id, project_id, issue_number, org_login, status, acceptance_criteria, difficulty_tier,
   reserved, application_window_opens_at, application_window_closes_at, published_at)
VALUES ($1,$2,$3,$4,'published','criteria',$5,false, now() - interval '1 hour', now() + interval '1 hour', now())
RETURNING id
`, hackathonID, projectID, issueNumber, orgLogin, tier).Scan(&id)
	if err != nil {
		t.Fatalf("fxPublishedIssue: %v", err)
	}
	return id
}

// fxGitHubAccount links a users row to a GitHub login, which the gates and
// the apply path both require.
func fxGitHubAccount(t *testing.T, pool db.DBPool, userID uuid.UUID, login string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO github_accounts (user_id, github_user_id, login, access_token)
VALUES ($1, $2, $3, '\x00'::bytea)
ON CONFLICT (user_id) DO UPDATE SET login = EXCLUDED.login
`, userID, fxNextGHUserID(), login); err != nil {
		t.Fatalf("fxGitHubAccount: %v", err)
	}
}

// fxApplication inserts an application in 'applied' status with a given fit,
// bypassing the gates - tests for the draw shouldn't have to satisfy Layer 1
// to set up a pool.
func fxApplication(t *testing.T, pool db.DBPool, hackathonID, issueID, userID uuid.UUID, login, fit string) uuid.UUID {
	t.Helper()
	if fit == "" {
		fit = "plausible"
	}
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
INSERT INTO hackathon_issue_applications
  (hackathon_id, hackathon_issue_id, user_id, github_login, status, fit, difficulty_match, fit_assessed_at)
VALUES ($1,$2,$3,$4,'applied',$5,'matched',now())
RETURNING id
`, hackathonID, issueID, userID, login, fit).Scan(&id)
	if err != nil {
		t.Fatalf("fxApplication: %v", err)
	}
	return id
}

// fxApplicant creates a user + github account + application in one step.
func fxApplicant(t *testing.T, pool db.DBPool, hackathonID, issueID uuid.UUID, login, fit string) uuid.UUID {
	t.Helper()
	userID := fxUser(t, pool)
	fxGitHubAccount(t, pool, userID, login)
	fxApplication(t, pool, hackathonID, issueID, userID, login, fit)
	return userID
}

// fxCloseWindow backdates an issue's application window so the runner and
// draw treat it as due.
func fxCloseWindow(t *testing.T, pool db.DBPool, issueID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
UPDATE hackathon_issues
SET application_window_opens_at = now() - interval '2 hours',
    application_window_closes_at = now() - interval '1 minute'
WHERE id = $1
`, issueID); err != nil {
		t.Fatalf("fxCloseWindow: %v", err)
	}
}

// fxSetConfig writes a per-hackathon config override directly, skipping the
// audit machinery SetValue would exercise.
func fxSetConfig(t *testing.T, pool db.DBPool, hackathonID uuid.UUID, key, value string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO hackathon_config_settings (hackathon_id, key, value)
VALUES ($1,$2,$3)
ON CONFLICT (hackathon_id, key) WHERE hackathon_id IS NOT NULL
DO UPDATE SET value = EXCLUDED.value, updated_at = now()
`, hackathonID, key, value); err != nil {
		t.Fatalf("fxSetConfig(%s): %v", key, err)
	}
}

// fxAssignment inserts an assignment directly, for tests about the lifecycle
// rather than about how the assignment was won.
func fxAssignment(t *testing.T, pool db.DBPool, hackathonID, issueID, projectID, userID uuid.UUID, issueNumber int, login string, staleAt *time.Time) uuid.UUID {
	t.Helper()
	var orgLogin string
	if err := pool.QueryRow(context.Background(),
		`SELECT SPLIT_PART(github_full_name, '/', 1) FROM projects WHERE id = $1`, projectID).Scan(&orgLogin); err != nil {
		t.Fatalf("fxAssignment: org: %v", err)
	}
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
INSERT INTO hackathon_assignments
  (hackathon_id, hackathon_issue_id, project_id, issue_number, user_id, github_login, org_login,
   status, holds_slot, stale_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,'active',true,$8)
RETURNING id
`, hackathonID, issueID, projectID, issueNumber, userID, login, orgLogin, staleAt).Scan(&id)
	if err != nil {
		t.Fatalf("fxAssignment: %v", err)
	}
	return id
}

func countRows(t *testing.T, pool db.DBPool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("countRows: %v", err)
	}
	return n
}
