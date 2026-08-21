# Checks that report success without checking anything

Every entry here is a real case from this repository where a verification step
ran, printed a clean result, and established nothing. They share one shape: the
check measured a **proxy** for the thing it was supposed to measure, and the
proxy was empty, absent, or mismatched for a reason unrelated to correctness.

An error is loud. An empty result looks like a pass. That is what makes these
expensive: the check does not fail, it *agrees with you*.

**The headings here carry no numbers, deliberately. Do not add them back.** Every
entry is appended, and two branches appending at once both reach for the next
ordinal — so the numbers collide in the merge while the content does not. That
happened twice: once when a branch added an eighth entry and four others had to
be renumbered, and again when both sides independently wrote a ninth and a tenth.
The second time the merge conflict was purely about the numbers. A descriptive
heading is stable, survives insertion anywhere, and can be linked to by name; an
ordinal is a merge conflict waiting for a second author. Cross-references cite
the heading text for the same reason.

## The through-line

Read this before the entries, because it is what they have in common and it
predicts where the next one will be.

**Every expensive mistake in this file happened in a position nothing checked.
Every cheap one happened in a position something did.**

The comparison is not hypothetical. In two days of one workstream:

| Mistake | Checked by | Cost |
|---|---|---|
| An invalid hex digit in a Move address literal (three times) | The compiler | A minute each. Non-events. |
| A mutation patch that changed the file but not the behaviour | Nothing | Reported as a survivor, read as a coverage gap, wrong |
| A test that skipped while the suite said `ok` | Nothing | Ran green in CI and in every worktree, checking two fewer things than anyone believed |
| A harness recording build errors as kills | Nothing | Four consecutive false greens, on the exact gap under investigation |
| Tests quietly encoding overfunding as supported | Nothing | Passed correctly for months; surfaced only when a rule tightened |

The same carelessness produced all of them. What differed was whether it landed
somewhere a machine would object.

**So the lesson is not "be more careful."** Carefulness is what was already being
applied, and it distributes evenly across positions that check and positions that
do not. The lesson is:

> **Move the thing you have to remember into a position that refuses.**

Three examples from the same workstream, each replacing a rule with a mechanism:

- *"Fund the leaf total, never the pool total"* was a runbook line. It became
  `assert!(total == funded_total)` — and an operator reading the runbook can no
  longer get it wrong, because the chain declines.
- *"Never retry a claim row"* was going to be a branch in the reconciler. It
  became a database constraint that makes such a row unstorable, plus a type that
  cannot be constructed from one. The reconciler's query cannot return the
  dangerous row, so its author does not need to know the danger exists.
- *"Never log the salt"* was a doc comment. It became a type whose `String()` and
  `MarshalJSON` return a redaction, so the accidental log line is harmless.

### A rule drawn too narrowly is a rule you get to break again

Worth its own note, because the failure was in the *generalisation* rather than in
the fix.

A browser tool passed a hex string where Move bytes were required. Fixed. Then the
same mistake, one field along. Fixed, and this time a rule was drawn: **every
`vector<u8>` argument comes from a helper, and no helper returns a string.** That
held — and the defect appeared a fourth time, in a value handed to a browser
extension:

```
TypeError: this.fee_payer_address.serialize is not a function
```

The extension stored the hex string and later tried to serialise it. Not a
`vector<u8>`, so the rule did not cover it, and the rule was not wrong — it was
*narrower than the thing it was drawn from*. The general shape is:

> **A value crossing a typed boundary must be constructed, never spelled.**

An address, a digest, a signature, an amount — anywhere a string is accepted for
something that is not text, the string will eventually be the wrong one and the
error will surface far from the call site. Three of these four surfaced as a
confident wrong answer somewhere else: a Move abort about a digest length, an
escrow that did not exist, an extension's internal `TypeError`.

**The check:** when a fix prompts a rule, ask whether the rule is as general as
the mistake. If the mistake was "a string where a typed value belongs" and the rule
says "vector<u8>", the rule will hold and the bug will return.

When you catch yourself writing a rule — in a runbook, a comment, a review note —
ask what would have to be true for the rule to be unnecessary. Sometimes nothing
reasonable. Often it is one constraint, one type, or one assertion, and then the
rule becomes an explanation of a mechanism rather than the mechanism itself.

### The positive instance, because everything else here is a failure

Every numbered entry below is a check that failed to refuse. This is the same
mechanism working, recorded so the file contains at least one example of what
success looks like.

The first sponsored claim, submitted against a deployed contract, aborted:

```
Move abort in ...::escrow: E_BAD_DIGEST_LENGTH(0xa)
```

The identity hash had been passed to the SDK as the string `"0x2222…"` rather
than as 32 bytes. BCS encoded it as a string, and the module received something
the wrong length.

**What that assertion bought.** Without it, `leaf_hash` would have hashed the
wrong bytes perfectly happily. The leaf would have been well-formed and wrong, the
proof would not have verified, and the visible symptom would have been a Merkle
mismatch — sending whoever debugged it into the tree construction, which is the
most-tested code in this workstream and would not have been at fault.

On a real event it is worse than a wasted afternoon. A root built from
string-encoded identity hashes publishes without complaint, commits permanently,
and produces leaves **nobody can ever claim**. The failure would surface at the
first genuine claim, against a root that cannot be corrected.

Instead: a named abort, on the first attempt, pointing at the exact argument.

`assert!(vector::length(&identity_hash) == DIGEST_LEN, ...)` is the kind of line
that reads as defensive noise in review — an obvious invariant, checked at a
boundary the caller controls. It is worth remembering what it actually did the
first time real bytes went through, next time one of these looks removable.

**The corollary, and the reason this file exists:** where a mechanism is not
possible, the check that stands in for it must be verified to actually check. That
is what every entry below is about.

**Entry 12 names a family rather than an incident.** Several of the entries
before it are instances of one shape - a test that passes for a reason its author
did not choose - and it is worth recognising the third occurrence as a repeat
rather than filing it as something new. The individual entries stay where they
are, because each also teaches its own mechanism.

**Entry 7 is a different family and is kept here deliberately.** Traps 1-6, 8 and 9
are checks that produced a confident wrong answer, and every one of them is
recoverable — you re-run the check properly and learn the truth. Trap 7 is the
opposite failure: nothing gave a wrong answer, because nothing was recorded at
all. The cause was knowable at the moment it happened and is now permanently
unrecoverable. A bad check wastes an afternoon; discarded evidence costs you the
incident.

**Entry 7 is a different family and is kept here deliberately.** Traps 1-6 are
checks that produced a confident wrong answer, and every one of them is
recoverable — you re-run the check properly and learn the truth. Trap 7 is the
opposite failure: nothing gave a wrong answer, because nothing was recorded at
all. The cause was knowable at the moment it happened and is now permanently
unrecoverable. A bad check wastes an afternoon; discarded evidence costs you the
incident.

## Shell flags that silently return nothing

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
verifies nothing - the same failure as **Shell flags that silently return nothing**, arriving through the setup instead
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

This is the shape of **Shell flags that silently return nothing** at a different layer: **a check whose success is
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

### Accumulated state producing a pass, not a failure

The mirror of the flaky test above, and harder to notice because the symptom is
green.

`TestSocialFollow_AdminListCarriesNoScreenshots` **passed in the full suite and
failed 6 times out of 6 when run alone** — and failed on `main` too, so it had
nothing to do with the change being tested. The review queue pages at 50, and
550 pending submissions had accumulated in the shared test database across
months of runs, so a freshly created submission no longer appeared on the first
page the assertion looked at.

Both directions come from the same cause — a shared database nothing truncates
— but they behave oppositely, and only one of them is loud:

| | symptom | how it is usually explained away |
|---|---|---|
| earlier case | passes twice, fails the third time | "flaky, probably the environment" |
| this case | **passes in CI, fails alone** | never noticed, because CI is green |

The second is worse. Nobody investigates a passing test, and running one test in
isolation is what you do when you are already suspicious — so the failure is
only ever seen by somebody who went looking for something else.

**The check:** a test that passes in the suite should pass alone. `go test -run
TestTheOneYouCareAbout` is one command, and disagreeing with the full run means
the test depends on state some other test leaves behind. Whichever way that
disagreement points, the test is not measuring what it claims.

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

## A regex guard that matched one gate of three

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

## A test that evaluated its own constant

`TestSupportDelivered_SQLAndGoAgree` compared a Go predicate against itself
rather than against the migration it was supposed to match. Breaking the SQL in
the migration did not fail it. The replacement reads the migration file from
disk.

## A mutation that never compiled

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

### The escalation: a restore that deleted the baseline

The case above miscounted one patch. The same mistake in the restore step
miscounts **every patch after the first**, and reports a wall of green.

Mutation-testing the Merkle tree construction, the harness restored each
mutated file with `git checkout internal/chain/merkle.go`. That file had
uncommitted changes — the refactor the new vector tests depend on — so the
restore reverted it to `HEAD` and discarded the refactor. From the second
mutation onward the package did not compile, and the run reported:

```
M5b nodePrefix 0x01 -> 0x09            -> FAIL     recorded as killed
M7  leaf sort ascending -> descending  -> FAIL     recorded as killed
M4  duplicate odd node, not promote    -> FAIL     recorded as killed
M8  remove the leaf sort entirely      -> FAIL     recorded as killed
```

Four kills, none of them real; the mutations never ran. And the wall is what
makes this worse than a single miscount — four consecutive greens are far more
convincing than one, and these four were reporting that the exact gap under
investigation had been closed.

Caught by noticing that changing a `const` from `0x01` to `0x09` cannot
plausibly break a build. The tell was in the output the whole time: `[build
failed]` where the other lines said `--- FAIL: TestName`.

Two guards. The second is the general one and reaches well past mutation
testing:

- **The harness must distinguish "the test failed" from "it did not compile"**
  and must never count the second as a kill. Match the build-failure string
  explicitly; do not infer from an exit code. Relatedly, the exit status of a
  pipeline is the *last* command's — `go test ./... | grep -v '^ok'` exits 0
  whenever grep does, whatever the tests did.
- **`git checkout <file>` is not an undo.** It restores from the index or
  `HEAD`, not from the state the file held a moment ago. While writing the
  tests you are about to mutate — which is the normal condition — the tree has
  uncommitted work in it and `git checkout` is a destructive operation. Restore
  from a copy instead:

```sh
cp internal/chain/merkle.go "$SCRATCH/merkle.go.baseline"
# ... mutate, run, read ...
cp "$SCRATCH/merkle.go.baseline" internal/chain/merkle.go
```

**Re-establish the baseline before the first mutation and after the last.** A
harness that never checks its own starting point cannot tell a mutation that
survived from a tree that was already broken.

### The third variant: a patch that changed the file but not the behaviour

Two entries above cover a build error recorded as a kill, and a broken restore
recorded as four. This is the mirror image, and it is the one that looks like a
finding.

Mutation-testing the sweep, a patch meant to check that sweeping does not clear
the claimed markers was written as:

```move
transfer_out(escrow, amount);
let _ = &escrow.claimed;      // intent: clear the table
```

It applied cleanly, compiled, and the suite passed — recorded as a survivor, and
read as "the evidence-intact property is untested". It is not: the patch clears
nothing, so the run measured nothing. Rewritten three ways that genuinely destroy
the evidence — zeroing `root_total`, unsetting `root`, zeroing `claimed_total` —
all three die against the same test that had appeared not to cover them.

Worth noting what nearly hid it: `sweep_unclaimed` takes an immutable borrow, so
the type system already forbids touching the markers. Every real version of that
mutation had to change `borrow_global` to `borrow_global_mut` first. A structural
guarantee and a tested one look identical from the outside, and only one of them
was being exercised.

**A no-op patch reporting a survivor and a build error reporting a kill are the
same bug wearing different clothes:** in both cases the harness reported on
something other than what it claimed to measure. The tell was identical all three
times — *a change that should obviously have had an effect appeared not to*.

**The guard is now a script, not an instinct:** `scripts/mutate.sh`. It refuses to
report until it has established that it works.

- The baseline must compile and pass **before the first mutation and after the
  last**. A failing baseline afterwards means a restore leaked and every result is
  void.
- Every run takes a **control mutation** the caller is certain is lethal. If the
  control survives, the harness cannot detect what it claims to, so the run aborts
  and reports nothing — the same shape as the control grep that must match in **Shell flags that silently return nothing**.
- Each patch must **measurably change the file**, by hash. A pattern that matched
  nothing is reported as `NOT APPLIED`, never as a survivor.
- A survivor is reported as **`SURVIVED (unverified)`**, because a survivor is the
  one outcome indistinguishable from a broken patch. Promoting it to a finding
  requires naming the observable it should have altered.
- Restores come from a copy, never from `git checkout`.

It takes the test command as an opaque string, so the same harness covers Go and
Move; it interprets only the exit status, which is why it never string-matches on
error text. That last point is the fix for the second variant: `error[E11001]` is
what a Move *test failure* prints.

## e2e checks on pages that never rendered

`e2e/widget-overlap.spec.ts` (frontend) reported 26 of 26 passing while 25 of
those pages had white-screened, because a route mock answered with the wrong
shape and React unmounted the tree. A page with no elements cannot overlap
anything.

It now asserts a minimum interactive-element count per page before checking for
collisions.

**The general rule for all of the above:** when a check passes, ask what it
would take for it to fail. If you cannot answer, break the thing on purpose and
confirm it goes red.

## A test that passed on a build where clicking did nothing

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

## Evidence discarded before it was written down

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

### The reference was in a deployed build, not the source

Twice in one day, which is what makes it the general rule rather than an
incident.

Removing the old API hostname took sign-in down. The audit beforehand had
searched three repositories for the hostname and reported the complete
dependency list: backend CI, frontend CI, a settings doc, the CORS allowlist.
Every one of those was real. The one that mattered was not in any repository.

`VITE_API_BASE_URL` is a Vite variable, set in Vercel, **inlined into the
bundle at build time**. The deployed frontend had `https://api.grainlify.0xo.in`
compiled into it as a string literal. No grep of any checkout would ever have
found it, because the value does not live in a checkout - it lives in a build
environment and then inside an artifact.

The same shape had already appeared hours earlier: a `200` from an SPA proved
a route existed, when the CDN was rewriting every path to the shell. Both are
the same mistake - **reading the source and reporting on the running system.**

