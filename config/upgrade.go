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
	up "go.mau.fi/util/configupgrade"
	"maunium.net/go/mautrix/bridge/bridgeconfig"
)

func DoUpgrade(helper *up.Helper) {
	if legacyDB, ok := helper.Get(up.Str, "appservice", "database"); ok {
		helper.Set(up.Str, legacyDB, "appservice", "database", "uri")
	}
	bridgeconfig.Upgrader.DoUpgrade(helper)

	helper.Copy(up.Int, "revision")

	helper.Copy(up.Str, "imessage", "platform")
	helper.Copy(up.Str, "imessage", "imessage_rest_path")
	helper.Copy(up.List, "imessage", "imessage_rest_args")
	helper.Copy(up.Str, "imessage", "contacts_mode")
	helper.Copy(up.Bool, "imessage", "log_ipc_payloads")
	helper.Copy(up.Str|up.Null, "imessage", "hacky_set_locale")
	helper.Copy(up.List, "imessage", "environment")
	helper.Copy(up.Str, "imessage", "unix_socket")
	helper.Copy(up.Int, "imessage", "ping_interval_seconds")
	helper.Copy(up.Bool, "imessage", "delete_media_after_upload")
	helper.Copy(up.Str|up.Null, "imessage", "bluebubbles_url")
	helper.Copy(up.Str|up.Null, "imessage", "bluebubbles_password")

	helper.Copy(up.Str|up.Null, "segment", "key")
	helper.Copy(up.Str|up.Null, "segment", "user_id")
	helper.Copy(up.Int|up.Str|up.Null, "hacky_startup_test", "identifier")
	helper.Copy(up.Str|up.Null, "hacky_startup_test", "message")
	helper.Copy(up.Str|up.Null, "hacky_startup_test", "response_message")
	helper.Copy(up.Str|up.Null, "hacky_startup_test", "key")
	helper.Copy(up.Bool, "hacky_startup_test", "echo_mode")
	helper.Copy(up.Bool, "hacky_startup_test", "send_on_startup")
	helper.Copy(up.Int, "hacky_startup_test", "periodic_resolve")

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
	helper.Copy(up.Str|up.Null, "bridge", "device_id")
	helper.Copy(up.Bool, "bridge", "sync_direct_chat_list")
	helper.Copy(up.Str|up.Null, "bridge", "login_shared_secret")
	helper.Copy(up.Str|up.Null, "bridge", "double_puppet_server_url")
	if legacyBackfillLimit, ok := helper.Get(up.Int, "bridge", "initial_backfill_limit"); ok {
		helper.Set(up.Int, legacyBackfillLimit, "bridge", "backfill", "initial_limit")
	} else {
		helper.Copy(up.Int, "bridge", "backfill", "initial_limit")
	}
	if legacySyncMaxAge, ok := helper.Get(up.Float|up.Int, "bridge", "chat_sync_max_age"); ok {
		helper.Set(up.Float, legacySyncMaxAge, "bridge", "backfill", "initial_sync_max_age")
	} else {
		helper.Copy(up.Float|up.Int, "bridge", "backfill", "initial_sync_max_age")
	}
	helper.Copy(up.Bool, "bridge", "backfill", "enable")
	helper.Copy(up.Int, "bridge", "backfill", "unread_hours_threshold")
	helper.Copy(up.Bool, "bridge", "backfill", "only_backfill")
	helper.Copy(up.Int, "bridge", "backfill", "immediate", "max_events")
	helper.Copy(up.List, "bridge", "backfill", "deferred")
	helper.Copy(up.Bool, "bridge", "periodic_sync")
	helper.Copy(up.Bool, "bridge", "find_portals_if_db_empty")
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
		helper.Copy(up.Str|up.Null, "bridge", "media_viewer", "homeserver")
		helper.Copy(up.Int, "bridge", "media_viewer", "sms_min_size")
		helper.Copy(up.Int, "bridge", "media_viewer", "imessage_min_size")
		helper.Copy(up.Str, "bridge", "media_viewer", "template")
	}
	helper.Copy(up.Map, "bridge", "double_puppet_server_map")
	helper.Copy(up.Bool, "bridge", "double_puppet_allow_discovery")
	if legacySecret, ok := helper.Get(up.Str, "bridge", "login_shared_secret"); ok && len(legacySecret) > 0 {
		baseNode := helper.GetBaseNode("bridge", "login_shared_secret_map")
		baseNode.Map[helper.GetBase("homeserver", "domain")] = up.StringNode(legacySecret)
		baseNode.UpdateContent()
	} else {
		helper.Copy(up.Map, "bridge", "login_shared_secret_map")
	}
	helper.Copy(up.Bool, "bridge", "convert_heif")
	helper.Copy(up.Bool, "bridge", "convert_tiff")
	helper.Copy(up.Bool, "bridge", "convert_video", "enabled")
	helper.Copy(up.List, "bridge", "convert_video", "ffmpeg_args")
	helper.Copy(up.Str, "bridge", "convert_video", "extension")
	helper.Copy(up.Str, "bridge", "convert_video", "mime_type")
	helper.Copy(up.Str, "bridge", "command_prefix")
	helper.Copy(up.Bool, "bridge", "force_uniform_dm_senders")
	helper.Copy(up.Bool, "bridge", "disable_sms_portals")
	helper.Copy(up.Bool, "bridge", "federate_rooms")
	helper.Copy(up.Bool, "bridge", "caption_in_message")
	helper.Copy(up.Str, "bridge", "private_chat_portal_meta")

	helper.Copy(up.Bool, "bridge", "encryption", "allow")
	helper.Copy(up.Bool, "bridge", "encryption", "default")
	helper.Copy(up.Bool, "bridge", "encryption", "appservice")
	helper.Copy(up.Bool, "bridge", "encryption", "require")
	helper.Copy(up.Bool, "bridge", "encryption", "delete_keys", "delete_outbound_on_ack")
	helper.Copy(up.Bool, "bridge", "encryption", "delete_keys", "dont_store_outbound")
	helper.Copy(up.Bool, "bridge", "encryption", "delete_keys", "ratchet_on_decrypt")
	helper.Copy(up.Bool, "bridge", "encryption", "delete_keys", "delete_fully_used_on_decrypt")
	helper.Copy(up.Bool, "bridge", "encryption", "delete_keys", "delete_prev_on_new_session")
	helper.Copy(up.Bool, "bridge", "encryption", "delete_keys", "delete_on_device_delete")
	helper.Copy(up.Bool, "bridge", "encryption", "delete_keys", "periodically_delete_expired")
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
	helper.Copy(up.Bool, "bridge", "encryption", "rotation", "disable_device_change_key_rotation")
	helper.Copy(up.Bool, "bridge", "relay", "enabled")
	helper.Copy(up.List, "bridge", "relay", "whitelist")
	helper.Copy(up.Map, "bridge", "relay", "message_formats")
}

var SpacedBlocks = [][]string{
	{"homeserver", "software"},
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
