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
	"time"

	log "maunium.net/go/maulogger/v2"
	"maunium.net/go/mautrix/id"
)

type MessageQuery struct {
	db  *Database
	log log.Logger
}

func (mq *MessageQuery) New() *Message {
	return &Message{
		db:  mq.db,
		log: mq.log,
	}
}

func (mq *MessageQuery) GetAll(chat string) (messages []*Message) {
	rows, err := mq.db.Query("SELECT chat_guid, guid, part, mxid, sender_guid, timestamp FROM message WHERE chat_guid=$1", chat)
	if err != nil || rows == nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		messages = append(messages, mq.New().Scan(rows))
	}
	return
}

func (mq *MessageQuery) GetLastByGUID(chat string, guid string) *Message {
	return mq.get("SELECT chat_guid, guid, part, mxid, sender_guid, timestamp "+
		"FROM message WHERE chat_guid=$1 AND guid=$2 ORDER BY part DESC LIMIT 1", chat, guid)
}

func (mq *MessageQuery) GetByGUID(chat string, guid string, part int) *Message {
	return mq.get("SELECT chat_guid, guid, part, mxid, sender_guid, timestamp "+
		"FROM message WHERE chat_guid=$1 AND guid=$2 AND part=$3", chat, guid, part)
}

func (mq *MessageQuery) GetByMXID(mxid id.EventID) *Message {
	return mq.get("SELECT chat_guid, guid, part, mxid, sender_guid, timestamp "+
		"FROM message WHERE mxid=$1", mxid)
}

func (mq *MessageQuery) GetLastInChat(chat string) *Message {
	msg := mq.get("SELECT chat_guid, guid, part, mxid, sender_guid, timestamp "+
		"FROM message WHERE chat_guid=$1 ORDER BY timestamp DESC LIMIT 1", chat)
	if msg == nil || msg.Timestamp == 0 {
		// Old db, we don't know what the last message is.
		return nil
	}
	return msg
}

func (mq *MessageQuery) get(query string, args ...interface{}) *Message {
	row := mq.db.QueryRow(query, args...)
	if row == nil {
		return nil
	}
	return mq.New().Scan(row)
}

type Message struct {
	db  *Database
	log log.Logger

	ChatGUID   string
	GUID       string
	Part       int
	MXID       id.EventID
	SenderGUID string
	Timestamp  int64
}

func (msg *Message) Time() time.Time {
	// Add 1 ms to avoid rounding down
	return time.Unix(msg.Timestamp/1000, ((msg.Timestamp%1000)+1)*int64(time.Millisecond))
}

func (msg *Message) Scan(row Scannable) *Message {
	err := row.Scan(&msg.ChatGUID, &msg.GUID, &msg.Part, &msg.MXID, &msg.SenderGUID, &msg.Timestamp)
	if err != nil {
		if err != sql.ErrNoRows {
			msg.log.Errorln("Database scan failed:", err)
		}
		return nil
	}
	return msg
}

func (msg *Message) Insert() {
	_, err := msg.db.Exec("INSERT INTO message (chat_guid, guid, part, mxid, sender_guid, timestamp) VALUES ($1, $2, $3, $4, $5, $6)",
		msg.ChatGUID, msg.GUID, msg.Part, msg.MXID, msg.SenderGUID, msg.Timestamp)
	if err != nil {
		msg.log.Warnfln("Failed to insert %s.%d@%s: %v", msg.GUID, msg.Part, msg.ChatGUID, err)
	}
}

func (msg *Message) Delete() {
	_, err := msg.db.Exec("DELETE FROM message WHERE chat_guid=$1 AND guid=$2", msg.ChatGUID, msg.GUID)
	if err != nil {
		msg.log.Warnfln("Failed to delete %s.%d@%s: %v", msg.GUID, msg.Part, msg.ChatGUID, err)
	}
}
