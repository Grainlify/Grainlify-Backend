# Checks that report success without checking anything

Every entry here is a real case from this repository where a verification step
ran, printed a clean result, and established nothing. They share one shape: the
check measured a **proxy** for the thing it was supposed to measure, and the
proxy was empty, absent, or mismatched for a reason unrelated to correctness.

An error is loud. An empty result looks like a pass. That is what makes these
expensive: the check does not fail, it *agrees with you*.

**Entry 7 is a different family and is kept here deliberately.** Traps 1-6 are
checks that produced a confident wrong answer, and every one of them is
recoverable — you re-run the check properly and learn the truth. Trap 7 is the
opposite failure: nothing gave a wrong answer, because nothing was recorded at
all. The cause was knowable at the moment it happened and is now permanently
unrecoverable. A bad check wastes an afternoon; discarded evidence costs you the
incident.

## 1. Shell flags that silently return nothing

### `grep -l -n`

```sh
git grep -l -n "SomePattern" -- '*.go'     # prints nothing, exit 1
```

`-l` (list matching files) and `-n` (show line numbers) are mutually
exclusive. Rather than erroring, grep returns no output. This was read as "the
pattern does not appear anywhere", and produced a confident, wrong claim that
CORS configuration was missing.

### A glob the shell expands before grep sees it

```sh
grep -rn "installation_repositories" internal/ --include=*.go   # zsh: no matches found
```

In zsh, `--include=*.go` is expanded by the **shell**, not passed through. With
no file named `--include=*.go` in the working directory, zsh aborts the command
with `no matches found` before grep runs at all. The surrounding `|| echo "NOT
handled anywhere"` then printed a definitive-sounding conclusion about code
that does exist — `installation_repositories` is handled, in
`internal/ingest/github_webhook.go`.

Quote the flag (`--include='*.go'`), or use `git grep`, which takes pathspecs
directly:

```sh
git grep -n "installation_repositories" -- '*.go'
```

**The general rule:** a search that returns nothing has two explanations —
the thing is absent, or the search did not run. Distinguish them before
concluding, with a control that must match:

```sh
git grep -n "func " -- '*.go' | head -1    # if this is empty, the search is broken
```

### A fixture arranged so the bug cannot manifest

`/projects/:id/issues/public` returned closed issues: no state predicate at
all, and an ordering that promoted them, because it sorts by
`updated_at_github DESC` and **closing an issue bumps that timestamp**. 42% of
what it returned was closed.

The obvious test seeds an open issue and a closed one and asserts only the open
one comes back. Written the obvious way it proves nothing:

```go
seed(1, "open",   "...", "2029-01-01")   // newest
seed(2, "closed", "...", "2020-01-01")   // oldest
```

With `LIMIT 50` and that ordering, the closed row is last. On a build with the
filter deleted the query still returns the open row first, the assertion still
passes, and the test has verified that a list containing one open issue
contains an open issue.

The fixture has to be arranged so the bug, if present, **must** show up:

```go
seed(1, "closed", "...", "2030-01-01")   // newest - what closing actually does
seed(2, "open",   "...", "2029-01-01")
```

Now the closed row is the first thing an unfiltered query returns, so nothing
but the predicate can produce a pass. And the test also asserts the open issue
IS returned, so "filter everything" fails too - one assertion for each
direction the fix can be wrong in.

**The general rule:** a fixture is part of the check, not scenery. After
writing one, ask which arrangement the bug survives, and use that one. If the
data is arranged so the defect cannot appear, the test documents the intent and
verifies nothing - the same failure as §1, arriving through the setup instead
of the assertion.

### A 200 from a single-page app is not evidence a route exists

```sh
curl -s -o /dev/null -w "%{http_code}" https://grainlify.com/support   # 200
```

