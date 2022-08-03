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
	"errors"
	"fmt"
	"html"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/gabriel-vasile/mimetype"
	log "maunium.net/go/maulogger/v2"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/bridge"
	"maunium.net/go/mautrix/crypto/attachment"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/pushrules"
	"maunium.net/go/mautrix/util/ffmpeg"

	"go.mau.fi/mautrix-imessage/database"
	"go.mau.fi/mautrix-imessage/imessage"
	"go.mau.fi/mautrix-imessage/imessage/ios"
	"go.mau.fi/mautrix-imessage/ipc"
)

func (br *IMBridge) GetPortalByMXID(mxid id.RoomID) *Portal {
	br.portalsLock.Lock()
	defer br.portalsLock.Unlock()
	portal, ok := br.portalsByMXID[mxid]
	if !ok {
		return br.loadDBPortal(br.DB.Portal.GetByMXID(mxid), "")
	}
	return portal
}

func (br *IMBridge) GetPortalByGUID(guid string) *Portal {
	br.portalsLock.Lock()
	defer br.portalsLock.Unlock()
	portal, ok := br.portalsByGUID[guid]
	if !ok {
		return br.loadDBPortal(br.DB.Portal.GetByGUID(guid), guid)
	}
	return portal
}

func (br *IMBridge) GetMessagesSince(chatGUID string, since time.Time) (out []string) {
	return br.DB.Message.GetIDsSince(chatGUID, since)
}

func (br *IMBridge) ReIDPortal(oldGUID, newGUID string) bool {
	br.portalsLock.Lock()
	defer br.portalsLock.Unlock()

	portal, ok := br.portalsByGUID[oldGUID]
	if !ok {
		portal = br.loadDBPortal(br.DB.Portal.GetByGUID(oldGUID), "")
		if portal == nil {
			br.Log.Debugfln("Ignoring chat ID change %s->%s, no portal with old ID found", oldGUID, newGUID)
			return false
		}
	}

	newPortal, ok := br.portalsByGUID[newGUID]
	if !ok {
		newPortal = br.loadDBPortal(br.DB.Portal.GetByGUID(newGUID), "")
	}
	if newPortal != nil {
		br.Log.Warnfln("Got chat ID change %s->%s, but portal with new ID already exists. Nuking old portal", oldGUID, newGUID)
		portal.Delete()
		if len(portal.MXID) > 0 && portal.bridge.user.DoublePuppetIntent != nil {
			_, _ = portal.bridge.user.DoublePuppetIntent.LeaveRoom(portal.MXID)
		}
		portal.Cleanup(false)
		return false
	}

	portal.log.Infoln("Changing chat ID to", newGUID)
	delete(br.portalsByGUID, portal.GUID)
	portal.Portal.ReID(newGUID)
	portal.Identifier = imessage.ParseIdentifier(portal.GUID)
	portal.log = portal.bridge.Log.Sub(fmt.Sprintf("Portal/%s", portal.GUID))
	br.portalsByGUID[portal.GUID] = portal
	if len(portal.MXID) > 0 {
		portal.UpdateBridgeInfo()
	}
	portal.log.Debugln("Chat ID changed successfully")
	return true
}

func (br *IMBridge) GetAllPortals() []*Portal {
	return br.dbPortalsToPortals(br.DB.Portal.GetAll())
}

func (br *IMBridge) dbPortalsToPortals(dbPortals []*database.Portal) []*Portal {
	br.portalsLock.Lock()
	defer br.portalsLock.Unlock()
	output := make([]*Portal, len(dbPortals))
	for index, dbPortal := range dbPortals {
		if dbPortal == nil {
			continue
		}
		portal, ok := br.portalsByGUID[dbPortal.GUID]
		if !ok {
			portal = br.loadDBPortal(dbPortal, "")
		}
		output[index] = portal
	}
	return output
}

func (br *IMBridge) loadDBPortal(dbPortal *database.Portal, guid string) *Portal {
	if dbPortal == nil {
		if guid == "" {
			return nil
		}
		dbPortal = br.DB.Portal.New()
		dbPortal.GUID = guid
		dbPortal.Insert()
	}
	portal := br.NewPortal(dbPortal)
	br.portalsByGUID[portal.GUID] = portal
	if len(portal.MXID) > 0 {
		br.portalsByMXID[portal.MXID] = portal
	}
	return portal
}

// newDummyPortal returns an initialized Portal with no event processing capabilities
func (br *IMBridge) newDummyPortal(dbPortal *database.Portal) *Portal {
	portal := &Portal{
		Portal: dbPortal,
		bridge: br,
		log:    br.Log.Sub(fmt.Sprintf("Portal/%s", dbPortal.GUID)),

		Identifier:      imessage.ParseIdentifier(dbPortal.GUID),
		Messages:        make(chan *imessage.Message, 0),
		ReadReceipts:    make(chan *imessage.ReadReceipt, 0),
		MessageStatuses: make(chan *imessage.SendMessageStatus, 0),
		MatrixMessages:  make(chan *event.Event, 0),
		backfillStart:   make(chan struct{}),
	}
	if !br.IM.Capabilities().MessageSendResponses {
		portal.messageDedup = make(map[string]SentMessage)
	}
	return portal
}

func (br *IMBridge) NewPortal(dbPortal *database.Portal) *Portal {
	portal := &Portal{
		Portal: dbPortal,
		bridge: br,
		log:    br.Log.Sub(fmt.Sprintf("Portal/%s", dbPortal.GUID)),

		Identifier:      imessage.ParseIdentifier(dbPortal.GUID),
		Messages:        make(chan *imessage.Message, 100),
		ReadReceipts:    make(chan *imessage.ReadReceipt, 100),
		MessageStatuses: make(chan *imessage.SendMessageStatus, 100),
		MatrixMessages:  make(chan *event.Event, 100),
		backfillStart:   make(chan struct{}),
	}
	if !br.IM.Capabilities().MessageSendResponses {
		portal.messageDedup = make(map[string]SentMessage)
	}
	go portal.handleMessageLoop()
	return portal
}

type SentMessage struct {
	EventID   id.EventID
	Timestamp time.Time
}

type Portal struct {
	*database.Portal

	bridge *IMBridge
	log    log.Logger

	Messages         chan *imessage.Message
	ReadReceipts     chan *imessage.ReadReceipt
	MessageStatuses  chan *imessage.SendMessageStatus
	MatrixMessages   chan *event.Event
	backfillStart    chan struct{}
	backfillWait     sync.WaitGroup
	backfillLock     sync.Mutex
	roomCreateLock   sync.Mutex
	messageDedup     map[string]SentMessage
	messageDedupLock sync.Mutex
	Identifier       imessage.Identifier

	userIsTyping bool
	typingLock   sync.Mutex
}

var _ bridge.Portal = (*Portal)(nil)

func (portal *Portal) IsEncrypted() bool {
	return portal.Encrypted
}

func (portal *Portal) MarkEncrypted() {
	portal.Encrypted = true
	portal.Update()
}

func (portal *Portal) ReceiveMatrixEvent(_ bridge.User, evt *event.Event) {
	portal.MatrixMessages <- evt
}

func (portal *Portal) SyncParticipants(chatInfo *imessage.ChatInfo) {
	for _, member := range chatInfo.Members {
		puppet := portal.bridge.GetPuppetByLocalID(member)
		puppet.Sync()
		err := puppet.Intent.EnsureJoined(portal.MXID)
		if err != nil {
			portal.log.Warnfln("Failed to make puppet of %s join %s: %v", member, portal.MXID, err)
		}
	}
}

func (portal *Portal) UpdateName(name string, intent *appservice.IntentAPI) *id.EventID {
	if portal.Name != name || intent != nil {
		if intent == nil {
			intent = portal.MainIntent()
		}
		resp, err := intent.SetRoomName(portal.MXID, name)
		if mainIntent := portal.MainIntent(); errors.Is(err, mautrix.MForbidden) && intent != mainIntent {
			resp, err = mainIntent.SetRoomName(portal.MXID, name)
		}
		if err != nil {
			portal.log.Warnln("Failed to set room name:", err)
		} else {
			portal.Name = name
			portal.UpdateBridgeInfo()
			return &resp.EventID
		}
	}
	return nil
}

// mergeIntoPortal creates a tombstone event redirecting the user to a different room.
// If the event is successfully created, the portal is deleted from the database and returns true. Otherwise, returns false.
func (portal *Portal) mergeIntoPortal(roomID id.RoomID, tombstoneMessage string) bool {
	portal.log.Infofln("Merging portal %s into %s: %s", portal.MXID, roomID, tombstoneMessage)
	_, err := portal.MainIntent().SendStateEvent(portal.MXID, event.StateTombstone, "", event.TombstoneEventContent{
		Body:            tombstoneMessage,
		ReplacementRoom: roomID,
	})
	if err != nil {
		portal.log.Errorfln("Error while tombstoning %s: %v", portal.MXID, err)
		return false
	}
	portal.Delete()
	if storedPortal := portal.bridge.portalsByGUID[portal.GUID]; storedPortal == portal && len(portal.GUID) != 0 {
		portal.bridge.portalsByGUID[portal.GUID] = nil
	}
	if storedPortal := portal.bridge.portalsByMXID[portal.MXID]; storedPortal == portal && len(portal.MXID) != 0 {
		portal.bridge.portalsByMXID[portal.MXID] = nil
	}
	return true
}

func (portal *Portal) SyncCorrelationID(chatInfo *imessage.ChatInfo) bool {
	if portal.Identifier.IsGroup {
		// groups do not get correlation IDs (yet?)
		return false
	}
	if len(chatInfo.CorrelationID) == 0 || chatInfo.CorrelationID == portal.CorrelationID {
		// no correlation ID, or no change
		return false
	}
	if existingPortal := portal.bridge.DB.Portal.GetByCorrelationID(chatInfo.CorrelationID); existingPortal != nil && existingPortal.GUID != portal.GUID {
		if len(existingPortal.MXID) == 0 {
			// existing is just a row, delete it
			existingPortal.Delete()
		} else {
			// well, they were here first, so let's delete ourselves
			portal.mergeIntoPortal(existingPortal.MXID, "This room has been deduplicated.")
			return false
		}
	}
	// store the correlation ID
	portal.CorrelationID = chatInfo.CorrelationID
	return true
}

