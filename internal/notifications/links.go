package notifications

import (
	"fmt"
	"net/url"
	"strings"
)

// Where a notification sends somebody.
//
// These were written as literals at each call site, in two different spellings
// - "/settings?tab=rewards" and "/settings?subtab=rewards" - and BOTH are
// wrong. There is no /settings route. React Router logs
// `No routes matched location "/settings?subtab=rewards"`, the page renders
// blank, and a refresh does not recover it; only the browser back button does.
//
// Every settings surface lives under the dashboard route, selected by query
// parameters: /dashboard?tab=settings&subtab=<name>. Building the path in one
// place is the point - five call sites each writing their own is how two
// spellings of the same wrong path ended up in production.

// SettingsSubtab is a settings screen a notification can point at. The values
// match SettingsPage's VALID_TABS; anything else falls back to Profile there,
// which silently sends somebody to the wrong screen rather than erroring.
type SettingsSubtab string

const (
	SubtabProfile       SettingsSubtab = "profile"
	SubtabNotifications SettingsSubtab = "notifications"
	SubtabReferrals     SettingsSubtab = "referrals"
	SubtabRewards       SettingsSubtab = "rewards"
	SubtabPayout        SettingsSubtab = "payout"
	SubtabBilling       SettingsSubtab = "billing"
	SubtabTerms         SettingsSubtab = "terms"
)

// DashboardPath is the only routed path the app serves for signed-in surfaces.
// Everything else is a query parameter on it.
const DashboardPath = "/dashboard"

// SettingsLink builds a link to a settings subtab.
func SettingsLink(subtab SettingsSubtab) string {
	return fmt.Sprintf("%s?tab=settings&subtab=%s", DashboardPath, url.QueryEscape(string(subtab)))
}

// ProjectLink builds a link to a project inside Browse.
func ProjectLink(projectID string) string {
	return fmt.Sprintf("%s?tab=browse&project=%s", DashboardPath, url.QueryEscape(projectID))
}

// MaintainerApplicationLink points a maintainer at an application they can
// actually act on.
//
// This used to be an IssueLink - /dashboard?tab=browse&... - which is the
// CONTRIBUTOR view of the issue. A maintainer clicking their own "new
// application" notification landed on a screen rendering Withdraw instead of
// Reject/Assign/Unassign, because those actions live on the maintainer
// surface. Not hidden behind a click: absent from the page they were sent to.
//
// Fifteen active maintainers had resolved one application between them. This
// is the likeliest reason: the notification led somewhere the work could not
// be done, and the surface where it could was reachable only through a view
// toggle that reset on every reload.
//
// view=maintainer is carried explicitly so the link works on a cold load. The
// dashboard reads it from the URL, so following this from an email or a fresh
// tab lands in the right mode rather than defaulting to contributor.
func MaintainerApplicationLink(projectID string, githubIssueID int64) string {
	return fmt.Sprintf("%s?tab=maintainers&view=maintainer&project=%s&issue=%d",
		DashboardPath, url.QueryEscape(projectID), githubIssueID)
}

// AbsoluteLink turns any of the builders below into a full URL, for somewhere
// a relative path cannot work - a GitHub issue comment, an email body.
//
// Falls back to the relative path when no base is configured, which resolves
// against the current origin if the reader happens to already be on the site
// and is dead otherwise. That is the pre-existing behaviour and is preserved
// deliberately: a comment with a half-working link is better than a handler
// that refuses to post one.
//
// This exists so an absolute link is the SAME path as the in-app one with a
// host in front, rather than a second hand-written copy of the format string.
// Four such copies had already accumulated in issue_applications.go alone.
func AbsoluteLink(baseURL, path string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" || !strings.HasPrefix(base, "http") {
		return path
	}
	return base + path
}

// MyApplicationsLink points a contributor at their own applications.
//
// The contributions board is the only place somebody can see that they applied
// to something and what became of it. A notification about a contributor's own
// application belongs here rather than on the issue: the issue shows the work,
// this shows their standing on it.
func MyApplicationsLink() string {
	return DashboardPath + "?tab=contributors"
}

// IssueLink builds a link to a specific issue inside a project.
func IssueLink(projectID string, githubIssueID int64) string {
	return fmt.Sprintf("%s?tab=browse&project=%s&issue=%d", DashboardPath, url.QueryEscape(projectID), githubIssueID)
}
