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

package ios

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"strings"
	"time"

	log "maunium.net/go/maulogger/v2"

	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-imessage/imessage"
	"go.mau.fi/mautrix-imessage/ipc"
)

const (
	IncomingMessage            ipc.Command = "message"
	IncomingReadReceipt        ipc.Command = "read_receipt"
	IncomingTypingNotification ipc.Command = "typing"
	IncomingChat               ipc.Command = "chat"
	IncomingChatID             ipc.Command = "chat_id"
	IncomingPingServer         ipc.Command = "ping_server"
	IncomingBridgeStatus       ipc.Command = "bridge_status"
	IncomingContact            ipc.Command = "contact"
	IncomingMessageIDQuery     ipc.Command = "message_ids_after_time"
	IncomingPushKey            ipc.Command = "push_key"
	IncomingSendMessageStatus  ipc.Command = "send_message_status"
	IncomingBackfillTask       ipc.Command = "backfill"
)

func floatToTime(unix float64) (time.Time, bool) {
	sec, dec := math.Modf(unix)
	intSec := int64(sec)
	if intSec < 1e10 {
		return time.Unix(intSec, int64(dec*(1e9))), false
	} else if intSec < 1e13 {
		return time.UnixMilli(intSec), true
	} else if intSec < 1e16 {
		return time.UnixMicro(intSec), true
	} else {
		return time.Unix(0, intSec), true
	}
}

func timeToFloat(time time.Time) float64 {
	if time.IsZero() {
		return 0
	}
	return float64(time.Unix()) + float64(time.Nanosecond())/1e9
}

type APIWithIPC interface {
	imessage.API
	SetIPC(*ipc.Processor)
	GetIPC() *ipc.Processor
	SetContactProxy(api imessage.ContactAPI)
	SetChatInfoProxy(api imessage.ChatInfoAPI)
}

type iOSConnector struct {
	IPC               *ipc.Processor
	bridge            imessage.Bridge
	log               log.Logger
	messageChan       chan *imessage.Message
	receiptChan       chan *imessage.ReadReceipt
	typingChan        chan *imessage.TypingNotification
	chatChan          chan *imessage.ChatInfo
	contactChan       chan *imessage.Contact
	messageStatusChan chan *imessage.SendMessageStatus
	backfillTaskChan  chan *imessage.BackfillTask
	isAndroid         bool
	contactProxy      imessage.ContactAPI
	chatInfoProxy     imessage.ChatInfoAPI
}

func NewPlainiOSConnector(logger log.Logger, bridge imessage.Bridge) APIWithIPC {
	return &iOSConnector{
		log:               logger,
		bridge:            bridge,
		messageChan:       make(chan *imessage.Message, 256),
		receiptChan:       make(chan *imessage.ReadReceipt, 32),
		typingChan:        make(chan *imessage.TypingNotification, 32),
		chatChan:          make(chan *imessage.ChatInfo, 32),
		contactChan:       make(chan *imessage.Contact, 2048),
		messageStatusChan: make(chan *imessage.SendMessageStatus, 32),
		backfillTaskChan:  make(chan *imessage.BackfillTask, 32),
		isAndroid:         bridge.GetConnectorConfig().Platform == "android",
	}
}

func NewiOSConnector(bridge imessage.Bridge) (imessage.API, error) {
	ios := NewPlainiOSConnector(bridge.GetLog().Sub("iMessage").Sub("iOS"), bridge)
	ios.SetIPC(bridge.GetIPC())
	return ios, nil
}

func init() {
	imessage.Implementations["ios"] = NewiOSConnector
	imessage.Implementations["android"] = NewiOSConnector
}

func (ios *iOSConnector) SetIPC(proc *ipc.Processor) {
	ios.IPC = proc
}

func (ios *iOSConnector) GetIPC() *ipc.Processor {
	return ios.IPC
}

func (ios *iOSConnector) SetContactProxy(api imessage.ContactAPI) {
	ios.contactProxy = api
}

func (ios *iOSConnector) SetChatInfoProxy(api imessage.ChatInfoAPI) {
	ios.chatInfoProxy = api
}

