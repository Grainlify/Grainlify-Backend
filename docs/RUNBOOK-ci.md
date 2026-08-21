# Runbook: CI

What `.github/workflows/ci.yml` checks, when it runs, and the one behaviour that
has already cost this repository fifty-two unchecked merges.

## What runs

One workflow, two jobs.

**`test`** — build, vet, a migration-collision check, then `go test -count=1 -p 1 -cover ./...`
against a `postgres:16` service container. The `-p 1` is load-bearing: several
packages share the one database and mutate global state, so Go's default
concurrent-per-package execution races without it.

**`deploy`** — `railway up` plus a production smoke test. Guarded on
`github.event_name == 'push' || 'workflow_dispatch'`, so **it cannot fire from a
pull request.**

## When it runs, and why the two triggers differ

```yaml
on:
  push:
    branches: [restore/initial-commit-with-auth-fixes]
  pull_request:
    branches: [restore/initial-commit-with-auth-fixes, main]
```

`push` is scoped to the working branch because the deploy job runs on push and
production is deployed by hand from that branch. `pull_request` covers `main` as
well, because `main` is the default branch and nearly every PR targets it.

The two lists differ **deliberately**. Adding `main` to `push` would deploy on
every merge to `main`, which is a separate decision from testing. Do not
"tidy" them into agreement.

## The trap: a workflow fix does not re-check anything already open

**`pull_request` triggers are evaluated at event time.** Merging a fix to
`ci.yml` into a base branch emits no `pull_request` event for PRs that are
already open, so they stay unchecked — and they do not show as failing or
pending. They show *nothing*, which reads exactly like a repository that does
not run CI.

Observed on 2026-08-20, in both directions in the same hour:

| PR | Cut from | Result |
|---|---|---|
| The PR that *added* `main` to the trigger | branch containing the fix | **Ran.** Its merge ref contained the fixed file |
| A PR opened from `main` before the fix landed | `main` without the fix | **Did not run,** even after the fix merged to `main` |
| A branch cut from `main` after the fix landed | `main` with the fix | **Ran automatically** |

The first row is why a trigger fix can be validated on its own PR: for a
`pull_request` event GitHub evaluates the workflow from the **merge ref** —
head merged into base — so a PR that adds its own trigger fires.

### What to do

**Any PR opened before the trigger fix needs a push to be checked.** A rebase
onto the current base, or an empty commit, produces the `synchronize` event that
makes the workflow evaluate again. Branches cut *after* the fix need nothing.

```sh
git fetch origin && git rebase origin/main && git push --force-with-lease
```

### How to tell whether a PR was checked at all

```sh
gh pr view <n> --json statusCheckRollup --jq '[.statusCheckRollup[]?|{name,conclusion}]'
```

An **empty array** means no workflow matched — not "checks are pending". That is
the state to look for, and it is the one that looks like nothing is wrong.
`gh run list --json event,headBranch` shows whether any run exists for the branch
at all.

## Why this is written down

Between 2026-07-31 — the last `pull_request` run this repository recorded — and
2026-08-20, **52 pull requests merged into `main` with no build, no vet, no test
and no migration-collision check.** Nobody noticed, because an unchecked PR and a
repository without CI look identical from the PR page, and both look identical to
a green one once merged.

The general form is the same one `VERIFICATION-TRAPS.md` catalogues under a
different heading: **the absence of a signal is not the signal that something
passed.** A run that never started reports nothing, and nothing is what a clean
result also looks like from a distance.

## Related

- Local suite behaviour, the `-p 1` requirement, and known flakes: `docs/TESTING-DEBT.md`
- Why a green suite says nothing about the Move contract: `internal/chain/aptos_fixture_drift_test.go`
- Counting what ran rather than reading the totals: `docs/VERIFICATION-TRAPS.md`

---

# Running the suite locally: one database per BRANCH

`#535` fails a PR whose migrations sit at or below main's highest. It runs at PR
time and does nothing for you at your desk. This section is the local half, and
it is workflow rather than a lesson: **create the database when you create the
branch, not after the collision.**

## Why a shared database breaks the moment two branches exist

`migrate.Up` records one version per database. A branch carrying migration N+1
migrates the shared database to N+1; switch to a branch that does not have that
file and the next run fails:

```
migrate.Up: no migration found for version 89: read down for version 89 .: file does not exist
```