That 200 meant nothing. `vercel.json` rewrites `/(.*)` to `/index.html`, so
the CDN answers **every** path with the app shell — `/support`, `/nonsense`,
`/a/b/c` — all 200, all identical bytes. The route did not exist. `SupportPage`
rendered only inside `Dashboard`, which is wrapped in `ProtectedRoute`, so an
anonymous visitor was redirected to sign-in.

It was reported upward as "/support already works anonymously", and a plan was
approved on it that would have replaced a working anonymous reporting path with
a link to a page that bounced exactly the people who cannot sign in — the six of
ten support reports that arrive anonymously from the landing page and `/signin`.

This is §1's shape at a different layer: **a check whose success is
indistinguishable from its failure.** A missing route and a present one return
the same status, the same content type, and the same length.

**What actually answers the question:**

1. Read the route table. `grep "<Route" App.tsx` is faster than any request and
   is the only source of truth for a client-rendered app.
2. Check the guard, not just the path. A route inside `ProtectedRoute` exists
   and is still unreachable for the people the feature is for.
3. If you must probe, probe the rendered DOM, not the status:

```js
await page.goto(url)
location.pathname          // did it redirect?
document.body.innerText    // did it render the thing, or the shell?
```

The control that would have caught it in one line: request a path that
certainly does not exist. If `/definitely-not-a-route` also returns 200, the
status code is telling you about the server's rewrite rule and nothing about
your route.

### A fixture that constructs a state the system should refuse

The sibling of the case above, and the more dangerous one: there the fixture
made the bug invisible, here it encoded the bug as expected behaviour.

Gating founding-pool entry on an approved social-follow proof broke a
settlement test. Its setup did this:

```go
// Same shares, but never followed -> ineligible, excluded entirely.
b := newUser(t, d)
AssignWave(ctx, d.Pool, b, cfg)   // cfg has the gate ON
```

It assigned a wave to somebody with no approved proof - which is exactly what
the gate now refuses, and exactly the defect being fixed. The test was not
wrong about its own subject: settlement must still exclude an ineligible
member from the divisor, and that property is real. It was wrong about how
that member comes to exist, and in being wrong it asserted that the system
permits the thing it should not.

The instinct when a change breaks a test is to ask whether the change is
wrong. Here the change was right, the assertion was right, and **the fixture
was the only wrong part** - which is the hardest of the three to see, because
setup code reads as scaffolding rather than as a claim.

The fix is to construct the state the way reality did:

```go
ungated := defaults()
ungated["founding_require_social_follow"] = "false"
AssignWave(ctx, d.Pool, b, ungated)   // assigned before the gate existed
// ...then settle with the gate ON
```

Not a workaround. The seventeen members who hold a position without an
approved proof were assigned before the gate existed, and AssignWave's
short-circuit deliberately never re-examines an existing member - so that is
the real history, and now the fixture tells it.

**The check:** when a fix breaks a test, ask which of the three parts is
wrong - the change, the assertion, or the setup. If the setup constructs a
state the system is now supposed to prevent, the test was asserting the bug
was permitted, and rewriting the setup is the fix rather than an accommodation.

### A check that ran, went green, and measured somewhere adjacent

CI smoke-tested production after every deploy — health, then the sign-in
redirect — against `api.grainlify.0xo.in`. That is the previous production
hostname, and it reaches the service **directly, bypassing Cloudflare**.

So the check ran, passed, and verified a path no real user takes. Anything
living in front of the WAF — a Cloudflare rule, a firewall change, a
certificate on the real hostname, the CDN itself — could be completely broken
while this went green after every single deploy. It was not measuring
production; it was measuring a service that production happens to share.

Same shape as a suite that reports green from a worktree where the tests that
matter are skipped: the check is real, the result is real, and the subject is
not the one you meant.

**The check:** for anything asserting "production works", ask whether it
traverses the same path a user does — same hostname, same CDN, same guards. A
smoke test that skips a layer is a smoke test for a system nobody uses.

### A blank where a value belongs reads as a fault, not a fact

Three occurrences, so it is a shape rather than an incident:

