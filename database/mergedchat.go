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
	log "maunium.net/go/maulogger/v2"
)

type MergedChatQuery struct {
	db  *Database
	log log.Logger
}

func (mcq *MergedChatQuery) Set(source, target string) error {
	_, err := mcq.db.Exec(`
		INSERT INTO merged_chat (source_guid, target_guid) VALUES ($1, $2)
		ON CONFLICT (source_guid) DO UPDATE SET target_guid=excluded.target_guid
	`, source, target)
	return err
}

func (mcq *MergedChatQuery) Remove(guid string) error {
	_, err := mcq.db.Exec("DELETE FROM merged_chat WHERE source_guid=$1", guid)
	return err
}
