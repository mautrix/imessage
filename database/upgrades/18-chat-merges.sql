-- v18: Add table for merging chats

CREATE TABLE merged_chat (
    source_guid TEXT PRIMARY KEY,
    target_guid TEXT NOT NULL,

    CONSTRAINT merged_chat_portal_fkey FOREIGN KEY (target_guid) REFERENCES portal(guid) ON DELETE CASCADE ON UPDATE CASCADE
);

INSERT INTO merged_chat (source_guid, target_guid) SELECT guid, guid FROM portal WHERE guid LIKE '%%;-;%%';
