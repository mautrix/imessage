// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
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

package connector

import (
	"bytes"
	"context"
	cryptoRand "crypto/rand"
	"encoding/base64"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"

	"github.com/lrhodin/imessage/pkg/rustpushgo"
)

// cmdFaceTime generates a shareable FaceTime link via the rustpush FaceTime
// client. The link is associated with the bridge's iMessage handle so any
// Matrix user can share it with their iMessage contacts to start a FaceTime
// call. Subsequent calls with the same handle return the same persistent link
// until it is cleared with !facetime-clear.
//
// Usage:
//
//	!facetime           — link for the primary handle
//	!facetime [handle]  — link for a specific registered handle
var cmdFaceTime = &commands.FullHandler{
	Name:    "facetime",
	Aliases: []string{"ft"},
	Func:    fnFaceTime,
	Help: commands.HelpMeta{
		Section:     HelpSectionFaceTime,
		Description: "In a DM portal: ring the contact and post a FaceTime web link anyone can tap (iOS, macOS, Android, Windows, Linux) to join the call. In the management room: print a shareable FaceTime web link for your account.",
		Args:        "[handle]",
	},
	RequiresLogin: true,
}

// cmdFaceTimeSend generates a FaceTime link and sends it as an iMessage to
// the contact in the current portal. Runs in a portal room only; the link is
// delivered transparently to the iMessage contact without appearing as a
// regular Matrix message.
var cmdFaceTimeSend = &commands.FullHandler{
	Name: "facetime-send",
	Func: fnFaceTimeSend,
	Help: commands.HelpMeta{
		Section:     HelpSectionFaceTime,
		Description: "Generate a FaceTime link and iMessage it to the contact in this portal so they can tap to join.",
	},
	RequiresLogin: true,
}

// cmdFaceTimeClear revokes all bridge FaceTime links so that the next
// !facetime call generates a fresh one.
var cmdFaceTimeClear = &commands.FullHandler{
	Name: "facetime-clear",
	Func: fnFaceTimeClear,
	Help: commands.HelpMeta{
		Section:     HelpSectionFaceTime,
		Description: "Revoke every bridge-created FaceTime link so the next `facetime` call mints a fresh one.",
	},
	RequiresLogin: true,
}

var cmdFaceTimeState = &commands.FullHandler{
	Name: "facetime-state",
	Func: fnFaceTimeState,
	Help: commands.HelpMeta{
		Section:     HelpSectionFaceTime,
		Description: "Dump raw FaceTime client state (sessions, links, pending requests) as JSON — debugging only.",
	},
	RequiresLogin: true,
}

var cmdFaceTimeSessionLink = &commands.FullHandler{
	Name: "facetime-session-link",
	Func: fnFaceTimeSessionLink,
	Help: commands.HelpMeta{
		Section:     HelpSectionFaceTime,
		Description: "Rebuild the join URL for an existing FaceTime session from its GUID.",
		Args:        "<session-guid>",
	},
	RequiresLogin: true,
}

var cmdFaceTimeUseLink = &commands.FullHandler{
	Name: "facetime-use-link",
	Func: fnFaceTimeUseLink,
	Help: commands.HelpMeta{
		Section:     HelpSectionFaceTime,
		Description: "Reassign a FaceTime link from one usage tag to another (e.g. 'personal' → 'work').",
		Args:        "<old-usage> <new-usage>",
	},
	RequiresLogin: true,
}

var cmdFaceTimeDeleteLink = &commands.FullHandler{
	Name: "facetime-delete-link",
	Func: fnFaceTimeDeleteLink,
	Help: commands.HelpMeta{
		Section:     HelpSectionFaceTime,
		Description: "Delete a specific FaceTime link by its pseud identifier.",
		Args:        "<pseud>",
	},
	RequiresLogin: true,
}

var cmdFaceTimeLetMeIn = &commands.FullHandler{
	Name: "facetime-letmein",
	Func: fnFaceTimeLetMeIn,
	Help: commands.HelpMeta{
		Section:     HelpSectionFaceTime,
		Description: "List FaceTime Let-Me-In requests that are pending delegated approval from this bridge.",
	},
	RequiresLogin: true,
}

var cmdFaceTimeLetMeInApprove = &commands.FullHandler{
	Name: "facetime-letmein-approve",
	Func: fnFaceTimeLetMeInApprove,
	Help: commands.HelpMeta{
		Section:     HelpSectionFaceTime,
		Description: "Approve a pending Let-Me-In request by delegation UUID (optionally restrict access to a named group).",
		Args:        "<delegation-uuid> [approved-group]",
	},
	RequiresLogin: true,
}

var cmdFaceTimeLetMeInDeny = &commands.FullHandler{
	Name: "facetime-letmein-deny",
	Func: fnFaceTimeLetMeInDeny,
	Help: commands.HelpMeta{
		Section:     HelpSectionFaceTime,
		Description: "Deny a pending Let-Me-In request by delegation UUID.",
		Args:        "<delegation-uuid>",
	},
	RequiresLogin: true,
}

var cmdFaceTimeCreateSession = &commands.FullHandler{
	Name: "facetime-create-session",
	Func: fnFaceTimeCreateSession,
	Help: commands.HelpMeta{
		Section:     HelpSectionFaceTime,
		Description: "Create a new FaceTime session for a given group ID and list of participant handles.",
		Args:        "<group-id> <participants...>",
	},
	RequiresLogin: true,
}

var cmdFaceTimeRing = &commands.FullHandler{
	Name: "facetime-ring",
	Func: fnFaceTimeRing,
	Help: commands.HelpMeta{
		Section:     HelpSectionFaceTime,
		Description: "Ring the listed targets in an existing FaceTime session; pass --letmein to include a LetMeIn push.",
		Args:        "<session-id> <targets...> [--letmein]",
	},
	RequiresLogin: true,
}

var cmdFaceTimeAddMembers = &commands.FullHandler{
	Name: "facetime-add-members",
	Func: fnFaceTimeAddMembers,
	Help: commands.HelpMeta{
		Section:     HelpSectionFaceTime,
		Description: "Add participants to an existing FaceTime session; pass --letmein to send a LetMeIn push.",
		Args:        "<session-id> <handles...> [--letmein]",
	},
	RequiresLogin: true,
}

