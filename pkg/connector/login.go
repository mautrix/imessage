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
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/id"

	"github.com/lrhodin/imessage/pkg/rustpushgo"
)

const (
	LoginFlowIDAppleID       = "apple-id"
	LoginFlowIDExternalKey   = "external-key"
	LoginStepAppleIDPassword = "fi.mau.imessage.login.appleid"
	LoginStepExternalKey     = "fi.mau.imessage.login.externalkey"
	LoginStepTwoFactor       = "fi.mau.imessage.login.2fa"
	LoginStepSelectDevice    = "fi.mau.imessage.login.select_device"
	LoginStepDevicePasscode  = "fi.mau.imessage.login.device_passcode"
	LoginStepSelectHandle    = "fi.mau.imessage.login.select_handle"
	LoginStepComplete        = "fi.mau.imessage.login.complete"
)

// AppleIDLogin implements the multi-step login flow:
// Apple ID + password → 2FA code → IDS registration → device selection → passcode → handle selection → connected.
type AppleIDLogin struct {
	User           *bridgev2.User
	Main           *IMConnector
	username       string
	cfg            *rustpushgo.WrappedOsConfig
	conn           *rustpushgo.WrappedApsConnection
	session        *rustpushgo.LoginSession
	result         *rustpushgo.IdsUsersWithIdentityRecord // set after IDS registration
	handle         string                                 // chosen handle
	devices        []rustpushgo.EscrowDeviceInfo          // escrow devices (fetched after IDS registration)
	selectedDevice int                                    // index into devices (-1 = not yet selected)
}

var _ bridgev2.LoginProcessUserInput = (*AppleIDLogin)(nil)

func (l *AppleIDLogin) Cancel() {}

func (l *AppleIDLogin) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	rustpushgo.InitLogger()

	cfg, err := rustpushgo.CreateLocalMacosConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize local NAC config: %w", err)
	}
	l.cfg = cfg

	// Reuse existing session state if available and keystore matches
	log := l.Main.Bridge.Log.With().Str("component", "imessage").Logger()
	session := loadCachedSession(l.User, log)
	if !session.validate(log) {
		session = nil
	}
	apsState := getExistingAPSState(session, log)
	l.conn = rustpushgo.Connect(cfg, apsState)

	return &bridgev2.LoginStep{
		Type:   bridgev2.LoginStepTypeUserInput,
		StepID: LoginStepAppleIDPassword,
		Instructions: "Enter your Apple ID credentials. " +
			"Registration uses local NAC (no relay needed).",
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{{
				Type: bridgev2.LoginInputFieldTypeEmail,
				ID:   "username",
				Name: "Apple ID",
			}, {
				Type: bridgev2.LoginInputFieldTypePassword,
				ID:   "password",
				Name: "Password",
			}},
		},
	}, nil
}

func (l *AppleIDLogin) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	// Device passcode step (after device selection, before handle selection)
	if passcode, ok := input["passcode"]; ok && l.result != nil {
		return l.handlePasscodeAndContinue(ctx, passcode)
	}

	// Device selection step (after IDS registration, before passcode)
	if device, ok := input["device"]; ok && l.result != nil {
		return l.handleDeviceSelection(device)
	}

	// Handle selection step (after device passcode)
	if l.result != nil {
		l.handle = parseHandleSelection(input["handle"], l.result.Users.GetHandles())
		return l.completeLogin(ctx)
	}

	// Step 1: Apple ID + password
	if l.session == nil {
		username := input["username"]
		if username == "" {
			return nil, fmt.Errorf("Apple ID is required")
		}
		password := input["password"]
		if password == "" {
			return nil, fmt.Errorf("Password is required")
		}
		l.username = username

		session, err := rustpushgo.LoginStart(username, password, l.cfg, l.conn)
		if err != nil {
			l.Main.Bridge.Log.Error().Err(err).Str("username", username).Msg("Login failed")
			return nil, fmt.Errorf("login failed: %w", err)
		}
		l.session = session

		if session.Needs2fa() {
			l.Main.Bridge.Log.Info().Str("username", username).Msg("Login succeeded, waiting for 2FA")
			return &bridgev2.LoginStep{
				Type:   bridgev2.LoginStepTypeUserInput,
				StepID: LoginStepTwoFactor,
				Instructions: "Enter your Apple ID verification code.\n\n" +
					"You may see a notification on your trusted Apple devices. " +
					"If not, you can generate a code manually:\n" +
					"• iPhone/iPad: Settings → [Your Name] → Sign-In & Security → Two-Factor Authentication → Get Verification Code\n" +
					"• Mac: System Settings → [Your Name] → Sign-In & Security → Two-Factor Authentication → Get Verification Code",
				UserInputParams: &bridgev2.LoginUserInputParams{
					Fields: []bridgev2.LoginInputDataField{{
						ID:   "code",
						Name: "2FA Code",
					}},
				},
			}, nil
		}

		// No 2FA needed — skip straight to IDS registration
		l.Main.Bridge.Log.Info().Str("username", username).Msg("Login succeeded without 2FA, finishing registration")
		return l.finishLogin(ctx)
	}

	// Step 2: 2FA code
	code := input["code"]
	if code == "" {
		return nil, fmt.Errorf("2FA code is required")
	}

	success, err := l.session.Submit2fa(code)
	if err != nil {
		return nil, fmt.Errorf("2FA verification failed: %w", err)
	}
	if !success {
		return nil, fmt.Errorf("2FA verification failed — invalid code")
	}

	return l.finishLogin(ctx)
}

