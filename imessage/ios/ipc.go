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
	}
	ios.IPC.SetHandler(IncomingMessage, ios.handleIncomingMessage)
	return ios, nil
}

func init() {
	imessage.Implementations["ios"] = NewiOSConnector
}

func (ios iOSConnector) Start() error {
	return nil
}

func (ios iOSConnector) Stop() {

}

func (ios iOSConnector) handleIncomingMessage(data json.RawMessage) interface{} {
	var message imessage.Message
	err := json.Unmarshal(data, &message)
	if err != nil {
		ios.log.Warnln("Failed to parse incoming message: %v", err)
		return nil
	}
	ios.messageChan <- &message
	return nil
}

func (ios iOSConnector) GetMessagesSinceDate(chatID string, minDate time.Time) ([]*imessage.Message, error) {
	return nil, nil
}

func (ios iOSConnector) GetMessagesWithLimit(chatID string, limit int) ([]*imessage.Message, error) {
	return nil, nil
}

func (ios iOSConnector) GetChatsWithMessagesAfter(minDate time.Time) ([]string, error) {
	return []string{}, nil
}

func (ios iOSConnector) MessageChan() <-chan *imessage.Message {
	return ios.messageChan
}

func (ios iOSConnector) GetContactInfo(identifier string) (*imessage.Contact, error) {
	var resp imessage.Contact
	err := ios.IPC.RequestWait(context.Background(), ReqGetContact, &GetContactRequest{UserGUID: identifier}, &resp)
	return &resp, err
}

func (ios iOSConnector) GetChatInfo(chatID string) (*imessage.ChatInfo, error) {
	var resp imessage.ChatInfo
	err := ios.IPC.RequestWait(context.Background(), ReqGetChat, &GetChatRequest{ChatGUID: chatID}, &resp)
	return &resp, err
}

func (ios iOSConnector) GetGroupAvatar(charID string) (imessage.Attachment, error) {
	return nil, nil
}

func (ios iOSConnector) SendMessage(chatID, text string) error {
	// TODO get GUID from response
	return ios.IPC.RequestWait(context.Background(), ReqSendMessage, &SendMessageRequest{
		ChatGUID: chatID,
		Text:     text,
	}, nil)
}

func (ios iOSConnector) SendFile(chatID, filename string, data []byte) error {
	return nil
}
