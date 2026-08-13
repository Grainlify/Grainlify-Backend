# Gap: what it takes to run one GrainHack event on one chain

Scope only. Sizing is coarse and deliberately so — the useful output is the
shape and ordering of the gap, not an estimate.

## Where it actually stands

The on-chain spec's build order runs to eight stages. Stages 1 and 2 are
written and tested; the Soroban contract from stage 4 exists. But the chain
layer as a whole is **unreachable**, and the reason is a single missing piece:

> **Nothing in the codebase inserts a row into `chain_configs` or
> `hackathon_chain_pools`.** No handler, no admin endpoint, no job, no seed
> migration.

`hackathon_chain_pools` is, by the spec's own words, "the source of truth for
which chains an event runs on". Every chain-aware path starts by asking
`EventChains` which chains an event runs, gets an empty list, and takes its
no-chain branch. Correctly — that is the designed behaviour for an off-chain
event. It also means every line of stage 1 and 2 code is dead in production
and nothing has ever exercised it outside tests.

So the gap is not "finish stages 3 through 8". It is closer to: **one small
missing piece makes the existing work reachable, and then four genuinely
missing pieces stand between reachable and paying.**

## The five pieces

### 1. A way to create a chain pool — small, and unblocks everything

An admin path to enable a chain (`chain_configs`: id, asset, minimum
confirmations, contract address, explorer template) and to attach pools to an
event (`hackathon_chain_pools`: contributor pool, maintainer pool, decimals).

Both tables exist with their constraints. This is CRUD plus validation.

The moment one row exists, a large amount of currently-dormant code goes live
at once — per-chain payout arithmetic, per-chain maintainer scoring, the
draw's refuse-without-a-confirmed-commit rule, the intake chain check. **That
is the risk in this item, not the code.** It should land behind an explicit
enable, and the first pool should be created against a mock adapter on a test
event, never straight onto something real.

### 2. Wire what is already built — small, mechanical, high value

Six symbols are built, tested and uncalled. Each is one call site:

| Symbol | Call site it needs |
|---|---|
| `ValidateIssueChain` | Issue intake, where an issue enters an event |
| `RetagIssueChain` | An admin/maintainer endpoint for changing an issue's chain |
| `UntaggedIssuesInChainEvents` | The periodic sweep, alerting on what it finds |
| `ComputePayoutsPerChain` | Settlement, replacing the single-pool call |
| `SettleMaintainerPoolPerChain` | Same, for the maintainer pool |
| `AssignmentRunner.WithChains` | Runner construction in `cmd/api` |

The last one is worth calling out. `CheckDrawCommit` is already wired into the
draw loop and looks active on inspection, but the runner's registry is never
attached, so it returns `nil` immediately. **The refuse-to-draw-without-a-
confirmed-commit rule is inert and appears live.** Attaching the registry is
one line and turns on a hard blocker; do it deliberately, not incidentally.

### 3. A real chain adapter — the first genuinely large item

Only `MockAdapter` implements `ChainAdapter`. `internal/soroban` exists but is
the bounty and program escrow client — different contract, different surface,
not an implementation of this interface.

Needed: a Soroban adapter implementing all thirteen interface methods against
the `grainhack-escrow` contract, plus the signer service the spec requires —
the adapter builds unsigned transactions and never holds keys. The signer is a
separate deployable with its own custody story, and it is the item most likely
to be underestimated.

Also required before real funds: the external audit of the contract, and a
full testnet lifecycle — fund, commit, reveal, root, claim, sweep.

### 4. Three missing tables, and the flows on top of them

The spec's data model lists these; none exist.

**`draw_seeds`** — issue id, encrypted seed, commit hash, commit tx, committed
at, revealed at, reveal tx. Without it there is nowhere to hold a seed between
commit and reveal, so the commit-reveal protocol cannot run at all. Note this
is the item the whole "verifiable draw" claim rests on: it is the highest
trust-per-line thing in the on-chain spec, and it is entirely absent.

Needs: seed generation and encryption at window open, publishing the commit,
revealing at draw time, and the retry path when a commit has not confirmed
before the window closes (`ExtendWindowForCommit` is built and uncalled).

**`claim_addresses`** — user id, chain id, address, verified at. Contributors
register their own; Grainlify never derives or infers one. Needs a
contributor-facing registration flow with per-chain address validation, which
the adapter interface already exposes.

**`claim_leaves`** — hackathon id, chain id, user id, leaf hash, amount, proof,
root, claimed at, claim tx. Needs leaf construction at settle, root
publication, proof serving (rate-limited — it is an enumeration surface), and
the contributor-facing claim UI.

The Merkle mathematics for all of this is built and pinned by golden tests.
What is missing is the storage and the flows, not the cryptography.

**Per-event salt storage.** `IdentityHash` requires a salt and refuses without
one. Nothing generates, encrypts or stores a per-event salt today. It must be
encrypted at rest and never released — including to us, operationally.

### 5. Reconciliation and operations

The spec requires a job comparing on-chain state to database state per event
per chain, multisig on anything moving funds outside the claim path, and a
funded-escrow blocking check on the transition to live. `FundAllEscrows` and
`PublishConfigHashAll` are built; the phase transition does not call them, so
today an event can go live with no escrow at all.

## Ordering, and where the cliff is

```
1. Pool creation ─────────────┐
2. Wire the built code ───────┴──> a chain-aware event that runs, no money
3. Adapter + signer ──────────┐
4. Seeds / addresses / leaves ┴──> an event that escrows, draws verifiably,
5. Reconciliation + guards ────┘    and can be claimed against
```

Items 1 and 2 are small and mostly reversible. Items 3 to 5 are the real
build, and 3 carries a custody and audit burden that is not a coding estimate.

**The cheap and valuable move is 1 and 2 against the mock adapter**, on a
throwaway event. That exercises two stages of untested-in-production code,
turns up whatever the abstraction got wrong, and costs nothing on-chain — which
is exactly what the spec's own stage 1 said to do and what has not happened
yet.

## One prerequisite that is not on this list

None of the above pays anyone even off-chain, because nothing disburses at all
— see `GAP-grainhack-payout-release.md`. A chain-running event would escrow
funds correctly and still have no path to a contributor's hands on the
off-chain side. That gap is the more urgent of the two.

## Recorded decisions that no contract honours

Separate from the five pieces above, because these are not missing work so much
as claims that outrun the code. Both are published — §11's decision table is
rendered by the public rules page — so the gap is between what a reader is told
and what exists.

**Contract upgradeability.** §11-#4 settles it: upgradeable, behind multisig and
a timelock exceeding the claim window. The Soroban contract implements no
upgrade path at all — there is no `upgrade` entrypoint, no admin-settable Wasm
hash, nothing. `contract_upgrade_policy` is one of the four Chains config keys
declared `Active: false`, so the rules page labels it *"not in effect yet"*,
which is accurate about the config and silent about the contract. The Solidity
port must either implement the recorded policy or the policy must change; what
it must not do is inherit the same silence on a second chain.

**The other three inactive Chains keys** — `empty_chain_pool_disposition`,
`unclaimed_sweep_days`, `unclaimed_sweep_destination` — are in the same
position. `unclaimed_sweep_days` is the sharpest of them: the contract has a
`sweep_delay` fixed at initialisation, and nothing reads the config key that is
supposed to govern it, so the published number and the enforced number are
independent by construction. That is the same class of problem as the referral
window before it was served from the constant that enforces it, and it wants the
same fix.
