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
	"strconv"
	"strings"
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

	createRoomsForBackfillError error
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
	br.AS.SetWebsocketCommandHandler("create_group", handler.handleWSCreateGroup)
	br.AS.SetWebsocketCommandHandler("start_dm", handler.handleWSStartDM)
	br.AS.SetWebsocketCommandHandler("resolve_identifier", handler.handleWSStartDM)
	br.AS.SetWebsocketCommandHandler("list_contacts", handler.handleWSGetContacts)
	br.AS.SetWebsocketCommandHandler("upload_contacts", handler.handleWSUploadContacts)
	br.AS.SetWebsocketCommandHandler("edit_ghost", handler.handleWSEditGhost)
	br.AS.SetWebsocketCommandHandler("do_hacky_test", handler.handleWSHackyTest)
	br.AS.SetWebsocketCommandHandler("create_rooms_for_backfill", handler.handleCreateRoomsForBackfill)
	br.AS.SetWebsocketCommandHandler("get_room_info_for_backfill", handler.handleGetRoomInfoForBackfill)
	return handler
}

func (mx *WebsocketCommandHandler) handleWSHackyTest(cmd appservice.WebsocketCommand) (bool, any) {
	mx.log.Debugfln("Starting hacky test due to manual request")
	mx.bridge.hackyStartupTests(false, true)
	return true, nil
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
	Force      bool   `json:"force"`
	ProfileOverride

	ActuallyStart bool `json:"-"`
}

type CreateGroupRequest struct {
	Users []string `json:"users"`

	AllowSMS bool `json:"allow_sms"`
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
		mx.log.Errorfln("Error in %s handler: %v", cmd.Command, err)
		return false, err
	} else {
		return true, resp
	}
}

func (mx *WebsocketCommandHandler) handleWSCreateGroup(cmd appservice.WebsocketCommand) (bool, interface{}) {
	var req CreateGroupRequest
	err := json.Unmarshal(cmd.Data, &req)
	if err != nil {
		return false, fmt.Errorf("failed to parse request: %w", err)
	}
	guids := make([]string, len(req.Users))
	for i, identifier := range req.Users {
		if strings.HasPrefix(identifier, "iMessage;-;") || strings.HasPrefix(identifier, "SMS;-;") {
			guids[i] = identifier
		} else {
			guids[i], err = mx.bridge.IM.ResolveIdentifier(identifier)
			if err != nil {
				return false, fmt.Errorf("failed to resolve identifier %s: %w", identifier, err)
			}
		}
		if strings.HasPrefix(guids[i], "SMS;-;") && !req.AllowSMS {
			return false, fmt.Errorf("%s is only available on SMS", identifier)
		}
	}
	mx.log.Debugfln("Creating group with guids %+v (resolved from identifiers %+v)", guids, req.Users)
	createResp, err := mx.bridge.IM.CreateGroup(guids)
	if err != nil {
		return false, fmt.Errorf("failed to create group: %w", err)
	}
	mx.log.Infofln("Created group %s (%s)", createResp.GUID, createResp.ThreadID)
	portal := mx.bridge.GetPortalByGUID(createResp.GUID)
	portal.ThreadID = createResp.ThreadID
	resp := StartDMResponse{
		RoomID:      portal.MXID,
		GUID:        createResp.GUID,
		JustCreated: len(portal.MXID) == 0,
	}
	err = portal.CreateMatrixRoom(nil, nil)
	if err != nil {
		return false, fmt.Errorf("failed to create Matrix room: %w", err)
	}
	resp.RoomID = portal.MXID
	return true, &resp
}

func (mx *WebsocketCommandHandler) handleWSGetContacts(_ appservice.WebsocketCommand) (bool, interface{}) {
	contacts, err := mx.bridge.IM.GetContactList()
	if err != nil {
		return false, err
	}
	return true, contacts
}

type UploadContactsRequest struct {
	Contacts []*imessage.Contact `json:"contacts"`
}

func (mx *WebsocketCommandHandler) handleWSUploadContacts(cmd appservice.WebsocketCommand) (bool, any) {
	var req UploadContactsRequest
	if err := json.Unmarshal(cmd.Data, &req); err != nil {
		return false, fmt.Errorf("failed to parse request: %w", err)
	}
	mx.bridge.UpdateMerges(req.Contacts)
	return true, nil
}

func isNumber(number string) bool {
	for _, char := range number {
		if (char < '0' || char > '9') && char != '+' {
			return false
		}
	}
	return true
}

func (mx *WebsocketCommandHandler) trackResolveIdentifier(actuallyTrack bool, identifier, status string) {
	if actuallyTrack {
		identifierType := "unknown"
		if isNumber(identifierType) {
			identifierType = "phone"
		} else if strings.ContainsRune(identifier, '@') {
			identifierType = "email"
		}
		Segment.Track("iMC resolve identifier", map[string]any{
			"status":            status,
			"is_startup_target": strconv.FormatBool(identifier == mx.bridge.Config.HackyStartupTest.Identifier),
			"identifier_type":   identifierType,
			"tmp_identifier":    identifier,
		})
	}
}

