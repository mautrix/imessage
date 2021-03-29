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
	"context"
	"encoding/json"
	"errors"
	"time"

	"go.mau.fi/mautrix-imessage/imessage"
	"go.mau.fi/mautrix-imessage/ipc"
	log "maunium.net/go/maulogger/v2"
)

const (
	IncomingMessage     ipc.Command = "message"
	IncomingReadReceipt ipc.Command = "read_receipt"
)

type iOSConnector struct {
	IPC         *ipc.Processor
	log         log.Logger
	messageChan chan *imessage.Message
}

func NewiOSConnector(bridge imessage.Bridge) (imessage.API, error) {
	ios := &iOSConnector{
		IPC: bridge.GetIPC(),
		log: bridge.GetLog().Sub("iMessage").Sub("iOS"),

		messageChan: make(chan *imessage.Message, 256),
	}
	ios.IPC.SetHandler(IncomingMessage, ios.handleIncomingMessage)
	return ios, nil
}

func init() {
	imessage.Implementations["ios"] = NewiOSConnector
}

func (ios *iOSConnector) Start() error {
	return nil
}

func (ios *iOSConnector) Stop() {

}

func (ios *iOSConnector) handleIncomingMessage(data json.RawMessage) interface{} {
	var message imessage.Message
	err := json.Unmarshal(data, &message)
	if err != nil {
		ios.log.Warnln("Failed to parse incoming message: %v", err)
		return nil
	}
	message.Time = time.Unix(0, message.UnixTime)
	select {
	case ios.messageChan <- &message:
	default:
		ios.log.Warnln("Incoming message buffer is full")
	}
	return nil
}

func (ios *iOSConnector) GetMessagesSinceDate(chatID string, minDate time.Time) (resp []*imessage.Message, err error) {
	return resp, ios.IPC.Request(context.Background(), ReqGetRecentMessages, &GetMessagesAfterRequest{
		ChatGUID:  chatID,
		Timestamp: minDate.UnixNano(),
	}, &resp)
}

func (ios *iOSConnector) GetMessagesWithLimit(chatID string, limit int) (resp []*imessage.Message, err error) {
	return resp, ios.IPC.Request(context.Background(), ReqGetRecentMessages, &GetRecentMessagesRequest{
		ChatGUID: chatID,
		Limit:    limit,
	}, &resp)
}

func (ios *iOSConnector) GetChatsWithMessagesAfter(minDate time.Time) (resp []string, err error) {
	return resp, ios.IPC.Request(context.Background(), ReqGetChats, &GetChatsRequest{
		MinTimestamp: minDate.UnixNano(),
	}, &resp)
}

func (ios *iOSConnector) MessageChan() <-chan *imessage.Message {
	return ios.messageChan
}

func (ios *iOSConnector) GetContactInfo(identifier string) (*imessage.Contact, error) {
	var resp imessage.Contact
	err := ios.IPC.Request(context.Background(), ReqGetContact, &GetContactRequest{UserGUID: identifier}, &resp)
	return &resp, err
}

func (ios *iOSConnector) GetChatInfo(chatID string) (*imessage.ChatInfo, error) {
	var resp imessage.ChatInfo
	err := ios.IPC.Request(context.Background(), ReqGetChat, &GetChatRequest{ChatGUID: chatID}, &resp)
	return &resp, err
}

func (ios *iOSConnector) GetGroupAvatar(chatID string) (imessage.Attachment, error) {
	return nil, nil
}

func (ios *iOSConnector) SendMessage(chatID, text string) (*imessage.SendResponse, error) {
	var resp imessage.SendResponse
	err := ios.IPC.Request(context.Background(), ReqSendMessage, &SendMessageRequest{
		ChatGUID: chatID,
		Text:     text,
	}, &resp)
	if err != nil {
		return nil, err
	}
	return &resp, err
}

func (ios *iOSConnector) SendFile(chatID, filename string, data []byte) (*imessage.SendResponse, error) {
	return nil, errors.New("sending files is not implemented yet")
}