var cmdFaceTimeRemoveMembers = &commands.FullHandler{
	Name: "facetime-remove-members",
	Func: fnFaceTimeRemoveMembers,
	Help: commands.HelpMeta{
		Section:     HelpSectionFaceTime,
		Description: "Remove participants from an existing FaceTime session.",
		Args:        "<session-id> <handles...>",
	},
	RequiresLogin: true,
}

// FaceTime link usage slots, mirroring OpenBubbles' rotation taxonomy
// (rustpush_service.dart:2699-2708). Each slot is an Apple-server-side usage
// tag bound to a distinct pre-minted pseud. OB pre-mints "next" and
// "nextincomingcall" at startup so Apple's identity-resolution layer has
// time to fully propagate the pseud↔handle binding before the link is
// actually used; using a freshly-minted link on-demand appears to leave
// the binding incomplete and causes peer's UI to attribute media to the
// bridge's IDS endpoint rather than the webview pseud.
const (
	ftLinkUsageNext             = "next"              // pre-minted outbound
	ftLinkUsageCurrent          = "current"           // in-flight outbound
	ftLinkUsageCurrentOld       = "current-old"       // prior outbound
	ftLinkUsageNextIncomingCall = "nextincomingcall"  // pre-minted inbound
	ftLinkUsageIncomingCall     = "incomingcall"      // in-flight inbound
	ftLinkUsageIncomingCallOld  = "incomingcall-old"  // prior inbound
)

// armBridgeFaceTimeCall does the Rust-side dance shared by the outbound
// !im facetime command and the missed-call callback notice: mint a session
// with no ring, queue a pending ring against the target, mint a session-
// specific join link (no letmein indirection), and pre-fill the web FT join
// page's display-name field with the caller's handle. Tapping the returned
// link joins this specific session directly — upstream's ConversationParticipantDidJoin
// wire then fires the JoinEvent that triggers the pending ring to the target.
//
// ringTTLSecs is the pending-ring lifetime: 60s for the live `!im facetime`
// flow (caller is actively waiting), much longer for missed-call callbacks
// (the user may not see the notice for a while).
func armBridgeFaceTimeCall(
	ft *rustpushgo.WrappedFaceTimeClient,
	callerHandle string,
	targetHandle string,
	ringTTLSecs uint64,
	proxy *faceTimeProxy,
	displayName string,
) (webLink string, sessionID string, err error) {
	sessionID, err = newFaceTimeSessionID()
	if err != nil {
		return "", "", fmt.Errorf("generate session ID: %w", err)
	}

	createErr := retryOnAPNsFlap(func() error {
		return ft.CreateSessionNoRing(sessionID, callerHandle, []string{targetHandle})
	})
	if createErr != nil {
		return "", sessionID, fmt.Errorf("create_session_no_ring: %w", createErr)
	}

	if pendErr := ft.RegisterPendingRing(sessionID, callerHandle, []string{targetHandle}, ringTTLSecs); pendErr != nil {
		return "", sessionID, fmt.Errorf("register_pending_ring: %w", pendErr)
	}

	// Use the pre-minted "next" outbound link rather than GetSessionLink.
	// get_link_for_usage returns a link whose pseud has been on Apple's
	// FT server long enough for identity-resolution propagation to complete
	// (mirroring OpenBubbles' rotation model). GetSessionLink minted a
	// session-specific pseud at call time, which appears to leave Apple's
	// server with an incomplete pseud↔handle binding when the webview
	// joins — causing peer's UI to attribute media to the bridge's IDS
	// endpoint instead of the joining webview.
	var link string
	linkErr := retryOnAPNsFlap(func() error {
		var innerErr error
		link, innerErr = getFaceTimeLinkWithRecovery(ft, callerHandle, ftLinkUsageNext)
		return innerErr
	})
	if linkErr != nil {
		return "", sessionID, fmt.Errorf("get_link_for_usage(next): %w", linkErr)
	}

	// Pin the link's session_link to this outgoing session so
	// auto_approve_bridge_letmein's linked_group branch matches when the
	// webview's letmein arrives. Bind uses the pre-rotation usage name
	// (the link is still in the "next" slot at this point); rotation
	// below moves it to "current" but preserves session_link.
	if bindErr := ft.BindBridgeLinkToSession(callerHandle, ftLinkUsageNext, sessionID); bindErr != nil {
		return "", sessionID, fmt.Errorf("bind_bridge_link_to_session: %w", bindErr)
	}

	// Rotate asynchronously so the next outbound call has a freshly
	// pre-minted "next" slot. The rotation itself isn't load-bearing for
	// this call — it moves "next" → "current" + mints a new "next" —
	// and its failure shouldn't block the ring we're about to emit.
	go func() {
		if err := rotateOutboundLink(ft, callerHandle); err != nil {
			// Silenced: not load-bearing for the current call. Next
			// call will retry via premintFaceTimeLinks-style
			// fallback if needed.
			_ = err
		}
	}()

	link = appendFaceTimeLinkName(link, displayName)
	// Wrap the raw facetime.apple.com link with the bridge's proxy if the
	// proxy endpoint is live. The proxy preserves the fragment and serves
	// a patched main.js that auto-submits the name and clicks Join — OB's
	// convention, see facetime_proxy.go. iOS/macOS UAs are 302-redirected
	// back to the raw URL so native FaceTime.app Universal Link handling
	// kicks in.
	link = proxy.buildLink(link)
	return link, sessionID, nil
}

// appendFaceTimeLinkName appends &n=<base64-name> to a FaceTime web join
// link so Apple's join page pre-fills the display-name field, sparing the
// user from typing their name before joining.
//
// Apple's web FT page base64-decodes the &n= value (matching the &k= and
// &p= pattern — both are URL-safe base64 of binary data in
// facetime.rs:102). Sending raw text caused the page to atob() it and
// render gibberish (reverted in 8d9c8f2); base64-encoding here matches
// what the page expects.
//
// This is the bridge's analogue of OpenBubbles' Android-native webview
// name injection (rustpush_service.dart:3222 passes "name" separately
// via MethodChannel). We can't inject JS into Apple's page from a
// Matrix link, so we ride the URL-fragment path instead.
func appendFaceTimeLinkName(link, name string) string {
	if name == "" {
		return link
	}
	encoded := base64.RawURLEncoding.EncodeToString([]byte(name))
	if strings.Contains(link, "#") {
		return link + "&n=" + encoded
	}
	return link + "#n=" + encoded
}

