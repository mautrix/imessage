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
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gabriel-vasile/mimetype"
	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"go.mau.fi/util/ffmpeg"
	"go.mau.fi/util/jsontime"
	log "maunium.net/go/maulogger/v2"
	"maunium.net/go/maulogger/v2/maulogadapt"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/bridge"
	"maunium.net/go/mautrix/bridge/bridgeconfig"
	"maunium.net/go/mautrix/bridge/status"
	"maunium.net/go/mautrix/crypto/attachment"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-imessage/database"
	"go.mau.fi/mautrix-imessage/imessage"
	"go.mau.fi/mautrix-imessage/ipc"
)

func (br *IMBridge) GetPortalByMXID(mxid id.RoomID) *Portal {
	br.portalsLock.Lock()
	defer br.portalsLock.Unlock()
	portal, ok := br.portalsByMXID[mxid]
	if !ok {
		return br.loadDBPortal(nil, br.DB.Portal.GetByMXID(mxid), "")
	}
	return portal
}

func (br *IMBridge) GetPortalByGUID(guid string) *Portal {
	return br.GetPortalByGUIDWithTransaction(nil, guid)
}

func (br *IMBridge) GetPortalByGUIDWithTransaction(txn dbutil.Execable, guid string) *Portal {
	br.portalsLock.Lock()
	defer br.portalsLock.Unlock()
	return br.maybeGetPortalByGUID(txn, guid, true)
}

func (br *IMBridge) GetPortalByGUIDIfExists(guid string) *Portal {
	br.portalsLock.Lock()
	defer br.portalsLock.Unlock()
	return br.maybeGetPortalByGUID(nil, guid, false)
}

func (br *IMBridge) maybeGetPortalByGUID(txn dbutil.Execable, guid string, createIfNotExist bool) *Portal {
	if br.Config.Bridge.DisableSMSPortals && strings.HasPrefix(guid, "SMS;-;") {
		parsed := imessage.ParseIdentifier(guid)
		if !parsed.IsGroup && parsed.Service == "SMS" {
			parsed.Service = "iMessage"
			guid = parsed.String()
		}
	}
	fallbackGUID := guid
	if !createIfNotExist {
		fallbackGUID = ""
	}
	portal, ok := br.portalsByGUID[guid]
	if !ok {
		return br.loadDBPortal(txn, br.DB.Portal.GetByGUID(guid), fallbackGUID)
	}
	return portal
}

func (br *IMBridge) GetMessagesSince(chatGUID string, since time.Time) (out []string) {
	return br.DB.Message.GetIDsSince(chatGUID, since)
}

func (br *IMBridge) ReIDPortal(oldGUID, newGUID string, mergeExisting bool) bool {
	br.portalsLock.Lock()
	defer br.portalsLock.Unlock()
	portal := br.maybeGetPortalByGUID(nil, oldGUID, false)
	if portal == nil {
		br.Log.Debugfln("Ignoring chat ID change %s->%s, no portal with old ID found", oldGUID, newGUID)
		return false
	}

	return portal.reIDInto(newGUID, nil, false, mergeExisting)
}

func (portal *Portal) reIDInto(newGUID string, newPortal *Portal, lock, mergeExisting bool) bool {
	br := portal.bridge
	if lock {
		br.portalsLock.Lock()
		defer br.portalsLock.Unlock()
	}
	if newPortal == nil {
		newPortal = br.maybeGetPortalByGUID(nil, newGUID, false)
	}
	if newPortal != nil {
		if mergeExisting && portal.MXID != "" && newPortal.MXID != "" && br.Config.Homeserver.Software == bridgeconfig.SoftwareHungry {
			br.Log.Infofln("Got chat ID change %s->%s, but portal with new ID already exists. Merging portals in background", portal.GUID, newGUID)
			go newPortal.Merge([]*Portal{portal})
			return false
		} else if newPortal.MXID == "" && portal.MXID != "" {
			br.Log.Infofln("Got chat ID change %s->%s. Portal with new ID already exists, but it doesn't have a room. Nuking new portal row before changing ID", portal.GUID, newGUID)
			newPortal.unlockedDelete()
		} else {
			br.Log.Warnfln("Got chat ID change %s->%s, but portal with new ID already exists. Nuking old portal and not changing ID", portal.GUID, newGUID)
			portal.unlockedDelete()
			if len(portal.MXID) > 0 && portal.bridge.user.DoublePuppetIntent != nil {
				_, _ = portal.bridge.user.DoublePuppetIntent.LeaveRoom(portal.MXID)
			}
			portal.Cleanup(false)
			return false
		}
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
	return br.dbPortalsToPortals(br.DB.Portal.GetAllWithMXID())
}

func (br *IMBridge) FindPortalsByThreadID(threadID string) []*Portal {
	return br.dbPortalsToPortals(br.DB.Portal.FindByThreadID(threadID))
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
			portal = br.loadDBPortal(nil, dbPortal, "")
		}
		output[index] = portal
	}
	return output
}

func (br *IMBridge) loadDBPortal(txn dbutil.Execable, dbPortal *database.Portal, guid string) *Portal {
	if dbPortal == nil {
		if guid == "" {
			return nil
		}
		dbPortal = br.DB.Portal.New()
		dbPortal.GUID = guid
		dbPortal.Insert(txn)
	} else if guid != dbPortal.GUID {
		aliasedPortal, ok := br.portalsByGUID[dbPortal.GUID]
		if ok {
			br.portalsByGUID[guid] = aliasedPortal
			return aliasedPortal
		}
	}
	portal := br.NewPortal(dbPortal)
	br.portalsByGUID[portal.GUID] = portal
	if portal.IsPrivateChat() {
		portal.SecondaryGUIDs = br.DB.MergedChat.GetAllForTarget(portal.GUID)
		for _, sourceGUID := range portal.SecondaryGUIDs {
			br.portalsByGUID[sourceGUID] = portal
		}
	}
	if len(portal.MXID) > 0 {
		br.portalsByMXID[portal.MXID] = portal
	}
	return portal
}

func (br *IMBridge) NewPortal(dbPortal *database.Portal) *Portal {
	portal := &Portal{
		Portal: dbPortal,
		bridge: br,
		zlog:   br.ZLog.With().Str("portal_guid", dbPortal.GUID).Logger(),

		Identifier:      imessage.ParseIdentifier(dbPortal.GUID),
		Messages:        make(chan *imessage.Message, 100),
		ReadReceipts:    make(chan *imessage.ReadReceipt, 100),
		MessageStatuses: make(chan *imessage.SendMessageStatus, 100),
		MatrixMessages:  make(chan *event.Event, 100),
		backfillStart:   make(chan struct{}),
	}
	portal.log = maulogadapt.ZeroAsMau(&portal.zlog)
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
	// Deprecated
	log  log.Logger
	zlog zerolog.Logger

	SecondaryGUIDs []string

	Messages        chan *imessage.Message
	ReadReceipts    chan *imessage.ReadReceipt
	MessageStatuses chan *imessage.SendMessageStatus
	MatrixMessages  chan *event.Event
	backfillStart   chan struct{}
	backfillWait    sync.WaitGroup
	backfillLock    sync.Mutex

	roomCreateLock   sync.Mutex
	messageDedup     map[string]SentMessage
	messageDedupLock sync.Mutex
	Identifier       imessage.Identifier

	userIsTyping bool
	typingLock   sync.Mutex
}

var (
	_ bridge.Portal                    = (*Portal)(nil)
	_ bridge.ReadReceiptHandlingPortal = (*Portal)(nil)
	_ bridge.TypingPortal              = (*Portal)(nil)
	//	_ bridge.MembershipHandlingPortal = (*Portal)(nil)
	//	_ bridge.MetaHandlingPortal = (*Portal)(nil)
	//	_ bridge.DisappearingPortal = (*Portal)(nil)
)

func (portal *Portal) IsEncrypted() bool {
	return portal.Encrypted
}

func (portal *Portal) MarkEncrypted() {
	portal.Encrypted = true
	portal.Update(nil)
}

func (portal *Portal) ReceiveMatrixEvent(_ bridge.User, evt *event.Event) {
	portal.MatrixMessages <- evt
}

