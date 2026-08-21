# Scope: #519 — emit `contract_address`, and what else the client is hardcoding

Investigation only. No code written.

The timing argument holds and is worth restating, because it is the whole reason
this is cheap now: **no client has hardcoded a module address yet.** Today this
is an addition. Once the claim screen exists it is a migration, and the constant
will already be wrong — testnet to mainnet is a redeploy to a different address
by definition, so this is a case we are guaranteed to hit rather than one we
might.

## 1. Where the value comes from

`chain_configs.contract_address`, seeded by migration 081.

**Confirmed populated in production:**

```
aptos-testnet | contract=0x1b419fe2…22c9 | asset={"symbol":"USDC","decimals":6,…} | enabled=true
```

### There are two failure modes, not one, and both must be errors

`contract_address` is **NULLABLE**:

```
chain_id               text  NOT NULL
contract_address       text  NULLABLE     <--
explorer_url_template  text  NULLABLE
rpc_endpoint_ref       text  NULLABLE
```

So "no row for this chain" and "row exists, column is NULL" are distinct, and a
naive `Scan(&s)` turns the second into `""` — an empty string a client will
happily concatenate into `::escrow::claim` and submit. That produces a
transaction against address `0x`, which is not a Grainlify escrow and not
anything else either.

**Both must fail the request, and they should not share an error name.** "This
chain is not configured" is an operator's missing seed; "this chain is configured
without a contract" is a half-finished seed. One name per cause, the rule this
codebase already applies to nonces and wallet responses.

The precedent is `AssetDecimalsFor`, which already errors rather than defaulting:
a zero-decimal fallback would render 250000 minor units as `250000.00 USDC`. The
same shape of harm, one field over.

## 2. Claims, not readiness

**Rule as given: wherever a client would otherwise need a constant.** Applied
honestly, that is `GET /me/claims` and `GET /me/claims/{id}`, and **not**
`/me/payout-readiness`.

- **Claims** — the client builds `<contract_address>::escrow::claim` and submits
  it. Without the field it hardcodes. Clear yes.
- **Readiness** — its four states are `not_applicable`, `ready`, `register_now`
  and `excluded_from_published`. None involves a contract call: registration is a
  signature and an HTTP POST, and it deliberately touches no chain. A client
  handling readiness needs no module address, so serving one there would be
  answering a question nobody asked.

The tempting argument for "both" is that a claim screen might load readiness
first. It still does not need it there — it needs it on the row it is about to
submit, which is the claim. Putting a chain constant on a route that never
touches the chain invites a client to cache it from the wrong place and to keep
using it after the claim response stops agreeing.

## 3. The wider sweep — and it is not one instance

You were right that one found by accident usually is not one. The finding is
larger than the issue:

> **`chain_configs` has seven columns. Exactly one is read by any Go code.**

```
SELECT (asset->>'decimals')::int FROM chain_configs WHERE chain_id = $1
```

That is the only query against the table in the codebase. `contract_address`,
`explorer_url_template` and `rpc_endpoint_ref` are written by migration 081 and
read by nobody — which is the "seeded config read by nobody" shape this
repository has recorded before, and I am the one who seeded it.

| Value | Where it lives | Status | What the client does instead |
|---|---|---|---|
| `contract_address` | `chain_configs` | **written, never read** | hardcodes the module address — **#519** |
| `explorer_url_template` | `chain_configs` | **written, never read** | hardcodes an explorer URL to link `published_tx` |
| Node / network for `chain_id` | **nowhere servable** | see below | maps `"aptos-testnet"` → SDK network + node URL itself |
| `asset.symbol` | `chain_configs.asset` | **read from a literal instead** | nothing — *we* hardcode it |
| `deadline` | **nowhere at all** | documented, never emitted | would hardcode a date |

### The three worth acting on

**`explorer_url_template`.** Already seeded, already correct, never served. A
client linking a transaction has to hardcode `explorer.aptoslabs.com/txn/%s?network=testnet`
— and the `network=testnet` query parameter is wrong on mainnet in the same
breath as the module address. Same trigger, same day, same fix.

**The node URL is the one that fails identically and has nowhere to come from.**
A client must turn `"aptos-testnet"` into an Aptos SDK network and a fullnode
endpoint, and nothing we serve tells it how. `rpc_endpoint_ref` cannot be served
verbatim: it holds the *name* of an environment variable, deliberately, so that
no endpoint with an API key in it ends up in a migration. So this needs a
decision rather than a field rename — either a new nullable `public_node_url`
column, or serving a network *label* (`"testnet"`) and letting the client keep the
SDK's own default endpoints. **I would take the label**: it is not a secret, it
does not rot when an RPC provider changes, and the SDK already knows the
addresses.

**`asset.symbol` is hardcoded server-side**, at `payout_claims.go:190`:

```go
"asset": fiber.Map{"symbol": "USDC", "decimals": decimals},
```

Decimals comes from config and the symbol beside it is a literal. Nobody is
harmed today, because the seeded symbol *is* `USDC` — which is exactly why it
would survive a change to anything else. It is the same defect one layer up: not
a constant the client hardcodes, a constant *we* hardcode while appearing to
serve config.

### The one that is not a constant

**`deadline`** is a different problem and should not be folded in. It is not a
value the client hardcodes; it is data that exists **nowhere off-chain** — no
column in any table holds a claim deadline. PR #518 recorded it correctly as
"not applicable yet" rather than deleting the promise.

It matters because the terms copy depends on it: the date has to be in front of
the person, not in the terms. Reading it needs `claim_deadline(escrow_addr)` on
chain — which needs `contract_address`, so #519 unblocks it — or a column
recorded at publication. **Worth its own issue, not this one.**

## 4. What `TestPayoutScopeDocMatchesTheHandlers` does not cover

Requested addition, stated rather than fixed. Note it lives in **open PR #518**
(`fix-payout-scope-doc`), not on main and not in my worktree, so it is another
session's file to edit.

The text, for whoever lands it:

> **It has no notion of WHICH handler a key belongs to.** The check concatenates
> every `payout_*.go` source and asks whether a documented key appears
> *somewhere* in that blob. A key documented under the wrong endpoint's example
> passes, so long as any handler in the package mentions it anywhere.
>
> That is not a hypothetical gap here. The challenge, registration, readiness and
> claims shapes overlap heavily — `chain_id` and `address` appear in nearly all of
> them — so a key copy-pasted between fenced blocks while editing is precisely
> the mistake this cannot see. It would confirm the key exists and say nothing
> about the response it was documented on.
>
> Catching that needs each fenced block associated with its route and checked
> against that route's own handler, which means parsing the response shape rather
> than grepping the package.

## Recommendation

Do `contract_address` and `explorer_url_template` together, on the claims routes
only, sourced from `chain_configs`, with **two distinct errors** for the unseeded
and half-seeded cases and no empty-string fallback in either. Decide the network
label separately — it needs your call on label-versus-URL. Leave `deadline` to
its own issue. Fix `asset.symbol` while in the file, because it is one line and
it is the same mistake.
