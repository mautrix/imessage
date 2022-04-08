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
	"fmt"
	"strconv"
	"strings"

	"maunium.net/go/maulogger/v2"

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
	User    *User
	Portal  *Portal
	Handler *CommandHandler
	RoomID  id.RoomID
	EventID id.EventID
	Command string
	Args    []string
	ReplyTo id.EventID
}

// Reply sends a reply to command as notice
func (ce *CommandEvent) Reply(msg string, args ...interface{}) {
	content := format.RenderMarkdown(fmt.Sprintf(msg, args...), true, false)
	content.MsgType = event.MsgNotice
	intent := ce.Bot
	if ce.Portal != nil && ce.Portal.IsPrivateChat() && !ce.Portal.Encrypted {
		intent = ce.Portal.MainIntent()
	}
	var err error
	if ce.Portal != nil && ce.Portal.Encrypted {
		_, err = ce.Portal.sendMessage(intent, event.EventMessage, &content, nil, 0)
	} else {
		_, err = intent.SendMessageEvent(ce.RoomID, event.EventMessage, content)
	}
	if err != nil {
		ce.Handler.log.Warnfln("Failed to reply to command: %v", err)
	}
}

// Handle handles messages to the bridge
func (handler *CommandHandler) Handle(roomID id.RoomID, eventID id.EventID, message string, replyTo id.EventID) {
	args := strings.Fields(message)
	if len(args) == 0 {
		args = []string{"unknown-command"}
	}
	ce := &CommandEvent{
		Bot:     handler.bridge.Bot,
		Bridge:  handler.bridge,
		User:    handler.bridge.user,
		Portal:  handler.bridge.GetPortalByMXID(roomID),
		Handler: handler,
		RoomID:  roomID,
		EventID: eventID,
		Command: strings.ToLower(args[0]),
		Args:    args[1:],
		ReplyTo: replyTo,
	}
	handler.log.Debugfln("Received command '%s' in %s", message, roomID)
	handler.CommandMux(ce)
}

func (handler *CommandHandler) CommandMux(ce *CommandEvent) {
	switch ce.Command {
	case "help":
		handler.CommandHelp(ce)
	case "version":
		handler.CommandVersion(ce)
	case "set-pl":
		handler.CommandSetPowerLevel(ce)
	case "discard-megolm-session", "discard-session":
		handler.CommandDiscardMegolmSession(ce)
	case "login-matrix":
		handler.CommandLoginMatrix(ce)
	case "ping-matrix":
		handler.CommandPingMatrix(ce)
	case "logout-matrix":
		handler.CommandLogoutMatrix(ce)
	case "sync":
		handler.CommandSync(ce)

	default:
		ce.Reply("Unknown command, use the `help` command for help.")
	}
}

func (handler *CommandHandler) CommandDiscardMegolmSession(ce *CommandEvent) {
	if handler.bridge.Crypto == nil {
		ce.Reply("This bridge instance doesn't have end-to-bridge encryption enabled")
	} else {
		handler.bridge.Crypto.ResetSession(ce.RoomID)
		ce.Reply("Successfully reset Megolm session in this room. New decryption keys will be shared the next time a message is sent from iMessage.")
	}
}

const cmdVersionHelp = `version - View the bridge version.`

func (handler *CommandHandler) CommandVersion(ce *CommandEvent) {
	linkifiedVersion := fmt.Sprintf("v%s", Version)
	if Tag == Version {
		linkifiedVersion = fmt.Sprintf("[v%s](%s/releases/v%s)", Version, URL, Tag)
	} else if len(Commit) > 8 {
		linkifiedVersion = strings.Replace(linkifiedVersion, Commit[:8], fmt.Sprintf("[%s](%s/commit/%s)", Commit[:8], URL, Commit), 1)
	}
	ce.Reply(fmt.Sprintf("[%s](%s) %s (%s)", Name, URL, linkifiedVersion, BuildTime))
}

