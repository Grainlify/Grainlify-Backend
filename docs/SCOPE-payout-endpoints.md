# Scope: payout HTTP endpoints

Investigation only. No code written.

**Routes**

| Route | Purpose |
|---|---|
| `POST /me/payout-address/challenge` | issue a purpose-scoped nonce |
| `POST /me/payout-address` | verify a signature and store the address |
| `GET /me/payout-address` | the current address for a chain |
| `GET /me/payout-readiness` | **does this person still need to register?** |
| `GET /me/claims` | published claims, with leaf and proof |
| `GET /me/claims/{settlement_id}` | one of them |

`/me/payout-readiness` is not an afterthought to the other five. It is the only
route that speaks to somebody who has *not* acted, and every other route in this
list is useless to a person who never registers an address.

Everything below the HTTP layer exists — `contributor_addresses`, nonce issuance
with `purpose`, canonicalisation, the reserved-range check, `claim_leaves`, and
salt-free proof serving. **There is no way for a person to reach any of it.**
That is the blocker, and it is the one with human latency in it: five testers is
a day, thirty-eight people is a week of waiting, and none of that starts until
somebody can register an address.

## The decision you asked for: proof serving keys on the USER

Not the connected address, and not the current stored address. **The
authenticated user, resolved through their address history.**

The reasoning, and it is the design paying off rather than a preference:

- `claim_leaves` deliberately has **no `user_id`** — that is what makes salt
  destruction meaningful rather than theatre. So "this user's leaf" cannot be a
  direct lookup.
- The leaf commits to the **frozen** address, and the contract requires the
  claimant to sign as that address. A payout cannot be redirected after
  publication by any means.
- `contributor_addresses` retains history via `superseded_at` rather than
  overwriting. That history is the join path: user → every address they have ever
  registered → `claim_leaves` by address.

Keying on the **connected** address strands the exact person this has to work
for: somebody who changed their payout address after a root was published
connects their current wallet, and is told they have no claim — while a real
entitlement sits in the tree payable to their previous one. Keying on the
**current stored** address fails identically.

So the response always names the frozen address explicitly, and flags when it is
not the user's current one. That difference is not an edge case to hide; it is
the single most important thing the UI has to say, because the person must claim
from a wallet they may not have connected.

## Endpoint 1 — address registration

Three routes, all behind `RequireAuth`.

### `POST /me/payout-address/challenge`

```json
{ "chain_id": "aptos-testnet", "address": "0x1b41…22c9" }
```
→ `200`
```json
{
  "nonce": "8f2c…",
  "message": "Grainlify payout address verification\nChain: aptos-testnet\nAddress: 0x1b41…22c9\nNonce: 8f2c…",
  "expires_at": "2026-08-19T15:04:05Z"
}
```

The address is canonicalised **before** the challenge is issued, so the message
commits to the same 66-character form that will be stored. Issuing a challenge
for `0x1b41…` and storing `0x0000…1b41…` would produce a signature over one
string and a row containing another.

### `POST /me/payout-address`

```json
{
  "chain_id": "aptos-testnet",
  "address": "0x1b41…22c9",
  "public_key": "0x…",
  "signature": "0x…",
  "nonce": "8f2c…",
  "scheme": "ed25519"
}
```
→ `201`
```json
{
  "chain_id": "aptos-testnet",
  "address": "0x1b41…22c9",
  "verified_at": "2026-08-19T15:04:05Z",
  "replaced": { "address": "0xb33b…4022", "superseded_at": "2026-08-19T15:04:05Z" }
}
```
`replaced` is `null` on a first registration. It is present rather than implied
because replacing a payout address is a consequential act and the response should
say what it displaced.

### `GET /me/payout-address?chain_id=aptos-testnet`

→ `200 { "address": …, "verified_at": …, "chain_id": … }` or `404` with
`{"error":"no_payout_address"}`.

### Errors, one name per cause

