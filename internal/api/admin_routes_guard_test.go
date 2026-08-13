package api

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Every route under /admin must enforce the live-role check.
//
// The conversion that introduced RequireLiveRole moved 41 routes by hand, and
// a hand-edit is one chance per route to miss one. More importantly, a route
// added next year by somebody who never saw that change would copy whatever
// its neighbours look like - and a single admin route registered without the
// check is a hole that no test elsewhere would notice, because every other
// route still passes.
//
// So this reads the route table as text, the same way the config Active-flag
// guard reads the handler corpus. It fails on any admin route that does not
// name the shared requireAdmin middleware.
//
// **There are no exemptions.** /bootstrap used to be one - it ran for a caller
// who was not yet an admin, by definition - and it has been removed entirely.
// The recovery path is a direct database UPDATE, not an endpoint.
//
// The map stays, empty, so that adding an exemption is a visible edit here
// rather than a silent omission. An empty map is also the stronger assertion:
// every admin route, without exception, names the shared check.
var adminRouteExemptions = map[string]string{}

func TestEveryAdminRouteEnforcesLiveRole(t *testing.T) {
	src, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatalf("read api.go: %v", err)
	}

	// adminGroup.Get("/path", ...middleware..., handler)
	routeRe := regexp.MustCompile(`adminGroup\.(Get|Post|Put|Patch|Delete)\("([^"]+)"([^\n]*)`)
	matches := routeRe.FindAllStringSubmatch(string(src), -1)
	if len(matches) == 0 {
		t.Fatal("found no adminGroup routes; this guard is not actually checking anything")
	}

	var checked, exempt int
	for _, m := range matches {
		method, path, rest := m[1], m[2], m[3]

		if reason, ok := adminRouteExemptions[path]; ok {
			exempt++
			// An exemption that quietly starts enforcing is fine; one that is
			// listed but no longer exists is stale and should be removed.
			t.Logf("exempt: %s %s (%s)", method, path, reason)
			continue
		}

		if !strings.Contains(rest, "requireAdmin") {
			t.Errorf("admin route %s %s does not enforce requireAdmin.\n"+
				"Every route under /admin must re-check the role against the database on each request; "+
				"authorising on the JWT claim alone means a revoked admin keeps access until their token expires. "+
				"If this route genuinely must run for a non-admin, add it to adminRouteExemptions with a reason.",
				method, path)
			continue
		}

		// Guard against the other direction too: RequireRole authorises on the
		// JWT claim, which is exactly what this change replaced.
		if strings.Contains(rest, `RequireRole("admin")`) {
			t.Errorf("admin route %s %s still uses the claim-based RequireRole", method, path)
		}
		checked++
	}

	t.Logf("%d admin routes enforce requireAdmin, %d exempt", checked, exempt)
	if checked < 40 {
		t.Errorf("only %d admin routes were verified; the route regex has probably stopped matching", checked)
	}
}

// TestNoClaimBasedAdminCheckRemains catches the pattern anywhere in the route
// table, including on routes registered outside adminGroup.
func TestNoClaimBasedAdminCheckRemains(t *testing.T) {
	src, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatalf("read api.go: %v", err)
	}
	if strings.Contains(string(src), `auth.RequireRole("admin")`) {
		t.Error(`auth.RequireRole("admin") is back in the route table. ` +
			`It authorises on the JWT's role claim, so a demoted admin keeps access until their token expires. ` +
			`Use requireAdmin (auth.RequireLiveRole), which re-reads the role from the database.`)
	}
}

// TestAdminRoutesAreAllUnderTheGroup: a route mounted on app directly with an
// /admin path would skip the group's auth entirely, and the guard above would
// never see it.
func TestAdminRoutesAreAllUnderTheGroup(t *testing.T) {
	src, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatalf("read api.go: %v", err)
	}
	stray := regexp.MustCompile(`app\.(Get|Post|Put|Patch|Delete)\("/admin`)
	if m := stray.FindAllString(string(src), -1); len(m) > 0 {
		t.Errorf("admin-path routes registered outside adminGroup: %v", m)
	}
}

// TestBootstrapRouteIsGone is a regression guard for a removal.
//
// /admin/bootstrap granted the first admin to whoever presented a shared
// environment token. It already refused once any admin existed, so it was shut
// in practice - but "shut" depended on the admin count staying above zero,
// which meant a variable could reopen a permanent grant path. The route was
// removed so that no configuration can bring it back.
//
// This asserts against the route table itself rather than against behaviour,
// because the failure being guarded against is somebody re-adding the
// endpoint - at which point a behavioural test would simply start testing the
// thing that should not exist.
func TestBootstrapRouteIsGone(t *testing.T) {
	src, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatalf("read api.go: %v", err)
	}
	if strings.Contains(string(src), "/bootstrap") {
		t.Error("api.go registers a /bootstrap route again. First-admin recovery is a database UPDATE " +
			"(see internal/handlers/break_glass_test.go), deliberately not an endpoint that an environment " +
			"variable can arm.")
	}
	if strings.Contains(string(src), "BootstrapAdmin") {
		t.Error("api.go references BootstrapAdmin again; the handler was removed with the route")
	}
}
