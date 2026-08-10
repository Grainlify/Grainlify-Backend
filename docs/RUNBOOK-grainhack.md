# Running a GrainHack, start to finish

Written from the code, not from the spec — every gate below is one the
software actually enforces, and the error messages quoted are the ones an
admin will really see.

**Nobody has run an event yet.** Do the first one at a $0 prize pool on repos
you control. Several steps here have never executed against real data.

---

## Before you start: four things that are not code

1. **The calibration set (§8).** 30–50 hand-labelled PRs. The judging prompt
   ships with few-shot examples explicitly marked as unvalidated placeholders.
   Until this exists, keep `ai_judging_enabled` off and judge by hand.
2. **A $0 dry run** on your own repos.
3. **Freeze and publish the draw weights.** They render at `/grainhack/rules`
   from live config — publish before applications open, not after.
4. **Decide the maintainer constants.** `maintainer_first_timers_reference`
   defaults to 200 and was tuned against six repos, three of which belong to
   one org. Sanity-check it against whoever actually enters.

---

## Phase 0 — Draft

Create the hackathon. Set name, `announced_at`, the application window,
`issue_prep_start`, `starts_at`, `ends_at`, and both prize pools.

**Decisions here:**

- **Contributor and maintainer pools are separate budget lines and can never
  draw on each other.** Set both now; a zero maintainer pool simply pays no
  maintainers.
- **Shadow mode.** `judging_shadow_mode` defaults to `true` and should stay
  true for event one: judging runs, produces verdicts, and pays nothing.
- **AI flags.** `ai_judging_enabled` and `ai_fit_assessment_enabled` default
  to false and should stay false until the calibration set exists.

Not visible publicly.

---

## Phase 1 — Project applications

Advance when the announcement and application dates are set. Projects apply;
you accept or reject each one.

**Decisions here:**

- **Reject and request-more-info both require a written reason.** It is shown
  to the applicant.
- **`maintainer_min_repo_age_days`** blocks repos with no history. An admin
  override requires a written reason and writes an audit row.

---

## Phase 2 — Issue preparation

Accepted projects add issues by applying the GrainHack label. Issues sync in
as `pending` and auto-publish once acceptance criteria and difficulty tier are
both set.

**Decisions here:**

- **`max_issues_per_org`** caps intake. The over-cap issue is rejected with a
  bot comment and a notification.
- **`auto_revert_oob_assignment`** decides whether Grainlify removes
  assignees maintainers add directly on GitHub and comments explaining why.
  Turning it off stops the writes to their repo; the out-of-band assignment is
  still recorded either way.

---

## Phase 3 — Live

**The entire config is snapshotted onto the hackathon at this transition.**
From here the event reads its snapshot, not live global config, so editing
global defaults cannot change a running event. Check the rules page now — it
serves the frozen snapshot for a live hackathon, and that is what contributors
read to decide whether to trust the event.

Application windows open per issue, draws run on the reconciler tick,
contributors submit PRs, maintainers review and merge.

**Decisions here:**

- **Nothing routine.** Draws run automatically. Use the admin "simulate draw"
  action to see ticket counts before a real draw runs.
- **Out-of-band assignments and association evidence** accumulate on the
  eligibility screen. Both are advisory — crossing a threshold flags an org
  for review and applies no penalty on its own.

---

## Phase 4 — Closed / judging

Advancing to `closed` **releases every in-flight assignment**. A PR merged
inside `merge_grace_period_hours` still counts; anything else releases and the
issue reverts to a normal bounty. Contributors are warned before `ends_at`,
not after.

Judging runs over qualifying merged PRs. With the AI flags off, verdict rows
are created with diff stats and pre-filter results and wait for you.

**Decisions here:**

- **Every verdict needs a final bucket before you can publish results.** The
  transition is blocked otherwise, with a count of what is outstanding.
- **The review queue has two distinct states.** Ordinary needs-review means
  one review was wrong and you pick which. **"Cannot resolve"** means the
  evidence supported neither reading — that is a finding about the bucket
  definitions, not about that PR. If those accumulate, fix the definitions and
  the calibration set.
- **Watch the disagreement rate.** §5.6 predicts 5–15%. Around 40% means the
  bucket definitions are ambiguous; the fix is the definitions, not the models.
- **Every override needs a written reason,** and overridden PRs are the most
  valuable additions to the calibration set — they are the cases the prompt
  got wrong.

---

## Phase 5 — Results published

**This opens the appeal window**, anchored to the moment you publish, not to a
planned date. Contributors can now see their full verdict — criteria,
citations, reasoning, bucket — and appeal it.

**Decisions here:**

- **Every appeal needs a human answer before the event can settle.** The
  transition is blocked while any is pending.
- **Decision reasons are mandatory in both directions.** A rejected appeal
  without one tells the contributor nothing; an upheld one without one throws
  away exactly the human-disagreed-with-model data the calibration set needs.
- **Upholding an appeal with a bucket change re-divides the pool for
  everyone**, so expect other people's numbers to move. That is correct.

---

## Phase 6 — Settled

Advancing here does three things: closes the appeal window, **recomputes the
whole contributor payout once** (§13-#4 — an upheld appeal changes total units
and therefore everyone's unit value), and scores and allocates the maintainer
pool.

**Payout release requires all four of:** an explicit admin action, not shadow
mode, phase `settled`, and `appeals_closed_at` set. The last two are separate
because the phase is your stated intent and `appeals_closed_at` is the record
that the arithmetic was actually redone. If the recompute failed, nobody gets
paid, deliberately.

**Then wait.** `maintainer_holdback_pct` (30%) of each maintainer payout is
held for `maintainer_holdback_days` (90). Release is **conditional on the repo
still being active** — it is not a timer. Sustained activity releases in full,
some activity releases a configurable share, nothing withholds, and money that
could not be measured stays pending and retries rather than being forfeited on
a failed API call. Withheld money goes to `maintainer_withheld_destination`,
published in advance.

The holdback job selects on `due_at <= now()` with no memory of previous runs,
so a service restart across a due date resolves it on the next tick.

---

## If something looks wrong

- **A config change had no effect.** Check the key is `Active` on the rules
  page. Four keys were once seeded, shown in the UI, and read by nothing;
  `TestConfigDefinitions_ActiveFlagMatchesActualUse` now makes that
  impossible, but check the flag before assuming the code is broken.
- **A live hackathon ignored a global default.** Correct — it reads its
  Phase 3 snapshot. Editing a live event's rules is a separate, explicitly
  labelled action that writes an audit row.
- **Payouts do not sum to the advertised pool.** They should, after settle.
  If they do not, the recompute did not run — check `appeals_closed_at`.
- **A maintainer's score looks flat across repos.** Two of the four criteria
  are floors rather than growth measures, so most legitimate repos score full
  marks on both. The rules page says so.
