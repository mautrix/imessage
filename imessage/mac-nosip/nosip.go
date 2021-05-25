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

package mac_nosip

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	log "maunium.net/go/maulogger/v2"

	"go.mau.fi/mautrix-imessage/imessage"
	"go.mau.fi/mautrix-imessage/imessage/ios"
	"go.mau.fi/mautrix-imessage/ipc"
)

type MacNoSIPConnector struct {
	ios.APIWithIPC
	path string
	proc *exec.Cmd
	log  log.Logger
}

func NewMacNoSIPConnector(bridge imessage.Bridge) (imessage.API, error) {
	logger := bridge.GetLog().Sub("iMessage").Sub("Mac-noSIP")
	return &MacNoSIPConnector{
		APIWithIPC: ios.NewPlainiOSConnector(logger),
		path:       bridge.GetConnectorConfig().IMRestPath,
		log:        logger,
	}, nil
}

func (mac *MacNoSIPConnector) Start() error {
	mac.proc = exec.Command(mac.path)

	stdout, err := mac.proc.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to get subprocess stdout pipe: %w", err)
	}
	stdin, err := mac.proc.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to get subprocess stdin pipe: %w", err)
	}

	mac.SetIPC(ipc.NewCustomProcessor(stdin, stdout, mac.log))

	err = mac.proc.Start()
	if err != nil {
		return fmt.Errorf("failed to start imessage-rest: %w", err)
	}
	return mac.APIWithIPC.Start()
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

func init() {
	imessage.Implementations["mac-nosip"] = NewMacNoSIPConnector
}
