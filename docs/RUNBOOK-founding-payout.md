# Runbook: paying the founding pool

Every step somebody performs, in order, from an empty database to money in
contributors' wallets. Nobody has performed the whole sequence. Every individual
piece has been exercised; the join between them has not.

Read this the whole way through once before starting. Several steps are
irreversible and two of them are irreversible in ways that are not obvious.

---

## Before you start

**`cmd/payout` is the entry point for every database step.** It did not exist
when this runbook was first written — four packages were built, tested and
mutation-tested with nothing calling them, and writing this document is what
found it.

```
READ-ONLY (repeatable, writes nothing)
  payout dry-run  --pool-usdc <amount>
  payout report   --settlement <id>          the gate
  payout status   --settlement <id>

IRREVERSIBLE (each its own act, deliberately)
  payout persist  --pool-usdc <amount>
  payout build    --settlement <id> --digest <hex> --acknowledge-undeliverable <minor>
  payout publish  --settlement <id> --escrow <addr> --tx <hash>
```

The irreversible steps are **separate subcommands, not flags**. Three of the five
irreversible moments below complete quietly, and a subcommand somebody has to
type is friction in exactly the right place. The read-only ones are trivially
repeatable on purpose: the gate only works if re-reading the report is cheaper
than arguing with it.

**Every subcommand refuses a non-local database** unless you name the host with
`--yes-run-against-remote-host=<host>` — the same guard as `cmd/migrate`, shared
from `internal/dbguard` rather than copied. The stakes differ though: a migration
against the wrong database is recoverable, while `persist` and `build` against
the wrong one produce a settlement and a tree for an event that does not exist
there, and a `publish_root` funded against that tree moves real money on its
strength.

**`--pool-usdc` is required and never defaulted.** The library falls back to 3000
USDC when the key is absent, which is a sensible library default and a dangerous
operator one. The number that decides how much money is distributed should be
typed by the person distributing it, and should appear in their shell history
next to the command that used it.

**Do not run these steps through a Go test.** A test writes to whatever
`TEST_DB_URL` names, and building the habit of pointing one at a real settlement
is precisely what the host guard exists to prevent.

---

## Phase 0 — preflight

**Run every `aptos` command in this runbook from the `Aptos-Contracts` checkout.**
The `grainlify-testnet` profile lives in `Aptos-Contracts/.aptos/config.yaml`,
which is gitignored and therefore **absent from every git worktree**. Run one from
anywhere else and you get:

```
Unable to find config /Users/<you>/.aptos/config.yaml, have you run `aptos init`?
```

which reads like a missing installation and is a wrong working directory. (This
is the one place the per-session worktree rule does not reach: the profile is not
in git, so it exists in exactly one directory.)

Check all of these before touching anything. Each has bitten at least once.

| Check | How | Wrong answer means |
|---|---|---|
| `SALT_ENC_KEY_B64` is set and 32 bytes | `echo -n "$SALT_ENC_KEY_B64" \| base64 -d \| wc -c` → `32` | tree build fails at the salt, after the settlement is persisted |
| `chain_configs` has your chain | `SELECT * FROM chain_configs WHERE chain_id='aptos-testnet'` | claims serve no asset decimals |
| Module is deployed | `aptos move view --function-id <mod>::escrow::recommended_claim_window` | nothing else works |
| Sponsor holds APT | `aptos account balance --profile grainlify-testnet` | funding transactions fail midway |
| Sponsor holds USDC ≥ leaf total | `primary_fungible_store::balance` | you discover it between funding and publishing, with a half-funded escrow |
| Migrations at 81+ | `SELECT version FROM schema_migrations` | `claim_leaves` does not exist |
| **`APTOS_TESTNET_RPC_URL` is set** | see *Environment variables* below | every live chain read returns `chain_endpoint_not_configured` |
| **`APTOS_SPONSOR_KEY` is set** | see *Environment variables* below | the API refuses to boot; claims cannot be sponsored |
| **Sponsor account is above the floor** | `curl .../v1/accounts/<sponsor>/balance/0x1::aptos_coin::AptosCoin` | sponsorship refuses for **everyone** |

### Environment variables

Both are set in the API's environment (Railway). Neither has a fallback, on
purpose: a default that works in development and misleads in production is the
failure mode that produced Grainlify-Backend#536.

#### `APTOS_TESTNET_RPC_URL`

```
https://api.testnet.aptoslabs.com
```

