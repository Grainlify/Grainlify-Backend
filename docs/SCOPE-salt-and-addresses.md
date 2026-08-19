# Scope: salt storage, claim_leaves, contributor_addresses

Investigation only. No code written.

These were ordered once and never built. They are the only thing between us and
a payout: a tree cannot be built without a salt, and a proof cannot be served
after settlement without persisted leaves.

## What exists today

| Thing | State |
|---|---|
| `IdentityHash = H(lower(login) \|\| salt)` | **Built**, vector-pinned, refuses an empty salt (`ErrNoSalt`) |
| `Proof()` / `VerifyProof()` | **Built**, vector-pinned |
| `payout_event_roots` (mig 078) | **Built** — `settlement_id` PK, root, `total_minor`, `leaf_count` |
| `SALT_ENC_KEY_B64` | Read into config. **Read by nobody.** Default empty |
| Salt storage / production | **Nothing.** No table, no generator, no encryption |
| `claim_leaves` | **Nothing** |
| `contributor_addresses` | **Nothing** |
| `wallets`, `auth_nonces`, `verify.go` | **Built and live** — nonce issuance, expiry, single-use, uniqueness |

So the salt has a key and nowhere to live, and the leaf builder has a function
that demands a salt nobody can supply.

## Part 1 — the salt

### Storage

`payout_event_salts`, keyed to `settlement_id` (PK, FK to `founding_settlements`).

- `ciphertext BYTEA`, `nonce BYTEA`, `key_version INT`
- `created_at`, `destroyed_at TIMESTAMPTZ NULL`

**AEAD with `settlement_id` as associated data.** Without that binding a
ciphertext can be copied from one settlement's row to another's and still
decrypt, which would silently give two events the same salt and make their
leaves cross-correlatable — the one property the per-event salt exists to
provide.

**Destruction is a tombstone, never a `DELETE`.** Set `ciphertext` to NULL and
stamp `destroyed_at`. A deleted row cannot answer "did this event have a salt,
and when did we destroy it?" — and that question is the audit trail for an
irreversible act. `ON DELETE RESTRICT` from the settlement, matching
`payout_event_roots`.

### The API: plaintext never leaves the package

Taking the stronger version, as directed. One entry point, shaped roughly:

```
WithSalt(ctx, settlementID, func(h Hasher) error { ... })
    Hasher.IdentityHashes(logins []string) ([][32]byte, error)
```

- No `Bytes()`, no getter, no `Salt` value that escapes the closure.
- **Batch in, batch out.** This is the part that makes the constraint hold:
  a per-login call invites a caller with a legitimate need for 40 hashes to ask
  for the raw salt so they can loop. With a batch entry point there is no such
  need, so there is no honest argument for an accessor — which is what stops one
  being added later by someone reasonable.
- Decrypt per call, zero on return. **No cache, no `sync.Once`, no package var.**
- **Key rotation lives inside the package too** — decrypt and re-encrypt without
  the plaintext crossing the boundary. Rotation is the most likely reason
  somebody would otherwise add an accessor.

### Why not `String()` / `MarshalJSON` redaction

Because the rule would be narrower than the mistake. Redacting those two defends
the paths we thought of; `%v` on an enclosing struct is the one we didn't, and
`%x` on a byte slice, and a debugger, and a struct dumped into an error. Each
patch would be correct and the class would survive.

This is the meta-rule we have hit before, and it is the same shape as the
`vector<u8>` sequence in the wallet tool: three fixes for "this field must be
bytes" before the rule was restated as *a value crossing a typed boundary must
be constructed, never spelled*, which is what finally held. **When a fix prompts
a rule, ask whether the rule is as general as the mistake.**

### What the closed API costs

Stating it plainly, because it is a real cost and I would still pay it:

1. **New uses need a new method, not a call.** An auditor export, a one-off
   support question, a migration tool — each becomes a reviewed code change
   rather than something done at a REPL. That is the point, and it is also
   friction on the day somebody needs it urgently.
2. **Tests cannot construct arbitrary salt states from outside.** Needs an
   in-package helper, which is a small ongoing tax and a place where the
   discipline could be quietly relaxed.
3. **No composition.** A caller wanting the hash *and* something else derived
   from the salt cannot combine them; they need a method that returns both.
4. **Batching forces chunking on very large sets.** At a few dozen leaves this
   costs nothing; it would matter at 10^6.

### The limit this does not remove, and must not be claimed to

**Our own database always holds the mapping.** `contributor_addresses` maps user
to address; `claim_leaves` maps address to amount. Joined, they reconstruct
exactly what the salt protects — without needing the salt at all.

The salt defends against someone holding **the chain plus public GitHub logins**,
which is the model `merkle.go` already describes accurately. It does not defend
against a compromise of our database. Destroying a salt closes the external path
and leaves the internal one open.