func (l *AppleIDLogin) finishLogin(ctx context.Context) (*bridgev2.LoginStep, error) {
	log := l.Main.Bridge.Log.With().Str("component", "imessage").Logger()

	// Reuse existing session state if available and keystore matches
	session := loadCachedSession(l.User, log)
	if !session.validate(log) {
		session = nil
	}

	// Reuse existing identity if available (avoids "new Mac" notifications)
	var existingIdentityArg **rustpushgo.WrappedIdsngmIdentity
	if existing := getExistingIdentity(session, log); existing != nil {
		existingIdentityArg = &existing
	} else {
		log.Info().Msg("No existing identity found, will generate new one (first login)")
	}

	// Reuse existing IDS users/registration if available (avoids register() call)
	var existingUsersArg **rustpushgo.WrappedIdsUsers
	if existing := getExistingUsers(session, log); existing != nil {
		existingUsersArg = &existing
	} else {
		log.Info().Msg("No existing users found, will register fresh (first login)")
	}

	result, err := l.session.Finish(l.cfg, l.conn, existingIdentityArg, existingUsersArg)
	if err != nil {
		l.Main.Bridge.Log.Error().Err(err).Msg("IDS registration failed during finishLogin")
		return nil, fmt.Errorf("login completion failed: %w", err)
	}
	l.result = &result
	l.selectedDevice = -1

	// Skip device selection and passcode when CloudKit backfill is disabled —
	// iCloud Keychain is only needed for decrypting CloudKit message records.
	if !l.Main.Config.UseCloudKitBackfill() {
		log.Info().Msg("CloudKit backfill disabled, skipping device selection and passcode")
		handles := l.result.Users.GetHandles()
		if step := handleSelectionStep(handles); step != nil {
			return step, nil
		}
		if len(handles) > 0 {
			l.handle = handles[0]
		}
		return l.completeLogin(ctx)
	}

	return fetchDevicesAndPrompt(log, l.result.TokenProvider, &l.devices, &l.selectedDevice)
}

func (l *AppleIDLogin) handleDeviceSelection(device string) (*bridgev2.LoginStep, error) {
	l.selectedDevice = parseDeviceSelection(device, l.devices)
	return devicePasscodeStepForDevice(l.devices, l.selectedDevice), nil
}

func (l *AppleIDLogin) handlePasscodeAndContinue(ctx context.Context, passcode string) (*bridgev2.LoginStep, error) {
	log := l.Main.Bridge.Log.With().Str("component", "imessage").Logger()
	if err := joinKeychainWithPasscode(log, l.result.TokenProvider, passcode, l.devices, l.selectedDevice); err != nil {
		return nil, err
	}

	handles := l.result.Users.GetHandles()
	if step := handleSelectionStep(handles); step != nil {
		return step, nil
	}
	if len(handles) > 0 {
		l.handle = handles[0]
	}
	return l.completeLogin(ctx)
}

