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

func runOsascript(script string, args ...string) (string, error) {
	args = append([]string{"-"}, args...)
	cmd := exec.Command("osascript", args...)

	var errorBuf bytes.Buffer
	cmd.Stderr = &errorBuf

	// Make sure Wait is always called even if something fails.
	defer func() {
		go func() {
			_ = cmd.Wait()
		}()
	}()

	var stdin io.WriteCloser
	var err error
	if stdin, err = cmd.StdinPipe(); err != nil {
		err = fmt.Errorf("failed to open stdin pipe: %w", err)
	} else if err = cmd.Start(); err != nil {
		err = fmt.Errorf("failed to run osascript: %w", err)
	} else if _, err = io.WriteString(stdin, script); err != nil {
		err = fmt.Errorf("failed to send script to osascript: %w", err)
	} else if err = stdin.Close(); err != nil {
		err = fmt.Errorf("failed to close stdin pipe: %w", err)
	} else if err = cmd.Wait(); err != nil {
		err = fmt.Errorf("failed to wait for osascript: %w (stderr: %s)", err, errorBuf.String())
	}
	return errorBuf.String(), err
}

func runOsascriptWithoutOutput(script string, args ...string) error {
	stderr, err := runOsascript(script, args...)
	if err != nil {
		return err
	} else if len(stderr) > 0 {
		return fmt.Errorf("osascript returned error: %s", stderr)
	}
	return nil
}

func is1728Error(err error) bool {
	exitErr := &exec.ExitError{}
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && strings.Contains(err.Error(), "-1728")
}

func (mac *macOSDatabase) runOsascriptWithRetry(script string, args ...string) error {
	err := runOsascriptWithoutOutput(script, args...)
	if is1728Error(err) {
		mac.log.Warnln("Retrying failed send in 1 second:", err)
		time.Sleep(1 * time.Second)
		err = runOsascriptWithoutOutput(script, args...)
		if is1728Error(err) {
			mac.log.Warnfln("Send failed again after retry: %v, collecting debug info for %s", err, args[0])
			go mac.collect1728DebugInfo(args[0])
		}
	}
	return err
}

func (mac *macOSDatabase) SendMessage(chatID, text string) (*imessage.SendResponse, error) {
	return nil, mac.runOsascriptWithRetry(sendMessage, chatID, text)
}

func (mac *macOSDatabase) SendFile(chatID, filename string, data []byte) (*imessage.SendResponse, error) {
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
