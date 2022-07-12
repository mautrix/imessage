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

	"go.mau.fi/mautrix-imessage/database/upgrades"
	"maunium.net/go/mautrix/util/dbutil"
)

type Database struct {
	*dbutil.Database

	User    *UserQuery
	Portal  *PortalQuery
	Puppet  *PuppetQuery
	Message *MessageQuery
	Tapback *TapbackQuery
	KV      *KeyValueQuery
}

func New(parent *dbutil.Database) *Database {
	db := &Database{
		Database: parent,
	}
	db.UpgradeTable = upgrades.Table
	_, err := db.Exec("PRAGMA foreign_keys = ON")
	if err != nil {
		db.Log.Warnln("Failed to enable foreign keys:", err)
	}

	db.User = &UserQuery{
		db:  db,
		log: db.Log.Sub("User"),
	}
	db.Portal = &PortalQuery{
		db:  db,
		log: db.Log.Sub("Portal"),
	}
	db.Puppet = &PuppetQuery{
		db:  db,
		log: db.Log.Sub("Puppet"),
	}
	db.Message = &MessageQuery{
		db:  db,
		log: db.Log.Sub("Message"),
	}
	db.Tapback = &TapbackQuery{
		db:  db,
		log: db.Log.Sub("Tapback"),
	}
	db.KV = &KeyValueQuery{
		db:  db,
		log: db.Log.Sub("KeyValue"),
	}
	return db
}