func (l *AppleIDLogin) completeLogin(ctx context.Context) (*bridgev2.LoginStep, error) {
	meta := &UserLoginMetadata{
		Platform:        "rustpush-local",
		APSState:        l.conn.State().ToString(),
		IDSUsers:        l.result.Users.ToString(),
		IDSIdentity:     l.result.Identity.ToString(),
		DeviceID:        l.cfg.GetDeviceId(),
		PreferredHandle: l.handle,
	}

	return completeLoginWithMeta(ctx, l.User, l.Main, l.username, l.cfg, l.conn, l.result, meta)
}

// ============================================================================
// External Key Login (cross-platform)
// ============================================================================

// ExternalKeyLogin implements the multi-step login flow for non-macOS platforms:
// Hardware key → Apple ID + password → 2FA code → IDS registration → device selection → passcode → handle selection → connected.
type ExternalKeyLogin struct {
	User           *bridgev2.User
	Main           *IMConnector
	hardwareKey    string
	username       string
	cfg            *rustpushgo.WrappedOsConfig
	conn           *rustpushgo.WrappedApsConnection
	session        *rustpushgo.LoginSession
	result         *rustpushgo.IdsUsersWithIdentityRecord // set after IDS registration
	handle         string                                 // chosen handle
	devices        []rustpushgo.EscrowDeviceInfo          // escrow devices (fetched after IDS registration)
	selectedDevice int                                    // index into devices (-1 = not yet selected)
}

var _ bridgev2.LoginProcessUserInput = (*ExternalKeyLogin)(nil)

func (l *ExternalKeyLogin) Cancel() {}

func (l *ExternalKeyLogin) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	return &bridgev2.LoginStep{
		Type:   bridgev2.LoginStepTypeUserInput,
		StepID: LoginStepExternalKey,
		Instructions: "Enter your hardware key (base64-encoded JSON).\n\n" +
			"This is extracted once from a real Mac using the key extraction tool.\n" +
			"It contains hardware identifiers needed for iMessage registration.",
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{{
				Type: bridgev2.LoginInputFieldTypePassword,
				ID:   "hardware_key",
				Name: "Hardware Key (base64)",
			}},
		},
	}, nil
}

func (l *ExternalKeyLogin) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	// Device passcode step (after device selection, before handle selection)
	if passcode, ok := input["passcode"]; ok && l.result != nil {
		return l.handlePasscodeAndContinue(ctx, passcode)
	}

	// Device selection step (after IDS registration, before passcode)
	if device, ok := input["device"]; ok && l.result != nil {
		return l.handleDeviceSelection(device)
	}

	// Handle selection step (after device passcode)
	if l.result != nil {
		l.handle = parseHandleSelection(input["handle"], l.result.Users.GetHandles())
		return l.completeLogin(ctx)
	}

	// Step 1: Hardware key
	if l.cfg == nil {
		hwKey := input["hardware_key"]
		if hwKey == "" {
			return nil, fmt.Errorf("hardware key is required")
		}
		l.hardwareKey = stripNonBase64(hwKey)

		rustpushgo.InitLogger()

		cfg, err := rustpushgo.CreateConfigFromHardwareKey(l.hardwareKey)
		if err != nil {
			return nil, fmt.Errorf("invalid hardware key: %w", err)
		}
		l.cfg = cfg

		// Reuse existing session state if available and keystore matches
		extLog := l.Main.Bridge.Log.With().Str("component", "imessage").Logger()
		session := loadCachedSession(l.User, extLog)
		if !session.validate(extLog) {
			session = nil
		}
		apsState := getExistingAPSState(session, extLog)
		l.conn = rustpushgo.Connect(cfg, apsState)

		nacNote := "Registration uses the hardware key for NAC validation (no Mac needed at runtime)."
		if cfg.RequiresNacRelay() {
			nacNote = "Apple Silicon hardware key detected.\n" +
				"The NAC relay server must be running on the Mac that provided this key during registration.\n" +
				"Start it with: go run tools/nac-relay/main.go"
		}
		return &bridgev2.LoginStep{
			Type:   bridgev2.LoginStepTypeUserInput,
			StepID: LoginStepAppleIDPassword,
			Instructions: "Enter your Apple ID credentials.\n" + nacNote,
			UserInputParams: &bridgev2.LoginUserInputParams{
				Fields: []bridgev2.LoginInputDataField{{
					Type: bridgev2.LoginInputFieldTypeEmail,
					ID:   "username",
					Name: "Apple ID",
				}, {
					Type: bridgev2.LoginInputFieldTypePassword,
					ID:   "password",
					Name: "Password",
				}},
			},
		}, nil
	}

	// Step 2: Apple ID + password
	if l.session == nil {
		username := input["username"]
		if username == "" {
			return nil, fmt.Errorf("Apple ID is required")
		}
		password := input["password"]
		if password == "" {
			return nil, fmt.Errorf("Password is required")
		}
		l.username = username

		session, err := rustpushgo.LoginStart(username, password, l.cfg, l.conn)
		if err != nil {
			l.Main.Bridge.Log.Error().Err(err).Str("username", username).Msg("Login failed")
			return nil, fmt.Errorf("login failed: %w", err)
		}
		l.session = session

		if session.Needs2fa() {
			l.Main.Bridge.Log.Info().Str("username", username).Msg("Login succeeded, waiting for 2FA")
			return &bridgev2.LoginStep{
				Type:   bridgev2.LoginStepTypeUserInput,
				StepID: LoginStepTwoFactor,
				Instructions: "Enter your Apple ID verification code.\n\n" +
					"You may see a notification on your trusted Apple devices.",
				UserInputParams: &bridgev2.LoginUserInputParams{
					Fields: []bridgev2.LoginInputDataField{{
						ID:   "code",
						Name: "2FA Code",
					}},
				},
			}, nil
		}

		l.Main.Bridge.Log.Info().Str("username", username).Msg("Login succeeded without 2FA")
		return l.finishLogin(ctx)
	}

	// Step 3: 2FA code
	code := input["code"]
	if code == "" {
		return nil, fmt.Errorf("2FA code is required")
	}

	success, err := l.session.Submit2fa(code)
	if err != nil {
		return nil, fmt.Errorf("2FA verification failed: %w", err)
	}
	if !success {
		return nil, fmt.Errorf("2FA verification failed — invalid code")
	}

	return l.finishLogin(ctx)
}

