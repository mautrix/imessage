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

package imessage

import (
	"errors"
	"fmt"
	"time"

	log "maunium.net/go/maulogger/v2"

	"go.mau.fi/mautrix-imessage/ipc"
)

var (
	ErrNotLoggedIn = errors.New("you're not logged into iMessage")
)

type API interface {
	Start() error
	Stop()
	GetMessagesSinceDate(chatID string, minDate time.Time) ([]*Message, error)
	GetMessagesWithLimit(chatID string, limit int) ([]*Message, error)
	GetChatsWithMessagesAfter(minDate time.Time) ([]string, error)
	MessageChan() <-chan *Message
	ReadReceiptChan() <-chan *ReadReceipt
	TypingNotificationChan() <-chan *TypingNotification
	ChatChan() <-chan *ChatInfo
	GetContactInfo(identifier string) (*Contact, error)
	GetChatInfo(chatID string) (*ChatInfo, error)
	GetGroupAvatar(chatID string) (*Attachment, error)

	SendMessage(chatID, text string) (*SendResponse, error)
	SendFile(chatID, filename string, data []byte) (*SendResponse, error)
	SendTapback(chatID, targetGUID string, targetPart int, tapback TapbackType, remove bool) (*SendResponse, error)
	SendReadReceipt(chatID, readUpTo string) error
	SendTypingNotification(chatID string, typing bool) error

	Capabilities() ConnectorCapabilities
}

type Bridge interface {
	GetIPC() *ipc.Processor
	GetLog() log.Logger
	GetConnectorConfig() *PlatformConfig
	PingServer() (start, serverTs, end time.Time)
}

var AppleEpoch = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
var Implementations = make(map[string]func(Bridge) (API, error))

type PlatformConfig struct {
	Platform string `yaml:"platform"`

	IMRestPath string `yaml:"imessage_rest_path"`
}

func (pc *PlatformConfig) BridgeName() string {
	switch pc.Platform {
	case "android":
		return "Android SMS Bridge"
	default:
		return "iMessage Bridge"
	}
}

func NewAPI(bridge Bridge) (API, error) {
	cfg := bridge.GetConnectorConfig()
	impl, ok := Implementations[cfg.Platform]
	if !ok {
		return nil, fmt.Errorf("no such platform \"%s\"", cfg.Platform)
	}
	return impl(bridge)
}
