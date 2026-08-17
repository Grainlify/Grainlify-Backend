package notifications

import (
	"fmt"
	"net/url"
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

// IssueLink builds a link to a specific issue inside a project.
func IssueLink(projectID string, githubIssueID int64) string {
	return fmt.Sprintf("%s?tab=browse&project=%s&issue=%d", DashboardPath, url.QueryEscape(projectID), githubIssueID)
}
