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
	"crypto/sha256"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/gabriel-vasile/mimetype"
	log "maunium.net/go/maulogger/v2"

	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-imessage/database"
	"go.mau.fi/mautrix-imessage/imessage"
)

var userIDRegex *regexp.Regexp

func (bridge *Bridge) ParsePuppetMXID(mxid id.UserID) (string, bool) {
	if userIDRegex == nil {
		userIDRegex = regexp.MustCompile(fmt.Sprintf("^@%s:%s$",
			bridge.Config.Bridge.FormatUsername("(.+)"),
			bridge.Config.Homeserver.Domain))
	}
	match := userIDRegex.FindStringSubmatch(string(mxid))
	if match == nil || len(match) != 2 {
		return "", false
	}

	localID := match[1]

	if strings.Contains(localID, "=40") {
		localpart, err := id.DecodeUserLocalpart(localID)
		if err != nil {
			bridge.Log.Debugfln("Failed to decode user localpart '%s': %v", localID, err)
			return "", false
		}
		return localpart, true
	} else {
		number, err := strconv.Atoi(localID)
		if err != nil {
			return "", false
		}
		return fmt.Sprintf("+%d", number), true
	}
}

func (bridge *Bridge) GetPuppetByMXID(mxid id.UserID) *Puppet {
	localID, ok := bridge.ParsePuppetMXID(mxid)
	if !ok {
		return nil
	}

	return bridge.GetPuppetByLocalID(localID)
}

func (bridge *Bridge) GetPuppetByGUID(guid string) *Puppet {
	return bridge.GetPuppetByLocalID(imessage.ParseIdentifier(guid).LocalID)
}

func (bridge *Bridge) GetPuppetByLocalID(id string) *Puppet {
	bridge.puppetsLock.Lock()
	defer bridge.puppetsLock.Unlock()
	puppet, ok := bridge.puppets[id]
	if !ok {
		dbPuppet := bridge.DB.Puppet.Get(id)
		if dbPuppet == nil {
			dbPuppet = bridge.DB.Puppet.New()
			dbPuppet.ID = id
			dbPuppet.Insert()
		}
		puppet = bridge.NewPuppet(dbPuppet)
		bridge.puppets[puppet.ID] = puppet
	}
	return puppet
}

func (bridge *Bridge) GetAllPuppets() []*Puppet {
	return bridge.dbPuppetsToPuppets(bridge.DB.Puppet.GetAll())
}

func (bridge *Bridge) dbPuppetsToPuppets(dbPuppets []*database.Puppet) []*Puppet {
	bridge.puppetsLock.Lock()
	defer bridge.puppetsLock.Unlock()
	output := make([]*Puppet, len(dbPuppets))
	for index, dbPuppet := range dbPuppets {
		if dbPuppet == nil {
			continue
		}
		puppet, ok := bridge.puppets[dbPuppet.ID]
		if !ok {
			puppet = bridge.NewPuppet(dbPuppet)
			bridge.puppets[dbPuppet.ID] = puppet
		}
		output[index] = puppet
	}
	return output
}

func (bridge *Bridge) FormatPuppetMXID(guid string) id.UserID {
	return id.NewUserID(
		bridge.Config.Bridge.FormatUsername(guid),
		bridge.Config.Homeserver.Domain)
}

func (bridge *Bridge) NewPuppet(dbPuppet *database.Puppet) *Puppet {
	mxid := bridge.FormatPuppetMXID(dbPuppet.ID)
	return &Puppet{
		Puppet: dbPuppet,
		bridge: bridge,
		log:    bridge.Log.Sub(fmt.Sprintf("Puppet/%s", dbPuppet.ID)),

		MXID:   mxid,
		Intent: bridge.AS.Intent(mxid),
	}
}

type Puppet struct {
	*database.Puppet

	bridge *Bridge
	log    log.Logger

	typingIn id.RoomID
	typingAt int64

	MXID   id.UserID
	Intent *appservice.IntentAPI
}

