package upgrades

import (
	"database/sql"
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


func init() {
	upgrades[1] = upgrade{"Make avatar fields optional", func(tx *sql.Tx, ctx context) error {
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
	}}
}
