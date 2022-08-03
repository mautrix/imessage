// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2022 Tulir Asokan
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
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gabriel-vasile/mimetype"
	log "maunium.net/go/maulogger/v2"
)

type Message struct {
	RowID int `json:"-"`

	GUID           string     `json:"guid"`
	Time           time.Time  `json:"-"`
	JSONUnixTime   float64    `json:"timestamp"`
	Subject        string     `json:"subject"`
	Text           string     `json:"text"`
	ChatGUID       string     `json:"chat_guid"`
	JSONSenderGUID string     `json:"sender_guid"`
	Sender         Identifier `json:"-"`
	JSONTargetGUID string     `json:"target_guid"`
	Target         Identifier `json:"-"`

	// The service from the sender_guid is extracted and stored here.
	Service string `json:"service"`

	IsFromMe       bool      `json:"is_from_me"`
	IsRead         bool      `json:"is_read"`
	ReadAt         time.Time `json:"-"`
	JSONUnixReadAt float64   `json:"read_at"`
	IsDelivered    bool
	IsSent         bool
	IsEmote        bool
	IsAudioMessage bool `json:"is_audio_message"`

	ReplyToGUID string   `json:"thread_originator_guid,omitempty"`
	ReplyToPart int      `json:"thread_originator_part,omitempty"`
	Tapback     *Tapback `json:"associated_message,omitempty"`

	// Deprecated: use attachments array
	Attachment *Attachment `json:"attachment,omitempty"`

	Attachments []*Attachment `json:"attachments,omitempty"`

	ErrorNotice string `json:"error_notice,omitempty"`

	ItemType        ItemType        `json:"item_type,omitempty"`
	GroupActionType GroupActionType `json:"group_action_type,omitempty"`
	NewGroupName    string          `json:"new_group_title,omitempty"`

	RichLink *RichLink `json:"rich_link,omitempty"`

	Metadata MessageMetadata `json:"metadata,omitempty"`

	SenderCorrelationID string `json:"sender_correlation_id,omitempty"`
	CorrelationID       string `json:"correlation_id,omitempty"`
}

type MessageMetadata = map[string]interface{}

func (msg *Message) SenderText() string {
	if msg.IsFromMe {
		return "self"
	}
	return msg.Sender.LocalID
}

type ReadReceipt struct {
	SenderGUID string `json:"sender_guid"`
	IsFromMe   bool   `json:"is_from_me"`
	ChatGUID   string `json:"chat_guid"`
	ReadUpTo   string `json:"read_up_to"`

	ReadAt         time.Time `json:"-"`
	JSONUnixReadAt float64   `json:"read_at"`

	SenderCorrelationID string `json:"sender_correlation_id,omitempty"`
	CorrelationID       string `json:"correlation_id,omitempty"`
}