func (puppet *Puppet) UpdateName(contact *imessage.Contact) bool {
	var name string
	if contact != nil {
		name = contact.Name()
	} else {
		if puppet.Displayname != "" {
			// Don't update displayname if there's no contact list name available
			return false
		}
		// TODO format if phone number
		name = puppet.ID
	}
	newName := puppet.bridge.Config.Bridge.FormatDisplayname(name)
	if puppet.Displayname != newName {
		err := puppet.Intent.SetDisplayName(newName)
		if err == nil {
			puppet.Displayname = newName
			go puppet.updatePortalName()
			return true
		} else {
			puppet.log.Warnln("Failed to set display name:", err)
		}
	}
	return false
}

func (puppet *Puppet) UpdateAvatar(contact *imessage.Contact) bool {
	if contact == nil || contact.Avatar == nil {
		return false
	}
	avatarHash := sha256.Sum256(contact.Avatar)
	if puppet.AvatarHash == nil || *puppet.AvatarHash != avatarHash {
		puppet.AvatarHash = &avatarHash
		mimeTypeData := mimetype.Detect(contact.Avatar)
		resp, err := puppet.Intent.UploadBytesWithName(contact.Avatar, mimeTypeData.String(), "image" + mimeTypeData.Extension())
		if err != nil {
			puppet.AvatarHash = nil
			puppet.log.Warnln("Failed to upload avatar:", err)
			return false
		}
		puppet.AvatarURL = resp.ContentURI
		err = puppet.Intent.SetAvatarURL(puppet.AvatarURL)
		if err != nil {
			puppet.AvatarHash = nil
			puppet.log.Warnln("Failed to set avatar:", err)
			return false
		}
		go puppet.updatePortalAvatar()
		return true
	}
	return false
}

func (puppet *Puppet) updatePortalMeta(meta func(portal *Portal)) {
	imID := imessage.Identifier{Service: "iMessage", LocalID: puppet.ID}.String()
	meta(puppet.bridge.GetPortalByGUID(imID))
	if strings.HasPrefix(puppet.ID, "+") {
		smsID := imessage.Identifier{Service: "SMS", LocalID: puppet.ID}.String()
		meta(puppet.bridge.GetPortalByGUID(smsID))
	}
}

func (puppet *Puppet) updatePortalAvatar() {
	puppet.updatePortalMeta(func(portal *Portal) {
		if len(portal.MXID) > 0 {
			_, err := portal.MainIntent().SetRoomAvatar(portal.MXID, puppet.AvatarURL)
			if err != nil {
				portal.log.Warnln("Failed to set avatar:", err)
			}
		}
		portal.AvatarURL = puppet.AvatarURL
		portal.AvatarHash = puppet.AvatarHash
		portal.Update()
	})
}

func (puppet *Puppet) updatePortalName() {
	puppet.updatePortalMeta(func(portal *Portal) {
		if len(portal.MXID) > 0 {
			_, err := portal.MainIntent().SetRoomName(portal.MXID, puppet.Displayname)
			if err != nil {
				portal.log.Warnln("Failed to set name:", err)
			}
		}
		portal.Name = puppet.Displayname
		portal.Update()
	})
}

func (puppet *Puppet) Sync() {
	err := puppet.Intent.EnsureRegistered()
	if err != nil {
		puppet.log.Errorln("Failed to ensure registered:", err)
	}

	contact, err := puppet.bridge.IM.GetContactInfo(puppet.ID)
	if err != nil {
		puppet.log.Errorln("Failed to get contact info:", err)
	} else if contact == nil {
		puppet.log.Debugln("No contact info found")
	}

	puppet.SyncWithContact(contact)
}

func (puppet *Puppet) SyncWithContact(contact *imessage.Contact) {
	update := false
	update = puppet.UpdateName(contact) || update
	update = puppet.UpdateAvatar(contact) || update
	if update {
		puppet.Update()
	}
}
