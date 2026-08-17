package handlers

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

// Properties of the applications flow that are checked against the source,
// because Apply() and Assign() cannot be exercised end to end here - both post
// a GitHub issue comment through a real installation token before they reach
// the code under test.
//
// Source-reading is the same technique already used for the undelivered-support
// index and the kyc_verified_at writers. It catches the regression the
// behavioural tests cannot see: a correct rule expressed a second time, badly.

const issueApplicationsSrc = "issue_applications.go"

// Not one hand-written dashboard path anywhere in this file.
//
// There were four. Each was a copy of a format string that lives in
// internal/notifications/links.go, and one of them had already drifted: a bot
// comment addressed to "Repo Maintainers" linked them to Browse, the
// CONTRIBUTOR view, where the Assign and Reject controls it was telling them
// to use do not exist.
func TestIssueApplications_BuildsEveryLinkAndWritesNone(t *testing.T) {
	src := readSource(t, issueApplicationsSrc)

	// Any literal containing the dashboard path, in any quoting style.
	literal := regexp.MustCompile(`"[^"]*` + regexp.QuoteMeta(notifications.DashboardPath) + `\?[^"]*"`)
	if found := literal.FindAllString(src, -1); len(found) > 0 {
		t.Errorf("hand-written dashboard links found - build them in internal/notifications/links.go instead:\n  %s",
			strings.Join(found, "\n  "))
	}
}

// Applying notifies BOTH sides.
//
// It used to notify only the maintainer. The applicant heard nothing at all,
// which meant the first message a contributor ever received about their own
// application was its refusal - 13 of the 14 people holding an open
// application had never had a single notification about it. A refusal arriving
// out of silence reads as a system that was never listening.
func TestApply_NotifiesTheApplicantAsWellAsTheMaintainer(t *testing.T) {
	body := funcBody(t, readSource(t, issueApplicationsSrc), "func (h *IssueApplicationsHandler) Apply()")

	if !strings.Contains(body, "TypeIssueApplicationSubmitted") {
		t.Error("Apply() no longer notifies the maintainer")
	}
	if !strings.Contains(body, "TypeIssueApplicationReceived") {
		t.Error("Apply() does not notify the applicant - the applicant side is silent again, " +
			"and the next thing they hear will be a rejection")
	}
	// Addressed to the applicant, not the project owner. Notifying the owner
	// twice would satisfy a naive check for two Notify calls.
	if !strings.Contains(body, "h.notify.Notify(c.Context(), userID, notifications.TypeIssueApplicationReceived") {
		t.Error("the applicant notification is not addressed to the applicant (userID)")
	}
}

// A contributor's own application points at their board; a refusal points at
// the issue, which is still open and still real. The board deliberately does
// not carry refusals.
func TestApply_ApplicantNotificationPointsAtTheirOwnBoard(t *testing.T) {
	body := funcBody(t, readSource(t, issueApplicationsSrc), "func (h *IssueApplicationsHandler) Apply()")
	idx := strings.Index(body, "TypeIssueApplicationReceived")
	if idx < 0 {
		t.Fatal("no applicant notification")
	}
	if !strings.Contains(body[idx:], "notifications.MyApplicationsLink()") {
		t.Error("the applicant notification does not link to their applications board")
	}
}

func readSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// funcBody returns the source of one function, from its signature to the next
// top-level closing brace. Crude, and sufficient: it fails loudly if the
// signature is absent, which is the case that would otherwise make an
// assertion vacuously pass.
func funcBody(t *testing.T, src, signature string) string {
	t.Helper()
	start := strings.Index(src, signature)
	if start < 0 {
		t.Fatalf("could not find %q - if it was renamed, this test needs updating rather than deleting", signature)
	}
	rest := src[start:]
	if end := strings.Index(rest, "\n}\n"); end > 0 {
		return rest[:end]
	}
	return rest
}