**The guard, which is one command in both cases:** ask what the RUNNING BUILD
contains, not only what the source says.

```sh
# what host does the deployed bundle actually talk to?
curl -s https://grainlify.com/ | grep -o '/assets/index-[^"]*\.js' \
  | xargs -I{} curl -s "https://grainlify.com{}" | grep -o 'https://api\.[a-z.]*'

# does the route exist, or is the CDN answering everything?
curl -s -o /dev/null -w '%{http_code}' https://example.com/definitely-not-a-route
```

Applied after the outage it found the reference in seconds. Applied before,
it would have prevented it - and it is now the final gate before removing any
hostname.

**The wider form:** configuration that exists only in a deployment environment
is a dependency no repository records, no audit reports, and no test covers.
Anything read from `import.meta.env`, `process.env` or `os.Getenv` and set in
a platform dashboard is invisible to every check that starts from the code.
Inventory those separately, and ask of each one whether a wrong value fails
loudly or silently - the silent ones are the ones that ship.

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

Same family as **Evidence discarded before it was written down**, one step earlier, and it hides from a different set of
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

### Verifying a rewrite against the copy you rewrote

A history rewrite is the sharpest version of this, because the check is so
plausible.

Two commits in a repository about to be made public contained an absolute local
path. `filter-branch` removed it, and the obvious confirmation is:

```sh
git grep -c "the-bad-string" $(git rev-list --all)     # 0 hits
```

Zero hits. **That check is worth almost nothing**, for two separate reasons, and
both are easy to miss:

1. **It confirms your own edit, not the published result.** You are grepping the
   working copy you just changed. Of course it is clean; you cleaned it.
2. **`filter-branch` keeps the originals in `refs/original/`.** They are still
   real objects, still reachable, and until they are dropped and the reflog
   expired they can still be pushed. Immediately after the rewrite the "clean"
   local repository still contained both original commits — `git cat-file -e`
   found them.

There is also a trap inside the fix: `--force-with-lease` was **rejected as
stale**, because `filter-branch` had rewritten `refs/remotes/origin/master` too.
The lease was comparing against a rewritten ghost rather than the real remote. It
takes a fetch to restore an accurate view before the lease means anything —
which is a check protecting you by refusing, and worth not overriding blindly.

**The guard: clone it fresh and check that.**

```sh
git clone <remote> /tmp/verify && cd /tmp/verify
git grep -c "the-bad-string" $(git rev-list --all)
git cat-file -e <old-sha> && echo "STILL THERE"
```

The clone is the only artefact that answers the question actually being asked —
*what will somebody else receive?* Everything before it answers *what do I
believe I did?*

Worth extending past rewrites: the same clone check is how you confirm a push
carried what you think it carried. For the Soroban repository it also built and
ran its suite from the clone, which catches the other half — a repository can
contain every file and still be missing something needed to use it.

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

## An intermittent failure invites you to blame the environment

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

## A check that would fail on correct output, kept alive only by never running

Found while auditing what still referenced the old hostname before deleting it.
The frontend CI smoke test asserted that the deployed bundle **contains**
`https://api.grainlify.0xo.in`:

```sh
if ! echo "$bundle" | grep -qE "https://api\.grainlify\.0xo\.in"; then
  echo "Attempt $i: fetched $bundle_path but it's missing the expected API URL - retrying"
```

That assertion had been correct. The migration to `api.grainlify.com` made it
false, and the bundle it was written to protect now fails it. Run against
today's production build it retries twelve times and emits `::error::`.

Nothing had noticed, because the workflow triggers on
`branches: [design/discover-page-redesign]` and `workflow_dispatch`. It never
runs on `main`.

**Correction to the first version of this entry**, which called the check
"dormant" and "kept alive only by never running". The run history says
otherwise: it ran eight times and passed every time, most recently
2026-08-15. It passed *honestly* - before 2026-08-18 the deployed bundle
really did contain `api.grainlify.0xo.in`, which is exactly why removing that
hostname took sign-in down. The assertion became false at ~07:00 on
2026-08-18, when `VITE_API_BASE_URL` was changed to `api.grainlify.com`. Zero
runs have happened since.

So the shape is not "a check that never ran" but **a check that stopped
running three days before the world moved under it**. That is a worse trap
than a never-run check, because the green history is real and reads as
evidence. Eight passes in the log invite you to trust the ninth without
running it.

This is worse than a check that reports success without checking anything,
because that one is at least running and can be caught by mutating what it
covers. A dormant check cannot be caught that way: mutate the code it guards
and nothing happens, which is indistinguishable from the check not existing.

**The guard.** For any check you are relying on, establish two separate facts:

1. **That it runs** - a trigger that actually fires for the branch in question.
   Read `on:` before believing a workflow covers `main`.
2. **That it can fail** - run it against known-bad input and watch it fail.

Both, separately. Neither implies the other. The same audit that found this had
just added an HSTS assertion to `verify-deployed.mjs` and confirmed fact 2 by
running it against production *before* the change deployed, where it correctly
reported `"max-age=63072000" - includeSubDomains missing` and exited 1. Fact 1
was already true there, since that script is invoked by hand. In CI the two
come apart, and the trigger is the half that gets forgotten.

**A related way to mislead yourself, hit while verifying the repointed check.**
Simulating the CI step locally reported `FAIL` on a bundle that was correct.
The cause was the simulation, not the check: `zsh`'s builtin `echo` interprets
backslash escapes, and minified JavaScript is full of them, so `echo "$bundle"`
mangled the content before `grep` saw it. GitHub Actions runs `bash`, where the
same line passes. **Reproduce a CI step in the shell CI uses, or the harness
becomes the finding.** `printf '%s'` is safe in both, and greping the file
directly is safer than either.


## Documentation that went stale because the world moved

Not a mistake anybody made. Correct when written, wrong now, and changed
without anyone editing the file.

`GITHUB_OAUTH_APP_SETTINGS.md` carried a banner instructing the reader to keep
the GitHub OAuth callback on `.0xo.in` **until** `api.grainlify.com` was
confirmed serving, because changing it prematurely "breaks every GitHub login
immediately". That was correct and careful advice on the day it was written.
`api.grainlify.com` then started serving, the callback moved, and the banner
kept giving the opposite of the right instruction to anybody who opened it -
including during an audit of what still pointed at the old hostname, where it
argued against a removal that was safe.

**The distinguishing feature is the tense.** The banner described a
*transition*, and a transition has an end. Prose describing a temporary state
has an expiry date that nothing enforces and no reviewer sees, because the file
does not change on the day it stops being true. Prose describing an invariant
does not.

### A second instance, found by checking rather than remembering

`RAILWAY_DEPLOYMENT.md` lists the environment variables to set. It names 20.
Production runs 44. Excluding the `RAILWAY_*` values the platform injects
itself, **16 real ones are undocumented** - among them `GITHUB_APP_ID`,
`GITHUB_APP_PRIVATE_KEY`, `MAILERCLOUD_API_KEY`, `DISCORD_BUG_REPORT_WEBHOOK_URL`
and six `TELEGRAM_*` keys, each gating a live feature. Three variables it does
document (`PORT`, `LOG_LEVEL`, `NATS_URL`) are not set at all.

Nobody wrote that wrongly. Features were added, each bringing configuration,
and none of them was a change to this file.

**A caution about this section's own examples.** This entry was going to cite
`RAILWAY_DEPLOYMENT.md` as documenting a healthcheck that was never applied.
Checking first, that is not what happened: `git log -S healthcheck` on that path
returns nothing, the file has exactly one commit, and the healthcheck was added
to `railway.json` in #482 without the doc ever mentioning one. The real staleness
in that file is the variable list above. **A remembered instance of a trap is
itself a claim to verify** - the failure mode this whole document exists to name
does not stop applying to the document.

### The guard

Documentation cannot be tested, so the answer is not "test the docs" - it is to
**move load-bearing facts out of prose and into something that executes**:

- A required-variable list belongs in a boot-time check that refuses to start,
  not in a deployment guide. The guide drifts silently; the check fails loudly
  on the one machine that matters.
- A hostname belongs in configuration read by the running system, not in an
  instruction telling a human which hostname to type.
- Where prose is genuinely the right medium, **write the invariant, not the
  transition** - and if a transition must be described, say what ends it, so a
  reader can check whether it already has.

### A verified fact has a scope, and the scope is the tree you checked it against

"Safe on both sides" was reported about a table rename: a check across every file
of another session's branch found the old table names referenced **zero** times,
so the rename could not break them. That was true, and it was true of commit
`1818238`.

Six commits later the same branch had `internal/payout/tables.go`, whose entire
contents are a constant holding the old table name, and a new function querying
the old table directly. The finding was now false. Neither session had edited
anything the other could see; the branch simply moved.

The repair is not more care at the moment of checking — the check was correct.
It is to **record what a fact was verified against**, so a later reader can tell
whether it still applies:

> Checked against `origin/session/aca22d76` at `1818238`. Re-check if that
> branch has moved.

A finding without a scope reads as permanent, and gets quoted back weeks later
as though it were. One with a scope carries its own expiry, which is the same
repair as writing the invariant rather than the transition, applied to a
verification result instead of prose.

## Two implementations of one rule, and the vector pinned the wrong one

Same family as 1-6: a check that measured a proxy. What makes this one worth its
own entry is where it sat — the node rule is what decides whether a contributor
can claim their money against a root that cannot be corrected. It was the
least-checked line in the payout path and the most expensive one to get wrong.

The claim **leaf** digest is pinned across two languages by a shared vector,
because the Go builder and the Soroban contract drifted apart once and a root
built by one could not be claimed against the other. The **tree** above the leaf
was never pinned, so vectors were added for it — roots at seven leaf counts,
asserted from both sides.

Asserted from Rust against this, in the contract's own test file:

```rust
fn node(env: &Env, a: &BytesN<32>, b: &BytesN<32>) -> BytesN<32> {
    let (x, y) = (a.to_array(), b.to_array());
    if x <= y { sha(env, &[&[NODE_PREFIX], &x, &y]) }
    else      { sha(env, &[&[NODE_PREFIX], &y, &x]) }
}
```

That helper **reimplements** `verify_proof`'s node rule rather than calling it.
So a root vector asserted through it pins the helper, and the helper and the
real verifier are then free to drift — which is the identical failure the leaf
vector exists to prevent, reproduced one level up by the very test written to
prevent it.

Measured, on the contract:

| mutation | caught by the vector? |
|---|---|
| `NODE_PREFIX` const `0x01` → `0x09` | yes — the helper reads the same const |
| the prefix removed **inside `verify_proof`** | **no** — only by settling a real claim |
| the pair ordering reversed **inside `verify_proof`** | **no** — only by settling a real claim |

The same trap was one keystroke from being reproduced on the Go side, where the
first draft of the vector test built its trees with a test-local copy of the
tree loop.

**The fix is two layers, and they catch different things:**

| layer | pins | catches |
|---|---|---|
| vector vs. helper | the two languages agree on the rule | a changed shared constant |
| a real claim through the real verifier | the shipping code implements that rule | a changed verifier |

On the Go side the loop was extracted instead, so `BuildMerkleTree` and the
vector test both call `buildFromDigests` — one implementation, nothing to drift.
On the contract side the helper stays (a test cannot easily call a private
function) and is compensated for by `a_claim_verifies_through_a_promoted_node_in_an_odd_tree`,
which publishes a real three-leaf root and claims against it. Three leaves
specifically: every pre-existing claim test used two, so none of them ever
crossed a promoted node.

**The general rule:** a cross-implementation vector is only as good as the code
path it drives. Before trusting one, ask *which function did this actually
call* — and if the answer is a helper defined in the test file, the vector is
pinning the test to itself. **Verify the artefact through the path production
will use**, which for a contract means settling a real transaction, not
recomputing a digest.

**The check:** for each shared vector, mutate the production function it is
supposed to pin — not the constant, the function body. If the vector test still
passes, it is pinning a copy.

## A test that skipped, in a suite that reported green

The newest one, and it was mine. Worth recording because the skip was *designed
in* as a convenience and read as prudence.

Two tests compare the Merkle vectors in this repository against the copies in the
sibling `Aptos-Contracts` repository. Written when that package lived inside this
one, they resolved a relative path. When it moved out to its own repository the
path became `../../../Aptos-Contracts`, which resolves on a developer machine
with both checked out side by side and nowhere else — so they were written to
skip when the sibling was missing, on the reasoning that a check which cannot run
should not fail.

That reasoning is wrong, and the output shows why:

```
--- SKIP: TestAptosFixture_IsAByteForByteCopy
--- SKIP: TestAptosMoveLiterals_MatchTheVector
ok      .../internal/chain      0.203s
```

`ok`. In CI, which performs a single checkout, both would have skipped on every
run forever. In the isolated worktree used to verify what actually ships, both
skipped too — because a worktree has no sibling directory either. The two
practices interacted: **the more carefully the suite was isolated, the less it
checked**, and it said `ok` the whole time.

A skip is not a neutral third outcome. It is a pass that establishes nothing, and
it is the one line nobody reads in a hundred-line test output.

**The fix, in two parts.**

An absent sibling now **fails**, with a message naming both remedies. And the
only way to a pass without the checks is to declare their absence in an
environment variable that is set in exactly one place — `.github/workflows/ci.yml`
— so the omission is reviewable code with a comment next to it, rather than a
runtime decision the test made on its own behalf.

```yaml
# This CI run verifies the Go half only. It says NOTHING about the Move
# contract, whose suite runs in its own repository.
GRAINLIFY_SIBLING_REPOS_ABSENT: '1'
```

Plus the guard from **A regex guard that matched one gate of three**, because two checks reading one file each is a suite that can
silently halve: a third test asserts how many sibling files are actually named
and reachable, and the literal count is checked against a hardcoded 7 as well as
against the vector's own length — so deleting a root from *both* sides fails
rather than shrinking the check.

**The general rule:** a conditional skip is a silent reduction in coverage, and
the condition is almost never "this check is not applicable" — it is "this check
could not find what it needed". Those want opposite outcomes. Prefer a failure
with instructions, and if a real environment must be exempt, make the exemption
live in that environment's configuration where a human reviews it.

