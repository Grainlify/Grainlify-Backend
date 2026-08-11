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

## Resolved 2026-08-10

Fixed in its own change (migration 000049 + `leaderboard.go`), because the
same query served the public, uncached `GET /leaderboard`.

Live endpoint, measured against production before and after. `/health` does
no database work and costs ~0.35s from the measuring machine, so subtracting
it separates server time from network round trip:

| | total | server-side |
|---|---|---|
| before | 1.93s | ~1.58s |
| after | 0.40s | ~0.05s |

Roughly 30x less server-side time, and now flat across offsets where it
previously got *worse* with depth (2.19s at offset 400).

`internal/handlers` went from 712s to 207s as a side effect, since the two
leaderboard tests were 69% of it. Those tests were also rewritten: bounded
search, "conclusively absent" distinguished from "gave up looking", and
fixtures that delete themselves so the ranking they search stops growing
every run.

Two correctness bugs were found and fixed along the way — a case-sensitive
`DISTINCT` combined with case-insensitive counting, and a `github_accounts`
join that fanned out because `login` has no unique constraint. One test-data
contributor was occupying 134 consecutive ranks. Both have regression tests.

### What is left

Only the per-test setup baseline: ~231 tests at ~0.97s each. Worth revisiting
if the package creeps back up, via `TestMain`-level migration and a shared
read-only baseline dataset — subject to the never-truncated constraint above.
Not urgent at 207s.

## How this was measured

    go test -v -count=1 -timeout 30m ./internal/handlers/ 2>&1 | tee out.log
    grep -E '^--- (PASS|FAIL|SKIP): ' out.log \
      | sed -E 's/^--- [A-Z]+: ([^ ]+) \(([0-9.]+)s\)/\2 \1/' \
      | sort -rn | head -25

If one or two tests dominate, fix those first; a broad shared-fixture
refactor may not be needed yet. That is exactly what happened here.

---

# Known non-reproducible input: median time to first review

`MedianTimeToFirstReviewHours` (`internal/github/signals.go`) is one of §7's
four maintainer-pool criteria. It is computed by sampling the GitHub API at
scoring time — there is no `github_pr_reviews` table and nothing about reviews
is synced.

**This means the number is not reproducible.** Re-score the same maintainer six
weeks later during an appeal and you may get a different answer, because the
sample window has moved and the underlying PRs have accumulated more reviews.
Every other input to a maintainer's score is reproducible from stored rows;
this one is not.

That matters specifically because §6 appeals are answerable only if the record
can be reconstructed — the whole reason verdicts store the model version,
prompt version, raw request and raw response. A maintainer appealing their
pool score on this criterion is appealing a number nobody can recompute.

Accepted deliberately for now: building a review-sync table is real work, the
criterion is one of four, and no real event has run. Worth knowing before
someone appeals on it.

The fix when it is picked up: sync PR reviews into a table on the existing
syncjobs path (same shape as `github_pull_requests`), and compute the median
from stored rows so it is a function of recorded history rather than of when
you asked. Failing that, snapshot the computed value onto the maintainer's
score row at settle time, so at least the number that was used is preserved
even if it cannot be re-derived.

---

# Future task: pin the database session timezone to UTC

The contribution-calendar timezone bug was fixed at each query site
(`AT TIME ZONE 'UTC'` on every timestamptz-to-day/week bucket in
`user_profile.go` and `org_ratings.go`, verified across UTC, IST, UTC-8 and
UTC+14).

The broader fix was deliberately **not** folded in: setting the connection
pool's session timezone to UTC would eliminate this entire bug class in one
line, because every `timestamptz`-to-`date` cast in the codebase currently
resolves in whatever timezone the session happens to have.

It was left out because it silently changes every other such cast — not just
the two calendars — and that is a larger change than a calendar fix should
carry. It needs its own change and its own testing.

What it would involve:

- Set `TimeZone=UTC` in the pgx pool's runtime params (or `SET TIME ZONE 'UTC'`
  on connection acquire), so dev machines match production instead of
  differing by their operator's location.