| Status | `error` | Cause |
|---|---|---|
| 400 | `address_malformed` | not hex, wrong length, missing `0x` |
| 400 | `address_reserved` | canonical value below `0x10` |
| 400 | `signature_address_mismatch` | key derives to a different address |
| 400 | `signature_invalid` | signature does not verify |
| 400 | `nonce_unknown` / `nonce_expired` / `nonce_used` / `nonce_wrong_purpose` | separately, never merged |
| 400 | `unsupported_scheme` | not a 32-byte Ed25519 key |
| 409 | `address_unchanged` | already the live address for this chain |

**`signature_address_mismatch` names both addresses and never silently updates:**

```json
{
  "error": "signature_address_mismatch",
  "claimed": "0x1b41…22c9",
  "derived": "0xb33b…4022",
  "detail": "The signature is valid, but the key that produced it belongs to a different address. Nothing has been saved. Connect the wallet holding 0x1b41…22c9, or register 0xb33b…4022 instead."
}
```

Storing the derived address instead would be the worst available behaviour: it is
a silent redirection of somebody's money to an address they did not name.

## Three integration details the frontend must match

These are contract, not implementation notes, and getting any of them wrong
produces a valid signature over the wrong bytes.

**1. The signed payload is the wallet's `fullMessage`, and the server
reconstructs it.** AIP-62 `signMessage` wraps our message:

```
APTOS
message: <our message>
nonce: <our nonce>
```

The signature is over that envelope, not over `message`. **The server must
rebuild it from the nonce it issued and verify against that** — never verify the
client's `fullMessage`, because a client that supplies both the payload and the
signature has been asked to mark its own homework.

**2. Call `signMessage({message, nonce})` with no optional flags.** AIP-62 lets a
caller request `address`, `application` and `chainId` be folded into the
envelope. Each adds a line, and the server cannot reconstruct what it did not
ask for. If the frontend needs one, it becomes part of this contract.

**3. Send the public key, not just the signature.** Aptos addresses are not
recoverable from an Ed25519 signature the way an EVM address is from secp256k1.
Verification derives the address from the key and compares — which is the check
`signature_address_mismatch` reports.

Derivation tries **Ed25519** and **SingleKey**, and accepts whichever reproduces
the claimed address. That is the same approach the wallet check arrived at, for
the same reason: which scheme an account uses is a property of the account, not
something to guess.

## What must change underneath

- **`CreateNonce` gains `purpose`.** It has none today.
- **`aptos_ed25519` in `VerifySignature`**, plus Aptos address derivation
  (`sha3-256(pubkey || 0x00)` for Ed25519, the SingleKey variant otherwise).
- **A consume path that cannot create a user.** See below — this is the one with
  teeth.

### The payout nonce path must be *structurally incapable* of creating a user

`ConsumeNonceAndUpsertUser` does exactly what its name says. On the sign-in path
that is correct: a wallet that has never been seen becomes an account. On a
payout path it is a live hazard, because the caller is **already authenticated**,
and minting an account there gives a person a second identity that owns their
payout address while their real account owns their contributions.

The obvious fix — a new function that happens not to call the upsert — is not
enough. A function that *can* mint an account, sitting on a payout path, is the
kind of thing somebody reuses in six months because the name looked close enough
and the signature fitted.

So the requirement is structural, and it is the same move as the AST surface
tests on `internal/salt`: make the capability absent rather than unused.

1. **`ConsumeNonceForPurpose` lives in its own file** with no import of the user
   repository and no `INSERT INTO users` in it.
2. **A source-level test asserts that**, in the manner of
   `internal/salt/surface_test.go`: parse the file, fail if it contains an insert
   or update against `users`, or an import that could perform one. A behavioural
   test cannot establish this — it can only show that a user was not created *on
   the paths it happens to exercise*, which is the weaker claim.
3. **The two are not variants of one function with a flag.** A boolean parameter
   deciding whether to create an account is a single edit away from being passed
   the wrong way, and the wrong way is silent: an extra row in `users` that
   nobody looks at until somebody's contributions and their payout address turn
   out to belong to different people.

The distinction being enforced is that **authentication mints identities and a
payout path must never do so.** Those are different jobs and they should not be
reachable from one another.

## Endpoint 2 — proof serving

### `GET /me/claims`

