-- v0 -> v21 (compatible with v18+): Latest schema

CREATE TABLE portal (
	guid              TEXT    PRIMARY KEY,
	mxid              TEXT    UNIQUE,
	name              TEXT    NOT NULL,
	avatar_hash       TEXT,
	avatar_url        TEXT,
	encrypted         BOOLEAN NOT NULL DEFAULT false,
	backfill_start_ts BIGINT NOT NULL DEFAULT 0,
	in_space          BOOLEAN NOT NULL DEFAULT false,
	thread_id         TEXT NOT NULL DEFAULT '',
	last_seen_handle  TEXT NOT NULL DEFAULT '',
	first_event_id    TEXT NOT NULL DEFAULT '',
	next_batch_id     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX portal_thread_id_idx ON portal (thread_id);

CREATE TABLE puppet (
	id               TEXT PRIMARY KEY,
	displayname      TEXT NOT NULL,
	name_overridden  BOOLEAN,
	avatar_hash      TEXT,
	avatar_url       TEXT,
	contact_info_set BOOLEAN NOT NULL DEFAULT false
);

CREATE TABLE "user" (
	mxid            TEXT PRIMARY KEY,
	access_token    TEXT NOT NULL,
	next_batch      TEXT NOT NULL,
	space_room      TEXT NOT NULL,
	management_room TEXT NOT NULL
);

CREATE TABLE message (
	portal_guid   TEXT REFERENCES portal(guid) ON DELETE CASCADE ON UPDATE CASCADE,
	guid          TEXT,
	part          INTEGER,
	mxid          TEXT NOT NULL UNIQUE,
	sender_guid   TEXT NOT NULL,
	handle_guid   TEXT NOT NULL DEFAULT '',
	timestamp     BIGINT NOT NULL,
	PRIMARY KEY (portal_guid, guid, part)
);

CREATE TABLE tapback (
	portal_guid  TEXT,
	message_guid TEXT,
	message_part INTEGER,
	sender_guid  TEXT,
	handle_guid  TEXT NOT NULL DEFAULT '',
	type         INTEGER NOT NULL,
	mxid         TEXT NOT NULL UNIQUE, guid TEXT DEFAULT NULL,
	PRIMARY KEY (portal_guid, message_guid, message_part, sender_guid),
	FOREIGN KEY (portal_guid, message_guid, message_part) REFERENCES message(portal_guid, guid, part) ON DELETE CASCADE ON UPDATE CASCADE
);

CREATE TABLE kv_store (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE merged_chat (
	source_guid TEXT PRIMARY KEY,
	target_guid TEXT NOT NULL,

	CONSTRAINT merged_chat_portal_fkey FOREIGN KEY (target_guid) REFERENCES portal(guid) ON DELETE CASCADE ON UPDATE CASCADE
);

CREATE TRIGGER on_portal_insert_add_merged_chat AFTER INSERT ON portal WHEN NEW.guid LIKE '%%;-;%%' BEGIN
	INSERT INTO merged_chat (source_guid, target_guid) VALUES (NEW.guid, NEW.guid)
	ON CONFLICT (source_guid) DO UPDATE SET target_guid=NEW.guid;
END;

CREATE TRIGGER on_merge_delete_portal AFTER INSERT ON merged_chat WHEN NEW.source_guid<>NEW.target_guid BEGIN
	DELETE FROM portal WHERE guid=NEW.source_guid;
END;

CREATE TABLE backfill_queue (
    queue_id INTEGER PRIMARY KEY
        -- only: postgres
        GENERATED ALWAYS AS IDENTITY
        ,
    user_mxid        TEXT,
    priority         INTEGER NOT NULL,
    portal_guid      TEXT,
    time_start       TIMESTAMP,
    time_end         TIMESTAMP,
    dispatch_time    TIMESTAMP,
    completed_at     TIMESTAMP,
    batch_delay      INTEGER,
    max_batch_events INTEGER NOT NULL,
    max_total_events INTEGER,

    FOREIGN KEY (user_mxid) REFERENCES "user" (mxid) ON DELETE CASCADE ON UPDATE CASCADE,
    FOREIGN KEY (portal_guid) REFERENCES portal (guid) ON DELETE CASCADE
);

CREATE TABLE backfill_state (
    user_mxid         TEXT,
    portal_guid       TEXT,
    processing_batch  BOOLEAN,
    backfill_complete BOOLEAN,
    first_expected_ts BIGINT,
    PRIMARY KEY (user_mxid, portal_guid),
    FOREIGN KEY (user_mxid) REFERENCES "user" (mxid) ON DELETE CASCADE ON UPDATE CASCADE,
    FOREIGN KEY (portal_guid) REFERENCES portal (guid) ON DELETE CASCADE
);
