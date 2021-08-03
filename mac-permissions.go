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

//+build darwin,!ios

package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/mattn/go-sqlite3"

	"go.mau.fi/mautrix-imessage/imessage"
	"go.mau.fi/mautrix-imessage/imessage/mac"
	_ "go.mau.fi/mautrix-imessage/imessage/mac-nosip"
)

func checkMacPermissions() {
	err := mac.CheckPermissions()
	if err != nil {
		fmt.Println(err)
	}
	if errors.Is(err, imessage.ErrNotLoggedIn) {
		os.Exit(41)
	} else if sqliteError := (sqlite3.Error{}); errors.As(err, &sqliteError) {
		if errors.Is(sqliteError.SystemErrno, os.ErrNotExist) {
			os.Exit(42)
		} else if errors.Is(sqliteError.SystemErrno, os.ErrPermission) {
			os.Exit(43)
		}
	} else if err != nil {
		os.Exit(49)
	}
}