Two consequences worth deciding separately, not silently:

- If we want the link genuinely unrecoverable, `contributor_addresses` needs a
  purge story too. Otherwise "we destroyed the salt" overstates what changed.
- **`SALT_ENC_KEY_B64` and `DB_URL` are both Railway environment variables.**
  Encryption at rest separates key from data only if they are separately
  compromisable; today one Railway compromise yields both. The design is still
  worth building — it defends against a database dump, a backup leak, and a Neon
  console — but not against the environment itself.

## Part 2 — `claim_leaves`, and why it is the same job

`claim_leaves` is the per-leaf detail under `payout_event_roots`' existing
`settlement_id` key:

- `settlement_id`, `leaf_index INT`, `leaf_hash BYTEA(32)`
- `claim_address TEXT`, `amount_minor BIGINT`, `identity_hash BYTEA(32)`, `pool`
- `UNIQUE (settlement_id, leaf_index)` and `UNIQUE (settlement_id, leaf_hash)`

`leaf_index` is not decoration: proofs must be reproducible, so the sorted
position has to be stored rather than recomputed from a set whose ordering
depends on data we may no longer hold.

### The design decision that makes a cold salt possible

**Proofs are served by `claim_address`, not by user.**

The alternative — storing `user_id` on the leaf — makes proof serving trivial and
makes salt destruction theatre, because the mapping the salt protects would sit
in the table in plaintext.

Keying on address avoids it, and costs nothing, because the contributor must
connect that wallet to claim anyway. They prove control of the address with the
same signature machinery; we return the proof for the leaf paying it. No salt,
no login, no identity lookup.

This also means `claim_leaves` publishes nothing the chain does not already
publish — every address and amount in it appears on-chain at claim time.

So the sequence is: build the tree with the salt → persist leaves → publish root
→ **the salt is never needed again** and can be destroyed on whatever schedule we
choose. Without `claim_leaves`, the salt can never go cold, which is why these
are one job.

## Part 3 — `contributor_addresses`

Reuse the machinery, separate the storage.

**Why separate**, restating the two reasons so they are in the record: `wallets`
answers *prove you hold this key* — authentication. A payout address answers
*send money here* — a destination. Merged, a wallet added to sign in silently
becomes somewhere money is sent. And a payout address must be **frozen at
leaf-build time** because the leaf commits to it, while `wallets` is mutable
current state; one row means changing your sign-in wallet either rewrites where
past money was headed or disagrees with it. **`contributor_addresses` holds
current intent; `claim_leaves` holds the frozen copy.**

Shape:

- `user_id`, `chain_id` (e.g. `aptos-testnet`), `address TEXT`
- `verified_at`, `nonce_id` (what was signed), `created_at`, `superseded_at`
- `UNIQUE (user_id, chain_id) WHERE superseded_at IS NULL` — one live payout
  address per chain, with history retained rather than overwritten

### Does the `wallet_type` CHECK need extending for Aptos?

Precise answer, because the split changes it:

- **`wallets.wallet_type` — no.** Nothing signs in with an Aptos wallet.
  Untouched.
- **`auth_nonces.wallet_type` — yes, if we reuse it**, and we should: it already
  solves issuance, expiry, single-use and uniqueness. Nonces are machinery;
  addresses are storage. Extending one CHECK is the smaller change than
  duplicating a working challenge table.

**One addition that is not optional: domain separation.** `auth_nonces` needs a
`purpose` column (`signin` / `payout_address`), and the signed message must
differ per purpose. Without it, a signature collected to sign in can be replayed
to register a payout destination — the two now mean different things, and the
challenge must say which one it is.

### Address validation

- **Reject anything below `0x10`.** `0x0`–`0xf` are Aptos reserved and framework
  addresses; a payout sent there is unrecoverable. Enforce at registration, where
  the person can still fix it, and assert again at tree build as defence in
  depth.
- **Canonicalise before storing or comparing.** Aptos addresses may be written
  short (`0x1`) or padded to 64 hex characters. Two spellings of one address
  defeat `UNIQUE` and produce a leaf that commits to a string the wallet will
  not match. Store the 32-byte canonical form. Same rule as the wallet tool:
  constructed, never spelled.
- Reject an address whose checksum/length is invalid rather than normalising it
  into something plausible.

## Migrations, and order of work

Production is at **78**. These take **79** and **80**:

- `000079_payout_salts_and_leaves` — `payout_event_salts` + `claim_leaves`
- `000080_contributor_addresses` — plus the `auth_nonces` `purpose` column and
  its widened CHECK

Order, matching the original one: salt storage and `claim_leaves` first — they
unblock building a tree at all — then `contributor_addresses`, which is what
makes the addresses in that tree real rather than assumed.

