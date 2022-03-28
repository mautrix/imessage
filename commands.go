// mautrix-whatsapp - A Matrix-WhatsApp puppeting bridge.
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
	// "context"
	// "encoding/json"
	// "errors"
	"fmt"
	// "html"
	// "math"
	// "sort"
	"strconv"
	"strings"

	"maunium.net/go/maulogger/v2"

	// "maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"
)

type CommandHandler struct {
	bridge *Bridge
	log    maulogger.Logger
}

// NewCommandHandler creates a CommandHandler
func NewCommandHandler(bridge *Bridge) *CommandHandler {
	return &CommandHandler{
		bridge: bridge,
		log:    bridge.Log.Sub("Command handler"),
	}
}

// CommandEvent stores all data which might be used to handle commands
type CommandEvent struct {
	Bot     *appservice.IntentAPI
	Bridge  *Bridge
	Portal  *Portal
	Handler *CommandHandler
	RoomID  id.RoomID
	EventID id.EventID
	User    *User
	Command string
	Args    []string
	ReplyTo id.EventID
}

// Reply sends a reply to command as notice
func (ce *CommandEvent) Reply(msg string, args ...interface{}) {
	content := format.RenderMarkdown(fmt.Sprintf(msg, args...), true, false)
	content.MsgType = event.MsgNotice
	intent := ce.Bot
	if ce.Portal != nil && ce.Portal.IsPrivateChat() {
		intent = ce.Portal.MainIntent()
	}
	_, err := intent.SendMessageEvent(ce.RoomID, event.EventMessage, content)
	if err != nil {
		ce.Handler.log.Warnfln("Failed to reply to command from %s: %v", ce.User.MXID, err)
	}
}

// Handle handles messages to the bridge
func (handler *CommandHandler) Handle(roomID id.RoomID, eventID id.EventID, user *User, message string, replyTo id.EventID) {
	args := strings.Fields(message)
	if len(args) == 0 {
		args = []string{"unknown-command"}
	}
	ce := &CommandEvent{
		Bot:     handler.bridge.Bot,
		Bridge:  handler.bridge,
		Portal:  handler.bridge.GetPortalByMXID(roomID),
		Handler: handler,
		RoomID:  roomID,
		EventID: eventID,
		User:    user,
		Command: strings.ToLower(args[0]),
		Args:    args[1:],
		ReplyTo: replyTo,
	}
	handler.log.Debugfln("%s sent '%s' in %s", user.MXID, message, roomID)
	handler.CommandMux(ce)
}

func (handler *CommandHandler) CommandMux(ce *CommandEvent) {
	switch ce.Command {
	case "help":
		handler.CommandHelp(ce)
	case "version":
		handler.CommandVersion(ce)
	// case "delete-portal":
	// 	handler.CommandDeletePortal(ce)
	// case "delete-all-portals":
	// 	handler.CommandDeleteAllPortals(ce)
	// case "discard-megolm-session", "discard-session":
	// 	handler.CommandDiscardMegolmSession(ce)
	// case "dev-test":
	// 	handler.CommandDevTest(ce)
	case "set-pl":
		handler.CommandSetPowerLevel(ce)
	// case "set-relay":
	// 	handler.CommandSetRelay(ce)
	// case "unset-relay":
	// 	handler.CommandUnsetRelay(ce)
	default:
		ce.Reply("Unknown command, use the `help` command for help.")
	}
}

// func (handler *CommandHandler) CommandDiscardMegolmSession(ce *CommandEvent) {
// 	if handler.bridge.Crypto == nil {
// 		ce.Reply("This bridge instance doesn't have end-to-bridge encryption enabled")
// 	} else if !ce.User.Admin {
// 		ce.Reply("Only the bridge admin can reset Megolm sessions")
// 	} else {
// 		handler.bridge.Crypto.ResetSession(ce.RoomID)
// 		ce.Reply("Successfully reset Megolm session in this room. New decryption keys will be shared the next time a message is sent from WhatsApp.")
// 	}
// }

// const cmdSetRelayHelp = `set-relay - Relay messages in this room through your WhatsApp account.`

