package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func stubRepos(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	original := pullsAPIBase
	pullsAPIBase = srv.URL + "/"
	t.Cleanup(func() {
		pullsAPIBase = original
		srv.Close()
	})
}

func TestRepoAdmins(t *testing.T) {
	stubRepos(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("permission") != "admin" {
			t.Errorf("permission filter = %q, want admin", r.URL.Query().Get("permission"))
		}
		_, _ = w.Write([]byte(`[{"login":"Maintainer-Alice"},{"login":"owner-bob"}]`))
	})

	admins, err := NewClient().RepoAdmins(context.Background(), "tok", "acme/widgets")
	if err != nil {
		t.Fatalf("RepoAdmins: %v", err)
	}
	// Lowercased, because GitHub logins are case-insensitive and the caller
	// compares against a PR author login of unknown case.
	if !admins["maintainer-alice"] || !admins["owner-bob"] {
		t.Errorf("admins = %v, want both logins lowercased", admins)
	}
}

// An unreadable permission list must surface as an error. Returning an empty
// set would read as "nobody is an admin", which is the direction that lets a
// maintainer's second account collect on their own repo.
func TestRepoAdmins_ErrorsRatherThanReturningEmpty(t *testing.T) {
	stubRepos(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	})

	admins, err := NewClient().RepoAdmins(context.Background(), "tok", "acme/widgets")
	if err == nil {
		t.Fatal("no error for an unreadable collaborator list")
	}
	if admins != nil {
		t.Errorf("admins = %v, want nil on error", admins)
	}
}

func TestCommitCIStatus(t *testing.T) {
	tests := []struct {
		name      string
		checkRuns string
		combined  string
		want      *bool
	}{
		{"a failing check run fails", `{"check_runs":[{"conclusion":"success"},{"conclusion":"failure"}]}`, `{"state":"success"}`, boolp(false)},
		{"a timed-out check run fails", `{"check_runs":[{"conclusion":"timed_out"}]}`, `{"state":"success"}`, boolp(false)},
		{"all-success check runs pass", `{"check_runs":[{"conclusion":"success"},{"conclusion":"skipped"}]}`, `{"state":"pending"}`, boolp(true)},
		{"legacy commit status failure fails", `{"check_runs":[]}`, `{"state":"failure"}`, boolp(false)},
		{"legacy commit status success passes", `{"check_runs":[]}`, `{"state":"success"}`, boolp(true)},
		// The case that matters most: a repo with no CI at all must be
		// unknown, not failing, or every contributor to it is rejected.
		{"no CI anywhere is unknown", `{"check_runs":[]}`, `{"state":"pending"}`, nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stubRepos(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/check-runs") {
					_, _ = w.Write([]byte(tc.checkRuns))
					return
				}
				_, _ = w.Write([]byte(tc.combined))
			})

			got, err := NewClient().CommitCIStatus(context.Background(), "tok", "acme/widgets", "abc123")
			if err != nil {
				t.Fatalf("CommitCIStatus: %v", err)
			}
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("got %v, want unknown", *got)
			case tc.want != nil && got == nil:
				t.Errorf("got unknown, want %v", *tc.want)
			case tc.want != nil && got != nil && *got != *tc.want:
				t.Errorf("got %v, want %v", *got, *tc.want)
			}
		})
	}
}

func TestCommitCIStatus_EmptySHAIsUnknown(t *testing.T) {
	stubRepos(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no HTTP call should be made without a SHA")
	})
	got, err := NewClient().CommitCIStatus(context.Background(), "tok", "acme/widgets", "")
	if err != nil || got != nil {
		t.Errorf("got (%v, %v), want (nil, nil)", got, err)
	}
}

func boolp(b bool) *bool { return &b }