// resolveFaceTimeDisplayName returns the value to stamp into the FaceTime
// web join link's `n=` fragment so the recipient sees a real name instead
// of a raw phone number on the peer's incoming-call UI. Preference order:
//  1. `facetime_display_name` config override (per-user escape hatch),
//  2. Apple ID "First Last" read from the cached SPD dict (the name Apple
//     has on file for this account — same name iMessage and FaceTime
//     already show everywhere else),
//  3. the bare iMessage handle as a last-resort fallback.
func (c *IMClient) resolveFaceTimeDisplayName(ctx context.Context) string {
	if override := strings.TrimSpace(c.Main.Config.FaceTimeDisplayName); override != "" {
		return override
	}
	if c.tokenProvider != nil && *c.tokenProvider != nil {
		if name := strings.TrimSpace((*c.tokenProvider).AppleAccountFullName()); name != "" {
			return name
		}
		c.UserLogin.Log.Debug().Msg("FaceTime display-name: Apple Account SPD lookup returned empty; set facetime_display_name in config to override")
	}
	return stripIdentifierPrefix(c.handle)
}

// retryOnAPNsFlap retries an APNs-dependent operation up to three times
// across 3s total when a transient APNs flap manifests as SendTimedOut or
// "Send timeout; try again". APNs' reconnect grace is 30s on our side, so a
// short retry almost always lands on the restored connection.
func retryOnAPNsFlap(op func() error) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = op()
		if err == nil {
			return nil
		}
		if !isLikelyDeliveredSendTimeout(err) {
			return err
		}
		time.Sleep(time.Duration(1+attempt) * time.Second)
	}
	return err
}

var faceTimeURLRegex = regexp.MustCompile(`(?i)(?:facetime://[^\s<>")']+|(?:https?://)?(?:www\.)?facetime\.apple\.com/[^\s<>")']+)`)

func fnFaceTime(ce *commands.Event) {
	// In a DM portal with no explicit handle arg, `!facetime` acts as
	// "call the contact": it creates a fresh session, posts the join link
	// as a silent bot notice, and queues a ring so the contact's phone
	// only rings once the caller actually taps the link. Group portals and
	// explicit-handle usage fall through to the link-only behavior below.
	if ce.Portal != nil && len(ce.Args) == 0 {
		if handled := fnFaceTimeCallInPortal(ce); handled {
			return
		}
	}

	client, handles, explicit, ok := faceTimeClientAndCandidates(ce)
	if !ok {
		return
	}

	ft, err := client.client.GetFacetimeClient()
	if err != nil {
		ce.Reply("Failed to initialize FaceTime client: %v", err)
		return
	}

	var lastErr error
	for _, handle := range handles {
		link, linkErr := getFaceTimeLinkWithRecovery(ft, handle, ftLinkUsageNext)
		if linkErr == nil {
			go func(h string) {
				_ = rotateOutboundLink(ft, h)
			}(handle)
			link = appendFaceTimeLinkName(link, client.resolveFaceTimeDisplayName(ce.Ctx))
			link = client.Main.ftProxy.buildLink(link)
			ce.Reply("FaceTime link for **%s**: %s\n\nShare this link to start a FaceTime call. Use `!im facetime-clear` to revoke it.", handle, link)
			return
		}
		lastErr = linkErr
		if explicit {
			break
		}
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no usable FaceTime handles found")
	}
	ce.Reply("Failed to get FaceTime link: %v\n\nAvailable handles: `%s`", lastErr, strings.Join(handles, "`, `"))
}