**Without the `/v1`, and this is the trap.** Aptos's own documentation lists the
endpoint as `https://api.testnet.aptoslabs.com/v1`, so the next person will paste
that — and the client appends its own `/v1`, producing `/v1/v1/...` and a 404 on
every chain read. Verified: the base returns `chain_id 2`; the documented form
with `/v1` returns 404 once the client appends.

`chain_configs.rpc_endpoint_ref` holds the *name* of this variable rather than
the URL, so no endpoint carrying an API key is ever written into a migration.
Unset, `/me/claims/:id/chain` returns **503 `chain_endpoint_not_configured`**
naming the variable, and deadline reminders cannot read a deadline.

#### `APTOS_SPONSOR_KEY`

The sponsor's Ed25519 private key. **Never in a migration, a log, an error body,
a response or a fixture** — enforced by surface tests in `internal/sponsor`.

Accepted formats, all equivalent, so paste whatever the tooling prints:

| form | example shape |
|---|---|
| bare hex | `abc1…` |
| `0x`-prefixed | `0xabc1…` |
| AIP-80 | `ed25519-priv-0xabc1…` |
| 32 bytes (seed) **or** 64 bytes (expanded) | either |

Anything else **refuses at boot**, naming the variable and never echoing the
value. It does not fall back to unsponsored claims.

**Confirm Railway is using the account you think it is** by checking the address,
which is public and safe to paste anywhere:

```
0xa1e01282cfd196ae6f1b236d136bc27bc23bcdd1614dc678d624a3c9a20d6e79
```

The service derives this address *from the key* rather than reading it from
config, so a mismatch here means the wrong key is set — not a mismatched pair.

#### Funding the sponsor

All figures from the measured milestone transaction: 14,920 gas units x 100
octas = **0.01492 APT per claim**. The first claim per recipient is the expensive
one, because it creates their token store — which is the case this exists for.

| | APT | meaning |
|---|---|---|
| one claim | 0.01492 | measured, not estimated |
| one 38-person settlement | 0.5670 | |
| **hard floor** | **1.7009** | below this the service **stops sponsoring and says so** |
| **minimum to run a payout at all** | **2.2678** | floor + one settlement |
| recommended | 5 | ~5 settlements of headroom above the floor |

The floor exists because an alarm needs a recipient and no channel reaches anyone
off-site. A floor depends on nobody reading anything: it bounds the drain and
surfaces to a contributor, who then tells us. **Failing visibly beats alerting
into a void.**

**The testnet faucet is web-only** — programmatic funding was removed, and
`aptos account fund-with-faucet` now replies *"you must visit
https://aptos.dev/network/faucet"*. To top up from an existing account instead:

```sh
cd Aptos-Contracts   # the CLI profile lives here
aptos account transfer --profile grainlify-testnet \
  --account 0xa1e01282cfd196ae6f1b236d136bc27bc23bcdd1614dc678d624a3c9a20d6e79 \
  --amount 500000000    # 5 APT, in octas
```

**Check the balance with the fungible-asset read, never the coin resource:**

```sh
curl -s https://api.testnet.aptoslabs.com/v1/accounts/<addr>/balance/0x1::aptos_coin::AptosCoin
```

`0x1::coin::CoinStore<AptosCoin>` returns **"Resource not found" for an account
holding APT perfectly well** — APT has migrated to a fungible-asset store. Using
it to verify a transfer means verifying with the query that reports zero for
funded accounts, and on the fail-closed sponsorship path a false zero refuses
every claim while the money sits there.

**Both queries, run against the sponsor account in the same minute, immediately
after the 5 APT transfer landed:**

```
$ curl .../v1/accounts/0xa1e0…6e79/balance/0x1::aptos_coin::AptosCoin
500000000                                          <- 5 APT, correct

$ curl .../v1/accounts/0xa1e0…6e79/resource/0x1::coin::CoinStore<0x1::aptos_coin::AptosCoin>
{"message":"Resource not found by Address(0xa1e0…)"}   <- same account, same minute
```

One account, one moment, two answers. The second is a **true answer to the
question asked** — that resource genuinely does not exist — and a false answer to
the question meant. Anybody verifying the transfer with it would have reported a
failed transfer that had in fact succeeded, and then gone looking at the transfer
rather than at the query.

**A 16- or 24-byte `SALT_ENC_KEY_B64` is accepted by AES and silently gives you
AES-128.** The length check refuses it; the check is the only thing between a
misconfigured key and a quietly weaker one.

---

## Phase 1 — collect addresses (weeks, not minutes)

**This is the only phase with human latency in it, and it is the long pole.**
Five testers is a day. Thirty-eight people is a week of waiting. Start it before
anything else, and do not start Phase 3 until it has converged.

