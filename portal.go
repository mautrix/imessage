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
	"crypto/sha256"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	log "maunium.net/go/maulogger/v2"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/crypto/attachment"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/pushrules"

	"go.mau.fi/mautrix-imessage/database"
	"go.mau.fi/mautrix-imessage/imessage"
)

func (bridge *Bridge) GetPortalByMXID(mxid id.RoomID) *Portal {
	bridge.portalsLock.Lock()
	defer bridge.portalsLock.Unlock()
	portal, ok := bridge.portalsByMXID[mxid]
	if !ok {
		return bridge.loadDBPortal(bridge.DB.Portal.GetByMXID(mxid), "")
	}
	return portal
}

func (bridge *Bridge) GetPortalByGUID(guid string) *Portal {
	bridge.portalsLock.Lock()
	defer bridge.portalsLock.Unlock()
	portal, ok := bridge.portalsByGUID[guid]
	if !ok {
		return bridge.loadDBPortal(bridge.DB.Portal.GetByGUID(guid), guid)
	}
	return portal
}

func (bridge *Bridge) ReIDPortal(oldGUID, newGUID string) bool {
	bridge.portalsLock.Lock()
	defer bridge.portalsLock.Unlock()

	portal, ok := bridge.portalsByGUID[oldGUID]
	if !ok {
		portal = bridge.loadDBPortal(bridge.DB.Portal.GetByGUID(oldGUID), "")
		if portal == nil {
			bridge.Log.Debugfln("Ignoring chat ID change %s->%s, no portal with old ID found", oldGUID, newGUID)
			return false
		}
	}

	newPortal, ok := bridge.portalsByGUID[newGUID]
	if !ok {
		newPortal = bridge.loadDBPortal(bridge.DB.Portal.GetByGUID(newGUID), "")
	}
	if newPortal != nil {
		bridge.Log.Warnfln("Ignoring chat ID change %s->%s, portal with new ID already exists", oldGUID, newGUID)
		return false
	}

	portal.log.Infoln("Changing chat ID to", newGUID)
	delete(bridge.portalsByGUID, portal.GUID)
	portal.Portal.ReID(newGUID)
	portal.Identifier = imessage.ParseIdentifier(portal.GUID)
	portal.log = portal.bridge.Log.Sub(fmt.Sprintf("Portal/%s", portal.GUID))
	bridge.portalsByGUID[portal.GUID] = portal
	if len(portal.MXID) > 0 {
		portal.UpdateBridgeInfo()
	}
	portal.log.Debugln("Chat ID changed successfully")
	return true
}

func (bridge *Bridge) GetAllPortals() []*Portal {
	return bridge.dbPortalsToPortals(bridge.DB.Portal.GetAll())
}

func (bridge *Bridge) dbPortalsToPortals(dbPortals []*database.Portal) []*Portal {
	bridge.portalsLock.Lock()
	defer bridge.portalsLock.Unlock()
	output := make([]*Portal, len(dbPortals))
	for index, dbPortal := range dbPortals {
		if dbPortal == nil {
			continue
		}
		portal, ok := bridge.portalsByGUID[dbPortal.GUID]
		if !ok {
			portal = bridge.loadDBPortal(dbPortal, "")
		}
		output[index] = portal
	}
	return output
}

func (bridge *Bridge) loadDBPortal(dbPortal *database.Portal, guid string) *Portal {
	if dbPortal == nil {
		if guid == "" {
			return nil
		}
		dbPortal = bridge.DB.Portal.New()
		dbPortal.GUID = guid
		dbPortal.Insert()
	}
	portal := bridge.NewPortal(dbPortal)
	bridge.portalsByGUID[portal.GUID] = portal
	if len(portal.MXID) > 0 {
		bridge.portalsByMXID[portal.MXID] = portal
	}
	return portal
}