func (ios *iOSConnector) Start(readyCallback func()) error {
	ios.IPC.SetHandler(IncomingMessage, ios.handleIncomingMessage)
	ios.IPC.SetHandler(IncomingReadReceipt, ios.handleIncomingReadReceipt)
	ios.IPC.SetHandler(IncomingTypingNotification, ios.handleIncomingTypingNotification)
	ios.IPC.SetHandler(IncomingChat, ios.handleIncomingChat)
	ios.IPC.SetHandler(IncomingChatID, ios.handleChatIDChange)
	ios.IPC.SetHandler(IncomingPingServer, ios.handleIncomingServerPing)
	ios.IPC.SetHandler(IncomingBridgeStatus, ios.handleIncomingStatus)
	ios.IPC.SetHandler(IncomingContact, ios.handleIncomingContact)
	ios.IPC.SetHandler(IncomingMessageIDQuery, ios.handleMessageIDQuery)
	ios.IPC.SetHandler(IncomingPushKey, ios.handlePushKey)
	ios.IPC.SetHandler(IncomingSendMessageStatus, ios.handleIncomingSendMessageStatus)
	ios.IPC.SetHandler(IncomingBackfillTask, ios.handleIncomingBackfillTask)
	readyCallback()
	return nil
}

func (ios *iOSConnector) Stop() {}

func (ios *iOSConnector) postprocessMessage(message *imessage.Message, source string) {
	if len(message.Service) == 0 {
		message.Service = imessage.ParseIdentifier(message.ChatGUID).Service
	}
	if !message.IsFromMe {
		message.Sender = imessage.ParseIdentifier(message.JSONSenderGUID)
	}
	if len(message.JSONTargetGUID) > 0 {
		message.Target = imessage.ParseIdentifier(message.JSONTargetGUID)
	}
	var warn bool
	message.Time, warn = floatToTime(message.JSONUnixTime)
	if warn {
		ios.log.Warnfln("Incorrect precision timestamp in %s (from %s): %v", message.GUID, source, message.JSONUnixTime)
	}
	message.ReadAt, warn = floatToTime(message.JSONUnixReadAt)
	if warn {
		ios.log.Warnfln("Incorrect precision read at timestamp in %s (from %s): %v", message.GUID, source, message.JSONUnixReadAt)
	}
	if message.Tapback != nil {
		_, err := message.Tapback.Parse()
		if err != nil {
			ios.log.Warnfln("Failed to parse tapback in %s: %v", message.GUID, err)
		}
	}
	if len(message.NewGroupName) > 0 && message.ItemType != imessage.ItemTypeName {
		ios.log.Warnfln("Autocorrecting item_type of message %s where new_group_name is set to %d (name change)", message.GUID, imessage.ItemTypeName)
		message.ItemType = imessage.ItemTypeName
	} else if message.ItemType == imessage.ItemTypeMessage && message.GroupActionType > 0 {
		ios.log.Warnfln("Autocorrecting item_type of message %s where group_action_type is set to %d (avatar change)", message.GUID, imessage.ItemTypeAvatar)
		message.ItemType = imessage.ItemTypeAvatar
	}
	if message.Attachment != nil && message.Attachments == nil {
		ios.log.Warnfln("Autocorrecting single attachment -> attachments array in message %s", message.GUID)
		message.Attachments = []*imessage.Attachment{message.Attachment}
	} else if message.Attachments != nil && len(message.Attachments) > 0 && message.Attachment == nil {
		message.Attachment = message.Attachments[0]
	}
}

func (ios *iOSConnector) handleIncomingMessage(data json.RawMessage) interface{} {
	var message imessage.Message
	err := json.Unmarshal(data, &message)
	if err != nil {
		ios.log.Warnln("Failed to parse incoming message: %v", err)
		return nil
	}
	ios.postprocessMessage(&message, "incoming message")
	select {
	case ios.messageChan <- &message:
	default:
		ios.log.Warnln("Incoming message buffer is full")
	}
	return nil
}

func (ios *iOSConnector) handleIncomingReadReceipt(data json.RawMessage) interface{} {
	var receipt imessage.ReadReceipt
	err := json.Unmarshal(data, &receipt)
	if err != nil {
		ios.log.Warnln("Failed to parse incoming read receipt: %v", err)
		return nil
	}
	var warn bool
	receipt.ReadAt, warn = floatToTime(receipt.JSONUnixReadAt)
	if warn {
		ios.log.Warnfln("Incorrect precision timestamp in incoming read receipt for %s: %v", receipt.ReadUpTo, receipt.JSONUnixReadAt)
	}

	select {
	case ios.receiptChan <- &receipt:
	default:
		ios.log.Warnln("Incoming receipt buffer is full")
	}
	return nil
}

