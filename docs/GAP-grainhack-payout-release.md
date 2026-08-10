# Gap: nothing disburses a GrainHack payout

## The problem

GrainHack computes what everyone is owed and never pays it. The whole
pipeline runs — draws assign, PRs are judged, buckets are set, `ComputePayout`
divides the pool, `SettleMaintainerPool` scores repos and writes payout rows,
`ReleaseDueHoldbacks` resolves holdbacks after 90 days — and at the end there
is no code path that moves money.

The chokepoint that was built to gate a release has no caller:

```
$ grep -rn "GuardPayoutRelease(" --include="*.go" internal/ cmd/ | grep -v _test
internal/hackathon/judging_policy.go:59:   // GuardPayoutRelease is the single chokepoint...
```

Its own doc comment describes it as "the single chokepoint every payout
release must pass" and it is thoroughly tested. There is nothing on the other
side of it.

This is not the same shape of gap as the chain layer. There the feature is
unreachable and obviously so. Here every visible artefact of a working payout
system exists — amounts, holdback statuses, due dates, withheld destinations —
and reads as though money moved.

## What exists

| Piece | State |
|---|---|
| `ComputePayout` — units, `unit_value`, floor strategy, diminishing curve | Built, tested, called from the appeals recompute |
| `PayoutPlan` per contributor | Computed |
| `SettleMaintainerPool` — scores, gross, holdback split, due date | Built, called on the transition to `settled` |
| `ReleaseDueHoldbacks` — resolves due holdbacks against repo activity | Built, wired into the assignment runner's periodic loop |
| `GuardPayoutRelease` — confirm flag, actor, run id, shadow mode, phase, appeals closed | Built, tested, **never called** |
| Anything that transfers value | **Does not exist** |

## What is missing

Ordered by what blocks what. Sizing is deliberately coarse — the point is the
shape of the gap, not an estimate to hold anyone to.

### 1. A payout run as a stored object

`GuardPayoutRelease` takes a `PayoutRunID`, and there is no table of payout
runs. Today `ComputePayout` returns a plan that is used and dropped. A release
needs the plan it is releasing to be a row someone can point at afterwards:
which verdicts, which amounts, which `unit_value`, computed when, from which
config snapshot.

Without this there is no answer to "what exactly did we pay, and why that
number" six weeks later, which is the same requirement the draw and verdict
records already meet.

### 2. A decision on what "paid" means off-chain

This is the question to answer before writing anything. The on-chain spec
answers it for chains — escrow, Merkle root, contributor claims — but no event
can run on a chain, so the first paying GrainHack will settle off-chain. The
options are materially different in effort:

- **Reuse the redemption path.** Contributors already receive USDC on Stellar
  through the points redemption flow, with KYC and manual team review. A
  GrainHack payout could credit that same balance. Cheapest, and it inherits
  KYC, review and payout mechanics that already work.
- **A separate GrainHack disbursement.** Its own ledger and its own transfer
  path. More work, and duplicates review and KYC unless deliberately shared.
- **Pay by hand for event one.** The AI spec's own rollout suggests this. Then
  the missing piece is a report and an audited "marked paid" action rather than
  a transfer.

The third is probably right for the first event, but it still needs 1, 3 and 4.

### 3. The release action itself

An admin-triggered endpoint that calls `GuardPayoutRelease`, and on success
marks the run released and records who did it and when. The guard is written;
this is the caller it was designed for, plus persistence of the outcome.

Must be idempotent. A double-fired release on a set of payouts is the one
failure here with no clean recovery.

### 4. Maintainer payouts have the same hole

`hackathon_maintainer_payouts` carries gross, holdback, released and withheld
amounts, and `ReleaseDueHoldbacks` moves rows to `released` /
`partially_released` / `withheld`. All of it is accounting. Nothing pays the
maintainer either, and the holdback release path does not go through
`GuardPayoutRelease` at all — worth deciding whether it should.

### 5. Contributors cannot see an amount

There is no contributor-facing endpoint that returns what someone earned. The
verdict view shows the bucket, criteria and reasoning; it does not show money.
Whatever "paid" comes to mean, a contributor needs to see the number before it
arrives, and to be told it exists.

## Recommended sequence

1. Decide question 2. Everything else is shaped by it.
2. Build the payout run object (1) — needed under every option, and it is the
   record an appeal argues against.
3. Build the release action (3) on top of the existing guard.
4. Expose the amount to contributors (5).
5. Decide whether the maintainer holdback release routes through the same
   guard (4).

## Until then

`judging_shadow_mode` defaults to `true` and now blocks the transition to
live, so an event cannot quietly go live and publish results it has no way to
honour. That guard is a stopgap for exactly this gap and should stay until
payouts release.
