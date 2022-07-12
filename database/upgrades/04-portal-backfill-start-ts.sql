-- v4: Add backfill_start_ts to portal table

ALTER TABLE portal ADD COLUMN backfill_start_ts BIGINT NOT NULL DEFAULT 0;