func (portal *Portal) SyncWithInfo(chatInfo *imessage.ChatInfo) {
	update := false
	if len(chatInfo.DisplayName) > 0 {
		update = portal.UpdateName(chatInfo.DisplayName, nil) != nil || update
	}
	update = portal.SyncCorrelationID(chatInfo) || update
	portal.SyncParticipants(chatInfo)
	if update {
		portal.Update()
		portal.UpdateBridgeInfo()
	}
}

func (portal *Portal) ensureUserInvited(user *User) {
	user.ensureInvited(portal.MainIntent(), portal.MXID, portal.IsPrivateChat())
}

func (portal *Portal) Sync(backfill bool) {
	if len(portal.MXID) == 0 {
		portal.log.Infoln("Creating Matrix room due to sync")
		err := portal.CreateMatrixRoom(nil, nil)
		if err != nil {
			portal.log.Errorln("Failed to create portal room:", err)
		}
		return
	}

	portal.ensureUserInvited(portal.bridge.user)
	portal.addToSpace(portal.bridge.user)

	if !portal.IsPrivateChat() {
		chatInfo, err := portal.bridge.IM.GetChatInfo(portal.GUID)
		if err != nil {
			portal.log.Errorln("Failed to get chat info:", err)
		}
		if chatInfo != nil {
			portal.SyncWithInfo(chatInfo)
		} else {
			portal.log.Warnln("Didn't get any chat info")
		}

		avatar, err := portal.bridge.IM.GetGroupAvatar(portal.GUID)
		if err != nil {
			portal.log.Warnln("Failed to get avatar:", err)
		} else if avatar != nil {
			portal.UpdateAvatar(avatar, portal.MainIntent())
		}
	} else {
		if len(portal.CorrelationID) == 0 && portal.bridge.IM.Capabilities().Correlation {
			chatInfo, err := portal.bridge.IM.GetChatInfo(portal.GUID)
			if err != nil {
				portal.log.Errorln("Failed to get chat info:", err)
			} else {
				if portal.SyncCorrelationID(chatInfo) {
					portal.Update()
				}
			}
		}
		puppet := portal.bridge.GetPuppetByLocalID(portal.Identifier.LocalID)
		puppet.Sync()
	}

	if backfill {
		portal.log.Debugln("Locking backfill (sync)")
		portal.lockBackfill()
		portal.log.Debugln("Starting sync backfill")
		portal.backfill()
		portal.log.Debugln("Unlocking backfill (sync)")
		portal.unlockBackfill()
	}
}

type CustomReadReceipt struct {
	Timestamp          int64  `json:"ts,omitempty"`
	DoublePuppetSource string `json:"fi.mau.double_puppet_source,omitempty"`
}

type CustomReadMarkers struct {
	mautrix.ReqSetReadMarkers
	ReadExtra      CustomReadReceipt `json:"com.beeper.read.extra"`
	FullyReadExtra CustomReadReceipt `json:"com.beeper.fully_read.extra"`
}

func (portal *Portal) markRead(intent *appservice.IntentAPI, eventID id.EventID, readAt time.Time) error {
	if intent == nil {
		return nil
	}
	var extra CustomReadReceipt
	if intent == portal.bridge.user.DoublePuppetIntent {
		extra.DoublePuppetSource = portal.bridge.Name
	}
	if !readAt.IsZero() {
		extra.Timestamp = readAt.UnixMilli()
	}
	content := CustomReadMarkers{
		ReqSetReadMarkers: mautrix.ReqSetReadMarkers{
			Read:      eventID,
			FullyRead: eventID,
		},
		ReadExtra:      extra,
		FullyReadExtra: extra,
	}
	return intent.SetReadMarkers(portal.MXID, &content)
}

func (portal *Portal) HandleiMessageReadReceipt(rr *imessage.ReadReceipt) {
	if len(portal.MXID) == 0 {
		return
	}
	var intent *appservice.IntentAPI
	if rr.IsFromMe {
		intent = portal.bridge.user.DoublePuppetIntent
	} else if rr.SenderGUID == rr.ChatGUID {
		intent = portal.MainIntent()
	} else {
		portal.log.Debugfln("Dropping unexpected read receipt %+v", *rr)
		return
	}
	if intent == nil {
		return
	}

	if message := portal.bridge.DB.Message.GetLastByGUID(portal.GUID, rr.ReadUpTo); message != nil {
		err := portal.markRead(intent, message.MXID, rr.ReadAt)
		if err != nil {
			portal.log.Warnln("Failed to send read receipt for %s from %s: %v", message.MXID, intent.UserID)
		}
	} else if tapback := portal.bridge.DB.Tapback.GetByTapbackGUID(portal.GUID, rr.ReadUpTo); tapback != nil {
		err := portal.markRead(intent, tapback.MXID, rr.ReadAt)
		if err != nil {
			portal.log.Warnln("Failed to send read receipt for %s from %s: %v", tapback.MXID, intent.UserID)
		}
	} else {
		portal.log.Debugfln("Dropping read receipt for %s: not found in db messages or tapbacks", rr.ReadUpTo)
	}
}

func (portal *Portal) handleMessageLoop() {
	portal.log.Debugln("Starting message processing loop")
	for {
		select {
		case msg := <-portal.Messages:
			portal.HandleiMessage(msg, false)
		case readReceipt := <-portal.ReadReceipts:
			portal.HandleiMessageReadReceipt(readReceipt)
		case <-portal.backfillStart:
			portal.log.Debugln("Backfill lock enabled, stopping new message processing")
			portal.backfillWait.Wait()
			portal.log.Debugln("Continuing new message processing")
		case evt := <-portal.MatrixMessages:
			switch evt.Type {
			case event.EventMessage, event.EventSticker:
				portal.HandleMatrixMessage(evt)
			case event.EventRedaction:
				portal.HandleMatrixRedaction(evt)
			case event.EventReaction:
				portal.HandleMatrixReaction(evt)
			default:
				portal.log.Warnln("Unsupported event type %+v in portal message channel", evt.Type)
			}
		case status := <-portal.MessageStatuses:
			portal.HandleiMessageSendMessageStatus(status)
		}
	}
}

