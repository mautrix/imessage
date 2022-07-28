-- v0 -> v14: Latest schema

CREATE TABLE portal (
	guid        		TEXT    PRIMARY KEY,
	mxid        		TEXT    UNIQUE,
	name        		TEXT    NOT NULL,
	avatar_hash 		TEXT,
	avatar_url  		TEXT,
	encrypted   		BOOLEAN NOT NULL DEFAULT false,
	backfill_start_ts 	BIGINT NOT NULL DEFAULT 0,
	in_space    		BOOLEAN NOT NULL DEFAULT false,
	correlation_id 		TEXT
);

CREATE TABLE puppet (
	id              TEXT PRIMARY KEY,
	displayname     TEXT NOT NULL,
	name_overridden BOOLEAN,
	avatar_hash     TEXT,
	avatar_url      TEXT,
	correlation_id 	TEXT
);

CREATE TABLE "user" (
	mxid            TEXT PRIMARY KEY,
	access_token    TEXT NOT NULL,
	next_batch      TEXT NOT NULL,
	space_room      TEXT NOT NULL,
	management_room TEXT NOT NULL
);

CREATE TABLE message (
	chat_guid     TEXT REFERENCES portal(guid) ON DELETE CASCADE ON UPDATE CASCADE,
	guid          TEXT,
	part          INTEGER,
	mxid          TEXT NOT NULL UNIQUE,
	sender_guid   TEXT NOT NULL,
	timestamp     BIGINT NOT NULL,
	PRIMARY KEY (chat_guid, guid, part)
);

CREATE TABLE tapback (
	chat_guid    TEXT,
	message_guid TEXT,
	message_part INTEGER,
	sender_guid  TEXT,
	type         INTEGER NOT NULL,
	mxid         TEXT NOT NULL UNIQUE, guid TEXT DEFAULT NULL,
	PRIMARY KEY (chat_guid, message_guid, message_part, sender_guid),
	FOREIGN KEY (chat_guid, message_guid, message_part) REFERENCES message(chat_guid, guid, part) ON DELETE CASCADE ON UPDATE CASCADE
);

CREATE TABLE kv_store (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
