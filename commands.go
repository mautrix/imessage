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
	"strings"

	"go.mau.fi/mautrix-imessage/imessage"
	"maunium.net/go/mautrix/bridge/commands"
)

var (
	HelpSectionManagingPortals = commands.HelpSection{Name: "Managing portals", Order: 15}
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
		cmdSearchContacts,
		cmdRefreshContacts,
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
		Section:     HelpSectionManagingPortals,
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

var cmdSearchContacts = &commands.FullHandler{
	Func: wrapCommand(fnSearchContacts),
	Name: "search-contacts",
	Help: commands.HelpMeta{
		Section:     HelpSectionManagingPortals,
		Description: "Searches contacts based on name, phone, and email (only for BlueBubbles mode).",
	},
	RequiresPortal: false,
	RequiresLogin:  false,
}

func fnSearchContacts(ce *WrappedCommandEvent) {
	ce.Bridge.ZLog.Trace().Interface("args", ce.Args).Str("cmd", ce.Command).Msg("fnSearchContacts")

	if len(ce.Args) == 0 {
		ce.Reply("**Usage:** `search-contacts <search terms>`")
		return
	}

	// TODO trim whitespace from args first
	contacts, err := ce.Bridge.IM.SearchContactList(ce.RawArgs)
	if err != nil {
		ce.Reply("Failed to search contacts: %s", err)
	} else {
		if contacts == nil || len(contacts) == 0 {
			ce.Reply("No contacts found for search `%s`", ce.RawArgs)
		} else {
			replyMessage := fmt.Sprintf("Found %d contacts:\n", len(contacts))

			for _, contact := range contacts {
				markdownString := buildContactString(contact)
				replyMessage += markdownString
				replyMessage += strings.Repeat("-", 40) + "\n"
			}

			ce.Reply(replyMessage)
		}
	}
}

func buildContactString(contact *imessage.Contact) string {
	name := contact.Nickname
	if name == "" {
		name = fmt.Sprintf("%s %s", contact.FirstName, contact.LastName)
	}

	contactInfo := fmt.Sprintf("**%s**\n", name)

	if len(contact.Phones) > 0 {
		contactInfo += "- **Phones:**\n"
		for _, phone := range contact.Phones {
			contactInfo += fmt.Sprintf("  - %s\n", phone)
		}
	}

	if len(contact.Emails) > 0 {
		contactInfo += "- **Emails:**\n"
		for _, email := range contact.Emails {
			contactInfo += fmt.Sprintf("  - %s\n", email)
		}
	}

	return contactInfo
}

var cmdRefreshContacts = &commands.FullHandler{
	Func: wrapCommand(fnRefreshContacts),
	Name: "refresh-contacts",
	Help: commands.HelpMeta{
		Section:     HelpSectionManagingPortals,
		Description: "Request that the bridge reload cached contacts (only for BlueBubbles mode).",
	},
	RequiresPortal: false,
	RequiresLogin:  false,
}

func fnRefreshContacts(ce *WrappedCommandEvent) {
	ce.Bridge.ZLog.Trace().Interface("args", ce.Args).Str("cmd", ce.Command).Msg("fnSearchContacts")

	err := ce.Bridge.IM.RefreshContactList()
	if err != nil {
		ce.Reply("Failed to search contacts: %s", err)
	} else {
		ce.Reply("Contacts List updated!")
	}
}
