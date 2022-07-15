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
	"maunium.net/go/mautrix/bridge/bridgeconfig"
	up "maunium.net/go/mautrix/util/configupgrade"
)

func DoUpgrade(helper *up.Helper) {
	if legacyDB, ok := helper.Get(up.Str, "appservice", "database"); ok {
		helper.Set(up.Str, legacyDB, "appservice", "database", "uri")
	}
	bridgeconfig.Upgrader.DoUpgrade(helper)

	helper.Copy(up.Int, "revision")

	helper.Copy(up.Str, "logging", "directory")
	helper.Copy(up.Str|up.Null, "logging", "file_name_format")
	helper.Copy(up.Str|up.Timestamp, "logging", "file_date_format")
	helper.Copy(up.Int, "logging", "file_mode")
	helper.Copy(up.Str|up.Timestamp, "logging", "timestamp_format")
	helper.Copy(up.Str, "logging", "print_level")

	helper.Copy(up.Str, "imessage", "platform")
	helper.Copy(up.Str, "imessage", "imessage_rest_path")
	helper.Copy(up.List, "imessage", "imessage_rest_args")
	helper.Copy(up.Bool, "imessage", "log_ipc_payloads")
	helper.Copy(up.Int, "imessage", "ping_interval_seconds")
	helper.Copy(up.Bool, "imessage", "chat_merging")
	helper.Copy(up.Bool, "imessage", "tombstone_old_rooms")

	helper.Copy(up.Str, "bridge", "user")
	helper.Copy(up.Str, "bridge", "username_template")
	helper.Copy(up.Str, "bridge", "displayname_template")
	helper.Copy(up.Bool, "bridge", "personal_filtering_spaces")

	helper.Copy(up.Bool, "bridge", "delivery_receipts")
	if legacyStatusEvents, ok := helper.Get(up.Bool, "bridge", "send_message_send_status_events"); ok && legacyStatusEvents != "" {
		helper.Set(up.Bool, legacyStatusEvents, "bridge", "message_status_events")
	} else {
		helper.Copy(up.Bool, "bridge", "message_status_events")
	}
	helper.Copy(up.Bool, "bridge", "send_error_notices")
	helper.Copy(up.Int, "bridge", "max_handle_seconds")
	helper.Copy(up.Bool, "bridge", "sync_with_custom_puppets")
	helper.Copy(up.Bool, "bridge", "sync_direct_chat_list")
	helper.Copy(up.Str|up.Null, "bridge", "login_shared_secret")
	helper.Copy(up.Str|up.Null, "bridge", "double_puppet_server_url")
	helper.Copy(up.Float|up.Int, "bridge", "chat_sync_max_age")
	helper.Copy(up.Int, "bridge", "initial_backfill_limit")
	helper.Copy(up.Bool, "bridge", "initial_backfill_disable_notifications")
	helper.Copy(up.Bool, "bridge", "periodic_sync")
	helper.Copy(up.Bool, "bridge", "find_portals_if_db_empty")
	helper.Copy(up.Bool, "bridge", "force_uniform_dm_senders")
	if legacyMediaViewerURL, ok := helper.Get(up.Str, "bridge", "media_viewer_url"); ok && legacyMediaViewerURL != "" {
		helper.Set(up.Str, legacyMediaViewerURL, "bridge", "media_viewer", "url")

		if legacyMinSize, ok := helper.Get(up.Int, "bridge", "media_viewer_min_size"); ok {
			helper.Set(up.Int, legacyMinSize, "bridge", "media_viewer", "sms_min_size")
		} else if legacyMinSize, ok = helper.Get(up.Int, "bridge", "media_viewer_sms_min_size"); ok {
			helper.Set(up.Int, legacyMinSize, "bridge", "media_viewer", "sms_min_size")
		}
		if imessageMinSize, ok := helper.Get(up.Int, "bridge", "media_viewer_imessage_min_size"); ok {
			helper.Set(up.Int, imessageMinSize, "bridge", "media_viewer", "imessage_min_size")
		}
		if template, ok := helper.Get(up.Str, "bridge", "media_viewer_template"); ok {
			helper.Set(up.Str, template, "bridge", "media_viewer", "template")
		}
	} else {
		helper.Copy(up.Str|up.Null, "bridge", "media_viewer", "url")
		helper.Copy(up.Int, "bridge", "media_viewer", "sms_min_size")
		helper.Copy(up.Int, "bridge", "media_viewer", "imessage_min_size")
		helper.Copy(up.Str, "bridge", "media_viewer", "template")
	}
	helper.Copy(up.Bool, "bridge", "federate_rooms")
	helper.Copy(up.Bool, "bridge", "convert_heif")
	helper.Copy(up.Bool, "bridge", "convert_tiff")
	helper.Copy(up.Bool, "bridge", "convert_video", "enabled")
	helper.Copy(up.List, "bridge", "convert_video", "ffmpeg_args")
	helper.Copy(up.Str, "bridge", "convert_video", "extension")
	helper.Copy(up.Str, "bridge", "convert_video", "mime_type")
	helper.Copy(up.Str, "bridge", "command_prefix")

	helper.Copy(up.Bool, "bridge", "encryption", "allow")
	helper.Copy(up.Bool, "bridge", "encryption", "default")
	helper.Copy(up.Bool, "bridge", "encryption", "appservice")
	helper.Copy(up.Bool, "bridge", "encryption", "require")
	helper.Copy(up.Str, "bridge", "encryption", "verification_levels", "receive")
	helper.Copy(up.Str, "bridge", "encryption", "verification_levels", "send")
	helper.Copy(up.Str, "bridge", "encryption", "verification_levels", "share")

	legacyKeyShareAllow, ok := helper.Get(up.Bool, "bridge", "encryption", "key_sharing", "allow")
	if ok {
		helper.Set(up.Bool, legacyKeyShareAllow, "bridge", "encryption", "allow_key_sharing")
		legacyKeyShareRequireCS, legacyOK1 := helper.Get(up.Bool, "bridge", "encryption", "key_sharing", "require_cross_signing")
		legacyKeyShareRequireVerification, legacyOK2 := helper.Get(up.Bool, "bridge", "encryption", "key_sharing", "require_verification")
		if legacyOK1 && legacyOK2 && legacyKeyShareRequireVerification == "false" && legacyKeyShareRequireCS == "false" {
			helper.Set(up.Str, "unverified", "bridge", "encryption", "verification_levels", "share")
		}
	} else {
		helper.Copy(up.Bool, "bridge", "encryption", "allow_key_sharing")
	}

	helper.Copy(up.Bool, "bridge", "encryption", "rotation", "enable_custom")
	helper.Copy(up.Int, "bridge", "encryption", "rotation", "milliseconds")
	helper.Copy(up.Int, "bridge", "encryption", "rotation", "messages")
	helper.Copy(up.Bool, "bridge", "relay", "enabled")
	helper.Copy(up.List, "bridge", "relay", "whitelist")
	helper.Copy(up.Map, "bridge", "relay", "message_formats")
}

var SpacedBlocks = [][]string{
	{"homeserver", "asmux"},
	{"appservice"},
	{"appservice", "id"},
	{"appservice", "as_token"},
	{"imessage"},
	{"bridge"},
	{"bridge", "username_template"},
	{"bridge", "delivery_receipts"},
	{"bridge", "encryption"},
	{"bridge", "relay"},
	{"logging"},
	{"revision"},
}