func (portal *Portal) addSecondaryGUIDs(guids []string) {
	portal.SecondaryGUIDs = append(portal.SecondaryGUIDs, guids...)
	sort.Strings(portal.SecondaryGUIDs)
	filtered := portal.SecondaryGUIDs[:0]
	for i, guid := range portal.SecondaryGUIDs {
		if i >= len(portal.SecondaryGUIDs)-1 || guid != portal.SecondaryGUIDs[i+1] {
			filtered = append(filtered, guid)
		}
	}
	portal.SecondaryGUIDs = filtered
}

func (portal *Portal) SyncParticipants(chatInfo *imessage.ChatInfo) (memberIDs []id.UserID) {
	var members map[id.UserID]mautrix.JoinedMember
	if portal.MXID != "" {
		membersResp, err := portal.MainIntent().JoinedMembers(portal.MXID)
		if err != nil {
			portal.log.Warnfln("Failed to get members in room to remove extra members: %v", err)
		} else {
			members = membersResp.Joined
			delete(members, portal.bridge.Bot.UserID)
			delete(members, portal.bridge.user.MXID)
		}
	}
	portal.zlog.Debug().
		Int("chat_info_member_count", len(chatInfo.Members)).
		Int("room_member_count", len(members)).
		Msg("Syncing participants")
	for _, member := range chatInfo.Members {
		puppet := portal.bridge.GetPuppetByLocalID(member)
		puppet.Sync()
		memberIDs = append(memberIDs, puppet.MXID)
		if portal.MXID != "" {
			err := puppet.Intent.EnsureJoined(portal.MXID)
			if err != nil {
				portal.log.Warnfln("Failed to make puppet of %s join %s: %v", member, portal.MXID, err)
			}
		}
		if members != nil {
			delete(members, puppet.MXID)
		}
	}
	if members != nil && !portal.bridge.Config.Bridge.Relay.Enabled {
		for userID := range members {
			portal.log.Debugfln("Removing %s as they don't seem to be in the group anymore", userID)
			_, err := portal.MainIntent().KickUser(portal.MXID, &mautrix.ReqKickUser{
				Reason: "user is no longer in group",
				UserID: userID,
			})
			if err != nil {
				portal.log.Errorfln("Failed to remove %s: %v", userID, err)
			}
		}
	}
	return memberIDs
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

func (portal *Portal) SyncWithInfo(chatInfo *imessage.ChatInfo) {
	portal.zlog.Debug().Interface("chat_info", chatInfo).Msg("Syncing with chat info")
	update := false
	if chatInfo.ThreadID != "" && chatInfo.ThreadID != portal.ThreadID {
		portal.log.Infofln("Found portal thread ID in sync: %s (prev: %s)", chatInfo.ThreadID, portal.ThreadID)
		portal.ThreadID = chatInfo.ThreadID
		update = true
	}
	if len(chatInfo.DisplayName) > 0 {
		update = portal.UpdateName(chatInfo.DisplayName, nil) != nil || update
	}
	if !portal.IsPrivateChat() && chatInfo.Members != nil {
		portal.SyncParticipants(chatInfo)
	}
	if update {
		portal.Update(nil)
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

	pls, err := portal.MainIntent().PowerLevels(portal.MXID)
	if err != nil {
		portal.zlog.Warn().Err(err).Msg("Failed to get power levels")
	} else if portal.updatePowerLevels(pls) {
		resp, err := portal.MainIntent().SetPowerLevels(portal.MXID, pls)
		if err != nil {
			portal.zlog.Warn().Err(err).Msg("Failed to update power levels")
		} else {
			portal.zlog.Debug().Str("event_id", resp.EventID.String()).Msg("Updated power levels")
		}
	}

	if !portal.IsPrivateChat() {
		chatInfo, err := portal.bridge.IM.GetChatInfo(portal.GUID, portal.ThreadID)
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
		puppet := portal.bridge.GetPuppetByLocalID(portal.Identifier.LocalID)
		puppet.Sync()
	}

	if backfill && portal.bridge.Config.Bridge.Backfill.Enable {
		portal.lockBackfill()
		portal.forwardBackfill()
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
	for {
		var start time.Time
		var thing string
		select {
		case msg := <-portal.Messages:
			start = time.Now()
			thing = "iMessage"
			portal.HandleiMessage(msg)
		case readReceipt := <-portal.ReadReceipts:
			start = time.Now()
			thing = "read receipt"
			portal.HandleiMessageReadReceipt(readReceipt)
		case <-portal.backfillStart:
			thing = "backfill lock"
			start = time.Now()
			portal.backfillWait.Wait()
		case evt := <-portal.MatrixMessages:
			start = time.Now()
			switch evt.Type {
			case event.EventMessage, event.EventSticker:
				thing = "Matrix message"
				portal.HandleMatrixMessage(evt)
			case event.EventRedaction:
				thing = "Matrix redaction"
				portal.HandleMatrixRedaction(evt)
			case event.EventReaction:
				thing = "Matrix reaction"
				portal.HandleMatrixReaction(evt)
			default:
				thing = "unsupported Matrix event"
				portal.log.Warnln("Unsupported event type %+v in portal message channel", evt.Type)
			}
		case msgStatus := <-portal.MessageStatuses:
			start = time.Now()
			thing = "message status"
			portal.HandleiMessageSendMessageStatus(msgStatus)
		}
		portal.log.Debugfln(
			"Handled %s in %s (queued: %di/%dr/%dm/%ds)",
			thing, time.Since(start),
			len(portal.messageDedup), len(portal.ReadReceipts), len(portal.MatrixMessages), len(portal.MessageStatuses),
		)
	}
}

func (portal *Portal) HandleiMessageSendMessageStatus(msgStatus *imessage.SendMessageStatus) {
	if msgStatus.GUID == portal.bridge.pendingHackyTestGUID && portal.Identifier.LocalID == portal.bridge.Config.HackyStartupTest.Identifier {
		portal.bridge.trackStartupTestPingStatus(msgStatus)
	}

	var eventID id.EventID
	if msg := portal.bridge.DB.Message.GetLastByGUID(portal.GUID, msgStatus.GUID); msg != nil {
		eventID = msg.MXID
	} else if tapback := portal.bridge.DB.Tapback.GetByTapbackGUID(portal.GUID, msgStatus.GUID); tapback != nil {
		eventID = tapback.MXID
	} else {
		portal.log.Debugfln("Dropping send message status for %s: not found in db messages or tapbacks", msgStatus.GUID)
		return
	}
	portal.log.Debugfln("Processing message status with type %s/%s for event %s/%s in %s/%s", msgStatus.Status, msgStatus.StatusCode, eventID, msgStatus.GUID, portal.MXID, msgStatus.ChatGUID)
	switch msgStatus.Status {
	case "delivered":
		go portal.bridge.SendRawMessageCheckpoint(&status.MessageCheckpoint{
			EventID:    eventID,
			RoomID:     portal.MXID,
			Step:       status.MsgStepRemote,
			Timestamp:  jsontime.UnixMilliNow(),
			Status:     status.MsgStatusDelivered,
			ReportedBy: status.MsgReportedByBridge,
		})
		if p := portal.GetDMPuppet(); p != nil {
			go portal.sendSuccessMessageStatus(eventID, msgStatus.Service, msgStatus.ChatGUID, []id.UserID{p.MXID})
		}
	case "sent":
		portal.sendSuccessCheckpoint(eventID, msgStatus.Service, msgStatus.ChatGUID)
	case "failed":
		evt, err := portal.MainIntent().GetEvent(portal.MXID, eventID)
		if err != nil {
			portal.log.Warnfln("Failed to lookup event %s/%s %s/%s: %v", string(eventID), portal.MXID, msgStatus.GUID, msgStatus.ChatGUID, err)
			return
		}
		errString := "internal error"
		humanReadableError := "internal error"
		if len(msgStatus.Message) != 0 {
			humanReadableError = msgStatus.Message
			if len(msgStatus.StatusCode) != 0 {
				errString = fmt.Sprintf("%s: %s", msgStatus.StatusCode, msgStatus.Message)
			} else {
				errString = msgStatus.Message
			}
		} else if len(msgStatus.StatusCode) != 0 {
			errString = msgStatus.StatusCode
		}
		if portal.bridge.isWarmingUp() {
			errString = "warmingUp: " + errString
			humanReadableError = "The bridge is still warming up - please wait"
		}
		portal.sendErrorMessage(evt, errors.New(errString), humanReadableError, true, status.MsgStatusPermFailure, msgStatus.ChatGUID)
	default:
		portal.log.Warnfln("Unrecognized message status type %s", msgStatus.Status)
	}
}

func (portal *Portal) GetBasePowerLevels() *event.PowerLevelsEventContent {
	anyone := 0
	nope := 99
	return &event.PowerLevelsEventContent{
		UsersDefault:    anyone,
		EventsDefault:   anyone,
		RedactPtr:       &anyone,
		StateDefaultPtr: &nope,
		BanPtr:          &nope,
		InvitePtr:       &nope,
		KickPtr:         &nope,
		Users: map[id.UserID]int{
			portal.MainIntent().UserID: 100,
			portal.bridge.Bot.UserID:   100,
		},
		Events: map[string]int{
			event.StateRoomName.Type:   anyone,
			event.StateRoomAvatar.Type: anyone,
			event.StateTopic.Type:      anyone,
		},
	}
}

func (portal *Portal) updatePowerLevels(pl *event.PowerLevelsEventContent) bool {
	return pl.EnsureUserLevel(portal.bridge.Bot.UserID, 100)
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

			GUID:     portal.GUID,
			ThreadID: portal.ThreadID,
			IsGroup:  portal.Identifier.IsGroup,
			Service:  portal.Identifier.Service,

			SendStatusStart: portal.bridge.SendStatusStartTS,
			TimeoutSeconds:  portal.bridge.Config.Bridge.MaxHandleSeconds,
		},
	}
	if portal.Identifier.Service == "SMS" {
		if portal.bridge.Config.IMessage.Platform == "android" {
			bridgeInfo.Protocol.ID = "android-sms"
			bridgeInfo.Protocol.DisplayName = "Android SMS"
			bridgeInfo.Protocol.ExternalURL = ""
			bridgeInfo.Channel.DeviceID = portal.bridge.Config.Bridge.DeviceID
		} else {
			bridgeInfo.Protocol.ID = "imessage-sms"
			bridgeInfo.Protocol.DisplayName = "iMessage (SMS)"
		}
	} else if portal.bridge.Config.IMessage.Platform == "ios" {
		bridgeInfo.Protocol.ID = "imessage-ios"
	} else if portal.bridge.Config.IMessage.Platform == "mac-nosip" {
		bridgeInfo.Protocol.ID = "imessage-nosip"
	} else if portal.bridge.Config.IMessage.Platform == "bluebubbles" {
		bridgeInfo.Protocol.ID = "imessagego"
	}
	return portal.getBridgeInfoStateKey(), bridgeInfo
}

func (portal *Portal) UpdateBridgeInfo() {
	if len(portal.MXID) == 0 {
		portal.log.Debugln("Not updating bridge info: no Matrix room created")
		return
	}
	portal.log.Debugln("Updating bridge info...")
	intent := portal.MainIntent()
	if portal.Encrypted && intent != portal.bridge.Bot && portal.IsPrivateChat() {
		intent = portal.bridge.Bot
	}
	stateKey, content := portal.getBridgeInfo()
	_, err := intent.SendStateEvent(portal.MXID, event.StateBridge, stateKey, content)
	if err != nil {
		portal.log.Warnln("Failed to update m.bridge:", err)
	}
	_, err = intent.SendStateEvent(portal.MXID, event.StateHalfShotBridge, stateKey, content)
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

func (portal *Portal) shouldSetDMRoomMetadata() bool {
	return !portal.IsPrivateChat() ||
		portal.bridge.Config.Bridge.PrivateChatPortalMeta == "always" ||
		(portal.IsEncrypted() && portal.bridge.Config.Bridge.PrivateChatPortalMeta != "never")
}

func (portal *Portal) getRoomCreateContent() *mautrix.ReqCreateRoom {
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
	if portal.bridge.Config.Bridge.Encryption.Default {
		initialState = append(initialState, &event.Event{
			Type: event.StateEncryption,
			Content: event.Content{
				Parsed: portal.GetEncryptionEventContent(),
			},
		})
		portal.Encrypted = true
	}
	if !portal.AvatarURL.IsEmpty() && portal.shouldSetDMRoomMetadata() {
		initialState = append(initialState, &event.Event{
			Type: event.StateRoomAvatar,
			Content: event.Content{
				Parsed: event.RoomAvatarEventContent{URL: portal.AvatarURL},
			},
		})
	}

	var invite []id.UserID

	if portal.IsPrivateChat() {
		invite = append(invite, portal.bridge.Bot.UserID)
	}

	autoJoinInvites := portal.bridge.Config.Homeserver.Software == bridgeconfig.SoftwareHungry
	if autoJoinInvites {
		invite = append(invite, portal.bridge.user.MXID)
	}

	creationContent := make(map[string]interface{})
	if !portal.bridge.Config.Bridge.FederateRooms {
		creationContent["m.federate"] = false
	}
	req := &mautrix.ReqCreateRoom{
		Visibility:      "private",
		Name:            portal.Name,
		Invite:          invite,
		Preset:          "private_chat",
		IsDirect:        portal.IsPrivateChat(),
		InitialState:    initialState,
		CreationContent: creationContent,
		RoomVersion:     "11",

		BeeperAutoJoinInvites: autoJoinInvites,
	}
	if !portal.shouldSetDMRoomMetadata() {
		req.Name = ""
	}
	return req
}

func (portal *Portal) preCreateDMSync(profileOverride *ProfileOverride) {
	puppet := portal.bridge.GetPuppetByLocalID(portal.Identifier.LocalID)
	puppet.Sync()
	if profileOverride != nil {
		puppet.SyncWithProfileOverride(*profileOverride)
	}
	portal.Name = puppet.Displayname
	portal.AvatarURL = puppet.AvatarURL
	portal.AvatarHash = puppet.AvatarHash
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
		chatInfo, err = portal.bridge.IM.GetChatInfo(portal.GUID, portal.ThreadID)
		if err != nil && !portal.IsPrivateChat() {
			// If there's no chat info for a group, it probably doesn't exist, and we shouldn't auto-create a Matrix room for it.
			return fmt.Errorf("failed to get chat info: %w", err)
		}
	}
	if chatInfo != nil {
		portal.Name = chatInfo.DisplayName
		portal.ThreadID = chatInfo.ThreadID
	} else {
		portal.log.Warnln("Didn't get any chat info")
	}

	if portal.IsPrivateChat() {
		portal.preCreateDMSync(profileOverride)
	} else {
		avatar, err := portal.bridge.IM.GetGroupAvatar(portal.GUID)
		if err != nil {
			portal.log.Warnln("Failed to get avatar:", err)
		} else if avatar != nil {
			portal.UpdateAvatar(avatar, portal.MainIntent())
		}
	}

	req := portal.getRoomCreateContent()
	if req.BeeperAutoJoinInvites && !portal.IsPrivateChat() {
		req.Invite = append(req.Invite, portal.SyncParticipants(chatInfo)...)
	}
	resp, err := intent.CreateRoom(req)
	if err != nil {
		return err
	}
	doBackfill := portal.bridge.Config.Bridge.Backfill.Enable && portal.bridge.Config.Bridge.Backfill.InitialLimit > 0
	if doBackfill {
		portal.log.Debugln("Locking backfill (create)")
		portal.lockBackfill()
	}
	portal.MXID = resp.RoomID
	portal.log.Debugln("Storing created room ID", portal.MXID, "in database")
	portal.Update(nil)
	portal.bridge.portalsLock.Lock()
	portal.bridge.portalsByMXID[portal.MXID] = portal
	portal.bridge.portalsLock.Unlock()

	portal.log.Debugln("Updating state store with initial memberships")
	inviteeMembership := event.MembershipInvite
	if req.BeeperAutoJoinInvites {
		inviteeMembership = event.MembershipJoin
	}
	for _, user := range req.Invite {
		portal.bridge.StateStore.SetMembership(portal.MXID, user, inviteeMembership)
	}

	if portal.Encrypted && !req.BeeperAutoJoinInvites {
		portal.log.Debugln("Ensuring bridge bot is joined to portal")
		err = portal.bridge.Bot.EnsureJoined(portal.MXID)
		if err != nil {
			portal.log.Errorln("Failed to join created portal with bridge bot for e2be:", err)
		}
	}

	if !req.BeeperAutoJoinInvites {
		portal.ensureUserInvited(portal.bridge.user)
	}
	portal.addToSpace(portal.bridge.user)

	if !portal.IsPrivateChat() {
		if !req.BeeperAutoJoinInvites {
			portal.log.Debugln("New portal is group chat, syncing participants")
			portal.SyncParticipants(chatInfo)
		}
	} else {
		portal.bridge.user.UpdateDirectChats(map[id.UserID][]id.RoomID{portal.GetDMPuppet().MXID: {portal.MXID}})
	}
	if portal.bridge.Config.Homeserver.Software != bridgeconfig.SoftwareHungry {
		firstEventResp, err := portal.MainIntent().SendMessageEvent(portal.MXID, PortalCreationDummyEvent, struct{}{})
		if err != nil {
			portal.log.Errorln("Failed to send dummy event to mark portal creation:", err)
		} else {
			portal.FirstEventID = firstEventResp.EventID
			portal.Update(nil)
		}
	}
	if doBackfill {
		go func() {
			portal.log.Debugln("Starting initial backfill")
			portal.forwardBackfill()
			portal.log.Debugln("Unlocking backfill (create)")
			portal.unlockBackfill()
		}()
	}
	portal.log.Debugln("Finished creating Matrix room")

	if portal.bridge.IM.Capabilities().ChatBridgeResult {
		portal.bridge.IM.SendChatBridgeResult(portal.GUID, portal.MXID)
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
		portal.Update(nil)
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

func (portal *Portal) sendMainIntentMessage(content interface{}) (*mautrix.RespSendEvent, error) {
	return portal.sendMessage(portal.MainIntent(), event.EventMessage, content, map[string]interface{}{}, 0)
}

func (portal *Portal) encrypt(intent *appservice.IntentAPI, content *event.Content, eventType event.Type) (event.Type, error) {
	if portal.Encrypted && portal.bridge.Crypto != nil {
		intent.AddDoublePuppetValue(content)
		handle, ok := content.Raw[bridgeInfoHandle].(string)
		err := portal.bridge.Crypto.Encrypt(portal.MXID, eventType, content)
		if err != nil {
			return eventType, fmt.Errorf("failed to encrypt event: %w", err)
		}
		eventType = event.EventEncrypted
		if ok && content.Raw == nil {
			content.Raw = map[string]any{
				bridgeInfoHandle: handle,
			}
		}
	}
	return eventType, nil
}

func (portal *Portal) sendMessage(intent *appservice.IntentAPI, eventType event.Type, content interface{}, extraContent map[string]interface{}, timestamp int64) (*mautrix.RespSendEvent, error) {
	wrappedContent := &event.Content{Parsed: content}
	wrappedContent.Raw = extraContent
	var err error
	eventType, err = portal.encrypt(intent, wrappedContent, eventType)
	if err != nil {
		return nil, err
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

func (portal *Portal) sendErrorMessage(evt *event.Event, rootErr error, humanReadableError string, isCertain bool, checkpointStatus status.MessageCheckpointStatus, handle string) {
	portal.bridge.SendMessageCheckpoint(evt, status.MsgStepRemote, rootErr, checkpointStatus, 0)

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
		msgStatusCode := event.MessageStatusRetriable
		switch checkpointStatus {
		case status.MsgStatusUnsupported:
			reason = event.MessageStatusUnsupported
			msgStatusCode = event.MessageStatusFail
		case status.MsgStatusTimeout:
			reason = event.MessageStatusTooOld
		}

		content := event.BeeperMessageStatusEventContent{
			Network: portal.getBridgeInfoStateKey(),
			RelatesTo: event.RelatesTo{
				Type:    event.RelReference,
				EventID: evt.ID,
			},
			Reason:  reason,
			Status:  msgStatusCode,
			Error:   rootErr.Error(),
			Message: humanReadableError,
		}
		extraContent := map[string]any{}
		if handle != "" && portal.bridge.IM.Capabilities().ContactChatMerging {
			extraContent[bridgeInfoHandle] = handle
			content.MutateEventKey = bridgeInfoHandle
		}
		_, err := errorIntent.SendMessageEvent(portal.MXID, event.BeeperMessageStatus, &event.Content{
			Parsed: &content,
			Raw:    extraContent,
		})
		if err != nil {
			portal.log.Warnln("Failed to send message send status event:", err)
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

func (portal *Portal) sendDeliveryReceipt(eventID id.EventID, service, handle string, sendCheckpoint bool) {
	if portal.bridge.Config.Bridge.DeliveryReceipts {
		err := portal.bridge.Bot.MarkRead(portal.MXID, eventID)
		if err != nil {
			portal.log.Debugfln("Failed to send delivery receipt for %s: %v", eventID, err)
		}
	}

	if sendCheckpoint {
		portal.sendSuccessCheckpoint(eventID, service, handle)
	}
}

func (portal *Portal) sendSuccessCheckpoint(eventID id.EventID, service, handle string) {
	// We don't have access to the entire event, so we are omitting some
	// metadata here. However, that metadata can be inferred from previous
	// checkpoints.
	checkpoint := status.MessageCheckpoint{
		EventID:    eventID,
		RoomID:     portal.MXID,
		Step:       status.MsgStepRemote,
		Timestamp:  jsontime.UnixMilliNow(),
		Status:     status.MsgStatusSuccess,
		ReportedBy: status.MsgReportedByBridge,
	}
	go func() {
		portal.bridge.SendRawMessageCheckpoint(&checkpoint)
		if (portal.Identifier.IsGroup || portal.Identifier.Service == "SMS") && portal.bridge.IM.Capabilities().DeliveredStatus {
			portal.bridge.SendRawMessageCheckpoint(&status.MessageCheckpoint{
				EventID:    eventID,
				RoomID:     portal.MXID,
				Step:       status.MsgStepRemote,
				Timestamp:  jsontime.UnixMilliNow(),
				Status:     status.MsgStatusDelivered,
				ReportedBy: status.MsgReportedByBridge,
				Info:       "fake group delivered status",
			})
		}
	}()

	portal.sendSuccessMessageStatus(eventID, service, handle, []id.UserID{})
}

func (portal *Portal) sendSuccessMessageStatus(eventID id.EventID, service, handle string, deliveredTo []id.UserID) {
	if !portal.bridge.Config.Bridge.MessageStatusEvents {
		return
	}

	mainContent := &event.BeeperMessageStatusEventContent{
		Network: portal.getBridgeInfoStateKey(),
		RelatesTo: event.RelatesTo{
			Type:    event.RelReference,
			EventID: eventID,
		},
		Status: event.MessageStatusSuccess,
	}

	if !portal.Identifier.IsGroup && portal.Identifier.Service == "iMessage" && portal.bridge.IM.Capabilities().DeliveredStatus {
		// This is an iMessage DM, then we want to include the list of users
		// that the message has been delivered to.
		mainContent.DeliveredToUsers = &deliveredTo
	}

	var extraContent map[string]any
	if portal.bridge.IM.Capabilities().ContactChatMerging {
		extraContent = map[string]any{
			bridgeInfoService: service,
			bridgeInfoHandle:  handle,
		}
		mainContent.MutateEventKey = bridgeInfoHandle
	}
	content := &event.Content{
		Parsed: mainContent,
		Raw:    extraContent,
	}

	statusIntent := portal.bridge.Bot
	if !portal.Encrypted {
		statusIntent = portal.MainIntent()
	}
	_, err := statusIntent.SendMessageEvent(portal.MXID, event.BeeperMessageStatus, content)
	if err != nil {
		portal.log.Warnln("Failed to send message send status event:", err)
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

	return fmt.Errorf("message is too old (over %d seconds)", portal.bridge.Config.Bridge.MaxHandleSeconds)
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

func (portal *Portal) getTargetGUID(thing string, eventID id.EventID, targetGUID string) string {
	if portal.IsPrivateChat() && portal.bridge.IM.Capabilities().ContactChatMerging {
		if targetGUID != "" {
			portal.log.Debugfln("Sending Matrix %s %s to %s (target guid)", thing, eventID, targetGUID)
			return targetGUID
		} else if portal.LastSeenHandle != "" && portal.LastSeenHandle != portal.GUID {
			portal.log.Debugfln("Sending Matrix %s %s to %s (last seen handle)", thing, eventID, portal.LastSeenHandle)
			return portal.LastSeenHandle
		}
		portal.log.Debugfln("Sending Matrix %s %s to %s (portal guid)", thing, eventID, portal.GUID)
	}
	return portal.GUID
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
	replyToID := msg.RelatesTo.GetReplyTo()
	if len(replyToID) > 0 {
		imsg := portal.bridge.DB.Message.GetByMXID(replyToID)
		if imsg != nil {
			messageReplyID = imsg.GUID
			messageReplyPart = imsg.Part
		}
	}

	if err := portal.shouldHandleMessage(evt); err != nil {
		portal.log.Debug(err)
		portal.sendErrorMessage(evt, err, err.Error(), true, status.MsgStatusTimeout, "")
		return
	}

	editEventID := msg.RelatesTo.GetReplaceID()
	if editEventID != "" && msg.NewContent != nil {
		msg = msg.NewContent
	}

	var err error
	var resp *imessage.SendResponse
	var wasEdit bool

	if editEventID != "" {
		wasEdit = true
		if !portal.bridge.IM.Capabilities().EditMessages {
			portal.zlog.Err(errors.ErrUnsupported).Msg("Bridge doesn't support editing messages!")
			return
		}

		editedMessage := portal.bridge.DB.Message.GetByMXID(editEventID)
		if editedMessage == nil {
			portal.zlog.Error().Msg("Failed to get message by MXID")
			return
		}

		if portal.bridge.IM.(imessage.VenturaFeatures) != nil {
			resp, err = portal.bridge.IM.(imessage.VenturaFeatures).EditMessage(portal.getTargetGUID("message edit", evt.ID, editedMessage.HandleGUID), editedMessage.GUID, msg.Body, editedMessage.Part)
		} else {
			portal.zlog.Err(errors.ErrUnsupported).Msg("Bridge didn't implment EditMessage!")
			return
		}
	}

	var imessageRichLink *imessage.RichLink
	if portal.bridge.IM.Capabilities().RichLinks {
		imessageRichLink = portal.convertURLPreviewToIMessage(evt)
	}
	metadata, _ := evt.Content.Raw["com.beeper.message_metadata"].(imessage.MessageMetadata)

	if (msg.MsgType == event.MsgText || msg.MsgType == event.MsgNotice || msg.MsgType == event.MsgEmote) && !wasEdit {
		if evt.Sender != portal.bridge.user.MXID {
			portal.addRelaybotFormat(evt.Sender, msg)
			if len(msg.Body) == 0 {
				return
			}
		} else if msg.MsgType == event.MsgEmote {
			msg.Body = "/me " + msg.Body
		}
		portal.addDedup(evt.ID, msg.Body)
		resp, err = portal.bridge.IM.SendMessage(portal.getTargetGUID("text message", evt.ID, ""), msg.Body, messageReplyID, messageReplyPart, imessageRichLink, metadata)
	} else if len(msg.URL) > 0 || msg.File != nil {
		resp, err = portal.handleMatrixMedia(msg, evt, messageReplyID, messageReplyPart, metadata)
	}

	if err != nil {
		portal.log.Errorln("Error sending to iMessage:", err)
		statusCode := status.MsgStatusPermFailure
		certain := false
		if errors.Is(err, ipc.ErrSizeLimitExceeded) {
			certain = true
			statusCode = status.MsgStatusUnsupported
		}
		var ipcErr ipc.Error
		if errors.As(err, &ipcErr) {
			certain = true
			err = errors.New(ipcErr.Message)
			switch ipcErr.Code {
			case ipc.ErrUnsupportedError.Code:
				statusCode = status.MsgStatusUnsupported
			case ipc.ErrTimeoutError.Code:
				statusCode = status.MsgStatusTimeout
			}
		}
		portal.sendErrorMessage(evt, err, ipcErr.Message, certain, statusCode, "")
	} else if resp != nil {
		dbMessage := portal.bridge.DB.Message.New()
		dbMessage.PortalGUID = portal.GUID
		dbMessage.HandleGUID = resp.ChatGUID
		dbMessage.GUID = resp.GUID
		dbMessage.MXID = evt.ID
		dbMessage.Timestamp = resp.Time.UnixMilli()
		portal.sendDeliveryReceipt(evt.ID, resp.Service, resp.ChatGUID, !portal.bridge.IM.Capabilities().MessageStatusCheckpoints)
		dbMessage.Insert(nil)
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
		portal.sendErrorMessage(evt, fmt.Errorf("malformed attachment URL: %w", err), "malformed attachment URL", true, status.MsgStatusPermFailure, "")
		portal.log.Warnfln("Malformed content URI in %s: %v", evt.ID, err)
		return nil, nil
	}
	var caption string
	filename := msg.Body
	if msg.FileName != "" && msg.FileName != msg.Body {
		filename = msg.FileName
		caption = msg.Body
	}
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
			return portal.bridge.IM.SendMessage(portal.getTargetGUID("media message (+viewer)", evt.ID, ""), caption, messageReplyID, messageReplyPart, nil, metadata)
		}
	}

	return portal.handleMatrixMediaDirect(url, file, filename, caption, evt, messageReplyID, messageReplyPart, metadata)
}

func (portal *Portal) handleMatrixMediaDirect(url id.ContentURI, file *event.EncryptedFileInfo, filename, caption string, evt *event.Event, messageReplyID string, messageReplyPart int, metadata imessage.MessageMetadata) (resp *imessage.SendResponse, err error) {
	var data []byte
	data, err = portal.MainIntent().DownloadBytes(url)
	if err != nil {
		portal.sendErrorMessage(evt, fmt.Errorf("failed to download attachment: %w", err), "failed to download attachment", true, status.MsgStatusPermFailure, "")
		portal.log.Errorfln("Failed to download media in %s: %v", evt.ID, err)
		return
	}
	if file != nil {
		err = file.DecryptInPlace(data)
		if err != nil {
			portal.sendErrorMessage(evt, fmt.Errorf("failed to decrypt attachment: %w", err), "failed to decrypt attachment", true, status.MsgStatusPermFailure, "")
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
		filePath, err = ffmpeg.ConvertPath(context.TODO(), filePath, ".caf", []string{}, []string{"-c:a", "libopus"}, false)
		mimeType = "audio/x-caf"
		isVoiceMemo = true
		filename = filepath.Base(filePath)

		if err != nil {
			log.Errorfln("Failed to transcode voice message to CAF. Error: %w", err)
			return
		}
	}

	resp, err = portal.bridge.IM.SendFile(portal.getTargetGUID("media message", evt.ID, ""), caption, filename, filePath, messageReplyID, messageReplyPart, mimeType, isVoiceMemo, metadata)
	portal.bridge.IM.SendFileCleanup(dir)
	return
}

func (portal *Portal) sendUnsupportedCheckpoint(evt *event.Event, step status.MessageCheckpointStep, err error) {
	portal.log.Errorf("Sending unsupported checkpoint for %s: %+v", evt.ID, err)
	portal.bridge.SendMessageCheckpoint(evt, step, err, status.MsgStatusUnsupported, 0)

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

func (portal *Portal) HandleMatrixReadReceipt(user bridge.User, eventID id.EventID, receipt event.ReadReceipt) {
	if user.GetMXID() != portal.bridge.user.MXID {
		return
	}

	if message := portal.bridge.DB.Message.GetByMXID(eventID); message != nil {
		portal.log.Debugfln("Marking %s/%s as read", message.GUID, message.MXID)
		err := portal.bridge.IM.SendReadReceipt(portal.getTargetGUID("read receipt to message", eventID, message.HandleGUID), message.GUID)
		if err != nil {
			portal.log.Warnln("Error marking message as read:", err)
		}
	} else if tapback := portal.bridge.DB.Tapback.GetByMXID(eventID); tapback != nil {
		portal.log.Debugfln("Marking %s/%s as read in %s", tapback.GUID, tapback.MXID)
		err := portal.bridge.IM.SendReadReceipt(portal.getTargetGUID("read receipt to tapback", eventID, tapback.HandleGUID), tapback.GUID)
		if err != nil {
			portal.log.Warnln("Error marking tapback as read:", err)
		}
	}
}

const typingNotificationsTemporarilyDisabled = false

func (portal *Portal) HandleMatrixTyping(userIDs []id.UserID) {
	if portal.Identifier.Service == "SMS" {
		return
	} else if typingNotificationsTemporarilyDisabled {
		portal.log.Debugfln("Dropping typing notification %v", userIDs)
		return
	}
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
		err := portal.bridge.IM.SendTypingNotification(portal.getTargetGUID("typing notification", "", ""), isTyping)
		if err != nil {
			portal.log.Warnfln("Failed to bridge typing status change: %v", err)
		} else {
			portal.log.Debugfln("Typing update sent")
		}
	}
}

func (portal *Portal) HandleMatrixReaction(evt *event.Event) {
	if !portal.bridge.IM.Capabilities().SendTapbacks {
		portal.sendUnsupportedCheckpoint(evt, status.MsgStepRemote, errors.New("reactions are not supported"))
		return
	}
	portal.log.Debugln("Starting handling of Matrix reaction", evt.ID)

	if err := portal.shouldHandleMessage(evt); err != nil {
		portal.log.Debug(err)
		portal.sendErrorMessage(evt, err, err.Error(), true, status.MsgStatusTimeout, "")
		return
	}

	doError := func(msg string, args ...any) {
		portal.log.Errorfln(msg, args...)
		portal.bridge.SendMessageErrorCheckpoint(evt, status.MsgStepRemote, fmt.Errorf(msg, args...), true, 0)
	}

	if reaction, ok := evt.Content.Parsed.(*event.ReactionEventContent); !ok || reaction.RelatesTo.Type != event.RelAnnotation {
		doError("Ignoring reaction %s due to unknown m.relates_to data", evt.ID)
	} else if tapbackType := imessage.TapbackFromEmoji(reaction.RelatesTo.Key); tapbackType == 0 {
		doError("Unknown reaction type %s in %s", reaction.RelatesTo.Key, reaction.RelatesTo.EventID)
	} else if target := portal.bridge.DB.Message.GetByMXID(reaction.RelatesTo.EventID); target == nil {
		doError("Unknown reaction target %s", reaction.RelatesTo.EventID)
	} else if existing := portal.bridge.DB.Tapback.GetByGUID(portal.GUID, target.GUID, target.Part, ""); existing != nil && existing.Type == tapbackType {
		doError("Ignoring outgoing tapback to %s/%s: type is same", reaction.RelatesTo.EventID, target.GUID)
	} else {
		targetChatGUID := portal.getTargetGUID("reaction", evt.ID, target.HandleGUID)
		if resp, err := portal.bridge.IM.SendTapback(targetChatGUID, target.GUID, target.Part, tapbackType, false); err != nil {
			doError("Failed to send tapback %d to %s: %v", tapbackType, target.GUID, err)
		} else if existing == nil {
			// TODO should timestamp be stored?
			portal.log.Debugfln("Handled Matrix reaction %s into new iMessage tapback %s", evt.ID, resp.GUID)
			if !portal.bridge.IM.Capabilities().MessageStatusCheckpoints {
				portal.bridge.SendMessageSuccessCheckpoint(evt, status.MsgStepRemote, 0)
			}
			tapback := portal.bridge.DB.Tapback.New()
			tapback.PortalGUID = portal.GUID
			tapback.HandleGUID = resp.ChatGUID
			tapback.GUID = resp.GUID
			tapback.MessageGUID = target.GUID
			tapback.MessagePart = target.Part
			tapback.Type = tapbackType
			tapback.MXID = evt.ID
			tapback.Insert(nil)
		} else {
			portal.log.Debugfln("Handled Matrix reaction %s into iMessage tapback %s, replacing old %s", evt.ID, resp.GUID, existing.MXID)
			if !portal.bridge.IM.Capabilities().MessageStatusCheckpoints {
				portal.bridge.SendMessageSuccessCheckpoint(evt, status.MsgStepRemote, 0)
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
	}
}

func (portal *Portal) HandleMatrixRedaction(evt *event.Event) {
	if !portal.bridge.IM.Capabilities().SendTapbacks {
		portal.sendUnsupportedCheckpoint(evt, status.MsgStepRemote, errors.New("reactions are not supported"))
		return
	}

	if err := portal.shouldHandleMessage(evt); err != nil {
		portal.log.Debug(err)
		portal.sendErrorMessage(evt, err, err.Error(), true, status.MsgStatusTimeout, "")
		return
	}

	redactedTapback := portal.bridge.DB.Tapback.GetByMXID(evt.Redacts)
	if redactedTapback != nil {
		portal.log.Debugln("Starting handling of Matrix redaction of tapback", evt.ID)
		redactedTapback.Delete()
		_, err := portal.bridge.IM.SendTapback(portal.getTargetGUID("tapback redaction", evt.ID, redactedTapback.HandleGUID), redactedTapback.MessageGUID, redactedTapback.MessagePart, redactedTapback.Type, true)
		if err != nil {
			portal.log.Errorfln("Failed to send removal of tapback %d to %s/%d: %v", redactedTapback.Type, redactedTapback.MessageGUID, redactedTapback.MessagePart, err)
			portal.bridge.SendMessageErrorCheckpoint(evt, status.MsgStepRemote, err, true, 0)
		} else {
			portal.log.Debugfln("Handled Matrix redaction %s of iMessage tapback %d to %s/%d", evt.ID, redactedTapback.Type, redactedTapback.MessageGUID, redactedTapback.MessagePart)
			if !portal.bridge.IM.Capabilities().MessageStatusCheckpoints {
				portal.bridge.SendMessageSuccessCheckpoint(evt, status.MsgStepRemote, 0)
			}
		}
		return
	}

	if !portal.bridge.IM.Capabilities().UnsendMessages {
		portal.sendUnsupportedCheckpoint(evt, status.MsgStepRemote, errors.New("redactions of messages are not supported"))
		return
	}

	redactedText := portal.bridge.DB.Message.GetByMXID(evt.Redacts)
	if redactedText != nil {
		portal.log.Debugln("Starting handling of Matrix redaction of text", evt.ID)
		redactedText.Delete()

		var err error
		if portal.bridge.IM.(imessage.VenturaFeatures) != nil {
			_, err = portal.bridge.IM.(imessage.VenturaFeatures).UnsendMessage(portal.getTargetGUID("message redaction", evt.ID, redactedText.HandleGUID), redactedText.GUID, redactedText.Part)
		} else {
			portal.zlog.Err(errors.ErrUnsupported).Msg("Bridge didn't implment UnsendMessage!")
			return
		}

		//_, err := portal.bridge.IM.UnsendMessage(portal.getTargetGUID("message redaction", evt.ID, redactedText.HandleGUID), redactedText.GUID, redactedText.Part)
		if err != nil {
			portal.log.Errorfln("Failed to send unsend of message %s/%d: %v", redactedText.GUID, redactedText.Part, err)
			portal.bridge.SendMessageErrorCheckpoint(evt, status.MsgStepRemote, err, true, 0)
		} else {
			portal.log.Debugfln("Handled Matrix redaction %s of iMessage message %s/%d", evt.ID, redactedText.GUID, redactedText.Part)
			if !portal.bridge.IM.Capabilities().MessageStatusCheckpoints {
				portal.bridge.SendMessageSuccessCheckpoint(evt, status.MsgStepRemote, 0)
			}
		}
		return
	}
	portal.sendUnsupportedCheckpoint(evt, status.MsgStepRemote, fmt.Errorf("can't redact non-reaction event"))
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
		portal.Update(nil)
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
		dbMessage.Timestamp = msg.Time.UnixMilli()
		dbMessage.Insert(nil)
		portal.sendDeliveryReceipt(dbMessage.MXID, msg.Service, msg.ChatGUID, true)
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
		portal.zlog.Warn().Msg("Removing group avatars is not supported at this time")
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
		puppet.log.Warnfln("Failed to set membership to %s in %s: %v", membership, portal.MXID, err)
		if membership == event.MembershipJoin {
			_ = puppet.Intent.EnsureJoined(portal.MXID, appservice.EnsureJoinedParams{IgnoreCache: true})
		} else if membership == event.MembershipLeave {
			_, _ = portal.MainIntent().KickUser(portal.MXID, &mautrix.ReqKickUser{UserID: puppet.MXID})
		}
		return nil
	} else {
		portal.bridge.AS.StateStore.SetMembership(portal.MXID, puppet.MXID, "join")
		return &resp.EventID
	}
}

func (portal *Portal) handleIMMemberChange(msg *imessage.Message, dbMessage *database.Message, intent *appservice.IntentAPI) *id.EventID {
	if len(msg.Target.LocalID) == 0 {
		portal.log.Debugfln("Ignoring member change item with empty target")
		return nil
	}
	puppet := portal.bridge.GetPuppetByLocalID(msg.Target.LocalID)
	puppet.Sync()
	if msg.GroupActionType == imessage.GroupActionAddUser {
		return portal.setMembership(intent, puppet, event.MembershipJoin, dbMessage.Timestamp)
	} else if msg.GroupActionType == imessage.GroupActionRemoveUser {
		return portal.setMembership(intent, puppet, event.MembershipLeave, dbMessage.Timestamp)
	} else {
		portal.log.Warnfln("Unexpected group action type %d in member change item", msg.GroupActionType)
	}
	return nil
}

func (portal *Portal) convertIMAttachment(msg *imessage.Message, attach *imessage.Attachment, intent *appservice.IntentAPI) (*event.MessageEventContent, map[string]interface{}, error) {
	data, err := attach.Read()
	if err != nil {
		portal.log.Errorfln("Failed to read attachment in %s: %v", msg.GUID, err)
		return nil, nil, fmt.Errorf("failed to read attachment: %w", err)
	}
	if portal.bridge.Config.IMessage.DeleteMediaAfterUpload {
		defer func() {
			err = attach.Delete()
			if err != nil {
				portal.log.Warnfln("Failed to delete attachment in %s: %v", msg.GUID, err)
			}
		}()
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
		uploaded, err := intent.UploadAsync(req)
		if err != nil {
			portal.log.Errorfln("Failed to asynchronously upload attachment in %s: %v", msg.GUID, err)
			return nil, nil, err
		}
		mxc = uploaded.ContentURI
	} else {
		uploaded, err := intent.UploadMedia(req)
		if err != nil {
			portal.log.Errorfln("Failed to upload attachment in %s: %v", msg.GUID, err)
			return nil, nil, err
		}
		mxc = uploaded.ContentURI
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
	return &content, extraContent, nil
}

type ConvertedMessage struct {
	Type    event.Type
	Content *event.MessageEventContent
	Extra   map[string]any
}

func (portal *Portal) convertIMAttachments(msg *imessage.Message, intent *appservice.IntentAPI) []*ConvertedMessage {
	converted := make([]*ConvertedMessage, len(msg.Attachments))
	for index, attach := range msg.Attachments {
		portal.log.Debugfln("Converting iMessage attachment %s.%d", msg.GUID, index)
		content, extra, err := portal.convertIMAttachment(msg, attach, intent)
		if extra == nil {
			extra = map[string]interface{}{}
		}
		if err != nil {
			content = &event.MessageEventContent{
				MsgType: event.MsgNotice,
				Body:    err.Error(),
			}
		} else if msg.Metadata != nil {
			extra["com.beeper.message_metadata"] = msg.Metadata
		}
		converted[index] = &ConvertedMessage{
			Type:    event.EventMessage,
			Content: content,
			Extra:   extra,
		}
	}
	return converted
}

func (portal *Portal) convertIMText(msg *imessage.Message) *ConvertedMessage {
	msg.Text = strings.ReplaceAll(msg.Text, "\ufffc", "")
	msg.Subject = strings.ReplaceAll(msg.Subject, "\ufffc", "")
	if len(msg.Text) == 0 && len(msg.Subject) == 0 {
		return nil
	}
	content := &event.MessageEventContent{
		MsgType: event.MsgText,
		Body:    msg.Text,
	}
	if len(msg.Subject) > 0 {
		content.Format = event.FormatHTML
		content.FormattedBody = fmt.Sprintf("<strong>%s</strong><br>%s", event.TextToHTML(msg.Subject), event.TextToHTML(content.Body))
		content.Body = fmt.Sprintf("**%s**\n%s", msg.Subject, msg.Text)
	}
	extraAttrs := map[string]any{}
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
	return &ConvertedMessage{
		Type:    event.EventMessage,
		Content: content,
		Extra:   extraAttrs,
	}
}

func (portal *Portal) GetReplyEvent(msg *imessage.Message) (id.EventID, *event.Event) {
	if len(msg.ReplyToGUID) == 0 {
		return "", nil
	}
	message := portal.bridge.DB.Message.GetByGUID(portal.GUID, msg.ReplyToGUID, msg.ReplyToPart)
	if message != nil {
		evt, err := portal.MainIntent().GetEvent(portal.MXID, message.MXID)
		if err != nil {
			portal.log.Warnln("Failed to get reply target event:", err)
			return message.MXID, nil
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
		return message.MXID, evt
	} else if portal.bridge.Config.Homeserver.Software == bridgeconfig.SoftwareHungry {
		portal.log.Debugfln("Using deterministic event ID for unknown reply target %s.%d", msg.ReplyToGUID, msg.ReplyToPart)
		return portal.deterministicEventID(msg.ReplyToGUID, msg.ReplyToPart), nil
	} else {
		portal.log.Debugfln("Unknown reply target %s.%d", msg.ReplyToGUID, msg.ReplyToPart)
	}
	return "", nil
}

func (portal *Portal) addSourceMetadata(msg *imessage.Message, to map[string]any) {
	if portal.bridge.IM.Capabilities().ContactChatMerging {
		to[bridgeInfoService] = msg.Service
		to[bridgeInfoHandle] = msg.ChatGUID
	}
}

func (portal *Portal) convertiMessage(msg *imessage.Message, intent *appservice.IntentAPI) []*ConvertedMessage {
	attachments := portal.convertIMAttachments(msg, intent)
	text := portal.convertIMText(msg)
	if text != nil && len(attachments) == 1 && portal.bridge.Config.Bridge.CaptionInMessage {
		attach := attachments[0].Content
		attach.FileName = attach.Body
		attach.Body = text.Content.Body
		attach.Format = text.Content.Format
		attach.FormattedBody = text.Content.FormattedBody
	} else if text != nil {
		attachments = append(attachments, text)
	}
	for _, part := range attachments {
		portal.addSourceMetadata(msg, part.Extra)
	}
	if msg.ReplyToGUID != "" {
		replyToMXID, replyToEvt := portal.GetReplyEvent(msg)
		if replyToMXID != "" {
			msg.ReplyProcessed = true
			for _, part := range attachments {
				if replyToEvt != nil {
					part.Content.SetReply(replyToEvt)
				} else {
					part.Content.RelatesTo = (&event.RelatesTo{}).SetReplyTo(replyToMXID)
				}
			}
		}
	}
	return attachments
}

func (portal *Portal) handleNormaliMessage(msg *imessage.Message, dbMessage *database.Message, intent *appservice.IntentAPI, mxid *id.EventID) {
	if msg.Metadata != nil && portal.bridge.Config.HackyStartupTest.Key != "" {
		if portal.bridge.Config.HackyStartupTest.EchoMode {
			_, ok := msg.Metadata[startupTestKey].(map[string]any)
			if ok {
				go portal.bridge.receiveStartupTestPing(msg)
			}
		} else if portal.Identifier.LocalID == portal.bridge.Config.HackyStartupTest.Identifier {
			resp, ok := msg.Metadata[startupTestResponseKey].(map[string]any)
			if ok {
				go portal.bridge.receiveStartupTestPong(resp, msg)
			}
		}
	}

	parts := portal.convertiMessage(msg, intent)
	if len(parts) == 0 {
		portal.log.Warnfln("iMessage %s doesn't contain any attachments nor text", msg.GUID)
	}
	for index, converted := range parts {
		if mxid != nil {
			if len(parts) == 1 {
				converted.Content.SetEdit(*mxid)
			}
		}
		portal.log.Debugfln("Sending iMessage attachment %s.%d", msg.GUID, index)
		resp, err := portal.sendMessage(intent, converted.Type, converted.Content, converted.Extra, dbMessage.Timestamp)
		if err != nil {
			portal.log.Errorfln("Failed to send attachment %s.%d: %v", msg.GUID, index, err)
		} else {
			portal.log.Debugfln("Handled iMessage attachment %s.%d -> %s", msg.GUID, index, resp.EventID)
			if mxid == nil {
				dbMessage.MXID = resp.EventID
				dbMessage.Part = index
				dbMessage.Insert(nil)
				dbMessage.Part++
			}
		}
	}
}

func (portal *Portal) handleIMError(msg *imessage.Message, dbMessage *database.Message, intent *appservice.IntentAPI) {
	if len(msg.ErrorNotice) > 0 {
		content := &event.MessageEventContent{
			MsgType: event.MsgNotice,
			Body:    msg.ErrorNotice,
		}
		replyToMXID, replyToEvt := portal.GetReplyEvent(msg)
		if replyToEvt != nil {
			content.SetReply(replyToEvt)
		} else if replyToMXID != "" {
			content.RelatesTo = (&event.RelatesTo{}).SetReplyTo(replyToMXID)
		}
		extra := map[string]any{}
		portal.addSourceMetadata(msg, extra)
		resp, err := portal.sendMessage(intent, event.EventMessage, content, extra, dbMessage.Timestamp)
		if err != nil {
			portal.log.Errorfln("Failed to send error notice %s: %v", msg.GUID, err)
			return
		}
		portal.log.Debugfln("Handled iMessage error notice %s.%d -> %s", msg.GUID, dbMessage.Part, resp.EventID)
		dbMessage.MXID = resp.EventID
		dbMessage.Insert(nil)
		dbMessage.Part++
	}
}

func (portal *Portal) getIntentForMessage(msg *imessage.Message, dbMessage *database.Message) *appservice.IntentAPI {
	if msg.IsFromMe {
		intent := portal.bridge.user.DoublePuppetIntent
		if dbMessage != nil && portal.isDuplicate(dbMessage, msg) {
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

func (portal *Portal) HandleiMessage(msg *imessage.Message) id.EventID {
	var dbMessage *database.Message
	var overrideSuccess bool

	defer func() {
		if err := recover(); err != nil {
			portal.log.Errorfln("Panic while handling %s: %v\n%s", msg.GUID, err, string(debug.Stack()))
		}
		hasMXID := dbMessage != nil && len(dbMessage.MXID) > 0
		var eventID id.EventID
		if hasMXID {
			eventID = dbMessage.MXID
		}
		portal.bridge.IM.SendMessageBridgeResult(msg.ChatGUID, msg.GUID, eventID, overrideSuccess || hasMXID)
	}()

	// Look up the message in the database
	dbMessage = portal.bridge.DB.Message.GetLastByGUID(portal.GUID, msg.GUID)

	if portal.IsPrivateChat() && msg.ChatGUID != portal.LastSeenHandle {
		portal.log.Debugfln("Updating last seen handle from %s to %s", portal.LastSeenHandle, msg.ChatGUID)
		portal.LastSeenHandle = msg.ChatGUID
		portal.Update(nil)
	}

	// Handle message tapbacks
	if msg.Tapback != nil {
		portal.HandleiMessageTapback(msg)
		return ""
	}

	// If the message exists in the database, handle edits or retractions
	if dbMessage != nil && dbMessage.MXID != "" {
		// DEVNOTE: It seems sometimes the message is just edited to remove data instead of actually retracting it

		if msg.IsRetracted ||
			(len(msg.Attachments) == 0 && len(msg.Text) == 0 && len(msg.Subject) == 0) {

			// Retract existing message
			if portal.HandleMessageRevoke(*msg) {
				portal.zlog.Debug().Str("messageGUID", msg.GUID).Str("chatGUID", msg.ChatGUID).Msg("Revoked message")
			} else {
				portal.zlog.Warn().Str("messageGUID", msg.GUID).Str("chatGUID", msg.ChatGUID).Msg("Failed to revoke message")
			}

			overrideSuccess = true
		} else if msg.IsEdited && dbMessage.Part > 0 {

			// Edit existing message
			intent := portal.getIntentForMessage(msg, nil)
			portal.handleNormaliMessage(msg, dbMessage, intent, &dbMessage.MXID)

			overrideSuccess = true
		} else if msg.IsRead && msg.IsFromMe {

			// Send read receipt
			err := portal.markRead(portal.MainIntent(), dbMessage.MXID, msg.ReadAt)
			if err != nil {
				portal.log.Warnfln("Failed to send read receipt for %s: %v", dbMessage.MXID, err)
			}

			overrideSuccess = true
		} else {
			portal.log.Debugln("Ignoring duplicate message", msg.GUID)
			// Send a success confirmation since it's a duplicate message
			overrideSuccess = true
		}
		return ""
	}

	// If the message is not found in the database, proceed with handling as usual
	portal.log.Debugfln("Starting handling of iMessage %s (type: %d, attachments: %d, text: %d)", msg.GUID, msg.ItemType, len(msg.Attachments), len(msg.Text))
	dbMessage = portal.bridge.DB.Message.New()
	dbMessage.PortalGUID = portal.GUID
	dbMessage.HandleGUID = msg.ChatGUID
	dbMessage.SenderGUID = msg.Sender.String()
	dbMessage.GUID = msg.GUID
	dbMessage.Timestamp = msg.Time.UnixMilli()

	intent := portal.getIntentForMessage(msg, dbMessage)
	if intent == nil {
		portal.log.Debugln("Handling of iMessage", msg.GUID, "was cancelled (didn't get an intent)")
		return dbMessage.MXID
	}

	var groupUpdateEventID *id.EventID

	switch msg.ItemType {
	case imessage.ItemTypeMessage:
		portal.handleNormaliMessage(msg, dbMessage, intent, nil)
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
		dbMessage.Insert(nil)
	}

	if len(dbMessage.MXID) > 0 {
		portal.sendDeliveryReceipt(dbMessage.MXID, "", "", false)
		if !msg.IsFromMe && msg.IsRead {
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
	intent := portal.getIntentForMessage(msg, nil)
	if intent == nil {
		return
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

	resp, err := intent.SendMassagedMessageEvent(portal.MXID, event.EventReaction, &content, msg.Time.UnixMilli())

	if err != nil {
		portal.log.Errorfln("Failed to send tapback from %s: %v", msg.SenderText(), err)
		return
	}

	if existing == nil {
		tapback := portal.bridge.DB.Tapback.New()
		tapback.PortalGUID = portal.GUID
		tapback.MessageGUID = target.GUID
		tapback.MessagePart = target.Part
		tapback.SenderGUID = senderGUID
		tapback.HandleGUID = msg.ChatGUID
		tapback.GUID = msg.GUID
		tapback.Type = msg.Tapback.Type
		tapback.MXID = resp.EventID
		tapback.Insert(nil)
	} else {
		existing.GUID = msg.GUID
		existing.Type = msg.Tapback.Type
		existing.MXID = resp.EventID
		existing.HandleGUID = msg.ChatGUID
		existing.Update()
	}
}

func (portal *Portal) HandleMessageRevoke(msg imessage.Message) bool {
	dbMessage := portal.bridge.DB.Message.GetLastByGUID(portal.GUID, msg.GUID)
	if dbMessage == nil {
		return true
	}
	intent := portal.getIntentForMessage(&msg, nil)
	if intent == nil {
		return false
	}
	_, err := intent.RedactEvent(portal.MXID, dbMessage.MXID)
	if err != nil {
		if errors.Is(err, mautrix.MForbidden) {
			_, err = portal.MainIntent().RedactEvent(portal.MXID, dbMessage.MXID)
			if err != nil {
				portal.log.Errorln("Failed to redact %s: %v", msg.GUID, err)
			}
		}
	} else {
		dbMessage.Delete()
	}
	return true
}

func (portal *Portal) Delete() {
	portal.bridge.portalsLock.Lock()
	portal.unlockedDelete()
	portal.bridge.portalsLock.Unlock()
}

func (portal *Portal) unlockedDelete() {
	portal.Portal.Delete()
	delete(portal.bridge.portalsByGUID, portal.GUID)
	for _, guid := range portal.SecondaryGUIDs {
		if storedPortal := portal.bridge.portalsByGUID[guid]; storedPortal == portal {
			portal.bridge.portalsByGUID[guid] = nil
		}
	}
	if len(portal.MXID) > 0 {
		delete(portal.bridge.portalsByMXID, portal.MXID)
	}
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
	intent := portal.MainIntent()
	if portal.bridge.SpecVersions.UnstableFeatures["com.beeper.room_yeeting"] {
		err := intent.BeeperDeleteRoom(portal.MXID)
		if err != nil && !errors.Is(err, mautrix.MNotFound) {
			portal.zlog.Err(err).Msg("Failed to delete room using hungryserv yeet endpoint")
		}
		return
	}
	if portal.IsPrivateChat() {
		_, err := intent.LeaveRoom(portal.MXID)
		if err != nil {
			portal.log.Warnln("Failed to leave private chat portal with main intent:", err)
		}
		return
	}
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
