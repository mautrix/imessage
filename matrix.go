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

package main

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"maunium.net/go/maulogger/v2"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-imessage/imessage"
)

const DefaultSyncProxyBackoff = 1 * time.Second
const MaxSyncProxyBackoff = 60 * time.Second

type WebsocketCommandHandler struct {
	bridge      *IMBridge
	log         maulogger.Logger
	errorTxnIDC *appservice.TransactionIDCache

	lastSyncProxyError time.Time
	syncProxyBackoff   time.Duration
	syncProxyWaiting   int64
}

func NewWebsocketCommandHandler(br *IMBridge) *WebsocketCommandHandler {
	handler := &WebsocketCommandHandler{
		bridge:           br,
		log:              br.Log.Sub("MatrixWebsocket"),
		errorTxnIDC:      appservice.NewTransactionIDCache(8),
		syncProxyBackoff: DefaultSyncProxyBackoff,
	}
	br.AS.PrepareWebsocket()
	br.AS.SetWebsocketCommandHandler("ping", handler.handleWSPing)
	br.AS.SetWebsocketCommandHandler("syncproxy_error", handler.handleWSSyncProxyError)
	br.AS.SetWebsocketCommandHandler("start_dm", handler.handleWSStartDM)
	br.AS.SetWebsocketCommandHandler("resolve_identifier", handler.handleWSStartDM)
	br.AS.SetWebsocketCommandHandler("list_contacts", handler.handleWSGetContacts)
	br.AS.SetWebsocketCommandHandler("edit_ghost", handler.handleWSEditGhost)
	return handler
}

func (mx *WebsocketCommandHandler) handleWSPing(cmd appservice.WebsocketCommand) (bool, interface{}) {
	var status imessage.BridgeStatus

	if mx.bridge.latestState != nil {
		status = *mx.bridge.latestState
	} else {
		status = imessage.BridgeStatus{
			StateEvent: BridgeStatusConnected,
			Timestamp:  time.Now().Unix(),
			TTL:        600,
			Source:     "bridge",
		}
	}

	return true, &status
}

func (mx *WebsocketCommandHandler) handleWSSyncProxyError(cmd appservice.WebsocketCommand) (bool, interface{}) {
	var data mautrix.RespError
	err := json.Unmarshal(cmd.Data, &data)

	if err != nil {
		mx.log.Warnln("Failed to unmarshal syncproxy_error data:", err)
	} else if txnID, ok := data.ExtraData["txn_id"].(string); !ok {
		mx.log.Warnln("Got syncproxy_error data with no transaction ID")
	} else if mx.errorTxnIDC.IsProcessed(txnID) {
		mx.log.Debugln("Ignoring syncproxy_error with duplicate transaction ID", txnID)
	} else {
		go mx.HandleSyncProxyError(&data, nil)
		mx.errorTxnIDC.MarkProcessed(txnID)
	}

	return true, &data
}

type ProfileOverride struct {
	Displayname string `json:"displayname,omitempty"`
	PhotoURL    string `json:"photo_url,omitempty"`
}

type EditGhostRequest struct {
	UserID id.UserID `json:"user_id"`
	RoomID id.RoomID `json:"room_id"`
	Reset  bool      `json:"reset"`
	ProfileOverride
}

func (mx *WebsocketCommandHandler) handleWSEditGhost(cmd appservice.WebsocketCommand) (bool, interface{}) {
	var req EditGhostRequest
	if err := json.Unmarshal(cmd.Data, &req); err != nil {
		return false, fmt.Errorf("failed to parse request: %w", err)
	}
	var puppet *Puppet
	if req.UserID != "" {
		puppet = mx.bridge.GetPuppetByMXID(req.UserID)
		if puppet == nil {
			return false, fmt.Errorf("user is not a bridge ghost")
		}
	} else if req.RoomID != "" {
		portal := mx.bridge.GetPortalByMXID(req.RoomID)
		if portal == nil {
			return false, fmt.Errorf("unknown room ID provided")
		} else if !portal.IsPrivateChat() {
			return false, fmt.Errorf("provided room is not a direct chat")
		} else if puppet = portal.GetDMPuppet(); puppet == nil {
			return false, fmt.Errorf("unexpected error: private chat portal doesn't have ghost")
		}
	} else {
		return false, fmt.Errorf("neither room nor user ID were provided")
	}
	if req.Reset {
		puppet.log.Debugfln("Marking name as not overridden and resyncing profile")
		puppet.NameOverridden = false
		puppet.Update()
		puppet.Sync()
	} else {
		puppet.log.Debugfln("Updating profile with %+v", req.ProfileOverride)
		puppet.SyncWithProfileOverride(req.ProfileOverride)
	}
	return true, struct{}{}
}

