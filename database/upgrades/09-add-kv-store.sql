-- v9: Add an arbitrary key-value store for random stuff

CREATE TABLE kv_store (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
