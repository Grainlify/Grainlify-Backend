# Checklist: announcing the Founding Contributor Pool

Everything here is a gate, not a suggestion. The announcement is the moment
the programme stops being changeable — before it, none of this matters; after
it, none of it can be fixed quietly.

## Lock the wave boundaries **as part of announcing**

**This is the one that will otherwise be remembered on the day, and cannot be
repaired afterwards.**

`GET /founding/waves` currently returns `boundaries_locked: false`. That is
correct today — nothing is announced, so nothing is fixed. It must be `true`
from the moment the announcement goes out.

Boundaries lock automatically when the *first member is assigned*, snapshotting
whatever config is live at that instant. That is the failure mode: announce
100/400, have someone edit the config before the first person verifies, and the
lock captures the edited numbers. The announced figures and the enforced ones
would differ, silently, with no way to tell which anyone believed.

So lock deliberately, before the announcement is published, not after and not
by waiting for the first signup.

**There is no admin action for this yet.** The mechanism available today is to
insert the single `founding_wave_lock` row directly with the announced values,
after which every assignment reads that row and `SetValue` refuses to change
any of the five locked keys. A proper admin action should exist before this is
done for real — doing it by hand is exactly the kind of step that gets done
wrong once.

Verify after locking:

```
GET /founding/waves      -> boundaries_locked: true
```

and confirm the returned `total` and `multiplier` match what the announcement
says. If they disagree, the announcement is wrong, not the API.

Why this matters more than it looks: the entire argument for pre-announced
waves over one soft limit was that the boundaries genuinely cannot move. A
wave that widens after the fact tells everyone that Grainlify's announced
limits are negotiable — and every published rule after that, including the
draw weights and frozen event config the anti-farming design rests on, gets
read as provisional.

## Before the announcement

- [ ] **Wave boundaries locked** (above), and the locked values match the copy.
- [ ] **Pool funded and set aside.** The announcement states a real number. It
      is a fixed liability from the moment it is published.
- [ ] **Docs live.** Already done — `docs.grainlify.com` describes the pool,
      the waves, the cap and the retired points system.
- [ ] **Points programme frozen in production.** Already done — `/redemptions`
      returns `410 points_programme_frozen`.
- [ ] **Redeem page shows the retirement.** Already done.

## What the announcement may say

- The pool is $X, funded and set aside
- It is distributed at the end of the first GrainHack
- Shares come from verifying, from merged pull requests, and from bringing
  people who ship merged pull requests
- Waves, **all three announced together**: first 100 at ×1.5, next 400 at
  ×1.25, everyone after at ×1.0
- Each wave closes for good, and the next multiplier is lower
- Following the social accounts is required to be eligible
- **Your share depends on how many others take part — this is not a fixed
  amount per person**
- Shares come overwhelmingly from merged pull requests, not from signing up

## What it may not say

- [ ] **No per-person figure, and no points-to-dollars rate.** A guard test
      fails the build if a computed settlement figure reaches a handler or a
      notification; the same rule applies to the announcement copy.
- [ ] **No payout date.** Say "at the end of the first GrainHack". There is
      still no disbursement path (`GAP-grainhack-payout-release.md`), so a
      named date is a promise nothing can currently keep.
- [ ] **Nothing implying the pool grows with participation.** It does not.

## Say the dilution plainly

More participants means a smaller share each. A community told this up front
treats it as the rules; one that discovers it on payout day treats it as a
bait-and-switch. Same fact, opposite outcome — and it costs nothing to say
first.

## After the announcement

- [ ] Watch `boundaries_locked` stays `true` and the locked values never move.
- [ ] If a wave fills faster than expected, **let it close.** Add a wave below
      it if more capacity is needed. Never widen one already announced.
