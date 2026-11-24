-- v22 (compatible with v18+): Add emoji column to tapback table for custom emoji reactions

ALTER TABLE tapback ADD COLUMN emoji TEXT NOT NULL DEFAULT '';
