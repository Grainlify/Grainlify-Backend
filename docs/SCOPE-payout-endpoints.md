# Scope: payout HTTP endpoints

Investigation only. No code written.

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
- **A new consume path.** `ConsumeNonceAndUpsertUser` **creates a user** — right
  for sign-in, wrong here, where the user is already authenticated and creating
  one would be a second account for a person who has one.
- **`aptos_ed25519` in `VerifySignature`**, plus Aptos address derivation
  (`sha3-256(pubkey || 0x00)` for Ed25519, the SingleKey variant otherwise).

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
| `superseded` | frozen address is one they have since replaced | **"This pays to 0xb33b…4022, the address you registered on 3 July. You'll need that wallet — the payout was locked to it when the event settled and cannot be moved."** |

With `address_status: "superseded"`, `current_address` carries their live one so
the UI can show both.

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

## Open question

**Should `GET /me/claims` include settlements whose root is built but not yet
published on chain?** My inclination is no — return only rows with a
`published_tx`, so nothing appears in a contributor's list that they cannot act
on. The alternative shows a pending state and invites "why can't I claim this
yet" at the moment we least want the question.