**And write down what a green run does not cover.** For this pair it is one
sentence, now in the test file, the README of the other repository, and the CI
comment: *a green backend run says nothing about the Move contract* — not when
the sibling is absent, and not when it is present either, because these tests
compare stored values and never compile a line of Move.

## When two sources disagree about where someone's money goes, ask

Not a check that lied — a design rule, kept here because it is the rule behind
several entries above and because the tempting alternative is always the one that
looks more helpful.

**When two sources of truth disagree about where a person's money should go,
surface the disagreement. Never reconcile it silently, in either direction.**

The pull is always toward silent reconciliation, because it reads as smoothness.
Every instance below arrived as a convenience.

### Address verification signed with a different address than the one saved

A contributor saves `0x…1a2b`, then signs the verification challenge with
`0x…c3d4` — usually a wallet defaulting to the wrong account.

The helpful-looking design updates the stored address to whichever one signed:
the wallet evidently controls that key, the user evidently intended it, and the
flow completes without friction. **It is wrong.** A mis-selected account silently
replaces an address the contributor deliberately typed, everything succeeds, and
their payout goes somewhere they did not choose. Nothing failed, so nothing gets
investigated.

Reject, and name both addresses: *"You signed with `0x…c3d4`, but the address you
saved is `0x…1a2b`."* Both remedies are one click. The person makes the choice.

### The database says paid, the chain says unclaimed

The chain is the system of record, so the database is corrected — but the
correction is to mark the row unpaid and **alert**, not to re-send the payment.

A database that can be wrong about payment is a database whose automatic
correction can also be wrong, and the failure mode of guessing here is paying
twice. Reconciling toward "pay again" is the direction that looks like fixing it.

### A claim row that looks like unfinished work

`chain_operations` holds operations we submit and operations we merely observe.
A stale claim row looks exactly like a crashed submission, and "recovering" it
means sending funds to somebody who never signed for them — a push payout, in a
system that is pull-based specifically to prevent that.

This one is not solved by asking, because there is no human in the loop at 3am. It
is solved by making the row unstorable in a retryable state (migration `000077`)
and un-constructible as a submittable value (`internal/chainops`). **Where the
disagreement can be resolved by a machine at all, remove the machine's ability to
resolve it in the dangerous direction.**

### The general shape

Ask which of these a piece of reconciliation logic is:

| | |
|---|---|
| Two sources disagree, and one is authoritative | Correct toward the authority, and **alert**. Do not act on the correction. |
| Two sources disagree, and a person can tell you which is right | Ask them. Name both values. |
| Two sources disagree, and neither is authoritative | Stop. This is the case where silent reconciliation does the most damage. |

**The test:** for any code that resolves a mismatch automatically, ask what
happens if it resolves it the wrong way. If the answer involves somebody's money
arriving somewhere they did not choose, or arriving twice, it is not a
reconciliation — it is a decision, and it belongs to a person.

## A search that answered a narrower question than the one asked

> **An absence is the dangerous answer, because absence is what a filter
> produces.** A search that finds something can be checked against what it
> found; a search that finds nothing offers nothing to check.

While verifying a deploy, a grep for registered routes was filtered down to
auth-related ones:

```sh
grep -rhoE '(Get|Post)\("(/[a-zA-Z0-9/_:.-]*)"' ... | grep -iE 'auth|login|session|token|me\b|dev'
```

It returned `/auth/github/callback` and not `/auth/github/login/callback`. The
live service was redirecting OAuth to the second one, so the obvious reading was
that sign-in was broken in production — a route the app redirects to that does
not exist. That would have been an outage report, thirteen commits after a push.

**Sign-in was fine.** Both routes exist. The pipeline's second stage dropped the
one that mattered, and the output looked exactly like a complete list of auth
routes because every line in it was an auth route.

That is the same defect as a substring check reporting old copy as still live,
and as the failed view call that got compared to a leaf and printed
**ROOT MISMATCH**: in each case a tool answered a **narrower question than the
one being asked**, and returned a confident answer to it. Nothing errored. The
narrowing was invisible in the result, because a filtered list and a complete
list look identical once you are reading the list.

### The check

**When a search reports something absent, ask what the search could not have
found.** Then re-run it without the narrowing step and diff the two. If a filter,
a path restriction, a file-type flag or an `--include` stands between the corpus
and the answer, the answer is about the filter until proven otherwise.

Cheapest habit that would have caught this one: before believing an absence,
grep the corpus for the *specific string you expected to be missing*, with no
pipeline after it. Here that is one command, and it returns the route
immediately.

## A fail-closed default that hid the absence of a test for itself

> **When the safe behaviour and the tested behaviour are the same behaviour,
> nothing distinguishes them** — and the test you did not write looks exactly
> like the test you did.

`internal/salt` decrypts a per-event salt, hands a hasher to a closure, and
zeroes the plaintext when the closure returns. Two protections, deliberately
overlapping:

1. the plaintext buffer is zeroed, and
2. the hasher is marked dead, so one that escapes its closure returns
   `ErrExpired` instead of hashing.

Nineteen tests passed. A mutation that **deleted the zeroing entirely** survived
every one of them.

The reason is the second protection. Every test that could have noticed the
plaintext still sitting in memory went through the hasher, and the hasher was
already refusing to answer — so the observable behaviour was identical whether
the buffer was zeroed or not. The design defended the property so well that
nothing was left to detect whether the property held.

That is the trap: **a defensive design can mask the absence of a test for the
thing it defends.** Belt and braces are good engineering and bad evidence,
because with both on you cannot tell from the outside whether either is holding
the trousers up.

The fix was a test that reaches past the public surface and asserts the buffer
itself:

```go
var h *hasher
_ = WithSalt(ctx, pool, key, id, func(got Hasher) error {
    h = got.(*hasher)
    if allZero(h.salt) { t.Fatal("already zeroed INSIDE the closure") }
    return nil
})
if !allZero(h.salt) { t.Fatal("plaintext still in memory after the closure returned") }
```

Reaching into an unexported field is normally a smell. Here it is the only
vantage point from which the two protections are distinguishable, and a test
that cannot distinguish them is testing one thing while appearing to test two.

### The check

For any property with more than one protection, ask: **if I removed protection A
and left B, would a single test change colour?** If not, A is unverified no
matter how many tests pass. Mutation testing finds these; reasoning about them
rarely does, because from the outside the redundant system looks like a working
one — which it is, right up until somebody removes the half nobody was checking.

## A fault that presented as a legitimate value

> The inverse of a blank where a value belongs: **a value where a *different*
> value belongs, with nothing to mark the difference.**

`internal/salt` requires `SALT_ENC_KEY_B64` to decode to exactly 32 bytes, for
AES-256. A mutation removing that length check survived the whole suite.

The tests fed it an empty key, unparseable base64, a 9-byte key and a 64-byte
key — and every one still failed, because `aes.NewCipher` rejects them on its
own. What no test fed it was **16 or 24 bytes**, and those are *valid AES key
sizes*. `aes.NewCipher` accepts them, GCM works, encryption and decryption round
trip, every test passes.

The system silently becomes AES-128. Nothing errors, nothing is blank, nothing
is malformed. The security level drops and the only artefact is a number nobody
prints.

This is the family the rest of this file is mostly the mirror of. Elsewhere the
danger is an **empty** result that looks like a pass — a filtered grep, a missing
selector, an unrendered page. Here the danger is a **well-formed** result that is
the wrong one. Both defeat "did it error?", and the second also defeats "did it
return something?", which is the fallback people reach for once burned by the
first.

Where it lurks: anywhere a parameter has several legal values of which only one
is correct — key sizes, hash algorithms, curve choices, rounding modes, decimal
precision, timezone handling, chain ids. A wrong choice among legal values is
indistinguishable from a right one at every layer that only checks legality.

### The check

**Test the boundary of the intended value, not the boundary of the legal one.**
Ask what the next-most-plausible legal value is, feed it, and require rejection.
Here that is one line:

```go
base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 16)), // valid AES, wrong AES
```

And when a check looks redundant because a library already validates, establish
what the library validates *against*. `aes.NewCipher` enforces "a legal AES key".
It has no opinion about which AES you meant.

## A reference that named WHICH, not WHAT

> **A reference that names which items, or where they sit, goes stale silently as
> the thing it covers grows.** One that names what it is looking for does not.
> The repair is always the same shape: glob, or search for a marker.

This is the entry to read first, because it is four separate incidents with one
cause, and they were fixed one at a time over a single day without anybody
noticing they were the same fault.

| Where | The reference | How it went stale |
|---|---|---|
| Branch migrations | `000076`, `000077`, `000078` | main added its own; the numbers had to be reassigned |
| Drift checks | `"../../../Aptos-Contracts"` | sessions moved into worktrees, one level deeper — every check failed |
| A script edit | replace line 34 | the line moved; the edit landed somewhere else |
| Mutation harness | `cp resolve.go digest.go build.go dryrun.go "$BAK/"` | `serve.go` was added later, mutated, and never restored |

None of them errored. A stale *which* does not announce itself, because the
reference is still perfectly valid — it just no longer refers to everything it is
meant to cover. That is what separates this from a broken path: a broken path
fails loudly the first time, and an incomplete list keeps working for the items
it happens to name.

The repair, every time:

```sh
cp resolve.go digest.go build.go "$BAK/"    # names WHICH
cp ./*.go "$BAK/"                           # names WHAT
```

```go
const dir = "../../../Aptos-Contracts"      // names WHERE
findUpwards("Aptos-Contracts/sources/escrow.move")  // names WHAT
```

For an in-place edit there is no glob to reach for, and the same repair takes the
form of an assertion on the text itself:

```python
del lines[26:40]                  # names WHERE - silently removes whatever is there now
assert src.count(block) == 1      # names WHAT - refuses if the file moved under you
src = src.replace(block, "", 1)
```

Both halves matter. A line range applied after an earlier edit shifted the file
removes something real and says nothing; the count turns that from silently-wrong
into a loud refusal. Hit while splitting a package: the assertion failed by one
line, so nothing was deleted and a build error survived to be noticed. That was
luck, and the assertion is what converted luck into a guarantee.

The drift-check fix and the harness fix are the *same repair applied to two
tools*, made hours apart, and neither of us connected them at the time. That is
the argument for writing the general form down rather than the incident: the next
instance will not look like a file list or a relative path, and the only thing
that will recognise it is the rule.

### A companion case: a rule scoped to the wrong verb

The same day, a rule that each session works in its own git worktree — written
about **writes**, because the incident that prompted it was one session branching
off another's unpushed commit — was broken through **reads**. I changed directory
into the shared checkout to *inspect* a tool, and then kept editing from where I
had landed. Three files were modified in the wrong tree before a commit reported
"nothing to commit" and gave it away.

Nothing about the editing was careless. The navigation was, one step earlier, and
by then the working directory was simply wrong for everything that followed.

> **A habit that puts you in the wrong place does not care which verb you use
> next.** So the rule extends: read from the worktree too. Not because reading is
> dangerous, but because reading is how you end up somewhere, and where you are
> is what the next command inherits.

### The check

**Ask of any reference: would this still be correct if somebody added one more?**
If the answer depends on them remembering to update it, it names WHICH. Replace
it with a glob, a marker search, or a query — something that finds members by
what they are.

## A tool that corrupted the source it was checking

> A harness that modifies files it did not back up leaves the working tree
> silently wrong, and **the corruption then presents as a bug in the code under
> test.**
>
> This is the file-list instance of the entry above. It is kept separate because
> its second-order damage — a tool corrupting its own subject — is worth its own
> warning.

`internal/payout/mutate.sh` backed up an explicit list of files:

```sh
BAK=$(mktemp -d); cp resolve.go digest.go build.go dryrun.go "$BAK/"
```

That list was written when the package had four files. `serve.go` arrived later,
mutations were added for it, and it was never backed up — so each mutation was
applied and never reverted. They **stacked**: five mutations deep by the end of
the run, the file was returning the wrong leaf's amount, the wrong proof, and a
zeroed identity hash.

The run itself reported `21 killed, 1 survived`. Nothing looked wrong.

The damage showed up afterwards, as three failing tests that read exactly like a
real defect in proof serving. Time went into debugging the *algorithm* — is the
tree sorted, is `leaf_index` the pre-sort position, is pgx aliasing the byte
slices — before checking whether the file on disk was the file that had been
written. It was not.

Then a second-order failure: an edit intended to fix the code silently did not
apply, because the text it searched for had been mutated out from under it. A
`.replace()` with no assertion reports success by saying nothing.

### The checks

**A tool that writes to the tree must prove it put the tree back.** Not restore
and hope — checksum before, restore, checksum after, and fail loudly if they
differ:

```sh
BAK=$(mktemp -d); cp ./*.go "$BAK/"
before=$(cat ./*.go | shasum | cut -d" " -f1)
restore_all() {
  cp "$BAK"/*.go ./
  [ "$before" = "$(cat ./*.go | shasum | cut -d" " -f1)" ] || { echo "RESTORE FAILED" >&2; exit 2; }
}
trap restore_all EXIT
```

**Enumerate by glob, never by list.** A hand-written list of files is correct on
the day it is written and silently incomplete from the next file onwards. This is
the same defect as a filtered grep reporting a route absent: the tool answered a
narrower question than the one being asked.

**Assert that a search-and-replace matched.** `s.replace(a, b)` that finds
nothing changes nothing and says nothing. Use an assertion, and read the failure
rather than re-running with a different guess.

And the diagnostic habit that would have saved the whole detour: **when tests
fail right after a tool ran over the source, check the source before debugging
the logic.** `grep -c` for the mutation markers takes seconds; the algorithm
hypotheses took considerably longer and were all wrong.

### A note on the harness itself

This is the third time the mutation harness has caught a fault in its own
operation: first a control mutation that survived, then five mutations that
silently never applied, and now this. The difference is that the first two were
faults it *survived* and this one it *caused*.

That is not an argument against the harness. A tool that runs over source and
reports on it will occasionally break it, and the harness is still the only thing
in this repository that has ever found an untested assertion. It is the argument
for the checksum: a tool permitted to write to the tree must prove it put the
tree back, every run, without being asked.

## An empty list that meant two opposite things

> **A filter on a field nothing populates is indistinguishable from an empty
> result set.** Both render as "you have nothing", and only one of them is true.

Three instances of this in a single day, on one feature. They were fixed
separately before anybody noticed they were one shape.