func (portal *Portal) HandleiMessageSendMessageStatus(status *imessage.SendMessageStatus) {
	var eventID id.EventID
	if msg := portal.bridge.DB.Message.GetLastByGUID(portal.GUID, status.GUID); msg != nil {
		eventID = msg.MXID
	} else if tapback := portal.bridge.DB.Tapback.GetByTapbackGUID(portal.GUID, status.GUID); tapback != nil {
		eventID = tapback.MXID
	} else {
		portal.log.Debugfln("Dropping send message status for %s: not found in db messages or tapbacks", status.GUID)
		return
	}
	portal.log.Debugfln("Processing message status with type %v for event %s/%s %s/%s", status.Status, string(eventID), portal.MXID, status.GUID, portal.GUID)
	if status.Status == "sent" {
		portal.sendSuccessCheckpoint(eventID, status.Service)
	} else if status.Status == "failed" {
		evt, err := portal.MainIntent().GetEvent(portal.MXID, eventID)
		if err != nil {
			portal.log.Warnfln("Failed to lookup event %s/%s %s/%s: %v", string(eventID), portal.MXID, status.GUID, status.ChatGUID, err)
			return
		}
		errString := "internal error"
		if len(status.Message) != 0 {
			errString = status.Message
		} else if len(status.StatusCode) != 0 {
			errString = status.StatusCode
		}
		portal.sendErrorMessage(evt, errors.New(errString), true, bridge.MsgStatusPermFailure)
	} else {
		portal.log.Infofln("Ignoring unused message status type %v for event %s/%s %s/%s", status.Status, string(eventID), portal.MXID, status.GUID, portal.GUID)
		return
	}
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

func (portal *Portal) backfill() {
	defer func() {
		if err := recover(); err != nil {
			portal.log.Errorln("Panic while backfilling: %v\n%s", err, string(debug.Stack()))
		}
	}()

	var messages []*imessage.Message
	var err error
	lastMessage := portal.bridge.DB.Message.GetLastInChat(portal.GUID)
	if lastMessage == nil && portal.BackfillStartTS == 0 {
		portal.log.Debugfln("Fetching up to %d messages for initial backfill", portal.bridge.Config.Bridge.InitialBackfillLimit)
		messages, err = portal.bridge.IM.GetMessagesWithLimit(portal.GUID, portal.bridge.Config.Bridge.InitialBackfillLimit)
	} else if lastMessage != nil {
		portal.log.Debugfln("Fetching messages since %s for catchup backfill", lastMessage.Time().String())
		messages, err = portal.bridge.IM.GetMessagesSinceDate(portal.GUID, lastMessage.Time())
	} else if portal.BackfillStartTS != 0 {
		startTime := time.Unix(0, portal.BackfillStartTS*int64(time.Millisecond))
		portal.log.Debugfln("Fetching messages since %s for catchup backfill after portal recovery", startTime.String())
		messages, err = portal.bridge.IM.GetMessagesSinceDate(portal.GUID, startTime)
	}
	if err != nil {
		portal.log.Errorln("Failed to fetch messages for backfilling:", err)
	} else if len(messages) == 0 {
		portal.log.Debugln("Nothing to backfill")
	} else {
		portal.log.Infofln("Backfilling %d messages", len(messages))
		var lastReadEvent id.EventID
		for _, message := range messages {
			mxid := portal.HandleiMessage(message, true)
			if message.IsRead || message.IsFromMe {
				lastReadEvent = mxid
			}
		}
		portal.log.Infoln("Backfill finished")
		if len(lastReadEvent) > 0 {
			err = portal.markRead(portal.bridge.user.DoublePuppetIntent, lastReadEvent, time.Time{})
			if err != nil {
				portal.log.Warnfln("Failed to mark %s as read with double puppet: %v", lastReadEvent, err)
			}
		}
	}
}

func (portal *Portal) disableNotifications() {
	if !portal.bridge.Config.Bridge.BackfillDisableNotifs || portal.bridge.user.DoublePuppetIntent == nil {
		return
	}
	portal.log.Debugfln("Disabling notifications for %s for backfilling", portal.bridge.user.MXID)
	ruleID := fmt.Sprintf("net.maunium.silence_while_backfilling.%s", portal.MXID)
	err := portal.bridge.user.DoublePuppetIntent.PutPushRule("global", pushrules.OverrideRule, ruleID, &mautrix.ReqPutPushRule{
		Actions: []pushrules.PushActionType{pushrules.ActionDontNotify},
		Conditions: []pushrules.PushCondition{{
			Kind:    pushrules.KindEventMatch,
			Key:     "room_id",
			Pattern: string(portal.MXID),
		}},
	})
	if err != nil {
		portal.log.Warnfln("Failed to disable notifications for %s while backfilling: %v", portal.bridge.user.MXID, err)
	}
}

func (portal *Portal) enableNotifications() {
	if !portal.bridge.Config.Bridge.BackfillDisableNotifs || portal.bridge.user.DoublePuppetIntent == nil {
		return
	}
	portal.log.Debugfln("Re-enabling notifications for %s after backfilling", portal.bridge.user.MXID)
	ruleID := fmt.Sprintf("net.maunium.silence_while_backfilling.%s", portal.MXID)
	err := portal.bridge.user.DoublePuppetIntent.DeletePushRule("global", pushrules.OverrideRule, ruleID)
	if err != nil {
		portal.log.Warnfln("Failed to re-enable notifications for %s after backfilling: %v", portal.bridge.user.MXID, err)
	}
}

func (portal *Portal) GetBasePowerLevels() *event.PowerLevelsEventContent {
	anyone := 0
	nope := 99
	invite := 50
	return &event.PowerLevelsEventContent{
		UsersDefault:    anyone,
		EventsDefault:   anyone,
		RedactPtr:       &anyone,
		StateDefaultPtr: &nope,
		BanPtr:          &nope,
		InvitePtr:       &invite,
		Users: map[id.UserID]int{
			portal.MainIntent().UserID: 100,
		},
		Events: map[string]int{
			event.StateRoomName.Type:   anyone,
			event.StateRoomAvatar.Type: anyone,
			event.StateTopic.Type:      anyone,
		},
	}
}

func (portal *Portal) getBridgeInfoStateKey() string {
	key := fmt.Sprintf("%s://%s/%s",
		bridgeInfoProto, strings.ToLower(portal.Identifier.Service), portal.GUID)
	if len(key) > 255 {
		key = fmt.Sprintf("%s://%s/%s", bridgeInfoProto, strings.ToLower(portal.Identifier.Service), sha256.Sum256([]byte(portal.GUID)))
	}
	return key
}

func (portal *Portal) getBridgeInfo() (string, CustomBridgeInfoContent) {
	bridgeInfo := CustomBridgeInfoContent{
		BridgeEventContent: event.BridgeEventContent{
			BridgeBot: portal.bridge.Bot.UserID,
			Creator:   portal.MainIntent().UserID,
			Protocol: event.BridgeInfoSection{
				ID:          "imessage",
				DisplayName: "iMessage",
				AvatarURL:   id.ContentURIString(portal.bridge.Config.AppService.Bot.Avatar),
				ExternalURL: "https://support.apple.com/messages",
			},
		},
		Channel: CustomBridgeInfoSection{
			BridgeInfoSection: event.BridgeInfoSection{
				ID:          portal.Identifier.LocalID,
				DisplayName: portal.Name,
				AvatarURL:   portal.AvatarURL.CUString(),
			},

			GUID:    portal.GUID,
			IsGroup: portal.Identifier.IsGroup,
			Service: portal.Identifier.Service,

			SendStatusStart: portal.bridge.SendStatusStartTS,
			TimeoutSeconds:  portal.bridge.Config.Bridge.MaxHandleSeconds,
		},
	}
	if portal.Identifier.Service == "SMS" {
		if portal.bridge.Config.IMessage.Platform == "android" {
			bridgeInfo.Protocol.ID = "android-sms"
			bridgeInfo.Protocol.DisplayName = "Android SMS"
			bridgeInfo.Protocol.ExternalURL = ""
		} else {
			bridgeInfo.Protocol.ID = "imessage-sms"
			bridgeInfo.Protocol.DisplayName = "iMessage (SMS)"
		}
	} else if portal.bridge.Config.IMessage.Platform == "ios" {
		bridgeInfo.Protocol.ID = "imessage-ios"
	} else if portal.bridge.Config.IMessage.Platform == "mac-nosip" {
		bridgeInfo.Protocol.ID = "imessage-nosip"
	}
	return portal.getBridgeInfoStateKey(), bridgeInfo
}

func (portal *Portal) UpdateBridgeInfo() {
	if len(portal.MXID) == 0 {
		portal.log.Debugln("Not updating bridge info: no Matrix room created")
		return
	}
	portal.log.Debugln("Updating bridge info...")
	stateKey, content := portal.getBridgeInfo()
	_, err := portal.MainIntent().SendStateEvent(portal.MXID, event.StateBridge, stateKey, content)
	if err != nil {
		portal.log.Warnln("Failed to update m.bridge:", err)
	}
	_, err = portal.MainIntent().SendStateEvent(portal.MXID, event.StateHalfShotBridge, stateKey, content)
	if err != nil {
		portal.log.Warnln("Failed to update uk.half-shot.bridge:", err)
	}
}

func (portal *Portal) GetEncryptionEventContent() (evt *event.EncryptionEventContent) {
	evt = &event.EncryptionEventContent{Algorithm: id.AlgorithmMegolmV1}
	if rot := portal.bridge.Config.Bridge.Encryption.Rotation; rot.EnableCustom {
		evt.RotationPeriodMillis = rot.Milliseconds
		evt.RotationPeriodMessages = rot.Messages
	}
	return
}

func (portal *Portal) CreateMatrixRoom(chatInfo *imessage.ChatInfo, profileOverride *ProfileOverride) error {
	portal.roomCreateLock.Lock()
	defer portal.roomCreateLock.Unlock()
	if len(portal.MXID) > 0 {
		return nil
	}

	intent := portal.MainIntent()
	err := intent.EnsureRegistered()
	if err != nil {
		return err
	}

	if chatInfo == nil {
		portal.log.Debugln("Getting chat info to create Matrix room")
		chatInfo, err = portal.bridge.IM.GetChatInfo(portal.GUID)
		if err != nil && !portal.IsPrivateChat() {
			// If there's no chat info for a group, it probably doesn't exist, and we shouldn't auto-create a Matrix room for it.
			return fmt.Errorf("failed to get chat info: %w", err)
		}
	}
	if chatInfo != nil {
		portal.Name = chatInfo.DisplayName
		portal.CorrelationID = chatInfo.CorrelationID
	} else {
		portal.log.Warnln("Didn't get any chat info")
	}

	if portal.IsPrivateChat() {
		puppet := portal.bridge.GetPuppetByLocalID(portal.Identifier.LocalID)
		puppet.Sync()
		if profileOverride != nil {
			puppet.SyncWithProfileOverride(*profileOverride)
		}
		portal.Name = puppet.Displayname
		portal.AvatarURL = puppet.AvatarURL
		portal.AvatarHash = puppet.AvatarHash
	} else {
		avatar, err := portal.bridge.IM.GetGroupAvatar(portal.GUID)
		if err != nil {
			portal.log.Warnln("Failed to get avatar:", err)
		} else if avatar != nil {
			portal.UpdateAvatar(avatar, portal.MainIntent())
		}
	}

	bridgeInfoStateKey, bridgeInfo := portal.getBridgeInfo()

	initialState := []*event.Event{{
		Type: event.StatePowerLevels,
		Content: event.Content{
			Parsed: portal.GetBasePowerLevels(),
		},
	}, {
		Type:     event.StateBridge,
		Content:  event.Content{Parsed: bridgeInfo},
		StateKey: &bridgeInfoStateKey,
	}, {
		// TODO remove this once https://github.com/matrix-org/matrix-doc/pull/2346 is in spec
		Type:     event.StateHalfShotBridge,
		Content:  event.Content{Parsed: bridgeInfo},
		StateKey: &bridgeInfoStateKey,
	}}
	if !portal.AvatarURL.IsEmpty() {
		initialState = append(initialState, &event.Event{
			Type: event.StateRoomAvatar,
			Content: event.Content{
				Parsed: event.RoomAvatarEventContent{URL: portal.AvatarURL},
			},
		})
	}

	var invite []id.UserID

	if portal.bridge.Config.Bridge.Encryption.Default {
		initialState = append(initialState, &event.Event{
			Type: event.StateEncryption,
			Content: event.Content{
				Parsed: portal.GetEncryptionEventContent(),
			},
		})
		portal.Encrypted = true
	}
	if portal.IsPrivateChat() {
		invite = append(invite, portal.bridge.Bot.UserID)
	}

	creationContent := make(map[string]interface{})
	if !portal.bridge.Config.Bridge.FederateRooms {
		creationContent["m.federate"] = false
	}
	resp, err := intent.CreateRoom(&mautrix.ReqCreateRoom{
		Visibility:      "private",
		Name:            portal.Name,
		Invite:          invite,
		Preset:          "private_chat",
		IsDirect:        portal.IsPrivateChat(),
		InitialState:    initialState,
		CreationContent: creationContent,
	})
	if err != nil {
		return err
	}
	portal.log.Debugln("Locking backfill (create)")
	portal.lockBackfill()
	portal.MXID = resp.RoomID
	portal.log.Debugln("Storing created room ID", portal.MXID, "in database")
	portal.Update()
	portal.bridge.portalsLock.Lock()
	portal.bridge.portalsByMXID[portal.MXID] = portal
	portal.bridge.portalsLock.Unlock()

	portal.log.Debugln("Updating state store with initial memberships")
	for _, user := range invite {
		portal.bridge.StateStore.SetMembership(portal.MXID, user, event.MembershipInvite)
	}

	if portal.Encrypted {
		portal.log.Debugln("Ensuring bridge bot is joined to portal")
		err = portal.bridge.Bot.EnsureJoined(portal.MXID)
		if err != nil {
			portal.log.Errorln("Failed to join created portal with bridge bot for e2be:", err)
		}
	}

	portal.ensureUserInvited(portal.bridge.user)
	portal.addToSpace(portal.bridge.user)

	if !portal.IsPrivateChat() {
		portal.log.Debugln("New portal is group chat, syncing participants")
		portal.SyncParticipants(chatInfo)
	} else {
		puppet := portal.bridge.GetPuppetByLocalID(portal.Identifier.LocalID)
		portal.bridge.user.UpdateDirectChats(map[id.UserID][]id.RoomID{puppet.MXID: {portal.MXID}})
	}
	if portal.bridge.user.DoublePuppetIntent != nil {
		portal.log.Debugln("Ensuring double puppet for", portal.bridge.user.MXID, "is joined to portal")
		_ = portal.bridge.user.DoublePuppetIntent.EnsureJoined(portal.MXID)
	}
	go func() {
		portal.disableNotifications()
		portal.log.Debugln("Starting initial backfill")
		portal.backfill()
		portal.enableNotifications()
		portal.log.Debugln("Unlocking backfill (create)")
		portal.unlockBackfill()
	}()
	portal.log.Debugln("Finished creating Matrix room")

	if portal.bridge.IM.Capabilities().ChatBridgeResult {
		portal.bridge.IPC.Send(ios.ReqChatBridgeResult, ios.ChatBridgeResult{
			ChatGUID: portal.GUID,
			MXID:     portal.MXID,
		})
	}

	return nil
}

func (portal *Portal) addToSpace(user *User) {
	spaceID := user.GetSpaceRoom()
	if len(spaceID) == 0 || portal.InSpace {
		return
	}
	_, err := portal.bridge.Bot.SendStateEvent(spaceID, event.StateSpaceChild, portal.MXID.String(), &event.SpaceChildEventContent{
		Via: []string{portal.bridge.Config.Homeserver.Domain},
	})
	if err != nil {
		portal.log.Errorfln("Failed to add room to %s's personal filtering space (%s): %v", user.MXID, spaceID, err)
	} else {
		portal.log.Debugfln("Added room to %s's personal filtering space (%s)", user.MXID, spaceID)
		portal.InSpace = true
		portal.Update()
	}
}

func (portal *Portal) IsPrivateChat() bool {
	return !portal.Identifier.IsGroup
}

func (portal *Portal) GetDMPuppet() *Puppet {
	if portal.IsPrivateChat() {
		return portal.bridge.GetPuppetByLocalID(portal.Identifier.LocalID)
	}
	return nil
}

func (portal *Portal) MainIntent() *appservice.IntentAPI {
	if portal.IsPrivateChat() {
		return portal.GetDMPuppet().Intent
	}
	return portal.bridge.Bot
}

func (portal *Portal) SetReply(content *event.MessageEventContent, msg *imessage.Message) {
	if len(msg.ReplyToGUID) == 0 {
		return
	}
	message := portal.bridge.DB.Message.GetByGUID(portal.GUID, msg.ReplyToGUID, msg.ReplyToPart)
	if message != nil {
		evt, err := portal.MainIntent().GetEvent(portal.MXID, message.MXID)
		if err != nil {
			portal.log.Warnln("Failed to get reply target:", err)
			return
		}
		if evt.Type == event.EventEncrypted {
			_ = evt.Content.ParseRaw(evt.Type)
			decryptedEvt, err := portal.bridge.Crypto.Decrypt(evt)
			if err != nil {
				portal.log.Warnln("Failed to decrypt reply target:", err)
			} else {
				evt = decryptedEvt
			}
		}
		_ = evt.Content.ParseRaw(evt.Type)
		content.SetReply(evt)
	} else {
		portal.log.Debugfln("Unknown reply target %s.%d", msg.ReplyToGUID, msg.ReplyToPart)
	}
	return
}

func (portal *Portal) sendMainIntentMessage(content interface{}) (*mautrix.RespSendEvent, error) {
	return portal.sendMessage(portal.MainIntent(), event.EventMessage, content, map[string]interface{}{}, 0)
}

func (portal *Portal) sendMessage(intent *appservice.IntentAPI, eventType event.Type, content interface{}, extraContent map[string]interface{}, timestamp int64) (*mautrix.RespSendEvent, error) {
	wrappedContent := &event.Content{Parsed: content}
	wrappedContent.Raw = extraContent
	intent.AddDoublePuppetValue(wrappedContent)
	if portal.Encrypted && portal.bridge.Crypto != nil {
		err := portal.bridge.Crypto.Encrypt(portal.MXID, eventType, wrappedContent)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt event: %w", err)
		}
		eventType = event.EventEncrypted
	}

	_, _ = intent.UserTyping(portal.MXID, false, 0)
	if timestamp == 0 {
		return intent.SendMessageEvent(portal.MXID, eventType, &wrappedContent)
	} else {
		return intent.SendMassagedMessageEvent(portal.MXID, eventType, &wrappedContent, timestamp)
	}
}

