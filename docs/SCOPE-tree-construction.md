# Scope: the settlement-to-tree path

Investigation only. No code written.

This is the piece whose absence I reported: the rule that an eligible member with
no address is excluded, and that we fund the leaf total rather than the pool, is
**built and enforced on chain** — `publish_root` requires `total == funded_total`
exactly. What does not exist is the Go that applies it. Nothing outside
`internal/chain` constructs a `ClaimLeaf`, no tree constructor is called
anywhere, and `founding.Line` has no address.

## Two findings that shape the design

### The identity hash needs a login, and a login can be renamed

`IdentityHash = H(lower(github_login) || salt)`. The login lives in
`github_accounts.login`; the stable identifier is `github_accounts.github_user_id`.
**GitHub logins can be changed by their owner.**

This matters less than it first appears, and the reason is worth stating because
it is another argument for `claim_leaves`. The chain's `claim` takes
`identity_hash` **as a caller-supplied argument** — it is never recomputed on
chain. Since we serve the stored `identity_hash` out of `claim_leaves` alongside
the proof, a contributor who renames between build and claim is unaffected. Their
hash is a snapshot of the login at build time, and the snapshot is what we hand
back.

**Recommendation: do not change the construction to use `github_user_id`.** It is
pinned by cross-implementation vectors in three repositories, and the rename risk
it would remove is already neutralised by persisting the hash. Record the
reasoning so nobody "fixes" it later. The one real consequence: `identity_hash`
is not reproducible from current data after a rename, so it must never be
recomputed as a verification step — compare against the stored value.

### `chain_configs` is empty

The table exists and holds zero rows, with no seed in any migration. Anything
resolving a chain through it today gets nothing. This is the recurring
"seeded config read by nobody" shape, arriving from the other direction: a
config read by something, seeded by nobody. Seeding `aptos-testnet` belongs in
this work, because the tree builder takes a `chain_id` and that value has to mean
something.

## Exclusion as a first-class outcome

The constraint is that a member with an amount and nowhere to send it is a
different case from one who earned nothing, and **anything that merely drops
them is wrong even if the totals come out right.**

Today `PayableLines()` filters on `AmountMinor > 0`. That is a filter, and it
conflates three unlike situations the moment addresses enter. The proposal is to
classify every member exactly once, with the tree built from one outcome and the
report enumerating all of them:

| Outcome | Meaning | In tree |
|---|---|---|
| `payable` | positive amount, verified address | yes |
| `ineligible` | excluded by rule; carries the existing `IneligibleReason` | no |
| `no_shares` | eligible, but the allocation rounded to zero | no |
| `no_address` | **earned an amount, has nowhere to receive it** | no |

`no_address` is the new one and the reason for the change. It is not a variant of
`ineligible`: those people earned nothing, and this person earned something we
cannot deliver. Conflating them in `founding_settlement_lines.ineligible_reason`
would put a real entitlement in a column whose name says there was none, and the
first person to read it would draw the wrong conclusion.

**Migration 000081** adds `founding_settlement_lines.excluded_reason`, separate
from `ineligible_reason`, plus the `chain_configs` seed.

## Shape of the work

### `founding.Line` gains

- `GitHubLogin string` — joined from `github_accounts`
- `ClaimAddress string` — canonical form, empty when none
- `Outcome Outcome` — the classification above
- `ExcludedReason string` — set only for `no_address`

The join is `founding_members` → `github_accounts` (for the login) →
`contributor_addresses` (live row for the target `chain_id`). Both are LEFT
joins: a missing login and a missing address are outcomes to record, not rows to
lose. **Never select `github_accounts.access_token`** — it is in that table and
has no business in a settlement query.

### A new package for construction

`internal/payout`, importing `internal/founding`, `internal/chain`,
`internal/salt` and `internal/payoutaddr`. The import direction matters:
`founding` must not import `payout`, so classification helpers that need only
settlement data stay in `founding`, and anything needing a salt or a tree lives
in `payout`.

This is also where `ClaimLeaf` is constructed — the first construction outside
`internal/chain`, which is the point of the exercise.

### Persistence

One transaction: create the salt, build the tree, insert `payout_event_roots`
and every `claim_leaves` row. `payout_event_roots.settlement_id` is a primary
key, so a second build for one settlement fails on the constraint rather than on
a check somebody might remove.

The salt is created **at tree build, not at settlement**, so a settlement that
never becomes a tree never acquires a secret we then have to manage and destroy.

### Bounds

Amounts are `big.Int` in Go and `u64` on chain. The builder must refuse a leaf
whose amount exceeds `math.MaxUint64` rather than truncating — a wrapped amount
is a valid-looking leaf for the wrong number, which is the family of the
key-length trap: a fault presenting as a legitimate value.

## The dry run, which is the gate

Emitted before anything touches the chain, and the thing you read.

**Contents**

1. **Every leaf**: user id, GitHub login, canonical claim address, amount in
   minor units and in USDC, and the tree index.
2. **Every excluded member by name**, with their amount and which outcome —
   `no_address` members listed separately and first, because they are the ones
   losing something recoverable.
3. **Totals that reconcile**, shown as an explicit identity rather than four
   numbers side by side:

   ```
   pool total          (founding_settlements.pool_minor)
   - leaf total        (sum of payable leaves)  <- what must be funded
   - excluded total    (sum of no_address amounts)
   - rounding residue  (apportionment remainder)
   = 0
   ```

   with `residue = pool total - leaf total` called out separately as **what would
   be sweepable with no timelock** if the pool were funded instead of the leaf
   total.

**It writes nothing, and it needs no salt.** That falls out of what a human
actually checks: names, addresses, amounts, who is in and who is out, and whether
the arithmetic closes. Leaf *digests* are not reviewable by eye, so the dry run
does not compute them — which means it needs no salt, creates no secret, and
keeps the `DryRun`/`Persist` split already established in `founding`. Digests and
the root appear in the build output, which is what gets archived alongside the
publish transaction.

### Freezing what was reviewed

Exclusion is irreversible once the root is published, and a dry run read on
Monday can be built from on Tuesday against changed data — somebody registers an
address, an allocation is corrected, a login is renamed.

**Proposal: the dry run emits a digest over its own inputs** (the ordered set of
user id, login, address, amount), and the build refuses unless it recomputes the
same digest. Then a change between review and publication is a loud failure
instead of a root that quietly does not match what was approved.

That converts "read the report, then run the build" from a convention into a
check. Cost: an address registered in the gap forces a re-review rather than
being silently included — which is the correct direction, since being silently
included is also being silently excluded for whoever the report said was in.

## What this does not cover

- **Funding, publishing and claiming.** This produces the tree, its rows and the
  report. Moving money is the next piece.
- **The maintainer pool.** The founding pool is `contributor`; the second pool
  is out of scope here.
- **Serving proofs.** `claim_leaves` makes it possible; the endpoint is separate.

## Open questions

1. **Should `no_address` members block the build entirely, or only warn?** A hard
   stop above some count — say, more than 10% of the pool — would prevent a tree
   built while the address-registration flow is quietly broken. My inclination is
   to make it a required acknowledgement rather than a threshold.
2. **Is the login required, or can a member without a `github_accounts` row still
   be paid?** Today the identity hash needs a login, so no. That makes a missing
   login a fourth outcome rather than an error.
