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
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/rs/zerolog"
	log "maunium.net/go/maulogger/v2"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/bridge"
	"maunium.net/go/mautrix/bridge/bridgeconfig"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-imessage/database"
)

type User struct {
	*database.User

	bridge *IMBridge
	log    log.Logger
	zlog   zerolog.Logger
	DoublePuppetIntent *appservice.IntentAPI

	mgmtCreateLock sync.Mutex

	spaceMembershipChecked bool

	BackfillQueue  *BackfillQueue
	backfillStatus BackfillStatus
	backfillError  error
}

var _ bridge.User = (*User)(nil)

func (user *User) GetPermissionLevel() bridgeconfig.PermissionLevel {
	if user == user.bridge.user {
		return bridgeconfig.PermissionLevelAdmin
	} else if user.bridge.Config.Bridge.Relay.IsWhitelisted(user.MXID) {
		return bridgeconfig.PermissionLevelRelay
	}
	return bridgeconfig.PermissionLevelBlock
}

func (user *User) IsLoggedIn() bool {
	return user == user.bridge.user
}

func (user *User) GetManagementRoomID() id.RoomID {
	return user.ManagementRoom
}

func (user *User) SetManagementRoom(roomID id.RoomID) {
	user.ManagementRoom = roomID
	user.Update()
}

func (user *User) GetMXID() id.UserID {
	return user.MXID
}

func (user *User) GetCommandState() map[string]interface{} {
	return nil
}

func (user *User) GetIDoublePuppet() bridge.DoublePuppet {
	if user == user.bridge.user {
		return user
	}
	return nil
}

func (user *User) GetIGhost() bridge.Ghost {
	return nil
}

func (br *IMBridge) loadDBUser() *User {
	dbUser := br.DB.User.GetByMXID(br.Config.Bridge.User)
	if dbUser == nil {
		dbUser = br.DB.User.New()
		dbUser.MXID = br.Config.Bridge.User
		dbUser.Insert()
	}
	user := br.NewUser(dbUser)
	return user
}

func (br *IMBridge) NewUser(dbUser *database.User) *User {
	user := &User{
		User:   dbUser,
		bridge: br,
		log:    br.Log.Sub("User").Sub(string(dbUser.MXID)),
	}

	return user
}

func (user *User) getDirectChats() map[id.UserID][]id.RoomID {
	res := make(map[id.UserID][]id.RoomID)
	privateChats := user.bridge.DB.Portal.FindPrivateChats()
	for _, portal := range privateChats {
		if len(portal.MXID) > 0 {
			// TODO Make FormatPuppetMXID work with chat GUIDs or add a field with the sender ID to portals
			res[user.bridge.FormatPuppetMXID(portal.GUID)] = []id.RoomID{portal.MXID}
		}
	}
	return res
}

func (user *User) UpdateDirectChats(chats map[id.UserID][]id.RoomID) {
	if !user.bridge.Config.Bridge.SyncDirectChatList || user.DoublePuppetIntent == nil {
		return
	}
	method := http.MethodPatch
	if chats == nil {
		chats = user.getDirectChats()
		method = http.MethodPut
	}
	user.log.Debugln("Updating m.direct list on homeserver")
	var err error
	if user.bridge.Config.Homeserver.Software == "asmux" {
		url := user.DoublePuppetIntent.BuildClientURL("unstable", "com.beeper.asmux", "dms")
		_, err = user.DoublePuppetIntent.MakeFullRequest(mautrix.FullRequest{
			Method:      method,
			URL:         url,
			RequestJSON: chats,
			Headers:     http.Header{"X-Asmux-Auth": {user.bridge.AS.Registration.AppToken}},
		})
	} else {
		existingChats := make(map[id.UserID][]id.RoomID)
		err = user.DoublePuppetIntent.GetAccountData(event.AccountDataDirectChats.Type, &existingChats)
		if err != nil {
			user.log.Warnln("Failed to get m.direct list to update it:", err)
			return
		}
		for userID, rooms := range existingChats {
			if _, ok := user.bridge.ParsePuppetMXID(userID); !ok {
				// This is not a ghost user, include it in the new list
				chats[userID] = rooms
			} else if _, ok := chats[userID]; !ok && method == http.MethodPatch {
				// This is a ghost user, but we're not replacing the whole list, so include it too
				chats[userID] = rooms
			}
		}
		err = user.DoublePuppetIntent.SetAccountData(event.AccountDataDirectChats.Type, &chats)
	}
	if err != nil {
		user.log.Warnln("Failed to update m.direct list:", err)
	}
}

func (user *User) ensureInvited(intent *appservice.IntentAPI, roomID id.RoomID, isDirect bool) (ok bool) {
	extraContent := map[string]interface{}{}
	if isDirect {
		extraContent["is_direct"] = true
	}
	if user.DoublePuppetIntent != nil {
		extraContent["fi.mau.will_auto_accept"] = true
	}
	_, err := intent.InviteUser(roomID, &mautrix.ReqInviteUser{UserID: user.MXID}, extraContent)
	var httpErr mautrix.HTTPError
	if err != nil && errors.As(err, &httpErr) && httpErr.RespError != nil && strings.Contains(httpErr.RespError.Err, "is already in the room") {
		user.bridge.StateStore.SetMembership(roomID, user.MXID, event.MembershipJoin)
		ok = true
		return
	} else if err != nil {
		user.log.Warnfln("Failed to invite user to %s: %v", roomID, err)
	} else {
		ok = true
	}

	if user.DoublePuppetIntent != nil {
		err = user.DoublePuppetIntent.EnsureJoined(roomID)
		if err != nil {
			user.log.Warnfln("Failed to auto-join %s as %s: %v", roomID, user.MXID, err)
		}
	}
	return
}

func (user *User) GetSpaceRoom() id.RoomID {
	if !user.bridge.Config.Bridge.PersonalFilteringSpaces {
		return ""
	}

	if len(user.SpaceRoom) == 0 {
		name := "iMessage"
		if user.bridge.Config.IMessage.Platform == "android" {
			name = "Android SMS"
		}
		resp, err := user.bridge.Bot.CreateRoom(&mautrix.ReqCreateRoom{
			Visibility: "private",
			Name:       name,
			Topic:      fmt.Sprintf("Your %s bridged chats", name),
			InitialState: []*event.Event{{
				Type: event.StateRoomAvatar,
				Content: event.Content{
					Parsed: &event.RoomAvatarEventContent{
						URL: user.bridge.Config.AppService.Bot.ParsedAvatar,
					},
				},
			}},
			CreationContent: map[string]interface{}{
				"type": event.RoomTypeSpace,
			},
			PowerLevelOverride: &event.PowerLevelsEventContent{
				Users: map[id.UserID]int{
					user.bridge.Bot.UserID: 9001,
					user.MXID:              100,
				},
			},
		})

		if err != nil {
			user.log.Errorln("Failed to auto-create space room:", err)
		} else {
			user.SpaceRoom = resp.RoomID
			user.Update()
			user.ensureInvited(user.bridge.Bot, user.SpaceRoom, false)
		}
	} else if !user.spaceMembershipChecked && !user.bridge.StateStore.IsInRoom(user.SpaceRoom, user.MXID) {
		user.ensureInvited(user.bridge.Bot, user.SpaceRoom, false)
	}
	user.spaceMembershipChecked = true

	return user.SpaceRoom
}
