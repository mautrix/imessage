// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2022 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package database

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	log "maunium.net/go/maulogger/v2"
	"maunium.net/go/mautrix/util/dbutil"
)

type MergedChatQuery struct {
	db  *Database
	log log.Logger
}

func (mcq *MergedChatQuery) Set(txn dbutil.Execable, target string, sources ...string) {
	if txn == nil {
		txn = mcq.db
	}
	placeholders := make([]string, len(sources))
	args := make([]any, len(sources)+1)
	args[0] = target
	for i, source := range sources {
		args[i+1] = source
		placeholders[i] = fmt.Sprintf("(?1, ?%d)", i+2)
	}
	_, err := txn.Exec(fmt.Sprintf("INSERT OR REPLACE INTO merged_chat (target_guid, source_guid) VALUES %s", strings.Join(placeholders, ", ")), args...)
	if err != nil {
		mcq.log.Warnfln("Failed to insert %s->%s: %v", sources, target, err)
	}
}

func (mcq *MergedChatQuery) Remove(guid string) {
	_, err := mcq.db.Exec("DELETE FROM merged_chat WHERE source_guid=$1", guid)
	if err != nil {
		mcq.log.Warnfln("Failed to remove %s: %v", guid, err)
	}
}

func (mcq *MergedChatQuery) GetAllForTarget(guid string) (sources []string) {
	rows, err := mcq.db.Query("SELECT source_guid FROM merged_chat WHERE target_guid=$1", guid)
	if err != nil {
		mcq.log.Errorfln("Failed to get merge sources for %s: %v", guid, err)
		return
	}
	for rows.Next() {
		var source string
		err = rows.Scan(&source)
		if err != nil {
			mcq.log.Errorfln("Failed to scan merge source: %v", err)
		} else {
			sources = append(sources, source)
		}
	}
	return
}

func (mcq *MergedChatQuery) Get(guid string) (target string) {
	err := mcq.db.QueryRow("SELECT target_guid FROM merged_chat WHERE source_guid=$1", guid).Scan(&target)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		mcq.log.Errorfln("Failed to get merge target for %s: %v", guid, err)
	}
	return
}