| surface | blank would have meant | what it says instead |
|---|---|---|
| pool position, failed load | "you have no position" | "couldn't load this — it doesn't affect your position" |
| KYC queue, reset row | (looks like a broken cell) | "no live session" |
| maintainer page, wrong view | "you have no access" | "you're viewing as a contributor" |

In each case the honest empty state and the failure state render identically,
and the reader cannot tell which they are looking at - so they assume the one
that is about them. **An absent value needs to say which kind of absence it
is**, and the wording differs enough per case that it cannot be solved once
in a component.

### The same principle, applied to placement

Some rules are wrong in a way no output reveals. The 300-approval cap is
checked inside the decision transaction, after a `pg_advisory_xact_lock`. Move
that check into the handler, before the transaction, and it returns the right
answer for every request that arrives alone - which is every request a test
makes. It fails only when two admins approve at the same moment, and then it
fails silently, by admitting more people than the cap allows.

So the test asserts **where the check is**, by reading the source: that the
lock is taken, that the count is read *after* it, and that the bulk path has no
second copy of the rule. Asserting the outcome would have passed on the broken
placement every time.

**When a check can be in the wrong place and still return the right answer,
test the placement.**

### The easiest place for this to hide is a test named after a guard

`kycReasonForWarning` excludes the `ip_analysis` feature from reason mapping so
a fraud signal is never disclosed to the contributor. It has a test called
`TestKYCReasonForWarning_IPAnalysisNeverMapsToAReason`, which listed the real
ip_analysis risk codes and asserted each returned nothing.

Deleting the guard entirely did not fail it. None of those risk names appear in
the mapping switch anyway, so they returned nothing for a reason that had
nothing to do with the guard. The test asserted something true, about code that
was not the code it was named after.

This was caught by mutation testing minutes after the guard was written, with
the file open — which is the point worth recording. The lesson is not "write
better tests". It is that **a test named after a guard is where this hides
best, because the name does the convincing.** Nobody re-reads the body of
`TestXNeverHappens` to check that X could have happened; the name has already
answered the question, and a reviewer who trusts it inherits the same blind
spot as the author.

The fix was to include, in the same table, inputs that **do** map under other
features — `SCREEN_CAPTURE_DETECTED`, `DATA_INCONSISTENT`,
`LOW_FACE_MATCH_SIMILARITY`. Those return a reason unless the guard stops them,
so the guard becomes the only thing that can produce a pass.

**The check:** for a test asserting that something never happens, ask what
makes it happen, and confirm that input is in the test. If every input in the
table would pass with the guard deleted, the test is documentation.

## 2. A regex guard that matched one gate of three

`internal/ranking/fork_guard_test.go` asserts that every verified-project gate
also excludes forks. Its first version used:

```go
regexp.MustCompile(`(?m)^\s*(?:AND\s+)?(p2?)\.status = 'verified'`)
```

Two of the three gates are introduced by `WHERE`, not `AND`, so the pattern
matched exactly one — and that one happened to be correct. The test passed
while the exclusion was missing from the other two. It was caught only because
a mutation deliberately removed the exclusion from the org board and **nothing
failed**.

The fix was to accept both introducers *and* to assert the number of gates
found:

```go
const wantGates = 3
if len(matches) != wantGates { t.Fatalf(...) }
```

**The general rule:** a structural check must assert **how much** it checked,
not only that what it checked was fine. Otherwise a restructure quietly reduces
its coverage to zero and it keeps passing.

## 3. A test that evaluated its own constant

`TestSupportDelivered_SQLAndGoAgree` compared a Go predicate against itself
rather than against the migration it was supposed to match. Breaking the SQL in
the migration did not fail it. The replacement reads the migration file from
disk.

## 4. A mutation that never compiled

While mutation-testing the support sink, two patches removed code that left a
variable unused. The build failed, the test command returned non-zero, and the
harness recorded "caught". A build failure proves nothing about a test.

