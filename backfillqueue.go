// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2023 Tulir Asokan, Sumner Evans
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
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	log "maunium.net/go/maulogger/v2"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-imessage/database"
	"go.mau.fi/mautrix-imessage/imessage"
)

type BackfillQueue struct {
	BackfillQuery  *database.BackfillQuery
	reCheckChannel chan bool
	log            log.Logger
}

func (bq *BackfillQueue) ReCheck() {
	bq.log.Infofln("Sending re-checks to channel")
	go func() {
		bq.reCheckChannel <- true
	}()
}

func (bq *BackfillQueue) GetNextBackfill(userID id.UserID) *database.Backfill {
	for {
		if backfill := bq.BackfillQuery.GetNext(userID); backfill != nil {
			backfill.MarkDispatched()
			return backfill
		}

		select {
		case <-bq.reCheckChannel:
		case <-time.After(time.Minute):
		}
	}
}

func (user *User) HandleBackfillRequestsLoop(ctx context.Context) {
	log := zerolog.Ctx(ctx).With().Str("component", "backfill_requests_loop").Logger()

	for {
		if count, err := user.bridge.DB.Backfill.Count(ctx, user.MXID); err != nil {
			user.setBackfillError(log, err, "Failed to get the number of backfills")
			return
		} else if incompleteCount, err := user.bridge.DB.Backfill.IncompleteCount(ctx, user.MXID); err != nil {
			user.setBackfillError(log, err, "Failed to get the number of incomplete backfills")
			return
		} else if count > 0 && incompleteCount == 0 {
			log.Info().
				Int("num_backfills", count).
				Msg("No incomplete backfills, setting status to done")
			user.setBackfillDone(log)
			time.Sleep(5 * time.Second)
			continue
		}

		log.Info().Msg("Getting backfill request")
		req := user.BackfillQueue.GetNextBackfill(user.MXID)
		log.Info().Interface("req", req).Msg("Handling backfill request")

		portal := user.bridge.GetPortalByGUID(req.PortalGUID)
		user.backfillInChunks(req, portal)
		req.MarkDone()
	}
}

func (user *User) EnqueueImmediateBackfill(txn dbutil.Execable, priority int, portal *Portal, earliestBridgedTimestamp time.Time) {
	maxMessages := user.bridge.Config.Bridge.Backfill.Immediate.MaxEvents
	initialBackfill := user.bridge.DB.Backfill.NewWithValues(user.MXID, priority, portal.GUID, nil, &earliestBridgedTimestamp, maxMessages, maxMessages, 0)
	initialBackfill.Insert(txn)
}

func (user *User) EnqueueDeferredBackfills(txn dbutil.Execable, portals []*Portal, startIdx int) {
	now := time.Now()
	numPortals := len(portals)
	for stageIdx, backfillStage := range user.bridge.Config.Bridge.Backfill.Deferred {
		for portalIdx, portal := range portals {
			var startDate *time.Time = nil
			if backfillStage.StartDaysAgo > 0 {
				startDaysAgo := now.AddDate(0, 0, -backfillStage.StartDaysAgo)
				startDate = &startDaysAgo
			}
			backfillMessages := user.bridge.DB.Backfill.NewWithValues(
				user.MXID, startIdx+stageIdx*numPortals+portalIdx, portal.GUID, startDate, nil, backfillStage.MaxBatchEvents, -1, backfillStage.BatchDelay)
			backfillMessages.Insert(txn)
		}
	}
}

type BackfillStatus string

const (
	BackfillStatusUnknown               BackfillStatus = "unknown"
	BackfillStatusLockedByAnotherDevice BackfillStatus = "locked_by_another_device"
	BackfillStatusRunning               BackfillStatus = "running"
	BackfillStatusError                 BackfillStatus = "error"
	BackfillStatusDone                  BackfillStatus = "done"
)

type BackfillInfo struct {
	Status BackfillStatus `json:"status"`
	Error  string         `json:"error,omitempty"`
}

