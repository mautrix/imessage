// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/id"

	"github.com/lrhodin/imessage/pkg/rustpushgo"
)

func isRunningOnMacOS() bool {
	return runtime.GOOS == "darwin"
}

type IMConnector struct {
	Bridge *bridgev2.Bridge
	Config IMConfig
}

var _ bridgev2.NetworkConnector = (*IMConnector)(nil)

func (c *IMConnector) GetName() bridgev2.BridgeName {
	return bridgev2.BridgeName{
		DisplayName:          "iMessage",
		NetworkURL:           "https://support.apple.com/messages",
		NetworkIcon:          "mxc://maunium.net/tManJEpANASZvDVzvRvhILdl",
		NetworkID:            "imessage",
		BeeperBridgeType:     "imessagego",
		DefaultPort:          29332,
		DefaultCommandPrefix: "!im",
	}
}

func (c *IMConnector) Init(bridge *bridgev2.Bridge) {
	c.Bridge = bridge
}

func (c *IMConnector) Start(ctx context.Context) error {
	// Override backfill defaults for iMessage CloudKit sync.
	// Applied in Start() because Init() runs before config YAML is loaded.
	// Only apply when CloudKit backfill is enabled — otherwise leave the
	// mautrix defaults alone (backfill won't be used).
	if c.Config.CloudKitBackfill {
		// The mautrix defaults (max_initial_messages=50, batch_size=100) are too
		// low — CloudKit chats can have tens of thousands of messages, and many
		// small backward batch_send requests create fragmented DAG branches that
		// clients can't paginate through. High max_initial_messages ensures all
		// messages are delivered in one forward batch during room creation.
		cfg := &c.Bridge.Config.Backfill
		if !cfg.Enabled {
			cfg.Enabled = true
		}
		if cfg.MaxInitialMessages < 100 {
			cfg.MaxInitialMessages = math.MaxInt32 // uncapped — backfill everything CloudKit downloaded
		}
		// Catchup should match the initial cap — unlimited when uncapped,
		// capped when the user caps max_initial_messages.
		cfg.MaxCatchupMessages = cfg.MaxInitialMessages
		if !cfg.Queue.Enabled {
			cfg.Queue.Enabled = true
		}
		if cfg.Queue.BatchSize <= 100 {
			cfg.Queue.BatchSize = 10000
		}
		if cfg.MaxInitialMessages < math.MaxInt32 {
			// User explicitly capped initial messages — disable backward
			// backfill so the cap is the final word on message count.
			cfg.Queue.MaxBatches = 0
		} else if cfg.Queue.MaxBatches == 0 {
			cfg.Queue.MaxBatches = -1
		}
	}

	// Auto-restore: if the DB has no logins but we have valid backup session
	// state (session.json + keystore), create a user_login from the backup
	// instead of requiring a full re-login.
	c.tryAutoRestore(ctx)

	return nil
}

