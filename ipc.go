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

package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"

	log "maunium.net/go/maulogger/v2"
)

type IPCCommand string

const (
	CommandPing     IPCCommand = "ping"
	CommandStop     IPCCommand = "stop"
	CommandResponse IPCCommand = "response"
	CommandError    IPCCommand = "error"
)

var (
	ErrUnknownCommand = errors.New("unknown command")
)

type Message struct {
	Command IPCCommand      `json:"command"`
	ID      int             `json:"id"`
	Data    json.RawMessage `json:"data"`
}

type ResponseMessage struct {
	Command IPCCommand  `json:"command"`
	ID      int         `json:"id,omitempty"`
	Data    interface{} `json:"data,omitempty"`
}

type PingResponse struct {
	OK bool `json:"ok"`
}

type IPCHandler struct {
	bridge *Bridge
	log    log.Logger
	lock   *sync.Mutex
	stdout *json.Encoder
	stdin  *json.Decoder
}

func NewIPCHandler(bridge *Bridge) *IPCHandler {
	return &IPCHandler{
		bridge: bridge,
		lock:   &bridge.Log.(*log.BasicLogger).StdoutLock,
		log:    bridge.Log.Sub("IPC"),
		stdout: json.NewEncoder(os.Stdout),
		stdin:  json.NewDecoder(os.Stdin),
	}
}

func (ipc *IPCHandler) Loop() {
	for {
		var msg Message
		err := ipc.stdin.Decode(&msg)
		if err == io.EOF {
			ipc.log.Debugln("Standard input closed, ending IPC loop")
			break
		} else if err != nil {
			ipc.log.Errorln("Failed to read input:", err)
			break
		} else {
			go ipc.Handle(&msg)
		}
	}

}

func (ipc *IPCHandler) respond(id int, response interface{}) {
	if id == 0 && response == nil {
		// No point in replying
		return
	}
	resp := ResponseMessage{Command: CommandResponse, ID: id, Data: response}
	respErr, isError := response.(error)
	if isError {
		resp.Data = respErr.Error()
		resp.Command = CommandError
	}
	ipc.lock.Lock()
	err := ipc.stdout.Encode(resp)
	ipc.lock.Unlock()
	if err != nil {
		ipc.log.Errorln("Failed to encode IPC response: %v", err)
	}
}

func (ipc *IPCHandler) Handle(msg *Message) {
	switch msg.Command {
	case CommandPing:
		// TODO do some checks?
		ipc.respond(msg.ID, PingResponse{true})
	case CommandStop:
		ipc.respond(msg.ID, nil)
		ipc.bridge.stop <- struct{}{}
	default:
		ipc.respond(msg.ID, ErrUnknownCommand)
	}
}
