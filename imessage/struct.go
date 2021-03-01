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

type Contact struct {
	FirstName string
	LastName  string
	Nickname  string
	Avatar    []byte
	AvatarRef string
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

type ChatInfo struct {
	Identifier
	DisplayName string
}

type Identifier struct {
	LocalID string
	Service string

	IsGroup bool
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