func (l *ExternalKeyLogin) finishLogin(ctx context.Context) (*bridgev2.LoginStep, error) {
	log := l.Main.Bridge.Log.With().Str("component", "imessage").Logger()

	// Reuse existing session state if available and keystore matches
	session := loadCachedSession(l.User, log)
	if !session.validate(log) {
		session = nil
	}

	// Reuse existing identity if available (avoids "new Mac" notifications)
	var existingIdentityArg **rustpushgo.WrappedIdsngmIdentity
	if existing := getExistingIdentity(session, log); existing != nil {
		existingIdentityArg = &existing
	} else {
		log.Info().Msg("No existing identity found, will generate new one (first login)")
	}

	// Reuse existing IDS users/registration if available (avoids register() call)
	var existingUsersArg **rustpushgo.WrappedIdsUsers
	if existing := getExistingUsers(session, log); existing != nil {
		existingUsersArg = &existing
	} else {
		log.Info().Msg("No existing users found, will register fresh (first login)")
	}

	result, err := l.session.Finish(l.cfg, l.conn, existingIdentityArg, existingUsersArg)
	if err != nil {
		l.Main.Bridge.Log.Error().Err(err).Msg("IDS registration failed during finishLogin")
		return nil, fmt.Errorf("login completion failed: %w", err)
	}
	l.result = &result
	l.selectedDevice = -1

	// Skip device selection and passcode when CloudKit backfill is disabled —
	// iCloud Keychain is only needed for decrypting CloudKit message records.
	if !l.Main.Config.UseCloudKitBackfill() {
		log.Info().Msg("CloudKit backfill disabled, skipping device selection and passcode")
		handles := l.result.Users.GetHandles()
		if step := handleSelectionStep(handles); step != nil {
			return step, nil
		}
		if len(handles) > 0 {
			l.handle = handles[0]
		}
		return l.completeLogin(ctx)
	}

	return fetchDevicesAndPrompt(log, l.result.TokenProvider, &l.devices, &l.selectedDevice)
}

func (l *ExternalKeyLogin) handleDeviceSelection(device string) (*bridgev2.LoginStep, error) {
	l.selectedDevice = parseDeviceSelection(device, l.devices)
	return devicePasscodeStepForDevice(l.devices, l.selectedDevice), nil
}

