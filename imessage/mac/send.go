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
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.mau.fi/mautrix-imessage/imessage"
)

const sendMessage = `
on run {targetChatID, messageText}
	tell application "Messages"
		send messageText to chat id targetChatID
	end tell
end run
`

const sendFile = `
on run {targetChatID, filePath}
	tell application "Messages"
		send filePath as POSIX file to chat id targetChatID
	end tell
end run
`

func runOsascript(script string, args ...string) error {
	args = append([]string{"-"}, args...)
	cmd := exec.Command("osascript", args...)
	var errorBuf bytes.Buffer
	cmd.Stderr = &errorBuf

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to open stdin pipe: %w", err)
	}
	err = cmd.Start()
	if err != nil {
		return fmt.Errorf("failed to run osascript: %w", err)
	}
	// Make sure Wait is always called even if something fails.
	defer func() {
		go func() {
			_ = cmd.Wait()
		}()
	}()
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
		return fmt.Errorf("failed to wait for osascript: %w (stderr: %s)", err, errorBuf.String())
	} else if errorBuf.Len() > 0 {
		return fmt.Errorf("osascript returned error: %s", errorBuf.String())
	}
	return nil
}

func (mac *macOSDatabase) runOsascriptWithRetry(script string, args ...string) error {
	err := runOsascript(script, args...)
	exitErr := &exec.ExitError{}
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && strings.Contains(err.Error(), "-1728") {
		mac.log.Warnln("Retrying failed send in 1 second: %v", err)
		time.Sleep(1 * time.Second)
		err = runOsascript(script, args...)
	}
	return err
}

func (mac *macOSDatabase) SendMessage(chatID, text string, replyTo string, replyToPart int) (*imessage.SendResponse, error) {
	return nil, mac.runOsascriptWithRetry(sendMessage, chatID, text)
}

func (mac *macOSDatabase) SendFile(chatID, filename string, data []byte, replyTo string, replyToPart int) (*imessage.SendResponse, error) {
	dir, err := ioutil.TempDir("", "mautrix-imessage-upload")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}
	filePath := filepath.Join(dir, filename)
	err = ioutil.WriteFile(filePath, data, 0640)
	if err != nil {
		return nil, fmt.Errorf("failed to write data to temp file: %w", err)
	}
	err = mac.runOsascriptWithRetry(sendFile, chatID, filePath)
	go func() {
		// TODO maybe log when the file gets removed
		// Random sleep to make sure the message has time to get sent
		time.Sleep(60 * time.Second)
		_ = os.Remove(filePath)
		_ = os.Remove(dir)
	}()
	return nil, err
}

func (mac *macOSDatabase) SendTapback(chatID, targetGUID string, targetPart int, tapback imessage.TapbackType, remove bool) (*imessage.SendResponse, error) {
	return nil, nil
}

func (mac *macOSDatabase) SendReadReceipt(chatID, readUpTo string) error {
	return nil
}

func (mac *macOSDatabase) SendTypingNotification(chatID string, typing bool) error {
	return nil
}