Nothing is wrong with either branch. The database is simply ahead of one of them,
and it stays ahead until somebody drops it.

This is not rare and it is not a sign of a mistake. It happens the moment two
branches with migrations exist, which on any active day is most of them — three
separate times in one day here, at versions 88, 89 and 90.

## The workflow

One container, many databases. Creating a database is instant; migrating it is
the slow part and it happens once per branch either way.

```sh
# when you create the branch, in the same breath
git checkout -b my-branch origin/main
createdb -h localhost -p 5435 -U postgres grainlify_test_my_branch

# and every run on that branch
TEST_DB_URL='postgres://postgres:test@localhost:5435/grainlify_test_my_branch?sslmode=disable' \
  go test -count=1 -p 1 ./...
```

`-p 1` is not optional — several packages share the one database and mutate
global state, so Go's default concurrent-per-package execution races without it.
CI passes it and says so at length.

## Do not hand-edit `schema_migrations`

Rolling back a local migration means **dropping the database**. Running a
`.down.sql` by hand and deleting the row leaves the recorded version pointing at
a state the files disagree with, and the next `migrate.Up` fails at some
unrelated earlier version — which reads as corruption rather than as the edit it
was. Drop it and let the chain re-run; it is a local test database and holds
nothing anyone needs.

## Why not just drop and recreate the shared one each time

Because the failure is silent in the other direction. A database migrated by
another branch does not announce itself, and the symptom — a test failing on a
duplicate key, or a column that does not exist — points at the test rather than
at the database. Separate databases remove the class instead of teaching people
to recognise it.

## The related trap

A migration numbered at or below what main already has **never runs**, and
`migrate.Up` reports success. See #535 for the CI check and
`docs/VERIFICATION-TRAPS.md` for why a silent skip is the worst shape a failure
can take.

---

# Migration numbering: timestamps, not the next integer

```sh
scripts/new-migration.sh add_contributor_public_keys
# migrations/20260821143702_add_contributor_public_keys.up.sql
# migrations/20260821143702_add_contributor_public_keys.down.sql
```

**Digits only.** golang-migrate parses the version with `^([0-9]+)_` and
`ParseUint(m[1], 10, 64)`, so `20260821T143702_name` is not a malformed
migration — it is an **invisible** one. The driver does not see the file at all,
and nothing reports a problem.

## Why not the next integer

Two branches created a minute apart pick the same next integer, and that
collision is detectable in only some of its windows:

| Window | Caught by |
|---|---|
| Both branches pushed, neither merged | the across-branches check |
| One already merged to main | the above-main check (#535) |
| **Two unpushed branches** | **nothing** |

Three windows, two checks, and the gap is the ordinary case: two people working
locally before either pushes. A timestamp cannot be produced twice, so the class
does not arise and no window needs covering.

It also removes the *ordering* requirement that a version bump cannot fix.
Sequential migrations on parallel branches must merge in ascending order, because
each is only above main's highest once the ones before it have landed. That
constraint forced two renumbers in one day here. Timestamps merge in any order.

## Existing migrations stay as they are

`000001` through `000091` are untouched. A timestamp sorts far above them, so the
chain is unbroken and nothing needs rewriting — which is the whole reason this is
cheap to adopt.

## It is self-enforcing once one has landed

main's highest becomes ~2.0e13, so **any future sequential number is below it**
and #535 refuses it by name. Reverting to integers is not possible without
somebody noticing, which is a stronger property than a convention documented
here.

## Deployment constraint: 64-bit only

`getLatestMigrationVersion` returns a `uint`. On the 64-bit platforms we deploy
to that holds values up to ~1.8e19 and a 14-digit timestamp is ~2.0e13, so there
is enormous headroom.

**On a 32-bit build, `uint` is 32 bits and caps at 4,294,967,295 — a timestamp
overflows it.** Nothing today is 32-bit and nothing is likely to be, but this is
a property of the *platform* rather than of the code, so it belongs where
somebody changing platforms will look rather than in a comment beside a
function.

## The check that enforces this caught the person who wrote it

Within a day of merging, #535 refused a migration numbered below main's highest —
added by the author of the check, who had merged another PR out of the order he
had written down two messages earlier.

That is the only real test of a guard. A check that has only ever caught other
people is a check whose author still believes they do not need it.

