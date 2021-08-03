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

package ios

import (
	"go.mau.fi/mautrix-imessage/imessage"
	"go.mau.fi/mautrix-imessage/ipc"
)

const (
	ReqSendMessage       ipc.Command = "send_message"
	ReqSendMedia         ipc.Command = "send_media"
	ReqSendTapback       ipc.Command = "send_tapback"
	ReqSendReadReceipt   ipc.Command = "send_read_receipt"
	ReqSetTyping         ipc.Command = "set_typing"
	ReqGetChats          ipc.Command = "get_chats"
	ReqGetChat           ipc.Command = "get_chat"
	ReqGetChatAvatar     ipc.Command = "get_chat_avatar"
	ReqGetContact        ipc.Command = "get_contact"
	ReqGetMessagesAfter  ipc.Command = "get_messages_after"
	ReqGetRecentMessages ipc.Command = "get_recent_messages"
)

type SendMessageRequest struct {
	ChatGUID    string `json:"chat_guid"`
	Text        string `json:"text"`
	ReplyTo     string `json:"reply_to"`
	ReplyToPart int    `json:"reply_to_part"`
}

type SendMediaRequest struct {
	ChatGUID string `json:"chat_guid"`
	imessage.Attachment
	ReplyTo     string `json:"reply_to"`
	ReplyToPart int    `json:"reply_to_part"`
}

type SendTapbackRequest struct {
	ChatGUID   string               `json:"chat_guid"`
	TargetGUID string               `json:"target_guid"`
	TargetPart int                  `json:"target_part"`
	Type       imessage.TapbackType `json:"type"`
}

type SendReadReceiptRequest struct {
	ChatGUID string `json:"chat_guid"`
	ReadUpTo string `json:"read_up_to"`
}

type SetTypingRequest struct {
	ChatGUID string `json:"chat_guid"`
	Typing   bool   `json:"typing"`
}

type GetChatRequest struct {
	ChatGUID string `json:"chat_guid"`
}

type GetChatsRequest struct {
	MinTimestamp float64 `json:"min_timestamp"`
}

type GetContactRequest struct {
	UserGUID string `json:"user_guid"`
}

type GetRecentMessagesRequest struct {
	ChatGUID string `json:"chat_guid"`
	Limit    int    `json:"limit"`
}

type GetMessagesAfterRequest struct {
	ChatGUID  string  `json:"chat_guid"`
	Timestamp float64 `json:"timestamp"`
}

type PingServerResponse struct {
	Start  float64 `json:"start_ts"`
	Server float64 `json:"server_ts"`
	End    float64 `json:"end_ts"`
}
