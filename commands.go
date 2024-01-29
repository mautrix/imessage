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
	"maunium.net/go/mautrix/bridge/commands"
)

var (
	HelpSectionChatManagement = commands.HelpSection{Name: "Chat management", Order: 11}
)

type WrappedCommandEvent struct {
	*commands.Event
	Bridge *IMBridge
	User   *User
	Portal *Portal
}

func (br *IMBridge) RegisterCommands() {
	proc := br.CommandProcessor.(*commands.Processor)
	proc.AddHandlers(
		cmdPM,
	)
}

func wrapCommand(handler func(*WrappedCommandEvent)) func(*commands.Event) {
	return func(ce *commands.Event) {
		user := ce.User.(*User)
		var portal *Portal
		if ce.Portal != nil {
			portal = ce.Portal.(*Portal)
		}
		br := ce.Bridge.Child.(*IMBridge)
		handler(&WrappedCommandEvent{ce, br, user, portal})
	}
}

var cmdPM = &commands.FullHandler{
	Func: wrapCommand(fnPM),
	Name: "pm",
	Help: commands.HelpMeta{
		Section:     HelpSectionChatManagement,
		Description: "Creates a new PM with the specified number or address.",
	},
	RequiresPortal: false,
	RequiresLogin:  false,
}

func fnPM(ce *WrappedCommandEvent) {
	ce.Bridge.ZLog.Trace().Interface("args", ce.Args).Str("cmd", ce.Command).Msg("fnPM")

	if len(ce.Args) == 0 {
		ce.Reply("**Usage:** `pm <international phone number>` OR `pm <apple id email address>`")
		return
	}

	startedDm, err := ce.Bridge.WebsocketHandler.StartChat(*&StartDMRequest{
		Identifier:    ce.RawArgs,
		Force:         false,
		ActuallyStart: true,
	})

	if err != nil {
		ce.Reply("Failed to start PM: %s", err)
	} else {
		ce.Reply("Created portal room [%s](%s) and invited you to it.", startedDm.RoomID, startedDm.RoomID.URI(ce.Bridge.Config.Homeserver.Domain).MatrixToURL())
	}
}

// TODO: potentially add the following commands
// contact-search (search contact for addresses so they can be used to start a dm)
// start-group-chat
