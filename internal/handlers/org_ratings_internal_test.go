package handlers

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// orgRatingsFxPRIDBase/Seq mint unique github_pr_id values across this
// file's tests, matching projectsFxNextGHUserID's pattern in the sibling
// handlers_test package (unreachable from here across the package
// boundary, so duplicated locally).
var orgRatingsFxPRIDBase = time.Now().UnixNano()
var orgRatingsFxPRIDSeq int64

func orgRatingsFxNextPRID() int64 {
	return orgRatingsFxPRIDBase + atomic.AddInt64(&orgRatingsFxPRIDSeq, 1)
}

// orgRatingsFxProject inserts a minimal verified, non-deleted projects row
// under orgLogin (github_full_name = "<orgLogin>/repo-<unique>") and
// returns its id. Mirrors projectsFxInsertProject's column list (that
// helper lives in the external handlers_test package and can't be called
// from here).
func orgRatingsFxProject(t *testing.T, pool db.DBPool, ownerUserID uuid.UUID, orgLogin string, starsCount *int) uuid.UUID {
	t.Helper()
	fullName := fmt.Sprintf("%s/repo-%s", orgLogin, uuid.New().String()[:8])
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
INSERT INTO projects (owner_user_id, github_full_name, status, needs_metadata, deleted_at, tags, stars_count)
VALUES ($1, $2, 'verified', false, NULL, $3, $4)
RETURNING id
`, ownerUserID, fullName, []byte("[]"), starsCount).Scan(&id)
	if err != nil {
		t.Fatalf("orgRatingsFxProject: insert project: %v", err)
	}
	return id
}

// orgRatingsFxPR inserts a github_pull_requests row for projectID with
// merged as an explicit parameter (existing test helpers in this repo all
// hardcode merged=false, unusable for eligibility tests that need merged
// PRs).
func orgRatingsFxPR(t *testing.T, pool db.DBPool, projectID uuid.UUID, number int, authorLogin string, merged bool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO github_pull_requests (project_id, github_pr_id, number, state, title, author_login, url, merged)
VALUES ($1, $2, $3, 'closed', $4, $5, $6, $7)
`, projectID, orgRatingsFxNextPRID(), number, "pr-"+uuid.New().String()[:8], authorLogin,
		fmt.Sprintf("https://github.com/test/test/pull/%d", number), merged)
	if err != nil {
		t.Fatalf("orgRatingsFxPR: insert: %v", err)
	}
}

func TestHasEligibleMergedPR_TrueForMergedPRInOrg(t *testing.T) {
	d := referralsTestDB(t)
	owner := referralsCreateUser(t, d)
	orgLogin := "org-" + uuid.New().String()[:8]
	projectID := orgRatingsFxProject(t, d.Pool, owner, orgLogin, nil)
	authorLogin := "author-" + uuid.New().String()[:8]
	orgRatingsFxPR(t, d.Pool, projectID, 1, authorLogin, true)

	eligible, err := hasEligibleMergedPR(context.Background(), d.Pool, authorLogin, orgLogin)
	if err != nil {
		t.Fatalf("hasEligibleMergedPR() error = %v", err)
	}
	if !eligible {
		t.Error("hasEligibleMergedPR() = false, want true for a merged PR by this author in this org")
	}
}

func TestHasEligibleMergedPR_FalseWhenNotMerged(t *testing.T) {
	d := referralsTestDB(t)
	owner := referralsCreateUser(t, d)
	orgLogin := "org-" + uuid.New().String()[:8]
	projectID := orgRatingsFxProject(t, d.Pool, owner, orgLogin, nil)
	authorLogin := "author-" + uuid.New().String()[:8]
	orgRatingsFxPR(t, d.Pool, projectID, 1, authorLogin, false)

	eligible, err := hasEligibleMergedPR(context.Background(), d.Pool, authorLogin, orgLogin)
	if err != nil {
		t.Fatalf("hasEligibleMergedPR() error = %v", err)
	}
	if eligible {
		t.Error("hasEligibleMergedPR() = true, want false for an unmerged PR")
	}
}

func TestHasEligibleMergedPR_FalseForDifferentOrg(t *testing.T) {
	d := referralsTestDB(t)
	owner := referralsCreateUser(t, d)
	orgLogin := "org-" + uuid.New().String()[:8]
	projectID := orgRatingsFxProject(t, d.Pool, owner, orgLogin, nil)
	authorLogin := "author-" + uuid.New().String()[:8]
	orgRatingsFxPR(t, d.Pool, projectID, 1, authorLogin, true)

	eligible, err := hasEligibleMergedPR(context.Background(), d.Pool, authorLogin, "different-org-"+uuid.New().String()[:8])
	if err != nil {
		t.Fatalf("hasEligibleMergedPR() error = %v", err)
	}
	if eligible {
		t.Error("hasEligibleMergedPR() = true, want false for a merged PR under a different org")
	}
}

func TestHasEligibleMergedPR_FalseForUnrelatedLogin(t *testing.T) {
	d := referralsTestDB(t)
	owner := referralsCreateUser(t, d)
	orgLogin := "org-" + uuid.New().String()[:8]
	projectID := orgRatingsFxProject(t, d.Pool, owner, orgLogin, nil)
	orgRatingsFxPR(t, d.Pool, projectID, 1, "someone-else-"+uuid.New().String()[:8], true)

	eligible, err := hasEligibleMergedPR(context.Background(), d.Pool, "not-the-author-"+uuid.New().String()[:8], orgLogin)
	if err != nil {
		t.Fatalf("hasEligibleMergedPR() error = %v", err)
	}
	if eligible {
		t.Error("hasEligibleMergedPR() = true, want false for a login with no PRs in this org at all")
	}
}

