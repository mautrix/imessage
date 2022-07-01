-- v13: Add a separate column for tracking
ALTER TABLE "puppet" ADD COLUMN name_override TEXT NOT NULL DEFAULT '';