func (l *ExternalKeyLogin) handlePasscodeAndContinue(ctx context.Context, passcode string) (*bridgev2.LoginStep, error) {
	log := l.Main.Bridge.Log.With().Str("component", "imessage").Logger()
	if err := joinKeychainWithPasscode(log, l.result.TokenProvider, passcode, l.devices, l.selectedDevice); err != nil {
		return nil, err
	}

	handles := l.result.Users.GetHandles()
	if step := handleSelectionStep(handles); step != nil {
		return step, nil
	}
	if len(handles) > 0 {
		l.handle = handles[0]
	}
	return l.completeLogin(ctx)
}

func (l *ExternalKeyLogin) completeLogin(ctx context.Context) (*bridgev2.LoginStep, error) {
	meta := &UserLoginMetadata{
		Platform:        "rustpush-external-key",
		APSState:        l.conn.State().ToString(),
		IDSUsers:        l.result.Users.ToString(),
		IDSIdentity:     l.result.Identity.ToString(),
		DeviceID:        l.cfg.GetDeviceId(),
		HardwareKey:     l.hardwareKey,
		PreferredHandle: l.handle,
	}

	return completeLoginWithMeta(ctx, l.User, l.Main, l.username, l.cfg, l.conn, l.result, meta)
}

// ============================================================================
// Existing session state lookup
// ============================================================================

// cachedSessionState holds the raw strings for all three session components.
// They are validated as a group against the keystore before use, since they
// reference each other's keys and are only useful together.
type cachedSessionState struct {
	IDSIdentity     string
	APSState        string
	IDSUsers        string
	PreferredHandle string
	source          string // "database" or "backup file", for logging
}

// loadCachedSession looks up all three session components (identity, APS state,
// IDS users) from the bridge database or backup session file. Returns nil if
// nothing is found. The returned state has NOT been validated against the
// keystore yet — call validate() before using.
func loadCachedSession(user *bridgev2.User, log zerolog.Logger) *cachedSessionState {
	// Check DB first
	for _, login := range user.GetCachedUserLogins() {
		if meta, ok := login.Metadata.(*UserLoginMetadata); ok {
			if meta.IDSUsers != "" || meta.IDSIdentity != "" || meta.APSState != "" {
				log.Info().Msg("Found existing session state in database")
				return &cachedSessionState{
					IDSIdentity:     meta.IDSIdentity,
					APSState:        meta.APSState,
					IDSUsers:        meta.IDSUsers,
					PreferredHandle: meta.PreferredHandle,
					source:          "database",
				}
			}
		}
	}
	// Fall back to session file (survives DB resets)
	state := loadSessionState(log)
	if state.IDSIdentity != "" || state.APSState != "" || state.IDSUsers != "" {
		log.Info().Msg("Found existing session state in backup file")
		return &cachedSessionState{
			IDSIdentity:     state.IDSIdentity,
			APSState:        state.APSState,
			IDSUsers:        state.IDSUsers,
			PreferredHandle: state.PreferredHandle,
			source:          "backup file",
		}
	}
	return nil
}

// validate checks that the cached IDS users state references keys that exist
// in the keystore. If the keystore was wiped, never migrated, or belongs to a
// different installation, this returns false and all cached state should be
// discarded (they are a coupled set).
func (c *cachedSessionState) validate(log zerolog.Logger) bool {
	if c == nil || c.IDSUsers == "" {
		return true // nothing to validate
	}
	users := rustpushgo.NewWrappedIdsUsers(&c.IDSUsers)
	if !users.ValidateKeystore() {
		log.Warn().
			Str("source", c.source).
			Msg("Cached session state references missing keystore keys — discarding (will register fresh)")
		return false
	}
	log.Info().Str("source", c.source).Msg("Cached session state validated against keystore")
	return true
}

// getExistingIdentity returns the cached IDSNGMIdentity for reuse during
// re-authentication (avoiding "new Mac" notifications).
// The session must have been validated before calling this.
func getExistingIdentity(session *cachedSessionState, log zerolog.Logger) *rustpushgo.WrappedIdsngmIdentity {
	if session != nil && session.IDSIdentity != "" {
		log.Info().Str("source", session.source).Msg("Reusing existing identity")
		identityStr := session.IDSIdentity
		return rustpushgo.NewWrappedIdsngmIdentity(&identityStr)
	}
	return nil
}

