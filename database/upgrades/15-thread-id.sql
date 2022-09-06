-- v15: Store internal thread ID for portals

ALTER TABLE portal ADD COLUMN thread_id TEXT NOT NULL DEFAULT '';
