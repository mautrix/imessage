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

package mac_nosip

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	log "maunium.net/go/maulogger/v2"

	"go.mau.fi/mautrix-imessage/imessage"
	"go.mau.fi/mautrix-imessage/imessage/ios"
	"go.mau.fi/mautrix-imessage/ipc"
)

const IncomingLog ipc.Command = "log"
const ReqPing ipc.Command = "ping"

type MacNoSIPConnector struct {
	ios.APIWithIPC
	path                string
	args                []string
	proc                *exec.Cmd
	log                 log.Logger
	procLog             log.Logger
	printPayloadContent bool
	pingInterval        time.Duration
	stopPinger          chan bool
	mergeChats          bool
}

func NewMacNoSIPConnector(bridge imessage.Bridge) (imessage.API, error) {
	logger := bridge.GetLog().Sub("iMessage").Sub("Mac-noSIP")
	processLogger := bridge.GetLog().Sub("iMessage").Sub("Barcelona")
	return &MacNoSIPConnector{
		APIWithIPC:          ios.NewPlainiOSConnector(logger, bridge),
		path:                bridge.GetConnectorConfig().IMRestPath,
		args:                bridge.GetConnectorConfig().IMRestArgs,
		log:                 logger,
		procLog:             processLogger,
		printPayloadContent: bridge.GetConnectorConfig().LogIPCPayloads,
		pingInterval:        time.Duration(bridge.GetConnectorConfig().PingInterval) * time.Second,
		stopPinger:          make(chan bool, 8),
		mergeChats:          bridge.GetConnectorConfig().ChatMerging,
	}, nil
}

func (mac *MacNoSIPConnector) Start(readyCallback func()) error {
	mac.log.Debugln("Preparing to execute", mac.path)
	args := mac.args
	if mac.mergeChats {
		args = append(args, "--enable-merged-chats")
	}
	mac.proc = exec.Command(mac.path, args...)

	if runtime.GOOS == "ios" {
		mac.log.Debugln("Running Barcelona connector on iOS, temp files will be world-readable")
		imessage.TempFilePermissions = 0644
		imessage.TempDirPermissions = 0755
	}

	stdout, err := mac.proc.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to get subprocess stdout pipe: %w", err)
	}
	stdin, err := mac.proc.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to get subprocess stdin pipe: %w", err)
	}

	ipcProc := ipc.NewCustomProcessor(stdin, stdout, mac.log, mac.printPayloadContent)
	mac.SetIPC(ipcProc)
	ipcProc.SetHandler(IncomingLog, mac.handleIncomingLog)
	go func() {
		ipcProc.Loop()
		if mac.proc.ProcessState.Exited() {
			mac.log.Errorfln("Barcelona died with exit code %d, exiting bridge...", mac.proc.ProcessState.ExitCode())
			os.Exit(mac.proc.ProcessState.ExitCode())
		}
	}()

	err = mac.proc.Start()
	if err != nil {
		return fmt.Errorf("failed to start imessage-rest: %w", err)
	}
	mac.log.Debugln("Process started, PID", mac.proc.Process.Pid)

	go mac.pingLoop(ipcProc)

	return mac.APIWithIPC.Start(readyCallback)
}

func (mac *MacNoSIPConnector) pingLoop(ipcProc *ipc.Processor) {
	for {
		resp, err := ipcProc.RequestAsync(ReqPing, nil)
		if err != nil {
			mac.log.Fatalln("Failed to send ping to Barcelona")
			os.Exit(254)
		}
		timeout := time.After(mac.pingInterval)
		select {
		case <-mac.stopPinger:
			return
		case <-timeout:
			mac.log.Fatalfln("Didn't receive pong from Barcelona within %s", mac.pingInterval)
			os.Exit(255)
		case rawData := <-resp:
			if rawData.Command == "error" {
				mac.log.Fatalfln("Barcelona returned error response to pong: %s", rawData.Data)
				os.Exit(253)
			}
		}
		select {
		case <-timeout:
		case <-mac.stopPinger:
			return
		}
	}
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
		mac.log.Debugln("Barcelona subprocess not running when Stop was called")
		return
	}
	mac.stopPinger <- true
	err := mac.proc.Process.Signal(syscall.SIGTERM)
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		mac.log.Warnln("Failed to send SIGTERM to Barcelona process:", err)
	}
	time.AfterFunc(3*time.Second, func() {
		err = mac.proc.Process.Kill()
		if err != nil && !errors.Is(err, os.ErrProcessDone) {
			mac.log.Warnln("Failed to kill Barcelona process:", err)
		}
	})
	err = mac.proc.Wait()
	if err != nil {
		mac.log.Warnln("Error waiting for Barcelona process:", err)
	}
}

func (mac *MacNoSIPConnector) Capabilities() imessage.ConnectorCapabilities {
	return imessage.ConnectorCapabilities{
		MessageSendResponses:     true,
		SendTapbacks:             true,
		SendReadReceipts:         true,
		SendTypingNotifications:  true,
		SendCaptions:             true,
		BridgeState:              true,
		MessageStatusCheckpoints: true,
		MergedChats:              mac.mergeChats,
		RichLinks:                true,
		Correlation:              true,
	}
}

func init() {
	imessage.Implementations["mac-nosip"] = NewMacNoSIPConnector
}
