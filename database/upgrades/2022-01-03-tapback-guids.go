package upgrades

import (
	"database/sql"
	"fmt"
)

func init() {
	upgrades[6] = upgrade{"Add a guid column to tapback table", func(tx *sql.Tx, c context) error {
		_, err := tx.Exec("ALTER TABLE tapback ADD COLUMN guid TEXT DEFAULT NULL")
		if err != nil {
			return fmt.Errorf("failed to create column: %w", err)
		}
		return nil
	}}
}