1. Deploy the frontend prompt driven by `GET /me/payout-readiness`. Anyone with
   `state: "register_now"` sees it **on first load**, not behind a payouts tab.
2. Watch the count:

   ```sql
   SELECT count(*) FILTER (WHERE a.address IS NOT NULL) AS registered,
          count(*)                                       AS members
   FROM founding_members m
   LEFT JOIN contributor_addresses a
          ON a.user_id = m.user_id AND a.chain_id = 'aptos-testnet'
         AND a.superseded_at IS NULL;
   ```
3. Chase the gap **by name**, individually. A broadcast reaches the people who
   already acted.

**Why this ordering is not negotiable.** A person without a registered address at
tree build is excluded from that tree, permanently — a root cannot be edited.
Their share becomes residue. Everything about that is recoverable *before*
publication and nothing about it is after.

---

## Phase 2 — compute the settlement

```sh
go run ./cmd/payout dry-run --pool-usdc 3000     # writes nothing
go run ./cmd/payout persist --pool-usdc 3000     # IRREVERSIBLE
```

Persist prints the `settlement_id`, and **everything downstream is keyed to it.**
Persisting twice creates two settlements and the second will happily build its
own tree, so record the id and use it.

---

## Phase 3 — the payout dry run · **THE GATE**

```sh
go run ./cmd/payout report --settlement <id>
```

Writes nothing, needs no salt, and prints the report you read.
**This is the last point at which anything is reversible.**

Read all four sections, in this order:

1. **`EARNED BUT UNDELIVERABLE`** — read this *before* the leaves. These are
   people who earned a real amount and are about to be excluded permanently. If
   the list is longer than you expect, **stop and go back to Phase 1.**
2. **`LEAVES`** — spot-check three addresses against what the people told you.
3. **`RECONCILIATION`** — must end `= 0 OK`. If it says `DOES NOT RECONCILE`,
   stop; something is wrong upstream of everything here.
4. **`FUND EXACTLY THIS`** — the leaf total. Not the pool total. Write it down.

Record two values verbatim:

```
input digest:  <64 hex characters>
acknowledge undeliverable total, in minor units: <integer>
```

**Archive the whole report.** It is the record of what was approved, and after
publication it is the only account of who was excluded and why.

---

## Phase 4 — build the tree

```sh
go run ./cmd/payout build --settlement <id> \
  --digest <64 hex from the report> \
  --acknowledge-undeliverable <integer from the report>
```

It re-resolves from current data and refuses unless the input digest still
matches and the undeliverable total is restated exactly. Both come from the
report, and the acknowledgement appears only in the reconciliation — so it cannot
be produced without having read it.

**If it reports `ErrInputsChanged`, do not look for a way around it.** There is no
override, deliberately. Something moved between your reading and your building —
an address was registered, an amount corrected, or a **GitHub login renamed**,
which changes an identity hash that is about to go into a permanent root. Re-run
Phase 3 and read the report again. It costs two minutes.

On success this creates the salt, writes `claim_leaves` and `payout_event_roots`,
and returns the root. **Record the root and the leaf total.**

A second build for the same settlement is refused by a primary key.

---

## Phase 5 — fund the escrow with the leaf total

```sh
MOD=0x1b419fe2b8c2a694eda8398af4bb6f6980915f9e3ed856b3b0fb4f26597f22c9
USDC=0x69091fbab5f7d635ee7ac5098cf0c1efbe31d68fec0f2cd565e8d168daf52832
EVENT=founding-2026-q3                       # fresh, never reused
EVHEX=$(python3 -c "print('$EVENT'.encode().hex())")

ESCROW=$(aptos move view --profile grainlify-testnet \
  --function-id ${MOD}::escrow::escrow_address --args address:$MOD hex:$EVHEX \
  | python3 -c "import sys,json;print(json.load(sys.stdin)['Result'][0])")

aptos move run --profile grainlify-testnet \
  --function-id ${MOD}::escrow::initialise \
  --args hex:$EVHEX address:$USDC address:$MOD u64:63072000    # 24-month window

aptos move run --profile grainlify-testnet \
  --function-id ${MOD}::escrow::fund --args address:$ESCROW u64:<LEAF TOTAL>
```

Then **verify before going further**:

```sh
aptos move view --profile grainlify-testnet \
  --function-id ${MOD}::escrow::funded_total --args address:$ESCROW
```

It must equal the leaf total exactly.

> ### Fund the leaf total, never the pool total
>
> If you fund the pool, the excess sits in the escrow as `balance - root_total`,
> and that is what `sweep_residue` returns **with no timelock**. An excluded
> contributor's share would be swept without the claim window ever protecting it.
> `publish_root` enforces this, so the mistake is caught — but it is caught
> *after* you have moved the money.

