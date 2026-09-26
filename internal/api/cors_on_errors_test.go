package api_test

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
)

// A response that fails must carry the same CORS headers as one that
// succeeds. Otherwise the browser refuses to let the page read the response
// at all, and reports "blocked by CORS policy" over the top of whatever the
// real status was - so a backend fault presents as a CORS misconfiguration
// and gets diagnosed as one. That happened here: a 502 that was really a
// database error in a downstream service spent a round being read as a CORS
// problem.
//
// Fiber's cors middleware sets these headers before calling the handler, so
// they survive any status the handler later chooses. This test pins that,
// because it is the kind of property that holds by accident until somebody
// reorders app.Use and nothing else notices.
func TestAPICORSHeadersSurviveErrorStatuses(t *testing.T) {
	app := apiWiringSuiteApp(apiWiringSuiteConfig())
	const origin = "https://grainlify.com"

	cases := []struct {
		name       string
		method     string
		path       string
		wantStatus int
	}{
		// The catch-all.
		{"unmatched route", fiber.MethodGet, "/no-such-route", fiber.StatusNotFound},
		// An authenticated route refusing an anonymous caller. This is the
		// shape the bounty wallet card hits.
		{"unauthenticated", fiber.MethodGet, "/me/bounty-wallet/link", fiber.StatusUnauthorized},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("Origin", origin)
			resp, err := app.Test(req, -1)
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if got := resp.Header.Get("Access-Control-Allow-Origin"); got != origin {
				t.Fatalf("Access-Control-Allow-Origin on a %d = %q, want %q", tc.wantStatus, got, origin)
			}
			if got := resp.Header.Get("Access-Control-Allow-Credentials"); got != "true" {
				t.Fatalf("Access-Control-Allow-Credentials on a %d = %q, want \"true\"", tc.wantStatus, got)
			}
			// Without this a shared cache can serve one origin's allowed
			// response to another origin.
			if got := resp.Header.Get("Vary"); got == "" {
				t.Fatalf("Vary header missing on a %d; it must name Origin", tc.wantStatus)
			}
		})
	}
}
