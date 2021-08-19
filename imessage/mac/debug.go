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

package mac

import (
	"encoding/json"
	"fmt"
	"strings"

	"go.mau.fi/mautrix-imessage/imessage"
)

const getAllAccountsJSON = `
tell application "Messages"
	repeat with num from 1 to (count of every service)
		set accProps to properties of (item num of every service)
		log "{\"id\": \"" & id of accProps & "\", \"description\": \"" & description of accProps & "\", \"enabled\": " & enabled of accProps & ", \"connection_status\": \"" & connection status of accProps & "\", \"service_type\": \"" & service type of accProps & "\"}"
	end repeat
end tell
`

const getChatDirectly = `
on run {chatID}
	tell application "Messages"
		set theChat to chat id chatID
		set chatProps to properties of theChat
		log chatProps
	end tell
end run
`

const getChatWithAccount = `
on run {chatID, accountID}
	tell application "Messages"
		set theService to service id accountID
		set theChat to chat id chatID of theService
		set chatProps to properties of theChat
		log chatProps
	end tell
end run
`

const getBuddyWithAccount = `
on run {buddyID, accountID}
	tell application "Messages"
		set theService to service id accountID
		set theParticipant to buddy buddyID of theService
		set participantProps to properties of theParticipant
		log participantProps
	end tell
end run
`

type Account struct {
	ID               string `json:"id"`
	Description      string `json:"description"`
	Enabled          bool   `json:"enabled"`
	ConnectionStatus string `json:"connection_status"`
	ServiceType      string `json:"service_type"`
}

func getAccounts() ([]Account, error) {
	output, err := runOsascript(getAllAccountsJSON)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(output), "\n")
	accounts := make([]Account, len(lines))
	for i, line := range lines {
		err = json.Unmarshal([]byte(line), &accounts[i])
		if err != nil {
			return accounts, fmt.Errorf("failed to deserialize account #%d: %v", i+1, err)
		}
	}
	return accounts, nil
}

func (mac *macOSDatabase) collect1728DebugInfo(identifier imessage.Identifier) {
	chatID := identifier.String()
	mac.log.Debugfln("------------------------------ %s -1728 debug info ------------------------------", chatID)
	accounts, err := getAccounts()
	mac.log.Debugln("Accounts error:", err)
	directChatOutput, err := runOsascript(getChatDirectly, chatID)
	if err != nil {
		mac.log.Debugfln("Error when getting %s directly: %v", chatID, err)
	} else {
		mac.log.Debugfln("Result when getting %s directly: %s", chatID, directChatOutput)
	}
	for index, acc := range accounts {
		mac.log.Debugfln("Account #%d: %+v", index+1, acc)
		var accChatOutput string
		accChatOutput, err = runOsascript(getChatWithAccount, chatID, acc.ID)
		if err != nil {
			mac.log.Debugfln("Error when getting chat %s with %s: %v", chatID, acc.ID, err)
		} else {
			mac.log.Debugfln("Result when getting chat %s with %s: %s", chatID, acc.ID, accChatOutput)
		}
		if !identifier.IsGroup {
			var accPartOutput string
			accPartOutput, err = runOsascript(getBuddyWithAccount, identifier.LocalID, acc.ID)
			if err != nil {
				mac.log.Debugfln("Error when getting buddy %s with %s: %v", identifier.LocalID, acc.ID, err)
			} else {
				mac.log.Debugfln("Result when getting buddy %s with %s: %s", identifier.LocalID, acc.ID, accPartOutput)
			}
		}
	}
	mac.log.Debugln(strings.Repeat("-", len(chatID) + 79))
}
