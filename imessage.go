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
	backfillTasks := imh.bridge.IM.BackfillTaskChan()
	for {
		var start time.Time
		var thing string
		select {
		case msg := <-messages:
			thing = "message"
			start = time.Now()
			imh.HandleMessage(msg)
		case rr := <-readReceipts:
			thing = "read receipt"
			start = time.Now()
			imh.HandleReadReceipt(rr)
		case notif := <-typingNotifications:
			thing = "typing notification"
			start = time.Now()
			imh.HandleTypingNotification(notif)
		case chat := <-chats:
			thing = "chat"
			start = time.Now()
			imh.HandleChat(chat)
		case contact := <-contacts:
			thing = "contact"
			start = time.Now()
			imh.HandleContact(contact)
		case status := <-messageStatuses:
			thing = "message status"
			start = time.Now()
			imh.HandleMessageStatus(status)
		case backfillTask := <-backfillTasks:
			thing = "backfill task"
			start = time.Now()
			imh.HandleBackfillTask(backfillTask)
		case <-imh.stop:
			return
		}
		imh.log.Debugfln(
			"Handled %s in %s (queued: %dm/%dr/%dt/%dch/%dct/%ds/%db)",
			thing, time.Since(start),
			len(messages), len(readReceipts), len(typingNotifications), len(chats), len(contacts), len(messageStatuses), len(backfillTasks),
		)
	}
}

const PortalBufferTimeout = 10 * time.Second

func (imh *iMessageHandler) rerouteGroupMMS(portal *Portal, msg *imessage.Message) *Portal {
	if !imh.bridge.Config.Bridge.RerouteSMSGroupReplies ||
		!portal.Identifier.IsGroup ||
		portal.Identifier.Service != "SMS" ||
		msg.ReplyToGUID == "" ||
		portal.MXID != "" {
		return portal
	}
	mergedChatGUID := imh.bridge.DB.MergedChat.Get(portal.GUID)
	if mergedChatGUID != "" && mergedChatGUID != portal.GUID {
		newPortal := imh.bridge.GetPortalByGUID(mergedChatGUID)
		if newPortal.MXID != "" {
			imh.log.Debugfln("Rerouted %s from %s to %s based on merged_chat table", msg.GUID, msg.ChatGUID, portal.GUID)
			return newPortal
		}
	}
	checkedChatGUIDs := []string{msg.ChatGUID}
	isCheckedGUID := func(guid string) bool {
		for _, checkedGUID := range checkedChatGUIDs {
			if guid == checkedGUID {
				return true
			}
		}
		return false
	}
	replyMsg := msg
	for i := 0; i < 20; i++ {
		chatGUID := imh.bridge.DB.Message.FindChatByGUID(replyMsg.ReplyToGUID)
		if chatGUID != "" && !isCheckedGUID(chatGUID) {
			newPortal := imh.bridge.GetPortalByGUID(chatGUID)
			if newPortal.MXID != "" {
				imh.log.Debugfln("Rerouted %s from %s to %s based on reply metadata (found reply in local db)", msg.GUID, msg.ChatGUID, portal.GUID)
				imh.log.Debugfln("Merging %+v -> %s", checkedChatGUIDs, newPortal.GUID)
				imh.bridge.DB.MergedChat.Set(nil, newPortal.GUID, checkedChatGUIDs...)
				return newPortal
			}
			checkedChatGUIDs = append(checkedChatGUIDs, chatGUID)
		}
		var err error
		replyMsg, err = imh.bridge.IM.GetMessage(replyMsg.ReplyToGUID)
		if err != nil {
			imh.log.Warnfln("Failed to get reply target %s for rerouting %s: %v", replyMsg.ReplyToGUID, msg.GUID, err)
			break
		}
		if !isCheckedGUID(replyMsg.ChatGUID) {
			newPortal := imh.bridge.GetPortalByGUID(chatGUID)
			if newPortal.MXID != "" {
				imh.log.Debugfln("Rerouted %s from %s to %s based on reply metadata (got reply msg from connector)", msg.GUID, msg.ChatGUID, portal.GUID)
				imh.log.Debugfln("Merging %+v -> %s", checkedChatGUIDs, newPortal.GUID)
				imh.bridge.DB.MergedChat.Set(nil, newPortal.GUID, checkedChatGUIDs...)
				return newPortal
			}
			checkedChatGUIDs = append(checkedChatGUIDs, chatGUID)
		}
	}
	imh.log.Debugfln("Didn't find any existing room to reroute %s into (checked portals %+v)", msg.GUID, checkedChatGUIDs)
	return portal
}

