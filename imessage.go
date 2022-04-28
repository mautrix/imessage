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

package main

import (
	"errors"

	log "maunium.net/go/maulogger/v2"
	"maunium.net/go/mautrix/appservice"

	"go.mau.fi/mautrix-imessage/imessage"
)

type iMessageHandler struct {
	bridge *Bridge
	log    log.Logger
	stop   chan struct{}
}

func NewiMessageHandler(bridge *Bridge) *iMessageHandler {
	return &iMessageHandler{
		bridge: bridge,
		log:    bridge.Log.Sub("iMessage"),
		stop:   make(chan struct{}),
	}
}

func (imh *iMessageHandler) Start() {
	messages := imh.bridge.IM.MessageChan()
	readReceipts := imh.bridge.IM.ReadReceiptChan()
	typingNotifications := imh.bridge.IM.TypingNotificationChan()
	chat := imh.bridge.IM.ChatChan()
	contact := imh.bridge.IM.ContactChan()
	messageStatuses := imh.bridge.IM.MessageStatusChan()
	for {
		select {
		case msg := <-messages:
			imh.HandleMessage(msg)
		case rr := <-readReceipts:
			imh.HandleReadReceipt(rr)
		case notif := <-typingNotifications:
			imh.HandleTypingNotification(notif)
		case c := <-chat:
			imh.HandleChat(c)
		case contact := <-contact:
			imh.HandleContact(contact)
		case status := <-messageStatuses:
			imh.HandleMessageStatus(status)
		case <-imh.stop:
			return
		}
	}
}

func (imh *iMessageHandler) HandleMessage(msg *imessage.Message) {
	// TODO trace log
	//imh.log.Debugfln("Received incoming message: %+v", msg)
	portal := imh.bridge.GetPortalByGUID(msg.ChatGUID)
	if len(portal.MXID) == 0 {
		portal.log.Infoln("Creating Matrix room to handle message")
		err := portal.CreateMatrixRoom(nil, nil)
		if err != nil {
			imh.log.Warnfln("Failed to create Matrix room to handle message: %v", err)
			return
		}
	}
	portal.Messages <- msg
}

func (imh *iMessageHandler) HandleMessageStatus(status *imessage.SendMessageStatus) {
	msg := imh.bridge.DB.Message.GetLastByOnlyGUID(status.GUID)
	if msg == nil {
		imh.log.Debugln("Ignoring message status for unknown message", status.GUID)
		return
	}
	portal := imh.bridge.GetPortalByGUID(msg.ChatGUID)
	if len(portal.GUID) == 0 {
		imh.log.Debugfln("Ignoring message status for message from unknown portal %s/%s", msg.GUID, msg.ChatGUID)
		return
	}
	imh.log.Debugfln("Processing message status with type %v for event %s/%s %s/%s", status.Status, msg.MXID, portal.MXID, msg.GUID, msg.ChatGUID)
	if status.Status == "sent" {
		portal.sendSuccessCheckpoint(msg.MXID)
	} else if status.Status == "failed" {
		evt, err := portal.MainIntent().GetEvent(portal.MXID, msg.MXID)
		if err != nil {
			imh.log.Warnfln("Failed to lookup event %s/%s %s/%s: %v", msg.MXID, portal.MXID, msg.GUID, msg.ChatGUID, err)
			return
		}
		errString := "internal error"
		if len(status.Message) != 0 {
			errString = status.Message
		} else if len(status.StatusCode) != 0 {
			errString = status.StatusCode
		}
		portal.sendErrorMessage(evt, errors.New(errString), true, appservice.StatusPermFailure)
	} else {
		imh.log.Infofln("Ignoring unused message status type %v for event %s/%s %s/%s", status.Status, msg.MXID, portal.MXID, msg.GUID, msg.ChatGUID)
		return
	}
}

func (imh *iMessageHandler) HandleReadReceipt(rr *imessage.ReadReceipt) {
	portal := imh.bridge.GetPortalByGUID(rr.ChatGUID)
	if len(portal.MXID) == 0 {
		return
	}
	portal.ReadReceipts <- rr
}

func (imh *iMessageHandler) HandleTypingNotification(notif *imessage.TypingNotification) {
	portal := imh.bridge.GetPortalByGUID(notif.ChatGUID)
	if len(portal.MXID) == 0 {
		return
	}
	_, err := portal.MainIntent().UserTyping(portal.MXID, notif.Typing, 60*1000)
	if err != nil {
		action := "typing"
		if !notif.Typing {
			action = "not typing"
		}
		portal.log.Warnln("Failed to mark %s as %s in %s: %v", portal.MainIntent().UserID, action, portal.MXID, err)
	}
}

func (imh *iMessageHandler) HandleChat(chat *imessage.ChatInfo) {
	portal := imh.bridge.GetPortalByGUID(chat.Identifier.String())
	if len(portal.MXID) > 0 {
		portal.log.Infoln("Syncing Matrix room to handle chat command")
		portal.SyncWithInfo(chat)
	} else if !chat.NoCreateRoom {
		portal.log.Infoln("Creating Matrix room to handle chat command")
		err := portal.CreateMatrixRoom(chat, nil)
		if err != nil {
			imh.log.Warnfln("Failed to create Matrix room to handle chat command: %v", err)
			return
		}
	}
}

func (imh *iMessageHandler) HandleContact(contact *imessage.Contact) {
	puppet := imh.bridge.GetPuppetByGUID(contact.UserGUID)
	if len(puppet.MXID) > 0 {
		puppet.log.Infoln("Syncing Puppet to handle contact command")
		puppet.SyncWithContact(contact)
	}
}

func (imh *iMessageHandler) Stop() {
	close(imh.stop)
}