| Where | The empty list said | It actually meant |
|---|---|---|
| `/me/claims` before publication | "you have no claims" | "you have a claim; the root is not published yet" |
| `/me/claims` for somebody with no address | "you have no claims" | "you were **permanently excluded** and your share is residue" |
| `/me/claims` after a **successful** publication | "you have no claims" | "`published_tx` was never recorded, because nothing set it" |

The third is the sharpest, because everything worked. The tree was built, the
escrow funded, `publish_root` accepted, the transaction confirmed on chain — and
every contributor saw an empty list, because the query filters on
`published_tx IS NOT NULL` and no code path wrote that column. A column existed,
a query depended on it, and nothing in between populated it.

That is the same family as a config table read by code and seeded by no
migration, and a column populated and never read: **a schema and a codebase
disagreeing about who is responsible for a value.** What makes this variant
worse is that the disagreement renders as a legitimate, reassuring UI state. An
unpopulated column produces no error, no warning and no empty-state distinct from
the real one — it produces a clean page that says nothing is wrong.

### Why this defeats the usual checks

- **Not an error.** The query succeeds. The handler returns `200`.
- **Not a blank where a value belongs.** The response is well-formed; the list is
  simply short.
- **Correct in the test.** An integration test that publishes *and records*
  passes. Only the real sequence, where recording is a separate act somebody must
  perform, exposes it.
- **Correct for most users.** Anybody genuinely owed nothing sees the identical
  page, so the bug is invisible in aggregate and visible only to the people it
  harms.

### The checks

**For every filter, ask who writes the field it filters on.** If the answer is
"an operator, later" or "another service", then absence is a *state*, not a
*result*, and the two need different renderings. `published_tx IS NULL` means
"not yet"; no rows means "nothing owed". They must not collapse.

**An empty result needs a reason attached, not just a count.** The fix here was
not to remove the filter — the filter is correct, contributors must not see
unpublishable claims. It was to add `/me/payout-readiness`, a route that speaks
to somebody who has *not* acted and can distinguish "nothing owed" from "owed and
blocked". Silence cannot carry that distinction; a second signal has to.

**Ask what the empty state looks like when the system is working perfectly.** If
that is identical to the failure, the interface has no way to tell you it broke.

## A comment describing a hazard, and the same hazard in the test beside it

> **Writing down a trap does not immunise its author against it.** The comment is
> evidence the hazard was *understood*, and no evidence at all that it was
> *avoided*.

`payout_claims.go` served an asset symbol. Decimals came from `chain_configs`;
the symbol beside it was the literal `"USDC"`. The comment written at the time
said, in as many words:

> nobody is harmed today, because the seeded symbol *is* `USDC` — which is
> exactly why it would survive a change to anything else

The test written minutes later asserted:

```go
if asset["symbol"] != "USDC" { t.Errorf(...) }
```

The seeded symbol is `USDC`. So the assertion passes whether the value comes
from the row or from the literal, and a mutation restoring the hardcoded string
survived the suite. **The test could not distinguish the two cases for precisely
the reason the comment had just finished explaining.**

Nothing was forgotten in between. The comment and the test were written minutes
apart by the same mind holding the same assumption — that `USDC` is what a
correct system returns — and that assumption is true in both the working and the
broken version. Understanding a hazard operates on the code you are looking at;
it does not carry to the next thing you write, because the next thing feels like
a different problem.

**This has now happened twice, in the same shape.** The second time was a
balance check: `CheckBalance` was tested with the balance set *equal to* the
floor, and the assertion looked for the floor's rendered value — which appeared
in the message from the **balance** position. A mutation replacing the floor with
a placeholder passed, because the expected string was still there for a different
reason.

Two instances, one rule:

> **Pick fixture values that CANNOT coincide with the thing being asserted.**
> If the setup value and the expected value are equal, the assertion cannot tell
> you which one it found — and "the right answer for the wrong reason" is
> indistinguishable from "the right answer".

Seeded symbol `USDC`, expected `USDC`. Balance equal to floor, expecting the
floor. In both, one distinct value in the fixture would have made the test
meaningful, and the cost of choosing one is zero.

The fix is to make the two cases *differ*: change the row and require the
response to follow.

```go
d.Pool.Exec(ctx, `UPDATE chain_configs SET asset = jsonb_set(asset,'{symbol}','"ZZZ"') …`)
// ... and now require "ZZZ"
```

### The check

**When an assertion compares against a constant, ask whether the wrong
implementation would produce that same constant.** Config that happens to equal
the default, an ID that happens to be zero, a first element that happens to be
the right one — in each case the test is confirming agreement between two things
that were never independent.

And the harder habit: **a comment noting "this only works because X happens to be
true" is a specification for the test that must be written next.** Treat it as a
TODO with an assertion attached, not as a caveat that has been dealt with by
being described.

## A fixture that bypassed an invariant tested the invariant

> **If your test setup writes state the system would never produce, you are
> testing the guard that rejects it — not the thing you meant to test.**

To reach the "two claims in one settlement" branch, a test inserted a
`claim_leaves` row directly:

```sql
INSERT INTO claim_leaves (settlement_id, leaf_index, leaf_hash, …)
VALUES ($1, 99, decode(repeat('ab',32),'hex'), …)
```

A fabricated leaf digest, in a tree that was already published. The test failed —
and the failure was correct. `ClaimFor` compares the rebuilt tree against the
published root and refuses to serve a proof from leaves that have drifted, which
is exactly what a hand-written leaf row is.

So the test exercised the drift check, reported it as a failure of the branch
under test, and said nothing whatsoever about that branch. The fix was to build
the state the way the system builds it: two real entitlements, a real tree, a
real publication, and then the address overlap that produces two matches.

### The check

**Ask what the system would have to do to reach the state your fixture wrote
directly.** If there is no such path, the fixture is describing an impossible
world and the test's outcome is about the guard standing between them.

This is the mirror of an entry above: there, a test passed because setup and
assertion shared an assumption. Here, a test failed because setup violated an
invariant the assertion depended on. **Both are the fixture, not the code** — and
in both, the reported result pointed at the wrong thing.

## A check that enumerates its own scope

> **A check that decides for itself what to look at answers a narrower question
> than the one it was asked — and reports the narrow answer in the wide
> question's words.**

Four instances in one day, four different mechanisms, one failure:

| The check | How it chose its scope | What it missed |
|---|---|---|
| Structural writer check | a hand-written list of files | a file added after the list was written |
| Scope-doc check | concatenates every payout handler into one blob | *which* handler a key belongs to — a key documented under the wrong endpoint passes |
| Mutation harness | `cp resolve.go digest.go build.go "$BAK/"` | `serve.go`, added later: mutated, never restored, five deep by the end |
| Mutation harness, again | `go test -run TestRegister_` | a test written after the filter, so a mutation its own suite would have caught reported SURVIVED |

None of them errored. Each reported a clean result in language that sounded
total — "no violations", "the document matches the handlers", "all mutations
killed" — while having examined a subset it selected itself.

**The scope is an input, and it is the input nobody validates.** Enormous care
goes into the assertion; the list of things the assertion runs over is written
once and never revisited, because it does not look like part of the logic.

### Why it recurs even once you know about it

The four above were fixed one at a time, hours apart, by somebody who had already
written up two of them. Each presented as a different problem — a stale file
list, a grep too coarse, a backup that missed a file, a `-run` filter — and the
shared shape is only visible when they are put in a column together. That is the
argument for naming the class rather than the instances: the fifth will not look
like a file list either.

### The checks

**Prefer a glob, a marker search, or a query to a list.** `cp ./*.go` cannot go
stale; `cp a.go b.go c.go` goes stale the moment someone adds `d.go`.

**Assert the input before trusting the output.** A check that examined zero files
finds zero problems, which is indistinguishable from a clean tree. Count what you
scanned and fail on an implausible count.

**Say what was excluded, in the result.** If a check bounds its scope on purpose,
that bound belongs in the output next to the verdict — "42 files, 0 violations"
rather than "0 violations". A number the reader can sanity-check is what turns a
silent narrowing into a visible one.

**And for a harness that filters tests: widen the filter whenever you add a test
to the file it guards.** Better, do not filter at all — run the package.

## A constraint added late audits the tests that were written without it

> **Adding a correctness constraint does not only prevent bad data going
> forward. It tells you which of your existing tests were relying on its
> absence.** That is a reason to add constraints early which has nothing to do
> with data integrity.

A unique index was added: one live payout address belongs to one account. The
production table held a single row, so it applied cleanly and changed nothing
about the data.

It broke the test suite immediately.

One fixture had been handing the same constant address to three different users.
Another two packages shared address constants and ran concurrently against one
database. Both had been passing for weeks — not because they were right, but
because **nothing had ever asserted the property they were violating.** The
constraint did not introduce the problem; it revealed that the tests had been
describing a world the system was about to stop permitting.

The corollary is uncomfortable and worth stating plainly: for as long as a rule
is unenforced, **passing tests are evidence that the rule is unenforced, not
evidence that the code respects it.** Every fixture written in that window is
free to depend on the gap, and none of them will announce that they do.

### The check

**When a constraint is added and tests fail, read the failures as an audit
before treating them as breakage.** Each one names a place that was relying on
the absence. The instinct is to fix the fixture and move on; the information is
in *which* fixtures needed fixing, because production code written in the same
window had the same freedom.

And the scheduling argument: a constraint costs a handful of fixture edits today
and an unbounded data-reconciliation exercise once real records depend on the
gap. **The cheap moment to add one is before it is true by accident** — while the
table holds one row rather than thirty-eight.

## An error named after the layer that noticed it

> **An error name that describes the layer which noticed the problem, rather
> than the problem, sends every future reader to the wrong place — and does it
> confidently.**

A view call to an Aptos fullnode returned `400`. The client mapped every non-200
to one error:

```go
ErrNodeFailed = errors.New("chain_node_unreachable")
```

The node was perfectly reachable. It had answered, correctly and quickly, with a
Move abort: `E_NOT_INITIALISED(0x2)` — *there is no escrow at that address.*

Two opposite facts arrive under one HTTP status. One means **check the network**;
the other means **check the address**. The name asserted the first with total
confidence, and an operator following it would examine node health, DNS, the RPC
provider and their own connectivity, finding nothing wrong with any of them,
because nothing was.

The name was accurate about **where the failure was detected** — the HTTP layer,
which saw a non-200 — and silent about **what failed**, which was neither HTTP
nor the node.

### Why this is easy to write and hard to see

At the point the error is constructed, the layer that noticed is the only thing
in scope. You are holding a `*http.Response`; "the node did not give me a 200" is
a true and complete description *of what you can see from there*. The
distinction requires looking INTO the body you already have, and the body is
right there — which is what makes this a habit failure rather than a
missing-information failure.

It was found by pointing the client at a real node with a deliberately wrong
address, reading the message, and noticing it named the wrong thing.

The fix separates the causes:

```go
ErrNodeFailed      // transport, timeout, non-200 with no abort in it
ErrContractAborted // the contract answered, and said no
```

with a test asserting neither satisfies the other's `errors.Is` — because two
names that both match are one name wearing two hats.

### The check

**Read your error names as instructions.** `chain_node_unreachable` is an
instruction: *go and look at the node.* If following that instruction would waste
somebody's time in the most likely failure case, the name is wrong regardless of
how accurate it is about the mechanism.

**Ask what the layer below actually said before summarising it.** A non-200 with
a body is not one fact; it is a status *and* a payload, and the payload usually
contains the real answer. Discarding it and naming the status is how a precise
upstream error becomes a vague local one.

And the general form, of which this is one instance: **name errors after causes,
not after detectors.** "Timeout", "unreachable", "parse failed", "non-200" all
describe the observer. "The escrow does not exist" describes the world.

## A milestone that read as a deployment

> **A milestone proves a thing CAN happen. Only a deploy proves it DOES.**
> Anything demonstrated by a local script needs that written beside it, or it
> reads as shipped.

The best result in the payout workstream was a sponsored claim on testnet: an
account with `sequence_number: 0` — its first transaction ever — received USDC
and paid **zero** gas, with a `fee_payer_signature` from our sponsor. Recorded,
linked, verified against the chain.

The fee payer was a laptop. `tools/wallet-check/serve.js`, listening on
`localhost:8899`, reading a testnet key out of a gitignored config. **No
production code pays anybody's gas**, and a contributor clicking Claim goes
nowhere near that script.

Nobody noticed for weeks.

### The part worth studying: every sentence written about it was true

Two things were documented carefully, and both are correct:

> sponsorship is entirely an AIP-39 **transaction-layer** concern — the contract
> cannot tell who paid

> the self-paid fallback is **the absence of a restriction**, not code, so it
> cannot be deleted

What was never written anywhere is that **nothing in production implements the
transaction layer.**

So there is no false sentence to find. Each statement explains why the *contract*
is silent about gas, and neither says who is not silent about it — and a reader
assembling them concludes *sponsorship is handled*, which is the conclusion the
author also held. **A gap between two true statements is invisible in a way a
wrong statement is not**: review catches wrong sentences, and there was nothing
to catch.

The milestone then did the rest. A demonstration that something *can* happen is
read as evidence that it *does*, because the demonstration is concrete — a real
hash, a real balance change — and the absence is abstract.

### Why testnet made it worse

APT is free from a faucet. So the self-paid path **works** in every rehearsal: a
tester tops up, claims, and it succeeds. The rehearsal passes while exercising a
path no real deployment uses, and the faucet is precisely the thing that will not
be there. A green testnet run was evidence about the faucet.

This generalises past gas: **any resource that is free in the test environment and
scarce in production turns a rehearsal into a test of the environment.**

### The checks

**Write the deployment status next to the demonstration, in the same block.** Not
in a status doc, not in an issue — beside the transaction hash, where the
impressive thing is:

> Sponsored claim: `0xe33e…` — **fee payer was `tools/wallet-check/serve.js`,
> a local script. No production sponsor exists.**

**For any capability, ask which deployed artifact provides it.** Name the file
that ships. If the answer is a script, a test, or a tool directory, the
capability is demonstrated and not shipped, and those are different words on
purpose.

**When two true statements sit next to each other, ask what a reader will
conclude from both.** The conclusion is not in either sentence and nothing
reviews it. Here: "the contract can't tell who pays" plus "self-paid needs no
code" reads as "gas is handled", which neither says.

