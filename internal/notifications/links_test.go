package notifications_test

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

// Every notification link must resolve to a route the app actually serves.
//
// Notifications navigated to "/settings?subtab=rewards" and
// "/settings?tab=referrals". There is no /settings route: React Router logs
// `No routes matched location`, the page renders blank, and a refresh does not
// recover it - only the browser back button does. Two different spellings of
// the same wrong path were live, which is what happens when five call sites
// each write their own literal.
//
// declaredRoutes mirrors the frontend's <Route path=...> list. If a route is
// added there and not here, this guard will reject a legitimate new link -
// which is the correct direction to fail: it forces the two lists to be
// reconciled deliberately rather than drifting.
var declaredRoutes = map[string]bool{
	"/":              true,
	"/signin":        true,
	"/signup":        true,
	"/auth/callback": true,
	"/dashboard":     true,
}

func assertRoutable(t *testing.T, link, context string) {
	t.Helper()
	if link == "" {
		return // no link is valid; a wrong one is not
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Errorf("%s: %q is not a parsable URL: %v", context, link, err)
		return
	}
	if !declaredRoutes[u.Path] {
		t.Errorf("%s: %q points at path %q, which the app does not route.\n\n"+
			"React Router renders a blank page for it and a refresh does not recover - only browser "+
			"back does. Settings surfaces live at /dashboard?tab=settings&subtab=<name>; build links "+
			"with notifications.SettingsLink rather than writing the path by hand.",
			context, link, u.Path)
	}
}

// The builders themselves.
func TestNotificationLinkBuildersProduceRoutablePaths(t *testing.T) {
	for _, subtab := range []notifications.SettingsSubtab{
		notifications.SubtabProfile, notifications.SubtabNotifications,
		notifications.SubtabReferrals, notifications.SubtabRewards,
		notifications.SubtabPayout, notifications.SubtabBilling, notifications.SubtabTerms,
	} {
		link := notifications.SettingsLink(subtab)
		assertRoutable(t, link, "SettingsLink("+string(subtab)+")")

		// The subtab has to survive into the query, or the link lands on
		// Profile and silently shows the wrong screen.
		u, _ := url.Parse(link)
		if u.Query().Get("subtab") != string(subtab) {
			t.Errorf("SettingsLink(%s) lost its subtab: %q", subtab, link)
		}
		if u.Query().Get("tab") != "settings" {
			t.Errorf("SettingsLink(%s) does not select the settings tab: %q", subtab, link)
		}
	}

	assertRoutable(t, notifications.ProjectLink("proj-1"), "ProjectLink")
	assertRoutable(t, notifications.IssueLink("proj-1", 42), "IssueLink")
}

// stripLineComments removes // comments so documentation that quotes a bad
// path is not mistaken for code that uses one.
func stripLineComments(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// Every link literal anywhere in the backend, so a call site that bypasses the
// builders cannot reintroduce this. The bug was not one bad string - it was
// five call sites each writing their own.
func TestNoNotificationCallSiteWritesAnUnroutablePath(t *testing.T) {
	root := filepath.Join("..", "..", "internal")
	// Paths that look like app links: a leading slash, then a word.
	linkLiteral := regexp.MustCompile(`"(/[a-z][a-zA-Z0-9/_-]*(?:\?[^"]*)?)"`)

	var checked int
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// Comments are stripped first: this file documents the broken paths it
		// exists to prevent, and quoting them in prose must not fail the check
		// that forbids them in code.
		code := stripLineComments(string(src))
		for _, m := range linkLiteral.FindAllStringSubmatch(code, -1) {
			raw := m[1]
			// Only things that look like FRONT-END links. API routes, file
			// paths and GitHub endpoints share the shape and are not links.
			if !strings.HasPrefix(raw, "/dashboard") && !strings.HasPrefix(raw, "/settings") {
				continue
			}
			checked++
			u, err := url.Parse(raw)
			if err != nil || !declaredRoutes[u.Path] {
				t.Errorf("%s contains link literal %q, which is not a routable path. "+
					"Use notifications.SettingsLink / ProjectLink / IssueLink.", path, raw)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	// Without this, a broken regex or a moved directory makes every assertion
	// above vacuous while the test still reports success.
	if checked == 0 {
		t.Fatal("found no dashboard/settings link literals at all - the scanner is broken, not the links")
	}
}

// A maintainer's application notification must land where the actions are.
//
// It pointed at /dashboard?tab=browse - the CONTRIBUTOR view of the issue,
// which renders Withdraw rather than Reject/Assign/Unassign. A maintainer
// clicking their own notification arrived somewhere the work could not be
// done, and the surface where it could was reachable only through a view
// toggle that reset on every reload.
func TestMaintainerApplicationLinkLandsOnTheMaintainerSurface(t *testing.T) {
	link := notifications.MaintainerApplicationLink("proj-1", 42)
	assertRoutable(t, link, "MaintainerApplicationLink")

	u, err := url.Parse(link)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := u.Query()

	if q.Get("tab") != "maintainers" {
		t.Errorf("tab = %q, want maintainers - Browse renders the contributor view, "+
			"where the maintainer actions do not exist", q.Get("tab"))
	}
	// Carried explicitly so the link works on a cold load: the dashboard reads
	// the view from the URL, and without it a fresh tab defaults to contributor
	// and lands on the fallback instead of the queue.
	if q.Get("view") != "maintainer" {
		t.Errorf("view = %q, want maintainer - following this from an email or a new tab "+
			"would otherwise open in contributor mode", q.Get("view"))
	}
	if q.Get("issue") != "42" || q.Get("project") != "proj-1" {
		t.Errorf("the application is not in view: project=%q issue=%q", q.Get("project"), q.Get("issue"))
	}
}