func (ios *iOSConnector) handleIncomingTypingNotification(data json.RawMessage) interface{} {
	var notif imessage.TypingNotification
	err := json.Unmarshal(data, &notif)
	if err != nil {
		ios.log.Warnln("Failed to parse incoming typing notification: %v", err)
		return nil
	}
	select {
	case ios.typingChan <- &notif:
	default:
		ios.log.Warnln("Incoming typing notification buffer is full")
	}
	return nil
}

func (ios *iOSConnector) handleIncomingChat(data json.RawMessage) interface{} {
	var chat imessage.ChatInfo
	err := json.Unmarshal(data, &chat)
	if err != nil {
		ios.log.Warnln("Failed to parse incoming chat:", err)
		return nil
	}
	chat.Identifier = imessage.ParseIdentifier(chat.JSONChatGUID)
	select {
	case ios.chatChan <- &chat:
	default:
		ios.log.Warnln("Incoming chat buffer is full")
	}
	return nil
}

type ChatIDChangeRequest struct {
	OldGUID string `json:"old_guid"`
	NewGUID string `json:"new_guid"`
}

type ChatIDChangeResponse struct {
	Changed bool `json:"changed"`
}

func (ios *iOSConnector) handleChatIDChange(data json.RawMessage) interface{} {
	var chatIDChange ChatIDChangeRequest
	err := json.Unmarshal(data, &chatIDChange)
	if err != nil {
		ios.log.Warnln("Failed to parse chat ID change:", err)
		return nil
	}
	return &ChatIDChangeResponse{
		Changed: ios.bridge.ReIDPortal(chatIDChange.OldGUID, chatIDChange.NewGUID, false),
	}
}

type MessageIDQueryRequest struct {
	ChatGUID  string  `json:"chat_guid"`
	AfterTime float64 `json:"after_time"`
}

type MessageIDQueryResponse struct {
	IDs []string `json:"ids"`
}

func (ios *iOSConnector) handleMessageIDQuery(data json.RawMessage) interface{} {
	var query MessageIDQueryRequest
	err := json.Unmarshal(data, &query)
	if err != nil {
		ios.log.Warnln("Failed to parse message ID query:", err)
		return nil
	}
	ts, warn := floatToTime(query.AfterTime)
	if warn {
		ios.log.Warnfln("Incorrect precision timestamp in message ID query for %s: %v", query.ChatGUID, query.AfterTime)
	}
	return &MessageIDQueryResponse{
		IDs: ios.bridge.GetMessagesSince(query.ChatGUID, ts),
	}
}

func (ios *iOSConnector) handlePushKey(data json.RawMessage) interface{} {
	var query imessage.PushKeyRequest
	err := json.Unmarshal(data, &query)
	if err != nil {
		ios.log.Warnln("Failed to parse set push key request:", err)
		return nil
	}
	ios.bridge.SetPushKey(&query)
	return nil
}

func (ios *iOSConnector) handleIncomingServerPing(_ json.RawMessage) interface{} {
	start, server, end := ios.bridge.PingServer()
	return &PingServerResponse{
		Start:  timeToFloat(start),
		Server: timeToFloat(server),
		End:    timeToFloat(end),
	}
}

func (ios *iOSConnector) handleIncomingStatus(data json.RawMessage) interface{} {
	var state imessage.BridgeStatus
	err := json.Unmarshal(data, &state)
	if err != nil {
		ios.log.Warnln("Failed to parse incoming status update:", err)
		return nil
	}
	ios.bridge.SendBridgeStatus(state)
	return nil
}

func (ios *iOSConnector) handleIncomingContact(data json.RawMessage) interface{} {
	var contact imessage.Contact
	err := json.Unmarshal(data, &contact)
	if err != nil {
		ios.log.Warnln("Failed to parse incoming contact:", err)
		return nil
	}
	select {
	case ios.contactChan <- &contact:
	default:
		ios.log.Warnln("Incoming contact buffer is full")
	}
	return nil
}

