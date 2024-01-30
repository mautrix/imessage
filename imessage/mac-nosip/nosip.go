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
	"net"
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
	unixSocket          string
	unixServer          net.Listener
	stopping            bool
	locale              string
	env                 []string

	chatInfoProxy imessage.API
}

type NoopContacts struct{}

func (n NoopContacts) GetContactInfo(_ string) (*imessage.Contact, error) {
	return nil, nil
}

func (n NoopContacts) GetContactList() ([]*imessage.Contact, error) {
	return []*imessage.Contact{}, nil
}

func (n NoopContacts) SearchContactList(searchTerms string) ([]*imessage.Contact, error) {
	return nil, errors.New("not implemented")
}

func NewMacNoSIPConnector(bridge imessage.Bridge) (imessage.API, error) {
	logger := bridge.GetLog().Sub("iMessage").Sub("Mac-noSIP")
	processLogger := bridge.GetLog().Sub("iMessage").Sub("Barcelona")
	iosConn := ios.NewPlainiOSConnector(logger, bridge)
	contactsMode := bridge.GetConnectorConfig().ContactsMode
	switch contactsMode {
	case "mac":
		contactProxy, err := setupContactProxy(logger)
		if err != nil {
			return nil, err
		}
		iosConn.SetContactProxy(contactProxy)
	case "disable":
		iosConn.SetContactProxy(NoopContacts{})
	case "ipc":
	default:
		return nil, fmt.Errorf("unknown contacts mode %q", contactsMode)
	}
	chatInfoProxy, err := setupChatInfoProxy(logger.Sub("ChatInfoProxy"))
	if err != nil {
		logger.Warnfln("Failed to set up chat info proxy: %v", err)
	} else {
		iosConn.SetChatInfoProxy(chatInfoProxy)
	}
	unixSocket := bridge.GetConnectorConfig().UnixSocket
	if unixSocket == "" {
		unixSocket = "mautrix-imessage.sock"
	}
	return &MacNoSIPConnector{
		APIWithIPC:          iosConn,
		path:                bridge.GetConnectorConfig().IMRestPath,
		args:                bridge.GetConnectorConfig().IMRestArgs,
		log:                 logger,
		procLog:             processLogger,
		printPayloadContent: bridge.GetConnectorConfig().LogIPCPayloads,
		pingInterval:        time.Duration(bridge.GetConnectorConfig().PingInterval) * time.Second,
		stopPinger:          make(chan bool, 8),
		unixSocket:          unixSocket,
		locale:              bridge.GetConnectorConfig().HackySetLocale,
		env:                 bridge.GetConnectorConfig().Environment,
	}, nil
}

func (mac *MacNoSIPConnector) run(command string, args ...string) (string, error) {
	output, err := exec.Command(command, args...).CombinedOutput()
	if err != nil {
		err = fmt.Errorf("failed to execute %s: %w", command, err)
	}
	return strings.TrimSpace(string(output)), err
}

func (mac *MacNoSIPConnector) fixLocale() {
	if mac.locale == "" {
		return
	}
	mac.log.Debugln("Checking user locale")
	locale, err := mac.run("/usr/bin/defaults", "read", "-g", "AppleLocale")
	if err != nil {
		mac.log.Warnfln("Failed to read current locale: %v", err)
		return
	}
	if locale != mac.locale {
		_, err = mac.run("/usr/bin/defaults", "write", "-g", "AppleLocale", mac.locale)
		if err != nil {
			mac.log.Warnfln("Failed to change locale: %v", err)
		} else {
			mac.log.Infofln("Changed user locale from %s to %s", locale, mac.locale)
		}
	} else {
		mac.log.Debugln("User locale is already set to", locale)
	}
}