**And grep for the mechanism, not the vocabulary.** Searching `sponsor` finds
event sponsors funding prize pools in the backend and `fund`'s signer in the Move
module — both real, neither relevant. The question is not "does the word appear"
but "which deployed code signs as fee payer", and the honest answer was: none.

## An oracle is something you could not have influenced

> **Not "independently implemented". Not "generated by a different tool".
> Outside your reach entirely.** A generated vector is an oracle only if
> something you cannot edit says it is.

A hand-written BCS encoder needed proving. Two artefacts existed to prove it
against:

1. **A generated vector** — built by a script in this repository, using a
   different SDK, in a different language.
2. **A transaction on a public chain** — signed months earlier by other software,
   recorded, immutable.

They disagreed with the encoder in opposite directions, and only one of them
could settle it.

The generated vector was **wrong**. An argument passed as a hex string was
encoded by the builder as a Move `String` — `"0x3333…"` as 66 ASCII bytes —
instead of `vector<u8>`. The encoder was right.

### Why "different implementation" was not enough

The encoder and the generator were written by the same session, hours apart, from
the same mental model of the transaction's shape. A different language and a
different SDK did nothing to separate them: **the mistake was in the model, and
both artefacts inherited it.** When they disagreed, neither had standing to
settle it, because the disagreement was between two expressions of one
understanding.

And the failure mode had a direction. Had only the generator existed, the
disagreement would have been read as *"the encoder is wrong"* — a **correct
encoder would have been edited to match a wrong reference**, with a green test
attesting to it afterwards. The test would then have been actively harmful: it
would defend the error against anybody who later fixed it.

The on-chain transaction settled it for one reason, and it is not that it was
produced by a different tool. **It is that we could not have influenced it.**
Signed by other software, at a time before the question existed, recorded
somewhere nobody can edit.

### Same family as identity that both sides derive

This is the shape recorded elsewhere in this file as *a test asserting a value
the fixture also produced* — seeded `USDC` compared against literal `USDC`, a
balance equal to the floor it was checked against. There, two values inside one
process agreed because they came from one source. Here, two **artefacts** inside
one mental model agreed with each other and with nothing else.

> Two things computed inside one head agree with each other and with nothing
> else. Agreement is only evidence when the things agreeing had a chance to
> disagree for reasons you did not supply.

### The check

**Rank your oracles by how far outside your control they are**, not by how
different they look:

| | strength |
|---|---|
| a value you wrote in the test | none — it is the assertion, twice |
| a second implementation you wrote | weak — shares your model |
| a tool you configured and ran | weak — you chose the inputs |
| a published artefact you did not produce | **strong** |
| a record you could not alter if you wanted to | **strongest** |

**Before trusting a generated vector, ask what would have to be true for it to be
wrong.** If the answer is "I would have had to misunderstand the format" — the
same thing that would make the code wrong — it is not an oracle, it is a second
opinion from the same source.

**When a generated reference and your code disagree, do not assume the code is
wrong.** Find something neither of you touched. If nothing like that exists,
say so rather than picking a winner.

## A true answer to the question you asked, and a false one to the question you meant

> **A migrated representation makes the old query correct and useless at the same
> time.** The API is not lying; it is answering something else.

The sponsorship path needed the sponsor's APT balance, to refuse rather than
drain the account. The obvious read:

```
GET /v1/accounts/{addr}/resource/0x1::coin::CoinStore<0x1::aptos_coin::AptosCoin>
→ 404 {"message":"Resource not found by Address(0x1b41…)"}
```

That account holds **9.77 APT**. APT has migrated to a fungible-asset store, so
the `CoinStore` resource genuinely does not exist — and the question asked was
*"does this coin resource exist"*, which was answered correctly. The question
meant was *"does this account hold APT"*.

Nothing errored in a way that pointed anywhere useful. A 404 for a resource is an
ordinary answer.

### The direction is what makes it dangerous

The balance read is on a **fail-closed** path: too little balance means refuse to
sponsor. So a false zero refuses **every** claim, for everyone, while the money
sits in the account untouched.

And the symptom points at the wrong thing. "Sponsorship is refusing because the
sponsor looks broke" reads as a funding problem, not a query problem — **we would
have topped the account up and watched nothing change.** The investigation would
have started at the account and never reached the URL.

### The general form, which is the reusable part

> **A value that means "none" must not be reachable by a path that means "I
> couldn't tell."**

Zero, empty, false and *absent* are answers. "The lookup failed", "the shape
changed", "I asked the wrong endpoint" are not answers, and collapsing the second
set into the first is how a system reports a confident wrong number. It is the
same family as an empty list meaning two opposite things, and as a filter on a
field nobody populates — each is a not-knowing wearing the costume of a knowing.

The test that holds it:

```go
// A failed read must ERROR, never return a bare zero.
for _, tc := range []struct{ status int; body string }{
    {404, `{"message":"Resource not found"}`},
    {200, `not-a-number`},
    {503, `upstream down`},
} { /* every one must produce an error */ }
```

### The checks

**When a read returns "nothing", ask whether the thing is absent or the question
is stale.** Migrations, renames and representation changes all turn a working
query into a correct-and-irrelevant one, and none of them break it loudly.

**Check a read against a value you know independently.** The account had a
balance visible in a block explorer and in the CLI. One comparison against
something outside the code path settles in seconds what reasoning about the API
will not settle at all — the oracle rule, applied to a query rather than a
vector.

**On a fail-closed path, treat "I could not read it" as its own outcome.** Not as
the safe value. The safe value is a decision; not knowing is a different state and
usually needs a different sentence.

## What a test asserts is not what its author believed it asserted

A family rather than an incident, and it now has enough members to be worth
naming. Several entries above are instances; this section is where the shape
lives, so the next one is recognised as a repeat rather than filed as a novelty.

**The failure:** a test passes, and it passes for a reason its author did not
choose. The assertion is true. It is simply about something other than the thing
the test is named for, and nothing distinguishes those two cases from the outside
— a green line looks identical either way.

Members so far, each also filed under its own mechanism because each *also*
teaches something specific:

| Where | What it actually asserted |
|---|---|
| **Shell flags that silently return nothing** — the test named after a guard | That inputs unrelated to the guard return nothing. Deleting the guard changed nothing. |
| **A regex guard that matched one gate of three** | That the one gate it happened to match was correct. |
| **A test that evaluated its own constant** | That a value equals itself. |
| **A test that passed on a build where clicking did nothing** — removal without rewiring | That a component was gone. Not that anything replaced it. |
| Below, tests that quietly overfunded | That publishing worked *when overfunded*, which nobody had decided was allowed. |

### The newest member: tests that pass for a reason nobody chose

The escrow's `publish_root` originally asserted `total <= balance`. Four tests
funded 1,000,000 and published a root of 600,000. They passed, correctly, and had
passed since they were written.

Nothing was wrong with the contract and nothing was wrong with the assertions.
What was wrong is that **overfunding had never been a decision** — it was a habit
the fixtures happened to encode, and every test run was quietly certifying it as
supported behaviour.

It surfaced only when the rule tightened to `total == funded_total`, because an
address-less contributor's share would otherwise sit in the escrow looking exactly
like residue, which is sweepable with no timelock. Four tests went red immediately.
That was the guard working. It was also the first time anybody had asked whether
those fixtures meant anything.

**What makes this variant distinct** from the others above: there was no defective
assertion to find. Each of those four tests was correct in isolation. The defect
was in what the *set* of them established — a norm nobody had chosen — and no
amount of reading any single test would have revealed it.

### The check

For the individual case, the one already in **e2e checks on pages that never rendered**: when a check passes, ask what it
would take for it to fail. If you cannot answer, break the thing on purpose.

For this variant, a different question, because the tests are individually fine:

> **What does my fixture assume that I have never decided?**

Fixture values are decisions in disguise. An amount, a count, a timestamp offset,
a funded balance — each is a claim about what the system supports, made by whoever
was writing a test at the time and inherited unexamined by everyone after. When a
rule later tightens and a batch of tests goes red together, that is not a
regression. **It is the first time the assumption was stated out loud**, and the
right response is to read what the fixtures had been asserting rather than to
adjust them until the suite is green again.

## An experiment that passed without ever running

Deliberately deploying a broken build, to prove Railway's healthcheck holds
traffic on the previous deployment, produced a clean result on the first
attempt. The healthcheck appeared to work. It had not been tested at all.

`railway up` uploads the directory named by the **linked project's**
`projectPath`, not the directory the command was typed in. That path pointed at
a different checkout, so the upload was clean `main` — the deliberately broken
build never left the machine. Railway built working code, it started, the
healthcheck passed, traffic moved, and every observation was consistent with
the hypothesis. The experiment confirmed a property of a deployment that did
not contain the code under test.

**It was caught by a single number that should not have been possible.** The
broken build was rigged to fail its healthcheck, so `/health` had to return
503 or time out. It returned **200**. A hypothesis that survives its test is
unremarkable; a control that reports the wrong value is not. Chasing the 200
rather than accepting the tidy result is the only reason the run was thrown
out and repeated properly, where the healthcheck did hold and the previous
deployment did keep serving.

### The general form

**An experiment can pass without having run.** Every layer between "I changed
the code" and "the system executed it" — a build cache, a stale artifact, a
CDN, a path indirection, a deploy that silently no-ops — can sever the two
while leaving the observations intact and agreeable. And the more the result
matches what was expected, the less it invites the question.

This is the same family as *the reference was in a deployed build, not the
source* and *a 200 from an SPA is not evidence a route exists*: reading one
thing and reporting on another. The difference is that those measured the
wrong object, and this measured the right object in a state that never
received the change.

### The guard

**Make the broken build prove it is broken before trusting what it tells
you.** An experiment needs a control that fails, and the control has to be
observed, not assumed:

1. Before drawing any conclusion, confirm the artifact under test is the one
   deployed. `/version` reporting the expected commit is one command and
   settles it.
2. Rig the failure so it produces an unmistakable signal — a 503, a
   distinctive log line, a route that only the broken build serves — and check
   for that signal explicitly.
3. If the signal is absent, the run is void regardless of how well the rest
   of the result fits. **A clean result from an experiment that did not run is
   indistinguishable from a clean result from one that did**, which is exactly
   why the control has to be verified rather than inferred.

## Answering a question adjacent to the one that was asked

A browser reported "Not Secure". Three consecutive investigations were run,
each thorough, each finding something real, and none of them answering the
question.

The question was **"which hostname is in the address bar?"** What all three
answered was **"which of our hostnames could produce this symptom?"** Those are
different questions, and the second one has plenty of true answers:

1. The first swept certificates, redirects, mixed content, DNS and HSTS across
   four `grainlify.com` hosts. Everything was valid. It concluded, correctly,
   that nothing there could produce the symptom — and treated that as progress.
2. The second, prompted by a guess about Cloudflare's one-level wildcard,
   examined the `.0xo.in` names and **did** find a real hostname mismatch:
   `api.grainlify.0xo.in` presenting `CN=*.up.railway.app`. Real, worth fixing,
   and not what anybody was looking at.
3. The third re-swept data columns and bundle chunks for `http://` and found
   only XML namespace identifiers and dead literals.

Each pass ended with a defensible statement about *our* infrastructure. The
symptom belonged to a browser on somebody's desk, and no amount of correct
server-side work was going to reach it.

**What would have collapsed it immediately** is one question asked at the
start: *what is in the address bar, and what does the padlock dropdown say?*
The eventual screenshot showed `grainlify.com` with "Certificate is valid" —
which excludes certificates, redirects and DNS in a single glance, and had been
available from the first minute.

### The general form

**A symptom reported without its context invites you to enumerate causes
instead of locating one.** Enumeration feels like progress because each step
produces a genuine finding, and genuine findings are exactly what makes it hard
to notice that the search space was never narrowed. Three real discoveries in a
row is not evidence of converging on the answer; it can equally mean the
question is broad enough to keep yielding.

The tell is a search that keeps succeeding without ever excluding anything. If
finding something does not shrink the space of remaining explanations, the
question being answered is not the one that was asked.

### The guard

Before enumerating causes, **pin the observation**: which host, which URL,
which browser, which user, when. If the report is second-hand, ask for the
screenshot before searching — one image can be worth more than a day of
correct investigation. And when a sweep comes back clean, say *"nothing here
could cause it"* rather than *"no problem found"*: the first keeps the
question open, the second quietly closes it.

## A claim about a dependency's behaviour that nobody ever checked

Every other entry here is about a check that ran and quietly answered a
narrower question than the one asked. This one is different: there was no
check. A fact about how browsers behave was asserted from general knowledge,
agreed by two people, used as the justification for building something, and was
wrong.

The claim was: *"the first project whose README carries an `http://` badge will
show mixed content on its detail page."* It sounds unremarkable. Mixed content
is a real thing, `http://` badges are common in old READMEs, and the page really
does render markdown images from data fetched live from GitHub. Both of us
stated it. Neither of us measured it.

Measured, current Chrome **already auto-upgrades passive mixed content**:

```
Mixed Content: The page at 'https://…' was loaded over HTTPS, but requested an
insecure element 'http://img.shields.io/…'. This request was automatically
upgraded to HTTPS.
```

An `http://` badge therefore produces a working image if an HTTPS version
exists, and a broken image if it does not. It never produces the "Not Secure"
state the whole argument rested on. The real behaviour splits by content type,
and neither branch matches what was assumed:

| | assumed | actual |
|---|---|---|
| passive (`img`) | degrades the page to insecure | auto-upgraded |
| active (stylesheet, `fetch`, script, iframe) | — | blocked outright, never upgraded |

The work that followed was still worth doing, which is what makes this
seductive. The header was the right change for a better reason — it removes a
dependency on a browser default that varies by engine and version — and the
conclusion "no page content could have caused that padlock" only became
available once the behaviour was measured. **A justification can be wrong while
the decision it produced is right**, and if the justification is never checked,
nobody finds out which they had.

### Why this shape is hard to catch

A wrong check produces an anomaly eventually: a test that fails, a number that
disagrees. An unchecked claim produces nothing. It is not contradicted, because
it was never put in a position to be contradicted. It propagates by being
repeated - here, from one person's report into another's instruction and back -
and each repetition makes it sound more established.

