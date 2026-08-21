# Scope: production gas sponsorship for claims

Investigation only. No code written. Gap filed as
**[#537](https://github.com/Grainlify/Grainlify-Backend/issues/537)**.

## What has to exist

One endpoint: the deployed equivalent of `tools/wallet-check/serve.js`'s
`/sponsor`. A contributor's wallet signs as **sender** of an AIP-39 fee-payer
transaction; this signs as **fee payer** and submits.

```
POST /me/claims/:settlement_id/sponsor
  { "sender_authenticator": "0x…", "raw_transaction": "0x…" }
→ { "transaction_hash": "0x…" }
```

The contract needs no change. `claim` is a plain `public entry fun` and cannot
tell who paid; sponsorship is entirely transaction-layer. That is exactly the
true statement that hid this gap, so it is worth restating with its missing
half: **the contract's indifference is why sponsorship is possible, not evidence
that anybody is doing it.**

## The refusal path

Shape checking — "is this a `::escrow::claim` against an escrow we published" —
protects the escrow and does nothing for the sponsor account, because **a failed
transaction still costs the fee payer gas**. A correctly-shaped claim against a
real escrow, for a leaf already claimed, aborts on chain and we pay. In a loop,
that drains the account, and every attempt passes shape checking.

Four defences. **None is droppable**, and they fail in different directions on
purpose.

### 0. Simulate **before signing** — verified against testnet, not assumed

Aptos exposes `POST /v1/transactions/simulate`. A simulated transaction executes
against real state and **costs nothing**.

**It runs before the contributor signs.** Measured on the real testnet node
against a purpose-built escrow:

| what was simulated | result |
|---|---|
| a valid claim, **zero signature** | `success=true`, `gas_used=4431`, *Executed successfully* |
| the same claim, wrong amount | `success=false`, `gas_used=49`, Move abort |

The node checks that the public key matches the account's **authentication key**;
it does not verify the signature. So what is needed is the contributor's *public
key*, not a signature — and a first attempt with a zero public key confirms the
distinction by returning `INVALID_AUTH_KEY`.

That changes what this defence is. It is not a post-hoc refusal after somebody
signs; it is a **pre-signature check**: the person never sees a wallet prompt for
an action we already know aborts, and "you have already claimed this" can be said
before asking them to approve anything.

The public key is stored at registration (migration 088) — we already received
and verified it, and simply never kept it.

**Fail open.** A simulation we cannot run is not evidence of a bad claim.
`ErrSimulationUnavailable` is a distinct error from an abort, and a test asserts
neither satisfies the other, because our own outage refusing somebody their money
in the contract's voice is the worse failure.

This subsumes the already-claimed case and every other abort: a bad proof, a
wrong amount, a swept escrow, a deadline passed, an identity hash that does not
match the leaf. The check in (1) knows one reason a claim aborts; simulation
knows all of them, including the ones nobody has thought of — which is the same
argument that makes (3) worth having.

It is not a replacement for (1), because a simulation is one more round trip and
`IsClaimed` is the cheap, legible answer to the overwhelmingly common case. But
if only one could be built, this is the one.

**Cost:** one extra RPC per sponsorship. **Catches:** every deterministic abort.
**Misses:** anything that changes between simulating and submitting — which is
(2)'s job.

### 1. Read claim state before sponsoring

`chainread.IsClaimed` exists and is already used by `/me/claims/:id/chain`.
Handles the ordinary case: somebody clicking Claim twice, or a stale tab.

**Not droppable** even given (0): it is cheaper, it names the reason precisely
for the response the contributor sees, and *"you have already claimed this"* is a
better sentence than *"the transaction would fail"*.

### 2. Rate-limit per user

Both (0) and (1) are **time-of-check-to-time-of-use**. Between reading and
submitting, the same leaf can be claimed by another submission — including one
we sponsored a moment earlier. The window is small and non-zero, and an attacker
who can loop can hit it.

**Not droppable, and it is the one that survives being wrong about everything
else.** It bounds the cost of losing the race, and it bounds the cost of a bug in
(0) or (1) — which is precisely why it must exist even if both are trusted.
A defence that only works when the other defences work is not a defence.

Proposed bound: **a small number of sponsored submissions per user per hour**,
plus a per-settlement cap, since a user has at most one leaf per settlement and
therefore needs at most one successful sponsorship for it.

### 3. Alarm on sponsor balance

The only one that catches an attack nobody anticipated, and the **symptom is what
makes it urgent**: once the account empties, claims fail for *everyone*, not just
whoever drained it. The griefing cost is bounded by the attacker's effort; the
damage is not bounded at all.

Reuses the KYC alerting path, as the reconciler does.

#### The threshold, derived rather than rounded

From the recorded milestone transaction, not an estimate:

```
14,920 gas units × 100 octas   = 1,492,000 octas = 0.01492 APT per claim
38 contributors (founding pool) = 0.5670 APT for a full settlement
× 3 margin                      = 1.7009 APT
```

The margin covers gas-price movement (100 octas/unit was observed, not
guaranteed), the first-claim-per-recipient case being the expensive one, and
retries.

**Two thresholds, not one.**

A **hard floor at 1.70 APT** where the service *stops sponsoring and says so*,
beneath a **dynamic alarm** above it. The floor exists because an alarm needs a
recipient, and no channel currently reaches anyone off-site — an alarm nobody
reads is the deadline problem again: a signal that fires correctly and changes
nothing. A floor depends on nobody reading anything. It bounds the drain and
surfaces as a clear message to a contributor, who then tells us. **Failing
visibly beats alerting into a void.**

**Alarm at 1.70 APT or above.** The basis is *"enough to pay for every remaining unclaimed
leaf, three times over"* — so the alarm fires while everyone still owed can still
be paid, rather than when the account is empty and they cannot.

Better still, and cheap: make it **dynamic** — `unclaimed_leaves × 0.01492 × 3` —
because the fixed number is only correct for a 38-person settlement and will be
quietly wrong for the next one. The fixed floor stays as a lower bound.

*Current balance: 9.7820 APT, about 17 full settlements.*

## The sponsor key

Same treatment as `rpc_endpoint_ref`, for the same reason.

- **Lives in:** the API's environment, as `APTOS_SPONSOR_KEY` — nowhere else.
- **Read by:** exactly one function, in one file, which signs and returns an
  authenticator. Nothing else imports it.
- **Never reaches:** a migration, a log line, an error body, a response, or a
  test fixture. `chain_configs` may hold the sponsor **address** (public, and
  useful for the balance alarm); it must never hold the key.
- **Enforced by:** a surface test in the manner of `internal/salt` and
  `internal/auth/payout_nonce.go` — parse the file, fail if the key escapes the
  signing function or appears in a format string.

The precedent is deliberate: `internal/salt` has no accessor rather than a
carefully-unused one, and the payout nonce path cannot create a user rather than
merely not doing so. **Make the capability absent, not unused.**

An absent or malformed key must **refuse to boot** rather than falling back to
unsponsored claims. A silent fallback here is the failure this whole issue is
about, arriving a second time: it would work in development and mislead in
production.

## Testnet will not test this

APT is free from a faucet, so a self-paid rehearsal passes. Any test of this
endpoint must assert **who paid**, not that the claim succeeded:

- the claimant's APT balance is **unchanged** across the claim;
- the sponsor's balance falls by exactly `gas_used × gas_unit_price`;
- the transaction's authenticator is `fee_payer_signature`.

That is what `scripts/sponsored-claim.js` already asserts, and it is the shape
the production test must keep. A test that only checks the USDC arrived would
pass on the self-paid path.

## Out of scope

- **Sponsoring anything but `claim`.** One function, on escrows we published.
- **Mainnet key management.** The mainnet deployer is a different account and a
  multisig; this is testnet.
- **The claim UI.** The frontend's half.

## Self-paid: offered, but never as a fallback

**Decided.** Self-paid is not a feature to build — it is what happens when the
client submits through the wallet without routing via the sponsor endpoint. So
the real question is whether the claim UI ever offers that route, and the answer
is yes, under three conditions:

1. **Only after the sponsored path has failed.** Never attempted first.
2. **Chosen explicitly by the person.** Never pre-selected, never automatic,
   never silent.
3. **With the cost named**, and the first-claim-is-dearer point stated — the
   first claim per recipient creates their token store and is the expensive one.

The case that decides it: somebody three days from a deadline, with APT in their
wallet, and our sponsor account empty. **Refusing them their own money to protect
a policy is the wrong answer.**

This does not weaken the default path. `LoadSigner` still refuses to boot on an
absent key rather than silently falling back to unsponsored claims — that is a
server-side default becoming wrong in production, which is a different thing from
a person making an informed choice at the moment they are blocked.

## Still to build

The four defences, the key handling and the pre-signature simulation are done.
**The submission half is not**: signing as fee payer requires BCS encoding of
`RawTransactionWithData::MultiAgentWithFeePayer` and its signing message, which
means taking on `github.com/aptos-labs/aptos-go-sdk` (v1.13.0 available, to be
pinned exactly — a range dependency is what broke the wallet tooling once
already). The endpoint cannot exist until that lands.
