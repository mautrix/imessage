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

	log "maunium.net/go/maulogger/v2"

	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/util/dbutil"
)

type UserQuery struct {
	db  *Database
	log log.Logger
}

func (uq *UserQuery) New() *User {
	return &User{
		db:  uq.db,
		log: uq.log,
	}
}

func (uq *UserQuery) GetByMXID(userID id.UserID) *User {
	row := uq.db.QueryRow(`SELECT mxid, access_token, next_batch, space_room, management_room FROM "user" WHERE mxid=$1`, userID)
	if row == nil {
		return nil
	}
	return uq.New().Scan(row)
}

type User struct {
	db  *Database
	log log.Logger

	MXID           id.UserID
	AccessToken    string
	NextBatch      string
	SpaceRoom      id.RoomID
	ManagementRoom id.RoomID
}

func (user *User) Scan(row dbutil.Scannable) *User {
	err := row.Scan(&user.MXID, &user.AccessToken, &user.NextBatch, &user.SpaceRoom, &user.ManagementRoom)
	if err != nil {
		if err != sql.ErrNoRows {
			user.log.Errorln("Database scan failed:", err)
		}
		return nil
	}
	return user
}

func (user *User) Insert() {
	_, err := user.db.Exec(`INSERT INTO "user" (mxid, access_token, next_batch, space_room, management_room) VALUES ($1, $2, $3, $4, $5)`,
		user.MXID, user.AccessToken, user.NextBatch, user.SpaceRoom, user.ManagementRoom)
	if err != nil {
		user.log.Warnfln("Failed to insert %s: %v", user.MXID, err)
	}
}

func (user *User) Update() {
	_, err := user.db.Exec(`UPDATE "user" SET access_token=$1, next_batch=$2, space_room=$3, management_room=$4 WHERE mxid=$5`,
		user.AccessToken, user.NextBatch, user.SpaceRoom, user.ManagementRoom, user.MXID)
	if err != nil {
		user.log.Warnfln("Failed to update %s: %v", user.MXID, err)
	}
}
