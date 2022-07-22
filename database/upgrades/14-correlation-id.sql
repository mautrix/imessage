-- v14: Add correlation_id columns

ALTER TABLE portal ADD COLUMN correlation_id TEXT;
ALTER TABLE puppet ADD COLUMN correlation_id TEXT;
ALTER TABLE message ADD COLUMN correlation_id TEXT;
ALTER TABLE tapback ADD COLUMN correlation_id TEXT;
