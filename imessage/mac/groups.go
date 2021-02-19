// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2021 Tulir Asokan
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

package mac

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
)

const groupMemberQuery = `
SELECT value FROM cn_handles
JOIN cn_handles_sources ON cn_handles.id = cn_handles_sources.cn_handle_id
JOIN sources ON cn_handles_sources.source_id = sources.id
WHERE cn_handles_sources.source_id = (SELECT id FROM sources WHERE group_id = $1 ORDER BY seconds_from_1970 DESC LIMIT 1)
`

const legacyGroupMemberQuery = `
SELECT DISTINCT(handle.id) FROM message
JOIN chat_message_join ON chat_message_join.message_id = message.ROWID
JOIN chat              ON chat_message_join.chat_id = chat.ROWID
JOIN handle            ON message.handle_id = handle.ROWID
WHERE chat.guid=$1
`

func (imdb *Database) prepareGroups() error {
	path, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	ppPath := filepath.Join(path, "Library", "PersonalizationPortrait", "PPSQLDatabase.db")
	imdb.ppDB, err = sql.Open("sqlite3", fmt.Sprintf("file:%s?mode=ro", ppPath))
	if err != nil {
		imdb.ppDB = nil
		return err
	}
	imdb.groupMemberQuery, err = imdb.ppDB.Prepare(groupMemberQuery)
	if err != nil {
		_ = imdb.ppDB.Close()
		imdb.ppDB = nil
		imdb.groupMemberQuery = nil
		return fmt.Errorf("failed to prepare group member query: %w", err)
	}
	return nil
}

func (imdb *Database) prepareLegacyGroups() error {
	var err error
	imdb.groupMemberQuery, err = imdb.chatDB.Prepare(legacyGroupMemberQuery)
	if err != nil {
		return fmt.Errorf("failed to prepare legacy group query: %w", err)
	}
	return nil
}

func (imdb *Database) GetGroupMembers(chatID string) ([]string, error) {
	res, err := imdb.groupMemberQuery.Query(chatID)
	if err != nil {
		return nil, fmt.Errorf("error querying group members: %w", err)
	}
	var users []string
	for res.Next() {
		var user string
		err = res.Scan(&user)
		if err != nil {
			return users, fmt.Errorf("error scanning row: %w", err)
		}
		if user[0] == '+' {
			user = phoneNumberCleaner.Replace(user)
		}
		users = append(users, user)
	}
	return users, nil
}
