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

package database

import (
	_ "github.com/mattn/go-sqlite3"
	"maunium.net/go/maulogger/v2"

	"maunium.net/go/mautrix/util/dbutil"

	"go.mau.fi/mautrix-imessage/database/upgrades"
)

type Database struct {
	*dbutil.Database

	User       *UserQuery
	Portal     *PortalQuery
	Puppet     *PuppetQuery
	Message    *MessageQuery
	Tapback    *TapbackQuery
	KV         *KeyValueQuery
	MergedChat *MergedChatQuery
}

func New(parent *dbutil.Database, log maulogger.Logger) *Database {
	db := &Database{
		Database: parent,
	}
	db.UpgradeTable = upgrades.Table

	db.User = &UserQuery{
		db:  db,
		log: log.Sub("User"),
	}
	db.Portal = &PortalQuery{
		db:  db,
		log: log.Sub("Portal"),
	}
	db.Puppet = &PuppetQuery{
		db:  db,
		log: log.Sub("Puppet"),
	}
	db.Message = &MessageQuery{
		db:  db,
		log: log.Sub("Message"),
	}
	db.Tapback = &TapbackQuery{
		db:  db,
		log: log.Sub("Tapback"),
	}
	db.KV = &KeyValueQuery{
		db:  db,
		log: log.Sub("KeyValue"),
	}
	db.MergedChat = &MergedChatQuery{
		db:  db,
		log: log.Sub("MergedChat"),
	}
	return db
}
