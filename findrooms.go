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
func (br *IMBridge) FindPortalsFromMatrix() error {
	br.Log.Infoln("Finding portal rooms from Matrix...")
	resp, err := br.Bot.JoinedRooms()
	if err != nil {
		return fmt.Errorf("failed to get joined rooms: %w", err)
	}
	foundPortals := 0
	for _, roomID := range resp.JoinedRooms {
		var roomState mautrix.RoomStateMap
		// Bypass IntentAPI here, we don't want it to try to join any rooms.
		roomState, err = br.Bot.Client.State(roomID)
		if errors.Is(err, mautrix.MNotFound) || errors.Is(err, mautrix.MForbidden) {
			// Expected error, just debug log and skip
			br.Log.Debugfln("Skipping %s: failed to get room state (%v)", roomID, err)
		} else if err != nil {
			// Unexpected error, log warning
			br.Log.Warnfln("Skipping %s: failed to get room state (%v)", roomID, err)
		} else if br.findPortal(roomID, roomState) {
			foundPortals++
		}
	}
	br.Log.Infofln("Portal finding completed, found %d portals", foundPortals)
	return nil
}

func (br *IMBridge) findPortal(roomID id.RoomID, state mautrix.RoomStateMap) bool {
	if existingPortal := br.GetPortalByMXID(roomID); existingPortal != nil {
		br.Log.Debugfln("Skipping %s: room is already a registered portal", roomID)
	} else if bridgeInfo, err := br.findBridgeInfo(state); err != nil {
		br.Log.Debugfln("Skipping %s: %s", roomID, err)
	} else if err = br.checkMembers(state); err != nil {
		br.Log.Debugfln("Skipping %s (to %s): %s", roomID, bridgeInfo.Channel.GUID, err)
	} else if portal := br.GetPortalByGUID(bridgeInfo.Channel.GUID); len(portal.MXID) > 0 {
		br.Log.Debugfln("Skipping %s (to %s): portal to chat already exists (%s)", roomID, portal.GUID, portal.MXID)
	} else {
		encryptionEvent, ok := state[event.StateEncryption][""]
		isEncrypted := ok && encryptionEvent.Content.AsEncryption().Algorithm == id.AlgorithmMegolmV1
		if !isEncrypted && br.Config.Bridge.Encryption.Default {
			br.Log.Debugfln("Skipping %s (to %s): room is not encrypted, but encryption is enabled by default", roomID, portal.GUID)
			return false
		}

		portal.MXID = roomID
		portal.Name = bridgeInfo.Channel.DisplayName
		portal.AvatarURL = bridgeInfo.Channel.AvatarURL.ParseOrIgnore()
		portal.Encrypted = isEncrypted
		// TODO find last message timestamp somewhere
		portal.BackfillStartTS = time.Now().UnixNano() / int64(time.Millisecond)
		portal.Update()
		br.Log.Infofln("Found portal %s to %s", roomID, portal.GUID)
		return true
	}
	return false
}

func (br *IMBridge) checkMembers(state mautrix.RoomStateMap) error {
	members, ok := state[event.StateMember]
	if !ok {
		return errors.New("didn't find member list")
	}
	bridgeBotMember, ok := members[br.Bot.UserID.String()]
	if !ok || bridgeBotMember.Content.AsMember().Membership != event.MembershipJoin {
		return fmt.Errorf("bridge bot %s is not joined", br.Bot.UserID)
	}
	userMember, ok := members[br.user.MXID.String()]
	if !ok || userMember.Content.AsMember().Membership != event.MembershipJoin {
		return fmt.Errorf("user %s is not joined", br.user.MXID)
	}
	return nil
}

func (br *IMBridge) findBridgeInfo(state mautrix.RoomStateMap) (*CustomBridgeInfoContent, error) {
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
			br.isBridgeOwnedMXID(evt.Sender) &&
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
		br.Log.Debugfln("Content of %s: %s", foundEvt.ID, foundEvt.Content.VeryRaw)
		return nil, fmt.Errorf("bridge info event %s has unexpected content (%T)", foundEvt.ID, foundEvt.Content.Parsed)
	} else if content.BridgeBot != br.Bot.UserID {
		return nil, fmt.Errorf("bridge info event %s has unexpected bridge bot (%s)", foundEvt.ID, content.BridgeBot)
	} else if len(content.Channel.GUID) == 0 {
		return nil, fmt.Errorf("bridge info event %s is missing the iMessage chat GUID", foundEvt.ID)
	}
	return content, nil
}

func (br *IMBridge) isBridgeOwnedMXID(userID id.UserID) bool {
	if userID == br.Bot.UserID {
		return true
	}
	return br.IsGhost(userID)
}
