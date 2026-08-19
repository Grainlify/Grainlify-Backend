package notifications

import (
	"fmt"
	"net/url"
	"strings"
)

// Where a notification sends somebody.
//
// # The routes here must match the frontend's
//
// The authoritative route list is Grainlify-Frontend/src/app/App.tsx - the
// <Route path=...> entries. Nothing enforces agreement across the two
// repositories, and pretending otherwise would be worse than saying so. What
// this file does is make the disagreement findable: once every path is built
// here, the thing that has to match App.tsx is ONE FILE rather than a repo, and
// "check these two files" is a question somebody can answer.
//
// If you add a route there, add a constructor here. If you remove one, the
// constructor is dead and its notifications point at nothing.
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

// Link is a path a notification points at.
//
// The field is unexported, so a Link can only be produced by a constructor in
// this file. That is the entire mechanism: Notify takes a Link rather than a
// string, so a hand-written path is not a thing a caller is able to express and
// the next one is a compile error rather than a blank page.
//
// This is the same rule as "a value crossing a typed boundary must be
// constructed, never spelled", applied to a path. Five call sites each writing
// their own literal is how two spellings of the same wrong route reached
// production and sat there for 43 notifications.
//
// Deliberately NOT enforced with a database CHECK constraint. A rejected write
// fails its surrounding transaction, so a cosmetic link bug would become a
// failed notification - and a KYC reset that cannot write its notification does
// not half-happen, it does not happen. The guard must not be able to cause a
// larger failure than the one it prevents. The backstop is a scan that alerts,
// in linkaudit.go.
type Link struct{ p string }

// String renders the path for storage. Empty for NoLink.
func (l Link) String() string { return l.p }

// NoLink is a notification with nowhere to go, stated rather than implied by an
// empty string.
var NoLink = Link{}

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
func SettingsLink(subtab SettingsSubtab) Link {
	return Link{fmt.Sprintf("%s?tab=settings&subtab=%s", DashboardPath, url.QueryEscape(string(subtab)))}
}

// ProjectLink builds a link to a project inside Browse.
func ProjectLink(projectID string) Link {
	return Link{fmt.Sprintf("%s?tab=browse&project=%s", DashboardPath, url.QueryEscape(projectID))}
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
func MaintainerApplicationLink(projectID string, githubIssueID int64) Link {
	return Link{fmt.Sprintf("%s?tab=maintainers&view=maintainer&project=%s&issue=%d",
		DashboardPath, url.QueryEscape(projectID), githubIssueID)}
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
func AbsoluteLink(baseURL string, l Link) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" || !strings.HasPrefix(base, "http") {
		return l.String()
	}
	return base + l.String()
}

// MyApplicationsLink points a contributor at their own applications.
//
// The contributions board is the only place somebody can see that they applied
// to something and what became of it. A notification about a contributor's own
// application belongs here rather than on the issue: the issue shows the work,
// this shows their standing on it.
func MyApplicationsLink() Link {
	return Link{DashboardPath + "?tab=contributors"}
}

// IssueLink builds a link to a specific issue inside a project.
func IssueLink(projectID string, githubIssueID int64) Link {
	return Link{fmt.Sprintf("%s?tab=browse&project=%s&issue=%d", DashboardPath, url.QueryEscape(projectID), githubIssueID)}
}
