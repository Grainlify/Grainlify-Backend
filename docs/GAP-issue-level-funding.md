# The schema has no issue-level funding

*Established 17 August 2026, read-only against production.*

## The claim

Public framing — the site, the docs, and six grant applications — describes
maintainers **publishing funded issues** that contributors claim and get paid
for.

## What the schema actually models

**There is no issue-level funding, and no field that could carry it.**

`github_issues` columns, in full:

```
id, project_id, github_issue_id, number, state, title, body, author_login,
url, created_at_github, updated_at_github, closed_at_github, last_seen_at,
assignees, labels, comments_count, comments
```

No amount. No reward. No bounty. No funded flag. No escrow reference. Nothing
that joins an issue to money.

Every money-bearing column in the database belongs to one of two families:

| Family | Columns |
| --- | --- |
| Hackathon | `hackathons.contributor_prize_pool`, `hackathon_payout_runs.*`, `hackathon_verdicts.payout_amount`, `hackathon_chain_pools.funded_at`, `hackathon_maintainer_payouts.*` |
| Founding pool | `founding_settlements.pool_usdc`, `founding_settlement_lines.usdc_amount` |
| Points | `point_ledger.amount`, `redemptions.usdc_amount` |

All three are **event- or settlement-scoped**. A reward attaches to a hackathon
verdict on a submission, or to a share of a settled pool. Never to an issue.

## This is not "no event has run yet"

That distinction matters, and it is the reason this note exists.

Nothing is populated - 0 hackathons, 0 payout runs, 0 funded chain pools, 0
verdicts with an amount, 0 settlements, 0 redemptions - but the point is not
the empty tables. It is that **running an event would not create issue-level
funding either**. A verdict pays a submission judged against a hackathon. There
is no path by which an individual issue acquires a reward, because no table
expresses that relationship.

So "we have not funded issues yet" is the wrong reading. The correct reading is
that the product models something adjacent to what is described: **a
contest-and-settlement model, not a per-issue bounty model.**

## Consequences

**Outside the product.** Anywhere the framing says a maintainer funds an issue
and a contributor claims it, that describes a model the system does not have.
This affects the site, the docs and grant applications - the last most, because
a reviewer who asks "show me a funded issue" cannot be shown one, and the
honest answer is that funding does not attach at that level.

**Inside the product.** A contributor issues browse cannot filter or sort by
reward, now or after the first event, without new modelling. Designing a slot
for that filter would be building around a field that does not exist - the same
shape as the personalisation copy, which promised matching on interests that
nothing implemented.

## What closing the gap would take, if that is the intent

Either:

1. **Model it** - an issue-level funding table (issue, amount, funder, escrow
   reference, claim state), plus the flows that write it and the payout path to
   settle it. This is a product, not a column.
2. **Change the framing** to describe what exists: pooled rewards settled by
   event and by founding-pool share, with issues as the work rather than the
   unit of payment.

The second is a writing task and can happen immediately. The first is
substantial and interacts with the payout path, which currently has no callers
(see `docs/reference/payout-path`).

**Do not** add a nullable `reward_amount` to `github_issues` as a partial step.
An issue-level reward that nothing funds, nothing escrows and nothing settles
is a field that makes the claim look true in the schema while remaining false
in the product - which is the failure this note exists to prevent.
