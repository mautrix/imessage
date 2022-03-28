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
	"fmt"
	"net/http"
	"sync"

	log "maunium.net/go/maulogger/v2"
	"maunium.net/go/mautrix/appservice"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-imessage/database"
)

type User struct {
	*database.User

	bridge *Bridge
	log    log.Logger

	DoublePuppetIntent *appservice.IntentAPI

	Admin            bool
	RelayWhitelisted bool

	mgmtCreateLock sync.Mutex

	customTypingIn map[id.RoomID]bool
}

func (bridge *Bridge) loadDBUser() *User {
	dbUser := bridge.DB.User.GetByMXID(bridge.Config.Bridge.User)
	if dbUser == nil {
		dbUser = bridge.DB.User.New()
		dbUser.MXID = bridge.Config.Bridge.User
		dbUser.Insert()
	}
	user := bridge.NewUser(dbUser)
	return user
}

func (bridge *Bridge) NewUser(dbUser *database.User) *User {
	user := &User{
		User:   dbUser,
		bridge: bridge,
		log:    bridge.Log.Sub("User").Sub(string(dbUser.MXID)),
	}

	user.RelayWhitelisted = user.bridge.Config.Bridge.Permissions.IsRelayWhitelisted(user.MXID)
	user.Admin = user.bridge.Config.Bridge.Permissions.IsAdmin(user.MXID)
	//delete
	user.log.Debugln("user is admin", user.Admin, " ", user.bridge.Config.Bridge.Permissions.IsAdmin(user.MXID))
	return user
}

func (user *User) GetManagementRoom() id.RoomID {
	if len(user.ManagementRoom) == 0 {
		user.mgmtCreateLock.Lock()
		defer user.mgmtCreateLock.Unlock()
		if len(user.ManagementRoom) > 0 {
			return user.ManagementRoom
		}
		creationContent := make(map[string]interface{})
		if !user.bridge.Config.Bridge.FederateRooms {
			creationContent["m.federate"] = false
		}
		resp, err := user.bridge.Bot.CreateRoom(&mautrix.ReqCreateRoom{
			Topic:           "iMessage bridge notices",
			IsDirect:        true,
			CreationContent: creationContent,
		})
		if err != nil {
			user.log.Errorln("Failed to auto-create management room:", err)
		} else {
			user.SetManagementRoom(resp.RoomID)
		}
	}
	return user.ManagementRoom
}

func (user *User) SetManagementRoom(roomID id.RoomID) {
	user.ManagementRoom = roomID
	user.Update()
}

func (user *User) sendBridgeNotice(formatString string, args ...interface{}) {
	notice := fmt.Sprintf(formatString, args...)
	_, err := user.bridge.Bot.SendNotice(user.GetManagementRoom(), notice)
	if err != nil {
		user.log.Warnf("Failed to send bridge notice \"%s\": %v", notice, err)
	}
}

func (user *User) sendMarkdownBridgeAlert(formatString string, args ...interface{}) {
	notice := fmt.Sprintf(formatString, args...)
	content := format.RenderMarkdown(notice, true, false)
	_, err := user.bridge.Bot.SendMessageEvent(user.GetManagementRoom(), event.EventMessage, content)
	if err != nil {
		user.log.Warnf("Failed to send bridge alert \"%s\": %v", notice, err)
	}
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
	if user.bridge.Config.Homeserver.Asmux {
		url := user.DoublePuppetIntent.BuildBaseURL("_matrix", "client", "unstable", "com.beeper.asmux", "dms")
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
