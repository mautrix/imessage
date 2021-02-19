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

package mac

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

var majorVer, minorVer int = 11, 2

func init() {
	out, err := exec.Command("sw_vers", "-productVersion").Output()
	if err != nil {
		panic(fmt.Errorf("failed to get macOS version: %w", err))
	}
	parts := strings.Split(string(out), ".")
	majorVer, err = strconv.Atoi(parts[0])
	if err != nil {
		panic(fmt.Errorf("failed to parse macOS major version: %w", err))
	}
	minorVer, err = strconv.Atoi(parts[1])
	if err != nil {
		panic(fmt.Errorf("failed to parse macOS minor version: %w", err))
	}
}
