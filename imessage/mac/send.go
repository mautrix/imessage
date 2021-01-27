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
	"fmt"
	"os"
	"os/exec"
)

const sendMessage = `
on run {targetChatId, targetMessage}
    tell application "Messages"
        set theBuddy to a reference to chat id targetChatId
        send targetMessage to theBuddy
    end tell
end run
`

func (imdb *Database) SendMessage(chatID, text string) error {
	cmd := exec.Command("osascript", "-", chatID, text)
	// TODO make these go somewhere else
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to open stdin pipe: %w", err)
	}
	err = cmd.Start()
	if err != nil {
		return fmt.Errorf("failed to run osascript: %w", err)
	}
	_, err = stdin.Write([]byte(sendMessage))
	if err != nil {
		return fmt.Errorf("failed to send script to osascript: %w", err)
	}
	err = stdin.Close()
	if err != nil {
		return fmt.Errorf("failed to close stdin pipe: %w", err)
	}
	err = cmd.Wait()
	if err != nil {
		return fmt.Errorf("failed to wait for osascript: %w", err)
	}
	return nil
}
