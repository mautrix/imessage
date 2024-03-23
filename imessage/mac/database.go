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
	"errors"
	"fmt"
	"sync"

	"maunium.net/go/mautrix/id"

	log "maunium.net/go/maulogger/v2"

	"go.mau.fi/mautrix-imessage/imessage"
)

type macOSDatabase struct {
	log    log.Logger
	bridge imessage.Bridge

	chatDBPath                   string
	chatDB                       *sql.DB
	messagesAfterQuery           *sql.Stmt
	messagesBetweenQuery         *sql.Stmt
	messagesBeforeWithLimitQuery *sql.Stmt
	singleMessageQuery           *sql.Stmt
	limitedMessagesQuery         *sql.Stmt
	newMessagesQuery             *sql.Stmt
	newReceiptsQuery             *sql.Stmt
	attachmentsQuery             *sql.Stmt
	chatQuery                    *sql.Stmt
	chatGUIDQuery                *sql.Stmt
	groupActionQuery             *sql.Stmt
	recentChatsQuery             *sql.Stmt
	groupMemberQuery             *sql.Stmt
	Messages                     chan *imessage.Message
	ReadReceipts                 chan *imessage.ReadReceipt
	stopWakeupDetecting          chan struct{}
	stopWatching                 chan struct{}
	stopWait                     sync.WaitGroup

	*ContactStore
}

func NewChatInfoDatabase(log log.Logger) (imessage.ChatInfoAPI, error) {
	mac := &macOSDatabase{
		log: log,
	}

	err := mac.prepareMessages()
	if err != nil {
		return nil, fmt.Errorf("failed to open message database: %w", err)
	}
	err = mac.prepareGroups()
	if err != nil {
		return nil, fmt.Errorf("failed to open group database: %w", err)
	}

	return mac, nil
}

func NewChatDatabase(bridge imessage.Bridge) (imessage.API, error) {
	mac := &macOSDatabase{
		log:    bridge.GetLog().Sub("iMessage").Sub("Mac"),
		bridge: bridge,
	}

	err := mac.prepareMessages()
	if err != nil {
		return nil, fmt.Errorf("failed to open message database: %w", err)
	}
	err = mac.prepareGroups()
	if err != nil {
		return nil, fmt.Errorf("failed to open group database: %w", err)
	}

	mac.ContactStore = NewContactStore()
	err = mac.ContactStore.RequestContactAccess()
	if err != nil {
		mac.log.Errorln("Failed to get contact access:", err)
	} else if mac.ContactStore.HasContactAccess {
		mac.log.Infoln("Contact access is allowed")
	} else {
		mac.log.Warnln("Contact access is not allowed")
	}

	return mac, nil
}

func init() {
	imessage.Implementations["mac"] = NewChatDatabase
}

func (mac *macOSDatabase) PreStartupSyncHook() (resp imessage.StartupSyncHookResponse, err error) {
	return
}

func (mac *macOSDatabase) UnsendMessage(chatID string, targetGUID string, targetPart int) (*imessage.SendResponse, error) {
	return nil, errors.ErrUnsupported
}

func (mac *macOSDatabase) EditMessage(chatID string, targetGUID string, newText string, targetPart int) (*imessage.SendResponse, error) {
	return nil, errors.ErrUnsupported
}

func (mac *macOSDatabase) PostStartupSyncHook() {}

func (mac *macOSDatabase) PrepareDM(guid string) error {
	// Nothing needed here
	return nil
}

func (mac *macOSDatabase) CreateGroup(guids []string) (*imessage.CreateGroupResponse, error) {
	return nil, errors.New("can't create groups on mac connector")
}

func (mac *macOSDatabase) SendMessageBridgeResult(chatID, messageID string, eventID id.EventID, success bool) {
}

func (mac *macOSDatabase) SendBackfillResult(chatID, backfillID string, success bool, idMap map[string][]id.EventID) {
}

func (mac *macOSDatabase) SendChatBridgeResult(guid string, mxid id.RoomID) {}

func (mac *macOSDatabase) Capabilities() imessage.ConnectorCapabilities {
	return imessage.ConnectorCapabilities{
		ContactChatMerging: true,
	}
}
