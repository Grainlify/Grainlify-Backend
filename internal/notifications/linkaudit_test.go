package notifications

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

// The audit finds a bad route, names the writer, and counts it.
func TestAuditLinkPaths_FindsUnknownRoutes(t *testing.T) {
	ctx := context.Background()
	d := dbtest.DB(t)

	var uid uuid.UUID
	if err := d.Pool.QueryRow(ctx, `INSERT INTO users DEFAULT VALUES RETURNING id`).Scan(&uid); err != nil {
		t.Fatalf("user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Pool.Exec(context.Background(), `DELETE FROM notifications WHERE user_id=$1`, uid)
		_, _ = d.Pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, uid)
	})

	for _, n := range []struct{ typ, link string }{
		{"social_follow_completed", "/settings?subtab=rewards"},
		{"social_follow_completed", "/settings?subtab=rewards"},
		{"referral_completed", "/settings?tab=referrals"},
		{"pr_merged", "/dashboard?tab=browse&project=x"}, // healthy, must not appear
		{"kyc_reset", ""}, // no link, must not appear
	} {
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO notifications (user_id, type, title, body, link_path) VALUES ($1,$2,'t','b',NULLIF($3,''))`,
			uid, n.typ, n.link); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	found, err := AuditLinkPaths(ctx, d.Pool)
	if err != nil {
		t.Fatalf("AuditLinkPaths: %v", err)
	}

	byRoute := map[string]UnknownLink{}
	for _, f := range found {
		byRoute[f.Route+"|"+f.Type] = f
	}
	if got := byRoute["/settings|social_follow_completed"].Count; got != 2 {
		t.Errorf("social_follow_completed count = %d, want 2", got)
	}
	if _, ok := byRoute["/settings|referral_completed"]; !ok {
		t.Error("referral_completed pointing at /settings was not reported")
	}
	// The healthy row and the linkless row must not be reported. An audit that
	// flags correct data gets muted, and a muted audit is not a backstop.
	for k := range byRoute {
		if strings.HasPrefix(k, "/dashboard") {
			t.Errorf("a valid /dashboard route was reported: %s", k)
		}
	}
}

// Clean data reports nothing, and says so in words a person can act on.
func TestAuditLinkPaths_CleanIsEmpty(t *testing.T) {
	ctx := context.Background()
	d := dbtest.DB(t)

	var uid uuid.UUID
	if err := d.Pool.QueryRow(ctx, `INSERT INTO users DEFAULT VALUES RETURNING id`).Scan(&uid); err != nil {
		t.Fatalf("user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Pool.Exec(context.Background(), `DELETE FROM notifications WHERE user_id=$1`, uid)
		_, _ = d.Pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, uid)
	})
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO notifications (user_id, type, title, body, link_path) VALUES ($1,'pr_merged','t','b',$2)`,
		uid, SettingsLink(SubtabRewards).String()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	found, err := AuditLinkPaths(ctx, d.Pool)
	if err != nil {
		t.Fatalf("AuditLinkPaths: %v", err)
	}
	for _, f := range found {
		if f.Type == "pr_merged" {
			t.Errorf("a constructor-built link was reported as unknown: %+v", f)
		}
	}
	if msg := Describe(nil); !strings.Contains(msg, "every notification link points at a route the app serves") {
		t.Errorf("empty description = %q", msg)
	}
}

// Every constructor in links.go must produce a route the audit accepts. This is
// the check that catches a constructor and the route list drifting apart -
// which is the failure the cross-repo gap makes possible.
func TestEveryConstructorProducesAKnownRoute(t *testing.T) {
	links := []Link{
		SettingsLink(SubtabRewards),
		SettingsLink(SubtabBilling),
		SettingsLink(SubtabReferrals),
		ProjectLink("p"),
		MaintainerApplicationLink("p", 1),
		MyApplicationsLink(),
		IssueLink("p", 1),
	}
	known := map[string]bool{}
	for _, r := range KnownRoutes {
		known[r] = true
	}
	for _, l := range links {
		route := strings.SplitN(l.String(), "?", 2)[0]
		if !known[route] {
			t.Errorf("constructor produced %q, whose route %q is not in KnownRoutes", l, route)
		}
	}
}
