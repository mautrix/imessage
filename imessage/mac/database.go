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

	chatDBPath    string
	chatDB        *sql.DB
	messagesQuery *sql.Stmt
	chatQuery     *sql.Stmt
	Messages      chan *imessage.Message
	stopWatching  chan struct{}

	ppDB             *sql.DB
	groupMemberQuery *sql.Stmt

	Contacts map[string]*imessage.Contact
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
	err = imdb.loadAddressBook()
	if err != nil {
		return nil, fmt.Errorf("failed to read address book: %w", err)
	}

	return imdb, nil
}

func init() {
	imessage.Implementations["mac"] = NewChatDatabase
}
