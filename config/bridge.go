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

	"maunium.net/go/mautrix/bridge/bridgeconfig"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type DeferredConfig struct {
	StartDaysAgo   int `yaml:"start_days_ago"`
	MaxBatchEvents int `yaml:"max_batch_events"`
	BatchDelay     int `yaml:"batch_delay"`
}

type BridgeConfig struct {
	User id.UserID `yaml:"user"`

	UsernameTemplate    string `yaml:"username_template"`
	DisplaynameTemplate string `yaml:"displayname_template"`

	DoublePuppetConfig bridgeconfig.DoublePuppetConfig `yaml:",inline"`

	PersonalFilteringSpaces bool `yaml:"personal_filtering_spaces"`

	DeliveryReceipts    bool `yaml:"delivery_receipts"`
	MessageStatusEvents bool `yaml:"message_status_events"`
	SendErrorNotices    bool `yaml:"send_error_notices"`

	MaxHandleSeconds int    `yaml:"max_handle_seconds"`
	DeviceID         string `yaml:"device_id"`

	SyncDirectChatList    bool   `yaml:"sync_direct_chat_list"`
	LoginSharedSecret     string `yaml:"login_shared_secret"`
	DoublePuppetServerURL string `yaml:"double_puppet_server_url"`
	Backfill              struct {
		Enable               bool    `yaml:"enable"`
		OnlyBackfill         bool    `yaml:"only_backfill"`
		InitialLimit         int     `yaml:"initial_limit"`
		InitialSyncMaxAge    float64 `yaml:"initial_sync_max_age"`
		UnreadHoursThreshold int     `yaml:"unread_hours_threshold"`
		Immediate            struct {
			MaxEvents int `yaml:"max_events"`
		} `yaml:"immediate"`

		Deferred []DeferredConfig `yaml:"deferred"`
	} `yaml:"backfill"`
	PeriodicSync       bool `yaml:"periodic_sync"`
	FindPortalsIfEmpty bool `yaml:"find_portals_if_db_empty"`
	MediaViewer        struct {
		URL        string `yaml:"url"`
		Homeserver string `yaml:"homeserver"`
		SMSMinSize int    `yaml:"sms_min_size"`
		IMMinSize  int    `yaml:"imessage_min_size"`
		Template   string `yaml:"template"`
	} `yaml:"media_viewer"`
	ConvertHEIF  bool `yaml:"convert_heif"`
	ConvertTIFF  bool `yaml:"convert_tiff"`
	ConvertVideo struct {
		Enabled    bool     `yaml:"enabled"`
		FFMPEGArgs []string `yaml:"ffmpeg_args"`
		MimeType   string   `yaml:"mime_type"`
		Extension  string   `yaml:"extension"`
	} `yaml:"convert_video"`
	CommandPrefix          string `yaml:"command_prefix"`
	ForceUniformDMSenders  bool   `yaml:"force_uniform_dm_senders"`
	DisableSMSPortals      bool   `yaml:"disable_sms_portals"`
	RerouteSMSGroupReplies bool   `yaml:"reroute_mms_group_replies"`
	FederateRooms          bool   `yaml:"federate_rooms"`
	CaptionInMessage       bool   `yaml:"caption_in_message"`
	PrivateChatPortalMeta  string `yaml:"private_chat_portal_meta"`

	Encryption bridgeconfig.EncryptionConfig `yaml:"encryption"`

	Relay RelayConfig `yaml:"relay"`

	usernameTemplate    *template.Template `yaml:"-"`
	displaynameTemplate *template.Template `yaml:"-"`
	communityTemplate   *template.Template `yaml:"-"`
}

func (bc BridgeConfig) GetDoublePuppetConfig() bridgeconfig.DoublePuppetConfig {
	return bc.DoublePuppetConfig
}

func (bc BridgeConfig) GetResendBridgeInfo() bool {
	return false
}

func (bc BridgeConfig) GetManagementRoomTexts() bridgeconfig.ManagementRoomTexts {
	return bridgeconfig.ManagementRoomTexts{
		Welcome:            "Hello, I'm an iMessage bridge bot.",
		WelcomeConnected:   "Use `help` for help.",
		WelcomeUnconnected: "",
		AdditionalHelp:     "",
	}
}

func (bc BridgeConfig) Validate() error {
	return nil
}

func (bc BridgeConfig) GetEncryptionConfig() bridgeconfig.EncryptionConfig {
	return bc.Encryption
}

func (bc BridgeConfig) EnableMessageStatusEvents() bool {
	return bc.MessageStatusEvents
}

func (bc BridgeConfig) EnableMessageErrorNotices() bool {
	return bc.SendErrorNotices
}

func (bc BridgeConfig) GetCommandPrefix() string {
	return bc.CommandPrefix
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
	} else {
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
