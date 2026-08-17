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