func (user *User) GetBackfillInfo() BackfillInfo {
	backfillInfo := BackfillInfo{Status: BackfillStatusUnknown}
	if user.backfillStatus != "" {
		backfillInfo.Status = user.backfillStatus
	}
	if user.backfillError != nil {
		backfillInfo.Error = user.backfillError.Error()
	}
	return backfillInfo
}

func (user *User) setBackfillError(log zerolog.Logger, err error, msg string) {
	log.Err(err).Msg(msg)
	user.backfillStatus = BackfillStatusError
	user.backfillError = fmt.Errorf("%s: %w", msg, err)
}

type BackfillStateAccountData struct {
	DeviceID id.DeviceID `json:"device_id"`
	Done     bool        `json:"done"`
}

func (user *User) setBackfillDone(log zerolog.Logger) {
	log.Info().
		Str("device_id", user.bridge.Config.Bridge.DeviceID).
		Msg("Setting backfill state account data to done")
	err := user.bridge.Bot.SetAccountData("fi.mau.imessage.backfill_state", &BackfillStateAccountData{
		DeviceID: id.DeviceID(user.bridge.Config.Bridge.DeviceID),
		Done:     true,
	})
	if err != nil {
		user.setBackfillError(log, err, "failed to set backfill state account data")
		return
	}
	user.backfillStatus = BackfillStatusDone
}

func (user *User) runOnlyBackfillMode() {
	log := user.bridge.ZLog.With().Str("mode", "backfill_only").Logger()
	ctx := log.WithContext(context.Background())

	// Start the backfill queue. We always want this running so that the
	// desktop app can request the backfill status.
	user.handleHistorySyncsLoop(ctx)

	if !user.bridge.SpecVersions.Supports(mautrix.BeeperFeatureBatchSending) {
		user.setBackfillError(log, nil, "The homeserver does not support Beeper's batch send endpoint")
		return
	}

	if user.bridge.Config.Bridge.DeviceID == "" {
		user.setBackfillError(log, nil, "No device ID set in the config")
		return
	}

	var backfillState BackfillStateAccountData
	err := user.bridge.Bot.GetAccountData("fi.mau.imessage.backfill_state", &backfillState)
	if err != nil {
		if !errors.Is(err, mautrix.MNotFound) {
			user.setBackfillError(log, err, "Error fetching backfill state account data")
			return
		}
	} else if backfillState.DeviceID.String() != user.bridge.Config.Bridge.DeviceID {
		user.backfillStatus = BackfillStatusLockedByAnotherDevice
		log.Warn().
			Str("device_id", backfillState.DeviceID.String()).
			Msg("Backfill already locked for a different device")
		return
	} else if backfillState.Done {
		log.Info().
			Str("device_id", backfillState.DeviceID.String()).
			Msg("Backfill already completed")
		user.backfillStatus = BackfillStatusDone
		return
	}

	if count, err := user.bridge.DB.Backfill.Count(ctx, user.MXID); err != nil {
		user.setBackfillError(log, err, "Failed to get the number of backfills")
		return
	} else if incompleteCount, err := user.bridge.DB.Backfill.IncompleteCount(ctx, user.MXID); err != nil {
		user.setBackfillError(log, err, "Failed to get the number of incomplete backfills")
		return
	} else if count > 0 && incompleteCount == 0 {
		log.Info().
			Int("num_backfills", count).
			Msg("No incomplete backfills, setting status to done")
		user.setBackfillDone(log)
		return
	} else {
		err = user.bridge.Crypto.ShareKeys(context.Background())
		if err != nil {
			user.setBackfillError(log, err, "Error sharing keys")
		}

		err = user.bridge.Bot.SetAccountData("fi.mau.imessage.backfill_state", &BackfillStateAccountData{
			DeviceID: id.DeviceID(user.bridge.Config.Bridge.DeviceID),
			Done:     false,
		})
		if err != nil {
			user.setBackfillError(log, err, "failed to set backfill state account data")
			return
		}
		user.backfillStatus = BackfillStatusRunning

		if count == 0 {
			log.Info().Msg("Starting backfill")
			user.getRoomsForBackfillAndEnqueue(ctx)
		} else {
			log.Info().
				Int("num_backfills", count).
				Int("num_incomplete_backfills", incompleteCount).
				Msg("Resuming backfill")
			// No need to do anything else because the history syncs loop is
			// already running
		}
	}
}

