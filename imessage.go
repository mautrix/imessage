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
		err := portal.CreateMatrixRoom(nil)
		if err != nil {
			imh.log.Warnfln("Failed to create Matrix room to handle message: %v", err)
			return
		}
	}
	portal.Messages <- msg
}

func (imh *iMessageHandler) HandleReadReceipt(rr *imessage.ReadReceipt) {
	portal := imh.bridge.GetPortalByGUID(rr.ChatGUID)
	if len(portal.MXID) == 0 {
		return
	}
	var intent *appservice.IntentAPI
	if rr.IsFromMe {
		intent = imh.bridge.user.DoublePuppetIntent
	} else if rr.SenderGUID == rr.ChatGUID {
		intent = portal.MainIntent()
	} else {
		portal.log.Debugfln("Dropping unexpected read receipt %+v", *rr)
		return
	}
	if intent == nil {
		return
	}
	message := imh.bridge.DB.Message.GetLastByGUID(portal.GUID, rr.ReadUpTo)
	if message == nil {
		portal.log.Debugfln("Dropping read receipt for %s: message not found in db", rr.ReadUpTo)
		return
	}
	err := intent.MarkRead(portal.MXID, message.MXID)
	if err != nil {
		portal.log.Warnln("Failed to send read receipt for %s from %s: %v", message.MXID, intent.UserID)
	}
}

func (imh *iMessageHandler) HandleTypingNotification(notif *imessage.TypingNotification) {
	portal := imh.bridge.GetPortalByGUID(notif.ChatGUID)
	if len(portal.MXID) == 0 {
		return
	}
	_, err := portal.MainIntent().UserTyping(portal.MXID, notif.Typing, 60 * 1000)
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
		err := portal.CreateMatrixRoom(chat)
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