// func (handler *CommandHandler) CommandSetRelay(ce *CommandEvent) {
// 	if !handler.bridge.Config.Bridge.Relay.Enabled {
// 		ce.Reply("Relay mode is not enabled on this instance of the bridge")
// 	} else if ce.Portal == nil {
// 		ce.Reply("This is not a portal room")
// 	} else if handler.bridge.Config.Bridge.Relay.AdminOnly && !ce.User.Admin {
// 		ce.Reply("Only admins are allowed to enable relay mode on this instance of the bridge")
// 	} else {
// 		ce.Portal.RelayUserID = ce.User.MXID
// 		ce.Portal.Update()
// 		ce.Reply("Messages from non-logged-in users in this room will now be bridged through your WhatsApp account")
// 	}
// }

// const cmdUnsetRelayHelp = `unset-relay - Stop relaying messages in this room.`

// func (handler *CommandHandler) CommandUnsetRelay(ce *CommandEvent) {
// 	if !handler.bridge.Config.Bridge.Relay.Enabled {
// 		ce.Reply("Relay mode is not enabled on this instance of the bridge")
// 	} else if ce.Portal == nil {
// 		ce.Reply("This is not a portal room")
// 	} else if handler.bridge.Config.Bridge.Relay.AdminOnly && !ce.User.Admin {
// 		ce.Reply("Only admins are allowed to enable relay mode on this instance of the bridge")
// 	} else {
// 		ce.Portal.RelayUserID = ""
// 		ce.Portal.Update()
// 		ce.Reply("Messages from non-logged-in users will no longer be bridged in this room")
// 	}
// }

// func (handler *CommandHandler) CommandDevTest(_ *CommandEvent) {

// }

const cmdVersionHelp = `version - View the bridge version`

func (handler *CommandHandler) CommandVersion(ce *CommandEvent) {
	linkifiedVersion := fmt.Sprintf("v%s", Version)
	if Tag == Version {
		linkifiedVersion = fmt.Sprintf("[v%s](%s/releases/v%s)", Version, URL, Tag)
	} else if len(Commit) > 8 {
		linkifiedVersion = strings.Replace(linkifiedVersion, Commit[:8], fmt.Sprintf("[%s](%s/commit/%s)", Commit[:8], URL, Commit), 1)
	}
	ce.Reply(fmt.Sprintf("[%s](%s) %s (%s)", Name, URL, linkifiedVersion, BuildTime))
}

const cmdSetPowerLevelHelp = `set-pl [user ID] <power level> - Change the power level in a portal room. Only for bridge admins.`

func (handler *CommandHandler) CommandSetPowerLevel(ce *CommandEvent) {
	if !ce.User.Admin {
		ce.Reply("Only bridge admins can use `set-pl`")
		return
	} else if ce.Portal == nil {
		ce.Reply("This is not a portal room")
		return
	}
	var level int
	var userID id.UserID
	var err error
	if len(ce.Args) == 1 {
		level, err = strconv.Atoi(ce.Args[0])
		if err != nil {
			ce.Reply("Invalid power level \"%s\"", ce.Args[0])
			return
		}
		userID = ce.User.MXID
	} else if len(ce.Args) == 2 {
		userID = id.UserID(ce.Args[0])
		_, _, err := userID.Parse()
		if err != nil {
			ce.Reply("Invalid user ID \"%s\"", ce.Args[0])
			return
		}
		level, err = strconv.Atoi(ce.Args[1])
		if err != nil {
			ce.Reply("Invalid power level \"%s\"", ce.Args[1])
			return
		}
	} else {
		ce.Reply("**Usage:** `set-pl [user] <level>`")
		return
	}
	intent := ce.Portal.MainIntent()
	_, err = intent.SetPowerLevel(ce.RoomID, userID, level)
	if err != nil {
		ce.Reply("Failed to set power levels: %v", err)
	}
}

const cmdHelpHelp = `help - Prints this help`

