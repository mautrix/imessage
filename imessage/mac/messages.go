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
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gabriel-vasile/mimetype"
	_ "github.com/mattn/go-sqlite3"
	log "maunium.net/go/maulogger/v2"

	"go.mau.fi/mautrix-imessage/imessage"
)

const baseMessagesQuery = `
SELECT
  message.guid, message.date, COALESCE(message.subject, ''), COALESCE(message.text, ''), COALESCE(message.service, ''), chat.guid,
  chat.chat_identifier, chat.service_name, COALESCE(handle.id, ''), COALESCE(handle.service, ''),
  message.is_from_me, message.is_read, message.is_delivered, message.is_sent, message.is_emote, message.is_audio_message,
  COALESCE(message.thread_originator_guid, ''), COALESCE(message.associated_message_guid, ''), message.associated_message_type,
  COALESCE(attachment.filename, ''), attachment.mime_type, COALESCE(attachment.transfer_name, ''),
  message.group_title, message.group_action_type
FROM message
JOIN chat_message_join ON chat_message_join.message_id = message.ROWID
JOIN chat              ON chat_message_join.chat_id = chat.ROWID
LEFT JOIN handle       ON message.handle_id = handle.ROWID
LEFT JOIN message_attachment_join ON message_attachment_join.message_id = message.ROWID
LEFT JOIN attachment              ON message_attachment_join.attachment_id = attachment.ROWID
`

var messagesQuery = baseMessagesQuery + `
WHERE (chat.guid=$1 OR $1='') AND message.date>$2
ORDER BY message.date ASC
`

var limitedMessagesQuery = baseMessagesQuery + `
WHERE (chat.guid=$1 OR $1='')
ORDER BY message.date DESC
LIMIT $2
`

const groupActionQuery = `
SELECT attachment.filename, attachment.mime_type, attachment.transfer_name
FROM message
JOIN chat_message_join ON chat_message_join.message_id = message.ROWID
JOIN chat              ON chat_message_join.chat_id = chat.ROWID
LEFT JOIN message_attachment_join ON message_attachment_join.message_id = message.ROWID
LEFT JOIN attachment              ON message_attachment_join.attachment_id = attachment.ROWID
WHERE message.group_action_type=$1 AND chat.guid=$2
ORDER BY message.date DESC LIMIT 1
`

const chatQuery = `
SELECT chat_identifier, service_name, display_name FROM chat WHERE guid=$1
`

const recentChatsQuery = `
SELECT DISTINCT(chat.guid) FROM message
JOIN chat_message_join ON chat_message_join.message_id = message.ROWID
JOIN chat              ON chat_message_join.chat_id = chat.ROWID
WHERE message.date>$1
`

func (imdb *Database) prepareMessages() error {
	path, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	imdb.chatDBPath = filepath.Join(path, "Library", "Messages", "chat.db")
	imdb.chatDB, err = sql.Open("sqlite3", fmt.Sprintf("file:%s?mode=ro", imdb.chatDBPath))
	if err != nil {
		return err
	}
	if !columnExists(imdb.chatDB, "message", "thread_originator_guid") {
		messagesQuery = strings.ReplaceAll(messagesQuery, "COALESCE(message.thread_originator_guid, '')", "''")
		limitedMessagesQuery = strings.ReplaceAll(limitedMessagesQuery, "COALESCE(message.thread_originator_guid, '')", "''")
	}
	if !columnExists(imdb.chatDB, "message", "group_action_type") {
		messagesQuery = strings.ReplaceAll(messagesQuery, "message.group_action_type", "0")
		limitedMessagesQuery = strings.ReplaceAll(limitedMessagesQuery, "message.group_action_type", "0")
	}
	imdb.messagesQuery, err = imdb.chatDB.Prepare(messagesQuery)
	if err != nil {
		return fmt.Errorf("failed to prepare message query: %w", err)
	}
	imdb.groupActionQuery, err = imdb.chatDB.Prepare(groupActionQuery)
	if err != nil {
		imdb.log.Warnln("Failed to prepare group action query:", err)
		imdb.groupActionQuery = nil
	}
	imdb.limitedMessagesQuery, err = imdb.chatDB.Prepare(limitedMessagesQuery)
	if err != nil {
		return fmt.Errorf("failed to prepare limited message query: %w", err)
	}
	imdb.chatQuery, err = imdb.chatDB.Prepare(chatQuery)
	if err != nil {
		return fmt.Errorf("failed to prepare chat query: %w", err)
	}
	imdb.recentChatsQuery, err = imdb.chatDB.Prepare(recentChatsQuery)
	if err != nil {
		return fmt.Errorf("failed to prepare recent chats query: %w", err)
	}

	messageChan := make(chan *imessage.Message)
	imdb.Messages = messageChan
	return nil
}