// getExistingAPSState returns the cached APS connection state for reuse during
// re-authentication (preserves push token, avoids new device registration).
// The session must have been validated before calling this.
func getExistingAPSState(session *cachedSessionState, log zerolog.Logger) *rustpushgo.WrappedApsState {
	if session != nil && session.APSState != "" {
		log.Info().Str("source", session.source).Msg("Reusing existing APS state")
		return rustpushgo.NewWrappedApsState(&session.APSState)
	}
	log.Info().Msg("No existing APS state found, will create new connection")
	return rustpushgo.NewWrappedApsState(nil)
}

// getExistingUsers returns the cached IDSUsers for reuse during
// re-authentication (avoids calling register() which triggers notifications).
// The session must have been validated before calling this.
func getExistingUsers(session *cachedSessionState, log zerolog.Logger) *rustpushgo.WrappedIdsUsers {
	if session != nil && session.IDSUsers != "" {
		log.Info().Str("source", session.source).Msg("Reusing existing IDS users")
		return rustpushgo.NewWrappedIdsUsers(&session.IDSUsers)
	}
	return nil
}

// ============================================================================
// Shared login helpers
// ============================================================================

// formatDeviceLabel returns a human-readable label for a device, e.g.
// "Ludvig's iPhone (iPhone15,2)".
func formatDeviceLabel(d rustpushgo.EscrowDeviceInfo) string {
	if d.DeviceName != "" && d.DeviceModel != "" {
		return fmt.Sprintf("%s (%s)", d.DeviceName, d.DeviceModel)
	}
	if d.DeviceName != "" {
		return d.DeviceName
	}
	if d.DeviceModel != "" {
		return fmt.Sprintf("Device (%s)", d.DeviceModel)
	}
	return fmt.Sprintf("Device (serial: %s)", d.Serial)
}

// fetchDevicesAndPrompt fetches escrow devices from the token provider and returns
// either a device selection step (multiple devices) or skips straight to the
// passcode step (single device, auto-selected). On failure, falls back to a
// generic passcode step.
func fetchDevicesAndPrompt(log zerolog.Logger, tp **rustpushgo.WrappedTokenProvider, devices *[]rustpushgo.EscrowDeviceInfo, selectedDevice *int) (*bridgev2.LoginStep, error) {
	if tp == nil || *tp == nil {
		log.Warn().Msg("No TokenProvider available, skipping device discovery")
		return devicePasscodeStepForDevice(nil, -1), nil
	}

	devs, err := (*tp).GetEscrowDevices()
	if err != nil {
		log.Warn().Err(err).Msg("Failed to fetch escrow devices, falling back to generic passcode prompt")
		return devicePasscodeStepForDevice(nil, -1), nil
	}
	*devices = devs

	if len(devs) == 1 {
		// Auto-select the only device and go straight to passcode
		*selectedDevice = 0
		log.Info().Str("device", formatDeviceLabel(devs[0])).Msg("Single escrow device found, auto-selected")
		return devicePasscodeStepForDevice(devs, 0), nil
	}

	// Multiple devices — let the user choose. Prefix each label with its
	// 1-based index so the Matrix bot's login flow (which renders Select
	// options as a plain list in chat, not as an app dropdown) shows a
	// clearly numbered picker instead of an ambiguous bullet list.
	options := make([]string, len(devs))
	for i, d := range devs {
		options[i] = fmt.Sprintf("%d. %s", i+1, formatDeviceLabel(d))
	}

	return &bridgev2.LoginStep{
		Type:   bridgev2.LoginStepTypeUserInput,
		StepID: LoginStepSelectDevice,
		Instructions: "Multiple Apple devices were found on your account.\n" +
			"Choose which device's passcode you want to use to join the iCloud Keychain.\n\n" +
			"Pick the device whose lock-screen passcode you know.",
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{{
				Type:    bridgev2.LoginInputFieldTypeSelect,
				ID:      "device",
				Name:    "Device",
				Options: options,
			}},
		},
	}, nil
}