// CommandHelp handles help command
func (handler *CommandHandler) CommandHelp(ce *CommandEvent) {
	cmdPrefix := ""
	if ce.User.ManagementRoom != ce.RoomID {
		cmdPrefix = handler.bridge.Config.Bridge.CommandPrefix + " "
	}

	ce.Reply("* " + strings.Join([]string{
		cmdPrefix + cmdHelpHelp,
		cmdPrefix + cmdVersionHelp,
		// cmdPrefix + cmdSetRelayHelp,
		// cmdPrefix + cmdUnsetRelayHelp,
		cmdPrefix + cmdSetPowerLevelHelp,
		// cmdPrefix + cmdDeletePortalHelp,
		// cmdPrefix + cmdDeleteAllPortalsHelp,
	}, "\n* "))
}

// func canDeletePortal(portal *Portal, userID id.UserID) bool {
// 	members, err := portal.MainIntent().JoinedMembers(portal.MXID)
// 	if err != nil {
// 		portal.log.Errorfln("Failed to get joined members to check if portal can be deleted by %s: %v", userID, err)
// 		return false
// 	}
// 	for otherUser := range members.Joined {
// 		_, isPuppet := portal.bridge.ParsePuppetMXID(otherUser)
// 		if isPuppet || otherUser == portal.bridge.Bot.UserID || otherUser == userID {
// 			continue
// 		}
// 		user := portal.bridge.GetUserByMXID(otherUser)
// 		if user != nil && user.Session != nil {
// 			return false
// 		}
// 	}
// 	return true
// }

// const cmdDeletePortalHelp = `delete-portal - Delete the current portal. If the portal is used by other people, this is limited to bridge admins.`

// func (handler *CommandHandler) CommandDeletePortal(ce *CommandEvent) {
// 	if ce.Portal == nil {
// 		ce.Reply("You must be in a portal room to use that command")
// 		return
// 	}

// 	if !ce.User.Admin && !canDeletePortal(ce.Portal, ce.User.MXID) {
// 		ce.Reply("Only bridge admins can delete portals with other Matrix users")
// 		return
// 	}

// 	ce.Portal.log.Infoln(ce.User.MXID, "requested deletion of portal.")
// 	ce.Portal.Delete()
// 	ce.Portal.Cleanup(false)
// }

// const cmdDeleteAllPortalsHelp = `delete-all-portals - Delete all portals.`

// func (handler *CommandHandler) CommandDeleteAllPortals(ce *CommandEvent) {
// 	portals := handler.bridge.GetAllPortals()
// 	var portalsToDelete []*Portal

// 	if ce.User.Admin {
// 		portalsToDelete = portals
// 	} else {
// 		portalsToDelete = portals[:0]
// 		for _, portal := range portals {
// 			if canDeletePortal(portal, ce.User.MXID) {
// 				portalsToDelete = append(portalsToDelete, portal)
// 			}
// 		}
// 	}

// 	leave := func(portal *Portal) {
// 		if len(portal.MXID) > 0 {
// 			_, _ = portal.MainIntent().KickUser(portal.MXID, &mautrix.ReqKickUser{
// 				Reason: "Deleting portal",
// 				UserID: ce.User.MXID,
// 			})
// 		}
// 	}
// 	customPuppet := handler.bridge.GetPuppetByCustomMXID(ce.User.MXID)
// 	if customPuppet != nil && customPuppet.CustomIntent() != nil {
// 		intent := customPuppet.CustomIntent()
// 		leave = func(portal *Portal) {
// 			if len(portal.MXID) > 0 {
// 				_, _ = intent.LeaveRoom(portal.MXID)
// 				_, _ = intent.ForgetRoom(portal.MXID)
// 			}
// 		}
// 	}
// 	ce.Reply("Found %d portals, deleting...", len(portalsToDelete))
// 	for _, portal := range portalsToDelete {
// 		portal.Delete()
// 		leave(portal)
// 	}
// 	ce.Reply("Finished deleting portal info. Now cleaning up rooms in background.")

// 	go func() {
// 		for _, portal := range portalsToDelete {
// 			portal.Cleanup(false)
// 		}
// 		ce.Reply("Finished background cleanup of deleted portal rooms.")
// 	}()
// }
