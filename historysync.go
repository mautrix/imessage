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

package main

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"runtime/debug"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/bridge/bridgeconfig"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/util/dbutil"

	"go.mau.fi/mautrix-imessage/database"
	"go.mau.fi/mautrix-imessage/imessage"
)

var PortalCreationDummyEvent = event.Type{Type: "fi.mau.dummy.portal_created", Class: event.MessageEventType}
var HistorySyncMarker = event.Type{Type: "org.matrix.msc2716.marker", Class: event.MessageEventType}

func (portal *Portal) lockBackfill() {
	portal.backfillLock.Lock()
	portal.backfillWait.Wait()
	portal.backfillWait.Add(1)
	select {
	case portal.backfillStart <- struct{}{}:
	default:
	}
}

func (portal *Portal) unlockBackfill() {
	portal.backfillWait.Done()
	portal.backfillLock.Unlock()
}

func (portal *Portal) forwardBackfill() {
	defer func() {
		if err := recover(); err != nil {
			portal.log.Errorfln("Panic while backfilling: %v\n%s", err, string(debug.Stack()))
		}
	}()

	var messages []*imessage.Message
	var err error
	var backfillID string
	lastMessage := portal.bridge.DB.Message.GetLastInChat(portal.GUID)
	if lastMessage == nil && portal.BackfillStartTS == 0 {
		portal.log.Debugfln("Fetching up to %d messages for initial backfill", portal.bridge.Config.Bridge.Backfill.InitialLimit)
		backfillID = fmt.Sprintf("bridge-initial-%s::%d", portal.Identifier.LocalID, time.Now().UnixMilli())
		messages, err = portal.bridge.IM.GetMessagesWithLimit(portal.GUID, portal.bridge.Config.Bridge.Backfill.InitialLimit, backfillID)
	} else if lastMessage != nil {
		portal.log.Debugfln("Fetching messages since %s for catchup backfill", lastMessage.Time().String())
		backfillID = fmt.Sprintf("bridge-catchup-msg-%s::%s::%d", portal.Identifier.LocalID, lastMessage.GUID, time.Now().UnixMilli())
		messages, err = portal.bridge.IM.GetMessagesSinceDate(portal.GUID, lastMessage.Time().Add(1*time.Millisecond), backfillID)
	} else if portal.BackfillStartTS != 0 {
		startTime := time.UnixMilli(portal.BackfillStartTS)
		portal.log.Debugfln("Fetching messages since %s for catchup backfill after portal recovery", startTime.String())
		backfillID = fmt.Sprintf("bridge-catchup-ts-%s::%d::%d", portal.Identifier.LocalID, startTime.UnixMilli(), time.Now().UnixMilli())
		messages, err = portal.bridge.IM.GetMessagesSinceDate(portal.GUID, startTime, backfillID)
	}
	for index, msg := range messages {
		if portal.bridge.DB.Message.GetByGUID(msg.ChatGUID, msg.GUID, 0) != nil {
			portal.log.Debugfln("Skipping duplicate message %s at start of forward backfill batch", msg.GUID)
			continue
		}
		messages = messages[index:]
		break
	}
	if err != nil {
		portal.log.Errorln("Failed to fetch messages for backfilling:", err)
		go portal.bridge.IM.SendBackfillResult(portal.GUID, backfillID, false, nil)
	} else if len(messages) == 0 {
		portal.log.Debugln("Nothing to backfill")
	} else {
		portal.sendBackfill(backfillID, messages, true)
	}
}

func (portal *Portal) deterministicEventID(messageID string, partIndex int) id.EventID {
	data := fmt.Sprintf("%s/imessage/%s/%d", portal.MXID, messageID, partIndex)
	sum := sha256.Sum256([]byte(data))
	domain := "imessage.apple.com"
	if portal.bridge.Config.IMessage.Platform == "android" {
		domain = "sms.android.local"
	}
	return id.EventID(fmt.Sprintf("$%s:%s", base64.RawURLEncoding.EncodeToString(sum[:]), domain))
}

type messageWithIndex struct {
	*imessage.Message
	Intent        *appservice.IntentAPI
	TapbackTarget *database.Message
	Index         int
}

type messageIndex struct {
	GUID  string
	Index int
}

