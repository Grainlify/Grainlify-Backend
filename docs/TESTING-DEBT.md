# Testing debt: the `internal/handlers` suite runtime

## The problem

`internal/handlers` crossed Go's default 600s test timeout on 2026-08-10.
Its runtime across a few rounds of added tests:

| Run | Duration |
|---|---|
| 1 | 438s |
| 2 | 494s |
| 3 | 552s |
| 4 | 600s+ (timed out) |

That is a curve, not a plateau. `TEST_TIMEOUT ?= 30m` in the Makefile is a
stopgap that buys a few more rounds and then puts us back here.

## Diagnosis — measured, 2026-08-10

**It is not accumulation.** Two tests are 69% of the entire suite:

| Test | Duration |
|---|---|
| `TestLeaderboardSuite_ExcludesContributorsFromNonVerifiedProjects` | 255.6s |
| `TestLeaderboardSuite_RanksByContributionCountAndReportsExpectedFields` | 233.4s |
| all other 231 tests combined | 223.2s |

The other 231 tests average ~0.97s each. That is the per-test `testDB(t)`
cost, and it is real — but it is not what pushed the suite past 600s.

### The mechanism

Three things compound:

1. **`GET /leaderboard` is O(C × N) per request, and `LIMIT`/`OFFSET` prune
   nothing.** The query (`internal/handlers/leaderboard.go`) runs six
   correlated subqueries per contributor, each matching on
   `LOWER(author_login) = LOWER(...)`. The `author_login` indexes are plain
   `btree(author_login)` — a `LOWER()` predicate cannot use them, so each
   subquery is a sequential scan. Worse, `ORDER BY contribution_count DESC`
   sorts on a *computed* value, so every contributor must be fully evaluated
   before `OFFSET` can discard anyone.

   Measured against the test DB at 3,348 contributors — note it is flat,
   which is the tell that `OFFSET` is buying nothing:

   | Page | Wall time |
   |---|---|
   | `OFFSET 0` | 6.29s |
   | `OFFSET 100` | 6.58s |
   | `OFFSET 200` | 6.59s |
   | `OFFSET 300` | 6.75s |

2. **Both slow tests scan every page.** `leaderboardSuiteFindAcrossPages`
   walks 100-row pages up to `maxRows = 20000`. At 3,348 contributors that
   is 34 pages × ~6.5s ≈ 221s per full scan.

   `ExcludesContributorsFromNonVerifiedProjects` is the worst case *by
   construction*: the login it searches for is deliberately absent, so it can
   never short-circuit on a hit and always scans to exhaustion. The ranking
   test seeds contributors with 4 and 1 contributions — near the bottom of a
   3,348-row ranking — and searches twice, once per contributor.

3. **The contributor count grows every run, forever.** `grainlify_test` is
   never truncated, and each run seeds ~3 new `lbsuite-*` logins (125
   accumulated as of this writing). So C grows monotonically, and the cost is
   quadratic in C. That is why the curve accelerates rather than stepping.

## The real fix

Not the shared-fixture work originally assumed — that would have addressed
the ~223s baseline while leaving the 489s dominant cost untouched. In
priority order:

1. **Scope the leaderboard tests instead of scanning it.** These tests need
   to assert ranking and filtering behaviour, not to locate a needle in a
   global ranking. Either add a test-only filter to the endpoint, or assert
   against the query at the store layer with a scoped dataset. This alone
   removes ~489s.
2. **Bound the page walk.** If a global scan really is wanted, `maxRows =
   20000` is 200 requests against a query that cannot be paged cheaply. It
   should be small and explicit.
3. **Only then** consider migrating once per package via `TestMain` and
   sharing a read-only baseline. Worth ~200s, and only worth doing after (1).

One constraint any of this must respect: `grainlify_test` is deliberately
never truncated between runs (see the Makefile's `-p 1` comment). Shared
fixtures must not leave rows that outlive a run and skew a later one — that
exact failure mode has already been hit once, with a "dominance value"
fixture that permanently outranked a different test's data on every
subsequent run. Note that the accumulation described in (3) above is that
same property biting in a different way.

## Related: this is a production problem too, not only a test problem

The same query serves the public, uncached `GET /leaderboard`. Measured
against **production** on 2026-08-10:

- 531 qualifying contributors
- **3.47s** for a single page

Production is small today (10 projects, ~3.2k issue+PR rows), so this is a
scaling cliff rather than an outage — but it is a public endpoint taking
three and a half seconds, and its cost grows quadratically with contributor
count, which is the number the platform exists to increase.

Not fixed here — it is a change to a live query serving real users and wants
its own change and its own verification. Sketch of the fix when it is picked
up: store `author_login` case-folded (or add
`CREATE INDEX ... ON github_issues (LOWER(author_login))`), replace the six
correlated subqueries with a single grouped aggregate CTE, and cache the
ranking rather than recomputing it per request.

## How this was measured

    go test -v -count=1 -timeout 30m ./internal/handlers/ 2>&1 | tee out.log
    grep -E '^--- (PASS|FAIL|SKIP): ' out.log \
      | sed -E 's/^--- [A-Z]+: ([^ ]+) \(([0-9.]+)s\)/\2 \1/' \
      | sort -rn | head -25

If one or two tests dominate, fix those first; a broad shared-fixture
refactor may not be needed yet. That is exactly what happened here.