// parseDeviceSelection converts the user's device selection back to an index
// into the devices list. Accepts three forms so the bot's numbered-picker
// flow works regardless of how the user replies:
//   - the bare 1-based index (e.g. "2")
//   - the full numbered label we emitted (e.g. "2. iPhone 15 (iPhone15,2)")
//   - the raw device label without the prefix (e.g. "iPhone 15 (iPhone15,2)")
func parseDeviceSelection(selected string, devices []rustpushgo.EscrowDeviceInfo) int {
	trimmed := strings.TrimSpace(selected)
	if n, err := strconv.Atoi(trimmed); err == nil && n >= 1 && n <= len(devices) {
		return n - 1
	}
	for i, d := range devices {
		label := formatDeviceLabel(d)
		numbered := fmt.Sprintf("%d. %s", i+1, label)
		if trimmed == label || trimmed == numbered {
			return i
		}
	}
	// Shouldn't happen with a select field, but default to first device
	if len(devices) > 0 {
		return 0
	}
	return -1
}

// devicePasscodeStepForDevice returns a login step prompting for the passcode,
// with context about which device the passcode is for.
func devicePasscodeStepForDevice(devices []rustpushgo.EscrowDeviceInfo, selectedDevice int) *bridgev2.LoginStep {
	var instructions string
	if selectedDevice >= 0 && selectedDevice < len(devices) {
		d := devices[selectedDevice]
		instructions = fmt.Sprintf(
			"Enter the passcode for %s.\n\n"+
				"This is the PIN or password you use to unlock this device. "+
				"It's needed to join the iCloud Keychain trust circle, which gives the bridge "+
				"access to your Messages in iCloud for backfilling chat history.\n\n"+
				"Your passcode is only used once during setup and is not stored.",
			formatDeviceLabel(d),
		)
	} else {
		instructions = "Enter the passcode you use to unlock your iPhone or Mac.\n\n" +
			"This is needed to join the iCloud Keychain trust circle, which gives the bridge " +
			"access to your Messages in iCloud for backfilling chat history.\n\n" +
			"Your passcode is only used once during setup and is not stored."
	}

	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeUserInput,
		StepID:       LoginStepDevicePasscode,
		Instructions: instructions,
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{{
				Type: bridgev2.LoginInputFieldTypePassword,
				ID:   "passcode",
				Name: "Device Passcode",
			}},
		},
	}
}

// joinKeychainWithPasscode calls the Rust FFI to join the iCloud Keychain trust
// circle using the provided device passcode. This is required for PCS-encrypted
// CloudKit records (Messages in iCloud).
// If a specific device was selected, the corresponding bottle is tried first.
func joinKeychainWithPasscode(log zerolog.Logger, tp **rustpushgo.WrappedTokenProvider, passcode string, devices []rustpushgo.EscrowDeviceInfo, selectedDevice int) error {
	if tp == nil || *tp == nil {
		log.Warn().Msg("No TokenProvider available, skipping keychain join")
		return nil
	}
	log.Info().Msg("Joining iCloud Keychain trust circle...")

	var result string
	var err error
	if selectedDevice >= 0 && selectedDevice < len(devices) {
		deviceIndex := devices[selectedDevice].Index
		log.Info().Uint32("device_index", deviceIndex).Str("device", formatDeviceLabel(devices[selectedDevice])).Msg("Using preferred device bottle")
		result, err = (*tp).JoinKeychainCliqueForDevice(passcode, deviceIndex)
	} else {
		result, err = (*tp).JoinKeychainClique(passcode)
	}

	if err != nil {
		log.Error().Err(err).Msg("Failed to join keychain trust circle")
		return fmt.Errorf("failed to join iCloud Keychain: %w", err)
	}
	log.Info().Str("result", result).Msg("Successfully joined iCloud Keychain trust circle")
	return nil
}

// handleSelectionStep returns a login step prompting the user to pick a handle,
// or nil if there are no handles. Always prompts (even with 1 handle) so the
// preferred handle is explicitly chosen and persisted.
//
// Handles are presented as a numbered list ("1. tel:+1…", "2. mailto:…") so the
// Matrix bot's chat-driven login flow lets users reply with just a number
// instead of retyping a full phone or email — same shape as fetchDevicesAndPrompt.
func handleSelectionStep(handles []string) *bridgev2.LoginStep {
	if len(handles) == 0 {
		return nil
	}
	options := make([]string, len(handles))
	for i, h := range handles {
		options[i] = fmt.Sprintf("%d. %s", i+1, h)
	}
	return &bridgev2.LoginStep{
		Type:   bridgev2.LoginStepTypeUserInput,
		StepID: LoginStepSelectHandle,
		Instructions: "Choose which identity to use for outgoing iMessages.\n" +
			"This is what recipients will see your messages \"from\".\n\n" +
			"Reply with the number (e.g. `1`) or the full handle.",
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{{
				Type:    bridgev2.LoginInputFieldTypeSelect,
				ID:      "handle",
				Name:    "Send messages as",
				Options: options,
			}},
		},
	}
}