func (portal *Portal) convertBackfill(messages []*imessage.Message) ([]*event.Event, []messageWithIndex, map[messageIndex]int, bool, error) {
	events := make([]*event.Event, 0, len(messages))
	metas := make([]messageWithIndex, 0, len(messages))
	metaIndexes := make(map[messageIndex]int, len(messages))
	var isRead bool
	for _, msg := range messages {
		if msg.ItemType != imessage.ItemTypeMessage && msg.Tapback == nil {
			portal.log.Debugln("Skipping", msg.GUID, "in backfill (not a message)")
			continue
		}
		intent := portal.getIntentForMessage(msg, nil)
		if intent == nil {
			portal.log.Debugln("Skipping", msg.GUID, "in backfill (didn't get an intent)")
			continue
		}

		if msg.Tapback != nil {
			// TODO handle tapbacks
			portal.log.Debugln("Skipping tapback", msg.GUID, "in backfill")
			continue
		}

		converted := portal.convertiMessage(msg, intent)
		for index, conv := range converted {
			evt := &event.Event{
				Sender:    intent.UserID,
				Type:      conv.Type,
				Timestamp: msg.Time.UnixMilli(),
				Content: event.Content{
					Parsed: conv.Content,
					Raw:    conv.Extra,
				},
			}
			var err error
			evt.Type, err = portal.encrypt(intent, &evt.Content, evt.Type)
			if err != nil {
				return nil, nil, nil, false, err
			}
			intent.AddDoublePuppetValue(&evt.Content)
			if portal.bridge.Config.Homeserver.Software == bridgeconfig.SoftwareHungry {
				evt.ID = portal.deterministicEventID(msg.GUID, index)
			}

			events = append(events, evt)
			metas = append(metas, messageWithIndex{msg, intent, nil, index})
			metaIndexes[messageIndex{msg.GUID, index}] = len(metas)
		}
		isRead = msg.IsRead || msg.IsFromMe
	}
	return events, metas, metaIndexes, isRead, nil
}