func (portal *Portal) encryptFile(data []byte, mimeType string) (string, *event.EncryptedFileInfo) {
	if !portal.Encrypted {
		return mimeType, nil
	}

	file := &event.EncryptedFileInfo{
		EncryptedFile: *attachment.NewEncryptedFile(),
		URL:           "",
	}
	file.EncryptInPlace(data)
	return "application/octet-stream", file
}

func (portal *Portal) sendErrorMessage(evt *event.Event, rootErr error, isCertain bool, status bridge.MessageCheckpointStatus) {
	portal.bridge.SendMessageCheckpoint(evt, bridge.MsgStepRemote, rootErr, status, 0)

	possibility := "may not have been"
	if isCertain {
		possibility = "was not"
	}

	errorIntent := portal.bridge.Bot
	if !portal.Encrypted {
		// Bridge bot isn't present in unencrypted DMs
		errorIntent = portal.MainIntent()
	}

	if portal.bridge.Config.Bridge.MessageStatusEvents {
		reason := event.MessageStatusGenericError
		msgStatus := event.MessageStatusRetriable
		switch status {
		case bridge.MsgStatusUnsupported:
			reason = event.MessageStatusUnsupported
			msgStatus = event.MessageStatusFail
		case bridge.MsgStatusTimeout:
			reason = event.MessageStatusTooOld
		}

		content := event.BeeperMessageStatusEventContent{
			Network: portal.getBridgeInfoStateKey(),
			RelatesTo: event.RelatesTo{
				Type:    event.RelReference,
				EventID: evt.ID,
			},
			Reason: reason,
			Status: msgStatus,
			Error:  rootErr.Error(),
		}
		content.FillLegacyBooleans()

		_, err := errorIntent.SendMessageEvent(portal.MXID, event.BeeperMessageStatus, &content)
		if err != nil {
			portal.log.Warnfln("Failed to send message send status event:", err)
			return
		}
	}
	if portal.bridge.Config.Bridge.SendErrorNotices {
		_, err := portal.sendMessage(errorIntent, event.EventMessage, event.MessageEventContent{
			MsgType: event.MsgNotice,
			Body:    fmt.Sprintf("\u26a0 Your message %s bridged: %v", possibility, rootErr),
		}, map[string]interface{}{}, 0)
		if err != nil {
			portal.log.Warnfln("Failed to send bridging error message:", err)
			return
		}
	}
}

func (portal *Portal) sendDeliveryReceipt(eventID id.EventID, service string, sendCheckpoint bool) {
	if portal.bridge.Config.Bridge.DeliveryReceipts {
		err := portal.bridge.Bot.MarkRead(portal.MXID, eventID)
		if err != nil {
			portal.log.Debugfln("Failed to send delivery receipt for %s: %v", eventID, err)
		}
	}

	if sendCheckpoint {
		portal.sendSuccessCheckpoint(eventID, service)
	}
}

func (portal *Portal) sendSuccessCheckpoint(eventID id.EventID, service string) {
	// We don't have access to the entire event, so we are omitting some
	// metadata here. However, that metadata can be inferred from previous
	// checkpoints.
	checkpoint := bridge.MessageCheckpoint{
		EventID:    eventID,
		RoomID:     portal.MXID,
		Step:       bridge.MsgStepRemote,
		Timestamp:  time.Now().UnixNano() / int64(time.Millisecond),
		Status:     bridge.MsgStatusSuccess,
		ReportedBy: bridge.MsgReportedByBridge,
	}
	go checkpoint.Send(&portal.bridge.Bridge)

	if portal.bridge.Config.Bridge.MessageStatusEvents {
		mainContent := &event.BeeperMessageStatusEventContent{
			Network: portal.getBridgeInfoStateKey(),
			RelatesTo: event.RelatesTo{
				Type:    event.RelReference,
				EventID: eventID,
			},
			Status: event.MessageStatusSuccess,
		}
		mainContent.FillLegacyBooleans()
		content := &event.Content{
			Parsed: mainContent,
			Raw: map[string]interface{}{
				bridgeInfoService: service,
			},
		}

		statusIntent := portal.bridge.Bot
		if !portal.Encrypted {
			statusIntent = portal.MainIntent()
		}
		_, err := statusIntent.SendMessageEvent(portal.MXID, event.BeeperMessageStatus, content)
		if err != nil {
			portal.log.Warnfln("Failed to send message send status event:", err)
		}
	}
}

func (portal *Portal) addDedup(eventID id.EventID, body string) {
	if portal.messageDedup != nil {
		portal.messageDedupLock.Lock()
		portal.messageDedup[strings.TrimSpace(body)] = SentMessage{
			EventID: eventID,
			// Set the timestamp to a bit before now to make sure the deduplication catches it properly
			Timestamp: time.Now().Add(-10 * time.Second),
		}
		portal.messageDedupLock.Unlock()
	}
}

func (portal *Portal) shouldHandleMessage(evt *event.Event) error {
	if portal.bridge.Config.Bridge.MaxHandleSeconds == 0 {
		return nil
	}
	if time.Since(time.UnixMilli(evt.Timestamp)) < time.Duration(portal.bridge.Config.Bridge.MaxHandleSeconds)*time.Second {
		return nil
	}

	return errors.New(fmt.Sprintf("It's been over %d seconds since the message arrived at the homeserver. Will not handle the event.", portal.bridge.Config.Bridge.MaxHandleSeconds))
}

func (portal *Portal) addRelaybotFormat(sender id.UserID, content *event.MessageEventContent) bool {
	member := portal.MainIntent().Member(portal.MXID, sender)
	if member == nil {
		member = &event.MemberEventContent{}
	}

	data, err := portal.bridge.Config.Bridge.Relay.FormatMessage(content, sender, *member)
	if err != nil {
		portal.log.Errorln("Failed to apply relaybot format:", err)
	}
	content.Body = data
	return true
}

