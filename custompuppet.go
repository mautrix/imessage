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
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/bridge"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

var (
	ErrMismatchingMXID = errors.New("whoami result does not match custom mxid")
)

var _ bridge.DoublePuppet = (*User)(nil)

func (user *User) SwitchCustomMXID(accessToken string, mxid id.UserID) error {
	if mxid != user.MXID {
		return errors.New("mismatching mxid")
	}
	user.AccessToken = accessToken
	return user.startCustomMXID()
}

func (user *User) CustomIntent() *appservice.IntentAPI {
	return user.DoublePuppetIntent
}

func (user *User) initDoublePuppet() {
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
	url := user.bridge.Config.Bridge.DoublePuppetServerURL
	if url == "" {
		url = user.bridge.AS.HomeserverURL
	}
	client, err := mautrix.NewClient(url, "", "")
	if err != nil {
		return err
	}
	client.Logger = user.bridge.AS.Log.Sub(string(user.MXID))
	client.Client = user.bridge.AS.HTTPClient
	client.DefaultHTTPRetries = user.bridge.AS.DefaultHTTPRetries
	resp, err := client.Login(&mautrix.ReqLogin{
		Type:                     mautrix.AuthTypePassword,
		Identifier:               mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: string(user.MXID)},
		Password:                 hex.EncodeToString(mac.Sum(nil)),
		DeviceID:                 id.DeviceID(user.bridge.Config.IMessage.BridgeName()),
		InitialDeviceDisplayName: user.bridge.Config.IMessage.BridgeName(),
	})
	if err != nil {
		return fmt.Errorf("failed to log in with shared secret: %w", err)
	}
	user.AccessToken = resp.AccessToken
	return nil
}

func (user *User) newDoublePuppetIntent() (*appservice.IntentAPI, error) {
	url := user.bridge.Config.Bridge.DoublePuppetServerURL
	if url == "" {
		url = user.bridge.AS.HomeserverURL
	}
	client, err := mautrix.NewClient(url, user.MXID, user.AccessToken)
	if err != nil {
		return nil, err
	}
	client.Logger = user.bridge.AS.Log.Sub(string(user.MXID))
	client.Client = user.bridge.AS.HTTPClient
	client.DefaultHTTPRetries = user.bridge.AS.DefaultHTTPRetries
	client.Syncer = user

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
	user.startSyncing()
	return nil
}

func (user *User) startSyncing() {
	if !user.bridge.Config.Bridge.SyncWithCustomPuppets {
		return
	}
	if !user.bridge.IM.Capabilities().SendTypingNotifications && !user.bridge.IM.Capabilities().SendReadReceipts {
		user.log.Warnln("Syncing with double puppet is enabled in config, but configured platform doesn't support sending typing notifications nor read receipts")
	}
	go func() {
		err := user.DoublePuppetIntent.Sync()
		if err != nil {
			user.log.Errorln("Fatal error syncing:", err)
		}
	}()
}

func (user *User) stopSyncing() {
	if !user.bridge.Config.Bridge.SyncWithCustomPuppets {
		return
	}
	user.DoublePuppetIntent.StopSync()
}

func (user *User) tryRelogin(cause error, action string) bool {
	user.log.Debugfln("Trying to relogin after '%v' while %s", cause, action)
	err := user.loginWithSharedSecret()
	if err != nil {
		user.log.Errorfln("Failed to relogin after '%v' while %s: %v", cause, action, err)
		return false
	}
	user.log.Infofln("Successfully relogined after '%v' while %s", cause, action)
	return true
}

func (user *User) handleReceiptEvent(portal *Portal, event *event.Event) {
	for eventID, receipts := range *event.Content.AsReceipt() {
		if receipt, ok := receipts.Read[user.MXID]; !ok {
			// Ignore receipt events where this user isn't present.
		} else if val, ok := receipt.Extra[appservice.DoublePuppetKey].(string); ok && user.DoublePuppetIntent != nil && val == portal.bridge.Name {
			// Ignore double puppeted read receipts.
		} else {
			portal.HandleMatrixReadReceipt(user, eventID, time.UnixMilli(receipt.Timestamp))
		}
	}
}

func (user *User) ProcessResponse(resp *mautrix.RespSync, _ string) error {
	for roomID, events := range resp.Rooms.Join {
		portal := user.bridge.GetPortalByMXID(roomID)
		if portal == nil {
			continue
		}
		for _, evt := range events.Ephemeral.Events {
			err := evt.Content.ParseRaw(evt.Type)
			if err != nil {
				return err
			}
			switch evt.Type {
			case event.EphemeralEventReceipt:
				go user.handleReceiptEvent(portal, evt)
			case event.EphemeralEventTyping:
				if portal.IsPrivateChat() {
					go portal.HandleMatrixTyping(evt.Content.AsTyping().UserIDs)
				}
			}
		}
	}

	return nil
}

func (user *User) OnFailedSync(_ *mautrix.RespSync, err error) (time.Duration, error) {
	user.log.Warnln("Failed to sync:", err)
	if errors.Is(err, mautrix.MUnknownToken) {
		if !user.tryRelogin(err, "syncing") {
			return 0, err
		}
		return 0, nil
	}
	return 10 * time.Second, nil
}

func (user *User) GetFilterJSON(_ id.UserID) *mautrix.Filter {
	everything := []event.Type{{Type: "*"}}
	return &mautrix.Filter{
		Presence: mautrix.FilterPart{
			Senders: []id.UserID{user.MXID},
			Types:   []event.Type{event.EphemeralEventPresence},
		},
		AccountData: mautrix.FilterPart{NotTypes: everything},
		Room: mautrix.RoomFilter{
			Ephemeral:    mautrix.FilterPart{Types: []event.Type{event.EphemeralEventTyping, event.EphemeralEventReceipt}},
			IncludeLeave: false,
			AccountData:  mautrix.FilterPart{NotTypes: everything},
			State:        mautrix.FilterPart{NotTypes: everything},
			Timeline:     mautrix.FilterPart{NotTypes: everything},
		},
	}
}
