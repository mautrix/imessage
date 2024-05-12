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
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"runtime/debug"
	"time"

	"go.mau.fi/util/dbutil"
	"golang.org/x/exp/slices"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/bridge/bridgeconfig"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-imessage/database"
	"go.mau.fi/mautrix-imessage/imessage"
)

var (
	PortalCreationDummyEvent = event.Type{Type: "fi.mau.dummy.portal_created", Class: event.MessageEventType}
	BackfillStatusEvent      = event.Type{Type: "com.beeper.backfill_status", Class: event.StateEventType}
)

func (user *User) handleHistorySyncsLoop(ctx context.Context) {
	if !user.bridge.Config.Bridge.Backfill.OnlyBackfill || !user.bridge.SpecVersions.Supports(mautrix.BeeperFeatureBatchSending) {
		user.log.Infofln("Not backfilling history since OnlyBackfill is disabled")
		return
	}

	// Start the backfill queue.
	user.BackfillQueue = &BackfillQueue{
		BackfillQuery:  user.bridge.DB.Backfill,
		reCheckChannel: make(chan bool),
		log:            user.log.Sub("BackfillQueue"),
	}

	// Handle all backfills in the same loop. Since new chats will not need to
	// be handled by this loop, priority is all that is needed.
	go user.HandleBackfillRequestsLoop(ctx)
}

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
		if portal.bridge.Config.Bridge.Backfill.InitialLimit <= 0 {
			portal.log.Debugfln("Not backfilling: initial limit is 0")
			return
		}
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
	allSkipped := true
	for index, msg := range messages {
		if portal.bridge.DB.Message.GetByGUID(msg.ChatGUID, msg.GUID, 0) != nil {
			portal.log.Debugfln("Skipping duplicate message %s at start of forward backfill batch", msg.GUID)
			continue
		}
		allSkipped = false
		messages = messages[index:]
		break
	}
	if err != nil {
		portal.log.Errorln("Failed to fetch messages for backfilling:", err)
		go portal.bridge.IM.SendBackfillResult(portal.GUID, backfillID, false, nil)
	} else if len(messages) == 0 || allSkipped {
		portal.log.Debugln("Nothing to backfill")
	} else {
		portal.sendBackfill(backfillID, messages, true, false, false)
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
	unreadThreshold := time.Duration(portal.bridge.Config.Bridge.Backfill.UnreadHoursThreshold) * time.Hour
	var isRead bool
	for _, msg := range messages {
		if msg.Tapback != nil {
			continue
		}

		intent := portal.getIntentForMessage(msg, nil)
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
		isRead = msg.IsRead || msg.IsFromMe || (unreadThreshold >= 0 && time.Since(msg.Time) > unreadThreshold)
	}
	return events, metas, metaIndexes, isRead, nil
}

func (portal *Portal) convertTapbacks(messages []*imessage.Message) ([]*event.Event, []messageWithIndex, map[messageIndex]int, bool, error) {
	events := make([]*event.Event, 0, len(messages))
	metas := make([]messageWithIndex, 0, len(messages))
	metaIndexes := make(map[messageIndex]int, len(messages))
	unreadThreshold := time.Duration(portal.bridge.Config.Bridge.Backfill.UnreadHoursThreshold) * time.Hour
	var isRead bool
	for _, msg := range messages {
		//Only want tapbacks
		if msg.Tapback == nil {
			continue
		}

		intent := portal.getIntentForMessage(msg, nil)
		dbMessage := portal.bridge.DB.Message.GetByGUID(portal.GUID, msg.Tapback.TargetGUID, msg.Tapback.TargetPart)
		if dbMessage == nil {
			//TODO BUG: This occurs when trying to find the target reaction for a rich link, related to #183
			portal.log.Errorfln("Failed to get target message for tabpack, %+v", msg)
			continue
		}

		evt := &event.Event{
			Sender:    intent.UserID,
			Type:      event.EventReaction,
			Timestamp: msg.Time.UnixMilli(),
			Content: event.Content{
				Parsed: &event.ReactionEventContent{
					RelatesTo: event.RelatesTo{
						Type:    event.RelAnnotation,
						EventID: dbMessage.MXID,
						Key:     msg.Tapback.Type.Emoji(),
					},
				},
			},
		}

		intent.AddDoublePuppetValue(&evt.Content)
		if portal.bridge.Config.Homeserver.Software == bridgeconfig.SoftwareHungry {
			evt.ID = portal.deterministicEventID(msg.GUID, 0)
		}

		events = append(events, evt)
		metas = append(metas, messageWithIndex{msg, intent, dbMessage, 0})
		metaIndexes[messageIndex{msg.GUID, 0}] = len(metas)

		isRead = msg.IsRead || msg.IsFromMe || (unreadThreshold >= 0 && time.Since(msg.Time) > unreadThreshold)
	}
	return events, metas, metaIndexes, isRead, nil
}

