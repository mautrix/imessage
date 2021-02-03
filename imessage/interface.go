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
	"strings"
	"time"
)

type API interface {
	Start() error
	Stop()
	GetMessages(chatID string, minDate time.Time) ([]*Message, error)
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

type Contact struct {
	FirstName string
	LastName  string
	Avatar    []byte
	Phones    []string
	Emails    []string
}

func (contact *Contact) Name() string {
	if len(contact.FirstName) > 0 {
		if len(contact.LastName) > 0 {
			return fmt.Sprintf("%s %s", contact.FirstName, contact.LastName)
		} else {
			return contact.FirstName
		}
	} else if len(contact.LastName) > 0 {
		return contact.LastName
	} else if len(contact.Emails) > 0 {
		return contact.Emails[0]
	} else if len(contact.Phones) > 0 {
		return contact.Phones[0]
	}
	return ""
}

type Attachment interface {
	GetMimeType() string
	GetFileName() string
	Read() ([]byte, error)
}

type Identifier struct {
	LocalID string
	Service string

	IsGroup bool
}

type ChatInfo struct {
	Identifier
	DisplayName string
}

func ParseIdentifier(guid string) Identifier {
	parts := strings.Split(guid, ";")
	return Identifier{
		Service: parts[0],
		IsGroup: parts[1] == "+",
		LocalID: parts[2],
	}
}

func (id Identifier) String() string {
	typeChar := '-'
	if id.IsGroup {
		typeChar = '+'
	}
	return fmt.Sprintf("%s;%c;%s", id.Service, typeChar, id.LocalID)
}

type Message struct {
	GUID     string
	Time     time.Time
	Subject  string
	Text     string
	Service  string
	ChatGUID string
	Chat     Identifier
	Sender   Identifier

	IsFromMe       bool
	IsRead         bool
	IsDelivered    bool
	IsSent         bool
	IsEmote        bool
	IsAudioMessage bool

	ReplyToGUID string
	Tapback     *Tapback

	Attachment Attachment
}

type TapbackType int

func TapbackFromEmoji(emoji string) TapbackType {
	if strings.HasSuffix(emoji, "\ufe0f") {
		emoji = emoji[:len(emoji)-1]
	}
	switch emoji {
	case "\u2665", "\u2764", "\U0001f499", "\U0001f49a", "\U0001f90e", "\U0001f5a4", "\U0001f90d", "\U0001f9e1",
		"\U0001f49b", "\U0001f49c", "\U0001f496", "\u2763", "\U0001f495", "\U0001f49f":
		// "♥", "❤", "💙", "💚", "🤎", "🖤", "🤍", "🧡", "💛", "💜", "💖", "❣", "💕", "💟"
		return TapbackLove
	case "\U0001f44d": // "👍"
		return TapbackLike
	case "\U0001f44e": // "👎"
		return TapbackDislike
	case "\U0001f602", "\U0001f639", "\U0001f606", "\U0001f923": // "😂", "😹", "😆", "🤣"
		return TapbackLaugh
	case "\u2755", "\u2757", "\u203c": // "❕", "❗", "‼",
		return TapbackEmphasis
	case "\u2753", "\u2754": // "❓", "❔"
		return TapbackQuestion
	default:
		return 0
	}
}

func (amt TapbackType) String() string {
	return amt.Emoji()
}

func (amt TapbackType) Emoji() string {
	switch amt {
	case 0:
		return ""
	case TapbackLove:
		return "\u2764\ufe0f" // "❤️"
	case TapbackLike:
		return "\U0001f44d\ufe0f" // "👍️"
	case TapbackDislike:
		return "\U0001f44e\ufe0f" // "👎️"
	case TapbackLaugh:
		return "\U0001f602" // "😂"
	case TapbackEmphasis:
		return "\u203c\ufe0f" // "‼️"
	case TapbackQuestion:
		return "\u2753\ufe0f" // "❓️"
	default:
		return "\ufffd" // "�"
	}
}

const (
	TapbackLove TapbackType = iota + 2000
	TapbackLike
	TapbackDislike
	TapbackLaugh
	TapbackEmphasis
	TapbackQuestion
)

type Tapback struct {
	TargetGUID string
	Remove     bool
	Type       TapbackType
}

func (tapback *Tapback) Parse() *Tapback {
	if tapback.Type >= 3000 && tapback.Type < 4000 {
		tapback.Type -= 1000
		tapback.Remove = true
	}
	tapback.TargetGUID = strings.Split(tapback.TargetGUID, "/")[1]
	return tapback
}
