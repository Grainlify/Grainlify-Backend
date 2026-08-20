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
