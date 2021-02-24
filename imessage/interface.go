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
	"fmt"
	"time"
)

type API interface {
	Start() error
	Stop()
	GetMessagesSinceDate(chatID string, minDate time.Time) ([]*Message, error)
	GetMessagesWithLimit(chatID string, limit int) ([]*Message, error)
	GetChatsWithMessagesAfter(minDate time.Time) ([]string, error)
	MessageChan() <-chan *Message
	GetContactInfo(identifier string) (*Contact, error)
	GetGroupMembers(chatID string) ([]string, error)
	GetChatInfo(chatID string) (*ChatInfo, error)

	SendMessage(chatID, text string) error
	SendFile(chatID, filename string, data []byte) error
}

var AppleEpoch = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
var Implementations = make(map[string]func() (API, error))

type PlatformConfig struct {
	Platform string `yaml:"platform"`
}

func NewAPI(cfg *PlatformConfig) (API, error) {
	impl, ok := Implementations[cfg.Platform]
	if !ok {
		return nil, fmt.Errorf("no such platform \"%s\"", cfg.Platform)
	}
	return impl()
}
