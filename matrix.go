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
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"maunium.net/go/maulogger/v2"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-imessage/imessage"
)

const DefaultSyncProxyBackoff = 1 * time.Second
const MaxSyncProxyBackoff = 60 * time.Second

type MatrixHandler struct {
	bridge *Bridge
	as     *appservice.AppService
	log    maulogger.Logger
	//cmd    *CommandHandler
	errorTxnIDC *appservice.TransactionIDCache

	lastSyncProxyError time.Time
	syncProxyBackoff   time.Duration
	syncProxyWaiting   int64
}

func NewMatrixHandler(bridge *Bridge) *MatrixHandler {
	handler := &MatrixHandler{
		bridge: bridge,
		as:     bridge.AS,
		log:    bridge.Log.Sub("Matrix"),
		//cmd:    NewCommandHandler(bridge),
		errorTxnIDC:      appservice.NewTransactionIDCache(8),
		syncProxyBackoff: DefaultSyncProxyBackoff,
	}
	bridge.EventProcessor.On(event.EventMessage, handler.HandleMessage)
	bridge.EventProcessor.On(event.EventReaction, handler.HandleReaction)
	bridge.EventProcessor.On(event.EventEncrypted, handler.HandleEncrypted)
	bridge.EventProcessor.On(event.EventSticker, handler.HandleMessage)
	bridge.EventProcessor.On(event.EventRedaction, handler.HandleRedaction)
	bridge.EventProcessor.On(event.StateMember, handler.HandleMembership)
	bridge.EventProcessor.On(event.StateEncryption, handler.HandleEncryption)
	bridge.AS.SetWebsocketCommandHandler("ping", handler.handleWSPing)
	bridge.AS.SetWebsocketCommandHandler("syncproxy_error", handler.handleWSSyncProxyError)
	return handler
}

func (mx *MatrixHandler) handleWSPing(cmd appservice.WebsocketCommand) (bool, interface{}) {
	var status imessage.BridgeStatus

	if mx.bridge.latestState != nil {
		status = *mx.bridge.latestState
	} else {
		status = imessage.BridgeStatus{
			StateEvent: BridgeStatusConnected,
			Timestamp:  time.Now().Unix(),
			TTL:        600,
			Source:     "bridge",
		}
	}

	return true, &status
}

func (mx *MatrixHandler) handleWSSyncProxyError(cmd appservice.WebsocketCommand) (bool, interface{}) {
	var data mautrix.RespError
	err := json.Unmarshal(cmd.Data, &data)

	if err != nil {
		mx.log.Warnln("Failed to unmarshal syncproxy_error data:", err)
	} else if txnID, ok := data.ExtraData["txn_id"].(string); !ok {
		mx.log.Warnln("Got syncproxy_error data with no transaction ID")
	} else if mx.errorTxnIDC.IsProcessed(txnID) {
		mx.log.Debugln("Ignoring syncproxy_error with duplicate transaction ID", txnID)
	} else {
		go mx.HandleSyncProxyError(&data, nil)
		mx.errorTxnIDC.MarkProcessed(txnID)
	}

	return true, &data
}

func (mx *MatrixHandler) HandleSyncProxyError(syncErr *mautrix.RespError, startErr error) {
	if !atomic.CompareAndSwapInt64(&mx.syncProxyWaiting, 0, 1) {
		var err interface{} = startErr
		if err == nil {
			err = syncErr.Err
		}
		mx.log.Debugfln("Got sync proxy error (%v), but there's already another thread waiting to restart sync proxy", err)
		return
	}
	if time.Now().Sub(mx.lastSyncProxyError) < MaxSyncProxyBackoff {
		mx.syncProxyBackoff *= 2
		if mx.syncProxyBackoff > MaxSyncProxyBackoff {
			mx.syncProxyBackoff = MaxSyncProxyBackoff
		}
	} else {
		mx.syncProxyBackoff = DefaultSyncProxyBackoff
	}
	mx.lastSyncProxyError = time.Now()
	if syncErr != nil {
		mx.log.Errorfln("Syncproxy told us that syncing failed: %s - Requesting a restart in %s", syncErr.Err, mx.syncProxyBackoff)
	} else if startErr != nil {
		mx.log.Errorfln("Failed to request sync proxy to start syncing: %v - Requesting a restart in %s", startErr, mx.syncProxyBackoff)
	}
	time.Sleep(mx.syncProxyBackoff)
	atomic.StoreInt64(&mx.syncProxyWaiting, 0)
	mx.bridge.requestStartSync()
}