func (portal *Portal) sendBackfill(backfillID string, messages []*imessage.Message, forward, forwardIfNoMessages, markAsRead bool) (success bool) {
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
	batchSending := portal.bridge.SpecVersions.Supports(mautrix.BeeperFeatureBatchSending)

	var validMessages []*imessage.Message
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
		if msg.Tapback != nil && msg.Tapback.Remove {
			//If we don't process it, there won't be a reaction; at least for BB, we never have to remove a reaction
			portal.log.Debugln("Skipping", msg.GUID, "in backfill (it was a remove tapback)")
			continue
		}

		validMessages = append(validMessages, msg)
	}

	events, metas, metaIndexes, isRead, err := portal.convertBackfill(validMessages)
	if err != nil {
		portal.log.Errorfln("Failed to convert messages for backfill: %v", err)
		return false
	}
	portal.log.Debugfln("Converted %d messages into %d message events to backfill", len(messages), len(events))
	if len(events) == 0 {
		return true
	}

	eventIDs, sendErr := portal.sendBackfillToMatrixServer(batchSending, forward, forwardIfNoMessages, markAsRead, isRead, events, metas, metaIndexes)
	if sendErr != nil {
		return false
	}
	portal.addBackfillToDB(metas, eventIDs, idMap, backfillID)

	//We have to process tapbacks after all other messages because we need texts in the DB in order to target them
	events, metas, metaIndexes, isRead, err = portal.convertTapbacks(validMessages)
	if err != nil {
		portal.log.Errorfln("Failed to convert tapbacks for backfill: %v", err)
		return false
	}
	portal.log.Debugfln("Converted %d messages into %d tapbacks events to backfill", len(messages), len(events))
	if len(events) == 0 {
		return true
	}

	eventIDs, sendErr = portal.sendBackfillToMatrixServer(batchSending, forward, forwardIfNoMessages, markAsRead, isRead, events, metas, metaIndexes)
	if sendErr != nil {
		return false
	}
	portal.addBackfillToDB(metas, eventIDs, idMap, backfillID)

	portal.log.Infofln("Finished backfill %s", backfillID)
	return true
}

func (portal *Portal) addBackfillToDB(metas []messageWithIndex, eventIDs []id.EventID, idMap map[string][]id.EventID, backfillID string) {
	for i, meta := range metas {
		idMap[meta.GUID] = append(idMap[meta.GUID], eventIDs[i])
	}
	txn, err := portal.bridge.DB.Begin()
	if err != nil {
		portal.log.Errorln("Failed to start transaction to save batch messages:", err)
	}
	portal.log.Debugfln("Inserting %d event IDs to database to finish backfill %s", len(eventIDs), backfillID)
	portal.finishBackfill(txn, eventIDs, metas)
	portal.Update(txn)
	err = txn.Commit()
	if err != nil {
		portal.log.Errorln("Failed to commit transaction to save batch messages:", err)
	}
}

