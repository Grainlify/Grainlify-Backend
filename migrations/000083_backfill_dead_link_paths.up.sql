-- 43 notifications point at a route that does not exist.
--
-- There is no /settings route. The app serves exactly six paths - /, /signin,
-- /signup, /auth/callback, /support and /dashboard - and every settings surface
-- is a query parameter on /dashboard. React Router logs "No routes matched",
-- the page renders blank, and a refresh does not recover it; only the back
-- button does.
--
-- Two spellings were written, both wrong, from five call sites each composing
-- the path by hand. That was fixed in #453 by building links in one place
-- (internal/notifications/links.go), and internal/notifications/links_test.go
-- asserts no "/settings" appears. Nothing emits these any more, so this is a
-- one-off repair rather than a treadmill.
--
-- The rows are repaired rather than nulled: the destination each was aiming at
-- is still correct and still reachable, so a person clicking an old
-- notification should arrive where it always meant to send them.
--
-- Scoped by exact value, not by prefix. Every distinct route in the table was
-- enumerated first - /dashboard (310 rows) and /settings (43) - so these two
-- patterns are the whole population and not the two somebody happened to spot.

UPDATE notifications
   SET link_path = '/dashboard?tab=settings&subtab=rewards'
 WHERE link_path = '/settings?subtab=rewards';

-- Note tab -> subtab. The old spelling used tab=referrals, which would select
-- the top-level dashboard tab rather than the settings subtab even if
-- /settings had resolved.
UPDATE notifications
   SET link_path = '/dashboard?tab=settings&subtab=referrals'
 WHERE link_path = '/settings?tab=referrals';
