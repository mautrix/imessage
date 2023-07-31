-- v21 (compatible with v18+): Add backfill queue

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
