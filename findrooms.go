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
	"errors"
	"fmt"
	"strings"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// FindPortalsFromMatrix finds portal rooms that the bridge bot is in and puts them in the local database.
func (bridge *Bridge) FindPortalsFromMatrix() error {
	bridge.Log.Infoln("Finding portal rooms from Matrix...")
	resp, err := bridge.Bot.JoinedRooms()
	if err != nil {
		return fmt.Errorf("failed to get joined rooms: %w", err)
	}
	foundPortals := 0
	for _, roomID := range resp.JoinedRooms {
		var roomState mautrix.RoomStateMap
		// Bypass IntentAPI here, we don't want it to try to join any rooms.
		roomState, err = bridge.Bot.Client.State(roomID)
		if errors.Is(err, mautrix.MNotFound) || errors.Is(err, mautrix.MForbidden) {
			// Expected error, just debug log and skip
			bridge.Log.Debugfln("Skipping %s: failed to get room state (%v)", roomID, err)
		} else if err != nil {
			// Unexpected error, log warning
			bridge.Log.Warnfln("Skipping %s: failed to get room state (%v)", roomID, err)
		} else if bridge.findPortal(roomID, roomState) {
			foundPortals++
		}
	}
	bridge.Log.Infofln("Portal finding completed, found %d portals", foundPortals)
	return nil
}

func (bridge *Bridge) findPortal(roomID id.RoomID, state mautrix.RoomStateMap) bool {
	if existingPortal := bridge.GetPortalByMXID(roomID); existingPortal != nil {
		bridge.Log.Debugfln("Skipping %s: room is already a registered portal", roomID)
	} else if bridgeInfo, err := bridge.findBridgeInfo(state); err != nil {
		bridge.Log.Debugfln("Skipping %s: %s", roomID, err)
	} else if err = bridge.checkMembers(state); err != nil {
		bridge.Log.Debugfln("Skipping %s (to %s): %s", roomID, bridgeInfo.Channel.GUID, err)
	} else if portal := bridge.GetPortalByGUID(bridgeInfo.Channel.GUID); len(portal.MXID) > 0 {
		bridge.Log.Debugfln("Skipping %s (to %s): portal to chat already exists (%s)", roomID, portal.GUID, portal.MXID)
	} else {
		portal.MXID = roomID
		portal.Name = bridgeInfo.Channel.DisplayName
		portal.AvatarURL = bridgeInfo.Channel.AvatarURL.ParseOrIgnore()
		encryptionEvent, ok := state[event.StateEncryption][""]
		portal.Encrypted = ok && encryptionEvent.Content.AsEncryption().Algorithm == id.AlgorithmMegolmV1
		// TODO find last message timestamp somewhere
		portal.BackfillStartTS = time.Now().UnixNano() / int64(time.Millisecond)
		portal.Update()
		bridge.Log.Infofln("Found portal %s to %s", roomID, portal.GUID)
		return true
	}
	return false
}

func (bridge *Bridge) checkMembers(state mautrix.RoomStateMap) error {
	members, ok := state[event.StateMember]
	if !ok {
		return errors.New("didn't find member list")
	}
	bridgeBotMember, ok := members[bridge.Bot.UserID.String()]
	if !ok || bridgeBotMember.Content.AsMember().Membership != event.MembershipJoin {
		return fmt.Errorf("bridge bot %s is not joined", bridge.Bot.UserID)
	}
	userMember, ok := members[bridge.user.MXID.String()]
	if !ok || userMember.Content.AsMember().Membership != event.MembershipJoin {
		return fmt.Errorf("user %s is not joined", bridge.user.MXID)
	}
	return nil
}

func (bridge *Bridge) findBridgeInfo(state mautrix.RoomStateMap) (*CustomBridgeInfoContent, error) {
	evts, ok := state[event.StateBridge]
	if !ok {
		return nil, errors.New("no bridge info events found")
	}
	var highestTs int64
	var foundEvt *event.Event
	for _, evt := range evts {
		// Check that the state key is somewhat expected
		if strings.HasPrefix(*evt.StateKey, bridgeInfoProto+"://") &&
			// Must be sent by the bridge bot or a ghost user
			bridge.isBridgeOwnedMXID(evt.Sender) &&
			// Theoretically we might want to change the state key, so get the one with the highest timestamp
			evt.Timestamp > highestTs {

			foundEvt = evt
			highestTs = evt.Timestamp
		}
	}
	if foundEvt == nil {
		return nil, errors.New("no valid bridge info event found")
	}
	content, ok := foundEvt.Content.Parsed.(*CustomBridgeInfoContent)
	if !ok {
		bridge.Log.Debugfln("Content of %s: %s", foundEvt.ID, foundEvt.Content.VeryRaw)
		return nil, fmt.Errorf("bridge info event %s has unexpected content (%T)", foundEvt.ID, foundEvt.Content.Parsed)
	} else if content.BridgeBot != bridge.Bot.UserID {
		return nil, fmt.Errorf("bridge info event %s has unexpected bridge bot (%s)", foundEvt.ID, content.BridgeBot)
	} else if len(content.Channel.GUID) == 0 {
		return nil, fmt.Errorf("bridge info event %s is missing the iMessage chat GUID", foundEvt.ID)
	}
	return content, nil
}

func (bridge *Bridge) isBridgeOwnedMXID(userID id.UserID) bool {
	if userID == bridge.Bot.UserID {
		return true
	}
	_, isPuppet := bridge.ParsePuppetMXID(userID)
	return isPuppet
}