func (portal *Portal) sendBackfill(backfillID string, messages []*imessage.Message, forward bool) (success bool) {
	idMap := make(map[string][]id.EventID, len(messages))
	for _, msg := range messages {
		idMap[msg.GUID] = []id.EventID{}
	}
	defer func() {
		err := recover()
		if err != nil {
			success = false
			portal.log.Errorfln("Backfill task panicked: %v\n%s", err, debug.Stack())
		}
		portal.bridge.IM.SendBackfillResult(portal.GUID, backfillID, success, idMap)
	}()
	if !portal.bridge.Config.Bridge.Backfill.MSC2716 && !forward {
		portal.log.Debugfln("Dropping non-forward backfill %s as MSC2716 is not enabled", backfillID)
		return true
	}
	events, metas, metaIndexes, isRead, err := portal.convertBackfill(messages)
	if err != nil {
		portal.log.Errorfln("Failed to convert messages for backfill: %v", err)
		return false
	}
	portal.log.Debugfln("Converted %d messages into %d events to backfill", len(messages), len(events))
	if len(events) == 0 {
		return true
	}
	var eventIDs []id.EventID
	var baseInsertionID id.EventID
	if portal.bridge.Config.Bridge.Backfill.MSC2716 {
		req := &mautrix.ReqBatchSend{
			StateEventsAtStart: nil,
			Events:             events,
		}
		saveBatchID := forward
		if forward {
			req.BeeperNewMessages = forward
			lastMessage := portal.bridge.DB.Message.GetLastInChat(portal.GUID)
			if lastMessage != nil {
				req.PrevEventID = lastMessage.MXID
			} else {
				saveBatchID = true
				req.PrevEventID = portal.FirstEventID
			}
		} else {
			req.PrevEventID = portal.FirstEventID
			req.BatchID = portal.NextBatchID
		}
		if req.PrevEventID == "" && portal.bridge.Config.Homeserver.Software != bridgeconfig.SoftwareHungry {
			portal.log.Debugfln("Dropping backfill %s as previous event is not known", backfillID)
			return true
		}
		if isRead {
			req.BeeperMarkReadBy = portal.bridge.user.MXID
		}
		resp, err := portal.MainIntent().BatchSend(portal.MXID, req)
		if err != nil {
			portal.log.Errorln("Failed to batch send history:", err)
			return false
		}
		if saveBatchID {
			portal.NextBatchID = resp.NextBatchID
		}
		eventIDs = resp.EventIDs
		baseInsertionID = resp.BaseInsertionEventID
	} else {
		eventIDs = make([]id.EventID, len(events))
		for i, evt := range events {
			meta := metas[i]
			// Fill reply metadata for messages we just sent
			if meta.ReplyToGUID != "" && !meta.ReplyProcessed {
				replyIndex, ok := metaIndexes[messageIndex{meta.ReplyToGUID, meta.ReplyToPart}]
				if ok && replyIndex > 0 && replyIndex < len(eventIDs) && len(eventIDs[replyIndex]) > 0 {
					evt.Content.AsMessage().RelatesTo = (&event.RelatesTo{}).SetReplyTo(eventIDs[replyIndex])
				}
			}
			resp, err := meta.Intent.SendMassagedMessageEvent(portal.MXID, evt.Type, &evt.Content, evt.Timestamp)
			if err != nil {
				portal.log.Errorfln("Failed to send event #%d in history: %v", i, err)
				return false
			}
			eventIDs[i] = resp.EventID
		}
		if isRead && portal.bridge.user.DoublePuppetIntent != nil {
			lastReadEvent := eventIDs[len(eventIDs)-1]
			err := portal.markRead(portal.bridge.user.DoublePuppetIntent, lastReadEvent, time.Time{})
			if err != nil {
				portal.log.Warnfln("Failed to mark %s as read with double puppet: %v", lastReadEvent, err)
			}
		}
	}
	for i, meta := range metas {
		idMap[meta.GUID] = append(idMap[meta.GUID], eventIDs[i])
	}
	txn, err := portal.bridge.DB.Begin()
	if err != nil {
		portal.log.Errorln("Failed to start transaction to save batch messages:", err)
		return true
	}
	portal.log.Debugfln("Inserting %d event IDs to database to finish backfill %s", len(eventIDs), backfillID)
	portal.finishBackfill(txn, eventIDs, metas)
	portal.Update(txn)
	err = txn.Commit()
	if err != nil {
		portal.log.Errorln("Failed to commit transaction to save batch messages:", err)
	}
	if portal.bridge.Config.Bridge.Backfill.MSC2716 && (!forward || portal.bridge.Config.Homeserver.Software != bridgeconfig.SoftwareHungry) {
		go portal.sendPostBackfillDummy(baseInsertionID)
	}
	portal.log.Infofln("Finished backfill %s", backfillID)
	return true
}

func (portal *Portal) sendPostBackfillDummy(insertionEventId id.EventID) {
	resp, err := portal.MainIntent().SendMessageEvent(portal.MXID, HistorySyncMarker, map[string]interface{}{
		"org.matrix.msc2716.marker.insertion": insertionEventId,
		//"m.marker.insertion":                  insertionEventId,
	})
	if err != nil {
		portal.log.Errorln("Error sending post-backfill dummy event:", err)
		return
	} else {
		portal.log.Debugfln("Sent post-backfill dummy event %s", resp.EventID)
	}
}

func (portal *Portal) finishBackfill(txn dbutil.Transaction, eventIDs []id.EventID, metas []messageWithIndex) {
	for i, info := range metas {
		if info.Tapback != nil {
			if info.Tapback.Remove {
				// TODO handle removing tapbacks?
			} else {
				// TODO can existing tapbacks be modified in backfill?
				dbTapback := portal.bridge.DB.Tapback.New()
				dbTapback.ChatGUID = portal.GUID
				dbTapback.SenderGUID = info.Sender.String()
				dbTapback.MessageGUID = info.TapbackTarget.GUID
				dbTapback.MessagePart = info.TapbackTarget.Part
				dbTapback.GUID = info.GUID
				dbTapback.Type = info.Tapback.Type
				dbTapback.MXID = eventIDs[i]
				dbTapback.Insert(txn)
			}
		} else {
			dbMessage := portal.bridge.DB.Message.New()
			dbMessage.ChatGUID = portal.GUID
			dbMessage.SenderGUID = info.Sender.String()
			dbMessage.GUID = info.GUID
			dbMessage.Part = info.Index
			dbMessage.Timestamp = info.Time.UnixMilli()
			dbMessage.MXID = eventIDs[i]
			dbMessage.Insert(txn)
		}
	}
}