const cmdSetPowerLevelHelp = `set-pl [user ID] <power level> - Change the power levels as the bridge bot.`

func (handler *CommandHandler) CommandSetPowerLevel(ce *CommandEvent) {
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
	intent := ce.Bot
	if ce.Portal != nil {
		intent = ce.Portal.MainIntent()
	}
	_, err = intent.SetPowerLevel(ce.RoomID, userID, level)
	if err != nil {
		ce.Reply("Failed to set power levels: %v", err)
	}
}

const cmdSyncHelp = `sync space - Create/Sync Space.`

func (handler *CommandHandler) CommandSync(ce *CommandEvent) {
	if len(ce.Args) == 0 {
		ce.Reply("**Usage:** `sync space`")
		return
	}
	args := strings.ToLower(strings.Join(ce.Args, " "))
	space := strings.Contains(args, "space")
	if space {
		if !ce.Bridge.Config.Bridge.PersonalFilteringSpaces.Enable {
			ce.Reply("Personal filtering spaces are not enabled on this instance of the bridge")
			return
		}
		keys := ce.Bridge.DB.Portal.FindPrivateChatsNotInSpace()
		count := 0
		for _, key := range keys {
			portal := ce.Bridge.GetPortalByGUID(key)
			portal.addToSpace(ce.User.bridge.user)
			count++
		}
		plural := "s"
		if count == 1 {
			plural = ""
		}
		ce.Reply("Added %d DM room%s to space", count, plural)
	}

}

const cmdLoginMatrixHelp = `login-matrix <_access token_> - Enable double puppeting.`

func (handler *CommandHandler) CommandLoginMatrix(ce *CommandEvent) {
	if len(ce.Args) == 0 {
		ce.Reply("**Usage:** `login-matrix <access token>`")
		return
	}
	ce.User.AccessToken = ce.Args[0]
	err := ce.User.startCustomMXID()
	if err != nil {
		ce.Reply("Failed to switch puppet: %v", err)
		return
	}
	ce.Reply("Successfully enabled double puppeting")
	ce.User.Update()
}

const cmdPingMatrixHelp = `ping-matrix - Check if your double puppet is working correctly.`

func (handler *CommandHandler) CommandPingMatrix(ce *CommandEvent) {
	if ce.User.AccessToken == "" || ce.User.DoublePuppetIntent == nil {
		ce.Reply("You have not enabled double puppeting.")
		return
	}

	resp, err := ce.User.DoublePuppetIntent.Whoami()
	if err != nil {
		ce.Reply("Failed to validate Matrix login: %v", err)
	} else {
		ce.Reply("Confirmed valid access token for %s / %s", resp.UserID, resp.DeviceID)
	}
}

const cmdLogoutMatrixHelp = `logout-matrix - Disable double puppeting.`

func (handler *CommandHandler) CommandLogoutMatrix(ce *CommandEvent) {
	if ce.User.AccessToken == "" {
		ce.Reply("You had not enabled double puppeting.")
		return
	}
	ce.User.AccessToken = ""
	ce.User.Update()
	ce.Reply("Successfully disabled double puppeting")
}

const cmdHelpHelp = `help - Prints this help page.`

// CommandHelp handles help command
func (handler *CommandHandler) CommandHelp(ce *CommandEvent) {
	cmdPrefix := handler.bridge.Config.Bridge.CommandPrefix + " "
	ce.Reply("* " + strings.Join([]string{
		cmdPrefix + cmdHelpHelp,
		cmdPrefix + cmdVersionHelp,
		cmdPrefix + cmdSetPowerLevelHelp,
		cmdPrefix + cmdLoginMatrixHelp,
		cmdPrefix + cmdPingMatrixHelp,
		cmdPrefix + cmdLogoutMatrixHelp,
		cmdPrefix + cmdSyncHelp,
	}, "\n* "))
}
