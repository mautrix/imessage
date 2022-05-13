package upgrades

import "database/sql"

func init() {
	upgrades[10] = upgrade{"Add service column to message", func(tx *sql.Tx, ctx context) error {
		_, err := tx.Exec(`ALTER TABLE "message" ADD COLUMN service TEXT NOT NULL DEFAULT ''`)
		if err != nil {
			return err
		}
		return err
	}}
}
