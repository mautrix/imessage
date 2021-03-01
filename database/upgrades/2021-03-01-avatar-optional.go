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
		_, _ = tx.Exec("PRAGMA foreign_keys = OFF")
		_, err := tx.Exec("ALTER TABLE puppet RENAME TO old_puppet")
		if err != nil {
			return err
		}
		_, err = tx.Exec("ALTER TABLE portal RENAME TO old_portal")
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
		_, err = tx.Exec("INSERT INTO puppet (id, displayname, avatar_hash, avatar_url) SELECT id, displayname, avatar, avatar_url FROM old_puppet")
		if err != nil {
			return err
		}
		_, err = tx.Exec("INSERT INTO portal (guid, mxid, name, avatar_hash, avatar_url, encrypted) SELECT guid, mxid, name, avatar, avatar_url, encrypted FROM old_portal")
		if err != nil {
			return err
		}
		_, _ = tx.Exec("DROP TABLE old_puppet")
		_, _ = tx.Exec("DROP TABLE old_portal")
		_, _ = tx.Exec("PRAGMA foreign_keys = ON")
		return nil
	}}
}