// tryAutoRestore checks if the database is empty but valid session state
// exists in the backup files.  If so, it creates a user_login entry from
// the backup, avoiding the need for a full Apple ID re-authentication.
func (c *IMConnector) tryAutoRestore(ctx context.Context) {
	log := c.Bridge.Log.With().Str("component", "imessage").Logger()

	// Only restore if there are no existing logins.
	usersWithLogins, err := c.Bridge.DB.UserLogin.GetAllUserIDsWithLogins(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to check existing logins for auto-restore")
		return
	}
	if len(usersWithLogins) > 0 {
		return // DB already has logins, nothing to restore
	}

	// Check for backup session state
	state := loadSessionState(log)
	if state.IDSUsers == "" || state.IDSIdentity == "" || state.APSState == "" {
		log.Debug().Msg("No complete backup session state found, skipping auto-restore")
		return
	}

	// Validate against keystore
	rustpushgo.InitLogger()
	session := &cachedSessionState{
		IDSIdentity: state.IDSIdentity,
		APSState:    state.APSState,
		IDSUsers:    state.IDSUsers,
		source:      "backup file (auto-restore)",
	}
	if !session.validate(log) {
		log.Info().Msg("Backup session state failed keystore validation, skipping auto-restore")
		return
	}
	// Chat.db mode doesn't join the keychain clique (no CloudKit), so
	// trustedpeers.plist is never written. Only require clique state
	// when CloudKit backfill is active.
	if c.Config.UseCloudKitBackfill() && !hasKeychainCliqueState(log) {
		log.Info().Msg("Skipping auto-restore: keychain trust circle not initialized (will require interactive login)")
		return
	}

	// Extract login ID and username from the cached IDS users
	users := rustpushgo.NewWrappedIdsUsers(&state.IDSUsers)
	loginID := networkid.UserLoginID(users.LoginId(0))
	if loginID == "" {
		log.Warn().Msg("Backup session has no login ID, skipping auto-restore")
		return
	}

	handles := users.GetHandles()
	username := string(loginID)
	if len(handles) > 0 {
		username = handles[0]
	}

	// Find the admin user to attach this login to
	adminMXID := ""
	for userID, perm := range c.Bridge.Config.Permissions {
		if perm.Admin {
			adminMXID = userID
			break
		}
	}
	if adminMXID == "" {
		log.Warn().Msg("No admin user in config, skipping auto-restore")
		return
	}

	user, err := c.Bridge.GetUserByMXID(ctx, id.UserID(adminMXID))
	if err != nil {
		log.Warn().Err(err).Msg("Failed to get admin user for auto-restore")
		return
	}

	log.Info().
		Str("login_id", string(loginID)).
		Str("username", username).
		Msg("Auto-restoring login from backup session state")

	platform := state.Platform
	if platform == "" {
		platform = runtime.GOOS
	}

	meta := &UserLoginMetadata{
		Platform:                 platform,
		HardwareKey:              state.HardwareKey,
		DeviceID:                 state.DeviceID,
		ChatsSynced:              false,
		APSState:                 state.APSState,
		IDSUsers:                 state.IDSUsers,
		IDSIdentity:              state.IDSIdentity,
		AccountUsername:          state.AccountUsername,
		AccountHashedPasswordHex: state.AccountHashedPasswordHex,
		AccountPET:               state.AccountPET,
		AccountADSID:             state.AccountADSID,
		AccountDSID:              state.AccountDSID,
		AccountSPDBase64:         state.AccountSPDBase64,
		MmeDelegateJSON:          state.MmeDelegateJSON,
	}

	_, err = user.NewLogin(ctx, &database.UserLogin{
		ID:         loginID,
		RemoteName: username,
		RemoteProfile: status.RemoteProfile{
			Name: username,
		},
		Metadata: meta,
	}, &bridgev2.NewLoginParams{
		DeleteOnConflict: true,
	})
	if err != nil {
		log.Err(err).Msg("Failed to auto-restore login from backup")
		return
	}

	log.Info().Str("login_id", string(loginID)).Msg("Successfully auto-restored login from backup session state")
}

func (c *IMConnector) GetLoginFlows() []bridgev2.LoginFlow {
	flows := []bridgev2.LoginFlow{}
	if isRunningOnMacOS() {
		flows = append(flows, bridgev2.LoginFlow{
			Name:        "Apple ID",
			Description: "Log in with your Apple ID to send and receive iMessages",
			ID:          LoginFlowIDAppleID,
		})
	}
	flows = append(flows, bridgev2.LoginFlow{
		Name:        "Apple ID (External Key)",
		Description: "Log in using a hardware key extracted from a Mac. Works on any platform.",
		ID:          LoginFlowIDExternalKey,
	})
	return flows
}

func (c *IMConnector) CreateLogin(ctx context.Context, user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	switch flowID {
	case LoginFlowIDAppleID:
		if !isRunningOnMacOS() {
			return nil, fmt.Errorf("Apple ID login requires macOS. Use 'External Key' login on other platforms.")
		}
		return &AppleIDLogin{User: user, Main: c}, nil
	case LoginFlowIDExternalKey:
		return &ExternalKeyLogin{User: user, Main: c}, nil
	default:
		return nil, fmt.Errorf("unknown login flow: %s", flowID)
	}
}

