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
	"sync"

	log "maunium.net/go/maulogger/v2"

	"go.mau.fi/mautrix-imessage/imessage"
)

type macOSDatabase struct {
	log    log.Logger
	bridge imessage.Bridge

	chatDBPath           string
	chatDB               *sql.DB
	messagesQuery        *sql.Stmt
	limitedMessagesQuery *sql.Stmt
	newMessagesQuery     *sql.Stmt
	newReceiptsQuery     *sql.Stmt
	attachmentsQuery     *sql.Stmt
	chatQuery            *sql.Stmt
	groupActionQuery     *sql.Stmt
	recentChatsQuery     *sql.Stmt
	Messages             chan *imessage.Message
	ReadReceipts         chan *imessage.ReadReceipt
	stopWakeupDetecting  chan struct{}
	stopWatching         chan struct{}
	stopWait             sync.WaitGroup

	ppDB             *sql.DB
	groupMemberQuery *sql.Stmt

	contactStore *ContactStore
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

	mac.contactStore = NewContactStore()
	err = mac.contactStore.RequestAccess()
	if err != nil {
		mac.log.Errorln("Failed to get contact access:", err)
	} else if mac.contactStore.HasAccess {
		mac.log.Infoln("Contact access is allowed")
	} else {
		mac.log.Warnln("Contact access is not allowed")
	}

	return mac, nil
}

func init() {
	imessage.Implementations["mac"] = NewChatDatabase
}

func (mac *macOSDatabase) Capabilities() imessage.ConnectorCapabilities {
	return imessage.ConnectorCapabilities{
		MessageSendResponses:    false,
		SendTapbacks:            false,
		SendReadReceipts:        false,
		SendTypingNotifications: false,
		BridgeState:             false,
	}
}