func (portal *Portal) HandleMatrixMessage(evt *event.Event) {
	msg, ok := evt.Content.Parsed.(*event.MessageEventContent)
	if !ok {
		// TODO log
		return
	}
	portal.log.Debugln("Starting handling Matrix message", evt.ID)

	var messageReplyID string
	var messageReplyPart int
	replyToID := msg.GetReplyTo()
	if len(replyToID) > 0 {
		imsg := portal.bridge.DB.Message.GetByMXID(replyToID)
		if imsg != nil {
			messageReplyID = imsg.GUID
			messageReplyPart = imsg.Part
		}
	}

	if err := portal.shouldHandleMessage(evt); err != nil {
		portal.log.Debug(err)
		portal.sendErrorMessage(evt, err, true, bridge.MsgStatusTimeout)
		return
	}

	var imessageRichLink *imessage.RichLink
	if portal.bridge.IM.Capabilities().RichLinks {
		imessageRichLink = portal.convertURLPreviewToIMessage(evt)
	}
	metadata, _ := evt.Content.Raw["com.beeper.message_metadata"].(imessage.MessageMetadata)

	var err error
	var resp *imessage.SendResponse
	if msg.MsgType == event.MsgText || msg.MsgType == event.MsgNotice || msg.MsgType == event.MsgEmote {
		if evt.Sender != portal.bridge.user.MXID {
			portal.addRelaybotFormat(evt.Sender, msg)
			if len(msg.Body) == 0 {
				return
			}
		} else if msg.MsgType == event.MsgEmote {
			msg.Body = "/me " + msg.Body
		}
		portal.addDedup(evt.ID, msg.Body)
		resp, err = portal.bridge.IM.SendMessage(portal.GUID, msg.Body, messageReplyID, messageReplyPart, imessageRichLink, metadata)
	} else if len(msg.URL) > 0 || msg.File != nil {
		resp, err = portal.handleMatrixMedia(msg, evt, messageReplyID, messageReplyPart, metadata)
	}
	if err != nil {
		portal.log.Errorln("Error sending to iMessage:", err)
		status := bridge.MsgStatusPermFailure
		certain := false
		if errors.Is(err, ipc.ErrSizeLimitExceeded) {
			certain = true
			status = bridge.MsgStatusUnsupported
		}
		var ipcErr ipc.Error
		if errors.As(err, &ipcErr) {
			certain = true
			err = errors.New(ipcErr.Message)
			switch ipcErr.Code {
			case ipc.ErrUnsupportedError.Code:
				status = bridge.MsgStatusUnsupported
			case ipc.ErrTimeoutError.Code:
				status = bridge.MsgStatusTimeout
			}
		}
		portal.sendErrorMessage(evt, err, certain, status)
	} else if resp != nil {
		dbMessage := portal.bridge.DB.Message.New()
		dbMessage.ChatGUID = portal.GUID
		dbMessage.GUID = resp.GUID
		dbMessage.MXID = evt.ID
		dbMessage.Timestamp = resp.Time.UnixNano() / 1e6
		portal.sendDeliveryReceipt(evt.ID, resp.Service, !portal.bridge.IM.Capabilities().MessageStatusCheckpoints)
		dbMessage.Insert()
		portal.log.Debugln("Handled Matrix message", evt.ID, "->", resp.GUID)
	} else {
		portal.log.Debugln("Handled Matrix message", evt.ID, "(waiting for echo)")
	}
}

func (portal *Portal) handleMatrixMedia(msg *event.MessageEventContent, evt *event.Event, messageReplyID string, messageReplyPart int, metadata imessage.MessageMetadata) (*imessage.SendResponse, error) {
	var url id.ContentURI
	var file *event.EncryptedFileInfo
	var err error
	if msg.File != nil {
		file = msg.File
		url, err = msg.File.URL.Parse()
	} else {
		url, err = msg.URL.Parse()
	}
	if err != nil {
		portal.sendErrorMessage(evt, fmt.Errorf("malformed attachment URL: %w", err), true, bridge.MsgStatusPermFailure)
		portal.log.Warnfln("Malformed content URI in %s: %v", evt.ID, err)
		return nil, nil
	}
	var caption string
	filename := msg.Body
	portal.addDedup(evt.ID, filename)
	if evt.Sender != portal.bridge.user.MXID {
		portal.addRelaybotFormat(evt.Sender, msg)
		caption = msg.Body
	}

	mediaViewerMinSize := portal.bridge.Config.Bridge.MediaViewer.IMMinSize
	if portal.Identifier.Service == "SMS" {
		mediaViewerMinSize = portal.bridge.Config.Bridge.MediaViewer.SMSMinSize
	}
	if len(portal.bridge.Config.Bridge.MediaViewer.URL) > 0 && mediaViewerMinSize > 0 && msg.Info != nil && msg.Info.Size >= mediaViewerMinSize {
		// SMS chat and the file is too big, make a media viewer URL
		var mediaURL string
		mediaURL, err = portal.bridge.createMediaViewerURL(&evt.Content)
		if err != nil {
			return nil, fmt.Errorf("failed to create media viewer URL: %w", err)
		}
		if len(caption) > 0 {
			caption += ": "
		}
		caption += fmt.Sprintf(portal.bridge.Config.Bridge.MediaViewer.Template, mediaURL)

		// Check if there's a thumbnail we can bridge.
		// If not, just send the link. If yes, send the thumbnail and the link as a caption.
		// TODO: we could try to compress images to fit even if the provided thumbnail is too big.
		var hasUsableThumbnail bool
		if msg.Info.ThumbnailInfo != nil && msg.Info.ThumbnailInfo.Size < mediaViewerMinSize {
			file = msg.Info.ThumbnailFile
			if file != nil {
				url, err = file.URL.Parse()
			} else {
				url, err = msg.Info.ThumbnailURL.Parse()
			}
			hasUsableThumbnail = err == nil && !url.IsEmpty() && portal.bridge.IM.Capabilities().SendCaptions
		}
		if !hasUsableThumbnail {
			portal.addDedup(evt.ID, caption)
			return portal.bridge.IM.SendMessage(portal.GUID, caption, messageReplyID, messageReplyPart, nil, metadata)
		}
	}

	return portal.handleMatrixMediaDirect(url, file, filename, caption, evt, messageReplyID, messageReplyPart, metadata)
}

func (portal *Portal) handleMatrixMediaDirect(url id.ContentURI, file *event.EncryptedFileInfo, filename, caption string, evt *event.Event, messageReplyID string, messageReplyPart int, metadata imessage.MessageMetadata) (resp *imessage.SendResponse, err error) {
	var data []byte
	data, err = portal.MainIntent().DownloadBytes(url)
	if err != nil {
		portal.sendErrorMessage(evt, fmt.Errorf("failed to download attachment: %w", err), true, bridge.MsgStatusPermFailure)
		portal.log.Errorfln("Failed to download media in %s: %v", evt.ID, err)
		return
	}
	if file != nil {
		data, err = file.Decrypt(data)
		if err != nil {
			portal.sendErrorMessage(evt, fmt.Errorf("failed to decrypt attachment: %w", err), true, bridge.MsgStatusPermFailure)
			portal.log.Errorfln("Failed to decrypt media in %s: %v", evt.ID, err)
			return
		}
	}

	var dir, filePath string
	dir, filePath, err = imessage.SendFilePrepare(filename, data)
	if err != nil {
		portal.log.Errorfln("failed to prepare to send file: %w", err)
		return
	}
	mimeType := mimetype.Detect(data).String()
	isVoiceMemo := false
	_, isMSC3245Voice := evt.Content.Raw["org.matrix.msc3245.voice"]
	// Only convert when sending to iMessage. SMS users probably don't want CAF.
	if portal.Identifier.Service == "iMessage" && isMSC3245Voice && strings.HasPrefix(mimeType, "audio/") {
		filePath, err = ffmpeg.ConvertPath(context.TODO(), filePath, ".caf", []string{}, []string{}, false)
		mimeType = "audio/x-caf"
		isVoiceMemo = true
		if err != nil {
			log.Errorfln("Failed to transcode voice message to CAF. Error: %w", err)
			return
		}
	}

	resp, err = portal.bridge.IM.SendFile(portal.GUID, caption, filename, filePath, messageReplyID, messageReplyPart, mimeType, isVoiceMemo, metadata)
	portal.bridge.IM.SendFileCleanup(dir)
	return
}

func (portal *Portal) sendUnsupportedCheckpoint(evt *event.Event, step bridge.MessageCheckpointStep, err error) {
	portal.log.Errorf("Sending unsupported checkpoint for %s: %+v", evt.ID, err)
	portal.bridge.SendMessageCheckpoint(evt, step, err, bridge.MsgStatusUnsupported, 0)

	if portal.bridge.Config.Bridge.MessageStatusEvents {
		content := event.BeeperMessageStatusEventContent{
			Network: portal.getBridgeInfoStateKey(),
			RelatesTo: event.RelatesTo{
				Type:    event.RelReference,
				EventID: evt.ID,
			},
			Status: event.MessageStatusFail,
			Reason: event.MessageStatusUnsupported,
			Error:  err.Error(),
		}
		content.FillLegacyBooleans()

		errorIntent := portal.bridge.Bot
		if !portal.Encrypted {
			errorIntent = portal.MainIntent()
		}
		_, sendErr := errorIntent.SendMessageEvent(portal.MXID, event.BeeperMessageStatus, &content)
		if sendErr != nil {
			portal.log.Warnln("Failed to send message send status event:", sendErr)
		}
	}
}

var _ bridge.ReadReceiptHandlingPortal = (*Portal)(nil)
var _ bridge.TypingPortal = (*Portal)(nil)

func (portal *Portal) HandleMatrixReadReceipt(user bridge.User, eventID id.EventID, ts time.Time) {
	if user.GetMXID() != portal.bridge.user.MXID {
		return
	}

	if message := portal.bridge.DB.Message.GetByMXID(eventID); message != nil {
		portal.log.Debugfln("Marking %s/%s as read", message.GUID, message.MXID)
		err := portal.bridge.IM.SendReadReceipt(portal.GUID, message.GUID)
		if err != nil {
			portal.log.Warnln("Error marking message as read:", err)
		}
	} else if tapback := portal.bridge.DB.Tapback.GetByMXID(eventID); tapback != nil {
		portal.log.Debugfln("Marking %s/%s as read", tapback.GUID, tapback.MXID)
		err := portal.bridge.IM.SendReadReceipt(portal.GUID, tapback.GUID)
		if err != nil {
			portal.log.Warnln("Error marking tapback as read:", err)
		}
	}
}

func (portal *Portal) HandleMatrixTyping(userIDs []id.UserID) {
	portal.typingLock.Lock()
	defer portal.typingLock.Unlock()

	isTyping := false
	for _, userID := range userIDs {
		if userID == portal.bridge.user.MXID {
			isTyping = true
			break
		}
	}
	if isTyping != portal.userIsTyping {
		portal.userIsTyping = isTyping
		if !isTyping {
			portal.log.Debugfln("Sending typing stop notification")
		} else {
			portal.log.Debugfln("Sending typing start notification")
		}
		err := portal.bridge.IM.SendTypingNotification(portal.GUID, isTyping)
		if err != nil {
			portal.log.Warnfln("Failed to bridge typing status change: %v", err)
		}
	}
}

