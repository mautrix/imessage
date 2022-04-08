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

package database

import (
	"database/sql"
	"errors"

	log "maunium.net/go/maulogger/v2"

	"maunium.net/go/mautrix/id"
)

type PortalQuery struct {
	db  *Database
	log log.Logger
}

func (pq *PortalQuery) New() *Portal {
	return &Portal{
		db:  pq.db,
		log: pq.log,

		inSpaceCache: make(map[string]bool),
	}
}

func (pq *PortalQuery) Count() (count int) {
	err := pq.db.QueryRow("SELECT COUNT(*) FROM portal").Scan(&count)
	if err != nil {
		pq.log.Warnln("Failed to scan number of portals:", err)
		count = -1
	}
	return
}

func (pq *PortalQuery) GetAll() []*Portal {
	return pq.getAll("SELECT * FROM portal")
}

func (pq *PortalQuery) GetByGUID(guid string) *Portal {
	return pq.get("SELECT * FROM portal WHERE guid=$1", guid)
}

func (pq *PortalQuery) GetByMXID(mxid id.RoomID) *Portal {
	return pq.get("SELECT * FROM portal WHERE mxid=$1", mxid)
}

func (pq *PortalQuery) FindPrivateChats() []*Portal {
	// TODO make sure this is right
	return pq.getAll("SELECT * FROM portal WHERE guid LIKE '%;-;%'")
}

func (pq *PortalQuery) FindPrivateChatsNotInSpace() (keys []string) {
	rows, err := pq.db.Query(`
		SELECT guid FROM portal WHERE mxid<>'' AND (in_space=false OR in_space IS NULL)
	`)
	if err != nil || rows == nil {
		return
	}
	for rows.Next() {
		var guid string
		err = rows.Scan(&guid)
		if err == nil {
			keys = append(keys)
		}
	}
	return
}

func (pq *PortalQuery) getAll(query string, args ...interface{}) (portals []*Portal) {
	rows, err := pq.db.Query(query, args...)
	if err != nil || rows == nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		portals = append(portals, pq.New().Scan(rows))
	}
	return
}

func (pq *PortalQuery) get(query string, args ...interface{}) *Portal {
	row := pq.db.QueryRow(query, args...)
	if row == nil {
		return nil
	}
	return pq.New().Scan(row)
}

type Portal struct {
	db  *Database
	log log.Logger

	GUID string
	MXID id.RoomID

	inSpaceCache map[string]bool

	Name       string
	AvatarHash *[32]byte
	AvatarURL  id.ContentURI
	Encrypted  bool

	BackfillStartTS int64

	in_space bool
}

func (portal *Portal) avatarHashSlice() []byte {
	if portal.AvatarHash == nil {
		return nil
	}
	return (*portal.AvatarHash)[:]
}

func (portal *Portal) Scan(row Scannable) *Portal {
	var mxid, avatarURL sql.NullString
	var avatarHashSlice []byte
	err := row.Scan(&portal.GUID, &mxid, &portal.Name, &avatarHashSlice, &avatarURL, &portal.Encrypted, &portal.BackfillStartTS, &portal.in_space)
	if err != nil {
		if err != sql.ErrNoRows {
			portal.log.Errorln("Database scan failed:", err)
		}
		return nil
	}
	portal.MXID = id.RoomID(mxid.String)
	portal.AvatarURL, _ = id.ParseContentURI(avatarURL.String)
	if avatarHashSlice != nil || len(avatarHashSlice) == 32 {
		var avatarHash [32]byte
		copy(avatarHash[:], avatarHashSlice)
		portal.AvatarHash = &avatarHash
	}
	return portal
}

func (portal *Portal) mxidPtr() *id.RoomID {
	if len(portal.MXID) > 0 {
		return &portal.MXID
	}
	return nil
}

func (portal *Portal) Insert() {
	_, err := portal.db.Exec("INSERT INTO portal (guid, mxid, name, avatar_hash, avatar_url, encrypted, backfill_start_ts) VALUES ($1, $2, $3, $4, $5, $6, $7)",
		portal.GUID, portal.mxidPtr(), portal.Name, portal.avatarHashSlice(), portal.AvatarURL.String(), portal.Encrypted, portal.BackfillStartTS)
	if err != nil {
		portal.log.Warnfln("Failed to insert %s: %v", portal.GUID, err)
	}
}

func (portal *Portal) Update() {
	var mxid *id.RoomID
	if len(portal.MXID) > 0 {
		mxid = &portal.MXID
	}
	_, err := portal.db.Exec("UPDATE portal SET mxid=$1, name=$2, avatar_hash=$3, avatar_url=$4, encrypted=$5, backfill_start_ts=$6 WHERE guid=$7",
		mxid, portal.Name, portal.avatarHashSlice(), portal.AvatarURL.String(), portal.Encrypted, portal.BackfillStartTS, portal.GUID)
	if err != nil {
		portal.log.Warnfln("Failed to update %s: %v", portal.GUID, err)
	}
}

func (portal *Portal) ReID(newGUID string) {
	_, err := portal.db.Exec("UPDATE portal SET guid=$1 WHERE guid=$2", newGUID, portal.GUID)
	if err != nil {
		portal.log.Warnfln("Failed to re-id %s: %v", portal.GUID, err)
	} else {
		portal.GUID = newGUID
	}
}

func (portal *Portal) Delete() {
	_, err := portal.db.Exec("DELETE FROM portal WHERE guid=$1", portal.GUID)
	if err != nil {
		portal.log.Warnfln("Failed to delete %s: %v", portal.GUID, err)
	}
}

func (portal *Portal) IsInSpace(guid string) bool {
	if cached, ok := portal.inSpaceCache[guid]; ok {
		return cached
	}
	var inSpace bool
	err := portal.db.QueryRow("SELECT in_space FROM portal WHERE guid=$1", guid).Scan(&inSpace)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		portal.log.Warnfln("Failed to scan in space status from portal table: %v", err)
	}
	portal.inSpaceCache[guid] = inSpace
	return inSpace
}

func (portal *Portal) MarkInSpace(guid string) {
	_, err := portal.db.Exec(`
		INSERT INTO portal (guid, mxid, name, in_space) VALUES ($1, $2, $3, true)
			ON CONFLICT (guid) DO UPDATE SET in_space=true
		`, portal.GUID, portal.mxidPtr(), portal.Name)
	if err != nil {
		portal.log.Warnfln("Failed to update in space status: %v", err)
	} else {
		portal.inSpaceCache[guid] = true
	}
}