func (mx *MatrixHandler) HandleEncryption(evt *event.Event) {
	if evt.Content.AsEncryption().Algorithm != id.AlgorithmMegolmV1 {
		return
	}
	portal := mx.bridge.GetPortalByMXID(evt.RoomID)
	if portal != nil && !portal.Encrypted {
		mx.log.Debugfln("%s enabled encryption in %s", evt.Sender, evt.RoomID)
		portal.Encrypted = true
		portal.Update()
	}
}

func (mx *MatrixHandler) joinAndCheckMembers(evt *event.Event, intent *appservice.IntentAPI) *mautrix.RespJoinedMembers {
	resp, err := intent.JoinRoomByID(evt.RoomID)
	if err != nil {
		mx.log.Debugfln("Failed to join room %s as %s with invite from %s: %v", evt.RoomID, intent.UserID, evt.Sender, err)
		return nil
	}

	members, err := intent.JoinedMembers(resp.RoomID)
	if err != nil {
		mx.log.Debugfln("Failed to get members in room %s after accepting invite from %s as %s: %v", resp.RoomID, evt.Sender, intent.UserID, err)
		_, _ = intent.LeaveRoom(resp.RoomID)
		return nil
	}

	if len(members.Joined) < 2 {
		mx.log.Debugln("Leaving empty room", resp.RoomID, "after accepting invite from", evt.Sender, "as", intent.UserID)
		_, _ = intent.LeaveRoom(resp.RoomID)
		return nil
	}
	return members
}

func (mx *MatrixHandler) HandleBotInvite(evt *event.Event) {
	intent := mx.as.BotIntent()

	if evt.Sender != mx.bridge.user.MXID {
		return
	}

	members := mx.joinAndCheckMembers(evt, intent)
	if members == nil {
		return
	}

	hasPuppets := false
	for mxid, _ := range members.Joined {
		if mxid == intent.UserID || mxid == evt.Sender {
			continue
		} else if _, ok := mx.bridge.ParsePuppetMXID(mxid); ok {
			hasPuppets = true
			continue
		}
		mx.log.Debugln("Leaving multi-user room", evt.RoomID, "after accepting invite from", evt.Sender)
		_, _ = intent.SendNotice(evt.RoomID, "This bridge is user-specific, please don't invite me into rooms with other users.")
		_, _ = intent.LeaveRoom(evt.RoomID)
		return
	}

	if !hasPuppets && (len(mx.bridge.user.ManagementRoom) == 0 || evt.Content.AsMember().IsDirect) {
		mx.bridge.user.SetManagementRoom(evt.RoomID)
		_, _ = intent.SendNotice(mx.bridge.user.ManagementRoom, "This room has been registered as your bridge management/status room. Don't send `help` to get a list of commands, because this bridge doesn't support commands yet.")
		mx.log.Debugln(evt.RoomID, "registered as a management room with", evt.Sender)
	}
}

func (mx *MatrixHandler) HandleMembership(evt *event.Event) {
	if _, isPuppet := mx.bridge.ParsePuppetMXID(evt.Sender); evt.Sender == mx.bridge.Bot.UserID || isPuppet {
		return
	}

	if mx.bridge.Crypto != nil {
		mx.bridge.Crypto.HandleMemberEvent(evt)
	}

	content := evt.Content.AsMember()
	if content.Membership == event.MembershipInvite && id.UserID(evt.GetStateKey()) == mx.as.BotMXID() {
		mx.HandleBotInvite(evt)
		return
	} else if content.Membership == event.MembershipLeave {
		portal := mx.bridge.GetPortalByMXID(evt.RoomID)
		if portal != nil {
			mx.log.Debugfln("Got leave event of %s in %s, checking if it needs to be cleaned up", evt.GetStateKey(), evt.RoomID)
			portal.CleanupIfEmpty(true)
		}
	}

	// TODO handle puppet invites to create chats?
}

func (mx *MatrixHandler) shouldIgnoreEvent(evt *event.Event) bool {
	if evt.Sender != mx.bridge.user.MXID {
		return true
	}
	isCustomPuppet, ok := evt.Content.Raw["net.maunium.imessage.puppet"].(bool)
	if ok && isCustomPuppet {
		return true
	}
	return false
}

const sessionWaitTimeout = 5 * time.Second

