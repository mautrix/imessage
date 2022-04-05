package upgrades

import (
	"database/sql"
)

func init() {
	upgrades[7] = upgrade{"Remove management room column from users", func(tx *sql.Tx, c context) error {
		_, err := tx.Exec("ALTER TABLE user DROP COLUMN management_room")
		return err
	}}
}