func (ios *iOSConnector) handleIncomingSendMessageStatus(data json.RawMessage) interface{} {
	var status imessage.SendMessageStatus
	err := json.Unmarshal(data, &status)
	if len(status.Service) == 0 {
		status.Service = imessage.ParseIdentifier(status.ChatGUID).Service
	}
	if err != nil {
		ios.log.Warnln("Failed to parse incoming send message status:", err)
		return nil
	}
	select {
	case ios.messageStatusChan <- &status:
	default:
		ios.log.Warnln("Incoming send message status buffer is full")
	}
	return nil
}

func (ios *iOSConnector) handleIncomingBackfillTask(data json.RawMessage) interface{} {
	var task imessage.BackfillTask
	err := json.Unmarshal(data, &task)
	if err != nil {
		ios.log.Warnln("Failed to parse incoming backfill task:", err)
		return nil
	}
	select {
	case ios.backfillTaskChan <- &task:
	default:
		ios.log.Warnln("Incoming backfill task buffer is full")
	}
	return nil
}

func (ios *iOSConnector) GetMessagesSinceDate(chatID string, minDate time.Time, backfillID string) ([]*imessage.Message, error) {
	resp := make([]*imessage.Message, 0)
	err := ios.IPC.Request(context.Background(), ReqGetMessagesAfter, &GetMessagesAfterRequest{
		ChatGUID:   chatID,
		Timestamp:  timeToFloat(minDate),
		BackfillID: backfillID,
	}, &resp)
	for _, msg := range resp {
		ios.postprocessMessage(msg, "messages since date")
	}
	return resp, err
}

func (ios *iOSConnector) GetMessagesBetween(chatID string, minDate, maxDate time.Time) ([]*imessage.Message, error) {
	panic("not implemented")
}

func (ios *iOSConnector) GetMessagesBeforeWithLimit(chatID string, before time.Time, limit int) ([]*imessage.Message, error) {
	panic("not implemented")
}

func (ios *iOSConnector) GetMessagesWithLimit(chatID string, limit int, backfillID string) ([]*imessage.Message, error) {
	resp := make([]*imessage.Message, 0)
	err := ios.IPC.Request(context.Background(), ReqGetRecentMessages, &GetRecentMessagesRequest{
		ChatGUID:   chatID,
		Limit:      limit,
		BackfillID: backfillID,
	}, &resp)
	for _, msg := range resp {
		ios.postprocessMessage(msg, "messages with limit")
	}
	return resp, err
}

func (ios *iOSConnector) GetMessage(guid string) (resp *imessage.Message, err error) {
	return resp, ios.IPC.Request(context.Background(), ReqGetMessage, &GetMessageRequest{
		GUID: guid,
	}, &resp)
}

func (ios *iOSConnector) GetChatsWithMessagesAfter(minDate time.Time) (resp []imessage.ChatIdentifier, err error) {
	return resp, ios.IPC.Request(context.Background(), ReqGetChats, &GetChatsRequest{
		MinTimestamp: timeToFloat(minDate),
	}, &resp)
}

func (ios *iOSConnector) MessageChan() <-chan *imessage.Message {
	return ios.messageChan
}

func (ios *iOSConnector) ReadReceiptChan() <-chan *imessage.ReadReceipt {
	return ios.receiptChan
}

func (ios *iOSConnector) TypingNotificationChan() <-chan *imessage.TypingNotification {
	return ios.typingChan
}

func (ios *iOSConnector) ChatChan() <-chan *imessage.ChatInfo {
	return ios.chatChan
}

func (ios *iOSConnector) ContactChan() <-chan *imessage.Contact {
	return ios.contactChan
}

func (ios *iOSConnector) MessageStatusChan() <-chan *imessage.SendMessageStatus {
	return ios.messageStatusChan
}

func (ios *iOSConnector) BackfillTaskChan() <-chan *imessage.BackfillTask {
	return ios.backfillTaskChan
}