// fnFaceTimeCallInPortal handles the portal-room variant of !facetime. It
// mirrors the OpenBubbles outgoing-call flow (openbubbles-app
// rustpush_service.dart#placeOutgoingCall):
//
//  1. Generate a fresh session UUID.
//  2. ft.CreateSession(uuid, bridge.handle, [target]) — rings the target
//     via upstream's prop_up_conv(ring=true) Invitation push.
//  3. ft.GetLinkForUsage(bridge.handle, "next") — fetch the pre-minted
//     outbound FaceTime web link from the "next" rotation slot
//     (mirroring OpenBubbles rustpush_service.dart:2718). Works in any
//     browser: a stable https://facetime.apple.com/join URL —
//     Chrome on Android, Firefox on Linux, Edge on Windows, Safari on
//     iOS/macOS).
//  4. Post the web URL to the user. facetime.apple.com is an Apple
//     Universal Link, so on iOS / macOS the OS intercepts the domain
//     and hands the URL off to the FaceTime app directly — no browser
//     round-trip. On Android / Windows / Linux the same URL opens in
//     the default browser and runs the FaceTime web client. One link
//     covers every platform.
//
// When the user taps the web URL:
//   - They authenticate against the personal link's pseudonym by sending a
//     let-me-in request to the bridge.
//   - The bridge's APNs recv loop dispatches the request through
//     auto_approve_bridge_letmein (pkg/rustpushgo/src/lib.rs:2924), which
//     walks the bridge's sessions looking for one owned by the link's
//     handle that has is_ringing_inaccurate=true — the session we just
//     created in step 2 — and routes the let-me-in there.
//   - Upstream respond_letmein (facetime.rs:1057) detects the OneOnOne-mode
//     bug that was breaking web-joiner connections and calls
//     prop_up_conv(ring=false) as the bridge to bump the active-participant
//     count, so Apple's callservicesd correctly exits OneOnOne mode before
//     the web client joins. Without that step, upstream's own comment
//     describes the failure as "If said device is the only one in the call
//     (ringing), it sees zero (0) remote participants, thus the condition
//     fails; OneOnOne mode is not exited, and the call fails." — the exact
//     symptom the bridge was exhibiting with the old get_session_link flow.
//
// Why not ft.GetSessionLink? Session links bypass the let-me-in path and
// therefore the OneOnOne-mode workaround. The web joiner enters the
// session directly via the session's pseudonym, the bridge has no
// opportunity to bump the participant count, and the call stalls on a
// locked OneOnOne state — this was the "rings, I join, they pick up, it
// never connects" bug.
//
// Returns true if the command was handled; false to fall through to the
// generic link-only branch (group portals, no usable target handle, etc.).
func fnFaceTimeCallInPortal(ce *commands.Event) bool {
	portalID := string(ce.Portal.ID)
	// Group portals fall through — the outgoing-call flow only targets a
	// single contact for now.
	if strings.HasPrefix(portalID, "gid:") || strings.Contains(portalID, ",") {
		return false
	}

	login := ce.User.GetDefaultLogin()
	if login == nil {
		ce.Reply("No active login found.")
		return true
	}
	client, isClient := login.Client.(*IMClient)
	if !isClient || client == nil || client.client == nil {
		ce.Reply("Bridge client not available.")
		return true
	}
	if client.handle == "" {
		ce.Reply("No iMessage handle configured. Please complete bridge setup first.")
		return true
	}

	conv := client.portalToConversation(ce.Portal)
	var target string
	for _, p := range conv.Participants {
		if !client.isMyHandle(p) {
			target = p
			break
		}
	}
	if target == "" {
		return false
	}

	ft, err := client.client.GetFacetimeClient()
	if err != nil {
		ce.Reply("Failed to initialize FaceTime client: %v", err)
		return true
	}

	webLink, sessionID, err := armBridgeFaceTimeCall(ft, client.handle, target, 60, client.Main.ftProxy, client.resolveFaceTimeDisplayName(ce.Ctx))
	if err != nil {
		switch {
		case isNonRetryableResourceClosed(err):
			ce.Reply("Failed to start FaceTime call: %v\n\nThe bridge's iMessage connection is in a terminal state — retrying this command won't help. The bridge needs to reconnect (try again in a minute, or log out and back in if it persists).", err)
		case isLikelyDeliveredSendTimeout(err):
			ce.Reply("Failed to start FaceTime call: %v\n\nSend-ack timeouts usually clear on a second try — run `!im facetime` again.", err)
		default:
			ce.Reply("Failed to start FaceTime call: %v", err)
		}
		return true
	}
	_ = sessionID

	bare := stripIdentifierPrefix(target)

	// One URL for everyone. facetime.apple.com is an Apple Universal Link:
	// iOS / macOS intercept the domain and hand the URL off to the FaceTime
	// app directly (no browser round-trip). Android / Windows / Linux just
	// open the web FaceTime client in the default browser. Same link,
	// platform-appropriate handling.
	ce.Reply(
		"📞 **FaceTime call ready for %s.**\n\n"+
			"[**🌐 Join FaceTime call**](%s)\n\n"+
			"⚠️ **Tapping this link will ring %s's phone.** The ring fires the moment you join — open the link when you're ready to be on camera. If you don't tap within 60 seconds the session is dropped and nothing rings. Works on iOS, macOS, Android, Windows, and Linux.\n\n"+
			"Raw URL: %s",
		bare, webLink, bare, webLink,
	)
	return true
}

// newFaceTimeSessionID returns a random uppercase UUID v4 — Apple's FaceTime
// session GUID wire format.
func newFaceTimeSessionID() (string, error) {
	var b [16]byte
	if _, err := cryptoRand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // RFC 4122 version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 1
	return strings.ToUpper(fmt.Sprintf(
		"%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		b[0], b[1], b[2], b[3], b[4], b[5], b[6], b[7],
		b[8], b[9], b[10], b[11], b[12], b[13], b[14], b[15],
	)), nil
}

// fnFaceTimeSend is the handler for !facetime-send. It generates a FaceTime
// link for the bridge's primary handle and sends it as an iMessage to the
// contact in the current portal. Must be run from inside a bridged portal room.
func fnFaceTimeSend(ce *commands.Event) {
	if ce.Portal == nil {
		ce.Reply("This command must be run from inside a bridged portal room.")
		return
	}

	login := ce.User.GetDefaultLogin()
	if login == nil {
		ce.Reply("No active login found.")
		return
	}
	client, isOK := login.Client.(*IMClient)
	if !isOK || client == nil || client.client == nil {
		ce.Reply("Bridge client not available.")
		return
	}
	if client.handle == "" {
		ce.Reply("No iMessage handle configured. Please complete bridge setup first.")
		return
	}

	ft, err := client.client.GetFacetimeClient()
	if err != nil {
		ce.Reply("Failed to initialize FaceTime client: %v", err)
		return
	}

	link, linkErr := getFaceTimeLinkWithRecovery(ft, client.handle, ftLinkUsageNext)
	if linkErr != nil {
		ce.Reply("Failed to get FaceTime link: %v", linkErr)
		return
	}
	go func() {
		_ = rotateOutboundLink(ft, client.handle)
	}()
	link = appendFaceTimeLinkName(link, client.resolveFaceTimeDisplayName(ce.Ctx))
	link = client.Main.ftProxy.buildLink(link)

	conv := client.portalToConversation(ce.Portal)
	if _, sendErr := client.client.SendMessage(conv, link, nil, client.handle, nil, nil, nil); sendErr != nil {
		recipient := stripIdentifierPrefix(string(ce.Portal.ID))
		if isLikelyDeliveredSendTimeout(sendErr) {
			ce.Reply("FaceTime link send timed out waiting for Apple ACK, but it may have already delivered to **%s**.\n\nCheck with them before retrying to avoid duplicates.", recipient)
			return
		}
		ce.Reply("Failed to send FaceTime link via iMessage: %v", sendErr)
		return
	}

	recipient := stripIdentifierPrefix(string(ce.Portal.ID))
	ce.Reply("FaceTime link sent to **%s** via iMessage.", recipient)
}

