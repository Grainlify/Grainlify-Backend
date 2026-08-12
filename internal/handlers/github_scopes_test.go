package handlers

import (
	"strings"
	"testing"
)

// TestGitHubLoginScopes_StayNarrow pins the set every user grants at sign-in.
//
// The two that matter are absences, and both are easy to reintroduce by
// accident while chasing an unrelated permission error:
//
//   - "repo" grants read/write on PRIVATE repositories. Nothing reads one, and
//     the sign-in page tells users we do not touch them - so requesting it made
//     that statement false for as long as it was there.
//   - "admin:repo_hook" is only needed when a maintainer registers a project.
//     Asking every contributor for webhook administration over all their
//     repositories, for a path most will never take, is consent in name only.
func TestGitHubLoginScopes_StayNarrow(t *testing.T) {
	want := []string{"read:user", "user:email", "read:org", "public_repo"}

	if len(githubLoginScopes) != len(want) {
		t.Fatalf("scopes = %v, want exactly %v", githubLoginScopes, want)
	}
	for i, w := range want {
		if githubLoginScopes[i] != w {
			t.Errorf("scope[%d] = %q, want %q", i, githubLoginScopes[i], w)
		}
	}

	for _, forbidden := range []string{"repo", "admin:repo_hook", "write:repo_hook", "delete_repo", "admin:org"} {
		for _, got := range githubLoginScopes {
			if got == forbidden {
				t.Errorf("sign-in requests %q.\n"+
					"If this is fixing a permission error: %q is almost certainly not the fix. Private-repo access "+
					"is never used, and webhook administration belongs at project registration, not at sign-up. "+
					"Check whether the call should use the GitHub App installation token instead.", forbidden, forbidden)
			}
		}
	}
}

// TestGitHubLoginScopes_MatchTheUserFacingPromise. The sign-in and sign-up
// pages both state we do not access private repositories. That claim is only
// true while "repo" is absent, so the two are tied together here rather than
// left to drift apart.
func TestGitHubLoginScopes_MatchTheUserFacingPromise(t *testing.T) {
	joined := strings.Join(githubLoginScopes, " ")
	if strings.Contains(joined, "repo") && !strings.Contains(joined, "public_repo") {
		t.Fatal("private repo access requested")
	}
	for _, s := range githubLoginScopes {
		if s == "repo" {
			t.Error("the sign-in page promises we never access private repositories; " +
				"requesting the \"repo\" scope makes that promise false")
		}
	}
}
