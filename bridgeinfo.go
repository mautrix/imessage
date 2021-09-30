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
	"reflect"

	"maunium.net/go/mautrix/event"
)

const bridgeInfoProto = "fi.mau.imessage"

type CustomBridgeInfoSection struct {
	event.BridgeInfoSection

	GUID    string `json:"fi.mau.imessage.guid,omitempty"`
	Service string `json:"fi.mau.imessage.service,omitempty"`
	IsGroup bool   `json:"fi.mau.imessage.is_group,omitempty"`
}

type CustomBridgeInfoContent struct {
	event.BridgeEventContent
	Channel CustomBridgeInfoSection `json:"channel"`
}

func init() {
	event.TypeMap[event.StateBridge] = reflect.TypeOf(CustomBridgeInfoContent{})
	event.TypeMap[event.StateHalfShotBridge] = reflect.TypeOf(CustomBridgeInfoContent{})
}