func getFaceTimeLinkWithRecovery(ft *rustpushgo.WrappedFaceTimeClient, handle, usage string) (string, error) {
	link, err := safeFaceTimeGetLink(ft, handle, usage)
	if err == nil {
		return link, nil
	}
	if !isRecoverableFaceTimeStateError(err) {
		return "", err
	}
	if clearErr := ft.ClearLinks(); clearErr != nil {
		return "", fmt.Errorf("%w (failed to clear stale FaceTime links: %v)", err, clearErr)
	}
	return safeFaceTimeGetLink(ft, handle, usage)
}

// rotateOutboundLink mirrors OpenBubbles' rotateLink (rustpush_service.dart:2705):
// "current" → "current-old", "next" → "current", mint new "next". The
// first two UseLinkFor calls may fail with NotFound on first run (slots
// don't exist yet); that's expected and the error is swallowed. The
// trailing mint is the part we care about — it leaves a fresh pre-minted
// link in "next" for the subsequent outbound call, giving Apple's server
// time to propagate the pseud↔handle binding before that call actually
// uses the link.
func rotateOutboundLink(ft *rustpushgo.WrappedFaceTimeClient, handle string) error {
	_ = ft.UseLinkFor(ftLinkUsageCurrent, ftLinkUsageCurrentOld)
	_ = ft.UseLinkFor(ftLinkUsageNext, ftLinkUsageCurrent)
	_, err := ft.GetLinkForUsage(handle, ftLinkUsageNext)
	return err
}

// rotateIncomingLink mirrors OpenBubbles' rotateIncomingLink (rustpush_service.dart:2699):
// "incomingcall" → "incomingcall-old", "nextincomingcall" → "incomingcall",
// mint new "nextincomingcall". Called after an inbound call is answered
// (peer-initiated FT ring dispatched to user via Matrix notice) so the
// next inbound ring uses a freshly pre-minted link.
func rotateIncomingLink(ft *rustpushgo.WrappedFaceTimeClient, handle string) error {
	_ = ft.UseLinkFor(ftLinkUsageIncomingCall, ftLinkUsageIncomingCallOld)
	_ = ft.UseLinkFor(ftLinkUsageNextIncomingCall, ftLinkUsageIncomingCall)
	_, err := ft.GetLinkForUsage(handle, ftLinkUsageNextIncomingCall)
	return err
}

// premintFaceTimeLinks fills the pre-minted slots ("next" and
// "nextincomingcall") at FT-client initialization. Idempotent — if a slot
// is already populated, GetLinkForUsage returns the existing link.
// Intentionally fire-and-forget: transient network failures are tolerable
// because rotateOutboundLink / rotateIncomingLink re-mint after each call.
func premintFaceTimeLinks(ft *rustpushgo.WrappedFaceTimeClient, handle string) {
	if handle == "" {
		return
	}
	if _, err := ft.GetLinkForUsage(handle, ftLinkUsageNext); err != nil {
		// Caller logs; we only signal via return values.
		_ = err
	}
	if _, err := ft.GetLinkForUsage(handle, ftLinkUsageNextIncomingCall); err != nil {
		_ = err
	}
}

func safeFaceTimeGetLink(ft *rustpushgo.WrappedFaceTimeClient, handle, usage string) (link string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("facetime client panicked: %v", r)
		}
	}()
	return ft.GetLinkForUsage(handle, usage)
}

func isRecoverableFaceTimeStateError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "no entry found for key") ||
		strings.Contains(msg, "No link??") ||
		strings.Contains(msg, "Failed to validate pseudonym")
}

func isLikelyDeliveredSendTimeout(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "send timeout; try again") ||
		strings.Contains(msg, "sendtimedout")
}

// isNonRetryableResourceClosed matches upstream's
// PushError::DoNotRetry(ResourceClosed) — a terminal failure of the bridge's
// IdentityManager resource loop (util.rs ResourceManager goes to
// ResourceState::Closed after a DoNotRetry generate error). Once that
// happens, every identity.cache_keys / send_message call fails identically
// until the bridge reconnects; retrying the same command is pointless.
func isNonRetryableResourceClosed(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Resource has been closed") ||
		strings.Contains(msg, "Do not retry")
}

func faceTimeClientAndCandidates(ce *commands.Event) (client *IMClient, handles []string, explicit bool, ok bool) {
	login := ce.User.GetDefaultLogin()
	if login == nil {
		ce.Reply("No active login found.")
		return nil, nil, false, false
	}
	var isOK bool
	client, isOK = login.Client.(*IMClient)
	if !isOK || client == nil || client.client == nil {
		ce.Reply("Bridge client not available.")
		return nil, nil, false, false
	}

	if len(client.allHandles) == 0 && client.handle == "" {
		ce.Reply("No iMessage handle configured. Please complete bridge setup first.")
		return nil, nil, false, false
	}

	if len(ce.Args) > 0 {
		explicit = true
		requested := strings.TrimSpace(ce.Args[0])
		resolved, found := resolveFaceTimeHandle(requested, client.allHandles)
		if !found {
			ce.Reply("Handle `%s` is not registered on this account. Available handles: `%s`", requested, strings.Join(client.allHandles, "`, `"))
			return nil, nil, true, false
		}
		return client, []string{resolved}, true, true
	}

	seen := make(map[string]struct{}, len(client.allHandles)+1)
	appendHandle := func(handle string) {
		if handle == "" {
			return
		}
		if _, exists := seen[handle]; exists {
			return
		}
		seen[handle] = struct{}{}
		handles = append(handles, handle)
	}
	appendHandle(client.handle)
	for _, handle := range client.allHandles {
		appendHandle(handle)
	}
	if len(handles) == 0 {
		ce.Reply("No iMessage handle configured. Please complete bridge setup first.")
		return nil, nil, false, false
	}
	return client, handles, false, true
}

func resolveFaceTimeHandle(requested string, available []string) (string, bool) {
	normalized := normalizeIdentifierForPortalID(addIdentifierPrefix(requested))
	for _, handle := range available {
		if normalizeIdentifierForPortalID(handle) == normalized {
			return handle, true
		}
	}
	return "", false
}