func (portal *Portal) HandleMatrixReaction(evt *event.Event) {
	if !portal.bridge.IM.Capabilities().SendTapbacks {
		portal.sendUnsupportedCheckpoint(evt, bridge.MsgStepRemote, errors.New("reactions are not supported"))
		return
	}
	portal.log.Debugln("Starting handling of Matrix reaction", evt.ID)

	if err := portal.shouldHandleMessage(evt); err != nil {
		portal.log.Debug(err)
		portal.sendErrorMessage(evt, err, true, bridge.MsgStatusTimeout)
		return
	}

	var errorMsg string

	if reaction, ok := evt.Content.Parsed.(*event.ReactionEventContent); !ok || reaction.RelatesTo.Type != event.RelAnnotation {
		errorMsg = fmt.Sprintf("Ignoring reaction %s due to unknown m.relates_to data", evt.ID)
	} else if tapbackType := imessage.TapbackFromEmoji(reaction.RelatesTo.Key); tapbackType == 0 {
		errorMsg = fmt.Sprintf("Unknown reaction type %s in %s", reaction.RelatesTo.Key, reaction.RelatesTo.EventID)
	} else if target := portal.bridge.DB.Message.GetByMXID(reaction.RelatesTo.EventID); target == nil {
		errorMsg = fmt.Sprintf("Unknown reaction target %s", reaction.RelatesTo.EventID)
	} else if existing := portal.bridge.DB.Tapback.GetByGUID(portal.GUID, target.GUID, target.Part, ""); existing != nil && existing.Type == tapbackType {
		errorMsg = fmt.Sprintf("Ignoring outgoing tapback to %s/%s: type is same", reaction.RelatesTo.EventID, target.GUID)
	} else if resp, err := portal.bridge.IM.SendTapback(portal.GUID, target.GUID, target.Part, tapbackType, false); err != nil {
		errorMsg = fmt.Sprintf("Failed to send tapback %d to %s: %v", tapbackType, target.GUID, err)
	} else if existing == nil {
		// TODO should timestamp be stored?
		portal.log.Debugfln("Handled Matrix reaction %s into new iMessage tapback %s", evt.ID, resp.GUID)
		if !portal.bridge.IM.Capabilities().MessageStatusCheckpoints {
			portal.bridge.SendMessageSuccessCheckpoint(evt, bridge.MsgStepRemote, 0)
		}
		tapback := portal.bridge.DB.Tapback.New()
		tapback.ChatGUID = portal.GUID
		tapback.GUID = resp.GUID
		tapback.MessageGUID = target.GUID
		tapback.MessagePart = target.Part
		tapback.Type = tapbackType
		tapback.MXID = evt.ID
		tapback.Insert()
	} else {
		portal.log.Debugfln("Handled Matrix reaction %s into iMessage tapback %s, replacing old %s", evt.ID, resp.GUID, existing.MXID)
		if !portal.bridge.IM.Capabilities().MessageStatusCheckpoints {
			portal.bridge.SendMessageSuccessCheckpoint(evt, bridge.MsgStepRemote, 0)
		}
		_, err = portal.MainIntent().RedactEvent(portal.MXID, existing.MXID)
		if err != nil {
			portal.log.Warnfln("Failed to redact old tapback %s to %s: %v", existing.MXID, target.MXID, err)
		}
		existing.GUID = resp.GUID
		existing.Type = tapbackType
		existing.MXID = evt.ID
		existing.Update()
	}

	if errorMsg != "" {
		portal.log.Errorfln(errorMsg)
		portal.bridge.SendMessageErrorCheckpoint(evt, bridge.MsgStepRemote, errors.New(errorMsg), true, 0)
	}
}

func (portal *Portal) HandleMatrixRedaction(evt *event.Event) {
	if !portal.bridge.IM.Capabilities().SendTapbacks {
		portal.sendUnsupportedCheckpoint(evt, bridge.MsgStepRemote, errors.New("redactions are not supported"))
		return
	}

	if err := portal.shouldHandleMessage(evt); err != nil {
		portal.log.Debug(err)
		portal.sendErrorMessage(evt, err, true, bridge.MsgStatusTimeout)
		return
	}

	redactedTapback := portal.bridge.DB.Tapback.GetByMXID(evt.Redacts)
	if redactedTapback != nil {
		portal.log.Debugln("Starting handling of Matrix redaction", evt.ID)
		redactedTapback.Delete()
		_, err := portal.bridge.IM.SendTapback(portal.GUID, redactedTapback.MessageGUID, redactedTapback.MessagePart, redactedTapback.Type, true)
		if err != nil {
			portal.log.Errorfln("Failed to send removal of tapback %d to %s/%d: %v", redactedTapback.Type, redactedTapback.MessageGUID, redactedTapback.MessagePart, err)
			portal.bridge.SendMessageErrorCheckpoint(evt, bridge.MsgStepRemote, err, true, 0)
		} else {
			portal.log.Debugfln("Handled Matrix redaction %s of iMessage tapback %d to %s/%d", evt.ID, redactedTapback.Type, redactedTapback.MessageGUID, redactedTapback.MessagePart)
			if !portal.bridge.IM.Capabilities().MessageStatusCheckpoints {
				portal.bridge.SendMessageSuccessCheckpoint(evt, bridge.MsgStepRemote, 0)
			}
		}
		return
	}
	portal.sendUnsupportedCheckpoint(evt, bridge.MsgStepRemote, fmt.Errorf("can't redact non-reaction event"))
}

func (portal *Portal) UpdateAvatar(attachment *imessage.Attachment, intent *appservice.IntentAPI) *id.EventID {
	data, err := attachment.Read()
	if err != nil {
		portal.log.Errorfln("Failed to read avatar attachment: %v", err)
		return nil
	}
	hash := sha256.Sum256(data)
	if portal.AvatarHash != nil && hash == *portal.AvatarHash {
		portal.log.Debugfln("Not updating avatar: hash matches current avatar")
		return nil
	}
	portal.AvatarHash = &hash
	uploadResp, err := intent.UploadBytes(data, attachment.GetMimeType())
	if err != nil {
		portal.AvatarHash = nil
		portal.log.Errorfln("Failed to upload avatar attachment: %v", err)
		return nil
	}
	portal.AvatarURL = uploadResp.ContentURI
	if len(portal.MXID) > 0 {
		resp, err := intent.SetRoomAvatar(portal.MXID, portal.AvatarURL)
		if errors.Is(err, mautrix.MForbidden) && intent != portal.MainIntent() {
			resp, err = portal.MainIntent().SetRoomAvatar(portal.MXID, portal.AvatarURL)
		}
		if err != nil {
			portal.AvatarHash = nil
			portal.log.Errorfln("Failed to set room avatar: %v", err)
			return nil
		}
		portal.Update()
		portal.UpdateBridgeInfo()
		portal.log.Debugfln("Successfully updated room avatar (%s / %s)", portal.AvatarURL, resp.EventID)
		return &resp.EventID
	} else {
		return nil
	}
}

func (portal *Portal) isDuplicate(dbMessage *database.Message, msg *imessage.Message) bool {
	if portal.messageDedup == nil {
		return false
	}
	dedupKey := msg.Text
	if len(msg.Attachments) == 1 {
		dedupKey = msg.Attachments[0].FileName
	}
	portal.messageDedupLock.Lock()
	dedup, isDup := portal.messageDedup[strings.TrimSpace(dedupKey)]
	if isDup {
		delete(portal.messageDedup, dedupKey)
		portal.messageDedupLock.Unlock()
		portal.log.Debugfln("Received echo for Matrix message %s -> %s", dedup.EventID, msg.GUID)
		if !dedup.Timestamp.Before(msg.Time) {
			portal.log.Warnfln("Echo for Matrix message %s has lower timestamp than expected (message: %s, expected: %s)", msg.Time.Unix(), dedup.Timestamp.Unix())
		}
		dbMessage.MXID = dedup.EventID
		dbMessage.Insert()
		portal.sendDeliveryReceipt(dbMessage.MXID, msg.Service, true)
		return true
	}
	portal.messageDedupLock.Unlock()
	return false
}

func (portal *Portal) handleIMAvatarChange(msg *imessage.Message, intent *appservice.IntentAPI) *id.EventID {
	if msg.GroupActionType == imessage.GroupActionSetAvatar {
		if len(msg.Attachments) == 1 {
			return portal.UpdateAvatar(msg.Attachments[0], intent)
		} else {
			portal.log.Debugfln("Unexpected number of attachments (%d) in set avatar group action", len(msg.Attachments))
		}
	} else if msg.GroupActionType == imessage.GroupActionRemoveAvatar {
		// TODO
	} else {
		portal.log.Warnfln("Unexpected group action type %d in avatar change item", msg.GroupActionType)
	}
	return nil
}

func (portal *Portal) setMembership(inviter *appservice.IntentAPI, puppet *Puppet, membership event.Membership, ts int64) *id.EventID {
	err := inviter.EnsureInvited(portal.MXID, puppet.MXID)
	if err != nil {
		if errors.Is(err, mautrix.MForbidden) {
			err = portal.MainIntent().EnsureInvited(portal.MXID, puppet.MXID)
		}
		if err != nil {
			portal.log.Warnfln("Failed to ensure %s is invited to %s: %v", puppet.MXID, portal.MXID, err)
		}
	}
	resp, err := puppet.Intent.SendMassagedStateEvent(portal.MXID, event.StateMember, puppet.MXID.String(), &event.MemberEventContent{
		Membership:  membership,
		AvatarURL:   puppet.AvatarURL.CUString(),
		Displayname: puppet.Displayname,
	}, ts)
	if err != nil {
		puppet.log.Warnfln("Failed to join %s: %v", portal.MXID, err)
		return nil
	} else {
		portal.bridge.AS.StateStore.SetMembership(portal.MXID, puppet.MXID, "join")
		return &resp.EventID
	}
}

