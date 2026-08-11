# Gap: nothing disburses a payout

Scope, not a build plan to execute yet. It blocks two things at once: GrainHack
contributor and maintainer payouts, and the Founding Contributor Pool
settlement, which by design routes through the same guarded path rather than a
second mechanism.

## The problem

Everything computes. Nothing pays.

Draws assign, PRs are judged, `ComputePayout` divides the pool, a payout run
row is written with every contributor's amount, `SettleMaintainerPool` scores
repos and splits the holdback, `ReleaseDueHoldbacks` resolves it 90 days
later, and `founding.Compute` divides the founding pool. At the end of all of
that, no code path moves value.

The chokepoint built for it has no caller:

```
$ grep -rn "GuardPayoutRelease(" --include="*.go" internal/ cmd/ | grep -v _test
internal/hackathon/judging_policy.go:59:   // GuardPayoutRelease is the single chokepoint...
```

Its own comment calls it "the single chokepoint every payout release must
pass". There is nothing on the other side of it.

**Correction to the first version of this document:** it claimed there was no
payout-run table and that one needed building. That was wrong.
`hackathon_payout_runs` exists, `CloseAppealsAndRecompute` writes to it, and
it already carries `unit_value`, `total_units`, floor state, `computed_by` and
a `published` flag. The gap is narrower than first stated, and the flag is the
clue: **`published` is never set to true by any code.** The schema anticipated
this step; nobody built it.

## What exists

| Piece | State |
|---|---|
| `hackathon_payout_runs` — pool, units, unit value, floor, `published` flag | Written on every appeals-close recompute; `published` never set |
| `hackathon_verdicts.payout_amount` / `payout_run_id` / `effective_units` | Written per contributor by the same recompute |
| `hackathon_maintainer_payouts` — gross, holdback, released, withheld, due date | Written at settle; resolved by the holdback job |
| `founding_settlements` / `founding_settlement_lines` | Written by `founding.Compute`; never released |
| `GuardPayoutRelease` — confirm flag, actor, run id, not-shadow, phase settled, appeals closed | Built, tested, **never called** |
| Anything that moves value | **Does not exist** |

## What is missing

### 1. The decision: what "paid" means for event one

Answer this before writing anything; everything else is shaped by it.

There is an existing precedent in this codebase, and it is the one to copy.
Point redemptions were **manually reviewed and manually sent**: an admin
listed pending requests, sent USDC out of band, and marked the row paid
(`ListAdmin` / `MarkPaid` / `Reject`). No automated transfer ever existed here
— which means "build a disbursement path" has never actually meant "build a
transfer engine" on this platform, and it should not start meaning that now.

Three options, in increasing cost:

- **Manual send, recorded release (recommended for event one).** An admin
  triggers a release, the guard runs, the run is marked published with actor
  and timestamp, and a per-contributor payout list is exported. The USDC send
  itself is a human operation. Smallest honest version; matches the redemption
  precedent; auditable.
- **Semi-automated via the existing Stellar path.** Reuse whatever sends
  redemption USDC today, driven from the payout list. Only worth it once the
  volume makes manual sending error-prone.
- **On-chain escrow and pull-based claims.** Already specified in the on-chain
  spec, and unreachable: nothing can create a chain pool
  (`GAP-grainhack-first-chain-event.md`). Not a candidate for event one.

### 2. Where `GuardPayoutRelease` gets called from

One admin endpoint, and only one, so the guard cannot be bypassed by a second
path added later:

```
POST /admin/hackathons/:id/payout-runs/:runId/release   { "confirm": true }
```

It must:

- call `GuardPayoutRelease` first and refuse on any error, unchanged
- set `published = true`, `published_at`, and the releasing actor **in the same
  transaction** as whatever it marks paid
- be idempotent on `published` — a double-fired release is the one failure here
  with no clean recovery, so re-releasing an already-published run must be a
  no-op rather than a second event
- write an audit row, like every other admin action

`GuardPayoutRelease` itself does not change. It already checks the five things
that matter and fails closed on all of them.

### 3. The founding settlement release

`founding_settlements.released_at` exists and is never set. The founding pool
settles through the same endpoint and the same guard — not a parallel one.
That was a deliberate constraint in the redesign and it should survive
implementation: two release paths means two places to forget a check.

Note the ordering constraint: the founding pool is divided at the **first
GrainHack's settlement**, so its release depends on that event reaching
`settled` with appeals closed — the same precondition the guard already
enforces.

### 4. Maintainer payouts and the holdback

`ReleaseDueHoldbacks` moves rows to `released` / `partially_released` /
`withheld` on a timer plus repo activity. That is accounting; nothing pays the
maintainer either.

Open question worth deciding rather than inheriting: **should the holdback
release route through `GuardPayoutRelease`?** It currently does not, and it
fires from a background job rather than an admin action. Arguments both ways —
the guard's phase/appeals checks are meaningless 90 days after settlement, but
"every payout passes one chokepoint" is worth more than the checks themselves.

### 5. Contributors cannot see an amount

No endpoint returns what anyone earned. For GrainHack that is a gap; for the
founding pool it is **deliberate and must stay that way** until a release path
exists — §6 forbids publishing a per-person figure, and a source-scanning
guard test currently fails the build if a founding settlement figure reaches a
handler or notification.

So this splits:

- GrainHack payout amounts: publishable after results, already stored per
  verdict, needs an endpoint.
- Founding shares: share counts are already exposed at `/founding/me`; the
  converted amount stays hidden until there is something real behind it.

## The smallest honest version

For one event, in order:

1. Decide (1) — manual send, recorded release.
2. Build the release endpoint (2): guard, mark published, audit, idempotent.
3. Export a payout list from the run — login, amount, and for the founding
   pool the wave multiplier that produced it, so a recipient can be answered.
4. Send manually, out of band.
5. Expose GrainHack payout amounts to contributors after release (5).

That is a few days of work and it is honest: it does not claim to transfer
anything it cannot, and every number it emits is one that was already computed
and can be reproduced from stored rows.

What it deliberately leaves out: automated transfers, on-chain escrow, and any
per-person founding figure. Each of those is a larger decision than the
release step itself, and none of them blocks event one.

## Until then

`judging_shadow_mode` defaults to `true` and blocks the transition to live, so
an event cannot quietly go live and publish results it has no way to honour.
That guard is a stopgap for exactly this gap and should stay until payouts
release.
