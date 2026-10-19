-- v22 (compatible with v18+): Preserve custom emoji Tapbacks
ALTER TABLE tapback ADD COLUMN custom_emoji TEXT NOT NULL DEFAULT '';
