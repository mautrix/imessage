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
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gabriel-vasile/mimetype"
	log "maunium.net/go/maulogger/v2"

	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/bridge"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-imessage/database"
	"go.mau.fi/mautrix-imessage/imessage"
)

var userIDRegex *regexp.Regexp

func (br *IMBridge) ParsePuppetMXID(mxid id.UserID) (string, bool) {
	if userIDRegex == nil {
		userIDRegex = regexp.MustCompile(fmt.Sprintf("^@%s:%s$",
			br.Config.Bridge.FormatUsername("(.+)"),
			br.Config.Homeserver.Domain))
	}
	match := userIDRegex.FindStringSubmatch(string(mxid))
	if match == nil || len(match) != 2 {
		return "", false
	}

	localID := match[1]

	if strings.Contains(localID, "=40") {
		localpart, err := id.DecodeUserLocalpart(localID)
		if err != nil {
			br.Log.Debugfln("Failed to decode user localpart '%s': %v", localID, err)
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

func (br *IMBridge) GetPuppetByMXID(mxid id.UserID) *Puppet {
	localID, ok := br.ParsePuppetMXID(mxid)
	if !ok {
		return nil
	}

	return br.GetPuppetByLocalID(localID)
}

func (br *IMBridge) GetPuppetByGUID(guid string) *Puppet {
	return br.GetPuppetByLocalID(imessage.ParseIdentifier(guid).LocalID)
}

func (br *IMBridge) GetPuppetByLocalID(id string) *Puppet {
	br.puppetsLock.Lock()
	defer br.puppetsLock.Unlock()
	puppet, ok := br.puppets[id]
	if !ok {
		dbPuppet := br.DB.Puppet.Get(id)
		if dbPuppet == nil {
			dbPuppet = br.DB.Puppet.New()
			dbPuppet.ID = id
			dbPuppet.Insert()
		}
		puppet = br.NewPuppet(dbPuppet)
		br.puppets[puppet.ID] = puppet
	}
	return puppet
}

func (br *IMBridge) GetAllPuppets() []*Puppet {
	return br.dbPuppetsToPuppets(br.DB.Puppet.GetAll())
}

func (br *IMBridge) dbPuppetsToPuppets(dbPuppets []*database.Puppet) []*Puppet {
	br.puppetsLock.Lock()
	defer br.puppetsLock.Unlock()
	output := make([]*Puppet, len(dbPuppets))
	for index, dbPuppet := range dbPuppets {
		if dbPuppet == nil {
			continue
		}
		puppet, ok := br.puppets[dbPuppet.ID]
		if !ok {
			puppet = br.NewPuppet(dbPuppet)
			br.puppets[dbPuppet.ID] = puppet
		}
		output[index] = puppet
	}
	return output
}

func (br *IMBridge) FormatPuppetMXID(guid string) id.UserID {
	return id.NewUserID(
		br.Config.Bridge.FormatUsername(guid),
		br.Config.Homeserver.Domain)
}

func (br *IMBridge) NewPuppet(dbPuppet *database.Puppet) *Puppet {
	mxid := br.FormatPuppetMXID(dbPuppet.ID)
	return &Puppet{
		Puppet: dbPuppet,
		bridge: br,
		log:    br.Log.Sub(fmt.Sprintf("Puppet/%s", dbPuppet.ID)),

		MXID:   mxid,
		Intent: br.AS.Intent(mxid),
	}
}

type Puppet struct {
	*database.Puppet

	bridge *IMBridge
	log    log.Logger

	typingIn id.RoomID
	typingAt int64

	MXID   id.UserID
	Intent *appservice.IntentAPI
}

var _ bridge.Ghost = (*Puppet)(nil)
var _ bridge.GhostWithProfile = (*Puppet)(nil)

func (puppet *Puppet) GetDisplayname() string {
	return puppet.Displayname
}

func (puppet *Puppet) GetAvatarURL() id.ContentURI {
	return puppet.AvatarURL
}

func (puppet *Puppet) CustomIntent() *appservice.IntentAPI {
	return nil
}

func (puppet *Puppet) SwitchCustomMXID(accessToken string, userID id.UserID) error {
	panic("Puppet.SwitchCustomMXID is not implemented")
}

func (puppet *Puppet) DefaultIntent() *appservice.IntentAPI {
	return puppet.Intent
}

func (puppet *Puppet) GetMXID() id.UserID {
	return puppet.MXID
}

func (puppet *Puppet) UpdateName(contact *imessage.Contact) bool {
	if puppet.NameOverridden {
		// Never replace custom names with contact list names
		return false
	} else if puppet.Displayname != "" && (contact == nil || !contact.HasName()) {
		// Don't update displayname if there's no contact list name available
		return false
	}
	if contact != nil {
		return puppet.UpdateNameDirect(contact.Name())
	} else {
		// TODO format if phone numbers
		return puppet.UpdateNameDirect(puppet.ID)
	}
}

func (puppet *Puppet) UpdateNameDirect(name string) bool {
	if len(name) == 0 {
		return false
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
	if contact == nil {
		return false
	}
	return puppet.UpdateAvatarFromBytes(contact.Avatar)
}

func (puppet *Puppet) UpdateAvatarFromBytes(avatar []byte) bool {
	if avatar == nil {
		return false
	}
	avatarHash := sha256.Sum256(avatar)
	if puppet.AvatarHash == nil || *puppet.AvatarHash != avatarHash {
		puppet.AvatarHash = &avatarHash
		mimeTypeData := mimetype.Detect(avatar)
		resp, err := puppet.Intent.UploadBytesWithName(avatar, mimeTypeData.String(), "avatar"+mimeTypeData.Extension())
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

func applyMeta(portal *Portal, meta func(portal *Portal)) {
	if portal == nil {
		return
	}
	portal.roomCreateLock.Lock()
	defer portal.roomCreateLock.Unlock()
	meta(portal)
}

func (puppet *Puppet) updatePortalMeta(meta func(portal *Portal)) {
	imID := imessage.Identifier{Service: "iMessage", LocalID: puppet.ID}.String()
	applyMeta(puppet.bridge.GetPortalByGUID(imID), meta)
	if strings.HasPrefix(puppet.ID, "+") {
		smsID := imessage.Identifier{Service: "SMS", LocalID: puppet.ID}.String()
		applyMeta(puppet.bridge.GetPortalByGUID(smsID), meta)
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
		portal.UpdateBridgeInfo()
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
		portal.UpdateBridgeInfo()
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

var avatarDownloadClient = http.Client{
	Timeout: 30 * time.Second,
}

func (puppet *Puppet) backgroundAvatarUpdate(url string) {
	puppet.log.Debugfln("Updating avatar from remote URL in background")
	var resp *http.Response
	var body []byte
	var err error
	defer func() {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()
	if resp, err = avatarDownloadClient.Get(url); err != nil {
		puppet.log.Warnfln("Failed to request override avatar from %s: %v", url, err)
	} else if body, err = io.ReadAll(resp.Body); err != nil {
		puppet.log.Warnfln("Failed to read override avatar from %s: %v", url, err)
	} else {
		puppet.UpdateAvatarFromBytes(body)
	}
}

func (puppet *Puppet) SyncWithProfileOverride(override ProfileOverride) {
	if len(override.Displayname) > 0 {
		puppet.UpdateNameDirect(override.Displayname)
	}
	if len(override.PhotoURL) > 0 {
		go puppet.backgroundAvatarUpdate(override.PhotoURL)
	}
}

func (puppet *Puppet) SyncWithContact(contact *imessage.Contact) {
	update := false
	update = puppet.UpdateName(contact) || update
	update = puppet.UpdateAvatar(contact) || update
	if update {
		puppet.Update()
	}
}