- Audit every remaining bare `::date`, `DATE(...)`, `date_trunc(...)` and
  `now()` comparison against a `timestamptz` column — the explicit
  `AT TIME ZONE 'UTC'` sites added for the calendars are already safe and
  would become no-ops, which is the intended belt-and-braces.
- Run the suite under at least `Asia/Kolkata` and `Pacific/Kiritimati`, since
  those are the offsets that expose a day-boundary disagreement.

Worth doing before an event runs across timezones, because "which day did this
contribution land on" becomes a payout-adjacent question once windows open and
close on dates.

---

# Testing debt: the points-programme freeze seam

## What it is

`internal/handlers/points_freeze.go` holds `pointsProgrammeFrozen`, a package
var rather than a const purely so tests can flip it. The seam exists because
freezing the fixed-rate points programme would otherwise have deleted live
coverage of behaviour that still has to work if the freeze is ever lifted:
redemption validation, the referral and social-follow award arithmetic, and
the idempotency of both completion paths.

Nine tests currently call `UnfreezePointsProgrammeForTest`.

## Why it is debt and not a design

The seam keeps code alive that nothing in production can reach. Every one of
those nine tests exercises a path that is, by decision, dead: the programme is
retired in favour of the Founding Contributor Pool, and the plan is not to
lift the freeze. Coverage of unreachable code reads as coverage, which is
worse than no coverage — it makes the suite look like it is testing more than
it is, and it makes the freeze look provisional when it is not.

## The deadline

**If the freeze has not been lifted by 2026-02-11 — six months from the
freeze — delete the seam and everything behind it:**

- `pointsProgrammeFrozen`, `guardPointsAccrual`, `pointsGrantAmount` and
  `pointsRedemptionsFrozen`
- `points_freeze_export_test.go` in its entirety
- the nine tests that call `UnfreezePointsProgrammeForTest`, and the award
  branches they cover
- most likely the points programme itself: `point_ledger`, `redemptions`,
  the `/points/me` and `/redemptions*` endpoints, and the award paths in
  `referrals.go` and `social_follow.go`

Deleting is the cheap outcome here, which is why it gets a date. Production
held zero points, zero referrals and zero redemptions when the freeze went in,
so there is no data to migrate and nobody to notify — and that will not become
more true by waiting. A flag nobody exercises, guarding a system nobody can
reach, is exactly the thing that rots into "we were not sure why this was
here" a year from now.

What must be kept if the points tables are dropped: the **referral graph**
and the **social-follow completions**. Those are not points-programme state —
the Founding Contributor Pool reads both, as its referral shares and its
eligibility gate.

---

# Not debt: the founding wave advisory lock is a correctness dependency

`founding.AssignWave` allocates sequence numbers under
`pg_advisory_xact_lock`, then reads `max(sequence_number) + 1`. That looks
like a hand-rolled sequence and it is tempting to "simplify" it into a
Postgres `SEQUENCE` or an `IDENTITY` column. **Do not.**

A Postgres sequence does not roll back. Two verifications landing together
where one fails leaves members 1, 2, 4 — sequence 3 is burned and can never be
issued. That is not a cosmetic gap:

- **The wave boundary is a count.** Founding is "the first 100", so a burned
  number silently shrinks the tier to 99 seats. The scarcity was announced;
  the seat quietly does not exist.
- **Nobody can hold it.** The Founding tier's entire value is that it is
  countable and closed. A member who never existed at position 3 is not
  recoverable later, because membership is permanent and assigned in order.
- **It is invisible until someone counts.** Nothing errors. The programme
  runs, the badge is issued, and the discrepancy surfaces only if somebody
  reconciles the member count against the announced slot count — most likely
  in public, in a complaint.

`TestAssignWave_ConcurrentAssignmentsStayGapless` runs twelve concurrent
assignments and asserts sequences 1..12 all exist exactly once. If that test
is ever failing after a change here, the change is wrong — do not relax the
assertion.

The lock is per-programme rather than per-user deliberately: the invariant is
global ordering, which cannot be enforced by a lock scoped to one member.