func fnFaceTimeClear(ce *commands.Event) {
	client, _, ok := faceTimeClientAndHandle(ce)
	if !ok {
		return
	}

	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("facetime client panicked: %v", r)
			}
		}()
		ft, ftErr := client.client.GetFacetimeClient()
		if ftErr != nil {
			err = ftErr
			return
		}
		err = ft.ClearLinks()
	}()

	if err != nil {
		ce.Reply("Failed to clear FaceTime links: %v", err)
		return
	}

	ce.Reply("All FaceTime links have been revoked. Use `!facetime` to generate a new one.")
}

// faceTimeClientAndHandle resolves the IMClient and target handle from a
// command event. Replies with an error message and returns ok=false on failure.
func faceTimeClientAndHandle(ce *commands.Event) (client *IMClient, handle string, ok bool) {
	login := ce.User.GetDefaultLogin()
	if login == nil {
		ce.Reply("No active login found.")
		return nil, "", false
	}
	var isOK bool
	client, isOK = login.Client.(*IMClient)
	if !isOK || client == nil || client.client == nil {
		ce.Reply("Bridge client not available.")
		return nil, "", false
	}

	// Explicit handle arg takes precedence over the primary bridge handle.
	handle = client.handle
	if len(ce.Args) > 0 {
		if arg := strings.TrimSpace(ce.Args[0]); arg != "" {
			handle = arg
		}
	}

	if handle == "" {
		ce.Reply("No iMessage handle configured. Please complete bridge setup first.")
		return nil, "", false
	}

	return client, handle, true
}

// maybeNotifyIncomingFaceTimeInvite scans inbound messages for FaceTime join
// links and emits a bot notice to the corresponding Matrix chat. If the portal
// isn't available yet, a fallback notice is sent to the management room.
func (c *IMClient) maybeNotifyIncomingFaceTimeInvite(log zerolog.Logger, msg *rustpushgo.WrappedMessage, portalKey networkid.PortalKey, senderIsFromMe bool, createPortal bool) {
	if msg == nil || senderIsFromMe || msg.IsStoredMessage {
		return
	}

	link := extractFaceTimeJoinLink(msg)
	if link == "" {
		return
	}

	sender := strings.TrimSpace(ptrStringOr(msg.Sender, ""))
	if sender == "" {
		sender = "someone"
	}

	go c.sendFaceTimeInviteNotice(log, portalKey, sender, link, createPortal)
}

func (c *IMClient) sendFaceTimeInviteNotice(log zerolog.Logger, portalKey networkid.PortalKey, sender string, link string, createPortal bool) {
	ctx := context.Background()
	markdown := fmt.Sprintf("📞 **Incoming FaceTime invite** from **%s**\\n\\n[Join FaceTime](%s)", sender, link)
	content := format.RenderMarkdown(markdown, true, false)

	attempts := 1
	if createPortal {
		attempts = 4
	}

	for attempt := 0; attempt < attempts; attempt++ {
		portal, err := c.Main.Bridge.GetExistingPortalByKey(ctx, portalKey)
		if err == nil && portal != nil && portal.MXID != "" {
			_, sendErr := c.Main.Bridge.Bot.SendMessage(ctx, portal.MXID, event.EventMessage, &event.Content{Parsed: content}, nil)
			if sendErr == nil {
				log.Info().Str("portal_id", string(portalKey.ID)).Str("facetime_link", link).Msg("Sent FaceTime invite notice to portal")
				return
			}
			log.Warn().Err(sendErr).Str("portal_mxid", string(portal.MXID)).Msg("Failed to send FaceTime invite notice to portal")
			break
		}
		if attempt < attempts-1 {
			time.Sleep(1500 * time.Millisecond)
		}
	}

	mgmtRoom, err := c.UserLogin.User.GetManagementRoom(ctx)
	if err != nil {
		log.Warn().Err(err).Str("portal_id", string(portalKey.ID)).Msg("Failed to get management room for FaceTime invite notice")
		return
	}

	_, sendErr := c.Main.Bridge.Bot.SendMessage(ctx, mgmtRoom, event.EventMessage, &event.Content{Parsed: content}, nil)
	if sendErr != nil {
		log.Warn().Err(sendErr).Str("management_room", string(mgmtRoom)).Msg("Failed to send FaceTime invite notice to management room")
		return
	}
	log.Info().Str("management_room", string(mgmtRoom)).Str("facetime_link", link).Msg("Sent FaceTime invite notice to management room")
}

func extractFaceTimeJoinLink(msg *rustpushgo.WrappedMessage) string {
	if msg == nil {
		return ""
	}

	texts := []string{
		ptrStringOr(msg.Text, ""),
		ptrStringOr(msg.Html, ""),
	}
	for _, text := range texts {
		if link := firstFaceTimeLinkInText(text); link != "" {
			return link
		}
		if unescaped := html.UnescapeString(text); unescaped != text {
			if link := firstFaceTimeLinkInText(unescaped); link != "" {
				return link
			}
		}
	}

	for i := range msg.Attachments {
		att := &msg.Attachments[i]
		if att.MimeType != "x-richlink/meta" || att.InlineData == nil {
			continue
		}
		fields := bytes.SplitN(*att.InlineData, []byte{0x01}, 5)
		for _, f := range fields {
			if link := firstFaceTimeLinkInText(string(f)); link != "" {
				return link
			}
		}
	}

	return ""
}

// extractFaceTimeGuid pulls the session guid from a FACETIME_RING / FACETIME_MISSED
// marker string.  The Rust-side facetime_event_to_wrapped formats these as
// "[[FACETIME_RING]] guid=<UUID> [<link>]".
func extractFaceTimeGuid(text string) string {
	const prefix = "guid="
	idx := strings.Index(text, prefix)
	if idx < 0 {
		return ""
	}
	rest := text[idx+len(prefix):]
	if sp := strings.IndexByte(rest, ' '); sp > 0 {
		return rest[:sp]
	}
	return strings.TrimSpace(rest)
}

func firstFaceTimeLinkInText(text string) string {
	for _, candidate := range faceTimeURLRegex.FindAllString(text, -1) {
		if normalized := normalizeFaceTimeLink(candidate); normalized != "" {
			return normalized
		}
	}
	for _, candidate := range urlRegex.FindAllString(text, -1) {
		if normalized := normalizeFaceTimeLink(candidate); normalized != "" {
			return normalized
		}
	}
	return ""
}