The harness now asserts each patch **applied** (the pattern matched exactly
once) **and compiled** before trusting the result. Note that `go vet` is the
wrong gate for this: it rejects unreachable code, so a valid mutation reads as
a compile failure. Use `go build`.

### The same trap in a typed frontend

A TypeScript mutation that fails `tsc` is the identical failure wearing
different clothes, and it is easier to walk into because the mutations are
smaller. Four of them, over two sessions:

```tsx
if (res.notified) {          →  if (true) {              // unused variable
setReasonCode(a === 1 ? x : '')  →  setReasonCode(a[0] ?? '')  // fine
Body: strings.TrimSpace(raw) →  Body: ""                 // unused import
```

Each one changes behaviour obviously. Each one failed to compile for a reason
having nothing to do with the test. And because the harness reported non-zero,
each was recorded as **caught** - a test proved nothing and got credit for it.

**The tell is identical in both languages:** an obviously behaviour-changing
edit appearing to be caught *instantly*, and the fix is the same - assert the
patch compiles before running the test, and when it does not, write a variant
that does rather than accepting the result. `if (true)` leaves a variable
unused; `if (x || !x)` does not. Zeroing a field can orphan an import;
`strings.TrimSpace(string(raw)[:0])` does not.

Three of those four were only conclusive on a second attempt. Retrying is
cheap; a mutation recorded as caught when it never ran is a test you now trust
for no reason.

## 5. e2e checks on pages that never rendered

`e2e/widget-overlap.spec.ts` (frontend) reported 26 of 26 passing while 25 of
those pages had white-screened, because a route mock answered with the wrong
shape and React unmounted the tree. A page with no elements cannot overlap
anything.

It now asserts a minimum interactive-element count per page before checking for
collisions.

**The general rule for all of the above:** when a check passes, ask what it
would take for it to fail. If you cannot answer, break the thing on purpose and
confirm it goes red.

## 6. A test that passed on a build where clicking did nothing

`DiscoverPage` owned a second issue-detail overlay behind its own `?dIssue=`
parameter. Fixing it meant two separate changes:

1. **removal** — the page stops rendering its own overlay
2. **rewiring** — the click reports upward so the shared overlay opens

The first test asserted only removal: *"the page no longer renders an issue
detail view."* It passed. It also passed when the click handler was mutated
back to writing `?dIssue=` — a build where **clicking an issue does nothing at
all**, which is worse than the inconsistency being fixed.

Only the second property is one a user can feel. Nobody notices that a
component was deleted; they notice that a click stopped working.

**The general rule:** when a fix moves behaviour from one place to another,
removal and rewiring are two properties and need two assertions. A test for the
old thing being gone will pass on a build where nothing replaced it.

### The same shape a second time, in the same change

Removing the `?dIssue=` read also silently broke every link somebody had
already shared: the parameter was ignored, so the page opened nothing. Nothing
server-side ever generated those URLs — no notification, email or Telegram
message — which meant they existed only in chats, where they could not be found
and fixed.

Caught by asking "who else holds one of these?", not by a test. The fix
translates the legacy parameter into the shared selection and strips it, and
there is now an assertion for that too.

## 7. Evidence discarded before it was written down

Signup returned `{"error":"github_user_fetch_failed"}` in production for
roughly seven minutes of active failures spread over half an hour, reported
twice by contributors. Diagnosing it took an hour and produced no proof,
because the one fact that would have answered it in a second had been read and
thrown away at the moment it arrived.

The callback made two GitHub calls and had two failure exits:

```go
tr, err := github.ExchangeCode(...)
if err != nil {
    return c.Status(401).JSON(fiber.Map{"error": "token_exchange_failed"})   // logged nothing
}
u, err := gh.GetUser(c.Context(), tr.AccessToken)
if err != nil {
    return c.Status(401).JSON(fiber.Map{"error": "github_user_fetch_failed"}) // logged nothing
}
```

