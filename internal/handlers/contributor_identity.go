package handlers

// ContributorLoginKey is the canonical SQL expression for identifying one
// contributor from an author_login column.
//
// GitHub logins are case-insensitive, and the same person is recorded with
// different capitalisation across issues and PRs. Counting or grouping on the
// raw column therefore splits one contributor into several.
//
// This has bitten three separate features. The leaderboard took a
// case-sensitive DISTINCT and then counted case-insensitively, which put one
// contributor in 134 consecutive ranks, each row reporting the combined
// total. The same shape in user_profile's rank query inflated the rank
// position - and therefore the rank tier - of everyone below a case-variant
// contributor. Six more sites across projects_public, ecosystems_public,
// org_ratings and search were counting distinct contributors the same way.
//
// Use this everywhere a contributor is counted, grouped, or joined on. The
// test in contributor_identity_test.go fails if a raw author_login DISTINCT
// reappears, because a convention nothing enforces is a convention that has
// already been broken somewhere you have not looked yet.
func ContributorLoginKey(column string) string {
	return "LOWER(" + column + ")"
}
