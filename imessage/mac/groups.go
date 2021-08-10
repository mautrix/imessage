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
	"fmt"
)

const groupMemberQuery = `
SELECT handle.id FROM chat
JOIN chat_handle_join ON chat_handle_join.chat_id = chat.ROWID
JOIN handle ON chat_handle_join.handle_id = handle.ROWID
WHERE chat.guid=$1
`

func (mac *macOSDatabase) prepareGroups() error {
	var err error
	mac.groupMemberQuery, err = mac.chatDB.Prepare(groupMemberQuery)
	if err != nil {
		return fmt.Errorf("failed to prepare legacy group query: %w", err)
	}
	return nil
}

func (mac *macOSDatabase) GetGroupMembers(chatID string) ([]string, error) {
	res, err := mac.groupMemberQuery.Query(chatID)
	if err != nil {
		return nil, fmt.Errorf("error querying group members: %w", err)
	}
	var users []string
	for res.Next() {
		var user string
		err = res.Scan(&user)
		if err != nil {
			return users, fmt.Errorf("error scanning row: %w", err)
		} else if len(user) == 0 {
			continue
		}
		users = append(users, user)
	}
	return users, nil
}
