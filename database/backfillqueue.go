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

package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	log "maunium.net/go/maulogger/v2"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/util/dbutil"
)

type BackfillQuery struct {
	db  *Database
	log log.Logger

	backfillQueryLock sync.Mutex
}

func (bq *BackfillQuery) New() *Backfill {
	return &Backfill{
		db:  bq.db,
		log: bq.log,
	}
}

func (bq *BackfillQuery) NewWithValues(userID id.UserID, priority int, portalGUID string, timeStart, timeEnd *time.Time, maxBatchEvents, maxTotalEvents, batchDelay int) *Backfill {
	return &Backfill{
		db:             bq.db,
		log:            bq.log,
		UserID:         userID,
		Priority:       priority,
		PortalGUID:     portalGUID,
		TimeStart:      timeStart,
		TimeEnd:        timeEnd,
		MaxBatchEvents: maxBatchEvents,
		MaxTotalEvents: maxTotalEvents,
		BatchDelay:     batchDelay,
	}
}

const (
	getNextBackfillQuery = `
		SELECT queue_id, user_mxid, priority, portal_guid, time_start, time_end, max_batch_events, max_total_events, batch_delay
		FROM backfill_queue
		WHERE user_mxid=$1
			AND (
				dispatch_time IS NULL
				OR (
					dispatch_time < $2
					AND completed_at IS NULL
				)
			)
		ORDER BY priority
		LIMIT 1
	`
	getIncompleteCountQuery = `
		SELECT COUNT(*)
		FROM backfill_queue
		WHERE user_mxid=$1 AND completed_at IS NULL
	`
	getCountQuery = `
		SELECT COUNT(*)
		FROM backfill_queue
		WHERE user_mxid=$1
	`
	getUnstartedOrInFlightQuery = `
		SELECT 1
		FROM backfill_queue
		WHERE user_mxid=$1
			AND (dispatch_time IS NULL OR completed_at IS NULL)
		LIMIT 1
	`
)

// GetNext returns the next backfill to perform
func (bq *BackfillQuery) GetNext(userID id.UserID) (backfill *Backfill) {
	bq.backfillQueryLock.Lock()
	defer bq.backfillQueryLock.Unlock()

	rows, err := bq.db.Query(getNextBackfillQuery, userID, time.Now().Add(-15*time.Minute))
	if err != nil || rows == nil {
		bq.log.Errorfln("Failed to query next backfill queue job: %v", err)
		return
	}
	defer rows.Close()
	if rows.Next() {
		backfill = bq.New().Scan(rows)
	}
	return
}

func (bq *BackfillQuery) IncompleteCount(ctx context.Context, userID id.UserID) (incompleteCount int, err error) {
	bq.backfillQueryLock.Lock()
	defer bq.backfillQueryLock.Unlock()

	row := bq.db.QueryRowContext(ctx, getIncompleteCountQuery, userID)
	err = row.Scan(&incompleteCount)
	return
}

func (bq *BackfillQuery) Count(ctx context.Context, userID id.UserID) (count int, err error) {
	bq.backfillQueryLock.Lock()
	defer bq.backfillQueryLock.Unlock()

	row := bq.db.QueryRowContext(ctx, getCountQuery, userID)
	err = row.Scan(&count)
	return
}

func (bq *BackfillQuery) DeleteAll(userID id.UserID) {
	bq.backfillQueryLock.Lock()
	defer bq.backfillQueryLock.Unlock()
	_, err := bq.db.Exec("DELETE FROM backfill_queue WHERE user_mxid=$1", userID)
	if err != nil {
		bq.log.Warnfln("Failed to delete backfill queue items for %s: %v", userID, err)
	}
}

func (bq *BackfillQuery) DeleteAllForPortal(userID id.UserID, portalGUID string) {
	bq.backfillQueryLock.Lock()
	defer bq.backfillQueryLock.Unlock()
	_, err := bq.db.Exec(`
		DELETE FROM backfill_queue
		WHERE user_mxid=$1
			AND portal_guid=$2
	`, userID, portalGUID)
	if err != nil {
		bq.log.Warnfln("Failed to delete backfill queue items for %s/%s: %v", userID, portalGUID, err)
	}
}

type Backfill struct {
	db  *Database
	log log.Logger

	// Fields
	QueueID        int
	UserID         id.UserID
	Priority       int
	PortalGUID     string
	TimeStart      *time.Time
	TimeEnd        *time.Time
	MaxBatchEvents int
	MaxTotalEvents int
	BatchDelay     int
	DispatchTime   *time.Time
	CompletedAt    *time.Time
}

func (b *Backfill) String() string {
	return fmt.Sprintf("Backfill{QueueID: %d, UserID: %s, Priority: %d, Portal: %s, TimeStart: %s, TimeEnd: %s, MaxBatchEvents: %d, MaxTotalEvents: %d, BatchDelay: %d, DispatchTime: %s, CompletedAt: %s}",
		b.QueueID, b.UserID, b.Priority, b.PortalGUID, b.TimeStart, b.TimeEnd, b.MaxBatchEvents, b.MaxTotalEvents, b.BatchDelay, b.CompletedAt, b.DispatchTime,
	)
}