type StartDMRequest struct {
	Identifier string `json:"identifier"`
	ProfileOverride

	ActuallyStart bool `json:"-"`
}

type StartDMResponse struct {
	RoomID      id.RoomID `json:"room_id,omitempty"`
	GUID        string    `json:"guid"`
	JustCreated bool      `json:"just_created"`
}

func (mx *WebsocketCommandHandler) handleWSStartDM(cmd appservice.WebsocketCommand) (bool, interface{}) {
	var req StartDMRequest
	if err := json.Unmarshal(cmd.Data, &req); err != nil {
		return false, fmt.Errorf("failed to parse request: %w", err)
	}
	req.ActuallyStart = cmd.Command == "start_dm"
	resp, err := mx.StartChat(req)
	if err != nil {
		return false, err
	} else {
		return true, resp
	}
}

func (mx *WebsocketCommandHandler) handleWSGetContacts(_ appservice.WebsocketCommand) (bool, interface{}) {
	contacts, err := mx.bridge.IM.GetContactList()
	if err != nil {
		return false, err
	}
	return true, contacts
}

func (mx *WebsocketCommandHandler) StartChat(req StartDMRequest) (*StartDMResponse, error) {
	var resp StartDMResponse
	var err error

	prepareDM := func() (*Portal, error) {
		// this is done if and only if ActuallyStart is true, so that the user can see that they would only have SMS behavior
		// this ensures that an iMessage room is created, instead of a bricked SMS room + an iMessage room
		if mx.bridge.IM.Capabilities().MergedChats {
			parsed := imessage.ParseIdentifier(resp.GUID)
			parsed.Service = "iMessage"
			resp.GUID = parsed.String()
		}
		if err := mx.bridge.IM.PrepareDM(resp.GUID); err != nil {
			return nil, err
		}
		return mx.bridge.GetPortalByGUID(resp.GUID), nil
	}

	if resp.GUID, err = mx.bridge.IM.ResolveIdentifier(req.Identifier); err != nil {
		return nil, fmt.Errorf("failed to resolve identifier: %w", err)
	} else if portal := mx.bridge.GetPortalByGUID(resp.GUID); len(portal.MXID) > 0 || !req.ActuallyStart {
		resp.RoomID = portal.MXID
		return &resp, nil
	} else if portal, err = prepareDM(); err != nil {
		return nil, fmt.Errorf("failed to prepare DM: %w", err)
	} else if err = portal.CreateMatrixRoom(nil, &req.ProfileOverride); err != nil {
		return nil, fmt.Errorf("failed to create Matrix room: %w", err)
	} else {
		resp.JustCreated = true
		resp.RoomID = portal.MXID
		return &resp, nil
	}
}

func (mx *WebsocketCommandHandler) HandleSyncProxyError(syncErr *mautrix.RespError, startErr error) {
	if !atomic.CompareAndSwapInt64(&mx.syncProxyWaiting, 0, 1) {
		var err interface{} = startErr
		if err == nil {
			err = syncErr.Err
		}
		mx.log.Debugfln("Got sync proxy error (%v), but there's already another thread waiting to restart sync proxy", err)
		return
	}
	if time.Now().Sub(mx.lastSyncProxyError) < MaxSyncProxyBackoff {
		mx.syncProxyBackoff *= 2
		if mx.syncProxyBackoff > MaxSyncProxyBackoff {
			mx.syncProxyBackoff = MaxSyncProxyBackoff
		}
	} else {
		mx.syncProxyBackoff = DefaultSyncProxyBackoff
	}
	mx.lastSyncProxyError = time.Now()
	if syncErr != nil {
		mx.log.Errorfln("Syncproxy told us that syncing failed: %s - Requesting a restart in %s", syncErr.Err, mx.syncProxyBackoff)
	} else if startErr != nil {
		mx.log.Errorfln("Failed to request sync proxy to start syncing: %v - Requesting a restart in %s", startErr, mx.syncProxyBackoff)
	}
	time.Sleep(mx.syncProxyBackoff)
	atomic.StoreInt64(&mx.syncProxyWaiting, 0)
	mx.bridge.RequestStartSync()
}