// parseHandleSelection converts the user's handle reply back to the actual
// handle string. Mirrors parseDeviceSelection — accepts a bare 1-based index
// ("2"), the full numbered label we emitted ("2. tel:+1..."), or the raw
// handle. Falls back to the trimmed input so direct-API callers that round-trip
// the option value continue to work.
func parseHandleSelection(selected string, handles []string) string {
	trimmed := strings.TrimSpace(selected)
	if n, err := strconv.Atoi(trimmed); err == nil && n >= 1 && n <= len(handles) {
		return handles[n-1]
	}
	for i, h := range handles {
		numbered := fmt.Sprintf("%d. %s", i+1, h)
		if trimmed == h || trimmed == numbered {
			return h
		}
	}
	return trimmed
}

// completeLoginWithMeta is the shared tail of both login flows: creates the
// IMClient, persists metadata, saves the identity backup file, and starts the
// bridge connection.
func completeLoginWithMeta(
	ctx context.Context,
	user *bridgev2.User,
	main *IMConnector,
	username string,
	cfg *rustpushgo.WrappedOsConfig,
	conn *rustpushgo.WrappedApsConnection,
	result *rustpushgo.IdsUsersWithIdentityRecord,
	meta *UserLoginMetadata,
) (*bridgev2.LoginStep, error) {
	log := main.Bridge.Log.With().Str("component", "imessage").Logger()

	// Store iCloud account persist data for TokenProvider restoration
	if result.AccountPersist != nil {
		meta.AccountUsername = result.AccountPersist.Username
		meta.AccountHashedPasswordHex = result.AccountPersist.HashedPasswordHex
		meta.AccountPET = result.AccountPersist.Pet
		meta.AccountADSID = result.AccountPersist.Adsid
		meta.AccountDSID = result.AccountPersist.Dsid
		meta.AccountSPDBase64 = result.AccountPersist.SpdBase64
		log.Info().Str("dsid", meta.AccountDSID).Msg("iCloud account credentials available for TokenProvider")
		// Also capture the MobileMe delegate so it can be seeded on restore
		if result.TokenProvider != nil && *result.TokenProvider != nil {
			tp := *result.TokenProvider
			if delegateJSON, mmeErr := tp.GetMmeDelegateJson(); mmeErr == nil && delegateJSON != nil {
				meta.MmeDelegateJSON = *delegateJSON
				log.Info().Msg("Captured MobileMe delegate for persistence")
			}
		}
	} else {
		log.Warn().Msg("No account persist data from login — cloud services will not be available")
	}

	// Persist full session state to backup file so it survives DB resets.
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

	loginID := networkid.UserLoginID(result.Users.LoginId(0))

	client := &IMClient{
		Main:                    main,
		config:                  cfg,
		users:                   result.Users,
		identity:                result.Identity,
		connection:              conn,
		tokenProvider:           result.TokenProvider,
		contactsReady:           false,
		contactsReadyCh:         make(chan struct{}),
		cloudStore:              newCloudBackfillStore(main.Bridge.DB.Database, loginID),
		sharedProfileStore:      newSharedProfileStore(main.Bridge.DB.Database, loginID),
		pendingAttachments:      newPendingAttachmentStore(main.Bridge.DB.Database, loginID),
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

	ul, err := user.NewLogin(ctx, &database.UserLogin{
		ID:         loginID,
		RemoteName: username,
		RemoteProfile: status.RemoteProfile{
			Name: username,
		},
		Metadata: meta,
	}, &bridgev2.NewLoginParams{
		DeleteOnConflict: true,
		LoadUserLogin: func(ctx context.Context, login *bridgev2.UserLogin) error {
			client.UserLogin = login
			login.Client = client
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create user login: %w", err)
	}

	go client.Connect(context.Background())

	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeComplete,
		StepID:       LoginStepComplete,
		Instructions: "Successfully logged in to iMessage. Bridge is starting.",
		CompleteParams: &bridgev2.LoginCompleteParams{
			UserLoginID: ul.ID,
			UserLogin:   ul,
		},
	}, nil
}