and underneath, the client reduced GitHub's entire answer to a number:

```go
return User{}, fmt.Errorf("github /user failed: status %d", resp.StatusCode)
```

Three separate losses, each sufficient on its own:

1. **Neither exit logged.** Between "using redirect_uri from state" and the
   final redirect there was not one line. In the logs a failed callback was
   indistinguishable from a *different* failed callback with a different cause.
2. **The body was dropped.** GitHub says what went wrong in plain text —
   `Bad credentials`, `You have exceeded a secondary rate limit`.
3. **The rate-limit headers were dropped.** `X-RateLimit-Remaining` is the one
   field that separates "we are out of budget" from every other 403.

`status 403` on its own is four incidents wearing one name — a revoked token,
the primary rate limit, a secondary per-IP limit, and GitHub degraded. They
need opposite responses: rotate a credential, back off, slow the caller down,
wait. The bare code distinguishes none of them, and GitHub had already told us
which.

What the investigation could do was *eliminate*: not a deploy (nothing had
touched the path in fourteen commits), not the client secret (three exchanges
succeeded in the same window), not the scopes (granted matched requested), not
first-time user creation (that returns 500, and every failure logged 401), not
a timeout (1.2-1.5s against a 10s limit). That narrowed it to a live GitHub
incident, corroborated by 72 unrelated 403s from the same host in two minutes.

**Strong, and still circumstantial.** The actual status code for those four
failures does not exist anywhere. It was in a variable named `err` and was
never written down.

### A real instance: deleting the record did not retract the copies

Every example above is about evidence that was never written down. This one is
the inverse and it actually happened, which is why it is here: the evidence was
written down, deleted, and reported as cleaned up — while the copies that
mattered had already left.

Settling the proxy question meant sending probe requests with spoofed
forwarding headers. The endpoint chosen was `POST /support-requests`, because
it was the one already being worked on. Three probes went through. The rows
were then deleted from `support_requests` and the cleanup reported.

The rows were never the exposure. That endpoint fans out to Telegram and
Discord *before* anything could be undone, so all three probes are sitting in a
human being's Telegram, permanently, and `DELETE` reached none of them. The
affected population was not in the database.

Nothing harmful was in them. The habit is the problem: "I removed the rows"
was reported as complete cleanup, and it was cleanup of the one copy that did
not matter.

**The check, before probing anything:** ask what the endpoint does *besides*
persist. If it notifies, posts, emails, or fans out, the probe is not
reversible and deleting afterwards is theatre. Either probe somewhere inert —
`/health` and `/version` would have answered this question exactly as well — or
decide to accept the residue in advance, so it is a decision rather than a
discovery.

**The guard.** At every external boundary, log the upstream status, the bounded
body, and the rate-limit budget *before* returning our own error name. One
error name per upstream call, and never a name that could cover two.

Bounded matters: an error path must not be a way for a remote host to write an
unbounded string into our logs. 1KB is plenty for a message.

There is one exception and it is worth stating, because the obvious fix
introduces a worse bug. **A boundary whose success response contains a
credential must never have a formatter that prints its body.** GitHub's token
endpoint returns the access token on success and an error payload on failure —
both with HTTP 200 — so `ExchangeCode` deliberately does *not* use the shared
`APIError` type and surfaces only the parsed `error` / `error_description`
fields. `TestExchangeCode_ErrorNeverContainsTheToken` pins that, because "log
the body" applied uniformly would have leaked every user's token into the logs.

### The related smell: one error name covering several causes

Worth checking for separately, because it survives even when logging is good.
The name is a promise about what failed, and a name broader than the call it
wraps sends the next person to the wrong place. During this incident the
question "does `github_user_fetch_failed` also cover the `/user/emails` call?"
was reasonable, load-bearing, and took real work to answer — it did not, but it
easily could have, and nothing in the code said so.

The check is mechanical: for each error name, list the calls that can produce
it. If the list has more than one entry, split the name.

