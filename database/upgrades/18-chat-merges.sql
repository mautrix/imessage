-- v18: Add table for merging chats

CREATE TABLE merged_chat (
    source_guid TEXT PRIMARY KEY,
    target_guid TEXT NOT NULL,

    CONSTRAINT merged_chat_portal_fkey FOREIGN KEY (target_guid) REFERENCES portal(guid) ON DELETE CASCADE ON UPDATE CASCADE
);

INSERT INTO merged_chat (source_guid, target_guid) SELECT guid, guid FROM portal WHERE guid LIKE '%%;-;%%';

CREATE TRIGGER on_portal_insert_add_merged_chat AFTER INSERT ON portal WHEN NEW.guid LIKE '%%;-;%%' BEGIN
	INSERT INTO merged_chat (source_guid, target_guid) VALUES (NEW.guid, NEW.guid)
    ON CONFLICT (source_guid) DO UPDATE SET target_guid=NEW.guid;
END;

CREATE TRIGGER on_merge_delete_portal AFTER INSERT ON merged_chat WHEN NEW.source_guid<>NEW.target_guid BEGIN
	DELETE FROM portal WHERE guid=NEW.source_guid;
END;

ALTER TABLE message RENAME COLUMN chat_guid TO portal_guid;
ALTER TABLE message ADD COLUMN handle_guid TEXT NOT NULL DEFAULT '';
ALTER TABLE tapback RENAME COLUMN chat_guid TO portal_guid;
ALTER TABLE tapback ADD COLUMN handle_guid TEXT NOT NULL DEFAULT '';
ALTER TABLE portal ADD COLUMN last_seen_handle TEXT NOT NULL DEFAULT '';
UPDATE portal SET last_seen_handle=guid WHERE guid LIKE '%%;-;%%';
UPDATE message SET handle_guid=portal_guid WHERE portal_guid LIKE '%%;-;%%';
UPDATE tapback SET handle_guid=portal_guid WHERE portal_guid LIKE '%%;-;%%';