func TestIsOrgOwner(t *testing.T) {
	d := referralsTestDB(t)
	owner := referralsCreateUser(t, d)
	orgLogin := "org-" + uuid.New().String()[:8]
	orgRatingsFxProject(t, d.Pool, owner, orgLogin, nil)

	isOwner, err := isOrgOwner(context.Background(), d.Pool, owner, orgLogin)
	if err != nil {
		t.Fatalf("isOrgOwner() error = %v", err)
	}
	if !isOwner {
		t.Error("isOrgOwner() = false, want true for the project's owner_user_id")
	}

	other := referralsCreateUser(t, d)
	isOwner, err = isOrgOwner(context.Background(), d.Pool, other, orgLogin)
	if err != nil {
		t.Fatalf("isOrgOwner() error = %v", err)
	}
	if isOwner {
		t.Error("isOrgOwner() = true, want false for a user unrelated to this org")
	}
}

func TestCanRateOrg_ExcludesSelfRating(t *testing.T) {
	d := referralsTestDB(t)
	owner := referralsCreateUser(t, d)
	orgLogin := "org-" + uuid.New().String()[:8]
	projectID := orgRatingsFxProject(t, d.Pool, owner, orgLogin, nil)

	// The org's own owner self-merges a PR into their own repo - must not be
	// enough to earn rating eligibility for their own org.
	ownerLogin := "owner-login-" + uuid.New().String()[:8]
	orgRatingsFxPR(t, d.Pool, projectID, 1, ownerLogin, true)

	canRate, err := canRateOrg(context.Background(), d.Pool, owner, ownerLogin, orgLogin)
	if err != nil {
		t.Fatalf("canRateOrg() error = %v", err)
	}
	if canRate {
		t.Error("canRateOrg() = true, want false: an org's own owner must not be able to rate their own org even with a merged PR")
	}
}

func TestCanRateOrg_TrueForEligibleNonOwner(t *testing.T) {
	d := referralsTestDB(t)
	owner := referralsCreateUser(t, d)
	orgLogin := "org-" + uuid.New().String()[:8]
	projectID := orgRatingsFxProject(t, d.Pool, owner, orgLogin, nil)

	contributor := referralsCreateUser(t, d)
	contributorLogin := "contributor-login-" + uuid.New().String()[:8]
	orgRatingsFxPR(t, d.Pool, projectID, 1, contributorLogin, true)

	canRate, err := canRateOrg(context.Background(), d.Pool, contributor, contributorLogin, orgLogin)
	if err != nil {
		t.Fatalf("canRateOrg() error = %v", err)
	}
	if !canRate {
		t.Error("canRateOrg() = false, want true for a non-owner with a merged PR in this org")
	}
}

func TestCanRateOrg_FalseWithoutMergedPR(t *testing.T) {
	d := referralsTestDB(t)
	owner := referralsCreateUser(t, d)
	orgLogin := "org-" + uuid.New().String()[:8]
	orgRatingsFxProject(t, d.Pool, owner, orgLogin, nil)

	other := referralsCreateUser(t, d)
	canRate, err := canRateOrg(context.Background(), d.Pool, other, "no-prs-login-"+uuid.New().String()[:8], orgLogin)
	if err != nil {
		t.Fatalf("canRateOrg() error = %v", err)
	}
	if canRate {
		t.Error("canRateOrg() = true, want false for a user with no merged PR in this org")
	}
}

func TestOrgRankPosition_RanksHigherContributorCountAhead(t *testing.T) {
	d := referralsTestDB(t)
	owner := referralsCreateUser(t, d)

	orgA := "org-a-" + uuid.New().String()[:8]
	projA := orgRatingsFxProject(t, d.Pool, owner, orgA, nil)
	for i := 0; i < 3; i++ {
		orgRatingsFxPR(t, d.Pool, projA, i+1, fmt.Sprintf("contrib-a-%d-%s", i, uuid.New().String()[:8]), true)
	}

	orgB := "org-b-" + uuid.New().String()[:8]
	projB := orgRatingsFxProject(t, d.Pool, owner, orgB, nil)
	orgRatingsFxPR(t, d.Pool, projB, 1, "contrib-b-"+uuid.New().String()[:8], true)

	posA, err := orgRankPosition(context.Background(), d.Pool, orgA)
	if err != nil {
		t.Fatalf("orgRankPosition(orgA) error = %v", err)
	}
	posB, err := orgRankPosition(context.Background(), d.Pool, orgB)
	if err != nil {
		t.Fatalf("orgRankPosition(orgB) error = %v", err)
	}
	if posA == nil || posB == nil {
		t.Fatalf("expected both orgs to be ranked, got posA=%v posB=%v", posA, posB)
	}
	if *posA >= *posB {
		t.Errorf("orgA (3 contributors) rank position = %d, orgB (1 contributor) rank position = %d; want orgA ranked ahead (smaller position number)", *posA, *posB)
	}
}

func TestOrgRankPosition_NilForOrgWithNoContributors(t *testing.T) {
	d := referralsTestDB(t)
	pos, err := orgRankPosition(context.Background(), d.Pool, "no-such-org-"+uuid.New().String())
	if err != nil {
		t.Fatalf("orgRankPosition() error = %v", err)
	}
	if pos != nil {
		t.Errorf("orgRankPosition() = %v, want nil for an org with zero ranked contributors", *pos)
	}
}
