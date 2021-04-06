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
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
)

var (
	ErrMismatchingMXID = errors.New("whoami result does not match custom mxid")
)

func (user *User) initDoublePuppet() {
	if _, homeserver, _ := user.MXID.Parse(); homeserver != user.bridge.Config.Homeserver.Domain {
		// user is on another homeserver
		return
	}
	var err error
	if len(user.AccessToken) > 0 {
		err = user.startCustomMXID()
		if errors.Is(err, mautrix.MUnknownToken) && len(user.bridge.Config.Bridge.LoginSharedSecret) > 0 {
			user.log.Debugln("Unknown token while starting custom puppet, trying to relogin with shared secret")
			err = user.loginWithSharedSecret()
			if err == nil {
				err = user.startCustomMXID()
			}
		}
	} else {
		err = user.loginWithSharedSecret()
		if err == nil {
			err = user.startCustomMXID()
		}
	}
	if err != nil {
		user.log.Warnln("Failed to switch to auto-logined custom puppet:", err)
	} else {
		user.log.Infoln("Successfully automatically enabled custom puppet")
	}
}

func (user *User) loginWithSharedSecret() error {
	user.log.Debugfln("Logging in with shared secret")
	mac := hmac.New(sha512.New, []byte(user.bridge.Config.Bridge.LoginSharedSecret))
	mac.Write([]byte(user.MXID))
	resp, err := user.bridge.AS.BotClient().Login(&mautrix.ReqLogin{
		Type:                     mautrix.AuthTypePassword,
		Identifier:               mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: string(user.MXID)},
		Password:                 hex.EncodeToString(mac.Sum(nil)),
		DeviceID:                 "iMessage Bridge",
		InitialDeviceDisplayName: "iMessage Bridge",
	})
	if err != nil {
		return fmt.Errorf("failed to log in with shared secret: %w", err)
	}
	user.AccessToken = resp.AccessToken
	return nil
}

func (user *User) newDoublePuppetIntent() (*appservice.IntentAPI, error) {
	client, err := mautrix.NewClient(user.bridge.AS.HomeserverURL, user.MXID, user.AccessToken)
	if err != nil {
		return nil, err
	}
	client.Logger = user.bridge.AS.Log.Sub(string(user.MXID))
	client.Client = user.bridge.AS.HTTPClient

	ia := user.bridge.AS.NewIntentAPI("custom")
	ia.Client = client
	ia.Localpart, _, _ = user.MXID.Parse()
	ia.UserID = user.MXID
	ia.IsCustomPuppet = true
	return ia, nil
}

func (user *User) clearCustomMXID() {
	user.AccessToken = ""
	user.NextBatch = ""
	user.DoublePuppetIntent = nil
}

func (user *User) startCustomMXID() error {
	if len(user.AccessToken) == 0 {
		user.clearCustomMXID()
		return nil
	}
	intent, err := user.newDoublePuppetIntent()
	if err != nil {
		user.clearCustomMXID()
		return fmt.Errorf("failed to create double puppet intent: %w", err)
	}
	resp, err := intent.Whoami()
	if err != nil {
		user.clearCustomMXID()
		return fmt.Errorf("failed to ensure double puppet token is valid: %w", err)
	}
	if resp.UserID != user.MXID {
		user.clearCustomMXID()
		return ErrMismatchingMXID
	}
	user.DoublePuppetIntent = intent
	return nil
}
