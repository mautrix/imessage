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
	"errors"
	"fmt"
	"strings"
	"time"

	log "maunium.net/go/maulogger/v2"

	"go.mau.fi/util/dbutil"
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

func (mq *MessageQuery) GetIDsSince(chat string, since time.Time) (messages []string) {
	rows, err := mq.db.Query("SELECT guid FROM message WHERE portal_guid=$1 AND timestamp>=$2 AND part=0 ORDER BY timestamp ASC", chat, since.Unix()*1000)
	if err != nil || rows == nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var msgID string
		err = rows.Scan(&msgID)
		if err != nil {
			mq.log.Errorln("Database scan failed:", err)
		} else {
			messages = append(messages, msgID)
		}
	}
	return
}

func (mq *MessageQuery) GetLastByGUID(chat string, guid string) *Message {
	return mq.get("SELECT portal_guid, guid, part, mxid, sender_guid, handle_guid, timestamp "+
		"FROM message WHERE portal_guid=$1 AND guid=$2 ORDER BY part DESC LIMIT 1", chat, guid)
}

func (mq *MessageQuery) FindChatByGUID(guid string) (chatGUID string) {
	err := mq.db.QueryRow("SELECT portal_guid FROM message WHERE guid=$1", guid).Scan(&chatGUID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		mq.log.Errorfln("Failed to find chat by GUID: %v", err)
	}
	return
}

func (mq *MessageQuery) GetByGUID(chat string, guid string, part int) *Message {
	return mq.get("SELECT portal_guid, guid, part, mxid, sender_guid, handle_guid, timestamp "+
		"FROM message WHERE portal_guid=$1 AND guid=$2 AND part=$3", chat, guid, part)
}

func (mq *MessageQuery) GetByMXID(mxid id.EventID) *Message {
	return mq.get("SELECT portal_guid, guid, part, mxid, sender_guid, handle_guid, timestamp "+
		"FROM message WHERE mxid=$1", mxid)
}

func (mq *MessageQuery) GetLastInChat(chat string) *Message {
	msg := mq.get("SELECT portal_guid, guid, part, mxid, sender_guid, handle_guid, timestamp "+
		"FROM message WHERE portal_guid=$1 ORDER BY timestamp DESC LIMIT 1", chat)
	if msg == nil || msg.Timestamp == 0 {
		// Old db, we don't know what the last message is.
		return nil
	}
	return msg
}

func (mq *MessageQuery) GetFirstInChat(chat string) *Message {
	msg := mq.get("SELECT portal_guid, guid, part, mxid, sender_guid, handle_guid, timestamp "+
		"FROM message WHERE portal_guid=$1 ORDER BY timestamp ASC LIMIT 1", chat)
	if msg == nil || msg.Timestamp == 0 {
		// Old db, we don't know what the first message is.
		return nil
	}
	return msg
}

func (mq *MessageQuery) GetEarliestTimestampInChat(chat string) (int64, error) {
	row := mq.db.QueryRow("SELECT MIN(timestamp) FROM message WHERE portal_guid=$1", chat)
	var timestamp sql.NullInt64
	if err := row.Scan(&timestamp); err != nil {
		return -1, err
	} else if !timestamp.Valid {
		return -1, nil
	} else {
		return timestamp.Int64, nil
	}
}

func (mq *MessageQuery) MergePortalGUID(txn dbutil.Execable, to string, from ...string) int64 {
	if txn == nil {
		txn = mq.db
	}
	args := make([]any, len(from)+1)
	args[0] = to
	for i, fr := range from {
		args[i+1] = fr
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(from)), ",")
	res, err := txn.Exec(fmt.Sprintf("UPDATE message SET portal_guid=? WHERE portal_guid IN (%s)", placeholders), args...)
	if err != nil {
		mq.log.Errorfln("Failed to update portal GUID for messages (%v -> %s): %v", err, from, to)
		return -1
	} else {
		affected, err := res.RowsAffected()
		if err != nil {
			mq.log.Warnfln("Failed to get number of rows affected by merge: %v", err)
		}
		return affected
	}
}

func (mq *MessageQuery) SplitPortalGUID(txn dbutil.Execable, fromHandle, fromPortal, to string) int64 {
	if txn == nil {
		txn = mq.db
	}
	res, err := txn.Exec("UPDATE message SET portal_guid=?1 WHERE portal_guid=?2 AND handle_guid=?3", to, fromPortal, fromHandle)
	if err != nil {
		mq.log.Errorfln("Failed to split portal GUID for messages (%s in %s -> %s): %v", fromHandle, fromPortal, to, err)
		return -1
	} else {
		affected, err := res.RowsAffected()
		if err != nil {
			mq.log.Warnfln("Failed to get number of rows affected by split: %v", err)
		}
		return affected
	}
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

	PortalGUID string
	GUID       string
	Part       int
	MXID       id.EventID
	SenderGUID string
	HandleGUID string
	Timestamp  int64
}

func (msg *Message) Time() time.Time {
	return time.UnixMilli(msg.Timestamp)
}

func (msg *Message) Scan(row dbutil.Scannable) *Message {
	err := row.Scan(&msg.PortalGUID, &msg.GUID, &msg.Part, &msg.MXID, &msg.SenderGUID, &msg.HandleGUID, &msg.Timestamp)
	if err != nil {
		if err != sql.ErrNoRows {
			msg.log.Errorln("Database scan failed:", err)
		}
		return nil
	}
	return msg
}

func (msg *Message) Insert(txn dbutil.Execable) {
	if txn == nil {
		txn = msg.db
	}
	_, err := txn.Exec("INSERT INTO message (portal_guid, guid, part, mxid, sender_guid, handle_guid, timestamp) VALUES ($1, $2, $3, $4, $5, $6, $7)",
		msg.PortalGUID, msg.GUID, msg.Part, msg.MXID, msg.SenderGUID, msg.HandleGUID, msg.Timestamp)
	if err != nil {
		msg.log.Warnfln("Failed to insert %s.%d@%s: %v", msg.GUID, msg.Part, msg.PortalGUID, err)
	}
}

func (msg *Message) Delete() {
	_, err := msg.db.Exec("DELETE FROM message WHERE portal_guid=$1 AND guid=$2", msg.PortalGUID, msg.GUID)
	if err != nil {
		msg.log.Warnfln("Failed to delete %s.%d@%s: %v", msg.GUID, msg.Part, msg.PortalGUID, err)
	}
}
