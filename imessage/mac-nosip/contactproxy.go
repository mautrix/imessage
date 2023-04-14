// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2023 Tulir Asokan
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

//go:build darwin && !ios

package mac_nosip

import (
	"fmt"

	log "maunium.net/go/maulogger/v2"

	"go.mau.fi/mautrix-imessage/imessage"
	"go.mau.fi/mautrix-imessage/imessage/mac"
)

func setupContactProxy(log log.Logger) (imessage.ContactAPI, error) {
	store := mac.NewContactStore()
	err := store.RequestContactAccess()
	if err != nil {
		return nil, fmt.Errorf("failed to request contact access: %w", err)
	} else if store.HasContactAccess {
		log.Infoln("Contact access is allowed")
	} else {
		log.Warnln("Contact access is not allowed")
	}
	return store, nil
}

var setupChatInfoProxy = mac.NewChatInfoDatabase
