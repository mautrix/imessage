-- v17: Add batch sending identifiers to portals

ALTER TABLE portal ADD COLUMN first_event_id TEXT NOT NULL DEFAULT '';
ALTER TABLE portal ADD COLUMN next_batch_id TEXT NOT NULL DEFAULT '';