→ `200`
```json
{
  "claims": [
    {
      "settlement_id": "a81a583b-…",
      "chain_id": "aptos-testnet",
      "pool": "contributor",
      "contract_address": "0x1b41…22c9",
      "escrow_address": "0xe5ff…22bc",
      "asset": { "symbol": "USDC", "decimals": 6 },

      "amount_minor": "250000",
      "amount": "0.250000",

      "claim_address": "0x1b41…22c9",
      "address_status": "current",
      "current_address": null,

      "identity_hash": "0x…",
      "leaf_hash": "0x…",
      "leaf_index": 0,
      "proof": ["0x…", "0x…"],
      "root": "0x9a7d…eb5a",
      "published_tx": "0x91e4…8cb5",

      "claimed_at": null,
      "deadline": "2028-03-12T00:00:00Z"
    }
  ]
}
```

Notes on specific fields, because each is there for a reason:

- **`amount_minor` is a string.** JSON numbers are float64 in most clients and
  minor units are exact integers. A number here would round somebody's payout in
  the browser.

  This is the exact-integer rule reaching **one boundary further than it had been
  traced.** Money was moved to integer minor units in the database and in Go, and
  both of those were treated as the fix. JSON is a third boundary, and it
  reintroduces float64 silently — no error, no truncation warning, just a value
  that is very slightly not the one we sent. The general form is worth keeping:
  **an exact-value guarantee has to be re-established at every boundary the value
  crosses, and a boundary is anywhere the representation changes hands.** Storage
  and language were two; serialisation is a third; whatever the frontend does
  with it is a fourth, which is why the string is accompanied by a pre-formatted
  `amount` rather than leaving the division to the client.
- **`identity_hash` is returned, never recomputed.** The contract takes it as a
  caller-supplied argument, and recomputing it would need the salt and would
  produce a different value for anybody who renamed on GitHub.
- **`contract_address` and `escrow_address` are served, not hardcoded.** A
  frontend with a baked-in module address is a frontend that pays into the wrong
  contract after a redeploy.
- **`claimed_at` is our record and may lag the chain.** The chain is the system of
  record; this is a hint for the UI, not an authority.

### `address_status` is the field the UI turns into a sentence

| Value | Meaning | What the UI must say |
|---|---|---|
| `current` | frozen address is their live one | nothing special |
| `superseded` | frozen address is one they have since replaced | see below — the copy must not stop at the bad news |

With `address_status: "superseded"`, `current_address` carries their live one so
the UI can show both.

**The copy must carry a remedy, and this is the part most likely to be dropped.**

> **This payout goes to `0xb33b…4022`** — the address you registered on 3 July.
> It was locked to that address when the event settled, so it can't be moved.
>
> If you still have that wallet, claim with it and you're done.
>
> **If you don't, contact us — this is recoverable and your reward is not lost.**
> Unclaimed funds return to Grainlify rather than being destroyed, and we can
> arrange another way to get this to you. We can also extend the claim window
> while you sort it out.

Stopping at "cannot be moved" is accurate and is the worst possible place to
stop, because it is exactly the moment somebody who has lost a wallet concludes
the money is gone and never asks. **The damage is not the bad news; it is the
person who never asks.**

The remedy is real, and it is worth stating why rather than asserting it, because
copy that promises a remedy nobody has verified is worse than none:

- `sweep_unclaimed` requires `now >= claim_deadline` and the admin signer, so
  funds cannot leave the escrow early or automatically.
- Its destination is **fixed at initialisation** and is an address we control, so
  swept funds come back to us rather than vanishing.
- `extend_deadline` exists and is extend-only, so the window can be lengthened
  while somebody recovers a wallet.

That is the same principle already in the terms copy for a missed deadline: the
date is when we *may* act, missing it is recoverable by asking, and the copy has
to say so at the moment of bad news rather than in a document nobody reads.

### `GET /me/claims/{settlement_id}`

The same object for one settlement, `404 {"error":"no_claim"}` when the user has
no leaf in it.

## Deliberately not in scope

- **Claiming.** No submission path, no transaction building. The frontend is the
  other session's half.