Funding twice is not recoverable by funding less. If you overfund, your options
are to publish a root matching the larger figure or to leave the escrow unused.

---

## Phase 6 — publish the root

```sh
aptos move run --profile grainlify-testnet \
  --function-id ${MOD}::escrow::publish_root \
  --args address:$ESCROW hex:<ROOT, no 0x> u64:<LEAF TOTAL>
```

**`E_ROOT_TOTAL_NOT_FUNDED(0x10)` means the root and the funding disagree**, in
either direction. Demonstrated on testnet both ways: underfunded aborts,
overfunded aborts, exact match publishes. Nothing has moved when it aborts;
reconcile the two numbers and try again.

A root is write-once. There is no second attempt at a different root for this
escrow — you would need a fresh event id, and a fresh escrow.

Then record it:

```sh
go run ./cmd/payout publish --settlement <id> --escrow <escrow addr> --tx <hash>
```

**Until `published_tx` is set, `/me/claims` returns nothing** — the published-only
rule keys on it, so a contributor sees an empty list until this step runs.
`payout publish` refuses a settlement that already has one recorded.

---

## Phase 7 — verify from outside the system

Do not skip this because the transaction succeeded.

1. **The chain agrees with the database:**
   ```sh
   aptos move view --profile grainlify-testnet \
     --function-id ${MOD}::escrow::root --args address:$ESCROW
   ```
   Compare byte for byte with `payout_event_roots.root`.
2. **A proof verifies.** `payout.ClaimFor` for one address; it verifies against
   the published root internally and refuses if the stored leaves have drifted.
3. **A person can see it.** `GET /me/claims` as a real contributor, and confirm
   `address_status` reads `current` for somebody who has not changed address.
4. **The wallet harness, steps A and B**, against a real browser wallet.
5. `go run ./cmd/payout status --settlement <id>` — leaves, claims, publication
   and whether the salt is still live, in one place.

---

## Phase 8 — tell people, and watch

**Announce with the deadline in the message**, not only in the terms. The date is
when we *may* return unclaimed funds — not when the claim stops working — and
missing it is recoverable by asking. Never write "claim by X or lose it": it is
false, and it teaches anyone who misses the date that asking is pointless.

Watch:

```sql
SELECT (SELECT count(*) FROM claim_leaves WHERE settlement_id = '<id>')            AS leaves,
       (SELECT count(*) FROM chain_operations
         WHERE event_ref = '<id>' AND kind = 'claim' AND state = 'paid')           AS claimed;
```

**`claim_leaves` has no `claimed_at` column, deliberately.** Whether a leaf was
claimed is a fact about the chain, and `chain_operations` is where the reconciler
records it — `state = 'paid'` is written by nothing else. A second copy on the
leaf row would be a second source of truth about where money went, which is the
disagreement the reconciler exists to detect rather than create.

To ask the chain directly, `is_claimed(escrow_addr, leaf_hash)` is authoritative
and needs no database at all.

Also watch the reconciler's alerts. **Alerts stop the reconciler rather than being
logged past** — a disagreement between our records and the chain about where
money went is not something to observe repeatedly.

---

## Phase 9 — the long tail

- **As the deadline approaches**, chase unclaimed leaves by name. A low claim
  rate is a communication failure, not a contributor failure.
- **Somebody lost the wallet their leaf pays to.** Tell them it is recoverable.
  `extend_deadline` buys time; after the window, funds sweep to a destination we
  control and can be paid another way. **Never let them conclude it is hopeless
  — that is the actual loss.**
- **`sweep_unclaimed`** requires the window to have closed. Deliberate, and after
  chasing, not on a schedule.
- **Destroying the salt** is deliberate and never scheduled, with a reason
  recorded. Proofs keep working afterwards — `claim_leaves` is what makes the
  salt disposable — and it is irreversible in both directions: it forecloses bulk
  deanonymisation by a future leaker *and* our own ability to answer "was that
  leaf really mine?" for a contributor who asks.

---

## The five irreversible moments

Worth knowing before you begin, because three of them do not look irreversible.

1. **`founding.Persist`** — creates the settlement id everything else keys to.
2. **`payout.Build`** — freezes each person's address into their leaf.
3. **`publish_root`** — write-once. Exclusions become permanent here.
4. **`sweep_unclaimed`** — funds leave the escrow.
5. **Destroying the salt** — the login-to-leaf link is gone for everyone,
   including us.

Only the third is loud about it.