func (user *User) getRoomsForBackfillAndEnqueue(ctx context.Context) {
	log := zerolog.Ctx(ctx).With().Str("method", "getRoomsForBackfillAndEnqueue").Logger()

	// Get every chat from the database
	chats, err := user.bridge.IM.GetChatsWithMessagesAfter(imessage.AppleEpoch)
	if err != nil {
		user.setBackfillError(log, err, "Error retrieving all chats")
		return
	}

	chatGUIDs := make([]string, len(chats))
	chatInfos := make([]*imessage.ChatInfo, len(chats))
	for i, chat := range chats {
		chatGUIDs[i] = chat.ChatGUID
		chatInfos[i], err = user.bridge.IM.GetChatInfo(chat.ChatGUID, chat.ThreadID)
		if err != nil {
			user.setBackfillError(log, err,
				fmt.Sprintf("Error getting chat info for %s from database", chat.ChatGUID))
			return
		}
		chatInfos[i].JSONChatGUID = chatInfos[i].Identifier.String()
	}

	// Ask the cloud bridge to create room IDs for every one of the chats.
	client := user.CustomIntent().Client
	url := client.BuildURL(mautrix.BaseURLPath{
		"_matrix", "asmux", "mxauth", "appservice", user.MXID.Localpart(), "imessagecloud",
		"exec", "create_rooms_for_backfill"})
	_, err = client.MakeRequest("POST", url, NewRoomBackfillRequest{
		Chats: chatInfos,
	}, nil)
	if err != nil {
		user.setBackfillError(log, err, "Error starting creation of backfill rooms")
		return
	}

	// Wait for the rooms to be created.
	var roomInfoResp RoomInfoForBackfillResponse
	for {
		url = client.BuildURL(mautrix.BaseURLPath{
			"_matrix", "asmux", "mxauth", "appservice", user.MXID.Localpart(), "imessagecloud",
			"exec", "get_room_info_for_backfill"})
		_, err = client.MakeRequest("POST", url, RoomInfoForBackfillRequest{
			ChatGUIDs: chatGUIDs,
		}, &roomInfoResp)
		if err != nil {
			user.setBackfillError(log, err, "Error requesting backfill room info")
			return
		}

		if roomInfoResp.Status == RoomCreationForBackfillStatusDone {
			break
		} else if roomInfoResp.Status == RoomCreationForBackfillStatusError {
			user.setBackfillError(log, fmt.Errorf(roomInfoResp.Error), "Error requesting backfill room IDs")
			return
		} else if roomInfoResp.Status == RoomCreationForBackfillStatusInProgress {
			log.Info().Msg("Backfill room creation still in progress, waiting 5 seconds")
			time.Sleep(5 * time.Second)
		} else {
			user.setBackfillError(log, fmt.Errorf("Unknown status %s", roomInfoResp.Status), "Error requesting backfill room IDs")
			return
		}
	}

	// Create all of the portals locally and enqueue backfill requests for
	// all of them.
	txn, err := user.bridge.DB.Begin()
	{
		portals := []*Portal{}
		var i int
		for _, chatIdentifier := range chats {
			roomInfo := roomInfoResp.Rooms[chatIdentifier.ChatGUID]
			portal := user.bridge.GetPortalByGUIDWithTransaction(txn, chatIdentifier.ChatGUID)
			portal.MXID = roomInfo.RoomID
			portal.Update(txn)
			portals = append(portals, portal)
			user.EnqueueImmediateBackfill(txn, i, portal, time.UnixMilli(roomInfo.EarliestBridgedTimestamp))
			i++
		}
		user.EnqueueDeferredBackfills(txn, portals, i)
	}
	if err = txn.Commit(); err != nil {
		user.setBackfillError(log, err, "Error committing backfill room IDs")
		return
	}
}
