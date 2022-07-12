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
	"database/sql"

	log "maunium.net/go/maulogger/v2"

	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/util/dbutil"

	"go.mau.fi/mautrix-imessage/imessage"
)

type TapbackQuery struct {
	db  *Database
	log log.Logger
}

func (mq *TapbackQuery) New() *Tapback {
	return &Tapback{
		db:  mq.db,
		log: mq.log,
	}
}

func (mq *TapbackQuery) GetByGUID(chat, message string, part int, sender string) *Tapback {
	return mq.get("SELECT chat_guid, guid, message_guid, message_part, sender_guid, type, mxid "+
		"FROM tapback WHERE chat_guid=$1 AND message_guid=$2 AND message_part=$3 AND sender_guid=$4",
		chat, message, part, sender)
}

func (mq *TapbackQuery) GetByTapbackGUID(chat, tapback string) *Tapback {
	return mq.get("SELECT chat_guid, guid, message_guid, message_part, sender_guid, type, mxid "+
		"FROM tapback WHERE chat_guid=$1 AND guid=$2",
		chat, tapback)
}

func (mq *TapbackQuery) GetByMXID(mxid id.EventID) *Tapback {
	return mq.get("SELECT chat_guid, guid, message_guid, message_part, sender_guid, type, mxid "+
		"FROM tapback WHERE mxid=$1", mxid)
}

func (mq *TapbackQuery) get(query string, args ...interface{}) *Tapback {
	row := mq.db.QueryRow(query, args...)
	if row == nil {
		return nil
	}
	return mq.New().Scan(row)
}

type Tapback struct {
	db  *Database
	log log.Logger

	ChatGUID    string
	GUID        string
	MessageGUID string
	MessagePart int
	SenderGUID  string
	Type        imessage.TapbackType
	MXID        id.EventID
}

func (tapback *Tapback) Scan(row dbutil.Scannable) *Tapback {
	var nullishGUID sql.NullString
	err := row.Scan(&tapback.ChatGUID, &nullishGUID, &tapback.MessageGUID, &tapback.MessagePart, &tapback.SenderGUID, &tapback.Type, &tapback.MXID)
	if err != nil {
		if err != sql.ErrNoRows {
			tapback.log.Errorln("Database scan failed:", err)
		}
		return nil
	}
	tapback.GUID = nullishGUID.String
	return tapback
}

func (tapback *Tapback) Insert() {
	_, err := tapback.db.Exec("INSERT INTO tapback (chat_guid, guid, message_guid, message_part, sender_guid, type, mxid) VALUES ($1, $2, $3, $4, $5, $6, $7)",
		tapback.ChatGUID, tapback.GUID, tapback.MessageGUID, tapback.MessagePart, tapback.SenderGUID, tapback.Type, tapback.MXID)
	if err != nil {
		tapback.log.Warnfln("Failed to insert tapback %s/%s.%d/%s: %v", tapback.ChatGUID, tapback.MessageGUID, tapback.MessagePart, tapback.SenderGUID, err)
	}
}

func (tapback *Tapback) Update() {
	_, err := tapback.db.Exec("UPDATE tapback SET guid=?5, type=?6, mxid=?7 WHERE chat_guid=?1 AND message_guid=?2 AND message_part=?3 AND sender_guid=?4",
		tapback.ChatGUID, tapback.MessageGUID, tapback.MessagePart, tapback.SenderGUID, tapback.GUID, tapback.Type, tapback.MXID)
	if err != nil {
		tapback.log.Warnfln("Failed to update tapback %s/%s.%d/%s: %v", tapback.ChatGUID, tapback.MessageGUID, tapback.MessagePart, tapback.SenderGUID, err)
	}
}

func (tapback *Tapback) Delete() {
	_, err := tapback.db.Exec("DELETE FROM tapback WHERE chat_guid=$1 AND message_guid=$2 AND message_part=$3 AND sender_guid=$4", tapback.ChatGUID, tapback.MessageGUID, tapback.MessagePart, tapback.SenderGUID)
	if err != nil {
		tapback.log.Warnfln("Failed to delete tapback %s/%s.%d/%s: %v", tapback.ChatGUID, tapback.MessageGUID, tapback.MessagePart, tapback.SenderGUID, err)
	}
}
