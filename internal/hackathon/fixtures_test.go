package hackathon

import (
	"context"
	"fmt"
	"math/rand"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// fxNextGHUserID mirrors the randomized-not-incrementing pattern established
// in internal/handlers' own test fixtures (see leaderboard_test.go) - many
// _test.go files across packages insert users concurrently in CI, so a
// counter based on package-init time risks colliding across packages.
func fxNextGHUserID() int64 {
	return rand.Int63()
}

func fxUser(t *testing.T, pool db.DBPool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
INSERT INTO users (role, display_name, github_user_id) VALUES ('contributor', $1, $2) RETURNING id
`, "hackathon-fx-user-"+uuid.New().String(), fxNextGHUserID()).Scan(&id)
	if err != nil {
		t.Fatalf("fxUser: %v", err)
	}
	return id
}

func fxAdmin(t *testing.T, pool db.DBPool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
INSERT INTO users (role, display_name, github_user_id) VALUES ('admin', $1, $2) RETURNING id
`, "hackathon-fx-admin-"+uuid.New().String(), fxNextGHUserID()).Scan(&id)
	if err != nil {
		t.Fatalf("fxAdmin: %v", err)
	}
	return id
}

// fxProject inserts a minimal verified project owned by ownerID.
func fxProject(t *testing.T, pool db.DBPool, ownerID uuid.UUID, orgLogin string) uuid.UUID {
	t.Helper()
	if orgLogin == "" {
		orgLogin = "hackathon-fx-org-" + uuid.New().String()[:8]
	}
	fullName := fmt.Sprintf("%s/repo-%s", orgLogin, uuid.New().String()[:8])
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
INSERT INTO projects (owner_user_id, github_full_name, status) VALUES ($1, $2, 'verified') RETURNING id
`, ownerID, fullName).Scan(&id)
	if err != nil {
		t.Fatalf("fxProject: %v", err)
	}
	return id
}

func fxProjectFullName(t *testing.T, pool db.DBPool, projectID uuid.UUID) string {
	t.Helper()
	var fullName string
	if err := pool.QueryRow(context.Background(), `SELECT github_full_name FROM projects WHERE id = $1`, projectID).Scan(&fullName); err != nil {
		t.Fatalf("fxProjectFullName: %v", err)
	}
	return fullName
}

// fxHackathonSpec configures fxHackathon's insert; zero values pick sane
// defaults (a draft hackathon named uniquely).
type fxHackathonSpec struct {
	Phase string // defaults to "draft"
	Name  string // defaults to a unique name
}

func fxHackathon(t *testing.T, pool db.DBPool, spec fxHackathonSpec) uuid.UUID {
	t.Helper()
	if spec.Phase == "" {
		spec.Phase = "draft"
	}
	if spec.Name == "" {
		spec.Name = "hackathon-fx-" + uuid.New().String()
	}
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
INSERT INTO hackathons (name, phase) VALUES ($1, $2) RETURNING id
`, spec.Name, spec.Phase).Scan(&id)
	if err != nil {
		t.Fatalf("fxHackathon: %v", err)
	}
	return id
}

// fxAcceptedApplication links projectID to hackathonID with an 'accepted'
// hackathon_project_applications row, the gate SyncIssueLabel/
// EffectiveGrainHackLabels require before doing anything with a project.
func fxAcceptedApplication(t *testing.T, pool db.DBPool, hackathonID, projectID, applicantID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
INSERT INTO hackathon_project_applications
  (hackathon_id, project_id, applicant_user_id, short_description, goal, expected_issue_count, maintainer_contact, status)
VALUES ($1, $2, $3, 'desc', 'goal', 5, 'contact@example.com', 'accepted')
RETURNING id
`, hackathonID, projectID, applicantID).Scan(&id)
	if err != nil {
		t.Fatalf("fxAcceptedApplication: %v", err)
	}
	return id
}
