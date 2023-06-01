-- v20 (compatible with v18+): Add index on portal thread IDs
CREATE INDEX portal_thread_id_idx ON portal (thread_id);