func (portal *Portal) sendBackfillToMatrixServer(batchSending, forward, forwardIfNoMessages, markAsRead, isRead bool, events []*event.Event, metas []messageWithIndex,
	metaIndexes map[messageIndex]int) (eventIDs []id.EventID, err error) {
	if batchSending {
		req := &mautrix.ReqBeeperBatchSend{
			Events:              events,
			Forward:             forward,
			ForwardIfNoMessages: forwardIfNoMessages,
		}
		if isRead || markAsRead {
			req.MarkReadBy = portal.bridge.user.MXID
		}
		resp, err := portal.MainIntent().BeeperBatchSend(portal.MXID, req)
		if err != nil {
			portal.log.Errorln("Failed to batch send history:", err)
			return nil, err
		}
		eventIDs = resp.EventIDs
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
				return nil, err
			}
			eventIDs[i] = resp.EventID
		}
		if (isRead || markAsRead) && portal.bridge.user.DoublePuppetIntent != nil {
			lastReadEvent := eventIDs[len(eventIDs)-1]
			err := portal.markRead(portal.bridge.user.DoublePuppetIntent, lastReadEvent, time.Time{})
			if err != nil {
				portal.log.Warnfln("Failed to mark %s as read with double puppet: %v", lastReadEvent, err)
			}
		}
	}
	return eventIDs, nil
}

func (portal *Portal) finishBackfill(txn dbutil.Transaction, eventIDs []id.EventID, metas []messageWithIndex) {
	for i, info := range metas {
		if info.Tapback != nil {
			if info.Tapback.Remove {
				continue
			}
			dbTapback := portal.bridge.DB.Tapback.New()
			dbTapback.PortalGUID = portal.GUID
			dbTapback.SenderGUID = info.Sender.String()
			dbTapback.MessageGUID = info.TapbackTarget.GUID
			dbTapback.MessagePart = info.TapbackTarget.Part
			dbTapback.GUID = info.GUID
			dbTapback.Type = info.Tapback.Type
			dbTapback.MXID = eventIDs[i]
			dbTapback.Insert(txn)
		} else {
			dbMessage := portal.bridge.DB.Message.New()
			dbMessage.PortalGUID = portal.GUID
			dbMessage.SenderGUID = info.Sender.String()
			dbMessage.GUID = info.GUID
			dbMessage.Part = info.Index
			dbMessage.Timestamp = info.Time.UnixMilli()
			dbMessage.MXID = eventIDs[i]
			dbMessage.Insert(txn)
		}
	}
}