The tell is a sentence about what a browser, runtime, library or platform
*does*, stated in the present tense, with no command or output behind it.
"Chrome blocks that." "React sanitises that." "The CDN caches that." Every one
of those is checkable in about a minute.

### The guard

**Before a claim about a dependency's behaviour becomes a reason to build
something, measure it on the version you actually ship against.** Not the
documentation, which describes intent and lags; not memory, which is a snapshot
of some earlier version.

For browsers specifically, that means driving a real browser and reading what it
did - the request scheme that went out, the console message, the resulting DOM
state - rather than reasoning from what the standard says should happen. The
measurement here took one script and two minutes, and it changed both the
rationale and the description of what the change does.

And when the measurement contradicts the claim, **say so in the artefact**, not
only in conversation. The commit message and PR description are where the next
person meets this, and a correction that lives only in a chat log is a
correction nobody will find.

## Creating a file and editing a file are different operations

`cat > path` does whichever the filesystem happens to allow. If the path is
free it creates; if the path is taken it truncates and replaces. The command
is identical either way, the output is identical either way — silence — and
which one happened depends on state nobody looked at.

Used to add a test file that already existed, it deleted four tests: HTML
comment stripping on one line and across several, normal markdown rendering,
and an empty-string case. The intent was "add a file". The operation performed
was "replace a file". Nothing in the transcript distinguished them.

This is the same family as **a value crossing a typed boundary must be
constructed, never spelled**. There, a string stands in for something that is
not text and the error surfaces far from the call site. Here, one command
stands in for two distinct operations and the loss surfaces far from the
command — in this case only in a test count, later, by accident.

### The guard

**Use a create that refuses an existing path.** Any of these fail loudly
instead of silently replacing:

```sh
set -o noclobber; cat > path        # errors if path exists
printf '%s' "$body" | tee path      # still clobbers - not a fix
[ -e path ] && { echo exists; exit 1; }   # explicit precondition
```

The general rule, independent of tool: **an operation whose destructiveness
depends on unexamined state is not one operation.** Split it. Either assert
the file does not exist and create it, or read it, extend it, and write it
back. Choosing between those is the work; letting the filesystem choose is
the bug.

The tell was on screen and unread. `git status` prints `A` for an added path
and `M` for a modified one, and it printed `M`. That distinction is the entire
finding, rendered in one character, and it scrolled past several times before
the arithmetic caught what it had been saying all along.

## A countable invariant catches what tooling does not

Adding ten tests should raise the suite count by exactly ten. It read 718 where
it should have read 722, and that four-test gap is the only reason the
overwrite above was found. Nothing else reported it. Not the test run, which
was green — the surviving tests passed and the new ones passed. Not typecheck,
not build, not CI, not review. Every signal available said the change was good,
because from each of their perspectives it was.

The suite was green **because the deleted tests were gone**. A test that no
longer exists cannot fail, so deleting tests improves every indicator a test
suite produces. That is the inversion worth internalising: the usual instruments
measure the tests that ran, and are structurally blind to the ones that stopped
existing.

### Why the arithmetic works when the instruments do not

A count is an invariant that spans the change. It does not care what passed; it
cares how many there are, and that quantity has an expected value known *before*
the change was made. Predict the number, then compare. A discrepancy is
information regardless of which direction it points:

- fewer than expected → something was removed, probably not deliberately
- more than expected → something was duplicated, or a helper is generating cases
- exactly as expected → the change did what was described

### The general form

**Before a change, state a quantity it should move and by how much. After,
check it.** It costs one sentence and catches a class of error that green
builds cannot, because it is the only check that notices absence.

It generalises past tests. Rows written by a migration, files in a build
output, routes registered, config keys read, endpoints in an OpenAPI document,
sections in this file. Anything countable with a predictable delta is a cheap
independent witness, and it is independent precisely because it is not derived
from the thing under test.

The habit pairs with the previous entry. One prevents the silent destruction;
the other notices when prevention failed. Neither is sufficient alone — the
count only worked here because the expected value had been stated as "ten
added" before the number was read.
## A result that passes through a transformation reports on the transformation

Three times in one session, the same line:

```sh
go build ./... 2>&1 | head -10 && echo "ALL PACKAGES BUILD"
```

It printed `ALL PACKAGES BUILD` while the build was failing. `$?` and `&&` see
the **last** command in a pipeline, which is `head`, and `head` succeeds at
printing whatever it was given — including a compiler error. The same line
reported a passing test suite over a failure, and an `exit: 0` beside an
experiment that had exited 1.

Every instance was self-inflicted tooling, not a defect in the thing under test,
which is exactly what makes it dangerous: the subject was innocent each time, so
there was nothing to investigate and no anomaly to chase. The check simply
agreed.

### The mechanism, because a rule you must remember is not a rule

The instinct is "remember not to pipe". That fails at precisely the moment it
matters, when you are three commands into a diagnosis and reaching for `head` to
keep the output short. Set the shell so the mistake cannot be made:

```sh
set -o pipefail      # a pipeline's status is the first non-zero, not the last
```

With `pipefail`, `go build … | head` returns the build's failure. Where that is
not available, capture before transforming:

```sh
go build ./... > /tmp/out.txt 2>&1; echo "exit: $?"   # status first, then read
```

`$?` must be read immediately, before any other command runs — including the
`echo` that displays it.

### The general form

**Anything that transforms a result also replaces its status.** A pipeline is
the obvious case; it is not the only one:

- a test harness that prints `SKIP` and exits 0, so the suite reports a clean
  sheet over tests that never ran
- a retry wrapper whose status is the last attempt's, hiding that the first
  three failed
- a formatter or reporter between a checker and the terminal, reporting on
  itself
- `|| true`, appended to silence noise, which converts every future failure of
  that line into a pass

The tell is a success message produced by something other than the thing being
checked. If the words "it passed" were printed by a wrapper rather than by the
tool, they describe the wrapper.

## When a check fails, the check is a suspect too

A failing check is evidence that the check and its subject disagree. Which of
them is wrong is a second question, and it is skipped almost every time, because
a red result arrives already labelled: the tool is the instrument and the code
is the thing being measured.

Four times in one session the instrument was wrong.

**A shell that rewrote the input.** Reproducing a CI step locally reported that
the deployed bundle was missing its API host. It was not. `zsh`'s builtin `echo`
interprets backslash escapes, minified JavaScript is full of them, and
`echo "$bundle" | grep` mangled the content before `grep` saw it. The same line
passes in `bash`, which is what CI runs. **The finding was the harness.**

**A flag that did not exist.** Testing a new argument check, `--confirm-host=…`
was rejected and read as the check having broken the production safety flag. The
real flag is `--yes-run-against-remote-host`. The rejection was correct
behaviour on an argument that genuinely is unrecognised.

**An assertion that fails on correct output.** A test asserted that rendered
markup contained no `src=`, to prove raw HTML was inert. Escaped text still
contains those characters — `&lt;script src="…"` — so the assertion failed
precisely because the code was doing the right thing. A sibling test searched
the source for `rehype-raw` and matched the comment *explaining why rehype-raw
is absent*.

**A truncated download.** A bundle saved with `curl -s > file` arrived at 117KB
of 575KB. The occurrence count for the API host went from 3 to 0 between two
runs, which looked exactly like production having changed under the
investigation. Nothing had changed; the file was short.

### Why it costs more than other traps

A wrong check that **passes** is caught eventually — by a mutation test, by the
bug it failed to stop. A wrong check that **fails** sends you into the subject,
which is innocent, so every hypothesis you form there is false and every
experiment confirms nothing. You cannot find a defect that is not present, and
the search does not terminate on its own. This is also the shape of *an
intermittent failure invites you to blame the environment*, seen from the other
side: there the environment was accused and the fixtures were guilty; here the
code is accused and the tool is. And *a tool that corrupted the source it was
checking* is one mechanism of this entry rather than a separate fault: there the
harness damaged its subject, so the damage presented as a bug in the code. Kept
separate because that one has a specific fix — back up by glob — while this is
the stance that finds it.

### The guard

**Before believing a red result about the subject, establish that the tool is
asking the question you think it is.** Three cheap moves, in order of how often
they settle it:

1. **Run the check against something known-good and known-bad.** A check that
   cannot pass is broken; a check that cannot fail measures nothing. Both are
   one command.
2. **Read what the check actually received**, not what you passed it. Print the
   length, the first bytes, the resolved argument. The four cases above were all
   visible at this step: a mangled string, an unknown flag, escaped text, a
   short file.
3. **Reproduce in the environment the check really runs in.** A local shell is
   not CI, and a headless browser with no extensions is not the reporter's
   browser.

The habit to build is narrow: **say "the check and the code disagree" rather
than "the code is broken"**, and hold both as suspects until one is excluded.
The sentence is the whole guard — it keeps a second hypothesis alive at the only
moment it is cheap to test.

## An assertion made in a layer that cannot model the defect

A notification body was clipped with `line-clamp-2`, so contributors could not
read the sentence telling them what to fix. The obvious test is:

```tsx
expect(await screen.findByText(LONG_BODY)).toBeInTheDocument()
```

**That passes with the bug present.** `line-clamp` is CSS. jsdom implements the
DOM but not layout — no boxes, no line breaking, no overflow — so the full
string is in the document either way. The assertion is true, the component is
broken, and nothing in the test output hints at the gap.

This is why it shipped. A test existed nearby, the text was clearly present, and
the check that would have caught it is not one anybody reaches for.

### Proven, not argued

The claim was not left as reasoning. A text-only test was written against the
clamped component and **passed**; the clamp was then removed and it passed
identically. A test with the same result in both states is measuring something
other than the thing under test.

The assertion that does work is structural, on the classes that do the clipping:

```tsx
expect(body.className).not.toMatch(/line-clamp|truncate|text-ellipsis/)
```

More brittle than a text assertion, and the only kind available in this layer.
Mutation-confirmed: restoring `line-clamp-2` fails it.

### The general form

**Ask which layer the defect lives in, and whether the assertion can see that
layer at all.** Test and defect must inhabit the same layer; when they do not,
the test is not weak, it is blind — it will pass at full confidence forever.

It generalises well past CSS:

| Defect lives in | Blind to it |
|---|---|
| Layout, paint, stacking | jsdom — no layout engine |
| A missing database index | a unit test — correctness is unchanged, only speed |
| A wrong endpoint or verb | a mocked client — the mock answers whatever it is asked |
| Schema drift | an in-memory fake — it has no schema |
| A wrong `Content-Type` | an assertion on the parsed body |
| Migration ordering | a test against an already-migrated database |
| A CDN rewrite | a request that never leaves the process |

Each of those is a real assertion, correctly written, that cannot fail for the
reason you care about. The mock case is the most common and the most seductive:
a mocked client makes the endpoint unfalsifiable, because the mock will answer a
wrong URL exactly as readily as a right one.

### The check

Before trusting a green test as evidence about a specific defect, ask: **if the
defect were present, would this assertion change?** If it would not, the test is
not covering it, whatever its name says. Two cheap ways to answer:

1. **Introduce the defect and watch the test fail.** A mutation is the only
   direct evidence that an assertion can see what it claims to.
2. **Name the layer out loud.** "This runs in jsdom, which has no layout" ends
   the question in one sentence, and it is the sentence nobody says.

When the right layer is unavailable — no browser in CI, no real database in a
unit suite — say so in the test rather than substituting an assertion from the
wrong layer and letting its green stand in for coverage it does not provide.

## Is the fix as general as the mistake?

`?tab=` was read once, in a `useState` initialiser, and never again. An in-app
link changed the URL and the page reverted, so every notification link followed
from inside the dashboard went nowhere. That was found, understood and fixed.

`project`, `issue`, `view` and `from` sat in the same file, read the same way,
in initialisers a few lines apart, and were left exactly as they were.

Three of eight notification types still landed on the wrong screen for another
day. The repair was reported as complete because the reported symptom stopped.

### Why the siblings are invisible

The bug arrives named after one member. "See all notifications doesn't work" is
about `tab`, so `tab` is what gets examined, fixed, and tested. Nothing in that
loop ever asks what else is built this way — the report scoped the search, and
the fix satisfied the report.

It is worse than an ordinary miss, because fixing one member **removes the
evidence**. The next person sees a file where `tab` demonstrably syncs from the
URL and concludes that URL syncing works here. The remaining four look like
they must be fine, since the mechanism plainly exists.

### The check

**Ask whether the fix is as general as the mistake.** Not "is the bug fixed" —
"is the *class* fixed". Concretely, when a defect is understood well enough to
repair:

1. Name the mechanism, not the symptom. Not "the tab didn't update" but "state
   initialised from the URL is never re-read after mount".
2. Grep for the mechanism. `useState(() => ...location.search...)` finds all
   ten in one search.
3. Fix or explicitly exempt every one, in the same change.

Step 3 is the one that gets skipped, and "explicitly exempt" is a real option —
some siblings genuinely differ. What is not an option is not looking.

### The same shape, three times in one day

- A rule written for one caller while its siblings kept the old behaviour.
- A guard proven for one parameter and assumed for the rest.
- This.

Each was found by exhaustion rather than by reasoning: following all eight
notification links rather than inspecting the one that was reported. **Five of
the eight worked and all five carried only `tab` and `subtab`** — the
diagnosis, in one line, available only because all eight were followed.

## A default parameter that swallows its own sentinel

```ts
export function claimReload(storage: Storage = safeSession()): boolean {
  if (!storage) return false;   // unreachable
  ...
}
```

The guard's most important behaviour is what it does when it **cannot** record
its flag: it must refuse, because without a durable flag there is no way to
prevent a reload loop. There is a line for exactly that, and a test for it.

The test could not reach the line. `claimReload(undefined)` does not pass
`undefined` — a default parameter value is substituted whenever the argument is
`undefined`, so the call resolves to the real `sessionStorage` and returns
`true`. The case the function was written to handle **was not expressible by
its caller.**

### Why review does not catch it

Because the default is obviously the right default. Every part reads correctly:
the fallback is sensible, the guard clause is present, the intent is legible.
The defect is not in any line but in the *arity* — in what the type permits a
caller to say. `Storage` cannot express "deliberately none", and the default
quietly converts the only spelling a caller would reach for into its opposite.

