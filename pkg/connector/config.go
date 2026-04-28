// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

import (
	_ "embed"
	"strings"
	"text/template"

	up "go.mau.fi/util/configupgrade"
	"gopkg.in/yaml.v3"
)

//go:embed example-config.yaml
var ExampleConfig string

type IMConfig struct {
	DisplaynameTemplate string `yaml:"displayname_template"`
	displaynameTemplate *template.Template

	// CloudKitBackfill enables message history backfill (master on/off switch).
	// When false, the bridge only handles real-time messages via APNs push
	// and skips the device PIN / iCloud Keychain steps during login.
	// Default is false.
	CloudKitBackfill bool `yaml:"cloudkit_backfill"`

	// BackfillSource selects the backfill engine when CloudKitBackfill is true.
	// "cloudkit" (default) syncs from iCloud; "chatdb" reads the local macOS
	// chat.db (requires Full Disk Access).
	BackfillSource string `yaml:"backfill_source"`

	// VideoTranscoding enables automatic remuxing/transcoding of non-MP4
	// videos (e.g. QuickTime .mov) to MP4 for broad Matrix client
	// compatibility.  Requires ffmpeg to be installed.  Default is false.
	VideoTranscoding bool `yaml:"video_transcoding"`

	// HEICConversion enables automatic conversion of HEIC/HEIF images
	// to JPEG for broad Matrix client compatibility.
	// Requires libheif to be installed.  Default is false.
	HEICConversion bool `yaml:"heic_conversion"`

	// HEICJPEGQuality sets the JPEG output quality (1–100) used when
	// converting HEIC/HEIF images. Default is 95.
	HEICJPEGQuality int `yaml:"heic_jpeg_quality"`

	// PreferredHandle overrides the outgoing iMessage identity.
	// Use the full URI format: "tel:+15551234567" or "mailto:user@example.com".
	// If empty, the handle chosen during login is used.
	PreferredHandle string `yaml:"preferred_handle"`

	// FaceTimeDisplayName overrides the display name pre-filled on the
	// FaceTime web join page (the value attached to `#n=…` in the ring-
	// notice link). If empty, the bridge reads the user's "First Last"
	// from the cached Apple Account SPD; if that's also unavailable it
	// falls back to the bare iMessage handle.
	FaceTimeDisplayName string `yaml:"facetime_display_name"`

	// DisableFaceTime turns off all bridge FaceTime integration: the
	// !facetime* slash commands aren't registered, inbound ring / missed /
	// answered-elsewhere notices aren't posted, and inbound peer-invite
	// notices are suppressed. Intended for users who own an Apple device
	// and answer FaceTime calls natively — the bridge wrapper adds nothing
	// in that case and just clutters the chat.
	DisableFaceTime bool `yaml:"disable_facetime"`

	// StatusKitShareOnStartup publishes share_status(available) once after
	// StatusKit init completes. Peer iOS reciprocates with a reshare (which
	// carries the key material needed to decrypt its subsequent presence
	// updates), so keeping this on dramatically improves the chance of
	// seeing contacts' Focus state in Matrix. Default true.
	StatusKitShareOnStartup bool `yaml:"statuskit_share_on_startup"`

	// CardDAV is an external CardDAV server for contact name resolution.
	// When configured, this is used instead of iCloud CardDAV contacts.
	CardDAV CardDAVConfig `yaml:"carddav"`
}

// CardDAVConfig configures an external CardDAV server for contact name resolution.
// Supports Google (with app passwords), Nextcloud, Radicale, Fastmail, etc.
type CardDAVConfig struct {
	// Email address used for CardDAV auto-discovery (RFC 6764 .well-known/carddav).
	// Also used as the username if Username is empty.
	Email string `yaml:"email"`

	// URL is the CardDAV server URL. Leave empty to auto-discover from Email.
	// Example: https://www.googleapis.com/carddav/v1/principals/you@gmail.com/lists/default/
	URL string `yaml:"url"`

	// Username for HTTP Basic authentication. Defaults to Email if empty.
	Username string `yaml:"username"`

	// PasswordEncrypted is the AES-256-GCM encrypted app password (base64).
	// Set by the install script via the carddav-setup subcommand.
	PasswordEncrypted string `yaml:"password_encrypted"`
}

// IsConfigured returns true if the CardDAV config has enough info to connect.
func (c *CardDAVConfig) IsConfigured() bool {
	return c.Email != "" && c.PasswordEncrypted != ""
}

// GetUsername returns the effective username (falls back to Email).
func (c *CardDAVConfig) GetUsername() string {
	if c.Username != "" {
		return c.Username
	}
	return c.Email
}

type umIMConfig IMConfig

func (c *IMConfig) UnmarshalYAML(node *yaml.Node) error {
	err := node.Decode((*umIMConfig)(c))
	if err != nil {
		return err
	}
	return c.PostProcess()
}

func (c *IMConfig) PostProcess() error {
	var err error
	c.displaynameTemplate, err = template.New("displayname").Parse(c.DisplaynameTemplate)
	return err
}

type DisplaynameParams struct {
	FirstName string
	LastName  string
	Nickname  string
	Phone     string
	Email     string
	ID        string
}

func (c *IMConfig) FormatDisplayname(params DisplaynameParams) string {
	var buf strings.Builder
	err := c.displaynameTemplate.Execute(&buf, &params)
	if err != nil {
		return params.ID
	}
	name := strings.TrimSpace(buf.String())
	if name == "" {
		return params.ID
	}
	return name
}

// UseChatDBBackfill returns true when backfill is enabled and sourced from chat.db.
func (c *IMConfig) UseChatDBBackfill() bool {
	return c.CloudKitBackfill && c.BackfillSource == "chatdb"
}

// UseCloudKitBackfill returns true when backfill is enabled and sourced from CloudKit.
func (c *IMConfig) UseCloudKitBackfill() bool {
	return c.CloudKitBackfill && c.BackfillSource != "chatdb"
}

func upgradeConfig(helper up.Helper) {
	helper.Copy(up.Str, "displayname_template")
	helper.Copy(up.Bool, "cloudkit_backfill")
	helper.Copy(up.Str, "backfill_source")
	helper.Copy(up.Bool, "video_transcoding")
	helper.Copy(up.Bool, "heic_conversion")
	helper.Copy(up.Int, "heic_jpeg_quality")
	helper.Copy(up.Str, "preferred_handle")
	helper.Copy(up.Str, "facetime_display_name")
	helper.Copy(up.Bool, "statuskit_share_on_startup")
	helper.Copy(up.Str, "carddav", "email")
	helper.Copy(up.Str, "carddav", "url")
	helper.Copy(up.Str, "carddav", "username")
	helper.Copy(up.Str, "carddav", "password_encrypted")
}

func (c *IMConnector) GetConfig() (string, any, up.Upgrader) {
	return ExampleConfig, &c.Config, up.SimpleUpgrader(upgradeConfig)
}
