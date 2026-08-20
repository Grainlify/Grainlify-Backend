package notifications

import (
	"strings"
	"testing"
)

// Every link a notification can carry is built here, once.
//
// The reason is a run of production bugs that were all the same bug: a path
// written by hand at the call site, drifting from the path the app actually
// serves. Two spellings of a settings route that does not exist. A maintainer
// sent to the contributor view of their own queue. Four separate copies of the
// issue path inside issue_applications.go, one of which was still pointing at
// Browse from a bot comment addressed to maintainers.

// Every builder must produce a path the app actually routes: the dashboard,
// with everything else as query parameters. A link that 404s is worse than no
// link - it reads as the product being broken rather than the message being
// wrong.
func TestEveryLinkBuilderTargetsTheDashboardRoute(t *testing.T) {
	links := map[string]string{
		"SettingsLink":              SettingsLink(SubtabBilling).String(),
		"ProjectLink":               ProjectLink("11111111-1111-1111-1111-111111111111").String(),
		"IssueLink":                 IssueLink("22222222-2222-2222-2222-222222222222", 99).String(),
		"MaintainerApplicationLink": MaintainerApplicationLink("33333333-3333-3333-3333-333333333333", 42).String(),
		"MyApplicationsLink":        MyApplicationsLink().String(),
	}
	for name, got := range links {
		if !strings.HasPrefix(got, DashboardPath+"?") {
			t.Errorf("%s = %q, want a %s?... path - there is no other routed surface", name, got, DashboardPath)
		}
		if strings.Contains(got, "/settings") {
			t.Errorf("%s = %q: /settings is not a route, it is a query parameter", name, got)
		}
	}
}

// The maintainer link is the one that has already been wrong in production,
// and the properties that made it wrong are worth naming individually.
func TestMaintainerApplicationLink_CarriesTheViewAndTheMaintainerTab(t *testing.T) {
	got := MaintainerApplicationLink("abc", 7).String()
	for _, want := range []string{"tab=maintainers", "view=maintainer", "project=abc", "issue=7"} {
		if !strings.Contains(got, want) {
			t.Errorf("MaintainerApplicationLink missing %q: %s", want, got)
		}
	}
	// The specific regression: it must not be the contributor view of the
	// issue, which renders Withdraw where Reject/Assign should be.
	if strings.Contains(got, "tab=browse") {
		t.Errorf("maintainer link points at Browse, the contributor view: %s", got)
	}
}

// AbsoluteLink exists so an absolute URL is the same path with a host in
// front, never a second hand-written copy.
func TestAbsoluteLink_IsTheSamePathWithAHost(t *testing.T) {
	link := IssueLink("p1", 5)
	path := link.String()

	if got, want := AbsoluteLink("https://grainlify.com", link), "https://grainlify.com"+path; got != want {
		t.Errorf("AbsoluteLink = %q, want %q", got, want)
	}
	// Trailing slashes and whitespace must not produce a double slash.
	if got := AbsoluteLink("  https://grainlify.com/  ", link); strings.Contains(got, ".com//") {
		t.Errorf("AbsoluteLink produced a double slash: %q", got)
	}
	// No base, or a non-URL base, degrades to the relative path rather than
	// emitting something broken like "example.com/dashboard?...".
	for _, base := range []string{"", "   ", "not-a-url", "grainlify.com"} {
		if got := AbsoluteLink(base, link); got != path {
			t.Errorf("AbsoluteLink(%q) = %q, want the bare path %q", base, got, path)
		}
	}
}

// A contributor's own application belongs on their board, not on the issue.
func TestMyApplicationsLink_PointsAtTheContributionsBoard(t *testing.T) {
	got := MyApplicationsLink().String()
	if !strings.Contains(got, "tab=contributors") {
		t.Errorf("MyApplicationsLink = %q, want the contributors tab", got)
	}
}