Boot-time refusal for an empty `SALT_ENC_KEY_B64` once a salt row can exist,
following the pattern main already set in "Refuse to boot when a live feature has
no configuration".

## Decisions

### Salt destruction is a deliberate act, never scheduled

Same principle as the sweep deadline: **the cutoff is an act, not a clock.** A
scheduled destruction fires while nobody is watching, and this one is
irreversible in both directions.

There is a second reason specific to the limit below. Because
`contributor_addresses` joined to `claim_leaves` reconstructs the mapping
anyway, a scheduled destruction would mostly produce *the feeling of having done
something*. So it is an act somebody takes, and the tombstone records **a reason
alongside `destroyed_at`** — `destroyed_reason TEXT`.

### `contributor_addresses` is not purged, and the claim is not made

A purge cannot work here, and the reasoning belongs in the record rather than a
purge being invented to satisfy the shape of the problem.

**The address has to stay current to receive future payouts.** So for as long as
somebody keeps the same payout address, the join keeps reconstructing the
mapping. That is not a gap a retention policy closes — a policy that deleted the
address would break the next payout, and one that kept it changes nothing.

The correct response is therefore **not a purge story. It is never making the
claim that a purge would be needed to support.** `merkle.go` already states the
property accurately: the salt raises the cost of *bulk* correlation by someone
holding the chain and the public list of GitHub logins. Nothing anywhere else may
overstate it — not these docs, not marketing, not a grant application, and in
particular never a sentence of the form *"we destroyed the salt, so we cannot
link you."* That sentence would be false.

**A future lever, noted and not built:** if a real one is ever wanted, it is
`claim_leaves` retention — dropping leaf rows after the claim window closes and
everything is claimed or swept. The trade-off is dispute resolution: those rows
are how we answer "you say I was paid, I say I was not", and after they are gone
the only record is the chain, which knows addresses and amounts but nothing about
who anyone is. Do not build it now.

### One payout address per chain

Per-settlement storage buys nothing, because **the freeze already provides
per-settlement semantics.** The address is frozen into `claim_leaves` at tree
build, so changing it between events already redirects only future ones.
Per-settlement storage would add only the ability to change an address for an
event whose tree has not been built yet — which is identical to changing it now.

And it costs the thing we can least afford. The largest friction in the payout
path is already a contributor installing a wallet, switching network and signing.
Making them repeat that per event is the version nobody completes.

## An eligible member with no registered address at tree build

**They are excluded from the tree, the root total comes in below the pool, and
the difference stays in the treasury as residue.** That is the intended
behaviour, and it is the exact case the fund-the-leaf-total rule exists for.

**On the chain side this is already built and enforced**, not merely planned.
`publish_root` requires `total == funded_total` — exactly, not at most — and the
comment above that assertion names this case directly: funding the whole pool
would leave the excluded member's share sitting in the escrow as
`balance - root_total`, which is precisely what `sweep_residue` returns with **no
timelock**. Their one protection, the claim window, would not apply. So the
operational rule is *fund the leaf total, never the pool total*, and the contract
enforces it rather than trusting a runbook.

**On the Go side it is not built, and I should not have implied otherwise.**
Checked rather than assumed: `internal/founding.Line` has no address field,
nothing outside `internal/chain` ever constructs a `ClaimLeaf`, and no tree
constructor is called anywhere. The settlement-to-tree path does not exist yet.
What exists is the rule and its on-chain enforcement; what has to be written is
the code that applies it.

Two things that path must do, neither of which exists:

1. **Exclude by address, and record why.** `Line` already carries
   `IneligibleReason` for people who got nothing, on the stated principle that
   why somebody got nothing matters as much as why somebody got something. A
   member excluded for having no address is a *different* case — they earned an
   amount and have nowhere to receive it — and it needs its own recorded reason
   rather than being silently dropped from `PayableLines()`.
2. **Make it visible before the tree is built, not after.** Exclusion is
   irreversible once the root is published: the tree cannot be edited, so their
   only remedy is a later settlement. Someone in that position should be told
   they are about to be excluded while registering an address still helps.

## Known limits

### Our own database reconstructs the mapping

Recorded above. Not a defect to fix; a claim never to make.

### Both secrets share one environment — acceptable for testnet, blocking for mainnet

`SALT_ENC_KEY_B64` and `DB_URL` are both Railway environment variables, so
encryption at rest defends a database dump, a backup leak and the Neon console,
but not a compromise of the environment holding both.

**This is accepted for testnet, where the funds have no market value.**

> **Trigger: moving the salt key out of the application environment is a
> precondition for mainnet.** Not a recommendation, not a follow-up — a gate.
> Anyone proceeding to mainnet with both secrets in one environment is
> overriding this decision, and should have to say so.

Written as a trigger rather than a regret so that it is a decision someone takes
deliberately, in the same spirit as the multisig requirement recorded for the
mainnet deployer account.
