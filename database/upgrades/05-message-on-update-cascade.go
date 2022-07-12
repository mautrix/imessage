package upgrades

import (
	"database/sql"
	"fmt"

	"maunium.net/go/mautrix/util/dbutil"
)

const createMessageTable3 = `CREATE TABLE message (
	chat_guid     TEXT REFERENCES portal(guid) ON DELETE CASCADE ON UPDATE CASCADE,
	guid          TEXT,
	part          INTEGER,
	mxid          TEXT NOT NULL UNIQUE,
	sender_guid   TEXT NOT NULL,
	timestamp     BIGINT NOT NULL,
	PRIMARY KEY (chat_guid, guid, part)
)`

const createTapbackTable3 = `CREATE TABLE tapback (
	chat_guid    TEXT,
	message_guid TEXT,
	message_part INTEGER,
	sender_guid  TEXT,
	type         INTEGER NOT NULL,
	mxid         TEXT NOT NULL UNIQUE,
	PRIMARY KEY (chat_guid, message_guid, message_part, sender_guid),
	FOREIGN KEY (chat_guid, message_guid, message_part) REFERENCES message(chat_guid, guid, part) ON DELETE CASCADE ON UPDATE CASCADE
)`

func init() {
	Table.Register(-1, 5, "Add ON UPDATE CASCADE to message foreign keys", func(tx *sql.Tx, db *dbutil.Database) error {
		_, err := tx.Exec("PRAGMA defer_foreign_keys = ON")
		if err != nil {
			return fmt.Errorf("failed to enable defer_foreign_keys pragma: %w", err)
		}
		_, err = tx.Exec("ALTER TABLE message RENAME TO old_message")
		if err != nil {
			return fmt.Errorf("failed to rename old message table: %w", err)
		}
		_, err = tx.Exec("ALTER TABLE tapback RENAME TO old_tapback")
		if err != nil {
			return fmt.Errorf("failed to rename old tapback table: %w", err)
		}
		_, err = tx.Exec(createMessageTable3)
		if err != nil {
			return fmt.Errorf("failed to create new message table: %w", err)
		}
		_, err = tx.Exec(createTapbackTable3)
		if err != nil {
			return fmt.Errorf("failed to create new tapback table: %w", err)
		}
		_, err = tx.Exec("INSERT INTO message SELECT chat_guid, guid, part, mxid, sender_guid, timestamp FROM old_message")
		if err != nil {
			return fmt.Errorf("failed to copy messages into new table: %w", err)
		}
		_, err = tx.Exec("INSERT INTO tapback SELECT chat_guid, message_guid, message_part, sender_guid, type, mxid FROM old_tapback")
		if err != nil {
			return fmt.Errorf("failed to copy tapbacks into new table: %w", err)
		}
		_, err = tx.Exec("DROP TABLE old_tapback")
		if err != nil {
			return fmt.Errorf("failed to drop old tapback table: %w", err)
		}
		_, err = tx.Exec("DROP TABLE old_message")
		if err != nil {
			return fmt.Errorf("failed to drop old message table: %w", err)
		}
		return nil
	})
}
