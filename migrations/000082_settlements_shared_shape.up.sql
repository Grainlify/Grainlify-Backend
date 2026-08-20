-- The settlement tables stop being founding's and become the shape every
-- producer writes.
--
-- Both tables are empty (0 rows, checked), so every rename here is free. It
-- stops being free the moment a settlement exists: a table named for one event
-- type, holding two, becomes a migration against live money rows.
--
-- Column renames go with the table renames for the same reason. "shares" is
-- founding's vocabulary - a hackathon's weight is effective_units, not shares -
-- and a column that lies about what it holds is the same staleness as a table
-- that does.

ALTER TABLE founding_settlements      RENAME TO settlements;
ALTER TABLE founding_settlement_lines RENAME TO settlement_lines;

ALTER TABLE settlements      RENAME COLUMN total_shares     TO total_weight;
ALTER TABLE settlements      RENAME COLUMN share_value_usdc TO unit_value_usdc;
ALTER TABLE settlement_lines RENAME COLUMN raw_shares       TO raw_weight;
ALTER TABLE settlement_lines RENAME COLUMN effective_shares TO effective_weight;

-- Constraint names follow a rename in Postgres only for the table, not for the
-- constraint's own name, so these are renamed explicitly. A constraint called
-- founding_settlements_pkey on a table called settlements is a small lie that
-- costs somebody five minutes at exactly the wrong moment.
ALTER INDEX founding_settlements_pkey      RENAME TO settlements_pkey;
ALTER INDEX founding_settlement_lines_pkey RENAME TO settlement_lines_pkey;
ALTER INDEX founding_settlement_lines_settlement_id_user_id_key
  RENAME TO settlement_lines_settlement_id_user_id_key;
ALTER INDEX idx_founding_settlement_lines_settlement
  RENAME TO idx_settlement_lines_settlement;

ALTER TABLE settlements RENAME CONSTRAINT founding_settlements_hackathon_id_fkey
  TO settlements_hackathon_id_fkey;
ALTER TABLE settlements RENAME CONSTRAINT founding_settlements_pool_minor_positive
  TO settlements_pool_minor_positive;
-- Named for the column it checks, which is now total_weight.
ALTER TABLE settlements RENAME CONSTRAINT founding_settlements_total_shares_check
  TO settlements_total_weight_check;

ALTER TABLE settlement_lines RENAME CONSTRAINT founding_settlement_lines_amount_minor_positive
  TO settlement_lines_amount_minor_positive;
ALTER TABLE settlement_lines RENAME CONSTRAINT founding_settlement_lines_settlement_id_fkey
  TO settlement_lines_settlement_id_fkey;
ALTER TABLE settlement_lines RENAME CONSTRAINT founding_settlement_lines_user_id_fkey
  TO settlement_lines_user_id_fkey;

-- The chain this settlement pays on. Event-level, matching payout.Settlement,
-- and matching payout_event_roots which already carries chain_id.
ALTER TABLE settlements ADD COLUMN chain_id text;

-- Which pool of the event this settlement divides.
--
-- A hackathon carries a contributor pool AND a maintainer pool on one row, so
-- one event produces TWO settlements, two roots, two escrows and two fundings.
-- Founding has only a contributor pool, which is why this defaults rather than
-- being backfilled.
ALTER TABLE settlements
  ADD COLUMN pool text NOT NULL DEFAULT 'contributor';

ALTER TABLE settlements
  ADD CONSTRAINT settlements_pool_known CHECK (pool IN ('contributor', 'maintainer'));

-- Idempotency. Re-running a producer for the same event and pool must not
-- create a second settlement, because a second settlement means a second root
-- and the first one is already published or fundable.
--
-- Partial because founding's settlements carry no hackathon_id, and there is
-- deliberately no equivalent uniqueness for them: founding settles once by
-- convention, not by constraint, and inventing a key for it here would be
-- asserting something this migration has not established.
CREATE UNIQUE INDEX settlements_one_per_event_pool
  ON settlements (hackathon_id, pool)
  WHERE hackathon_id IS NOT NULL;