func normalizeFaceTimeLink(candidate string) string {
	link := strings.TrimSpace(candidate)
	link = strings.TrimRight(link, ".,;:!?)]}\"'")
	if link == "" {
		return ""
	}

	lower := strings.ToLower(link)
	if strings.HasPrefix(lower, "facetime://") {
		return link
	}

	if !strings.Contains(link, "://") && strings.HasPrefix(lower, "facetime.apple.com/") {
		link = "https://" + link
	}
	if strings.HasPrefix(strings.ToLower(link), "www.facetime.apple.com/") {
		link = "https://" + link
	}

	parsed, err := url.Parse(link)
	if err != nil {
		return ""
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "facetime.apple.com" && host != "www.facetime.apple.com" {
		return ""
	}
	if parsed.Path == "" || parsed.Path == "/" {
		return ""
	}

	return link
}

func faceTimeClientOnly(ce *commands.Event) (*IMClient, bool) {
	login := ce.User.GetDefaultLogin()
	if login == nil {
		ce.Reply("No active login found.")
		return nil, false
	}
	client, ok := login.Client.(*IMClient)
	if !ok || client == nil || client.client == nil {
		ce.Reply("Bridge client not available.")
		return nil, false
	}
	return client, true
}

func parseListArgs(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	joined := strings.Join(args, " ")
	parts := strings.FieldsFunc(joined, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func fnFaceTimeState(ce *commands.Event) {
	client, ok := faceTimeClientOnly(ce)
	if !ok {
		return
	}
	var state string
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("facetime client panicked: %v", r)
			}
		}()
		ft, ftErr := client.client.GetFacetimeClient()
		if ftErr != nil {
			err = ftErr
			return
		}
		state, err = ft.ExportStateJson()
	}()
	if err != nil {
		ce.Reply("Failed to export FaceTime state: %v", err)
		return
	}
	if len(state) > 12000 {
		state = state[:12000] + "\n... (truncated)"
	}
	ce.Reply("```json\n%s\n```", state)
}

func fnFaceTimeSessionLink(ce *commands.Event) {
	if len(ce.Args) < 1 {
		ce.Reply("Usage: `!facetime-session-link <session-guid>`")
		return
	}
	client, ok := faceTimeClientOnly(ce)
	if !ok {
		return
	}
	sessionID := strings.TrimSpace(ce.Args[0])
	var link string
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("facetime client panicked: %v", r)
			}
		}()
		ft, ftErr := client.client.GetFacetimeClient()
		if ftErr != nil {
			err = ftErr
			return
		}
		link, err = ft.GetSessionLink(sessionID)
	}()
	if err != nil {
		ce.Reply("Failed to get session link: %v", err)
		return
	}
	ce.Reply("FaceTime session link: %s", link)
}

func fnFaceTimeUseLink(ce *commands.Event) {
	if len(ce.Args) < 2 {
		ce.Reply("Usage: `!facetime-use-link <old-usage> <new-usage>`")
		return
	}
	client, ok := faceTimeClientOnly(ce)
	if !ok {
		return
	}
	oldUsage := strings.TrimSpace(ce.Args[0])
	newUsage := strings.TrimSpace(ce.Args[1])
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("facetime client panicked: %v", r)
			}
		}()
		ft, ftErr := client.client.GetFacetimeClient()
		if ftErr != nil {
			err = ftErr
			return
		}
		err = ft.UseLinkFor(oldUsage, newUsage)
	}()
	if err != nil {
		ce.Reply("Failed to move link usage: %v", err)
		return
	}
	ce.Reply("Moved FaceTime link usage from `%s` to `%s`.", oldUsage, newUsage)
}

func fnFaceTimeDeleteLink(ce *commands.Event) {
	if len(ce.Args) < 1 {
		ce.Reply("Usage: `!facetime-delete-link <pseud>`")
		return
	}
	client, ok := faceTimeClientOnly(ce)
	if !ok {
		return
	}
	pseud := strings.TrimSpace(ce.Args[0])
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("facetime client panicked: %v", r)
			}
		}()
		ft, ftErr := client.client.GetFacetimeClient()
		if ftErr != nil {
			err = ftErr
			return
		}
		err = ft.DeleteLink(pseud)
	}()
	if err != nil {
		ce.Reply("Failed to delete FaceTime link: %v", err)
		return
	}
	ce.Reply("Deleted FaceTime link pseud `%s`.", pseud)
}

func fnFaceTimeLetMeIn(ce *commands.Event) {
	client, ok := faceTimeClientOnly(ce)
	if !ok {
		return
	}
	var reqs []rustpushgo.WrappedLetMeInRequest
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("facetime client panicked: %v", r)
			}
		}()
		ft, ftErr := client.client.GetFacetimeClient()
		if ftErr != nil {
			err = ftErr
			return
		}
		reqs = ft.ListDelegatedLetmeinRequests()
	}()
	if err != nil {
		ce.Reply("Failed to list Let Me In requests: %v", err)
		return
	}
	if len(reqs) == 0 {
		ce.Reply("No pending delegated Let Me In requests.")
		return
	}
	var sb strings.Builder
	sb.WriteString("**Pending Let Me In Requests**\n\n")
	for i := range reqs {
		r := reqs[i]
		nick := ptrStringOr(r.Nickname, "")
		usage := ptrStringOr(r.Usage, "")
		sb.WriteString(fmt.Sprintf("%d. requestor=`%s` delegation=`%s` pseud=`%s`", i+1, r.Requestor, r.DelegationUuid, r.Pseud))
		if nick != "" {
			sb.WriteString(fmt.Sprintf(" nickname=`%s`", nick))
		}
		if usage != "" {
			sb.WriteString(fmt.Sprintf(" usage=`%s`", usage))
		}
		sb.WriteString("\n")
	}
	ce.Reply(sb.String())
}

