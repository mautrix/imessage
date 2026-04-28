// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Tulir Asokan, Ludvig Rhodin
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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/beeper/bridge-manager/api/beeperapi"

	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"
	"maunium.net/go/mautrix/id"

	"github.com/lrhodin/imessage/pkg/connector"
)

var (
	Tag       = "unknown"
	Commit    = "unknown"
	BuildTime = "unknown"
)

var m = mxmain.BridgeMain{
	Name:        "mautrix-imessage",
	URL:         "https://github.com/lrhodin/imessage",
	Description: "A Matrix-iMessage puppeting bridge (bridgev2).",
	Version:     "0.1.0",

	Connector: &connector.IMConnector{},
}

func init() {
	m.PostInit = func() {
		proc := m.Bridge.Commands.(*commands.Processor)
		conn, _ := m.Connector.(*connector.IMConnector)
		disableFT := conn != nil && conn.Config.DisableFaceTime
		for _, h := range connector.BridgeCommands(disableFT) {
			proc.AddHandler(h)
		}
	}
}

func main() {
	m.InitVersion(Tag, Commit, BuildTime)

	// Handle subcommands / flags before normal bridge startup.
	if len(os.Args) > 1 && os.Args[0] != "-" {
		switch os.Args[1] {
		case "login":
			// Remove "login" from args so flag parsing in PreInit works.
			os.Args = append(os.Args[:1], os.Args[2:]...)
			runInteractiveLogin(&m)
			return
		case "check-restore":
			// Validate that backup session state can be restored without
			// re-authentication. Exits 0 if valid, 1 if not.
			if connector.CheckSessionRestore() {
				fmt.Fprintln(os.Stderr, "[+] Backup session state is valid — login can be auto-restored")
				os.Exit(0)
			} else {
				fmt.Fprintln(os.Stderr, "[-] No valid backup session state — login required")
				os.Exit(1)
			}
		case "list-handles":
			// Print available iMessage handles (phone/email) from session state.
			handles := connector.ListHandles()
			if len(handles) == 0 {
				os.Exit(1)
			}
			for _, h := range handles {
				fmt.Println(h)
			}
			return
		case "carddav-setup":
			// Discover CardDAV URL + encrypt password for install scripts.
			runCardDAVSetup()
			return
		case "init-db":
			// Initialize the database schema and exit without starting the
			// bridge. Used by install scripts to create the DB before asking
			// setup questions, without connecting to Matrix or APNs.
			os.Args = append(os.Args[:1], os.Args[2:]...)
			m.PreInit()
			repairPermissions(&m)
			m.Init()
			fmt.Fprintln(os.Stderr, "Database initialized successfully")
			os.Exit(0)
		}
	}

	// --setup flag: check permissions (FDA + Contacts) via native dialogs.
	if isSetupMode() {
		// Remove --setup from args so it doesn't confuse the bridge.
		var filtered []string
		for _, a := range os.Args {
			if a != "--setup" && a != "-setup" {
				filtered = append(filtered, a)
			}
		}
		os.Args = filtered
		runSetupPermissions()
		return
	}

	// Instead of m.Run(), manually call PreInit/Init/Start so we can
	// repair broken permissions before validateConfig() runs in Init().
	m.PreInit()
	repairPermissions(&m)
	m.Init()
	m.Start()
	exitCode := m.WaitForInterrupt()
	m.Stop()
	os.Exit(exitCode)
}

