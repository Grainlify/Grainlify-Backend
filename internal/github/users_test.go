package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func withStub(t *testing.T, target *string, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	original := *target
	*target = srv.URL
	t.Cleanup(func() {
		*target = original
		srv.Close()
	})
}

func TestGetPublicUser(t *testing.T) {
	withStub(t, &publicUserAPIBase, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/octocat") {
			t.Errorf("path = %q, want it to end in /octocat", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"login":"octocat","type":"User","created_at":"2015-01-02T00:00:00Z","public_repos":7}`))
	})
	// httptest gives a bare origin; the client appends the login directly.
	publicUserAPIBase += "/"

	u, err := NewClient().GetPublicUser(context.Background(), "tok", "octocat")
	if err != nil {
		t.Fatalf("GetPublicUser: %v", err)
	}
	if u.Login != "octocat" || u.Type != "User" || u.PublicRepos != 7 {
		t.Errorf("got %+v, want octocat/User/7", u)
	}
	if u.CreatedAt.Year() != 2015 {
		t.Errorf("CreatedAt = %v, want 2015 - the account-age gate measures against this", u.CreatedAt)
	}
}

// The three IsOrgMember outcomes matter individually: 204 blocks the
// applicant, 404 lets them through, and 302 must surface as an error rather
// than being read as "not a member" - see the doc comment on IsOrgMember.
func TestIsOrgMember(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		wantMember bool
		wantErr    bool
	}{
		{"204 means member", http.StatusNoContent, true, false},
		{"404 means not a member", http.StatusNotFound, false, false},
		{"302 means the token cannot see membership", http.StatusFound, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withStub(t, &orgAPIBase, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			})
			orgAPIBase += "/"

			member, err := NewClient().IsOrgMember(context.Background(), "tok", "acme", "octocat")
			if member != tc.wantMember {
				t.Errorf("member = %v, want %v", member, tc.wantMember)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// TestUserHasPublicActivityBefore is the determinate half of the
// pre-event-activity gate: given a working token, a zero result really does
// mean "no history", and that is what makes the gate enforceable rather
// than permanently failing open.
func TestUserHasPublicActivityBefore(t *testing.T) {
	announced := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	t.Run("results present means prior activity", func(t *testing.T) {
		withStub(t, &searchAPIBase, func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query().Get("q")
			if !strings.Contains(q, "author:octocat") || !strings.Contains(q, "author-date:<2026-08-01") {
				t.Errorf("query = %q, want it scoped to the author and to before announced_at", q)
			}
			_, _ = w.Write([]byte(`{"total_count":12}`))
		})
		got, err := NewClient().UserHasPublicActivityBefore(context.Background(), "tok", "octocat", announced, 1)
		if err != nil || !got {
			t.Errorf("got %v, err %v; want true", got, err)
		}
	})

	t.Run("zero results means no prior activity", func(t *testing.T) {
		withStub(t, &searchAPIBase, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"total_count":0}`))
		})
		got, err := NewClient().UserHasPublicActivityBefore(context.Background(), "tok", "brand-new", announced, 1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got {
			t.Error("got true for an account with no indexed commits before the announcement")
		}
	})

	t.Run("an API error is reported, not silently false", func(t *testing.T) {
		withStub(t, &searchAPIBase, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"rate limit exceeded"}`))
		})
		_, err := NewClient().UserHasPublicActivityBefore(context.Background(), "tok", "octocat", announced, 1)
		if err == nil {
			t.Error("a rate-limited search returned no error - the gate would read it as 'no history' and reject a real contributor")
		}
	})

	t.Run("minCommits below 1 short-circuits without a call", func(t *testing.T) {
		withStub(t, &searchAPIBase, func(w http.ResponseWriter, r *http.Request) {
			t.Error("no HTTP call should be made when the check is disabled")
		})
		got, err := NewClient().UserHasPublicActivityBefore(context.Background(), "tok", "octocat", announced, 0)
		if err != nil || !got {
			t.Errorf("got %v, err %v; want true", got, err)
		}
	})
}