### A local commit is not a pushed commit

Same family as §7, one step earlier, and it hides from a different set of
checks.

A docs change was committed at 14:20Z and never pushed. It had a real commit
hash, a real message, and a clean `git status` — the tree looked finished. It
was invisible to **every check that reads the remote**: it was not in the merge
list, not in a PR, not in any deploy, and not on the site. Six hours later an
audit of "what merged and what is live" found nothing wrong, because the audit
asked the remote and the remote had never heard of it.

The failure modes stack, and each one is invisible to the check below it:

| state | looks fine to |
|---|---|
| committed, not pushed | `git status`, `git log`, the local tree |
| pushed, not merged | the branch, CI on the branch |
| merged, not deployed | the merge list, `git log origin/main` |
| deployed, wrong artifact | the deploy log |

Only the last is caught by asking the running service what commit it is —
which is what `scripts/verify-deployed.mjs` does, and why it exists. The rows
above it need their own check.

**The guard:** an audit of "what shipped" must start from the working tree, not
from the remote. Two commands, and the first is the one nobody runs:

```sh
git log --branches --not --remotes --oneline   # committed here, pushed nowhere
git status --porcelain                         # not even committed
```

Run them in every repo, not just the one being worked on. The commit that went
missing was in the docs repo during a backend day, which is exactly how it
stayed missing: nobody was looking at that tree.

**And a corollary that bit on the same day.** A shared working tree makes
`git status` ambiguous rather than merely incomplete: a second session had
deleted a whole package in the same checkout, so a full test run reported green
for a tree that was neither `origin/main` nor `origin/main` plus the change
under test. The result was not wrong so much as about something else. Verify in
an isolated worktree by default — `git worktree add --detach <path> <branch>` —
so the thing being tested is exactly the thing being shipped.

## 8. An intermittent failure invites you to blame the environment

The environment is usually innocent.

Three new tests went green twice and red on the third run, with no code change
between them. The first explanation reached for was the shared test database -
a second session was working in the same checkout that day, the suite touches
a database several things use, and "flaky, probably the shared DB" is a
complete-sounding story that requires nothing further of you.

It was wrong. The tests were the problem, and specifically their fixtures:

```go
const reason = "subject-deletion audit survival probe"   // fixed, every run
// ...
`SELECT ... FROM kyc_reset_audit WHERE reason = $1`, reason
```

Each run inserted a row with that same reason and left it behind. By the third
run four rows shared it, `QueryRow` returned an arbitrary one - typically an
earlier run's, already nulled by an earlier deletion - and the assertions ran
against a row the test had never written. The count made it deterministic in
hindsight and random-looking at the time.

**The tell:** "flaky" is a description, not a diagnosis, and it is the only bug
class whose most popular explanation lives outside the code. Nobody says "it's
probably the environment" about a test that fails every time.

**The check, in order, before the word flaky is used at all:**

1. Run it N times in a row and count. Truly external noise is rarely 1-in-3.
2. Ask what the test leaves behind. Anything written with a fixed identifier -
   a constant string, a hardcoded id, a well-known email - accumulates, and a
   read keyed on that identifier drifts onto a stranger's row.
3. Ask what else writes to the same rows.
4. Only then consider the environment.

Keying on an identifier the test itself created (`INSERT ... RETURNING id`) is
immune to all of it, and `t.Cleanup` keeps the table from growing whatever the
outcome. Both were applied, and the five consecutive runs that followed are the
evidence - one green run would have proved nothing, since one green run is what
started this.

### The related habit worth keeping

These same tests were written to execute a real `DELETE` rather than to read
`information_schema`. That mattered: the constraint under test was
self-contradictory - `NOT NULL` together with `ON DELETE SET NULL` - and a
catalogue query reported both facts happily, each looking correct on its own.
Only running the delete showed which one won.

**A schema check that reads metadata tests what was declared. Executing the
operation tests what happens.** When those can differ, the second is the one
that matters.
