package api

import (
	"os"
	"strings"
	"testing"
)

// Which header the app trusts for the caller's address.
//
// Established by probing our own edge from outside it, on both reachable
// paths, with deliberately spoofed values. Spoofed values never arrived:
// Railway's edge strips an inbound X-Forwarded-For and overwrites X-Real-IP,
// and it is Railway doing it rather than Cloudflare, because the direct path
// behaves identically.
//
// Asserted against the source rather than by making a request, because what
// is being pinned is a configuration choice whose wrongness is invisible in
// any single-caller test: keying on the wrong header returns a plausible
// address every time and only fails as an aggregate, when strangers are
// throttled together and no individual is isolated.
func TestFiberConfig_TrustsXRealIPAndNotForwardedFor(t *testing.T) {
	src, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatalf("read api.go: %v", err)
	}
	s := string(src)

	if !strings.Contains(s, `ProxyHeader: "X-Real-IP"`) {
		t.Error(`ProxyHeader is not "X-Real-IP" - without it c.IP() returns Railway's 100.64.0.x CGNAT ` +
			`proxy address, which rotates per request, and every rate limit keyed on it isolates nobody`)
	}

	// The specific wrong answer, and the one somebody will reach for: the
	// leftmost X-Forwarded-For entry is a Cloudflare edge address on the path
	// real users take, shared by an enormous number of people.
	if strings.Contains(s, `ProxyHeader: "X-Forwarded-For"`) {
		t.Error(`ProxyHeader is X-Forwarded-For: on the Cloudflare-fronted path its leftmost entry is a ` +
			`shared, rotating Cloudflare edge IP, not the caller`)
	}
}

// The old production hostname must not be what CI smoke-tests, because it is
// the one that bypasses Cloudflare - testing through it would verify a path
// real users do not take and miss anything the WAF sits in front of.
func TestCI_SmokeTestsTheCloudflareFrontedHost(t *testing.T) {
	src, err := os.ReadFile("../../.github/workflows/ci.yml")
	if err != nil {
		t.Skipf("ci.yml not readable from here: %v", err)
	}
	if strings.Contains(string(src), "0xo.in") {
		t.Error("CI still smoke-tests api.grainlify.0xo.in, the hostname that bypasses Cloudflare")
	}
}