type AttachmentInfo struct {
	FileName     string
	MimeType     sql.NullString
	triedMagic bool
	TransferName string
}

func (ai *AttachmentInfo) GetMimeType() string {
	if !ai.MimeType.Valid {
		if ai.triedMagic {
			return ""
		}
		ai.triedMagic = true
		mime, err := mimetype.DetectFile(ai.FileName)
		if err != nil {
			log.DefaultLogger.Warnfln("Failed to detect mime type from %s: %v", ai.FileName, err)
			return ""
		}
		ai.MimeType.String = mime.String()
		ai.MimeType.Valid = true
	}
	return ai.MimeType.String
}

func (ai *AttachmentInfo) GetFileName() string {
	return ai.TransferName
}

func (ai *AttachmentInfo) Read() ([]byte, error) {
	if strings.HasPrefix(ai.FileName, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("failed to get home directory: %w", err)
		}
		ai.FileName = filepath.Join(home, ai.FileName[2:])
	}
	return ioutil.ReadFile(ai.FileName)
}

func (imdb *Database) scanMessages(res *sql.Rows) (messages []*imessage.Message, err error) {
	for res.Next() {
		var message imessage.Message
		var tapback imessage.Tapback
		var attachment AttachmentInfo
		var timestamp int64
		var newGroupTitle sql.NullString
		err = res.Scan(&message.GUID, &timestamp, &message.Subject, &message.Text, &message.Service, &message.ChatGUID,
			&message.Chat.LocalID, &message.Chat.Service, &message.Sender.LocalID, &message.Sender.Service,
			&message.IsFromMe, &message.IsRead, &message.IsDelivered, &message.IsSent, &message.IsEmote, &message.IsAudioMessage,
			&message.ReplyToGUID, &tapback.TargetGUID, &tapback.Type,
			&attachment.FileName, &attachment.MimeType, &attachment.TransferName,
			&newGroupTitle, &message.GroupActionType)
		if err != nil {
			err = fmt.Errorf("error scanning row: %w", err)
			return
		}
		message.Time = time.Unix(imessage.AppleEpoch.Unix(), timestamp)
		if len(attachment.FileName) > 0 {
			message.Attachment = &attachment
		}
		if newGroupTitle.Valid {
			message.GroupActionType = imessage.GroupActionSetName
			message.NewGroupName = newGroupTitle.String
		}
		if len(tapback.TargetGUID) > 0 {
			message.Tapback, err = tapback.Parse()
			if err != nil {
				imdb.log.Warnfln("Failed to parse tapback in %s: %v", message.GUID, err)
			}
		}
		messages = append(messages, &message)
	}
	return
}

func reverseArray(messages []*imessage.Message) {
	for left, right := 0, len(messages)-1; left < right; left, right = left+1, right-1 {
		messages[left], messages[right] = messages[right], messages[left]
	}
}

func columnExists(db *sql.DB, table, column string) bool {
	row := db.QueryRow(fmt.Sprintf(`SELECT name FROM pragma_table_info("%s") WHERE name=$1;`, table), column)
	var name string
	_ = row.Scan(&name)
	return name == column
}