- **Serving proofs by arbitrary address.** Only the authenticated user's own
  addresses. A published proof is not secret — anyone with the leaves can compute
  one, and the contract still requires the claimant's signature — but an endpoint
  that maps an address to a person is a correlation oracle, and that is what the
  salt exists to make expensive.
- **Sweep, extend, or admin routes.**

## `/me/claims` returns published settlements only — with a condition

Only rows carrying a `published_tx`. Nothing should appear in a contributor's
list that they cannot act on.

**But an empty list must never be the only signal a person gets**, and this is
the failure that decides whether the payout works at all.

Somebody with no verified address sees an empty `/me/claims` before publication
and an empty `/me/claims` after it. The two look identical and mean opposite
things:

| Before publication | After publication |
|---|---|
| They can still register and be included | They are **permanently excluded** from that tree |
| Nothing is lost | Their share is residue and becomes sweepable |
| Fixable in two minutes | Fixable only by a second settlement, if there is one |

A root cannot be edited. So the moment of publication converts a recoverable gap
into an irreversible one, and the interface says nothing different on either side
of it. **Silence cannot be the carrier of that distinction.**

### `GET /me/payout-readiness?chain_id=aptos-testnet`

Independent of any settlement, answerable before one exists, and the reason we
collect addresses early rather than at payout time.

```json
{
  "chain_id": "aptos-testnet",
  "may_be_owed": true,
  "basis": "founding_member",
  "has_verified_address": false,
  "action_required": true,
  "state": "register_now",
  "excluded_from": []
}
```

`may_be_owed` is computed from **entitlement state, not from settlements** —
founding membership and recorded shares — so it is true from the day somebody
becomes eligible, long before any tree exists. That is the whole point: an
address collected the week before a settlement costs nothing, and one collected
the week after costs somebody their payout.

`state` is what the UI branches on. One name per situation:

| `state` | Situation | What the UI owes the person |
|---|---|---|
| `not_applicable` | no entitlement basis | nothing |
| `ready` | may be owed, address verified | nothing; optionally confirm which address |
| `register_now` | **may be owed, no verified address, nothing published yet** | a persistent prompt — this is the one that must not be silent |
| `excluded_from_published` | a root was published without them | the amount they missed, and that it needs a person to resolve |

For `excluded_from_published`, `excluded_from` carries one entry per settlement
with `settlement_id`, `excluded_reason` (`no_address` or `no_github_account`) and
`remedy: "contact_support"`.

**It deliberately carries no amount, and this reverses what an earlier draft of
this document said.** `internal/founding/no_money_in_ui_test.go` enforces §6 —
no computed per-person figure may reach a UI — and it caught the draft version
when the handler was written.

The guard draws a line I had not seen, and it is the right one:

- A **claim** amount comes from `claim_leaves`. It is on chain, claimable with a
  proof, and already public. Not a promise; a published fact. The guard does not
  forbid it.
- An **exclusion** amount comes from `founding_settlement_lines`. It is a figure
  for money the person will **not** receive, with no disbursement path to honour
  it — exactly the promise §6 exists to prevent, and worse than the case §6 was
  written for because it attaches to a disappointment.

So the person is told they were excluded and what to do about it, and the number
stays in the database. The query lives in `internal/payout`, which renders
nothing, rather than in a handler.

### The prompt is specified here even though the UI is not ours

Deliberately, because this is the piece most likely to fall between the two
halves: it is not a screen anyone was asked to build, it belongs to no single
endpoint, and it only matters at a moment nobody is looking. Neither side would
naturally own it.

**`register_now` must be visible without the person going looking for it** — on
first load, not behind a payouts tab a contributor has no reason to open. Roughly:

> **Add a payout address**
>
> You're eligible for a Grainlify reward. To receive one you'll need to add an
> address you control — it takes about three minutes and you only do it once.
>
> Do it before the next payout is settled: once a payout is finalised, the list of
> addresses is locked and can't be changed for that round.

Not "or you will lose your reward" — that is the cliff wording the terms copy
already rejects, and it is not true: a person excluded from one settlement can be
included in a later one. What is true is that they miss *that* round, and the
copy should say the specific thing rather than the frightening one.
