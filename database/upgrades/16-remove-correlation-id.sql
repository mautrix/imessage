-- v16: Remove correlation_id columns

ALTER TABLE portal DROP COLUMN correlation_id;
ALTER TABLE puppet DROP COLUMN correlation_id;