It is the value-shaped traps in reverse. Those are about a value arriving in a
shape nothing checked. This is about a value that **cannot arrive at all**, and
so a branch that can never be exercised, in the branch that matters most.

### The check

**For every optional parameter, ask what "deliberately none" looks like, and
whether the type can say it.**

If the answer is "you'd pass `undefined`", the default has swallowed it. Split
the two meanings so absence and emptiness are different values:

```ts
export function claimReload(storage?: Storage | null): boolean {
  const store = storage === undefined ? safeSession() : (storage ?? undefined);
  if (!store) return false;   // now reachable, and tested
  ...
}
```

`null` says none. Omitting the argument still gets the real one.

The tell, and it is worth trusting: **a test you cannot write for a branch you
can see** is not a gap in the test, it is a report about the signature.

## Fail toward acting; refuse only on evidence

A guard that suppresses a recovery must do so on evidence of a bad state, never
on the absence of evidence of a good one.

The stale-chunk reload is skipped when `navigator.onLine` is `false` — a
browser that knows it has no network will not fetch on reload either, so
reloading could only replace an explanation with a blank page. But the check
defaults to *online* whenever it cannot be read:

```ts
return typeof navigator === 'undefined' || navigator.onLine !== false;
```

Not `navigator?.onLine === true`. The difference is the whole point. Requiring
proof of connectivity would disable recovery in every environment that does not
implement `navigator.onLine`, to prevent a reload in a state we never
established. The condition names the bad state and treats everything else as
permission to proceed.

That asymmetry is the difference between a guard and an obstacle, and it
generalises past this one flag: a precondition phrased as "proceed only if I
can confirm things are fine" fails closed on every unknown, and unknowns are
the common case. Phrase it as "stop if I can see something wrong."

It also composes with the rule already recorded here — that a guard must not be
able to cause a larger failure than the one it prevents. A guard which fails
closed on unknowns is exactly how that happens.

## A mutation that lands on the wrong line reports the wrong result

Mutation testing the URL reader, two of five mutations survived, which should
mean two properties had no test behind them. One did. The other was a lie.

```
const issueId = params.get("issue");     // appears TWICE in the file
```

Once in the `useState` initialiser, once in the reader under test. The harness
did `replace(old, new, 1)` and hit the initialiser — code the filtered test
never exercises. Nothing broke, so the run reported **"test still passed"**,
which reads as "this property is untested" and invites writing a test that
already exists.

Both failure directions are available here and they mislead differently. An
unapplied mutation reports a good test as useless. A mutation applied in the
wrong place reports a good test as useless *and* points at innocent code.

### The check

Assert the anchor is **unique** before mutating, and abort when it is not:

```
n = source.count(anchor)
if n != 1: abort(f"anchor occurs {n} times - refusing to guess which")
```

Refusing is right, rather than picking the first or mutating all. A harness
that guesses produces results indistinguishable from real ones.

This belongs with the existing rule that a mutation must be confirmed **applied
and compiled** before its result is trusted. Applied, compiled, and *in the
right place* — three conditions, and the third is the one a diff-free check
cannot see, because the file did change.

## A panic reports zero skips on its way out

A test run reported **RUN 634 · PASS 628 · FAIL 6 · SKIP 0**. The six failures
were understood and expected. `SKIP 0` was read as "and nothing was quietly
declined" — which is exactly what it usually means.

Forty-five of the package's 389 tests had not run.

One of the six failures was not an assertion failure. An admin route returned
403, the list came back empty, and the test indexed the first row of it:

```
rows := listRows(t, first)
if len(rows) != pageSize {
    t.Errorf(...)          // records the wrong length and CONTINUES
}
...
firstID := rows[0].(map[string]any)["id"]   // panic: index out of range
```

A panic kills the test binary. Every test after that point in the package never
ran, and `go test` does not count them — not as failures, not as skips, not at
all. They are absent from the denominator, so the totals stay internally
consistent and nothing looks wrong.

### Why this is worse than a skip

A skipped test reports itself. `dbtest.DB` skipping on an unset `TEST_DB_URL`
is a weak signal, but it is a signal: the count goes up and someone can notice
it. A panic produces the opposite — `SKIP 0` is not a missing signal, it is a
**false reassurance**, and it is loudest precisely when the most tests are
missing.

Worse, the loss is silent about *what* was lost. Among the 45 were the only two
tests covering a guard the change under review had just rewritten. The run
looked like evidence for that change and contained none.

### The check

Two things, neither of which is "read the output more carefully":

**Assert before you index.** Any `x[0]` on something derived from a response
body needs a length assertion above it, and the assertion must be `t.Fatalf`,
not `t.Errorf` — `t.Errorf` records and continues, straight into the panic.
Sweeping the four database-backed packages for this found one unguarded site
and thirteen already correct, so the convention exists; it was one omission,
and one is enough.

**Reconcile the count, do not read it.** The honest check is arithmetic:

```
declared=$(grep -rhoE '^func Test[A-Za-z0-9_]+' *_test.go | sort -u | wc -l)
ran=$(grep -cE '^=== RUN   Test[A-Za-z0-9_]+$' run.txt)
```

If those disagree, the run is not a result. This is the same shape as the
countable invariant elsewhere in this file: the totals a tool reports about
itself cannot detect the case where the tool stopped early, because the tool
computed them from what it got to.

### The general form

**A count of what was declined is only trustworthy from a process that lived
long enough to decline it.** Zero skips, zero errors, zero warnings — each is
evidence of absence only if the reporter reached the end. Prefer a check that
compares against something known *outside* the run: how many tests exist in the
source, how many rows should be in the table, how many files should have been
written. A run that dies mid-way will report a clean sweep of the part it
reached, and there is nothing in its own output that distinguishes that from a
clean sweep of everything.

## A test that keeps passing for a different reason

A guard was safe because the bad state was unreachable. `KYCAdminHandler`'s
reset nulls `kyc_session_id`, and the reconciler queues on
`WHERE kyc_session_id IS NOT NULL`, so a reset contributor left the queue
entirely. The test asserting "the reconciler never undoes a reset" passed
because the session was **never queried at all**.

Then a change was proposed that keeps the session alive. Under it the row is
back in the queue, the reconciler reads a still-`Approved` session, and writes
the contributor back to verified — silently reversing an admin's decision.

The test still passes.

Not because the guard survived. Because the assertion — final status is
`expired` — is also true when the reconcile is skipped, when the fixture never
loaded, and when the code path is dead. It was written against a world where
one thing made it true, and it now reports on a different world without
saying so.

### Why no signal catches this

Every instrument a suite has points the wrong way:

- **The test passes.** No failure, no flake, nothing to investigate.
- **Coverage goes up, not down.** The new conditional is executed by this very
  test. A line-coverage report shows the guard covered.
- **The diff looks complete.** Implementation changed, test still green — which
  is exactly what a correct refactor looks like.
- **Mutation testing may not catch it either**, if the mutant is killed by a
  different test that happens to overlap.

A test that fails when it should pass is loud. A test that passes when it
should fail is silent. A test that passes *for a reason that no longer exists*
is worse than either: it is a green tick actively certifying a property nobody
is checking.

### The check: assert the reason, then assert the inverse

Two things, and the second is the one that is usually missing.

**1. When the implementation changes, the test must be rewritten, not re-run.**
If it goes green without being touched, that is not reassurance — it is the
symptom. Ask what makes the assertion true *now*, and if the answer is
different from what made it true before, the test has stopped testing what its
name says.

Where the mechanism is cheap to observe, assert it directly rather than only
its consequence:

```go
if f.calls[session] != 0 {
    t.Errorf("the reset session was queried %d times; it must leave the queue entirely", ...)
}
```

That line fails the moment the impossibility becomes a conditional, because it
asserts *why* rather than *what*.

**2. Add the inverse case.** A guard that simply never fires is
indistinguishable from a guard that fires correctly, unless something proves it
can be *not* triggered. For "a reset newer than the decision wins", the missing
test is: reset, then a **newer** decision, and assert the reconciler **does**
take it.

Without the inverse, `return false` passes the whole suite.

### The general form

**A structural impossibility being converted into a conditional is not a
refactor, and it does not look like one in a diff.** When the reason a bad
outcome cannot happen moves from the shape of the data into a line of code that
has to be right, every test covering it needs re-deriving, because they were
all passing on the strength of the old reason.

Say that sentence out loud in the review. It is the only thing that reliably
stops the change being read as a tidy-up.

## A structural check that enumerates its own inputs

`TestKYCVerifiedAt_EveryWriterGuardsTheTransition` reads the handler sources and
asserts that every write of `kyc_verified_at` is guarded by the transition
check. Its own comment states the promise:

> It fails if a third writer appears, or if either existing one reverts — the
> case the behavioural tests above cannot see.

It could not fail if a third writer appeared. The list was written by hand:

```go
files := []string{"kyc.go", "didit_webhook.go"}
```

A third writer arrived in `kyc_status_reconciler.go`, carrying its own copy of
the `CASE` block. The test never read that file, because a file nobody adds to
the slice is a file it cannot see. The count stayed at **2**, matched
`wantWriters`, and read as correct for as long as it existed.

The check was not weakened. It was **never covering what it claimed to cover**,
and its own success was the evidence offered for that claim.

### Why the count made it worse

The `wantWriters` constant is the good idea in that test — the thing that turns
"all the ones I looked at are guarded" into "and there are exactly this many."
It is the same countable invariant that appears elsewhere in this file, and it
is why the drift is supposed to be impossible.

But a count over a hand-written list counts *the list*, not the codebase. Both
halves — the guard and the count — were computed from the same incomplete input,
so they agreed with each other perfectly and with reality not at all. **Two
checks derived from one wrong premise do not corroborate; they repeat.**

### The check

**Glob, do not enumerate.**

```go
entries, _ := filepath.Glob("*.go")            // every file, not a list of files
for _, name := range entries {
    if strings.HasSuffix(name, "_test.go") { continue }
    ...
}
if len(files) < 10 {
    t.Fatalf("globbed only %d source files; the scan is not running where it thinks it is", len(files))
}
```

The floor assertion matters as much as the glob. A glob that silently returns
nothing — wrong working directory, wrong pattern — produces zero findings, which
is indistinguishable from a clean codebase. Assert the *input* is plausible
before trusting the *output*, because a scan of nothing passes every check you
can write about its results.

### The general form

**Any check whose inputs are listed by hand is a check on that list.** It will
find what somebody remembered to give it and report the result as if it had
looked everywhere. The failure is silent, it survives review — the list looks
deliberate — and it gets *more* convincing over time as the list ages into
something nobody questions.

Third instance of this shape recorded here: the mutation harness that mutated
one file from a fixed set, this guard, and the deferred-tool sweep that scanned
a directory it had been handed rather than the tree. Where a check can derive
its own inputs, it must; where it genuinely cannot, the list needs an assertion
about its own size, so that shrinking it is a failure rather than a quieter run.

## An undo whose blast radius exceeds the change

Two incidents, one shape, both self-inflicted while verifying something else.

**One — `git stash`, proving a new test could fail.** The right instinct: revert
the implementation, keep the test, confirm it goes red. The command was
`git stash`, which took *both* — the test was uncommitted too. The run reported
`Tests 3 passed`, and 3 is what passes when the new test is not there at all.
The check proved nothing and looked like it had proved everything.

**Two — `git checkout -- <file>`, undoing one added line.** A line had been added
to a document to confirm a new check caught it. `git checkout --` restores the
file to HEAD, so it discarded that line *and* every correction made to the file
in the preceding hour. The command printed nothing, which is what it prints on
success.

### The part that is not "be careful with git"

Both commands **report success identically whether they undid one thing or
forty.** There is no output that distinguishes the intended scope from the
actual one. `git stash` says nothing about what it swept; `git checkout --` says
nothing at all.

So the loss is not discoverable at the moment it happens. It is discoverable at
the **next check that reads the file** — and only if there is one. In the first
case that was the test count, noticed because 3 was a suspicious number. In the
second it was a grep for a string that should have been present. Both were
noticed by accident, one step later, and either could as easily have been
noticed by nobody: a stash that hides a test produces a green run, and a revert
that removes an hour of edits produces a document that still parses.

### The check

**Verify the undo, not the flags.** Before trusting any result that depended on
reverting something, assert the revert did what you meant:

```sh
git status --porcelain          # what is actually modified now
git stash list                  # did that sweep more than intended
```

And prefer an undo whose scope is stated rather than implied. Copying the one
file aside and back is uglier than `git checkout --` and cannot take anything
with it:

```sh
cp target.go /tmp/keep && git checkout -- target.go   # ... test ...
cp /tmp/keep target.go
```

The general rule is about **when** you verify rather than which command you
type: a destructive step performed *in service of* a check must be confirmed
before the check's result is believed, because the check itself will not notice
that its inputs were removed. A test run does not know its test is missing. A
document does not know a paragraph is gone.

### Why this belongs with the rest of this file

It is the same failure as the panic reporting `SKIP 0` and the guard counting a
hand-written list: **an absence produced by the tooling, reported as a normal
result.** Three tests passing is a real number. A clean `git status` is a real
state. Neither says "and something you wrote is no longer here", because nothing
in the pipeline was asked to compare against what you expected to still exist.

## A test that constructs a state its own system cannot produce

`settlement_lines.excluded_reason` was read in two places and written in none.
`ExclusionsFor` filters `WHERE excluded_reason IS NOT NULL`, so it returned zero
rows always; `/me/payout-readiness` could never answer `excluded_from_published`;
and a UI state built for that answer was unreachable in production.

Every test covering it passed, because each test **inserted the row itself**.

That is the shape: a fixture writes a value no code path writes, the assertions
against it hold, and the suite reports a working feature. The test is not wrong
about what the code does with the state — it is wrong that the state occurs.

### Why this survives review

A fixture that sets up its own preconditions is *correct practice*. Nothing in
the test looks suspicious: it inserts a row, calls the code, asserts the result.
The defect is not in what the test contains but in what the system does not, and
no amount of reading the test file reveals it. You have to go and ask who writes
the column, which is a question a passing test actively discourages.

It is also the most convincing kind of green. A feature with tests reads as more
finished than one without, so the tests make the gap *harder* to find than if
they had never been written.

### The check