func (mx *WebsocketCommandHandler) StartChat(req StartDMRequest) (*StartDMResponse, error) {
	var resp StartDMResponse
	var err error
	var forced bool

	if resp.GUID, err = mx.bridge.IM.ResolveIdentifier(req.Identifier); err != nil {
		if req.Force && req.ActuallyStart {
			mx.log.Debugfln("Failed to resolve identifier %s (%v), but forcing creation anyway", req.Identifier, err)
			resp.GUID = req.Identifier
			forced = true
		} else {
			mx.trackResolveIdentifier(!req.ActuallyStart, req.Identifier, "fail")
			return nil, fmt.Errorf("failed to resolve identifier: %w", err)
		}
	}
	if parsed := imessage.ParseIdentifier(resp.GUID); parsed.Service == "SMS" && !isNumber(parsed.LocalID) {
		mx.trackResolveIdentifier(!req.ActuallyStart, req.Identifier, "fail")
		return nil, fmt.Errorf("can't start SMS with non-numeric identifier")
	} else if portal := mx.bridge.GetPortalByGUID(resp.GUID); len(portal.MXID) > 0 || !req.ActuallyStart {
		status := "success"
		if parsed.Service == "SMS" {
			status = "sms"
		}
		if !forced {
			mx.trackResolveIdentifier(!req.ActuallyStart, req.Identifier, status)
		}
		resp.RoomID = portal.MXID
		return &resp, nil
	} else if err = mx.bridge.IM.PrepareDM(resp.GUID); err != nil {
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

type NewRoomBackfillRequest struct {
	Chats []*imessage.ChatInfo `json:"chats"`
}

func (mx *WebsocketCommandHandler) handleCreateRoomsForBackfill(cmd appservice.WebsocketCommand) (bool, any) {
	var req NewRoomBackfillRequest
	if err := json.Unmarshal(cmd.Data, &req); err != nil {
		return false, fmt.Errorf("failed to parse request: %w", err)
	}

	mx.log.Debugfln("Got request to create %d rooms for backfill", len(req.Chats))
	go func() {
		mx.createRoomsForBackfillError = nil
		for _, info := range req.Chats {
			info.Identifier = imessage.ParseIdentifier(info.JSONChatGUID)
			portals := mx.bridge.FindPortalsByThreadID(info.ThreadID)
			var portal *Portal
			if len(portals) > 1 {
				mx.log.Warnfln("Found multiple portals with thread ID %s (message chat guid: %s)", info.ThreadID, info.Identifier.String())
				continue
			} else if len(portals) == 1 {
				portal = portals[0]
			} else {
				// This will create the new portal
				portal = mx.bridge.GetPortalByGUID(info.Identifier.String())
			}

			if len(portal.MXID) == 0 {
				portal.zlog.Info().Msg("Creating Matrix room with latest chat info")
				err := portal.CreateMatrixRoom(info, nil)
				if err != nil {
					mx.createRoomsForBackfillError = err
					return
				}
			} else {
				portal.zlog.Info().Msg("Syncing Matrix room with latest chat info")
				portal.SyncWithInfo(info)
			}

			mx.log.Debugfln("Room %s created for backfilling %s", portal.MXID, info.JSONChatGUID)
		}
	}()
	return true, struct{}{}
}

type RoomInfoForBackfillRequest struct {
	ChatGUIDs []string `json:"chats_guids"`
}

type RoomInfoForBackfill struct {
	RoomID                   id.RoomID `json:"room_id"`
	EarliestBridgedTimestamp int64     `json:"earliest_bridged_timestamp"`
}

type RoomCreationForBackfillStatus string

const (
	RoomCreationForBackfillStatusInProgress RoomCreationForBackfillStatus = "in-progress"
	RoomCreationForBackfillStatusDone       RoomCreationForBackfillStatus = "done"
	RoomCreationForBackfillStatusError      RoomCreationForBackfillStatus = "error"
)

type RoomInfoForBackfillResponse struct {
	Status RoomCreationForBackfillStatus  `json:"status"`
	Error  string                         `json:"error,omitempty"`
	Rooms  map[string]RoomInfoForBackfill `json:"rooms,omitempty"`
}

func (mx *WebsocketCommandHandler) handleGetRoomInfoForBackfill(cmd appservice.WebsocketCommand) (bool, any) {
	if mx.createRoomsForBackfillError != nil {
		return true, RoomInfoForBackfillResponse{
			Status: RoomCreationForBackfillStatusError,
			Error:  mx.createRoomsForBackfillError.Error(),
		}
	}

	var req RoomInfoForBackfillRequest
	if err := json.Unmarshal(cmd.Data, &req); err != nil {
		return false, fmt.Errorf("failed to parse request: %w", err)
	}

	resp := RoomInfoForBackfillResponse{
		Status: RoomCreationForBackfillStatusDone,
		Rooms:  map[string]RoomInfoForBackfill{},
	}
	now := time.Now().UnixMilli()
	mx.log.Debugfln("Got request to get room info for backfills")
	for _, chatGUID := range req.ChatGUIDs {
		portal := mx.bridge.GetPortalByGUID(chatGUID)

		if len(portal.MXID) == 0 {
			return true, RoomInfoForBackfillResponse{Status: RoomCreationForBackfillStatusInProgress}
		}

		timestamp, err := mx.bridge.DB.Message.GetEarliestTimestampInChat(chatGUID)
		if err != nil || timestamp < 0 {
			timestamp = now
		}
		resp.Rooms[portal.GUID] = RoomInfoForBackfill{
			RoomID:                   portal.MXID,
			EarliestBridgedTimestamp: timestamp,
		}
	}

	return true, resp
}
