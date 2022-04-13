package upgrades

import (
	"database/sql"
)

func init() {
	upgrades[9] = upgrade{"Add personal filtering space info to user tables", func(tx *sql.Tx, ctx context) error {
		_, err := tx.Exec(`ALTER TABLE "user" ADD COLUMN space_room TEXT NOT NULL DEFAULT ''`)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`ALTER TABLE portal ADD COLUMN in_space BOOLEAN NOT NULL DEFAULT false`)
		return err
	}}
}