func (mac *MacNoSIPConnector) Start(readyCallback func()) error {
	if mac.locale != "" {
		mac.fixLocale()
	}
	mac.log.Debugln("Preparing to execute", mac.path)
	args := append(mac.args, "--unix-socket", mac.unixSocket)
	mac.proc = exec.Command(mac.path, args...)
	mac.proc.Env = append(os.Environ(), mac.env...)
	mac.proc.Stdout = mac.procLog.Sub("Stdout").Writer(log.LevelInfo)
	mac.proc.Stderr = mac.procLog.Sub("Stderr").Writer(log.LevelError)

	if runtime.GOOS == "ios" {
		mac.log.Debugln("Running Barcelona connector on iOS, temp files will be world-readable")
		imessage.TempFilePermissions = 0644
		imessage.TempDirPermissions = 0755
	}

	var err error
	if _, err = os.Stat(mac.unixSocket); err == nil {
		mac.log.Debugln("Unlinking existing unix socket")
		err = syscall.Unlink(mac.unixSocket)
		if err != nil {
			mac.log.Warnln("Error unlinking existing unix socket:", err)
		}
	}
	mac.unixServer, err = net.Listen("unix", mac.unixSocket)
	if err != nil {
		return fmt.Errorf("failed to open unix socket: %w", err)
	}

	err = mac.proc.Start()
	if err != nil {
		return fmt.Errorf("failed to start Barcelona: %w", err)
	}
	go func() {
		err := mac.proc.Wait()
		if mac.stopping {
			return
		}
		if err != nil {
			mac.log.Errorfln("Barcelona died with exit code %d and error %v, exiting bridge...", mac.proc.ProcessState.ExitCode(), err)
		} else {
			mac.log.Errorfln("Barcelona died with exit code %d, exiting bridge...", mac.proc.ProcessState.ExitCode())
		}
		_ = mac.unixServer.Close()
		_ = syscall.Unlink(mac.unixSocket)
		os.Exit(mac.proc.ProcessState.ExitCode())
	}()
	mac.log.Debugln("Process started, PID", mac.proc.Process.Pid)

	conn, err := mac.unixServer.Accept()
	if err != nil {
		mac.log.Errorfln("Error accepting unix socket connection: %v", err)
		os.Exit(44)
	}
	mac.log.Debugln("Received unix socket connection")

	ipcProc := ipc.NewCustomProcessor(conn, conn, mac.log, mac.printPayloadContent)
	mac.SetIPC(ipcProc)
	ipcProc.SetHandler(IncomingLog, mac.handleIncomingLog)
	go ipcProc.Loop()

	go mac.pingLoop(ipcProc)

	return mac.APIWithIPC.Start(readyCallback)
}

const maxTimeouts = 2

func (mac *MacNoSIPConnector) pingLoop(ipcProc *ipc.Processor) {
	timeouts := 0
	for {
		resp, _, err := ipcProc.RequestAsync(ReqPing, nil)
		if err != nil {
			mac.log.Fatalln("Failed to send ping to Barcelona")
			os.Exit(254)
		}
		timeout := time.After(mac.pingInterval)
		select {
		case <-mac.stopPinger:
			return
		case <-timeout:
			timeouts++
			if timeouts >= maxTimeouts {
				mac.log.Fatalfln("Didn't receive pong from Barcelona within %s", mac.pingInterval)
				os.Exit(255)
			} else {
				mac.log.Warnfln("Didn't receive pong from Barcelona within %s", mac.pingInterval)
				continue
			}
		case rawData := <-resp:
			if rawData.Command == "error" {
				mac.log.Fatalfln("Barcelona returned error response to pong: %s", rawData.Data)
				os.Exit(253)
			}
			timeouts = 0
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
	mac.stopping = true
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
	_ = mac.unixServer.Close()
	_ = syscall.Unlink(mac.unixSocket)
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
		DeliveredStatus:          true,
		ContactChatMerging:       true,
		RichLinks:                true,
	}
}

func init() {
	imessage.Implementations["mac-nosip"] = NewMacNoSIPConnector
}
