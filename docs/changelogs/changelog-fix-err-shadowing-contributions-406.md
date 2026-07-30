## Changelog - Fix err shadowing in contribution handlers (Issue #406)

### Description
`UserProfileHandler.ContributionCalendar()` and `UserProfileHandler.ContributionActivity()` silently discarded the error from their `github_accounts` lookup. In both the `user_id` query-param branch and the own-profile (JWT `sub`) branch, `parsedUserID, err := uuid.Parse(...)` declared a new block-scoped `err` that shadowed the outer one; the following `err = h.db.Pool.QueryRow(...).Scan(&githubLogin)` assigned to that inner variable, which was never read again.

As a result any failure of that lookup — a genuine connection or query error, not just "no linked account" — fell through to `if githubLogin == nil`, and both endpoints answered `200 OK` with an empty calendar / empty activity list. A user whose contribution history vanished after a transient database blip could not distinguish that from having no GitHub account linked, and operators got no signal at all, because the error was discarded before any logging.

### Changes
- `internal/handlers/user_profile.go`
  - Checked the `github_accounts` lookup error in all four affected branches (two handlers x two branches), using the inline `if err := ...; err != nil && !errors.Is(err, pgx.ErrNoRows)` form so no assigned-but-unread `err` remains.
  - `pgx.ErrNoRows` still degrades to an empty calendar/activity response, unchanged from previous behavior. Any other error now logs via `slog.Error` and returns `500` with the error code `github_account_lookup_failed`, distinct from the existing `calendar_fetch_failed` / `activity_fetch_failed`.
  - Removed the now-orphaned `var err error` declaration in `ContributionCalendar()`; its only consumer (`rows, err := h.db.Pool.Query(...)`) declares it via `:=`. `ContributionActivity()` keeps its outer `err`, which comes from `ParsePagination` and is still used.
  - Added imports `errors` and `github.com/jackc/pgx/v5` (both already direct module dependencies; `go.mod` unchanged).
- `internal/handlers/user_profile_internal_test.go`
  - Added `githubLookupPool`, a `db.DBPool` fake whose `QueryRow` scan fails with a configurable error, plus `scanErrRow`.
  - `TestContributionHandlers_LookupDBErrorReturns500` covers all four branches and asserts both the `500` status and the `github_account_lookup_failed` error code, which pins the failure to the lookup rather than to a later query.
  - `TestContributionHandlers_LookupNoRowsStaysEmpty200` covers the same four branches and pins the unchanged `pgx.ErrNoRows` behavior (empty `200`, `total: 0`).

### Verification Steps
- `go test ./internal/handlers/ -run TestContributionHandlers -v` — 8/8 subtests pass.
- Regression proven by reverting `internal/handlers/user_profile.go` to its pre-fix state and re-running: the 4 error subtests fail with exactly the reported bug, e.g. `status = 200, want 500 (body: {"calendar":[],"total":0})` and `(body: {"activities":[],"limit":50,"offset":0,"total":0})`. The 4 `pgx.ErrNoRows` subtests keep passing pre-fix, confirming that path is untouched.
- Confirmed empirically that Fiber does not recover handler panics into a `500` (`app.Test` returns `runtime.Goexit() called in handler or server panic` with a nil response and the test binary aborts), so the fake returns errors instead of panicking and the tests cannot pass for the wrong reason.
- `go vet ./internal/handlers/` clean; `go build ./...` and `go test ./internal/handlers/` pass.
- Coverage: `ContributionCalendar` 36.4%, `ContributionActivity` 40.0% (package `internal/handlers`: 32.9%). These functions had no test calling them at all before this change. The remaining statements are the contribution SQL queries and row-scanning loops, which need a live Postgres (`TEST_DB_URL`) and are out of scope here.

### Notes
`cmd/worker` (`TestGracefulShutdown_ContextCancelStopsWait`) and `internal/auth` (`TestVerifySignature_EdgeCases_EVM`) fail on this branch, but they fail identically on the parent commit `d58607e` without any of these changes. Both are pre-existing and unrelated.
