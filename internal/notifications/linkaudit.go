package notifications

import (
	"context"
	"fmt"
	"strings"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// The backstop, and why it is a scan rather than a constraint.
//
// The Link type stops a hand-written path being written *from Go*. It cannot
// stop a migration, a manual INSERT, or a future service writing directly to
// the table - and 43 rows already reached production pointing at a route that
// does not exist.
//
// A CHECK constraint would catch those, and is the wrong tool. A rejected write
// fails its surrounding transaction, so a cosmetic link bug becomes a failed
// notification - and a KYC reset that cannot write its notification does not
// half-happen, it does not happen. **A guard must not be able to cause a larger
// failure than the one it prevents.** Same reasoning as the anonymous support
// cap failing open: a broken count must not become an outage on the route a
// locked-out person needs.
//
// So this reads and reports. It has no power to refuse anything, which is
// exactly the property that makes it safe to run against live data - the same
// split as the reconciler, which alerts on a disagreement and never resolves
// one itself.

// KnownRoutes are the paths the frontend actually serves.
//
// Must match the <Route path=...> entries in
// Grainlify-Frontend/src/app/App.tsx. Nothing enforces that across the two
// repositories; this list and links.go's constructors are the two places that
// have to agree with it, and naming the file is the difference between somebody
// finding the drift and not.
var KnownRoutes = []string{
	"/",
	"/signin",
	"/signup",
	"/auth/callback",
	"/support",
	"/dashboard",
}

// UnknownLink is one notification pointing somewhere the app does not serve.
type UnknownLink struct {
	Route string
	Type  string
	Count int
}

// AuditLinkPaths reports every stored link_path whose route is not one the
// frontend serves. An empty result is the healthy state.
//
// Grouped by route and type rather than returning rows: the useful output is
// "social_follow_completed is writing /settings, 30 times", not thirty
// individual ids. A list of ids invites fixing those thirty and leaving the
// writer alone, which is how the same rows come back.
func AuditLinkPaths(ctx context.Context, pool db.DBPool) ([]UnknownLink, error) {
	rows, err := pool.Query(ctx, `
SELECT split_part(link_path, '?', 1) AS route, type, count(*)::int
FROM notifications
WHERE link_path IS NOT NULL AND link_path <> ''
  AND split_part(link_path, '?', 1) <> ALL($1::text[])
GROUP BY 1, 2
ORDER BY 3 DESC, 1, 2
`, KnownRoutes)
	if err != nil {
		return nil, fmt.Errorf("notifications.AuditLinkPaths: %w", err)
	}
	defer rows.Close()

	var out []UnknownLink
	for rows.Next() {
		var u UnknownLink
		if err := rows.Scan(&u.Route, &u.Type, &u.Count); err != nil {
			return nil, fmt.Errorf("notifications.AuditLinkPaths: scan: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("notifications.AuditLinkPaths: iterate: %w", err)
	}
	return out, nil
}

// Describe renders findings for a human reading an alert.
func Describe(findings []UnknownLink) string {
	if len(findings) == 0 {
		return "every notification link points at a route the app serves"
	}
	var b strings.Builder
	total := 0
	for _, f := range findings {
		total += f.Count
	}
	fmt.Fprintf(&b, "%d notification(s) point at a route the app does not serve:\n", total)
	for _, f := range findings {
		fmt.Fprintf(&b, "  %s  (%s)  x%d\n", f.Route, f.Type, f.Count)
	}
	b.WriteString("These render blank and only the back button recovers. " +
		"Build the path with a constructor in links.go rather than repairing the rows alone.")
	return b.String()
}
