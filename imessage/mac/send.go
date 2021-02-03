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
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
)

const sendMessage = `
on run {targetChatID, messageText}
	tell application "Messages"
		set theBuddy to a reference to chat id targetChatID
		send messageText to theBuddy
	end tell
end run
`

const sendFile = `
on run {targetChatID, filePath}
	tell application "Messages"
		set theBuddy to a reference to chat id targetChatID
		set theFile to (filePath as POSIX file)
		send theFile to theBuddy
	end tell
end run
`

func runOsascript(script string, args ...string) error {
	args = append([]string{"-"}, args...)
	cmd := exec.Command("osascript", args...)
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
	_, err = io.WriteString(stdin, script)
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

func (imdb *Database) SendMessage(chatID, text string) error {
	return runOsascript(sendMessage, chatID, text)
}

func (imdb *Database) SendFile(chatID, filename string, data []byte) error {
	dir, err := ioutil.TempDir("", "mautrix-imessage-upload")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.Remove(dir)
	filePath := filepath.Join(dir, filename)
	err = ioutil.WriteFile(filePath, data, 0640)
	if err != nil {
		return fmt.Errorf("failed to write data to temp file: %w", err)
	}
	defer os.Remove(filePath)
	return runOsascript(sendFile, chatID, filePath)
}