func (ios *iOSConnector) GetContactInfo(identifier string) (*imessage.Contact, error) {
	if ios.contactProxy != nil {
		return ios.contactProxy.GetContactInfo(identifier)
	}
	var resp imessage.Contact
	err := ios.IPC.Request(context.Background(), ReqGetContact, &GetContactRequest{UserGUID: identifier}, &resp)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

func (ios *iOSConnector) GetContactList() ([]*imessage.Contact, error) {
	if ios.contactProxy != nil {
		return ios.contactProxy.GetContactList()
	}
	var resp GetContactListResponse
	err := ios.IPC.Request(context.Background(), ReqGetContactList, nil, &resp)
	return resp.Contacts, err
}

func (ios *iOSConnector) SearchContactList(searchTerms string) ([]*imessage.Contact, error) {
	return nil, errors.New("not implemented")
}

func (ios *iOSConnector) RefreshContactList() error {
	return errors.New("not implemented")
}

func (ios *iOSConnector) GetChatInfo(chatID, threadID string) (*imessage.ChatInfo, error) {
	var resp imessage.ChatInfo
	err := ios.IPC.Request(context.Background(), ReqGetChat, &GetChatRequest{ChatGUID: chatID, ThreadID: threadID}, &resp)
	if err != nil {
		if ios.chatInfoProxy != nil {
			ios.log.Warnfln("Failed to get chat info for %s: %v, falling back to chat info proxy", chatID, err)
			return ios.chatInfoProxy.GetChatInfo(chatID, threadID)
		}
		return nil, err
	}
	return &resp, nil
}

func (ios *iOSConnector) GetGroupAvatar(chatID string) (*imessage.Attachment, error) {
	var resp imessage.Attachment
	err := ios.IPC.Request(context.Background(), ReqGetChatAvatar, &GetChatRequest{ChatGUID: chatID}, &resp)
	if err != nil {
		if ios.chatInfoProxy != nil {
			ios.log.Warnfln("Failed to get group avatar for %s: %v, falling back to chat info proxy", chatID, err)
			return ios.chatInfoProxy.GetGroupAvatar(chatID)
		}
		return nil, err
	}
	return &resp, nil
}

func (ios *iOSConnector) SendMessage(chatID, text string, replyTo string, replyToPart int, richLink *imessage.RichLink, metadata imessage.MessageMetadata) (*imessage.SendResponse, error) {
	var resp imessage.SendResponse
	err := ios.IPC.Request(context.Background(), ReqSendMessage, &SendMessageRequest{
		ChatGUID:    chatID,
		Text:        text,
		ReplyTo:     replyTo,
		ReplyToPart: replyToPart,
		RichLink:    richLink,
		Metadata:    metadata,
	}, &resp)
	if err == nil {
		var warn bool
		resp.Time, warn = floatToTime(resp.UnixTime)
		if warn {
			ios.log.Warnfln("Incorrect precision timestamp in message send response %s: %v", resp.GUID, resp.UnixTime)
		}
	}
	if len(resp.Service) == 0 {
		resp.Service = imessage.ParseIdentifier(chatID).Service
	}
	return &resp, err
}

func (ios *iOSConnector) SendFile(chatID, text, filename, pathOnDisk, guid, replyTo string, replyToPart int, mimeType string, voiceMemo bool, metadata imessage.MessageMetadata) (*imessage.SendResponse, error) {
	var resp imessage.SendResponse
	err := ios.IPC.Request(context.Background(), ReqSendMedia, &SendMediaRequest{
		ChatGUID: chatID,
		Text:     text,
		Attachment: imessage.Attachment{
			GUID:       guid,
			FileName:   filename,
			PathOnDisk: pathOnDisk,
			MimeType:   mimeType,
		},
		ReplyTo:        replyTo,
		ReplyToPart:    replyToPart,
		IsAudioMessage: voiceMemo,
		Metadata:       metadata,
	}, &resp)
	if err == nil {
		var warn bool
		resp.Time, warn = floatToTime(resp.UnixTime)
		if warn {
			ios.log.Warnfln("Incorrect precision timestamp in file message send response %s: %v", resp.GUID, resp.UnixTime)
		}
	}
	return &resp, err
}

func (ios *iOSConnector) SendFileCleanup(sendFileDir string) {
	_ = os.RemoveAll(sendFileDir)
}

func (ios *iOSConnector) SendTapback(chatID, targetGUID string, targetPart int, tapback imessage.TapbackType, remove bool) (*imessage.SendResponse, error) {
	if remove {
		tapback += imessage.TapbackRemoveOffset
	}
	var resp imessage.SendResponse
	err := ios.IPC.Request(context.Background(), ReqSendTapback, &SendTapbackRequest{
		ChatGUID:   chatID,
		TargetGUID: targetGUID,
		TargetPart: targetPart,
		Type:       tapback,
	}, &resp)
	if err != nil {
		return nil, err
	}
	return &resp, err
}

func (ios *iOSConnector) SendReadReceipt(chatID, readUpTo string) error {
	return ios.IPC.Send(ReqSendReadReceipt, &SendReadReceiptRequest{
		ChatGUID: chatID,
		ReadUpTo: readUpTo,
	})
}

func (ios *iOSConnector) SendTypingNotification(chatID string, typing bool) error {
	return ios.IPC.Send(ReqSetTyping, &SetTypingRequest{
		ChatGUID: chatID,
		Typing:   typing,
	})
}

func (ios *iOSConnector) SendMessageBridgeResult(chatID, messageID string, eventID id.EventID, success bool) {
	if !ios.isAndroid {
		// Only android needs message bridging confirmations
		return
	}
	_ = ios.IPC.Send(ReqMessageBridgeResult, &MessageBridgeResult{
		ChatGUID: chatID,
		GUID:     messageID,
		EventID:  eventID,
		Success:  success,
	})
}

func (ios *iOSConnector) SendBackfillResult(chatID, backfillID string, success bool, idMap map[string][]id.EventID) {
	if !ios.isAndroid {
		// Only android needs message bridging confirmations
		return
	}
	if idMap == nil {
		idMap = map[string][]id.EventID{}
	}
	_ = ios.IPC.Send(ReqBackfillResult, &BackfillResult{
		ChatGUID:   chatID,
		BackfillID: backfillID,
		Success:    success,
		MessageIDs: idMap,
	})
}

func (ios *iOSConnector) SendChatBridgeResult(guid string, mxid id.RoomID) {
	_ = ios.IPC.Send(ReqChatBridgeResult, &ChatBridgeResult{
		ChatGUID: guid,
		MXID:     mxid,
	})
}

func (ios *iOSConnector) NotifyUpcomingMessage(eventID id.EventID) {
	if !ios.isAndroid {
		// Only android needs to be notified about upcoming messages to stay awake
		return
	}
	_ = ios.IPC.Send(ReqUpcomingMessage, &UpcomingMessage{EventID: eventID})
}

func (ios *iOSConnector) PreStartupSyncHook() (resp imessage.StartupSyncHookResponse, err error) {
	err = ios.IPC.Request(context.Background(), ReqPreStartupSync, nil, &resp)
	return
}

func (ios *iOSConnector) PostStartupSyncHook() {
	_ = ios.IPC.Send(ReqPostStartupSync, nil)
}

func (ios *iOSConnector) ResolveIdentifier(identifier string) (string, error) {
	if ios.isAndroid {
		return imessage.Identifier{
			LocalID: identifier,
			Service: "SMS",
			IsGroup: false,
		}.String(), nil
	}
	req := ResolveIdentifierRequest{Identifier: identifier}
	var resp ResolveIdentifierResponse
	err := ios.IPC.Request(context.Background(), ReqResolveIdentifier, &req, &resp)
	// Hack: barcelona probably shouldn't return mailto: or tel:
	resp.GUID = strings.Replace(resp.GUID, "iMessage;-;mailto:", "iMessage;-;", 1)
	resp.GUID = strings.Replace(resp.GUID, "iMessage;-;tel:", "iMessage;-;", 1)
	return resp.GUID, err
}

func (ios *iOSConnector) PrepareDM(guid string) error {
	if ios.isAndroid {
		return nil
	}
	return ios.IPC.Request(context.Background(), ReqPrepareDM, &PrepareDMRequest{GUID: guid}, nil)
}

func (ios *iOSConnector) CreateGroup(users []string) (*imessage.CreateGroupResponse, error) {
	var resp imessage.CreateGroupResponse
	err := ios.IPC.Request(context.Background(), ReqCreateGroup, &CreateGroupRequest{GUIDs: users}, &resp)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

func (ios *iOSConnector) Capabilities() imessage.ConnectorCapabilities {
	return imessage.ConnectorCapabilities{
		MessageSendResponses:     true,
		MessageStatusCheckpoints: ios.isAndroid,
		SendTapbacks:             !ios.isAndroid,
		SendReadReceipts:         !ios.isAndroid,
		SendTypingNotifications:  !ios.isAndroid,
		SendCaptions:             ios.isAndroid,
		BridgeState:              false,
		ContactChatMerging:       !ios.isAndroid,
		ChatBridgeResult:         ios.isAndroid,
	}
}