func (portal *Portal) handleIMMemberChange(msg *imessage.Message, dbMessage *database.Message, intent *appservice.IntentAPI) *id.EventID {
	if len(msg.Target.LocalID) == 0 {
		return nil
	}
	puppet := portal.bridge.GetPuppetByLocalID(msg.Target.LocalID)
	puppet.Sync()
	if msg.GroupActionType == imessage.GroupActionAddUser {
		return portal.setMembership(intent, puppet, event.MembershipJoin, dbMessage.Timestamp)
	} else if msg.GroupActionType == imessage.GroupActionRemoveUser {
		// TODO make sure this won't break anything and enable it
		//return portal.setMembership(intent, puppet, event.MembershipLeave, dbMessage.Timestamp)
	} else {
		portal.log.Warnfln("Unexpected group action type %d in member change item", msg.GroupActionType)
	}
	return nil
}

func (portal *Portal) handleIMAttachment(msg *imessage.Message, attach *imessage.Attachment, intent *appservice.IntentAPI) (*event.MessageEventContent, map[string]interface{}, error) {
	data, err := attach.Read()
	if err != nil {
		portal.log.Errorfln("Failed to read attachment in %s: %v", msg.GUID, err)
		return nil, nil, fmt.Errorf("failed to read attachment: %w", err)
	}

	mimeType := attach.GetMimeType()
	fileName := attach.GetFileName()
	extraContent := map[string]interface{}{}

	if msg.IsAudioMessage {
		ogg, err := ffmpeg.ConvertBytes(context.TODO(), data, ".ogg", []string{}, []string{"-c:a", "libopus"}, "audio/x-caf")
		if err == nil {
			extraContent["org.matrix.msc1767.audio"] = map[string]interface{}{}
			extraContent["org.matrix.msc3245.voice"] = map[string]interface{}{}
			mimeType = "audio/ogg"
			fileName = "Voice Message.ogg"
			data = ogg
		} else {
			portal.log.Errorf("Failed to convert audio message to ogg/opus: %v - sending without conversion", err)
		}
	}

	extraContent[bridgeInfoService] = msg.Service

	if CanConvertHEIF && portal.bridge.Config.Bridge.ConvertHEIF && (mimeType == "image/heic" || mimeType == "image/heif") {
		convertedData, err := ConvertHEIF(data)
		if err == nil {
			mimeType = "image/jpeg"
			fileName += ".jpg"
			data = convertedData
		} else {
			portal.log.Errorf("Failed to convert heif image to jpeg: %v - sending without conversion", err)
		}
	}

	if portal.bridge.Config.Bridge.ConvertTIFF && mimeType == "image/tiff" {
		convertedData, err := ConvertTIFF(data)
		if err == nil {
			mimeType = "image/jpeg"
			fileName += ".jpg"
			data = convertedData
		} else {
			portal.log.Errorf("Failed to convert tiff image to jpeg: %v - sending without conversion", err)
		}
	}

	if portal.bridge.Config.Bridge.ConvertVideo.Enabled && mimeType == "video/quicktime" {
		conv := portal.bridge.Config.Bridge.ConvertVideo
		convertedData, err := ffmpeg.ConvertBytes(context.TODO(), data, "."+conv.Extension, []string{}, conv.FFMPEGArgs, "video/quicktime")
		if err == nil {
			mimeType = conv.MimeType
			fileName += "." + conv.Extension
			data = convertedData
		} else {
			portal.log.Errorf("Failed to convert quicktime video to webm: %v - sending without conversion", err)
		}
	}

	uploadMime, uploadInfo := portal.encryptFile(data, mimeType)

	req := mautrix.ReqUploadMedia{
		ContentBytes: data,
		ContentType:  uploadMime,
	}
	var mxc id.ContentURI
	if portal.bridge.Config.Homeserver.AsyncMedia {
		uploaded, err := intent.UnstableUploadAsync(req)
		if err != nil {
			return nil, nil, err
		}
		mxc = uploaded.ContentURI
	} else {
		uploaded, err := intent.UploadMedia(req)
		if err != nil {
			return nil, nil, err
		}
		mxc = uploaded.ContentURI
	}

	if err != nil {
		portal.log.Errorfln("Failed to upload attachment in %s: %v", msg.GUID, err)
		return nil, nil, fmt.Errorf("failed to upload attachment")
	}
	var content event.MessageEventContent
	if uploadInfo != nil {
		uploadInfo.URL = mxc.CUString()
		content.File = uploadInfo
	} else {
		content.URL = mxc.CUString()
	}
	content.Body = fileName
	content.Info = &event.FileInfo{
		MimeType: mimeType,
		Size:     len(data),
	}
	switch strings.Split(mimeType, "/")[0] {
	case "image":
		content.MsgType = event.MsgImage
	case "video":
		content.MsgType = event.MsgVideo
	case "audio":
		content.MsgType = event.MsgAudio
	default:
		content.MsgType = event.MsgFile
	}
	portal.SetReply(&content, msg)
	return &content, extraContent, nil
}

func (portal *Portal) handleIMAttachments(msg *imessage.Message, dbMessage *database.Message, intent *appservice.IntentAPI) {
	if msg.Attachments == nil {
		return
	}
	for index, attach := range msg.Attachments {
		portal.log.Debugfln("Handling iMessage attachment %s.%d", msg.GUID, index)
		mediaContent, extraContent, err := portal.handleIMAttachment(msg, attach, intent)
		var resp *mautrix.RespSendEvent
		if err != nil {
			// Errors are already logged in handleIMAttachment so no need to log here, just send to Matrix room.
			resp, err = portal.sendMessage(intent, event.EventMessage, &event.MessageEventContent{
				MsgType: event.MsgNotice,
				Body:    err.Error(),
			}, extraContent, dbMessage.Timestamp)
		} else {
			if msg.Metadata != nil {
				extraContent["com.beeper.message_metadata"] = msg.Metadata
			}
			resp, err = portal.sendMessage(intent, event.EventMessage, &mediaContent, extraContent, dbMessage.Timestamp)
		}
		if err != nil {
			portal.log.Errorfln("Failed to send attachment %s.%d: %v", msg.GUID, index, err)
		} else {
			portal.log.Debugfln("Handled iMessage attachment %s.%d -> %s", msg.GUID, index, resp.EventID)
			dbMessage.MXID = resp.EventID
			dbMessage.Part = index
			dbMessage.Insert()
			// Attachments set the part explicitly, but a potential caption after attachments won't,
			// so pre-set the next part index here.
			dbMessage.Part++
		}
	}
}

func (portal *Portal) handleIMText(msg *imessage.Message, dbMessage *database.Message, intent *appservice.IntentAPI) {
	msg.Text = strings.ReplaceAll(msg.Text, "\ufffc", "")
	msg.Subject = strings.ReplaceAll(msg.Subject, "\ufffc", "")
	if len(msg.Text) > 0 {
		content := &event.MessageEventContent{
			MsgType: event.MsgText,
			Body:    msg.Text,
		}
		if len(msg.Subject) > 0 {
			content.Body = fmt.Sprintf("**%s**\n%s", msg.Subject, msg.Text)
			content.Format = event.FormatHTML
			content.FormattedBody = fmt.Sprintf("<strong>%s</strong><br>%s", html.EscapeString(msg.Subject), html.EscapeString(msg.Text))
		}
		portal.SetReply(content, msg)
		extraAttrs := map[string]interface{}{
			bridgeInfoService: msg.Service,
		}
		if msg.RichLink != nil {
			portal.log.Debugfln("Handling rich link in iMessage %s", msg.GUID)
			linkPreview := portal.convertRichLinkToBeeper(msg.RichLink)
			if linkPreview != nil {
				extraAttrs["com.beeper.linkpreviews"] = []*BeeperLinkPreview{linkPreview}
				portal.log.Debugfln("Link preview metadata converted for %s", msg.GUID)
			}
		}
		if msg.Metadata != nil {
			extraAttrs["com.beeper.message_metadata"] = msg.Metadata
		}
		resp, err := portal.sendMessage(intent, event.EventMessage, content, extraAttrs, dbMessage.Timestamp)
		if err != nil {
			portal.log.Errorfln("Failed to send message %s: %v", msg.GUID, err)
			return
		}
		portal.log.Debugfln("Handled iMessage text %s.%d -> %s", msg.GUID, dbMessage.Part, resp.EventID)
		dbMessage.MXID = resp.EventID
		dbMessage.Insert()
		dbMessage.Part++
	} else if len(msg.Attachments) == 0 {
		portal.log.Warnfln("iMessage %s doesn't contain any attachments nor text", msg.GUID)
	}
}

func (portal *Portal) handleIMError(msg *imessage.Message, dbMessage *database.Message, intent *appservice.IntentAPI) {
	if len(msg.ErrorNotice) > 0 {
		content := &event.MessageEventContent{
			MsgType: event.MsgNotice,
			Body:    msg.ErrorNotice,
		}
		portal.SetReply(content, msg)
		resp, err := portal.sendMessage(intent, event.EventMessage, content, map[string]interface{}{}, dbMessage.Timestamp)
		if err != nil {
			portal.log.Errorfln("Failed to send error notice %s: %v", msg.GUID, err)
			return
		}
		portal.log.Debugfln("Handled iMessage error notice %s.%d -> %s", msg.GUID, dbMessage.Part, resp.EventID)
		dbMessage.MXID = resp.EventID
		dbMessage.Insert()
		dbMessage.Part++
	}
}

func (portal *Portal) getIntentForMessage(msg *imessage.Message, dbMessage *database.Message) *appservice.IntentAPI {
	if msg.IsFromMe {
		intent := portal.bridge.user.DoublePuppetIntent
		if portal.isDuplicate(dbMessage, msg) {
			return nil
		} else if intent == nil {
			portal.log.Debugfln("Dropping own message in %s as double puppeting is not initialized", msg.ChatGUID)
			return nil
		}
		return intent
	} else if len(msg.Sender.LocalID) > 0 {
		localID := msg.Sender.LocalID
		if portal.bridge.Config.Bridge.ForceUniformDMSenders && portal.IsPrivateChat() && msg.Sender.LocalID != portal.Identifier.LocalID {
			portal.log.Debugfln("Message received from %s, which is not the expected sender %s. Forcing the original puppet.", localID, portal.Identifier.LocalID)
			localID = portal.Identifier.LocalID
		}
		puppet := portal.bridge.GetPuppetByLocalID(localID)
		if len(puppet.Displayname) == 0 {
			portal.log.Debugfln("Displayname of %s is empty, syncing before handling %s", puppet.ID, msg.GUID)
			puppet.Sync()
		}
		return puppet.Intent
	}
	return portal.MainIntent()
}

