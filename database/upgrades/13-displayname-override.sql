-- v13: Remember whether a ghost displayname has been overridden
ALTER TABLE puppet ADD COLUMN name_overridden BOOLEAN NOT NULL DEFAULT false;