func fnFaceTimeLetMeInApprove(ce *commands.Event) {
	if len(ce.Args) < 1 {
		ce.Reply("Usage: `!facetime-letmein-approve <delegation-uuid> [approved-group]`")
		return
	}
	client, ok := faceTimeClientOnly(ce)
	if !ok {
		return
	}
	delegationUUID := strings.TrimSpace(ce.Args[0])
	approvedGroup := ""
	if len(ce.Args) > 1 {
		approvedGroup = strings.TrimSpace(ce.Args[1])
	}
	var approvedPtr *string
	if approvedGroup != "" {
		approvedPtr = &approvedGroup
	}
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("facetime client panicked: %v", r)
			}
		}()
		ft, ftErr := client.client.GetFacetimeClient()
		if ftErr != nil {
			err = ftErr
			return
		}
		err = ft.RespondDelegatedLetmein(delegationUUID, approvedPtr)
	}()
	if err != nil {
		ce.Reply("Failed to approve Let Me In request: %v", err)
		return
	}
	ce.Reply("Approved Let Me In request `%s`.", delegationUUID)
}

func fnFaceTimeLetMeInDeny(ce *commands.Event) {
	if len(ce.Args) < 1 {
		ce.Reply("Usage: `!facetime-letmein-deny <delegation-uuid>`")
		return
	}
	client, ok := faceTimeClientOnly(ce)
	if !ok {
		return
	}
	delegationUUID := strings.TrimSpace(ce.Args[0])
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("facetime client panicked: %v", r)
			}
		}()
		ft, ftErr := client.client.GetFacetimeClient()
		if ftErr != nil {
			err = ftErr
			return
		}
		err = ft.RespondDelegatedLetmein(delegationUUID, nil)
	}()
	if err != nil {
		ce.Reply("Failed to deny Let Me In request: %v", err)
		return
	}
	ce.Reply("Denied Let Me In request `%s`.", delegationUUID)
}

func fnFaceTimeCreateSession(ce *commands.Event) {
	if len(ce.Args) < 2 {
		ce.Reply("Usage: `!facetime-create-session <group-id> <participants...>`")
		return
	}
	client, ok := faceTimeClientOnly(ce)
	if !ok {
		return
	}
	groupID := strings.TrimSpace(ce.Args[0])
	participants := parseListArgs(ce.Args[1:])
	if len(participants) == 0 {
		ce.Reply("Please provide at least one participant handle.")
		return
	}
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("facetime client panicked: %v", r)
			}
		}()
		ft, ftErr := client.client.GetFacetimeClient()
		if ftErr != nil {
			err = ftErr
			return
		}
		err = ft.CreateSession(groupID, client.handle, participants)
	}()
	if err != nil {
		ce.Reply("Failed to create FaceTime session: %v", err)
		return
	}
	ce.Reply("Created FaceTime session for group `%s` with %d participant(s).", groupID, len(participants))
}

func fnFaceTimeRing(ce *commands.Event) {
	if len(ce.Args) < 2 {
		ce.Reply("Usage: `!facetime-ring <session-id> <targets...> [--letmein]`")
		return
	}
	client, ok := faceTimeClientOnly(ce)
	if !ok {
		return
	}
	sessionID := strings.TrimSpace(ce.Args[0])
	letmein := false
	rawTargets := make([]string, 0, len(ce.Args)-1)
	for _, arg := range ce.Args[1:] {
		if strings.EqualFold(arg, "--letmein") {
			letmein = true
			continue
		}
		rawTargets = append(rawTargets, arg)
	}
	targets := parseListArgs(rawTargets)
	if len(targets) == 0 {
		ce.Reply("Please provide at least one ring target.")
		return
	}
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("facetime client panicked: %v", r)
			}
		}()
		ft, ftErr := client.client.GetFacetimeClient()
		if ftErr != nil {
			err = ftErr
			return
		}
		err = ft.Ring(sessionID, targets, letmein)
	}()
	if err != nil {
		ce.Reply("Failed to ring FaceTime session: %v", err)
		return
	}
	ce.Reply("Rang %d target(s) in FaceTime session `%s`.", len(targets), sessionID)
}

func fnFaceTimeAddMembers(ce *commands.Event) {
	if len(ce.Args) < 2 {
		ce.Reply("Usage: `!facetime-add-members <session-id> <handles...> [--letmein]`")
		return
	}
	client, ok := faceTimeClientOnly(ce)
	if !ok {
		return
	}
	sessionID := strings.TrimSpace(ce.Args[0])
	letmein := false
	rawHandles := make([]string, 0, len(ce.Args)-1)
	for _, arg := range ce.Args[1:] {
		if strings.EqualFold(arg, "--letmein") {
			letmein = true
			continue
		}
		rawHandles = append(rawHandles, arg)
	}
	handles := parseListArgs(rawHandles)
	if len(handles) == 0 {
		ce.Reply("Please provide at least one handle to add.")
		return
	}
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("facetime client panicked: %v", r)
			}
		}()
		ft, ftErr := client.client.GetFacetimeClient()
		if ftErr != nil {
			err = ftErr
			return
		}
		err = ft.AddMembers(sessionID, handles, letmein, nil)
	}()
	if err != nil {
		ce.Reply("Failed to add FaceTime members: %v", err)
		return
	}
	ce.Reply("Added %d member(s) to FaceTime session `%s`.", len(handles), sessionID)
}

func fnFaceTimeRemoveMembers(ce *commands.Event) {
	if len(ce.Args) < 2 {
		ce.Reply("Usage: `!facetime-remove-members <session-id> <handles...>`")
		return
	}
	client, ok := faceTimeClientOnly(ce)
	if !ok {
		return
	}
	sessionID := strings.TrimSpace(ce.Args[0])
	handles := parseListArgs(ce.Args[1:])
	if len(handles) == 0 {
		ce.Reply("Please provide at least one handle to remove.")
		return
	}
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("facetime client panicked: %v", r)
			}
		}()
		ft, ftErr := client.client.GetFacetimeClient()
		if ftErr != nil {
			err = ftErr
			return
		}
		err = ft.RemoveMembers(sessionID, handles)
	}()
	if err != nil {
		ce.Reply("Failed to remove FaceTime members: %v", err)
		return
	}
	ce.Reply("Removed %d member(s) from FaceTime session `%s`.", len(handles), sessionID)
}
