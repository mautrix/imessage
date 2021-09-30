package upgrades

import (
	"database/sql"
	"fmt"
)

func init() {
	upgrades[3] = upgrade{"Add backfill_start_ts to portal table", func(tx *sql.Tx, ctx context) error {
		_, err := tx.Exec("ALTER TABLE portal ADD COLUMN backfill_start_ts BIGINT NOT NULL DEFAULT 0")
		if err != nil {
			return fmt.Errorf("failed to create column: %w", err)
		}
		return nil
	}}
}