func (imdb *Database) GetMessagesWithLimit(chatID string, limit int) ([]*imessage.Message, error) {
	res, err := imdb.limitedMessagesQuery.Query(chatID, limit)
	if err != nil {
		return nil, fmt.Errorf("error querying messages with limit: %w", err)
	}
	messages, err := imdb.scanMessages(res)
	if err != nil {
		return messages, err
	}
	reverseArray(messages)
	return messages, err
}

func (imdb *Database) GetMessagesSinceDate(chatID string, minDate time.Time) ([]*imessage.Message, error) {
	res, err := imdb.messagesQuery.Query(chatID, minDate.UnixNano()-imessage.AppleEpoch.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("error querying messages after date: %w", err)
	}
	return imdb.scanMessages(res)
}

func (imdb *Database) GetChatsWithMessagesAfter(minDate time.Time) ([]string, error) {
	res, err := imdb.recentChatsQuery.Query(minDate.UnixNano() - imessage.AppleEpoch.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("error querying chats with messages after date: %w", err)
	}
	var chats []string
	for res.Next() {
		var chatID string
		err = res.Scan(&chatID)
		if err != nil {
			return chats, fmt.Errorf("error scanning row: %w", err)
		}
		chats = append(chats, chatID)
	}
	return chats, nil
}

func (imdb *Database) GetChatInfo(chatID string) (*imessage.ChatInfo, error) {
	row := imdb.chatQuery.QueryRow(chatID)
	var info imessage.ChatInfo
	info.Identifier = imessage.ParseIdentifier(chatID)
	err := row.Scan(&info.Identifier.LocalID, &info.Identifier.Service, &info.DisplayName)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &info, err
}

func (imdb *Database) GetGroupAvatar(chatID string) (imessage.Attachment, error) {
	if imdb.groupActionQuery == nil {
		return nil, nil
	}
	row := imdb.groupActionQuery.QueryRow(imessage.GroupActionSetAvatar, chatID)
	var avatar AttachmentInfo
	err := row.Scan(&avatar.FileName, &avatar.MimeType, &avatar.TransferName)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &avatar, err
}

func (imdb *Database) Stop() {
	imdb.stopWatching <- struct{}{}
}

func (imdb *Database) MessageChan() <-chan *imessage.Message {
	return imdb.Messages
}

func (imdb *Database) Start() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to create fsnotify watcher: %w", err)
	}
	defer watcher.Close()

	stop := make(chan struct{}, 1)
	imdb.stopWatching = stop

	err = watcher.Add(filepath.Dir(imdb.chatDBPath))
	if err != nil {
		return fmt.Errorf("failed to add chat DB to fsnotify watcher: %w", err)
	}

	var dropEvents bool
	var handleLock sync.Mutex
	nonSentMessages := make(map[string]bool)
	lastMessageTimestamp := time.Now()
Loop:
	for {
		select {
		case _, ok := <-watcher.Events:
			if !ok {
				break Loop
			} else if dropEvents {
				continue
			}
			dropEvents = true
			go func() {
				handleLock.Lock()
				defer handleLock.Unlock()
				time.Sleep(50 * time.Millisecond)
				newMessages, err := imdb.GetMessagesSinceDate("", lastMessageTimestamp)
				if err != nil {
					// TODO use proper logger
					fmt.Println("Error reading messages after fsevent:", err)
					//return fmt.Errorf("error reading messages after fsevent: %w", err)
				}
				dropEvents = false
				for _, message := range newMessages {
					if message.Time.After(lastMessageTimestamp) {
						lastMessageTimestamp = message.Time
					}

					if !message.IsSent {
						nonSentMessages[message.GUID] = true
					} else if _, ok := nonSentMessages[message.GUID]; ok {
						delete(nonSentMessages, message.GUID)
						continue
					}

					imdb.Messages <- message
				}
			}()
		case err := <-watcher.Errors:
			return fmt.Errorf("error in watcher: %w", err)
		case <-stop:
			break Loop
		}
	}
	return nil
}
