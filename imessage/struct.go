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
	RowID int `json:"-"`

	GUID     string    `json:"guid"`
	Time     time.Time `json:"-"`
	UnixTime float64   `json:"timestamp"`
	Subject  string    `json:"subject"`
	Text     string    `json:"text"`
	//Service  string `json:"service"`
	ChatGUID   string `json:"chat_guid"`
	SenderGUID string `json:"sender_guid"`
	//Chat     Identifier
	Sender Identifier

	IsFromMe       bool `json:"is_from_me"`
	IsRead         bool `json:"is_read"`
	IsDelivered    bool
	IsSent         bool
	IsEmote        bool
	IsAudioMessage bool

	ReplyToGUID string   `json:"thread_originator_guid,omitempty"`
	Tapback     *Tapback `json:"associated_message,omitempty"`

	Attachment Attachment `json:"attachment,omitempty"`

	GroupActionType GroupActionType `json:"group_action_type,omitempty"`
	NewGroupName    string          `json:"new_group_title,omitempty"`
}

type GroupActionType int

const (
	GroupActionNone GroupActionType = iota
	GroupActionSetAvatar
	GroupActionRemoveAvatar

	// Internal types

	GroupActionSetName GroupActionType = iota + 0xff
)

type Contact struct {
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
	Nickname  string `json:"nickname,omitempty"`
	Avatar    []byte
	AvatarB64 string   `json:"avatar,omitempty"`
	Phones    []string `json:"phones,omitempty"`
	Emails    []string `json:"emails,omitempty"`
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
	Identifier  `json:"-"`
	DisplayName string   `json:"title"`
	Members     []string `json:"members"`
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

type SendResponse struct {
	GUID     string    `json:"guid"`
	Time     time.Time `json:"-"`
	UnixTime float64   `json:"timestamp"`
}
