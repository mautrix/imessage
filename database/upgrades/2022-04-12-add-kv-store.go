package upgrades

import (
	"database/sql"
)

func init() {
	upgrades[8] = upgrade{"Add an arbitrary key-value store for random stuff", func(tx *sql.Tx, c context) error {
		_, err := tx.Exec(`CREATE TABLE kv_store (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`)
		return err
	}}
}
