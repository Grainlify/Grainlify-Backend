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
