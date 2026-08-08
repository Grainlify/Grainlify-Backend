package handlers

import (
	"errors"
	"testing"

	"github.com/jagadeesh/grainlify/backend/internal/github"
)

func TestRepoPrivacyFromFetch(t *testing.T) {
	t.Run("confirmed public repo is not private and returns owner avatar", func(t *testing.T) {
		repo := github.Repo{Private: false}
		repo.Owner.AvatarURL = "https://cdn.example/avatar.png"

		confirmedPrivate, avatar := repoPrivacyFromFetch(repo, nil)
		if confirmedPrivate {
			t.Errorf("confirmedPrivate = true, want false for a confirmed-public repo")
		}
		if avatar == nil || *avatar != "https://cdn.example/avatar.png" {
			t.Errorf("avatar = %v, want the owner's avatar URL", avatar)
		}
	})

	t.Run("confirmed private repo is private with no avatar", func(t *testing.T) {
		repo := github.Repo{Private: true}
		repo.Owner.AvatarURL = "https://cdn.example/avatar.png"

		confirmedPrivate, avatar := repoPrivacyFromFetch(repo, nil)
		if !confirmedPrivate {
			t.Errorf("confirmedPrivate = false, want true for a confirmed-private repo")
		}
		if avatar != nil {
			t.Errorf("avatar = %v, want nil (we don't enrich private repos)", avatar)
		}
	})

	// This is the actual regression case: a fetch error (rate limit, expired
	// token, transient 5xx, momentary network blip) must NOT be treated as
	// confirmation the repo is private. Before this fix, Mine() treated any
	// GetRepo() error as "private" and soft-deleted the project - so a flaky
	// GitHub API call could permanently wipe a legitimate public project from
	// a maintainer's dashboard.
	t.Run("fetch error is not evidence of privacy - does not confirm private", func(t *testing.T) {
		confirmedPrivate, avatar := repoPrivacyFromFetch(github.Repo{}, errors.New("github: rate limited"))
		if confirmedPrivate {
			t.Errorf("confirmedPrivate = true, want false: a fetch error is unknown status, not confirmed-private, and must not trigger the caller's soft-delete")
		}
		if avatar != nil {
			t.Errorf("avatar = %v, want nil on error", avatar)
		}
	})
}