func (c *IMConnector) LoadUserLogin(ctx context.Context, login *bridgev2.UserLogin) error {
	meta := login.Metadata.(*UserLoginMetadata)
	log := c.Bridge.Log.With().Str("component", "imessage").Logger()

	rustpushgo.InitLogger()

	var cfg *rustpushgo.WrappedOsConfig
	var err error

	if meta.HardwareKey != "" {
		// Cross-platform mode: use hardware key with open-absinthe NAC emulation.
		if meta.DeviceID != "" {
			cfg, err = rustpushgo.CreateConfigFromHardwareKeyWithDeviceId(meta.HardwareKey, meta.DeviceID)
		} else {
			cfg, err = rustpushgo.CreateConfigFromHardwareKey(meta.HardwareKey)
		}
	} else if isRunningOnMacOS() {
		// Local macOS mode: use IOKit + AAAbsintheContext.
		if meta.DeviceID != "" {
			cfg, err = rustpushgo.CreateLocalMacosConfigWithDeviceId(meta.DeviceID)
		} else {
			cfg, err = rustpushgo.CreateLocalMacosConfig()
		}
	} else {
		return fmt.Errorf("no hardware key configured and not running on macOS — re-login with 'External Key' flow")
	}
	if err != nil {
		return fmt.Errorf("failed to create config: %w", err)
	}

	usersStr := &meta.IDSUsers
	identityStr := &meta.IDSIdentity
	apsStateStr := &meta.APSState

	// Eagerly persist full session state to the backup file so it survives DB resets.
	//
	// Guard against overwriting a good backup with empty state. client.Connect
	// (client.go ValidateKeystore path) wipes meta.IDSUsers/IDSIdentity/APSState
	// from the DB when the keystore is missing and flips to StateBadCredentials;
	// on the NEXT LoadUserLogin the meta is empty here, and without this guard
	// we'd blow away session.json — escalating a recoverable key-loss into a
	// full re-auth because tryAutoRestore on a future boot now finds no backup.
	if meta.IDSUsers == "" && meta.IDSIdentity == "" && meta.APSState == "" {
		log.Warn().Msg("LoadUserLogin: meta has no IDSUsers/IDSIdentity/APSState; skipping session.json overwrite to preserve existing backup")
	} else {
		saveSessionState(log, PersistedSessionState{
			IDSIdentity:              meta.IDSIdentity,
			APSState:                 meta.APSState,
			IDSUsers:                 meta.IDSUsers,
			PreferredHandle:          meta.PreferredHandle,
			Platform:                 meta.Platform,
			HardwareKey:              meta.HardwareKey,
			DeviceID:                 meta.DeviceID,
			AccountUsername:          meta.AccountUsername,
			AccountHashedPasswordHex: meta.AccountHashedPasswordHex,
			AccountPET:               meta.AccountPET,
			AccountADSID:             meta.AccountADSID,
			AccountDSID:              meta.AccountDSID,
			AccountSPDBase64:         meta.AccountSPDBase64,
			MmeDelegateJSON:          meta.MmeDelegateJSON,
		})
	}

	client := &IMClient{
		Main:                    c,
		UserLogin:               login,
		config:                  cfg,
		users:                   rustpushgo.NewWrappedIdsUsers(usersStr),
		identity:                rustpushgo.NewWrappedIdsngmIdentity(identityStr),
		connection:              rustpushgo.Connect(cfg, rustpushgo.NewWrappedApsState(apsStateStr)),
		contactsReady:           false,
		contactsReadyCh:         make(chan struct{}),
		cloudStore:              newCloudBackfillStore(c.Bridge.DB.Database, login.ID),
		sharedProfileStore:      newSharedProfileStore(c.Bridge.DB.Database, login.ID),
		pendingAttachments:      newPendingAttachmentStore(c.Bridge.DB.Database, login.ID),
		fordCache:               NewFordKeyCache(),
		recentUnsends:           make(map[string]time.Time),
		recentOutboundUnsends:   make(map[string]time.Time),
		recentSmsReactionEchoes: make(map[string]time.Time),
		smsPortals:              make(map[string]bool),
		sharedStreamAssetCache:  make(map[string]map[string]struct{}),
		sharedAlbumRooms:        make(map[string]id.RoomID),
		imGroupNames:            make(map[string]string),
		imGroupGuids:            make(map[string]string),
		imGroupParticipants:     make(map[string][]string),
		gidAliases:              make(map[string]string),
		lastGroupForMember:      make(map[string]networkid.PortalKey),
		restorePipelines:        make(map[string]bool),
		forwardBackfillSem:      make(chan struct{}, 3),
	}

	login.Client = client
	return nil
}