func (bridge *Bridge) NewPortal(dbPortal *database.Portal) *Portal {
	portal := &Portal{
		Portal: dbPortal,
		bridge: bridge,
		log:    bridge.Log.Sub(fmt.Sprintf("Portal/%s", dbPortal.GUID)),

		Identifier:    imessage.ParseIdentifier(dbPortal.GUID),
		Messages:      make(chan *imessage.Message, 100),
		backfillStart: make(chan struct{}),
	}
	if !bridge.IM.Capabilities().MessageSendResponses {
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

	bridge *Bridge
	log    log.Logger

	Messages         chan *imessage.Message
	backfillStart    chan struct{}
	backfillWait     sync.WaitGroup
	backfillLock     sync.Mutex
	roomCreateLock   sync.Mutex
	messageDedup     map[string]SentMessage
	messageDedupLock sync.Mutex
	Identifier       imessage.Identifier
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

func (portal *Portal) SyncWithInfo(chatInfo *imessage.ChatInfo) {
	update := false
	if len(chatInfo.DisplayName) > 0 {
		update = portal.UpdateName(chatInfo.DisplayName, nil) != nil || update
	}
	portal.SyncParticipants(chatInfo)
	if update {
		portal.Update()
		portal.UpdateBridgeInfo()
	}
}

func (portal *Portal) ensureUserInvited(user *User) {
	inviteContent := event.Content{
		Parsed: &event.MemberEventContent{
			Membership: event.MembershipInvite,
			IsDirect:   portal.IsPrivateChat(),
		},
		Raw: map[string]interface{}{},
	}
	if user.DoublePuppetIntent != nil {
		inviteContent.Raw["fi.mau.will_auto_accept"] = true
	}
	_, err := portal.MainIntent().SendStateEvent(portal.MXID, event.StateMember, user.MXID.String(), &inviteContent)
	var httpErr mautrix.HTTPError
	if err != nil && errors.As(err, &httpErr) && httpErr.RespError != nil && strings.Contains(httpErr.RespError.Err, "is already in the room") {
		portal.bridge.StateStore.SetMembership(portal.MXID, user.MXID, event.MembershipJoin)
	} else if err != nil {
		portal.log.Warnfln("Failed to invite %s: %v", user.MXID, err)
	}

	if user.DoublePuppetIntent != nil {
		err = user.DoublePuppetIntent.EnsureJoined(portal.MXID)
		if err != nil {
			portal.log.Warnfln("Failed to auto-join portal as %s: %v", user.MXID, err)
		}
	}
}

func (portal *Portal) Sync(backfill bool) {
	if len(portal.MXID) == 0 {
		portal.log.Infoln("Creating Matrix room due to sync")
		err := portal.CreateMatrixRoom(nil)
		if err != nil {
			portal.log.Errorln("Failed to create portal room:", err)
		}
		return
	}

	portal.ensureUserInvited(portal.bridge.user)

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

func (portal *Portal) handleMessageLoop() {
	portal.log.Debugln("Starting message processing loop")
	for {
		select {
		case msg := <-portal.Messages:
			portal.HandleiMessage(msg, false)
		case <-portal.backfillStart:
			portal.log.Debugln("Backfill lock enabled, stopping new message processing")
			portal.backfillWait.Wait()
			portal.log.Debugln("Continuing new message processing")
		}
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
		if len(lastReadEvent) > 0 && portal.bridge.user.DoublePuppetIntent != nil {
			err = portal.bridge.user.DoublePuppetIntent.MarkRead(portal.MXID, lastReadEvent)
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
	bridgeInfoStateKey := fmt.Sprintf("%s://%s/%s",
		bridgeInfoProto, strings.ToLower(portal.Identifier.Service), portal.GUID)
	return bridgeInfoStateKey, bridgeInfo
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

func (portal *Portal) CreateMatrixRoom(chatInfo *imessage.ChatInfo) error {
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
		if err != nil {
			// If there's no chat info, the chat probably doesn't exist and we shouldn't auto-create a Matrix room for it.
			// TODO if we want a `pm` command or something, this check won't work
			return fmt.Errorf("failed to get chat info: %w", err)
		}
	}
	if chatInfo != nil {
		portal.Name = chatInfo.DisplayName
	} else {
		portal.log.Warnln("Didn't get any chat info")
	}

	if portal.IsPrivateChat() {
		puppet := portal.bridge.GetPuppetByLocalID(portal.Identifier.LocalID)
		puppet.Sync()
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
				Parsed: event.EncryptionEventContent{Algorithm: id.AlgorithmMegolmV1},
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
	return nil
}

func (portal *Portal) IsPrivateChat() bool {
	return !portal.Identifier.IsGroup
}

func (portal *Portal) MainIntent() *appservice.IntentAPI {
	if portal.IsPrivateChat() {
		return portal.bridge.GetPuppetByLocalID(portal.Identifier.LocalID).Intent
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
	return portal.sendMessage(portal.MainIntent(), event.EventMessage, content, 0)
}

func (portal *Portal) sendMessage(intent *appservice.IntentAPI, eventType event.Type, content interface{}, timestamp int64) (*mautrix.RespSendEvent, error) {
	wrappedContent := event.Content{Parsed: content}
	if timestamp != 0 && intent.IsCustomPuppet {
		wrappedContent.Raw = map[string]interface{}{
			"net.maunium.imessage.puppet": intent.IsCustomPuppet,
		}
	}
	if portal.Encrypted && portal.bridge.Crypto != nil {
		encrypted, err := portal.bridge.Crypto.Encrypt(portal.MXID, eventType, wrappedContent)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt event: %w", err)
		}
		eventType = event.EventEncrypted
		wrappedContent.Parsed = encrypted
	}
	_, _ = intent.UserTyping(portal.MXID, false, 0)
	if timestamp == 0 {
		return intent.SendMessageEvent(portal.MXID, eventType, &wrappedContent)
	} else {
		return intent.SendMassagedMessageEvent(portal.MXID, eventType, &wrappedContent, timestamp)
	}
}

func (portal *Portal) encryptFile(data []byte, mimeType string) ([]byte, string, *event.EncryptedFileInfo) {
	if !portal.Encrypted {
		return data, mimeType, nil
	}

	file := &event.EncryptedFileInfo{
		EncryptedFile: *attachment.NewEncryptedFile(),
		URL:           "",
	}
	return file.Encrypt(data), "application/octet-stream", file
}

func (portal *Portal) sendErrorMessage(message string, isCertain bool) id.EventID {
	possibility := "may not have been"
	if isCertain {
		possibility = "was not"
	}
	resp, err := portal.sendMainIntentMessage(event.MessageEventContent{
		MsgType: event.MsgNotice,
		Body:    fmt.Sprintf("\u26a0 Your message %s bridged: %v", possibility, message),
	})
	if err != nil {
		portal.log.Warnfln("Failed to send bridging error message:", err)
		return ""
	}
	return resp.EventID
}

func (portal *Portal) sendDeliveryReceipt(eventID id.EventID) {
	if portal.bridge.Config.Bridge.DeliveryReceipts {
		err := portal.bridge.Bot.MarkRead(portal.MXID, eventID)
		if err != nil {
			portal.log.Debugfln("Failed to send delivery receipt for %s: %v", eventID, err)
		}
	}
}

func (portal *Portal) HandleMatrixMessage(evt *event.Event) {
	msg, ok := evt.Content.Parsed.(*event.MessageEventContent)
	if !ok {
		// TODO log
		return
	}
	portal.log.Debugln("Starting handling Matrix message", evt.ID)
	if portal.messageDedup != nil {
		portal.messageDedupLock.Lock()
		portal.messageDedup[strings.TrimSpace(msg.Body)] = SentMessage{
			EventID: evt.ID,
			// Set the timestamp to a bit before now to make sure the deduplication catches it properly
			Timestamp: time.Now().Add(-10 * time.Second),
		}
		portal.messageDedupLock.Unlock()
	}

	var messageReplyID string
	var messageReplyPart int
	replyToID := msg.GetReplyTo()
	if len(replyToID) > 0 {
		msg.RemoveReplyFallback()
		imsg := portal.bridge.DB.Message.GetByMXID(replyToID)
		if imsg != nil {
			messageReplyID = imsg.GUID
			messageReplyPart = imsg.Part
		}
	}

	var err error
	var resp *imessage.SendResponse
	if msg.MsgType == event.MsgText {
		resp, err = portal.bridge.IM.SendMessage(portal.GUID, msg.Body, messageReplyID, messageReplyPart)
	} else if len(msg.URL) > 0 || msg.File != nil {
		var data []byte
		var url id.ContentURI
		if msg.File != nil {
			url, err = msg.File.URL.Parse()
		} else {
			url, err = msg.URL.Parse()
		}
		if err != nil {
			portal.sendErrorMessage(fmt.Sprintf("malformed attachment URL: %v", err), true)
			portal.log.Warnfln("Malformed content URI in %s: %v", evt.ID, err)
			return
		}
		data, err = portal.MainIntent().DownloadBytes(url)
		if err != nil {
			portal.sendErrorMessage(fmt.Sprintf("failed to download attachment: %v", err), true)
			portal.log.Errorfln("Failed to download media in %s: %v", evt.ID, err)
			return
		}
		if msg.File != nil {
			data, err = msg.File.Decrypt(data)
			if err != nil {
				portal.sendErrorMessage(fmt.Sprintf("failed to decrypt attachment: %v", err), true)
				portal.log.Errorfln("Failed to decrypt media in %s: %v", evt.ID, err)
				return
			}
		}
		resp, err = portal.bridge.IM.SendFile(portal.GUID, msg.Body, data, messageReplyID, messageReplyPart)
	}
	if err != nil {
		portal.log.Errorln("Error sending to iMessage:", err)
		portal.sendErrorMessage(err.Error(), false)
	} else if resp != nil {
		dbMessage := portal.bridge.DB.Message.New()
		dbMessage.ChatGUID = portal.GUID
		dbMessage.GUID = resp.GUID
		dbMessage.MXID = evt.ID
		dbMessage.Timestamp = resp.Time.UnixNano() / 1e6
		portal.sendDeliveryReceipt(evt.ID)
		dbMessage.Insert()
		portal.log.Debugln("Handled Matrix message", evt.ID, "->", resp.GUID)
	} else {
		portal.log.Debugln("Handled Matrix message", evt.ID, "(waiting for echo)")
	}
}

func (portal *Portal) HandleMatrixReaction(evt *event.Event) {
	if !portal.bridge.IM.Capabilities().SendTapbacks {
		return
	}
	portal.log.Debugln("Starting handling of Matrix reaction", evt.ID)
	if reaction, ok := evt.Content.Parsed.(*event.ReactionEventContent); !ok || reaction.RelatesTo.Type != event.RelAnnotation {
		portal.log.Debugfln("Ignoring reaction %s due to unknown m.relates_to data", evt.ID)
	} else if tapbackType := imessage.TapbackFromEmoji(reaction.RelatesTo.Key); tapbackType == 0 {
		portal.log.Debugfln("Unknown reaction type %s in %s", reaction.RelatesTo.Key, reaction.RelatesTo.EventID)
	} else if target := portal.bridge.DB.Message.GetByMXID(reaction.RelatesTo.EventID); target == nil {
		portal.log.Debugfln("Unknown reaction target %s", reaction.RelatesTo.EventID)
	} else if existing := portal.bridge.DB.Tapback.GetByGUID(portal.GUID, target.GUID, target.Part, ""); existing != nil && existing.Type == tapbackType {
		portal.log.Debugfln("Ignoring outgoing tapback to %s/%s: type is same", reaction.RelatesTo.EventID, target.GUID)
	} else if resp, err := portal.bridge.IM.SendTapback(portal.GUID, target.GUID, target.Part, tapbackType, false); err != nil {
		portal.log.Errorfln("Failed to send tapback %d to %s: %v", tapbackType, target.GUID, err)
	} else if existing == nil {
		// TODO should tapback GUID and timestamp be stored?
		portal.log.Debugfln("Handled Matrix reaction %s into new iMessage tapback %s", evt.ID, resp.GUID)
		tapback := portal.bridge.DB.Tapback.New()
		tapback.ChatGUID = portal.GUID
		tapback.MessageGUID = target.GUID
		tapback.MessagePart = target.Part
		tapback.Type = tapbackType
		tapback.MXID = evt.ID
		tapback.Insert()
	} else {
		portal.log.Debugfln("Handled Matrix reaction %s into iMessage tapback %s, replacing old %s", evt.ID, resp.GUID, existing.MXID)
		_, err = portal.MainIntent().RedactEvent(portal.MXID, existing.MXID)
		if err != nil {
			portal.log.Warnfln("Failed to redact old tapback %s to %s: %v", existing.MXID, target.MXID, err)
		}
		existing.Type = tapbackType
		existing.MXID = evt.ID
		existing.Update()
	}
}

func (portal *Portal) HandleMatrixRedaction(evt *event.Event) {
	redactedTapback := portal.bridge.DB.Tapback.GetByMXID(evt.Redacts)
	if redactedTapback != nil {
		portal.log.Debugln("Starting handling of Matrix redaction", evt.ID)
		redactedTapback.Delete()
		_, err := portal.bridge.IM.SendTapback(portal.GUID, redactedTapback.MessageGUID, redactedTapback.MessagePart, redactedTapback.Type, true)
		if err != nil {
			portal.log.Errorfln("Failed to send removal of tapback %d to %s/%d: %v", redactedTapback.Type, redactedTapback.MessageGUID, redactedTapback.MessagePart, err)
		} else {
			portal.log.Debugfln("Handled Matrix redaction %s of iMessage tapback %d to %s/%d", evt.ID, redactedTapback.Type, redactedTapback.MessageGUID, redactedTapback.MessagePart)
		}
	}
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
		portal.sendDeliveryReceipt(dbMessage.MXID)
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

func (portal *Portal) handleIMAttachment(msg *imessage.Message, attach *imessage.Attachment, intent *appservice.IntentAPI) (*event.MessageEventContent, error) {
	data, err := attach.Read()
	if err != nil {
		portal.log.Errorfln("Failed to read attachment in %s: %v", msg.GUID, err)
		return nil, fmt.Errorf("failed to read attachment: %w", err)
	}
	data, uploadMime, uploadInfo := portal.encryptFile(data, attach.GetMimeType())
	uploadResp, err := intent.UploadBytes(data, uploadMime)
	if err != nil {
		portal.log.Errorfln("Failed to upload attachment in %s: %v", msg.GUID, err)
		return nil, fmt.Errorf("failed to re-upload attachment")
	}
	var content event.MessageEventContent
	if uploadInfo != nil {
		uploadInfo.URL = uploadResp.ContentURI.CUString()
		content.File = uploadInfo
	} else {
		content.URL = uploadResp.ContentURI.CUString()
	}
	content.Body = attach.FileName
	content.Info = &event.FileInfo{
		MimeType: attach.GetMimeType(),
		Size:     len(data),
	}
	switch strings.Split(attach.GetMimeType(), "/")[0] {
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
	return &content, nil
}

func (portal *Portal) handleIMAttachments(msg *imessage.Message, dbMessage *database.Message, intent *appservice.IntentAPI) {
	if msg.Attachments == nil {
		return
	}
	for index, attach := range msg.Attachments {
		mediaContent, err := portal.handleIMAttachment(msg, attach, intent)
		var resp *mautrix.RespSendEvent
		if err != nil {
			// Errors are already logged in handleIMAttachment so no need to log here, just send to Matrix room.
			resp, err = portal.sendMessage(intent, event.EventMessage, &event.MessageEventContent{
				MsgType: event.MsgNotice,
				Body:    err.Error(),
			}, dbMessage.Timestamp)
		} else {
			resp, err = portal.sendMessage(intent, event.EventMessage, &mediaContent, dbMessage.Timestamp)
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
	if len(msg.Text) > 0 {
		content := &event.MessageEventContent{
			MsgType: event.MsgText,
			Body:    msg.Text,
		}
		portal.SetReply(content, msg)
		resp, err := portal.sendMessage(intent, event.EventMessage, content, dbMessage.Timestamp)
		if err != nil {
			portal.log.Errorfln("Failed to send message %s: %v", msg.GUID, err)
			return
		}
		portal.log.Debugfln("Handled iMessage text %s.%d -> %s", msg.GUID, dbMessage.Part, resp.EventID)
		dbMessage.MXID = resp.EventID
		dbMessage.Insert()
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
		puppet := portal.bridge.GetPuppetByLocalID(msg.Sender.LocalID)
		if len(puppet.Displayname) == 0 {
			portal.log.Debugfln("Displayname of %s is empty, syncing before handling %s", puppet.ID, msg.GUID)
			puppet.Sync()
		}
		return puppet.Intent
	}
	return portal.MainIntent()
}

func (portal *Portal) HandleiMessage(msg *imessage.Message, isBackfill bool) id.EventID {
	defer func() {
		if err := recover(); err != nil {
			portal.log.Errorfln("Panic while handling %s: %v\n%s", msg.GUID, err, string(debug.Stack()))
		}
	}()

	if msg.Tapback != nil {
		portal.HandleiMessageTapback(msg)
		return ""
	} else if portal.bridge.DB.Message.GetLastByGUID(portal.GUID, msg.GUID) != nil {
		portal.log.Debugln("Ignoring duplicate message", msg.GUID)
		return ""
	}

	portal.log.Debugln("Starting handling of iMessage", msg.GUID)
	dbMessage := portal.bridge.DB.Message.New()
	dbMessage.ChatGUID = portal.GUID
	dbMessage.SenderGUID = msg.Sender.String()
	dbMessage.GUID = msg.GUID
	dbMessage.Timestamp = msg.Time.UnixNano() / int64(time.Millisecond)

	intent := portal.getIntentForMessage(msg, dbMessage)
	if intent == nil {
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
	default:
		portal.log.Debugfln("Dropping message %s with unknown item type %d", msg.GUID, msg.ItemType)
		return ""
	}

	if groupUpdateEventID != nil {
		dbMessage.MXID = *groupUpdateEventID
		dbMessage.Insert()
	}

	if len(dbMessage.MXID) > 0 {
		portal.sendDeliveryReceipt(dbMessage.MXID)
		if !isBackfill && !msg.IsFromMe && msg.IsRead && portal.bridge.user.DoublePuppetIntent != nil {
			err := portal.bridge.user.DoublePuppetIntent.MarkRead(portal.MXID, dbMessage.MXID)
			if err != nil {
				portal.log.Warnln("Failed to mark %s as read after bridging: %v", dbMessage.MXID, err)
			}
		}
	} else {
		portal.log.Debugfln("Unhandled message %s (%d attachments, %d characters of text)", msg.GUID, len(msg.Attachments), len(msg.Text))
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

	resp, err := intent.SendReaction(portal.MXID, target.MXID, msg.Tapback.Type.Emoji())
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
		tapback.Type = msg.Tapback.Type
		tapback.MXID = resp.EventID
		tapback.Insert()
	} else {
		_, err = intent.RedactEvent(portal.MXID, existing.MXID)
		if err != nil {
			portal.log.Warnfln("Failed to redact old tapback from %s: %v", msg.SenderText(), err)
		}
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