func (imh *iMessageHandler) updateChatGUIDByThreadID(portal *Portal, threadID string) *Portal {
	if len(portal.MXID) > 0 || !portal.Identifier.IsGroup || threadID == "" || portal.bridge.Config.IMessage.Platform == "android" {
		return portal
	}
	existingByThreadID := imh.bridge.FindPortalsByThreadID(threadID)
	if len(existingByThreadID) > 1 {
		imh.log.Warnfln("Found multiple portals with thread ID %s (message chat guid: %s)", threadID, portal.GUID)
	} else if len(existingByThreadID) == 0 {
		// no need to log, this is just an ordinary new group
	} else if existingByThreadID[0].MXID != "" {
		imh.log.Infofln("Found existing portal %s for thread ID %s, merging %s into it", existingByThreadID[0].GUID, threadID, portal.GUID)
		existingByThreadID[0].reIDInto(portal.GUID, portal, true, false)
		return existingByThreadID[0]
	} else {
		imh.log.Infofln("Found existing portal %s for thread ID %s, but it doesn't have a room", existingByThreadID[0].GUID, threadID, portal.GUID)
	}
	return portal
}

func (imh *iMessageHandler) getPortalFromMessage(msg *imessage.Message) *Portal {
	portal := imh.rerouteGroupMMS(imh.bridge.GetPortalByGUID(msg.ChatGUID), msg)
	return portal
}

func (imh *iMessageHandler) HandleMessage(msg *imessage.Message) {
	imh.log.Debugfln("Received incoming message %s in %s (%s)", msg.GUID, msg.ChatGUID, msg.ThreadID)
	// TODO trace log
	//imh.log.Debugfln("Received incoming message: %+v", msg)
	portal := imh.updateChatGUIDByThreadID(
		imh.rerouteGroupMMS(
			imh.bridge.GetPortalByGUID(msg.ChatGUID),
			msg,
		),
		msg.ThreadID,
	)
	if len(portal.MXID) == 0 {
		if portal.ThreadID == "" {
			portal.ThreadID = msg.ThreadID
		}
		portal.log.Infoln("Creating Matrix room to handle message")
		err := portal.CreateMatrixRoom(nil, nil)
		if err != nil {
			imh.log.Warnfln("Failed to create Matrix room to handle message: %v", err)
			return
		}
	}
	select {
	case portal.Messages <- msg:
	case <-time.After(PortalBufferTimeout):
		imh.log.Errorln("Portal message buffer is still full after 10 seconds, dropping message %s", msg.GUID)
	}
}

func (imh *iMessageHandler) HandleMessageStatus(status *imessage.SendMessageStatus) {
	portal := imh.bridge.GetPortalByGUID(status.ChatGUID)
	if len(portal.MXID) == 0 {
		imh.log.Debugfln("Ignoring message status for message from unknown portal %s/%s", status.GUID, status.ChatGUID)
		return
	}
	select {
	case portal.MessageStatuses <- status:
	case <-time.After(PortalBufferTimeout):
		imh.log.Errorln("Portal message status buffer is still full after 10 seconds, dropping %+v", *status)
	}
}

func (imh *iMessageHandler) HandleReadReceipt(rr *imessage.ReadReceipt) {
	portal := imh.bridge.GetPortalByGUID(rr.ChatGUID)
	if len(portal.MXID) == 0 {
		imh.log.Debugfln("Ignoring read receipt in unknown portal %s", rr.ChatGUID)
		return
	}
	select {
	case portal.ReadReceipts <- rr:
	case <-time.After(PortalBufferTimeout):
		imh.log.Errorln("Portal read receipt buffer is still full after 10 seconds, dropping %+v", *rr)
	}
}

func (imh *iMessageHandler) HandleTypingNotification(notif *imessage.TypingNotification) {
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
	chat.Identifier = imessage.ParseIdentifier(chat.JSONChatGUID)
	portal := imh.bridge.GetPortalByGUID(chat.Identifier.String())
	portal = imh.updateChatGUIDByThreadID(portal, chat.ThreadID)
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

func (imh *iMessageHandler) HandleBackfillTask(task *imessage.BackfillTask) {
	if !imh.bridge.Config.Bridge.Backfill.Enable {
		imh.log.Warnfln("Connector sent backfill task, but backfill is disabled in bridge config")
		imh.bridge.IM.SendBackfillResult(task.ChatGUID, task.BackfillID, false, nil)
		return
	}
	portal := imh.bridge.GetPortalByGUID(task.ChatGUID)
	if len(portal.MXID) == 0 {
		portal.log.Errorfln("Tried to backfill chat %s with no portal", portal.GUID)
		imh.bridge.IM.SendBackfillResult(portal.GUID, task.BackfillID, false, nil)
		return
	}
	portal.log.Debugfln("Running backfill %s in background", task.BackfillID)
	go portal.sendBackfill(task.BackfillID, task.Messages, false)
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
