-- Restores the broken paths, which is what a down migration means here.
--
-- Only rows whose type matches the original mapping are reverted, so a
-- notification legitimately written to these destinations after the backfill is
-- left alone rather than being broken by a rollback.
UPDATE notifications
   SET link_path = '/settings?subtab=rewards'
 WHERE link_path = '/dashboard?tab=settings&subtab=rewards'
   AND type = 'social_follow_completed'
   AND created_at < '2026-08-17';

UPDATE notifications
   SET link_path = '/settings?tab=referrals'
 WHERE link_path = '/dashboard?tab=settings&subtab=referrals'
   AND type = 'referral_completed'
   AND created_at < '2026-08-17';
