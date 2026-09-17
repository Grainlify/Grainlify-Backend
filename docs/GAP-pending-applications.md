# 125 issues have applications and nothing resolves them

*Established 17 August 2026, read-only against production.*

## The state

| | |
| --- | --- |
| Applications with status `applied` | **159** |
| Across | **128 issues**, from **12 contributors** |
| Open issues carrying a pending application | **125** |
| Of those, unassigned on GitHub - so they look free | **117** |
| Most applicants on one issue | 4 |
| Applications that ever reached `assigned` | **5** |
| `hackathon_draws` | **0** |
| `hackathon_assignments` | **0** |

**The weighted draw has never executed in production.** The 5 assignments came
through `issue_applications.assigned_at` - a maintainer acting directly - not
through the mechanism the platform describes.

## Nobody has been waiting long. Yet.

The oldest pending application is **2 days old** (15 August). This is not a
backlog anybody has been ignoring for weeks, and no contributor needs an
apology today.

It is worth writing down precisely because that is currently true and will stop
being true on its own. 159 applications accumulated in three days with nothing
resolving them.

## What a contributor experiences

**Applying sends them nothing.** `TypeIssueApplicationSubmitted` is sent to the
project OWNER (`issue_applications.go:240`). The applicant receives no
confirmation. They are notified only if they are assigned
(`TypeIssueAssigned`) or rejected (`TypeIssueApplicationRejected`) - both of
which require somebody to act first.

**There is no state between "applied" and "assigned" that explains anything.**
The contributor's own view groups their items by raw status - `applied`,
`assigned`, `pending_review` - so an application shows as "applied" and stays
there indefinitely. Nothing says how many others applied, where they stand, or
what happens next, because nothing knows.

So from a contributor's side: they apply, hear nothing, and see a status that
never changes. The only signal the system produces goes to the maintainer.

## The shape this rhymes with

This is the KYC review queue again. A state that requires a human, nobody
notified, no age visible to anybody, and no alert when it grows - discovered
because somebody complained rather than because the system said so.

That one was fixed with an alert on transition plus a sweep for anything
sitting too long. The same two pieces would apply here, and the sweep matters
more, because the draw not running is silent by construction.

## One concentration worth knowing

`Stephan-Thomas` holds **101 of the 159** pending applications - 64% - all
filed on 15 August, all against `ValoraSec/valorasec`, a repository owned by
`solomon35-stack`. Not self-dealing, but one account applying to essentially
every issue in somebody else's repository in a single day.

That is the same account as the 7 self-owned merges and 8 fork-based merges
recorded in the merged_by investigation. Worth a look as behaviour, separate
from the mechanism gap.

## What this is not

Not a browse problem. The contributor issues browse now excludes issues with
pending applications from its default view and shows an applicant count on the
row, so it will not present a contested issue as free. That is a display fix
for a real gap underneath it.

The gap is that **applications accumulate and nothing resolves them.**
