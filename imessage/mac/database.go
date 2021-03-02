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

	log "maunium.net/go/maulogger/v2"

	"go.mau.fi/mautrix-imessage/imessage"
)

type Database struct {
	log log.Logger

	chatDBPath           string
	chatDB               *sql.DB
	messagesQuery        *sql.Stmt
	limitedMessagesQuery *sql.Stmt
	chatQuery            *sql.Stmt
	recentChatsQuery     *sql.Stmt
	Messages             chan *imessage.Message
	stopWatching         chan struct{}

	ppDB             *sql.DB
	groupMemberQuery *sql.Stmt

	contactStore *ContactStore
}

func NewChatDatabase() (imessage.API, error) {
	imdb := &Database{
		log: log.Sub("iMessage").Sub("Mac"),
	}

	err := imdb.prepareMessages()
	if err != nil {
		return nil, fmt.Errorf("failed to open message database: %w", err)
	}
	err = imdb.prepareGroups()
	if err != nil {
		imdb.log.Debugln("Failed to open group database: %v. Falling back to message database for querying group members.")
		err = imdb.prepareLegacyGroups()
		if err != nil {
			return nil, fmt.Errorf("failed to open legacy group database: %w", err)
		}
	}

	imdb.contactStore = NewContactStore()
	err = imdb.contactStore.RequestAccess()
	if err != nil {
		imdb.log.Errorln("Failed to get contact access:", err)
	} else if imdb.contactStore.HasAccess {
		imdb.log.Infoln("Contact access is allowed")
	} else {
		imdb.log.Warnln("Contact access is not allowed")
	}

	return imdb, nil
}

func init() {
	imessage.Implementations["mac"] = NewChatDatabase
}