func (mx *MatrixHandler) HandleEncrypted(evt *event.Event) {
	if mx.shouldIgnoreEvent(evt) || mx.bridge.Crypto == nil {
		return
	}

	decrypted, err := mx.bridge.Crypto.Decrypt(evt)
	if errors.Is(err, NoSessionFound) {
		content := evt.Content.AsEncrypted()
		mx.log.Debugfln("Couldn't find session %s trying to decrypt %s, waiting %d seconds...", content.SessionID, evt.ID, int(sessionWaitTimeout.Seconds()))
		if mx.bridge.Crypto.WaitForSession(evt.RoomID, content.SenderKey, content.SessionID, sessionWaitTimeout) {
			mx.log.Debugfln("Got session %s after waiting, trying to decrypt %s again", content.SessionID, evt.ID)
			decrypted, err = mx.bridge.Crypto.Decrypt(evt)
		} else {
			go mx.waitLongerForSession(evt)
			return
		}
	}
	if err != nil {
		mx.log.Warnfln("Failed to decrypt %s: %v", evt.ID, err)
		_, _ = mx.bridge.Bot.SendNotice(evt.RoomID, fmt.Sprintf(
			"\u26a0 Your message was not bridged: %v", err))
		return
	}
	mx.bridge.EventProcessor.Dispatch(decrypted)
}

func (mx *MatrixHandler) waitLongerForSession(evt *event.Event) {
	const extendedTimeout = sessionWaitTimeout * 2

	content := evt.Content.AsEncrypted()
	mx.log.Debugfln("Couldn't find session %s trying to decrypt %s, waiting %d more seconds...",
		content.SessionID, evt.ID, int(extendedTimeout.Seconds()))

	resp, err := mx.bridge.Bot.SendNotice(evt.RoomID, fmt.Sprintf(
		"\u26a0 Your message was not bridged: the bridge hasn't received the decryption keys. "+
			"The bridge will retry for %d seconds. If this error keeps happening, try restarting your client.",
		int(extendedTimeout.Seconds())))
	if err != nil {
		mx.log.Errorfln("Failed to send decryption error to %s: %v", evt.RoomID, err)
	}
	update := event.MessageEventContent{MsgType: event.MsgNotice}

	if mx.bridge.Crypto.WaitForSession(evt.RoomID, content.SenderKey, content.SessionID, extendedTimeout) {
		mx.log.Debugfln("Got session %s after waiting more, trying to decrypt %s again", content.SessionID, evt.ID)
		var decrypted *event.Event
		decrypted, err = mx.bridge.Crypto.Decrypt(evt)
		if err == nil {
			mx.bridge.EventProcessor.Dispatch(decrypted)
			_, _ = mx.bridge.Bot.RedactEvent(evt.RoomID, resp.EventID)
			return
		}
		mx.log.Warnfln("Failed to decrypt %s: %v", evt.ID, err)
		update.Body = fmt.Sprintf("\u26a0 Your message was not bridged: %v", err)
	} else {
		mx.log.Debugfln("Didn't get %s, giving up on %s", content.SessionID, evt.ID)
		update.Body = "\u26a0 Your message was not bridged: the bridge hasn't received the decryption keys. " +
			"If this keeps happening, try restarting your client."
	}

	newContent := update
	update.NewContent = &newContent
	if resp != nil {
		update.RelatesTo = &event.RelatesTo{
			Type:    event.RelReplace,
			EventID: resp.EventID,
		}
	}
	_, err = mx.bridge.Bot.SendMessageEvent(evt.RoomID, event.EventMessage, &update)
	if err != nil {
		mx.log.Debugfln("Failed to update decryption error notice %s: %v", resp.EventID, err)
	}
}

func (mx *MatrixHandler) HandleMessage(evt *event.Event) {
	if mx.shouldIgnoreEvent(evt) {
		return
	}

	content := evt.Content.AsMessage()
	if content.MsgType == event.MsgText {
		commandPrefix := mx.bridge.Config.Bridge.CommandPrefix
		hasCommandPrefix := strings.HasPrefix(content.Body, commandPrefix)
		if hasCommandPrefix {
			content.Body = strings.TrimLeft(content.Body[len(commandPrefix):], " ")
		}
		if hasCommandPrefix || evt.RoomID == mx.bridge.user.ManagementRoom {
			// TODO uncomment after commands exist
			//mx.cmd.Handle(evt.RoomID, mx.bridge.user, content.Body)
			return
		}
	}

	portal := mx.bridge.GetPortalByMXID(evt.RoomID)
	if portal != nil {
		portal.HandleMatrixMessage(evt)
	}
}

func (mx *MatrixHandler) HandleReaction(evt *event.Event) {
	if mx.shouldIgnoreEvent(evt) {
		return
	}

	portal := mx.bridge.GetPortalByMXID(evt.RoomID)
	if portal != nil {
		portal.HandleMatrixReaction(evt)
	}
}

func (mx *MatrixHandler) HandleRedaction(evt *event.Event) {
	if mx.shouldIgnoreEvent(evt) {
		return
	}

	portal := mx.bridge.GetPortalByMXID(evt.RoomID)
	if portal != nil {
		portal.HandleMatrixRedaction(evt)
	}
}