// repairPermissions detects and fixes broken bridge.permissions before the
// bridge's validateConfig() rejects the config. This handles cases where
// bbctl generated a config with an empty or invalid username, leaving
// permissions with only example.com defaults.
func repairPermissions(br *mxmain.BridgeMain) {
	if br.Config == nil {
		return
	}
	configured := br.Config.Bridge.Permissions.IsConfigured()
	fmt.Fprintf(os.Stderr, "[permissions] IsConfigured=%v entries=%d\n", configured, len(br.Config.Bridge.Permissions))
	for key := range br.Config.Bridge.Permissions {
		fmt.Fprintf(os.Stderr, "[permissions]   %q\n", key)
	}
	if configured {
		return
	}

	// Permissions are not configured — try to derive the correct MXID
	// from bbctl's saved credentials.
	username := loadBBCtlUsername()
	if username == "" {
		fmt.Fprintf(os.Stderr, "[permissions] loadBBCtlUsername returned empty — cannot repair\n")
		return
	}

	mxid := id.NewUserID(username, "beeper.com")

	// Remove bogus entries (example.com defaults, empty username variants,
	// wildcard relay) from the in-memory map so findAdminUser() doesn't
	// pick them over the real MXID. Patterns match fixPermissionsOnDisk()
	// and the shell fix_permissions() function.
	for key := range br.Config.Bridge.Permissions {
		if strings.Contains(key, "example.com") || key == "*" ||
			key == "@:" || key == "@" || strings.HasPrefix(key, "@:") {
			delete(br.Config.Bridge.Permissions, key)
		}
	}

	br.Config.Bridge.Permissions[string(mxid)] = &bridgeconfig.PermissionLevelAdmin

	// Also persist the fix to config.yaml so this is a one-time repair.
	if br.ConfigPath != "" {
		fixPermissionsOnDisk(br.ConfigPath, string(mxid))
	}

	fmt.Fprintf(os.Stderr, "Auto-repaired bridge.permissions: %s → admin\n", mxid)
}

// loadBBCtlUsername reads the username from bbctl's config.json.
func loadBBCtlUsername() string {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	path := filepath.Join(configDir, "bbctl", "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var cfg struct {
		Environments map[string]struct {
			Username    string `json:"username"`
			AccessToken string `json:"access_token"`
		} `json:"environments"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return ""
	}
	if prod, ok := cfg.Environments["prod"]; ok {
		if prod.Username != "" {
			return prod.Username
		}
		// Username empty but have credentials — try whoami as last resort
		if strings.HasPrefix(prod.AccessToken, "syt_") {
			resp, err := beeperapi.Whoami("beeper.com", prod.AccessToken)
			if err == nil && resp.UserInfo.Username != "" {
				return resp.UserInfo.Username
			}
		}
	}
	return ""
}

// fixPermissionsOnDisk patches the config.yaml file to set the correct admin
// MXID and remove all bogus permission entries (example.com defaults, empty
// username variants). Matches the same patterns as the in-memory cleanup in
// repairPermissions() and the shell repair function in the install scripts.
func fixPermissionsOnDisk(configPath string, mxid string) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return
	}

	// isBogusPermLine returns true for any permissions entry that should be
	// removed: example.com defaults, empty-username variants, wildcard relay.
	isBogusPermLine := func(trimmed string) bool {
		// Empty-username patterns: "@:beeper.com", "@": ...
		if strings.Contains(trimmed, `"@":`) || strings.Contains(trimmed, `"@:`) {
			return true
		}
		// Example.com defaults: "@admin:example.com", "example.com"
		if strings.Contains(trimmed, "example.com") {
			return true
		}
		// Wildcard relay entry from example config
		if strings.HasPrefix(trimmed, `"*":`) {
			return true
		}
		return false
	}

	lines := strings.Split(string(data), "\n")
	inPerms := false
	replaced := false
	var out []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Track whether we're inside the permissions block.
		if strings.HasPrefix(trimmed, "permissions:") {
			inPerms = true
			out = append(out, line)
			continue
		}
		// A non-indented, non-empty line exits the permissions block.
		if inPerms && trimmed != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			inPerms = false
		}

		if inPerms && isBogusPermLine(trimmed) {
			if !replaced && strings.Contains(trimmed, ": admin") {
				// Replace the first admin line with the correct MXID.
				indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
				out = append(out, indent+`"`+mxid+`": admin`)
				replaced = true
			}
			// Drop all other bogus lines (example.com user, wildcard relay, etc.)
			continue
		}
		out = append(out, line)
	}
	_ = os.WriteFile(configPath, []byte(strings.Join(out, "\n")), 0600)
}
