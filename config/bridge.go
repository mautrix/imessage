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

package config

import (
	"bytes"
	"fmt"

	"strconv"
	"strings"
	"text/template"

	"maunium.net/go/mautrix/id"
)

type BridgeConfig struct {
	User id.UserID `yaml:"user"`

	UsernameTemplate    string `yaml:"username_template"`
	DisplaynameTemplate string `yaml:"displayname_template"`

	DeliveryReceipts            bool `yaml:"delivery_receipts"`
	SendMessageSendStatusEvents bool `yaml:"send_message_send_status_events"`
	SendErrorNotices            bool `yaml:"send_error_notices"`

	MaxHandleSeconds int `yaml:"max_handle_seconds"`

	SyncWithCustomPuppets bool    `yaml:"sync_with_custom_puppets"`
	SyncDirectChatList    bool    `yaml:"sync_direct_chat_list"`
	LoginSharedSecret     string  `yaml:"login_shared_secret"`
	ChatSyncMaxAge        float64 `yaml:"chat_sync_max_age"`
	InitialBackfillLimit  int     `yaml:"initial_backfill_limit"`
	BackfillDisableNotifs bool    `yaml:"initial_backfill_disable_notifications"`
	PeriodicSync          bool    `yaml:"periodic_sync"`
	FindPortalsIfEmpty    bool    `yaml:"find_portals_if_db_empty"`
	MediaViewerURL        string  `yaml:"media_viewer_url"`
	MediaViewerMinSize    int     `yaml:"media_viewer_min_size"`
	AllowUserInvite       bool    `yaml:"allow_user_invite"`

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

	ManagementRoomText struct {
		Welcome        string `yaml:"welcome"`
		AdditionalHelp string `yaml:"additional_help"`
	} `yaml:"management_room_text"`

	Permissions PermissionConfig `yaml:"permissions"`

	usernameTemplate    *template.Template `yaml:"-"`
	displaynameTemplate *template.Template `yaml:"-"`
	communityTemplate   *template.Template `yaml:"-"`
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
	} else if username != "(.+)" && username != ".+" {
		username = id.EncodeUserLocalpart(username)
	}
	var buf bytes.Buffer
	bc.usernameTemplate.Execute(&buf, username)
	return buf.String()
}

type PermissionConfig map[string]PermissionLevel

type PermissionLevel int

const (
	PermissionLevelDefault PermissionLevel = 0
	PermissionLevelRelay   PermissionLevel = 5
	PermissionLevelAdmin   PermissionLevel = 100
)

func (pc *PermissionConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	rawPC := make(map[string]string)
	err := unmarshal(&rawPC)
	if err != nil {
		return err
	}

	if *pc == nil {
		*pc = make(map[string]PermissionLevel)
	}
	for key, value := range rawPC {
		switch strings.ToLower(value) {
		case "relaybot", "relay":
			(*pc)[key] = PermissionLevelRelay
		case "admin":
			(*pc)[key] = PermissionLevelAdmin
		default:
			val, err := strconv.Atoi(value)
			if err != nil {
				(*pc)[key] = PermissionLevelDefault
			} else {
				(*pc)[key] = PermissionLevel(val)
			}
		}
	}
	return nil
}

func (pc *PermissionConfig) MarshalYAML() (interface{}, error) {
	fmt.Println("marshal")
	if *pc == nil {
		return nil, nil
	}
	rawPC := make(map[string]string)
	for key, value := range *pc {
		switch value {
		case PermissionLevelRelay:
			rawPC[key] = "relay"
		case PermissionLevelAdmin:
			rawPC[key] = "admin"
		default:
			rawPC[key] = strconv.Itoa(int(value))
		}
	}
	return rawPC, nil
}

func (pc PermissionConfig) IsRelayWhitelisted(userID id.UserID) bool {
	return pc.GetPermissionLevel(userID) >= PermissionLevelRelay
}

func (pc PermissionConfig) IsAdmin(userID id.UserID) bool {
	return pc.GetPermissionLevel(userID) >= PermissionLevelAdmin
}

func (pc PermissionConfig) GetPermissionLevel(userID id.UserID) PermissionLevel {
	permissions, ok := pc[string(userID)]
	if ok {
		return permissions
	}

	_, homeserver, _ := userID.Parse()
	permissions, ok = pc[homeserver]
	if len(homeserver) > 0 && ok {
		return permissions
	}

	permissions, ok = pc["*"]
	if ok {
		return permissions
	}

	return PermissionLevelDefault
}