func (user *User) backfillInChunks(req *database.Backfill, portal *Portal) {
	if len(portal.MXID) == 0 {
		user.log.Errorfln("Portal %s has no room ID, but backfill was requested", portal.GUID)
		return
	}
	portal.Sync(false)

	backfillState := user.bridge.DB.Backfill.GetBackfillState(user.MXID, portal.GUID)
	if backfillState == nil {
		backfillState = user.bridge.DB.Backfill.NewBackfillState(user.MXID, portal.GUID)
	}
	backfillState.SetProcessingBatch(true)
	defer backfillState.SetProcessingBatch(false)
	portal.updateBackfillStatus(backfillState)

	var timeStart = imessage.AppleEpoch
	if req.TimeStart != nil {
		timeStart = *req.TimeStart
		user.log.Debugfln("Limiting backfill to start at %v", timeStart)
	}

	var timeEnd time.Time
	var forwardIfNoMessages, shouldMarkAsRead bool
	if req.TimeEnd != nil {
		timeEnd = *req.TimeEnd
		user.log.Debugfln("Limiting backfill to end at %v", req.TimeEnd)
		forwardIfNoMessages = true
	} else {
		firstMessage := portal.bridge.DB.Message.GetFirstInChat(portal.GUID)
		if firstMessage != nil {
			timeEnd = firstMessage.Time().Add(-1 * time.Millisecond)
			user.log.Debugfln("Limiting backfill to end at %v", timeEnd)
		} else {
			// Portal is empty, but no TimeEnd was set.
			user.log.Errorln("Portal %s is empty, but no TimeEnd was set", portal.MXID)
			return
		}
	}

	backfillID := fmt.Sprintf("bridge-chunk-%s::%s-%s::%d", portal.GUID, timeStart, timeEnd, time.Now().UnixMilli())

	// If the message was before the unread hours threshold, mark it as
	// read.
	lastMessages, err := user.bridge.IM.GetMessagesWithLimit(portal.GUID, 1, backfillID)
	if err != nil {
		user.log.Errorfln("Failed to get last message from database")
		return
	} else if len(lastMessages) == 1 {
		shouldMarkAsRead = user.bridge.Config.Bridge.Backfill.UnreadHoursThreshold > 0 &&
			lastMessages[0].Time.Before(time.Now().Add(time.Duration(-user.bridge.Config.Bridge.Backfill.UnreadHoursThreshold)*time.Hour))
	}

	var allMsgs []*imessage.Message
	if req.MaxTotalEvents >= 0 {
		allMsgs, err = user.bridge.IM.GetMessagesBeforeWithLimit(portal.GUID, timeEnd, req.MaxTotalEvents)
	} else {
		allMsgs, err = user.bridge.IM.GetMessagesBetween(portal.GUID, timeStart, timeEnd)
	}
	if err != nil {
		user.log.Errorfln("Failed to get messages between %v and %v: %v", req.TimeStart, timeEnd, err)
		return
	}

	if len(allMsgs) == 0 {
		user.log.Debugfln("Not backfilling %s (%v - %v): no bridgeable messages found", portal.GUID, timeStart, timeEnd)
		return
	}

	user.log.Infofln("Backfilling %d messages in %s, %d messages at a time (queue ID: %d)", len(allMsgs), portal.GUID, req.MaxBatchEvents, req.QueueID)
	toBackfill := allMsgs[0:]
	for len(toBackfill) > 0 {
		var msgs []*imessage.Message
		if len(toBackfill) <= req.MaxBatchEvents || req.MaxBatchEvents < 0 {
			msgs = toBackfill
			toBackfill = nil
		} else {
			msgs = toBackfill[:req.MaxBatchEvents]
			toBackfill = toBackfill[req.MaxBatchEvents:]
		}

		if len(msgs) > 0 {
			time.Sleep(time.Duration(req.BatchDelay) * time.Second)
			user.log.Debugfln("Backfilling %d messages in %s (queue ID: %d)", len(msgs), portal.GUID, req.QueueID)

			// The sendBackfill function wants the messages in order, but the
			// queries give it in reversed order.
			slices.Reverse(msgs)
			portal.sendBackfill(backfillID, msgs, false, forwardIfNoMessages, shouldMarkAsRead)
		}
	}
	user.log.Debugfln("Finished backfilling %d messages in %s (queue ID: %d)", len(allMsgs), portal.GUID, req.QueueID)

	if req.TimeStart == nil && req.TimeEnd == nil {
		// If both the start time and end time are nil, then this is the max
		// history backfill, so there is no more history to backfill.
		backfillState.BackfillComplete = true
		backfillState.FirstExpectedTimestamp = uint64(allMsgs[len(allMsgs)-1].Time.UnixMilli())
		backfillState.Upsert()
		portal.updateBackfillStatus(backfillState)
	}
}

func (portal *Portal) updateBackfillStatus(backfillState *database.BackfillState) {
	backfillStatus := "backfilling"
	if backfillState.BackfillComplete {
		backfillStatus = "complete"
	}

	_, err := portal.bridge.Bot.SendStateEvent(portal.MXID, BackfillStatusEvent, "", map[string]any{
		"status":          backfillStatus,
		"first_timestamp": backfillState.FirstExpectedTimestamp * 1000,
	})
	if err != nil {
		portal.log.Errorln("Error sending backfill status event:", err)
	}
}
