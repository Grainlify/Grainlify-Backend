-- Give a wave to the one verified contributor who has none.
--
-- greatest0fallt1me verified on 2026-01-04. The Founding Contributor Pool did
-- not exist: founding_wave_lock froze its boundaries on 2026-08-15 12:26:31,
-- seven months later. So there was no transition for AssignWave to observe and
-- they hold a verification with no membership - the only such account.
--
-- APPENDING IS A CHOICE, AND THIS IS WHAT IT ASSERTS.
--
--   A sequence number records the order in which the pool OBSERVED somebody,
--   not the order in which they verified.
--
-- That is already true of every existing row, and it is measurable rather than
-- a matter of opinion:
--
--   * sequence_number matches rank-by-assigned_at    37 of 37
--   * sequence_number matches rank-by-kyc_verified_at 4 of 37
--
-- kyc_verified_at is not the tie-breaker it appears to be. It was re-stamped
-- on every repeated observation of an already-verified user, so all 37 members
-- carry a kyc_verified_at LATER than the wave assignment their verification
-- caused - impossible, and by up to 26 hours. That write is fixed now (stamped
-- on the transition only), but the historical values are drifted and cannot be
-- recovered. assigned_at is the only trustworthy ordering, and it agrees with
-- sequence perfectly.
--
-- WHY NOT INSERT AT #1 AND RENUMBER.
--
-- It would honour a first-come reading of the badge that was never what we
-- stored, and it would do so by moving 37 real people down one - changing an
-- assignment the programme calls permanent, for everybody, to fix a number for
-- one person. §4.1: a rule that quietly changes after launch tells the
-- community that Grainlify's published limits are not real. That cost is
-- unbounded; the benefit is one badge number.
--
-- WHY NOW.
--
-- 37 of 100 founding slots are taken, so #38 is inside the founding band and
-- carries the same x1.5 multiplier as #1. The choice is free TODAY and there
-- is no money difference in either direction. At the 100/101 boundary it stops
-- being free and becomes an argument about somebody's multiplier. Settled here
-- so it is not reopened there.
--
-- Written as a set rather than a hardcoded uuid: it targets exactly the
-- condition ("verified, but never assigned a wave"), which today matches one
-- account and on a fresh database matches none.

INSERT INTO founding_members (user_id, sequence_number, wave, multiplier)
SELECT
  c.id,
  -- max+1, allocated in one statement so concurrent inserts cannot collide
  -- with it. sequence_number is UNIQUE, so a collision would fail loudly
  -- rather than corrupt the ordering.
  (SELECT COALESCE(max(sequence_number), 0) FROM founding_members)
    + row_number() OVER (ORDER BY c.created_at, c.id),
  -- Read from the frozen boundaries, never from config: the multiplier a
  -- backfilled member receives must be the one in force, not whatever an
  -- environment variable says today.
  CASE
    WHEN (SELECT COALESCE(max(sequence_number), 0) FROM founding_members)
           + row_number() OVER (ORDER BY c.created_at, c.id) <= l.founding_slots
      THEN 'founding'
    WHEN (SELECT COALESCE(max(sequence_number), 0) FROM founding_members)
           + row_number() OVER (ORDER BY c.created_at, c.id) <= l.founding_slots + l.wave_two_slots
      THEN 'wave_two'
    ELSE 'open'
  END,
  CASE
    WHEN (SELECT COALESCE(max(sequence_number), 0) FROM founding_members)
           + row_number() OVER (ORDER BY c.created_at, c.id) <= l.founding_slots
      THEN l.multiplier_founding
    WHEN (SELECT COALESCE(max(sequence_number), 0) FROM founding_members)
           + row_number() OVER (ORDER BY c.created_at, c.id) <= l.founding_slots + l.wave_two_slots
      THEN l.multiplier_wave_two
    ELSE l.multiplier_open
  END
FROM users c
CROSS JOIN founding_wave_lock l
WHERE l.id = true
  AND c.kyc_status = 'verified'
  AND NOT EXISTS (SELECT 1 FROM founding_members m WHERE m.user_id = c.id)
ON CONFLICT (user_id) DO NOTHING;