func (portal *Portal) HandleiMessage(msg *imessage.Message, isBackfill bool) id.EventID {
	var dbMessage *database.Message
	var overrideSuccess bool
	defer func() {
		if err := recover(); err != nil {
			portal.log.Errorfln("Panic while handling %s: %v\n%s", msg.GUID, err, string(debug.Stack()))
		}
		portal.bridge.IM.SendMessageBridgeResult(portal.GUID, msg.GUID, overrideSuccess || (dbMessage != nil && len(dbMessage.MXID) > 0))
	}()

	if msg.Tapback != nil {
		portal.HandleiMessageTapback(msg)
		return ""
	} else if portal.bridge.DB.Message.GetLastByGUID(portal.GUID, msg.GUID) != nil {
		portal.log.Debugln("Ignoring duplicate message", msg.GUID)
		// Send a success confirmation since it's a duplicate message
		overrideSuccess = true
		return ""
	}

	portal.log.Debugfln("Starting handling of iMessage %s (type: %d, attachments: %d, text: %d)", msg.GUID, msg.ItemType, len(msg.Attachments), len(msg.Text))
	dbMessage = portal.bridge.DB.Message.New()
	dbMessage.ChatGUID = portal.GUID
	dbMessage.SenderGUID = msg.Sender.String()
	dbMessage.GUID = msg.GUID
	dbMessage.Timestamp = msg.Time.UnixNano() / int64(time.Millisecond)

	intent := portal.getIntentForMessage(msg, dbMessage)
	if intent == nil {
		portal.log.Debugln("Handling of iMessage", msg.GUID, "was cancelled (didn't get an intent)")
		return dbMessage.MXID
	}

	var groupUpdateEventID *id.EventID

	switch msg.ItemType {
	case imessage.ItemTypeMessage:
		portal.handleIMAttachments(msg, dbMessage, intent)
		portal.handleIMText(msg, dbMessage, intent)
	case imessage.ItemTypeMember:
		groupUpdateEventID = portal.handleIMMemberChange(msg, dbMessage, intent)
	case imessage.ItemTypeName:
		groupUpdateEventID = portal.UpdateName(msg.NewGroupName, intent)
	case imessage.ItemTypeAvatar:
		groupUpdateEventID = portal.handleIMAvatarChange(msg, intent)
	case imessage.ItemTypeError:
		// Handled below
	default:
		portal.log.Debugfln("Dropping message %s with unknown item type %d", msg.GUID, msg.ItemType)
		return ""
	}

	portal.handleIMError(msg, dbMessage, intent)

	if groupUpdateEventID != nil {
		dbMessage.MXID = *groupUpdateEventID
		dbMessage.Insert()
	}

	if len(dbMessage.MXID) > 0 {
		portal.sendDeliveryReceipt(dbMessage.MXID, msg.Service, false)
		if !isBackfill && !msg.IsFromMe && msg.IsRead {
			err := portal.markRead(portal.bridge.user.DoublePuppetIntent, dbMessage.MXID, time.Time{})
			if err != nil {
				portal.log.Warnln("Failed to mark %s as read after bridging: %v", dbMessage.MXID, err)
			}
		}
	} else {
		portal.log.Debugfln("Unhandled message %s", msg.GUID)
	}
	return dbMessage.MXID
}

func (portal *Portal) HandleiMessageTapback(msg *imessage.Message) {
	portal.log.Debugln("Starting handling of iMessage tapback", msg.GUID, "to", msg.Tapback.TargetGUID)
	target := portal.bridge.DB.Message.GetByGUID(portal.GUID, msg.Tapback.TargetGUID, msg.Tapback.TargetPart)
	if target == nil {
		portal.log.Debugfln("Unknown tapback target %s.%d", msg.Tapback.TargetGUID, msg.Tapback.TargetPart)
		return
	}
	var intent *appservice.IntentAPI
	if msg.IsFromMe {
		intent = portal.bridge.user.DoublePuppetIntent
		if intent == nil {
			portal.log.Debugfln("Dropping own tapback in %s as double puppeting is not initialized", msg.ChatGUID)
			return
		}
	} else {
		puppet := portal.bridge.GetPuppetByLocalID(msg.Sender.LocalID)
		intent = puppet.Intent
	}
	senderGUID := msg.Sender.String()

	existing := portal.bridge.DB.Tapback.GetByGUID(portal.GUID, target.GUID, target.Part, senderGUID)
	if msg.Tapback.Remove {
		if existing == nil {
			return
		}
		_, err := intent.RedactEvent(portal.MXID, existing.MXID)
		if err != nil {
			portal.log.Warnfln("Failed to remove tapback from %s: %v", msg.SenderText(), err)
		}
		existing.Delete()
		return
	} else if existing != nil && existing.Type == msg.Tapback.Type {
		portal.log.Debugfln("Ignoring tapback from %s to %s: type is same", msg.SenderText(), target.GUID)
		return
	}

	content := &event.ReactionEventContent{
		RelatesTo: event.RelatesTo{
			EventID: target.MXID,
			Type:    event.RelAnnotation,
			Key:     msg.Tapback.Type.Emoji(),
		},
	}

	if existing != nil {
		if _, err := intent.RedactEvent(portal.MXID, existing.MXID); err != nil {
			portal.log.Warnfln("Failed to redact old tapback from %s: %v", msg.SenderText(), err)
		}
	}

	resp, err := intent.SendMessageEvent(portal.MXID, event.EventReaction, &content)

	if err != nil {
		portal.log.Errorfln("Failed to send tapback from %s: %v", msg.SenderText(), err)
		return
	}

	if existing == nil {
		tapback := portal.bridge.DB.Tapback.New()
		tapback.ChatGUID = portal.GUID
		tapback.MessageGUID = target.GUID
		tapback.MessagePart = target.Part
		tapback.SenderGUID = senderGUID
		tapback.GUID = msg.GUID
		tapback.Type = msg.Tapback.Type
		tapback.MXID = resp.EventID
		tapback.Insert()
	} else {
		existing.GUID = msg.GUID
		existing.Type = msg.Tapback.Type
		existing.MXID = resp.EventID
		existing.Update()
	}
}

func (portal *Portal) Delete() {
	portal.Portal.Delete()
	portal.bridge.portalsLock.Lock()
	delete(portal.bridge.portalsByGUID, portal.GUID)
	if len(portal.MXID) > 0 {
		delete(portal.bridge.portalsByMXID, portal.MXID)
	}
	portal.bridge.portalsLock.Unlock()
}

func (portal *Portal) GetMatrixUsers() ([]id.UserID, error) {
	members, err := portal.MainIntent().JoinedMembers(portal.MXID)
	if err != nil {
		return nil, fmt.Errorf("failed to get member list: %w", err)
	}
	var users []id.UserID
	for userID := range members.Joined {
		_, isPuppet := portal.bridge.ParsePuppetMXID(userID)
		if !isPuppet && userID != portal.bridge.Bot.UserID {
			users = append(users, userID)
		}
	}
	return users, nil
}

// TombstoneOrReIDIfNeeded returns true if the portal metadata should be synchronized
func (portal *Portal) TombstoneOrReIDIfNeeded() (retargeted, tombstoned bool) {
	if portal.Identifier.Service != "SMS" || !portal.bridge.Config.IMessage.TombstoneOldRooms || len(portal.MXID) == 0 {
		return false, false
	}
	identifier := portal.Identifier
	identifier.Service = "iMessage"
	if replacement := portal.bridge.DB.Portal.GetByGUID(identifier.String()); replacement != nil {
		if len(replacement.MXID) == 0 {
			portal.log.Infofln("ReID %s to %s for chat merging", portal.Identifier.String(), identifier.String())
			replacement.Delete()
			portal.ReID(identifier.String())
			portal.Identifier = identifier
			return true, false
		}
		return false, portal.mergeIntoPortal(replacement.MXID, "SMS rooms have been merged with iMessage rooms.")
	} else {
		portal.log.Infofln("ReID %s to %s for chat merging", portal.Identifier.String(), identifier.String())
		portal.ReID(identifier.String())
		portal.Identifier = identifier
		return true, false
	}
}

func (portal *Portal) CleanupIfEmpty(deleteIfForbidden bool) bool {
	if len(portal.MXID) == 0 {
		return false
	}

	users, err := portal.GetMatrixUsers()
	if err != nil {
		if deleteIfForbidden && errors.Is(err, mautrix.MForbidden) {
			portal.log.Errorfln("Got %v while checking if portal is empty, assuming it's gone", err)
			portal.Delete()
			return true
		} else {
			portal.log.Errorfln("Failed to get Matrix user list to determine if portal needs to be cleaned up: %v", err)
		}
		return false
	}

	if len(users) == 0 {
		portal.log.Infoln("Room seems to be empty, cleaning up...")
		portal.Delete()
		portal.Cleanup(false)
		return true
	}
	return false
}

func (portal *Portal) Cleanup(puppetsOnly bool) {
	if len(portal.MXID) == 0 {
		return
	}
	if portal.IsPrivateChat() {
		_, err := portal.MainIntent().LeaveRoom(portal.MXID)
		if err != nil {
			portal.log.Warnln("Failed to leave private chat portal with main intent:", err)
		}
		return
	}
	intent := portal.MainIntent()
	members, err := intent.JoinedMembers(portal.MXID)
	if err != nil {
		portal.log.Errorln("Failed to get portal members for cleanup:", err)
		return
	}
	if _, isJoined := members.Joined[portal.bridge.user.MXID]; !puppetsOnly && !isJoined {
		// Kick the user even if they're not joined in case they're invited.
		_, _ = intent.KickUser(portal.MXID, &mautrix.ReqKickUser{UserID: portal.bridge.user.MXID, Reason: "Deleting portal"})
	}
	for member := range members.Joined {
		if member == intent.UserID {
			continue
		}
		puppet := portal.bridge.GetPuppetByMXID(member)
		if puppet != nil {
			_, err = puppet.Intent.LeaveRoom(portal.MXID)
			if err != nil {
				portal.log.Errorln("Error leaving as puppet while cleaning up portal:", err)
			}
		} else if !puppetsOnly {
			_, err = intent.KickUser(portal.MXID, &mautrix.ReqKickUser{UserID: member, Reason: "Deleting portal"})
			if err != nil {
				portal.log.Errorln("Error kicking user while cleaning up portal:", err)
			}
		}
	}
	_, err = intent.LeaveRoom(portal.MXID)
	if err != nil {
		portal.log.Errorln("Error leaving with main intent while cleaning up portal:", err)
	}
}
