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

package config

import (
	"bytes"
	"strconv"
	"strings"
	"text/template"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type BridgeConfig struct {
	User id.UserID `yaml:"user"`

	UsernameTemplate    string `yaml:"username_template"`
	DisplaynameTemplate string `yaml:"displayname_template"`

	DeliveryReceipts    bool `yaml:"delivery_receipts"`
	MessageStatusEvents bool `yaml:"message_status_events"`
	SendErrorNotices    bool `yaml:"send_error_notices"`

	MaxHandleSeconds int `yaml:"max_handle_seconds"`

	SyncWithCustomPuppets bool    `yaml:"sync_with_custom_puppets"`
	SyncDirectChatList    bool    `yaml:"sync_direct_chat_list"`
	LoginSharedSecret     string  `yaml:"login_shared_secret"`
	DoublePuppetServerURL string  `yaml:"double_puppet_server_url"`
	ChatSyncMaxAge        float64 `yaml:"chat_sync_max_age"`
	InitialBackfillLimit  int     `yaml:"initial_backfill_limit"`
	BackfillDisableNotifs bool    `yaml:"initial_backfill_disable_notifications"`
	PeriodicSync          bool    `yaml:"periodic_sync"`
	FindPortalsIfEmpty    bool    `yaml:"find_portals_if_db_empty"`
	MediaViewerURL        string  `yaml:"media_viewer_url"`
	MediaViewerSMSMinSize int     `yaml:"media_viewer_sms_min_size"`
	MediaViewerIMMinSize  int     `yaml:"media_viewer_imessage_min_size"`
	ConvertHEIF           bool    `yaml:"convert_heif"`
	ConvertVideo          struct {
		Enabled    bool     `yaml:"enabled"`
		FFMPEGArgs []string `yaml:"ffmpeg_args"`
	} `yaml:"convert_video"`

	FederateRooms bool `yaml:"federate_rooms"`

	CommandPrefix string `yaml:"command_prefix"`

	Encryption struct {
		Allow   bool `yaml:"allow"`
		Default bool `yaml:"default"`

		Appservice bool `yaml:"appservice"`

		KeySharing struct {
			Allow               bool `yaml:"allow"`
			RequireCrossSigning bool `yaml:"require_cross_signing"`
			RequireVerification bool `yaml:"require_verification"`
		} `yaml:"key_sharing"`
	} `yaml:"encryption"`

	Relay RelayConfig `yaml:"relay"`

	usernameTemplate    *template.Template `yaml:"-"`
	displaynameTemplate *template.Template `yaml:"-"`
	communityTemplate   *template.Template `yaml:"-"`

	OldMediaViewerMinSize  int  `yaml:"media_viewer_min_size"`
	OldMessageStatusEvents bool `yaml:"send_message_send_status_events"`
}

func (bc *BridgeConfig) setDefaults() {
	bc.DeliveryReceipts = false
	bc.SyncWithCustomPuppets = false
	bc.LoginSharedSecret = ""
	bc.ChatSyncMaxAge = 0.5
	bc.InitialBackfillLimit = 100
	bc.BackfillDisableNotifs = true
	bc.PeriodicSync = true
	bc.FederateRooms = true
	bc.MediaViewerSMSMinSize = 400 * 1024
	bc.MediaViewerIMMinSize = 50 * 1024 * 1024
}

type umBridgeConfig BridgeConfig

func (bc *BridgeConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	err := unmarshal((*umBridgeConfig)(bc))
	if err != nil {
		return err
	}

	bc.usernameTemplate, err = template.New("username").Parse(bc.UsernameTemplate)
	if err != nil {
		return err
	}

	bc.displaynameTemplate, err = template.New("displayname").Parse(bc.DisplaynameTemplate)
	if err != nil {
		return err
	}

	if bc.OldMediaViewerMinSize > 0 {
		bc.MediaViewerSMSMinSize = bc.OldMediaViewerMinSize
	}
	if bc.OldMessageStatusEvents {
		bc.MessageStatusEvents = true
	}

	return nil
}

type UsernameTemplateArgs struct {
	UserID id.UserID
}

func (bc BridgeConfig) FormatDisplayname(name string) string {
	var buf strings.Builder
	bc.displaynameTemplate.Execute(&buf, name)
	return buf.String()
}

func (bc BridgeConfig) FormatUsername(username string) string {
	if strings.HasPrefix(username, "+") {
		if _, err := strconv.Atoi(username[1:]); err == nil {
			username = username[1:]
		}
	} else if username != "(.+)" && username != ".+" {
		username = id.EncodeUserLocalpart(username)
	}
	var buf bytes.Buffer
	bc.usernameTemplate.Execute(&buf, username)
	return buf.String()
}

type RelayConfig struct {
	Enabled        bool                         `yaml:"enabled"`
	Whitelist      []string                     `yaml:"whitelist"`
	MessageFormats map[event.MessageType]string `yaml:"message_formats"`

	messageTemplates *template.Template  `yaml:"-"`
	whitelistMap     map[string]struct{} `yaml:"-"`
	isAllWhitelisted bool                `yaml:"-"`
}

type umRelayConfig RelayConfig

func (rc *RelayConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	err := unmarshal((*umRelayConfig)(rc))
	if err != nil {
		return err
	}

	rc.messageTemplates = template.New("messageTemplates")
	for key, format := range rc.MessageFormats {
		_, err = rc.messageTemplates.New(string(key)).Parse(format)
		if err != nil {
			return err
		}
	}

	rc.whitelistMap = make(map[string]struct{}, len(rc.Whitelist))
	for _, item := range rc.Whitelist {
		rc.whitelistMap[item] = struct{}{}
		if item == "*" {
			rc.isAllWhitelisted = true
		}
	}

	return nil
}

func (rc *RelayConfig) IsWhitelisted(userID id.UserID) bool {
	if !rc.Enabled {
		return false
	} else if rc.isAllWhitelisted {
		return true
	} else if _, ok := rc.whitelistMap[string(userID)]; ok {
		return true
	} else {
		_, homeserver, _ := userID.Parse()
		_, ok = rc.whitelistMap[homeserver]
		return len(homeserver) > 0 && ok
	}
}

type Sender struct {
	UserID string
	event.MemberEventContent
}

type formatData struct {
	Sender   Sender
	Message  string
	FileName string
	Content  *event.MessageEventContent
}

func (rc *RelayConfig) FormatMessage(content *event.MessageEventContent, sender id.UserID, member event.MemberEventContent) (string, error) {
	if len(member.Displayname) == 0 {
		member.Displayname = sender.String()
	}
	var formatted strings.Builder
	err := rc.messageTemplates.ExecuteTemplate(&formatted, string(content.MsgType), formatData{
		Sender: Sender{
			UserID:             sender.String(),
			MemberEventContent: member,
		},
		Content:  content,
		Message:  content.Body,
		FileName: content.Body,
	})
	return formatted.String(), err
}