func (b *Backfill) Scan(row dbutil.Scannable) *Backfill {
	var maxTotalEvents, batchDelay sql.NullInt32
	err := row.Scan(&b.QueueID, &b.UserID, &b.Priority, &b.PortalGUID, &b.TimeStart, &b.TimeEnd, &b.MaxBatchEvents, &maxTotalEvents, &batchDelay)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			b.log.Errorln("Database scan failed:", err)
		}
		return nil
	}
	b.MaxTotalEvents = int(maxTotalEvents.Int32)
	b.BatchDelay = int(batchDelay.Int32)
	return b
}

func (b *Backfill) Insert(txn dbutil.Execable) {
	b.db.Backfill.backfillQueryLock.Lock()
	defer b.db.Backfill.backfillQueryLock.Unlock()

	if txn == nil {
		txn = b.db
	}
	rows, err := txn.Query(`
		INSERT INTO backfill_queue
			(user_mxid, priority, portal_guid, time_start, time_end, max_batch_events, max_total_events, batch_delay, dispatch_time, completed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING queue_id
	`, b.UserID, b.Priority, b.PortalGUID, b.TimeStart, b.TimeEnd, b.MaxBatchEvents, b.MaxTotalEvents, b.BatchDelay, b.DispatchTime, b.CompletedAt)
	defer rows.Close()
	if err != nil || !rows.Next() {
		b.log.Warnfln("Failed to insert backfill for %s with priority %d: %v", b.PortalGUID, b.Priority, err)
		return
	}
	err = rows.Scan(&b.QueueID)
	if err != nil {
		b.log.Warnfln("Failed to insert backfill for %s with priority %d: %v", b.PortalGUID, b.Priority, err)
	}
}

func (b *Backfill) MarkDispatched() {
	b.db.Backfill.backfillQueryLock.Lock()
	defer b.db.Backfill.backfillQueryLock.Unlock()

	if b.QueueID == 0 {
		b.log.Errorfln("Cannot mark backfill as dispatched without queue_id. Maybe it wasn't actually inserted in the database?")
		return
	}
	_, err := b.db.Exec("UPDATE backfill_queue SET dispatch_time=$1 WHERE queue_id=$2", time.Now(), b.QueueID)
	if err != nil {
		b.log.Warnfln("Failed to mark backfill with priority %d as dispatched: %v", b.Priority, err)
	}
}

func (b *Backfill) MarkDone() {
	b.db.Backfill.backfillQueryLock.Lock()
	defer b.db.Backfill.backfillQueryLock.Unlock()

	if b.QueueID == 0 {
		b.log.Errorfln("Cannot mark backfill done without queue_id. Maybe it wasn't actually inserted in the database?")
		return
	}
	_, err := b.db.Exec("UPDATE backfill_queue SET completed_at=$1 WHERE queue_id=$2", time.Now(), b.QueueID)
	if err != nil {
		b.log.Warnfln("Failed to mark backfill with priority %d as complete: %v", b.Priority, err)
	}
}

func (bq *BackfillQuery) NewBackfillState(userID id.UserID, portalGUID string) *BackfillState {
	return &BackfillState{
		db:         bq.db,
		log:        bq.log,
		UserID:     userID,
		PortalGUID: portalGUID,
	}
}

const (
	getBackfillState = `
		SELECT user_mxid, portal_guid, processing_batch, backfill_complete, first_expected_ts
		FROM backfill_state
		WHERE user_mxid=$1
			AND portal_guid=$2
	`
)

type BackfillState struct {
	db  *Database
	log log.Logger

	// Fields
	UserID                 id.UserID
	PortalGUID             string
	ProcessingBatch        bool
	BackfillComplete       bool
	FirstExpectedTimestamp uint64
}

func (b *BackfillState) Scan(row dbutil.Scannable) *BackfillState {
	err := row.Scan(&b.UserID, &b.PortalGUID, &b.ProcessingBatch, &b.BackfillComplete, &b.FirstExpectedTimestamp)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			b.log.Errorln("Database scan failed:", err)
		}
		return nil
	}
	return b
}

func (b *BackfillState) Upsert() {
	_, err := b.db.Exec(`
		INSERT INTO backfill_state
			(user_mxid, portal_guid, processing_batch, backfill_complete, first_expected_ts)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_mxid, portal_guid)
		DO UPDATE SET
			processing_batch=EXCLUDED.processing_batch,
			backfill_complete=EXCLUDED.backfill_complete,
			first_expected_ts=EXCLUDED.first_expected_ts`,
		b.UserID, b.PortalGUID, b.ProcessingBatch, b.BackfillComplete, b.FirstExpectedTimestamp)
	if err != nil {
		b.log.Warnfln("Failed to insert backfill state for %s: %v", b.PortalGUID, err)
	}
}

func (b *BackfillState) SetProcessingBatch(processing bool) {
	b.ProcessingBatch = processing
	b.Upsert()
}

func (bq *BackfillQuery) GetBackfillState(userID id.UserID, portalGUID string) (backfillState *BackfillState) {
	rows, err := bq.db.Query(getBackfillState, userID, portalGUID)
	if err != nil || rows == nil {
		bq.log.Error(err)
		return
	}
	defer rows.Close()
	if rows.Next() {
		backfillState = bq.NewBackfillState(userID, portalGUID).Scan(rows)
	}
	return
}
