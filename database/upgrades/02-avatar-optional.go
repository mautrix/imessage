package upgrades

import (
	"database/sql"

	"maunium.net/go/mautrix/util/dbutil"
)

const createPortalTable2 = `CREATE TABLE portal (
	guid       TEXT    PRIMARY KEY,
	mxid       TEXT    UNIQUE,
	name       TEXT    NOT NULL,
	avatar_hash TEXT,
	avatar_url  TEXT,
	encrypted  BOOLEAN NOT NULL DEFAULT 0
)`

const createPuppetTable2 = `CREATE TABLE puppet (
	id           TEXT PRIMARY KEY,
	displayname  TEXT NOT NULL,
	avatar_hash  TEXT,
	avatar_url   TEXT
)`

const createMessageTable = `CREATE TABLE message (
	chat_guid     TEXT REFERENCES portal(guid) ON DELETE CASCADE,
	guid          TEXT,
	mxid          TEXT NOT NULL UNIQUE,
	sender_guid   TEXT NOT NULL,
	timestamp     BIGINT NOT NULL,
	PRIMARY KEY (chat_guid, guid)
)`

const createTapbackTable = `CREATE TABLE tapback (
	chat_guid    TEXT,
	message_guid TEXT,
	sender_guid  TEXT,
    type         INTEGER NOT NULL,
	mxid         TEXT NOT NULL UNIQUE,
	PRIMARY KEY (chat_guid, message_guid, sender_guid),
	FOREIGN KEY (chat_guid, message_guid) REFERENCES message(chat_guid, guid) ON DELETE CASCADE
)`

func init() {
	Table.Register(-1, 2, "Make avatar fields optional", func(tx *sql.Tx, db *dbutil.Database) error {
		_, err := tx.Exec("PRAGMA defer_foreign_keys = ON")
		if err != nil {
			return err
		}
		_, err = tx.Exec("ALTER TABLE puppet RENAME TO old_puppet")
		if err != nil {
			return err
		}
		_, err = tx.Exec("ALTER TABLE portal RENAME TO old_portal")
		if err != nil {
			return err
		}
		_, err = tx.Exec("ALTER TABLE message RENAME TO old_message")
		if err != nil {
			return err
		}
		_, err = tx.Exec("ALTER TABLE tapback RENAME TO old_tapback")
		if err != nil {
			return err
		}
		_, err = tx.Exec(createPortalTable2)
		if err != nil {
			return err
		}
		_, err = tx.Exec(createPuppetTable2)
		if err != nil {
			return err
		}
		_, err = tx.Exec(createMessageTable)
		if err != nil {
			return err
		}
		_, err = tx.Exec(createTapbackTable)
		if err != nil {
			return err
		}
		_, err = tx.Exec("INSERT INTO puppet SELECT * FROM old_puppet")
		if err != nil {
			return err
		}
		_, err = tx.Exec("INSERT INTO portal SELECT * FROM old_portal")
		if err != nil {
			return err
		}
		_, err = tx.Exec("INSERT INTO message SELECT * FROM old_message")
		if err != nil {
			return err
		}
		_, err = tx.Exec("INSERT INTO tapback SELECT * FROM old_tapback")
		if err != nil {
			return err
		}
		_, err = tx.Exec("DROP TABLE old_tapback")
		if err != nil {
			return err
		}
		_, err = tx.Exec("DROP TABLE old_message")
		if err != nil {
			return err
		}
		_, err = tx.Exec("DROP TABLE old_portal")
		if err != nil {
			return err
		}
		_, err = tx.Exec("DROP TABLE old_puppet")
		if err != nil {
			return err
		}
		return nil
	})
}
