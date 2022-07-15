-- v7: Add a guid column to tapback table

ALTER TABLE tapback ADD COLUMN guid TEXT DEFAULT NULL;
