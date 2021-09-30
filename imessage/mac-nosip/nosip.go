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

//+build go1.16

package mac_nosip

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	log "maunium.net/go/maulogger/v2"

	"go.mau.fi/mautrix-imessage/imessage"
	"go.mau.fi/mautrix-imessage/imessage/ios"
	"go.mau.fi/mautrix-imessage/ipc"
)

const IncomingLog ipc.Command = "log"

type MacNoSIPConnector struct {
	ios.APIWithIPC
	path                string
	proc                *exec.Cmd
	log                 log.Logger
	procLog             log.Logger
	printPayloadContent bool
}

func NewMacNoSIPConnector(bridge imessage.Bridge) (imessage.API, error) {
	logger := bridge.GetLog().Sub("iMessage").Sub("Mac-noSIP")
	processLogger := bridge.GetLog().Sub("iMessage").Sub("Barcelona")
	return &MacNoSIPConnector{
		APIWithIPC:          ios.NewPlainiOSConnector(logger, bridge),
		path:                bridge.GetConnectorConfig().IMRestPath,
		log:                 logger,
		procLog:             processLogger,
		printPayloadContent: bridge.GetConnectorConfig().LogIPCPayloads,
	}, nil
}

func (mac *MacNoSIPConnector) Start() error {
	mac.log.Debugln("Preparing to execute", mac.path)
	mac.proc = exec.Command(mac.path)

	stdout, err := mac.proc.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to get subprocess stdout pipe: %w", err)
	}
	stdin, err := mac.proc.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to get subprocess stdin pipe: %w", err)
	}

	ipcProc := ipc.NewCustomProcessor(stdin, stdout, mac.log, mac.printPayloadContent)
	go func() {
		ipcProc.Loop()
		if mac.proc.ProcessState.Exited() {
			mac.log.Errorf("nosip proc died with exit code %d – goodbye", mac.proc.ProcessState.ExitCode())
			os.Exit(mac.proc.ProcessState.ExitCode())
		}
	}()
	mac.SetIPC(ipcProc)

	err = mac.proc.Start()
	if err != nil {
		return fmt.Errorf("failed to start imessage-rest: %w", err)
	}
	mac.log.Debugln("Process started, PID", mac.proc.Process.Pid)
	ipcProc.SetHandler(IncomingLog, mac.handleIncomingLog)
	return mac.APIWithIPC.Start()
}

type LogLine struct {
	Message  string                 `json:"message"`
	Level    string                 `json:"level"`
	Module   string                 `json:"module"`
	Metadata map[string]interface{} `json:"metadata"`
}

func getLevelFromName(name string) log.Level {
	switch strings.ToUpper(name) {
	case "DEBUG":
		return log.LevelDebug
	case "INFO":
		return log.LevelInfo
	case "WARN":
		return log.LevelWarn
	case "ERROR":
		return log.LevelError
	case "FATAL":
		return log.LevelFatal
	default:
		return log.Level{Name: name, Color: -1, Severity: 1}
	}
}

func (mac *MacNoSIPConnector) handleIncomingLog(data json.RawMessage) interface{} {
	var message LogLine
	err := json.Unmarshal(data, &message)
	if err != nil {
		mac.log.Warnfln("Failed to parse incoming log line: %v (data: %s)", err, data)
		return nil
	}
	logger := mac.procLog.Subm(message.Module, message.Metadata)
	logger.Log(getLevelFromName(message.Level), message.Message)
	return nil
}

func (mac *MacNoSIPConnector) Stop() {
	if mac.proc == nil || mac.proc.ProcessState == nil || mac.proc.ProcessState.Exited() {
		mac.log.Debugln("imessage-rest subprocess not running when Stop was called")
		return
	}
	err := mac.proc.Process.Signal(syscall.SIGTERM)
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		mac.log.Warnln("Failed to send SIGTERM to imessage-rest process:", err)
	}
	time.AfterFunc(3*time.Second, func() {
		err = mac.proc.Process.Kill()
		if err != nil && !errors.Is(err, os.ErrProcessDone) {
			mac.log.Warnln("Failed to kill imessage-rest process:", err)
		}
	})
	err = mac.proc.Wait()
	if err != nil {
		mac.log.Warnln("Error waiting for imessage-rest process:", err)
	}
}

func (mac *MacNoSIPConnector) Capabilities() imessage.ConnectorCapabilities {
	return imessage.ConnectorCapabilities{
		MessageSendResponses:    true,
		SendTapbacks:            true,
		SendReadReceipts:        true,
		SendTypingNotifications: true,
		BridgeState:             true,
	}
}

func init() {
	imessage.Implementations["mac-nosip"] = NewMacNoSIPConnector
}
