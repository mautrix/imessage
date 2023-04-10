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
	"fmt"

	log "maunium.net/go/maulogger/v2"

	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/util/dbutil"
)

type PuppetQuery struct {
	db  *Database
	log log.Logger
}

func (pq *PuppetQuery) New() *Puppet {
	return &Puppet{
		db:  pq.db,
		log: pq.log,
	}
}

const puppetColumns = "id, displayname, name_overridden, avatar_hash, avatar_url, contact_info_set"

func (pq *PuppetQuery) GetAll() (puppets []*Puppet) {
	rows, err := pq.db.Query(fmt.Sprintf("SELECT %s FROM puppet", puppetColumns))
	if err != nil || rows == nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		puppets = append(puppets, pq.New().Scan(rows))
	}
	return
}

func (pq *PuppetQuery) Get(id string) *Puppet {
	row := pq.db.QueryRow(fmt.Sprintf("SELECT %s FROM puppet WHERE id=$1", puppetColumns), id)
	if row == nil {
		return nil
	}
	return pq.New().Scan(row)
}

type Puppet struct {
	db  *Database
	log log.Logger

	ID             string
	Displayname    string
	NameOverridden bool
	AvatarHash     *[32]byte
	AvatarURL      id.ContentURI
	ContactInfoSet bool
}

func (puppet *Puppet) avatarHashSlice() []byte {
	if puppet.AvatarHash == nil {
		return nil
	}
	return (*puppet.AvatarHash)[:]
}

func (puppet *Puppet) Scan(row dbutil.Scannable) *Puppet {
	var avatarURL sql.NullString
	var avatarHashSlice []byte
	err := row.Scan(&puppet.ID, &puppet.Displayname, &puppet.NameOverridden, &avatarHashSlice, &avatarURL, &puppet.ContactInfoSet)
	if err != nil {
		if err != sql.ErrNoRows {
			puppet.log.Errorln("Database scan failed:", err)
		}
		return nil
	}
	puppet.AvatarURL, _ = id.ParseContentURI(avatarURL.String)
	if avatarHashSlice != nil || len(avatarHashSlice) == 32 {
		var avatarHash [32]byte
		copy(avatarHash[:], avatarHashSlice)
		puppet.AvatarHash = &avatarHash
	}
	return puppet
}

func (puppet *Puppet) Insert() {
	_, err := puppet.db.Exec("INSERT INTO puppet (id, displayname, name_overridden, avatar_hash, avatar_url, contact_info_set) VALUES ($1, $2, $3, $4, $5, $6)",
		puppet.ID, puppet.Displayname, puppet.NameOverridden, puppet.avatarHashSlice(), puppet.AvatarURL.String(), puppet.ContactInfoSet)
	if err != nil {
		puppet.log.Warnfln("Failed to insert %s: %v", puppet.ID, err)
	}
}

func (puppet *Puppet) Update() {
	_, err := puppet.db.Exec("UPDATE puppet SET displayname=$1, name_overridden=$2, avatar_hash=$3, avatar_url=$4, contact_info_set=$5 WHERE id=$6",
		puppet.Displayname, puppet.NameOverridden, puppet.avatarHashSlice(), puppet.AvatarURL.String(), puppet.ContactInfoSet, puppet.ID)
	if err != nil {
		puppet.log.Warnfln("Failed to update %s: %v", puppet.ID, err)
	}
}
