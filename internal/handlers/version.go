package handlers

import (
	"os"
	"strings"

	"github.com/gofiber/fiber/v2"
)

// What commit is actually running.
//
// This exists because "I checked production" was, until now, somebody loading
// a page and believing it. A deployed service that cannot say which commit it
// is makes every claim about it unfalsifiable: work that was never committed,
// never merged, or merged but not yet deployed all look identical from the
// outside, and all three have happened here.
//
// With this, "verified" becomes mechanical: fetch the sha, compare it to the
// commit containing the change. If they differ, the change is not live,
// whatever the tests said.
//
// The value comes from the platform at build time. Railway injects
// RAILWAY_GIT_COMMIT_SHA; the others are accepted so this keeps working if the
// service moves, rather than silently reporting "unknown" from a host nobody
// remembered to configure.
var commitSHAEnvVars = []string{
	"RAILWAY_GIT_COMMIT_SHA",
	"VERCEL_GIT_COMMIT_SHA",
	"GIT_COMMIT_SHA",
	"SOURCE_COMMIT",
	"COMMIT_SHA",
}

// BuildCommitSHA returns the deployed commit, or "" when the platform did not
// supply one. Empty is reported honestly rather than as a plausible default:
// a fabricated sha would make the check pass while proving nothing, which is
// the failure this endpoint exists to remove.
func BuildCommitSHA() string {
	for _, k := range commitSHAEnvVars {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// Version reports the running build. Unauthenticated on purpose - a commit sha
// is not a secret, the repositories are public, and a check that needs
// credentials is one that gets skipped.
func Version() fiber.Handler {
	return func(c *fiber.Ctx) error {
		sha := BuildCommitSHA()
		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"service": "patchwork-api",
			"commit":  sha,
			// Explicit rather than inferred from an empty string, so a caller
			// cannot mistake "not configured" for "matches nothing".
			"commit_known": sha != "",
		})
	}
}
