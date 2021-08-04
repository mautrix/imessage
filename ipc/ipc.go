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

package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"sync"
	"sync/atomic"

	log "maunium.net/go/maulogger/v2"
)

const (
	CommandResponse = "response"
	CommandError    = "error"
)

var (
	ErrUnknownCommand = Error{"unknown-command", "Unknown command"}
)

type Command string

type Message struct {
	Command Command         `json:"command"`
	ID      int             `json:"id"`
	Data    json.RawMessage `json:"data"`
}

type OutgoingMessage struct {
	Command Command     `json:"command"`
	ID      int         `json:"id,omitempty"`
	Data    interface{} `json:"data,omitempty"`
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (err Error) Error() string {
	return fmt.Sprintf("%s: %s", err.Code, err.Message)
}

func (err Error) Is(other error) bool {
	otherErr, ok := other.(Error)
	if !ok {
		return false
	}
	return otherErr.Code == err.Code
}

type HandlerFunc func(message json.RawMessage) interface{}

type Processor struct {
	log    log.Logger
	lock   *sync.Mutex
	stdout *json.Encoder
	stdin  *json.Decoder

	handlers   map[Command]HandlerFunc
	waiters    map[int]chan<- *Message
	waiterLock sync.Mutex
	reqID      int32

	printPayloadContent bool
}

func newProcessor(lock *sync.Mutex, output io.Writer, input io.Reader, logger log.Logger, printPayloadContent bool) *Processor {
	return &Processor{
		lock:                lock,
		log:                 logger,
		stdout:              json.NewEncoder(output),
		stdin:               json.NewDecoder(input),
		handlers:            make(map[Command]HandlerFunc),
		waiters:             make(map[int]chan<- *Message),
		printPayloadContent: printPayloadContent,
	}
}

func NewCustomProcessor(output io.Writer, input io.Reader, logger log.Logger, printPayloadContent bool) *Processor {
	return newProcessor(&sync.Mutex{}, output, input, logger.Sub("IPC"), printPayloadContent)
}

func NewStdioProcessor(logger log.Logger, printPayloadContent bool) *Processor {
	return newProcessor(&logger.(*log.BasicLogger).StdoutLock, os.Stdout, os.Stdin, logger.Sub("IPC"), printPayloadContent)
}

func (ipc *Processor) Loop() {
	for {
		var msg Message
		err := ipc.stdin.Decode(&msg)
		if err == io.EOF {
			ipc.log.Debugln("Standard input closed, ending IPC loop")
			break
		} else if err != nil {
			ipc.log.Errorln("Failed to read input:", err)
			break
		}

		if msg.Command != "log" {
			if ipc.printPayloadContent {
				maxLength := 200
				snip := "…"
				if len(msg.Data) < maxLength {
					snip = ""
					maxLength = len(msg.Data)
				}

				ipc.log.Debugfln("Received IPC command: %s/%d - %s%s", msg.Command, msg.ID, msg.Data[:maxLength], snip)
			} else {
				ipc.log.Debugfln("Received IPC command: %s/%d", msg.Command, msg.ID)
			}
		}

		if msg.Command == "response" || msg.Command == "error" {
			ipc.waiterLock.Lock()
			waiter, ok := ipc.waiters[msg.ID]
			if !ok {
				ipc.log.Warnln("Nothing waiting for IPC response to %d", msg.ID)
			} else {
				delete(ipc.waiters, msg.ID)
				waiter <- &msg
			}
			ipc.waiterLock.Unlock()
		} else {
			handler, ok := ipc.handlers[msg.Command]
			if !ok {
				ipc.respond(msg.ID, ErrUnknownCommand)
			} else {
				go ipc.callHandler(&msg, handler)
			}
		}
	}
}

func (ipc *Processor) Send(cmd Command, data interface{}) error {
	ipc.lock.Lock()
	err := ipc.stdout.Encode(OutgoingMessage{Command: cmd, Data: data})
	ipc.lock.Unlock()
	return err
}

func (ipc *Processor) RequestAsync(cmd Command, data interface{}) (<-chan *Message, error) {
	respChan := make(chan *Message, 1)
	reqID := int(atomic.AddInt32(&ipc.reqID, 1))
	ipc.waiterLock.Lock()
	ipc.waiters[reqID] = respChan
	ipc.waiterLock.Unlock()
	ipc.lock.Lock()
	err := ipc.stdout.Encode(OutgoingMessage{Command: cmd, ID: reqID, Data: data})
	ipc.lock.Unlock()
	if err != nil {
		ipc.waiterLock.Lock()
		delete(ipc.waiters, reqID)
		ipc.waiterLock.Unlock()
		close(respChan)
	}
	return respChan, err
}

func (ipc *Processor) Request(ctx context.Context, cmd Command, reqData interface{}, respData interface{}) error {
	respChan, err := ipc.RequestAsync(cmd, reqData)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	select {
	case rawData := <-respChan:
		if rawData.Command == "error" {
			var respErr Error
			err = json.Unmarshal(rawData.Data, &respErr)
			if err != nil {
				return fmt.Errorf("failed to parse error response: %w", err)
			}
			return respErr
		}
		if respData != nil {
			err = json.Unmarshal(rawData.Data, &respData)
			if err != nil {
				return fmt.Errorf("failed to parse response: %w", err)
			}
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("context finished: %w", ctx.Err())
	}
}

func (ipc *Processor) callHandler(msg *Message, handler HandlerFunc) {
	defer func() {
		err := recover()
		if err != nil {
			ipc.log.Errorfln("Panic in IPC handler for %s: %v:\n%s", msg.Command, err, string(debug.Stack()))
			ipc.respond(msg.ID, err)
		}
	}()
	resp := handler(msg.Data)
	ipc.respond(msg.ID, resp)
}

func (ipc *Processor) respond(id int, response interface{}) {
	if id == 0 && response == nil {
		// No point in replying
		return
	}
	resp := OutgoingMessage{Command: CommandResponse, ID: id, Data: response}
	respErr, isError := response.(error)
	if isError {
		_, isRealError := respErr.(Error)
		if isRealError {
			resp.Data = respErr
		} else {
			resp.Data = Error{
				Code:    "error",
				Message: respErr.Error(),
			}
		}
		resp.Command = CommandError
	}
	ipc.lock.Lock()
	err := ipc.stdout.Encode(resp)
	ipc.lock.Unlock()
	if err != nil {
		ipc.log.Errorln("Failed to encode IPC response: %v", err)
	}
}

func (ipc *Processor) SetHandler(command Command, handler HandlerFunc) {
	ipc.handlers[command] = handler
}