type TypingNotification struct {
	ChatGUID      string `json:"chat_guid"`
	Typing        bool   `json:"typing"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

type GroupActionType int

const (
	GroupActionAddUser    GroupActionType = 0
	GroupActionRemoveUser GroupActionType = 1

	GroupActionSetAvatar    GroupActionType = 1
	GroupActionRemoveAvatar GroupActionType = 2
)

type ItemType int

const (
	ItemTypeMessage ItemType = iota
	ItemTypeMember
	ItemTypeName
	ItemTypeAvatar

	ItemTypeError ItemType = -100
)

type Contact struct {
	FirstName string   `json:"first_name,omitempty"`
	LastName  string   `json:"last_name,omitempty"`
	Nickname  string   `json:"nickname,omitempty"`
	Avatar    []byte   `json:"avatar,omitempty"`
	Phones    []string `json:"phones,omitempty"`
	Emails    []string `json:"emails,omitempty"`
	UserGUID  string   `json:"user_guid,omitempty"`

	PrimaryIdentifier string `json:"primary_identifier,omitempty"`
	CorrelationID     string `json:"correlation_id,omitempty"`
}

func (contact *Contact) HasName() bool {
	return len(contact.FirstName) > 0 || len(contact.LastName) > 0 || len(contact.Nickname) > 0
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
	} else if len(contact.Nickname) > 0 {
		return contact.Nickname
	} else if len(contact.Emails) > 0 {
		return contact.Emails[0]
	} else if len(contact.Phones) > 0 {
		return contact.Phones[0]
	}
	return ""
}

type Attachment struct {
	GUID       string `json:"guid,omitempty"`
	PathOnDisk string `json:"path_on_disk"`
	FileName   string `json:"file_name"`
	MimeType   string `json:"mime_type,omitempty"`
	triedMagic bool
}

func (attachment *Attachment) GetMimeType() string {
	if attachment.MimeType == "" {
		if attachment.triedMagic {
			return ""
		}
		attachment.triedMagic = true
		mime, err := mimetype.DetectFile(attachment.PathOnDisk)
		if err != nil {
			log.DefaultLogger.Warnfln("Failed to detect mime type from %s: %v", attachment.PathOnDisk, err)
			return ""
		}
		attachment.MimeType = mime.String()
	}
	return attachment.MimeType
}

func (attachment *Attachment) GetFileName() string {
	return attachment.FileName
}

func (attachment *Attachment) Read() ([]byte, error) {
	if strings.HasPrefix(attachment.PathOnDisk, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("failed to get home directory: %w", err)
		}
		attachment.PathOnDisk = filepath.Join(home, attachment.PathOnDisk[2:])
	}
	return os.ReadFile(attachment.PathOnDisk)
}

type ChatInfo struct {
	JSONChatGUID  string `json:"chat_guid"`
	Identifier    `json:"-"`
	DisplayName   string   `json:"title"`
	Members       []string `json:"members"`
	NoCreateRoom  bool     `json:"no_create_room"`
	CorrelationID string   `json:"correlation_id,omitempty"`
}

type Identifier struct {
	LocalID string
	Service string

	IsGroup bool
}

func ParseIdentifier(guid string) Identifier {
	if len(guid) == 0 {
		return Identifier{}
	}
	parts := strings.Split(guid, ";")
	return Identifier{
		Service: parts[0],
		IsGroup: parts[1] == "+",
		LocalID: parts[2],
	}
}

func (id Identifier) String() string {
	if len(id.LocalID) == 0 {
		return ""
	}
	typeChar := '-'
	if id.IsGroup {
		typeChar = '+'
	}
	return fmt.Sprintf("%s;%c;%s", id.Service, typeChar, id.LocalID)
}

type SendResponse struct {
	GUID     string    `json:"guid"`
	Service  string    `json:"service"`
	Time     time.Time `json:"-"`
	UnixTime float64   `json:"timestamp"`
}

type ConnectorCapabilities struct {
	MessageSendResponses     bool
	SendTapbacks             bool
	SendReadReceipts         bool
	SendTypingNotifications  bool
	SendCaptions             bool
	BridgeState              bool
	MessageStatusCheckpoints bool
	MergedChats              bool
	RichLinks                bool
	Correlation              bool
	ChatBridgeResult         bool
}

type PushKeyRequest struct {
	URL string `json:"url"`

	AppID     string `json:"app_id"`
	PushKey   string `json:"pushkey"`
	PushKeyTS int64  `json:"pushkey_ts"`

	Data map[string]interface{} `json:"data"`
}

type SendMessageStatus struct {
	GUID                string `json:"guid"`
	ChatGUID            string `json:"chat_guid"`
	Status              string `json:"status"`
	Service             string `json:"service"`
	Message             string `json:"message,omitempty"`
	StatusCode          string `json:"status_code,omitempty"`
	CorrelationID       string `json:"correlation_id,omitempty"`
	SenderCorrelationID string `json:"sender_correlation_id,omitempty"`
}

type StartupSyncHookResponse struct {
	SkipSync bool `json:"skip_sync"`
}

type RichLinkAsset struct {
	MimeType          string               `json:"mimeType,omitempty"`
	AccessibilityText string               `json:"accessibilityText,omitempty"`
	Source            *RichLinkAssetSource `json:"source,omitempty"`
	OriginalURL       string               `json:"originalURL,omitempty"`
	Size              *RichLinkAssetSize   `json:"size,omitempty"`
}

type RichLinkAssetSource struct {
	URL  string `json:"url,omitempty"`
	Data []byte `json:"data,omitempty"`
}

type RichLinkAssetSize struct {
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

type RichLinkVideoAsset struct {
	HasAudio     bool          `json:"hasAudio,omitempty"`
	YouTubeURL   string        `json:"youTubeURL,omitempty"`
	StreamingURL string        `json:"streamingURL,omitempty"`
	Asset        RichLinkAsset `json:"asset"`
}

type RichLink struct {
	OriginalURL            string              `json:"originalURL,omitempty"`
	URL                    string              `json:"URL,omitempty"`
	Title                  string              `json:"title,omitempty"`
	Summary                string              `json:"summary,omitempty"`
	SelectedText           string              `json:"selectedText,omitempty"`
	SiteName               string              `json:"siteName,omitempty"`
	RelatedURL             string              `json:"relatedURL,omitempty"`
	Creator                string              `json:"creator,omitempty"`
	CreatorFacebookProfile string              `json:"creatorFacebookProfile,omitempty"`
	CreatorTwitterUsername string              `json:"creatorTwitterUsername,omitempty"`
	ItemType               string              `json:"itemType,omitempty"`
	Icon                   *RichLinkAsset      `json:"icon,omitempty"`
	Image                  *RichLinkAsset      `json:"image,omitempty"`
	Video                  *RichLinkVideoAsset `json:"video,omitempty"`
	Audio                  *RichLinkAsset      `json:"audio,omitempty"`
	Images                 []RichLinkAsset     `json:"images,omitempty"`
	Videos                 []RichLinkAsset     `json:"videos,omitempty"`
	StreamingVideos        []RichLinkAsset     `json:"streamingVideos,omitempty"`
	Audios                 []RichLinkAsset     `json:"audios,omitempty"`
}
