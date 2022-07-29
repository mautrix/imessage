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
	"time"

	log "maunium.net/go/maulogger/v2"

	"go.mau.fi/mautrix-imessage/imessage"
)

type iMessageHandler struct {
	bridge *IMBridge
	log    log.Logger
	stop   chan struct{}
}

func NewiMessageHandler(bridge *IMBridge) *iMessageHandler {
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
	chats := imh.bridge.IM.ChatChan()
	contacts := imh.bridge.IM.ContactChan()
	messageStatuses := imh.bridge.IM.MessageStatusChan()
	for {
		select {
		case msg := <-messages:
			imh.HandleMessage(msg)
		case rr := <-readReceipts:
			imh.HandleReadReceipt(rr)
		case notif := <-typingNotifications:
			imh.HandleTypingNotification(notif)
		case chat := <-chats:
			imh.HandleChat(chat)
		case contact := <-contacts:
			imh.HandleContact(contact)
		case status := <-messageStatuses:
			imh.HandleMessageStatus(status)
		case <-imh.stop:
			return
		}
	}
}

// resolveChatGUIDWithCorrelationIdentifier takes a GUID/UUID pair, and determines whether a different, pre-existing chat GUID should be used instead.
// if a pre-existing chat is found, and a portal exists with the incoming GUID, the portal will be tombstoned and forgotten.
func (imh *iMessageHandler) resolveChatGUIDWithCorrelationIdentifier(guid string, correlationID string) string {
	if !imh.bridge.IM.Capabilities().Correlation || len(correlationID) == 0 {
		// no correlation, passthrough
		return guid
	}
	parsed := imessage.ParseIdentifier(guid)
	if parsed.IsGroup {
		// we don't correlate groups right now, passthrough
		return guid
	}
	if portal := imh.bridge.DB.Portal.GetByCorrelationID(correlationID); portal != nil {
		// there's already a portal with this correlation ID
		if portal.GUID != guid {
			// the existing portal has a different GUID
			if existingPortal := imh.bridge.DB.Portal.GetByGUID(guid); existingPortal != nil {
				// the incoming GUID has an existing portal
				if len(existingPortal.MXID) == 0 {
					// its just a row, delete it
					existingPortal.Delete()
				} else {
					// tombstone it
					imh.bridge.newDummyPortal(existingPortal).mergeIntoPortal(portal.MXID, "This room has been deduplicated.")
				}
			}
		}
		return portal.GUID
	}
	imh.bridge.DB.Portal.StoreCorrelation(guid, correlationID)
	return guid
}

// resolveIdentifiers takes a chat GUID, sender GUID, and correlation ID, and maps the GUIDs to pre-existing GUIDs if possible.
// this preserves consistency and makes the sender appear to come from the same person, avoiding issues where the DM sender
// is not a participant and cannot join the portal.
func (imh *iMessageHandler) resolveIdentifiers(guid string, correlationID string, senderID string, senderCorrelationID string, identifier imessage.Identifier, fromMe bool) (newGUID string, newSender string, newIdentifier imessage.Identifier) {
	if !imh.bridge.IM.Capabilities().Correlation {
		// no correlation
		return guid, senderID, identifier
	}
	if len(senderID) > 0 && len(senderCorrelationID) > 0 {
		// store the correlation for this sender
		imh.bridge.DB.Puppet.StoreCorrelation(senderID, correlationID)
	}
	parsed := imessage.ParseIdentifier(guid)
	if parsed.IsGroup || len(correlationID) == 0 {
		// todo: correlate group senders, requires knowledge of who is in the portal, this is not easily accessible right now.
		return guid, senderID, identifier
	}
	newGUID = imh.resolveChatGUIDWithCorrelationIdentifier(guid, correlationID)
	return newGUID, senderID, identifier
}

func (imh *iMessageHandler) HandleMessage(msg *imessage.Message) {
	// TODO trace log
	//imh.log.Debugfln("Received incoming message: %+v", msg)
	msg.ChatGUID, msg.JSONSenderGUID, msg.Sender = imh.resolveIdentifiers(msg.ChatGUID, msg.CorrelationID, msg.JSONSenderGUID, msg.SenderCorrelationID, msg.Sender, msg.IsFromMe)
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
	status.ChatGUID = imh.resolveChatGUIDWithCorrelationIdentifier(status.ChatGUID, status.CorrelationID)
	portal := imh.bridge.GetPortalByGUID(status.ChatGUID)
	if len(portal.GUID) == 0 {
		imh.log.Debugfln("Ignoring message status for message from unknown portal %s/%s", status.GUID, status.ChatGUID)
		return
	}
	portal.MessageStatuses <- status
}

func (imh *iMessageHandler) HandleReadReceipt(rr *imessage.ReadReceipt) {
	rr.ChatGUID, rr.SenderGUID, _ = imh.resolveIdentifiers(rr.ChatGUID, rr.CorrelationID, rr.SenderGUID, rr.SenderCorrelationID, imessage.Identifier{}, rr.IsFromMe)
	portal := imh.bridge.GetPortalByGUID(rr.ChatGUID)
	if len(portal.MXID) == 0 {
		return
	}
	portal.ReadReceipts <- rr
}

func (imh *iMessageHandler) HandleTypingNotification(notif *imessage.TypingNotification) {
	notif.ChatGUID = imh.resolveChatGUIDWithCorrelationIdentifier(notif.ChatGUID, notif.CorrelationID)
	portal := imh.bridge.GetPortalByGUID(notif.ChatGUID)
	if len(portal.MXID) == 0 {
		return
	}
	_, err := portal.MainIntent().UserTyping(portal.MXID, notif.Typing, 60*time.Second)
	if err != nil {
		action := "typing"
		if !notif.Typing {
			action = "not typing"
		}
		portal.log.Warnln("Failed to mark %s as %s in %s: %v", portal.MainIntent().UserID, action, portal.MXID, err)
	}
}

func (imh *iMessageHandler) HandleChat(chat *imessage.ChatInfo) {
	chat.JSONChatGUID = imh.resolveChatGUIDWithCorrelationIdentifier(chat.JSONChatGUID, chat.CorrelationID)
	chat.Identifier = imessage.ParseIdentifier(chat.JSONChatGUID)
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