**For any state a test constructs by hand, ask what in production produces it.**
If the answer is a code path, name it in the fixture. If there is no answer, that
is the bug, and it is upstream of everything the test asserts.

Mechanically, for a column: grep for writes, not for uses.

```sh
grep -rn "excluded_reason" --include='*.go' . | grep -iE "insert|update|set "
```

Empty output against a column with readers is the finding. The same question for
an enum value, a status string, or an error code: **who emits it?**

### The general form

**A test proves the code handles a state. It does not prove the state exists.**
Those are different claims, and only the first is checked by running the suite -
which means the second has to be established deliberately, once, by looking.

Third instance of this family recorded here, all found in one day: the harness
button asserted against a control the code could not reach, the writer-guard
counted a hand-written list rather than the tree, and this. The common root is
that a check derives its own subject from something the author supplied instead
of from the system, so it measures the author's belief rather than the code.

## A comment is a weak guard even against its own author

Migration 086 made one live payout address belong to one account, and its own
comment says what that does to fixtures:

> Fixtures used to hand the same constant to several people, which migration 086
> now forbids: one live address belongs to one account. That is the index doing
> its job, and the fixture was relying on something the system no longer permits.

A fixture in a second package went on using two fixed address constants. The
tests passed against a fresh database and collided permanently on any database
where a run had failed before its cleanup registered — the exact hazard, in the
exact form, written down by somebody who had just fixed it elsewhere.

The warning existed. It was in the repository. It had been read — the fix it
describes was applied in one package the same day. And the second package was
not checked, because a comment records a hazard where the hazard *was*, not
where the reader is.

### The stronger claim

The usual lesson is "comments do not enforce anything", which invites the reply
that a careful reader will still act on them. This is worse than that:

**The person who documents a hazard is not reliably guarded by their own
documentation.** Writing it down feels like discharging it. The note is
addressed to a future stranger, and the author is not who they are picturing
when they write it.

So a comment is a good explanation and a poor control, *including* for the
person who wrote it, minutes later, in a file they did not think of as related.

### The check

Where a hazard is enumerable, ask the system rather than the reader:

```sql
-- every column a repeated fixture value could collide on
SELECT a.attname FROM pg_index i ... WHERE i.indisunique
```

Then sweep the tree against that set. Thirty-eight columns and one glob answers
"does this hazard exist anywhere else" in a way that no number of careful
readers does — and it answers it for packages nobody thought to look at, which
is where the second instance was.

Keep the comment. It explains *why* the check exists, which the check cannot say
for itself. Just do not let writing it feel like having handled it.

## An invariant whose two sides share an upstream decision

A money-path assertion: when the whole pool is allocated, the residue must equal
the total owed to excluded members.

```
residue   = pool - leafTotal            // one route
excluded  = Σ amount of owed members    // another route
assert residue == excluded
```

Two figures, computed differently, meeting. It reads as a check that held money
was not paid to somebody else — and that is how the comment above it was
written.

It is not. Reclassifying a held member as payable moves the member's amount
*into* the leaf total and *out of* the excluded total in the same step: residue
falls by exactly what excluded falls by, both reach zero, and the equality still
holds. The assertion survives the precise failure its comment claimed it caught.

### Why it looked like it covered more

**Both sides are computed downstream of the classification.** An error in the
classification propagates into both, in the same direction, by the same amount.
The invariant is testing the two *routes* from a decision, not the decision.

And it is convincing because the thing it genuinely catches — money
double-counted, or belonging to no bucket — is the thing you would naturally
describe it as checking. "The totals reconcile" is exactly the sentence somebody
reaches for when asked whether the money is right, which is what makes this
shape worse on a money path than anywhere else.

### The failure is in the comment, not the code

This is what makes it a class rather than an anecdote.

The assertion is correct, useful, and worth keeping. Nothing about it needs to
change. **The defect is the next person's belief about what is covered**, and no
test run can surface that: the suite is green, the invariant holds, and the
sentence above it is false. It survives review because a reviewer checks whether
the assertion is true, not whether the claim about it is.

### The check

**When you write down what an invariant proves, mutate the thing you just said
it catches.** If it survives, the sentence is wrong, not the assertion.

That is the only way this is findable. It was found exactly that way here:
disabling the classification branch, expecting the identity to fail, and
watching it pass.

The habit generalises past invariants. Any sentence of the form "this catches X"
is a testable claim, and mutating X is how you test it. A comment that has never
been mutated against its own claim is a hypothesis written in the indicative.

### The repair, when the sentence is wrong

Narrow the claim rather than widen the assertion. Here the comment now says the
identity proves the held money is exactly the money not in the tree, states
plainly that it does **not** establish the classification, and names the tests
that do. A guard with an honest scope is worth more than one with an
aspirational one, because the honest scope tells the next person what still
needs covering.

## A comment describing machinery that does not name it

```go
// Reachable by design rather than only by corruption: account deletion
// leaves an anonymised tombstone - the users row is retained with
// identifying columns nulled and github_accounts is hard deleted along
// with its token
```

There is no account deletion. No route, no handler, nothing that nulls those
columns. The sentence describes a mechanism that has never existed.

Two people read it in one day and both concluded the path was there. One scoped
work that depended on wiring into it. The absence was found only by going to
look for the function to call.

### Why this is a different failure from the comment-as-guard entry

Those comments describe **hazards**, and fail because the author does not heed
their own warning. These describe **mechanisms**, and fail because *the reader
cannot tell an aspiration from a fact.*

Prose about machinery has no failure mode. It does not go red, it does not stop
compiling, and it reads exactly the same whether the machinery was built,
planned, removed, or renamed. It ages into documentation by sitting still.

Worse, it actively hides the gap it creates. The absence of account deletion was
harder to notice *because* something in the codebase described it — a search for
"account deletion" returns a confident paragraph, which is what somebody
checking would find and stop at.

### The rule

**A comment describing machinery must name it: a function, a file, or a route.**

```go
// bad   - unfalsifiable prose that reads as documentation
// account deletion leaves an anonymised tombstone

// good  - a claim that fails a grep the day it stops being true
// DeleteAccount in handlers/account.go leaves an anonymised tombstone
```

The named version can be checked in five seconds and **dies honestly**: rename
the function, delete the file, never write it, and the comment is visibly wrong
to the next person who looks. The unnamed version survives all four.

This is the same move as globbing rather than enumerating, pointed at prose: tie
the claim to something the system can contradict.

### The check

For any comment asserting behaviour exists elsewhere, grep the identifier it
names. If it names none, that is the finding — not because the comment is
necessarily wrong, but because **nothing will ever tell you when it becomes
wrong.**

Statements about intent, rationale and consequence need no identifier; they are
not claims about code. It is specifically the sentence of the form *"X happens
over there"* that has to say where.

## A demonstrated capability read as a shipped one

A sponsored claim landed on Aptos testnet on 18 August: a real transaction, a
real `fee_payer_signature`, `sequence_number: 0`, the claimant's first-ever
transaction, full amount received, nothing paid. It was the headline result of
the milestone.

Nothing in production implements it. There is no fee-payer service, no sponsor
account handling, and no endpoint that co-signs a claim. A contributor clicking
Claim pays their own gas.

Two people built on the belief that sponsorship was live. One wrote specimen
claim-screen copy promising "we cover the network cost". The other justified a
fail-open Claim button partly on the grounds that gas was sponsored.

### Nothing anywhere was false

This is what separates it from every other entry in this file. There is no
incorrect statement to find:

- The milestone document describes the sponsored claim **accurately**. It
  happened, and the transaction hash resolves.
- Sponsorship is a **transaction-layer** concern, and it was described as one.
  The on-chain module neither knows nor cares who pays the fee, so nothing in
  the contract's documentation is wrong either.
- The self-paid path is the **absence of a restriction**, not a decision. Nobody
  wrote "contributors pay their own gas", because nobody chose it — it is simply
  what happens when no fee payer is attached.

So every document stayed true, and a false conclusion was available to every
reader. The gap was not in what was written but in what nobody thought to write:
**that the transaction layer had no production implementation.** Absence has no
natural home in a document about a presence.

### Why "it works" is the most dangerous phrase in a milestone

A demonstration answers *can this happen*. A deploy answers *does this happen*.
The two are reported in the same words, celebrated in the same message, and the
first is very often produced by tooling that will not exist on the real path — a
script, a local signer, a hand-built transaction, a key on somebody's laptop.

**A milestone proves a thing CAN happen. Only a deploy proves it DOES.**

### The check

When a capability is demonstrated, record the demonstration and the deployment
state **in the same sentence**, because they will otherwise be read as one fact:

```
Sponsored claim: WORKING on testnet (0xe33e61b8…), via a local signer.
NOT DEPLOYED - no production fee payer exists. See #536.
```

And for anything a demonstration relies on, ask what carried it: if the answer is
a script, a laptop key, or a hand-assembled call, that thing is the gap, and it
is invisible from the result.

The general habit: **for any capability you believe the system has, name the
code path that provides it in production.** If you cannot, you have read a
demonstration as a deployment — which is the same failure as the
machinery-comment entry, arriving from the opposite direction. There, prose
described machinery that did not exist. Here, machinery existed and its
production absence was described by nobody.

## Copy that is true now and false later, with no expiry attached

Two sentences about the same fact, written weeks apart:

> We cover the network cost, so the full 12.50 arrives.

> You'll pay a small network fee in APT from this wallet.

The first was written when a sponsored claim had just been demonstrated. It was
never true of production, and nothing about it said when it would become true or
whether it already was. It sat in a flow specification and was read as current.

The second is true today and becomes false the day a fee payer deploys.

### The problem is not that copy goes stale

Everything goes stale. The problem is that **a sentence with no stated expiry
cannot be distinguished from one that is permanently true**, so nobody knows
whether to check it, and the check has no trigger.

A wrong sentence about behaviour is not usually found by re-reading — it is found
by somebody acting on it. That is a long feedback loop, and on a money path it is
somebody's money.

### The device

Give any such sentence its **removal condition**, beside it, naming the thing
that will make it false:

```
{/* ── REMOVE WHEN Grainlify-Backend#536 SHIPS ──────────────────────
    True today and false the day a fee payer is deployed.
    When #536 lands, DELETE this paragraph; do not edit it into
    "we cover the network cost" - that sentence has its own home and
    its own removal condition there.
    ─────────────────────────────────────────────────────────────── */}
```

Three parts, and each is doing work:

1. **The condition, named as an issue.** A tracked thing that closes, so the
   expiry has an event rather than a date somebody has to remember.
2. **Delete, not edit.** Editing invites the replacement to inherit the position
   without inheriting the scrutiny — which is precisely how a promise about
   money ends up in a screen nobody re-read.
3. **Why the condition lives here** rather than in the issue alone. The issue is
   read by whoever picks up the work; the copy is read by whoever touches the
   file for an unrelated reason, and that is the person who would otherwise
   preserve it.

Where a test can hold the sentence, pair one with it and mark both for deletion
together. Then removing the copy is a deliberate act with a failing test behind
it, rather than a tidy-up somebody does or forgets.

### The general form

**Any statement that is true because of a current state should name the state.**
Not "we cover the network cost" but "we cover it, since #536 shipped"; not "the
claimant pays" but "the claimant pays until #536 ships".

The version without the condition is not more concise. It is the same sentence
with the expiry deleted, and the deletion is invisible.

## A forbidden-phrase list matches the sentence denying the thing

A test asserted that no claim-deadline message states a date on which something
happens, because nothing does — the sweep needs an admin signer, not a clock. It
banned a list of phrases, one of which was `"automatically on"`.

It failed on the copy that was already right:

> After the window closes we may return unclaimed payouts to Grainlify. **Nothing
> happens automatically on that date**, and if you are late you can ask us to
> extend it.

The sentence denies the event. The substring cannot see that.

### Why this is worth a line

**Negation is invisible to a substring**, so a forbidden-phrase list flags the
text most carefully written to avoid the thing it is banning. The better the copy,
the more likely it names the hazard in order to deny it — so the check is
biased against exactly the sentences you want.

The failure mode is not the false positive. It is what the false positive
provokes: the quickest fix is to reword the *copy* until the test passes, which
means a test with a bad rule silently edits the product. Here that would have
removed the sentence telling people the date is not a guillotine.

### The general form

**A phrase is only bannable if it asserts the thing regardless of what precedes
it.**

```go
// bad  - "Nothing happens automatically on that date" trips this
"automatically on"

// good - assertions whatever comes before them
"will be returned on"
"is returned on"
"funds are returned on"
```

Same shape as the enumerated-inputs entry, pointed at language rather than
files: the check tests the strings someone thought of, not the property. Where
the property matters more than the phrasing — and in copy it usually does — the
honest options are to assert the *presence* of the true sentence rather than the
absence of false ones, or to accept that the list is a smoke alarm and read the
copy.

When such a test fails, the first question is whether the copy is wrong or the
rule is. It was the rule both times it happened here.

## A queue maintained by what is changing drops what has stopped

A status report listed the open pull requests at the end of a long session. It
was accurate about every branch touched that hour and silent about one that had
been green and unmerged since the morning — the oldest item, with no
dependencies, and the only one that could have merged at any point.

Nothing was wrong with the list. It was assembled the way such lists are
assembled: from what had just moved.

### Why the omission selects for the worst item

An item drops off a working set when it stops generating events. So the thing a
change-driven list forgets is, by construction, **the thing that has been
quietly ready the longest** — no failures, no conflicts, no notifications, and
therefore nothing to put it back in view.

The list is not merely incomplete. Its blind spot is aimed at the item with the
lowest cost to finish and the longest time already spent waiting.

### The check

**Enumerate from the system, not from memory.** The queue is not what you recall
touching; it is what the system says is open:

```sh
gh pr list --state open --json number,title,updatedAt \
  --jq 'sort_by(.updatedAt)|.[]|"\(.updatedAt[0:10]) #\(.number) \(.title)"'
```

Sorted **oldest first**, deliberately: newest-first reproduces the same bias the
report had, because recency is what put the other items in mind already.

### The general form

Same family as globbing rather than enumerating and as counting declared tests
against executed ones — a check whose inputs come from the author's attention
covers what the author attended to. Here the input was a working memory of a
session, and the property being reported on — *is anything outstanding* — is
precisely the one that memory is worst at, because an item outstanding for long
enough stops being remembered as outstanding at all.
