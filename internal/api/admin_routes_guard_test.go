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
// The one deliberate exemption is /bootstrap, which by definition runs for a
// caller who is not yet an admin. It is listed explicitly so adding a second
// exemption is a visible edit to this list rather than a silent omission.
var adminRouteExemptions = map[string]string{
	"/bootstrap": "grants the first admin on a fresh install; the caller is not an admin yet by definition",
}

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
