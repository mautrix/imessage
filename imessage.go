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
	for {
		select {
		case msg := <-messages:
			imh.HandleMessage(msg)
		case rr := <-readReceipts:
			imh.HandleReadReceipt(rr)
		case <-imh.stop:
			break
		}
	}
}

func (imh *iMessageHandler) HandleMessage(msg *imessage.Message) {
	// TODO trace log
	//imh.log.Debugfln("Received incoming message: %+v", msg)
	portal := imh.bridge.GetPortalByGUID(msg.ChatGUID)
	if len(portal.MXID) == 0 {
		err := portal.CreateMatrixRoom()
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
	message := imh.bridge.DB.Message.GetByGUID(portal.GUID, rr.ReadUpTo)
	if message == nil {
		portal.log.Debugfln("Dropping read receipt for %s: message not found in db", rr.ReadUpTo)
		return
	}
	err := intent.MarkRead(portal.MXID, message.MXID)
	if err != nil {
		portal.log.Warnln("Failed to send read receipt for %s from %s: %v", message.MXID, intent.UserID)
	}
}

func (imh *iMessageHandler) Stop() {
	close(imh.stop)
}
