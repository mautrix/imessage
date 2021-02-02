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

	log "maunium.net/go/maulogger/v2"

	"maunium.net/go/mautrix/id"
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

func (pq *PuppetQuery) GetAll() (puppets []*Puppet) {
	rows, err := pq.db.Query("SELECT id, displayname, avatar, avatar_url FROM puppet")
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
	row := pq.db.QueryRow("SELECT id, displayname, avatar, avatar_url FROM puppet WHERE id=$1", id)
	if row == nil {
		return nil
	}
	return pq.New().Scan(row)
}

type Puppet struct {
	db  *Database
	log log.Logger

	ID     string
	Displayname string
	Avatar      string
	AvatarURL   id.ContentURI
}

func (puppet *Puppet) Scan(row Scannable) *Puppet {
	var avatarURL sql.NullString
	err := row.Scan(&puppet.ID, &puppet.Displayname, &puppet.Avatar, &avatarURL)
	if err != nil {
		if err != sql.ErrNoRows {
			puppet.log.Errorln("Database scan failed:", err)
		}
		return nil
	}
	puppet.AvatarURL, _ = id.ParseContentURI(avatarURL.String)
	return puppet
}

func (puppet *Puppet) Insert() {
	_, err := puppet.db.Exec("INSERT INTO puppet (id, displayname, avatar, avatar_url) VALUES ($1, $2, $3, $4)",
		puppet.ID, puppet.Displayname, puppet.Avatar, puppet.AvatarURL.String())
	if err != nil {
		puppet.log.Warnfln("Failed to insert %s: %v", puppet.ID, err)
	}
}

func (puppet *Puppet) Update() {
	_, err := puppet.db.Exec("UPDATE puppet SET displayname=$1, avatar=$2, avatar_url=$3 WHERE id=$4",
		puppet.Displayname, puppet.Avatar, puppet.AvatarURL.String(), puppet.ID)
	if err != nil {
		puppet.log.Warnfln("Failed to update %s: %v", puppet.ID, err)
	}
}
