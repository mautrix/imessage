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
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"

	"github.com/lrhodin/imessage/imessage"
)

// Help sections for Apple-service commands added by this bridge. Orders slot
// in after bridgev2's built-in sections (General=0, Auth=10, Chats=20,
// Admin=50), so `!im help` renders each service as its own heading at the
// bottom instead of lumping everything under "General".
var (
	HelpSectionFaceTime      = commands.HelpSection{Name: "FaceTime", Order: 60}
	HelpSectionFindMy        = commands.HelpSection{Name: "Find My", Order: 70}
	HelpSectionSharedStreams = commands.HelpSection{Name: "Shared Streams", Order: 80}
	HelpSectionStatusKit     = commands.HelpSection{Name: "StatusKit", Order: 90}
)

// BridgeCommands returns the custom slash commands for the iMessage bridge.
// Register these in main.go's PostInit hook:
//
//	m.Bridge.Commands.(*commands.Processor).AddHandlers(connector.BridgeCommands()...)
func BridgeCommands() []*commands.FullHandler {
	cmds := []*commands.FullHandler{
		cmdRestoreChat,
		cmdRestoreDebug,
		cmdMsgDebug,
		cmdContacts,
		// Apple service integrations
		cmdFaceTime,
		cmdFaceTimeSend,
		cmdFaceTimeClear,
		cmdFaceTimeInvalidatePeer,
		cmdFaceTimeRotateIdentity,
		cmdFaceTimeState,
		cmdFaceTimeSessionLink,
		cmdFaceTimeUseLink,
		cmdFaceTimeDeleteLink,
		cmdFaceTimeLetMeIn,
		cmdFaceTimeLetMeInApprove,
		cmdFaceTimeLetMeInDeny,
		cmdFaceTimeCreateSession,
		cmdFaceTimeRing,
		cmdFaceTimeAddMembers,
		cmdFaceTimeRemoveMembers,
		cmdFindMy,
		cmdFindMyAcceptShare,
		cmdFindMyDeleteItem,
		cmdFindMyRenameBeacon,
		cmdFindMyStateJSON,
		cmdFindMyDevices,
		cmdFindMyFriends,
		cmdFindMyFriendsImport,
		cmdSharedAlbums,
		cmdSharedSubscribe,
		cmdSharedSubscribeToken,
		cmdSharedUnsubscribe,
		cmdSharedState,
		cmdSharedAssetsJSON,
		cmdSharedDeleteAssets,
		cmdDeleteRoom,
		cmdStatuskitState,
		cmdStatuskitShare,
		cmdStatuskitResetKeys,
		cmdStatuskitRollKeys,
		cmdStatuskitRequestHandles,
		cmdStatuskitClearInterest,
		cmdStatuskitInviteToChannel,
		cmdStatuskitInviteAll,
	}
	return cmds
}

// cmdRestoreChat lists deleted rooms, then waits for the user to reply with
// just a number to restore that room.
//
// Usage:
//
//	!restore-chat    — show numbered list of restorable rooms
//	3                — (bare number) restore room #3 from the list
var cmdRestoreChat = &commands.FullHandler{
	Name:    "restore-chat",
	Aliases: []string{"restore"},
	Func:    fnRestoreChat,
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "List iMessage chats in the recycle bin that can be recreated. Reply with the item number to restore that room and backfill its history.",
		Args:        "",
	},
	RequiresLogin: true,
}

func fnRestoreChat(ce *commands.Event) {
	login := ce.User.GetDefaultLogin()
	if login == nil {
		ce.Reply("No active login found.")
		return
	}
	client, ok := login.Client.(*IMClient)
	if !ok || client == nil {
		ce.Reply("Bridge client not available.")
		return
	}

	// chatdb backend: use chat.db restore path.
	if client.Main.Config.UseChatDBBackfill() && client.chatDB != nil {
		fnRestoreChatFromChatDB(ce, login, client)
		return
	}

	// CloudKit backend: use delete-aware CloudKit restore.
	if client.Main.Config.UseCloudKitBackfill() && client.cloudStore != nil {
		fnRestoreChatFromCloudKit(ce, login, client)
		return
	}

	ce.Reply("No backfill source available.")
}

// fnRestoreChatFromChatDB handles restore-chat using the local macOS chat.db.
// Lists all chats in chat.db that don't have an active Matrix room.
func fnRestoreChatFromChatDB(ce *commands.Event, login *bridgev2.UserLogin, client *IMClient) {
	chats, err := client.chatDB.api.GetChatsWithMessagesAfter(time.Time{})
	if err != nil {
		ce.Reply("Failed to query chat.db: %v", err)
		return
	}

	type chatDBEntry struct {
		portalID string
		name     string
	}
	var candidates []chatDBEntry

	for _, chat := range chats {
		parsed := imessage.ParseIdentifier(chat.ChatGUID)
		if parsed.LocalID == "" {
			continue
		}

		var portalID string
		if parsed.IsGroup {
			info, err := client.chatDB.api.GetChatInfo(chat.ChatGUID, chat.ThreadID)
			if err != nil || info == nil {
				continue
			}
			members := []string{client.handle}
			for _, m := range info.Members {
				members = append(members, addIdentifierPrefix(m))
			}
			sort.Strings(members)
			portalID = strings.Join(members, ",")
		} else {
			portalID = string(identifierToPortalID(parsed))
		}

		portalKey := networkid.PortalKey{ID: networkid.PortalID(portalID), Receiver: login.ID}
		existing, _ := ce.Bridge.GetExistingPortalByKey(ce.Ctx, portalKey)
		if existing != nil && existing.MXID != "" {
			continue // room already exists
		}

		name := friendlyPortalName(ce.Ctx, ce.Bridge, client, portalKey, portalID)
		candidates = append(candidates, chatDBEntry{portalID: portalID, name: name})
	}

	if len(candidates) == 0 {
		ce.Reply("No chats found in chat.db that can be restored.")
		return
	}

	var sb strings.Builder
	sb.WriteString("**Chats available to restore from chat.db:**\n\n")
	for i, c := range candidates {
		sb.WriteString(fmt.Sprintf("%d. **%s**\n", i+1, c.name))
	}
	sb.WriteString("\nReply with a number to restore, or `$cmdprefix cancel` to cancel.")
	ce.Reply(sb.String())

	commands.StoreCommandState(ce.User, &commands.CommandState{
		Action: "restore chat",
		Next: commands.MinimalCommandHandlerFunc(func(ce *commands.Event) {
			n, err := strconv.Atoi(strings.TrimSpace(ce.RawArgs))
			if err != nil || n < 1 || n > len(candidates) {
				ce.Reply("Please reply with a number between 1 and %d, or `$cmdprefix cancel` to cancel.", len(candidates))
				return
			}

			commands.StoreCommandState(ce.User, nil)

			chosen := candidates[n-1]
			portalKey := networkid.PortalKey{ID: networkid.PortalID(chosen.portalID), Receiver: login.ID}

			// Remove from recentlyDeletedPortals so recreation isn't blocked.
			client.recentlyDeletedPortalsMu.Lock()
			delete(client.recentlyDeletedPortals, chosen.portalID)
			client.recentlyDeletedPortalsMu.Unlock()

			client.Main.Bridge.QueueRemoteEvent(login, &simplevent.ChatResync{
				EventMeta: simplevent.EventMeta{
					Type:         bridgev2.RemoteEventChatResync,
					PortalKey:    portalKey,
					CreatePortal: true,
					Timestamp:    time.Now(),
				},
				GetChatInfoFunc: client.GetChatInfo,
			})

			ce.Reply("Restoring **%s** — the room will appear shortly with history from chat.db.", chosen.name)
		}),
		Cancel: func() {},
	})
}

// restoreChatCandidate represents a chat that can be restored.
type restoreChatCandidate struct {
	portalID       string
	displayName    string
	participants   []string // normalized participants from recycle bin (may be nil)
	groupID        string   // CloudKit group UUID (for groups)
	chatID         string   // CloudKit chat_identifier
	groupPhotoGuid string   // CloudKit group photo GUID (for group avatar)
	source         string   // debug: which source produced this candidate
}

// fnRestoreChatFromCloudKit finds deleted chats from CloudKit recycle-bin
// state, then presents them for restore.
func fnRestoreChatFromCloudKit(ce *commands.Event, login *bridgev2.UserLogin, client *IMClient) {
	ce.Reply("Querying deleted chats…")

	var candidates []restoreChatCandidate
	seenPortalIDs := make(map[string]bool)

	// portalIsLive returns true if a Matrix room already exists for the portal.
	// Only checks bridge portal state (MXID), NOT cloud_chat DB — a live
	// cloud_chat row can exist from metadata refresh without the portal
	// actually being restored (e.g., user started restore then trashed it).
	portalIsLive := func(portalID string) bool {
		portalKey := networkid.PortalKey{ID: networkid.PortalID(portalID), Receiver: login.ID}
		if existing, _ := ce.Bridge.GetExistingPortalByKey(ce.Ctx, portalKey); existing != nil && existing.MXID != "" {
			return true
		}
		return false
	}

	// Source 1: Use recoverable chat identities directly from Apple's recycle bin.
	if client.client != nil && client.cloudStore != nil {
		recoverableChats, err := client.client.ListRecoverableChats()
		if err == nil {
			for _, chat := range recoverableChats {
				portalID := client.resolvePortalIDForCloudChat(chat.Participants, chat.DisplayName, chat.GroupId, chat.Style)
				if portalID == "" || seenPortalIDs[portalID] {
					continue
				}
				if portalIsLive(portalID) {
					continue
				}
				portalKey := networkid.PortalKey{ID: networkid.PortalID(portalID), Receiver: login.ID}
				// Prefer CloudKit's display_name for groups (user-set custom name).
				// friendlyPortalName falls back to member names which prevents
				// the old portalID-equality check from triggering.
				name := ""
				if chat.DisplayName != nil && *chat.DisplayName != "" {
					name = *chat.DisplayName
				}
				if name == "" {
					name = friendlyPortalName(ce.Ctx, ce.Bridge, client, portalKey, portalID)
				}
				// If the local lookup returned the "Group <uuid>…" fallback but
				// the recycle bin chat has participants, build a name from them.
				isGroup := strings.HasPrefix(portalID, "gid:") || strings.Contains(portalID, ",")
				if isGroup && strings.HasPrefix(name, "Group ") && len(chat.Participants) > 0 {
					normalized := make([]string, 0, len(chat.Participants))
					for _, p := range chat.Participants {
						n := normalizeIdentifierForPortalID(p)
						if n != "" {
							normalized = append(normalized, n)
						}
					}
					if len(normalized) > 0 {
						if built := client.buildGroupName(normalized); built != "" && built != "Group Chat" {
							name = built
						}
					}
				}
				// Normalize participants for later use during restore.
				var normParts []string
				for _, p := range chat.Participants {
					if n := normalizeIdentifierForPortalID(p); n != "" {
						normParts = append(normParts, n)
					}
				}
				photoGuid := ""
				if chat.GroupPhotoGuid != nil {
					photoGuid = *chat.GroupPhotoGuid
				}
				candidates = append(candidates, restoreChatCandidate{
					portalID:       portalID,
					displayName:    name,
					participants:   normParts,
					groupID:        chat.GroupId,
					chatID:         chat.CloudChatId,
					groupPhotoGuid: photoGuid,
					source:         "S1:recycle",
				})
				seenPortalIDs[portalID] = true
			}
		}
	}

	// Source 2: Derive deleted portals from recoverable message metadata.
	if client.client != nil && client.cloudStore != nil {
		entries, err := client.client.ListRecoverableMessageGuids()
		if err == nil && len(entries) > 0 {
			for _, hint := range client.buildRecoverableMessagePortalHints(ce.Ctx, entries) {
				if seenPortalIDs[hint.PortalID] {
					continue
				}
				if portalIsLive(hint.PortalID) {
					continue
				}
				info, err := client.cloudStore.getSoftDeletedPortalInfo(ce.Ctx, hint.PortalID)
				if err != nil {
					continue
				}
				if !info.Deleted && hint.Count < 2 {
					continue
				}
				portalKey := networkid.PortalKey{ID: networkid.PortalID(hint.PortalID), Receiver: login.ID}
				name := friendlyPortalName(ce.Ctx, ce.Bridge, client, portalKey, hint.PortalID)
				candidates = append(candidates, restoreChatCandidate{
					portalID:     hint.PortalID,
					displayName:  name,
					participants: hint.Participants,
					chatID:       hint.CloudChatID,
					source:       "S2:msg-hint",
				})
				seenPortalIDs[hint.PortalID] = true
			}

			// Source 3: Match recoverable GUIDs against cloud_message (pre-seed fallback).
			states, err := client.cloudStore.classifyRecycleBinPortals(ce.Ctx, entries)
			if err == nil {
				for _, state := range states {
					if !state.LooksDeleted() {
						continue
					}
					portalID := state.PortalID
					if seenPortalIDs[portalID] {
						continue
					}
					if portalIsLive(portalID) {
						continue
					}
					portalKey := networkid.PortalKey{ID: networkid.PortalID(portalID), Receiver: login.ID}
					name := friendlyPortalName(ce.Ctx, ce.Bridge, client, portalKey, portalID)
					candidates = append(candidates, restoreChatCandidate{
						portalID:    portalID,
						displayName: name,
						source:      "S3",
					})
					seenPortalIDs[portalID] = true
				}
			}
		}
	}

	// Source 4: Locally soft-deleted portals (from seed or APNs delete).
	if client.cloudStore != nil {
		deleted, err := client.cloudStore.listSoftDeletedPortals(ce.Ctx)
		if err == nil {
			for _, p := range deleted {
				if seenPortalIDs[p.PortalID] {
					continue
				}
				if portalIsLive(p.PortalID) {
					continue
				}
				portalKey := networkid.PortalKey{ID: networkid.PortalID(p.PortalID), Receiver: login.ID}
				name := friendlyPortalName(ce.Ctx, ce.Bridge, client, portalKey, p.PortalID)
				var parts []string
				if p.ParticipantsJSON != "" {
					_ = json.Unmarshal([]byte(p.ParticipantsJSON), &parts)
				}
				candidates = append(candidates, restoreChatCandidate{
					portalID:     p.PortalID,
					displayName:  name,
					groupID:      p.GroupID,
					chatID:       p.CloudChatID,
					participants: parts,
					source:       "S4:soft-del",
				})
				seenPortalIDs[p.PortalID] = true
			}
		}
	}

	// Source 5: In-memory recentlyDeletedPortals — catches portals deleted
	// this session that have no cloud_chat rows at all (e.g. APNs-only chats
	// that were never synced from CloudKit). Without this, deleting a chat
	// from Beeper that has no CloudKit backing makes it unrestorable.
	client.recentlyDeletedPortalsMu.RLock()
	for portalID := range client.recentlyDeletedPortals {
		if seenPortalIDs[portalID] {
			continue
		}
		if portalIsLive(portalID) {
			continue
		}
		portalKey := networkid.PortalKey{ID: networkid.PortalID(portalID), Receiver: login.ID}
		name := friendlyPortalName(ce.Ctx, ce.Bridge, client, portalKey, portalID)
		candidates = append(candidates, restoreChatCandidate{
			portalID:    portalID,
			displayName: name,
			source:      "S5:recent",
		})
		seenPortalIDs[portalID] = true
	}
	client.recentlyDeletedPortalsMu.RUnlock()

	// Sort candidates so gid: entries come before comma-based entries.
	// gid: entries carry the authoritative CloudKit metadata (custom group
	// name, groupID) while style=0 entries only have participant names.
	// By processing gid: first, the dedup keeps the better-named candidate.
	sort.SliceStable(candidates, func(i, j int) bool {
		iGid := strings.HasPrefix(candidates[i].portalID, "gid:")
		jGid := strings.HasPrefix(candidates[j].portalID, "gid:")
		if iGid != jGid {
			return iGid
		}
		return false
	})

	// Deduplicate group candidates by protocol group UUID / participants.
	// The same group can appear with different portal IDs (gid: vs
	// participant-based) from different sources. Deduping by group identity
	// keeps one row per conversation while still allowing distinct groups
	// that share a display name.
	//
	// For gid: candidates without an explicit groupID, cross-reference
	// cloud_chat to find the real group_id. This handles the case where
	// a chat_id UUID differs from the group_id UUID — without this,
	// gid:<chat_id> and gid:<group_id> look like different groups.
	{
		seenGroups := make(map[string]bool)
		// Track participant sets (full set match).
		seenParticipantSets := make(map[string]bool)
		// Track individual participants seen in gid: candidates.
		// Per-participant encryption envelopes each get a unique UUID,
		// so two gid: candidates for the same group will have different
		// UUIDs but share members. If any non-self member of a gid:
		// candidate was already seen in another gid: candidate, they're
		// the same conversation.
		seenGidMembers := make(map[string]bool)
		var deduped []restoreChatCandidate
		for _, c := range candidates {
			if isGroupPortalID(c.portalID) {
				groupID := c.groupID
				if groupID == "" && strings.HasPrefix(c.portalID, "gid:") && client.cloudStore != nil {
					groupID = client.cloudStore.getGroupIDForPortalID(ce.Ctx, c.portalID)
				}
				key := groupPortalDedupKey(c.portalID, groupID, c.participants)
				// Also check cross-reference keys: a group's chat_id UUID
				// differs from its group_id UUID, so register both so the
				// second candidate (with the other UUID) gets caught.
				altKeys := make([]string, 0, 2)
				if c.chatID != "" {
					altKeys = append(altKeys, "group:"+normalizeUUID(c.chatID))
				}
				if strings.HasPrefix(c.portalID, "gid:") {
					altKeys = append(altKeys, "group:"+normalizeUUID(strings.TrimPrefix(c.portalID, "gid:")))
				}
				isDup := seenGroups[key]
				if !isDup {
					for _, ak := range altKeys {
						if seenGroups[ak] {
							isDup = true
							break
						}
					}
				}
				// Check participant set (full match).
				if !isDup && len(c.participants) > 0 {
					pkey := strings.Join(normalizeRecoverableParticipants(c.participants), ",")
					if pkey != "" && seenParticipantSets[pkey] {
						isDup = true
					}
				}
				// Check individual members for gid: candidates.
				// Per-participant encryption envelopes produce different
				// gid: UUIDs for the same group — one UUID per member.
				// Messages from James → gid:uuid-A, from Ludvig → gid:uuid-B.
				// The participant sets are different ([James,self] vs [Ludvig,self])
				// but they share the group. Detect this by checking if ANY
				// non-self member was already seen in a prior gid: candidate.
				if !isDup && strings.HasPrefix(c.portalID, "gid:") && len(c.participants) > 0 {
					for _, p := range c.participants {
						norm := strings.ToLower(p)
						if client.isMyHandle(norm) {
							continue
						}
						if seenGidMembers[norm] {
							isDup = true
							break
						}
					}
				}
				// Last resort: display-name dedup for gid: candidates that
				// have no participants (e.g. Source 4/5 which only know the
				// portal_id). Without this, per-participant encryption UUIDs
				// that appear in Source 4 produce duplicate entries since all
				// other dedup checks require participants.
				if !isDup && strings.HasPrefix(c.portalID, "gid:") && len(c.participants) == 0 {
					nameKey := "gid-name:" + strings.ToLower(c.displayName)
					if seenGroups[nameKey] {
						isDup = true
					}
				}
				if isDup {
					continue
				}
				seenGroups[key] = true
				for _, ak := range altKeys {
					seenGroups[ak] = true
				}
				// Register display-name key for gid: candidates.
				if strings.HasPrefix(c.portalID, "gid:") {
					nameKey := "gid-name:" + strings.ToLower(c.displayName)
					seenGroups[nameKey] = true
				}

				// Register participant set for ALL group candidates.
				if len(c.participants) > 0 {
					pkey := strings.Join(normalizeRecoverableParticipants(c.participants), ",")
					if pkey != "" {
						seenParticipantSets[pkey] = true
					}
				}
				// Register individual members for gid: overlap detection.
				if strings.HasPrefix(c.portalID, "gid:") {
					for _, p := range c.participants {
						norm := strings.ToLower(p)
						if !client.isMyHandle(norm) {
							seenGidMembers[norm] = true
						}
					}
				}
			}
			deduped = append(deduped, c)
		}
		candidates = deduped
	}

	if len(candidates) == 0 {
		client.cloudSyncRunningLock.RLock()
		syncing := client.cloudSyncRunning
		client.cloudSyncRunningLock.RUnlock()
		if syncing || !client.isCloudSyncDone() {
			ce.Reply("iMessage history is still syncing from iCloud — please try again once the sync is complete.")
		} else {
			ce.Reply("No deleted chats found.")
		}
		return
	}

	var sb strings.Builder
	sb.WriteString("**Deleted chats available to restore:**\n\n")
	for i, c := range candidates {
		sb.WriteString(fmt.Sprintf("%d. **%s**\n", i+1, c.displayName))
	}
	sb.WriteString("\nReply with a number to restore, or `$cmdprefix cancel` to cancel.")
	ce.Reply(sb.String())

	commands.StoreCommandState(ce.User, &commands.CommandState{
		Action: "restore chat",
		Next: commands.MinimalCommandHandlerFunc(func(ce *commands.Event) {
			n, err := strconv.Atoi(strings.TrimSpace(ce.RawArgs))
			if err != nil || n < 1 || n > len(candidates) {
				ce.Reply("Please reply with a number between 1 and %d, or `$cmdprefix cancel` to cancel.", len(candidates))
				return
			}

			commands.StoreCommandState(ce.User, nil)

			chosen := candidates[n-1]
			portalKey := networkid.PortalKey{ID: networkid.PortalID(chosen.portalID), Receiver: login.ID}
			if err := client.startRestoreBackfillPipeline(restorePipelineOptions{
				PortalID:       chosen.portalID,
				PortalKey:      portalKey,
				Source:         "restore_chat_cmd",
				DisplayName:    chosen.displayName,
				Participants:   chosen.participants,
				ChatID:         chosen.chatID,
				GroupID:        chosen.groupID,
				GroupPhotoGuid: chosen.groupPhotoGuid,
				RecoverOnApple: true,
				Notify: func(format string, args ...any) {
					ce.Reply(format, args...)
				},
			}); err != nil {
				ce.Reply("Failed to start restore for **%s**: %v", chosen.displayName, err)
			}
		}),
		Cancel: func() {},
	})
}

// friendlyPortalName returns a human-readable name for a portal.
// Tries the bridgev2 portal DB first, then the IMClient's resolveGroupName
// (which checks cloud_chat for display_name and participant contacts),
// then falls back to formatting the portal_id.
func friendlyPortalName(ctx context.Context, bridge *bridgev2.Bridge, client *IMClient, key networkid.PortalKey, portalID string) string {
	if portal, _ := bridge.GetExistingPortalByKey(ctx, key); portal != nil && portal.Name != "" {
		return portal.Name
	}
	// For group chats, resolve from cloud store (display_name / contact names).
	isGroup := strings.HasPrefix(portalID, "gid:") || strings.Contains(portalID, ",")
	if isGroup && client != nil {
		if name, _ := client.resolveGroupName(ctx, portalID); name != "" && name != "Group Chat" {
			return name
		}
	}
	// For DM portals, try to resolve a contact name.
	if client != nil && !isGroup {
		contact := client.lookupContact(portalID)
		if contact != nil && contact.HasName() {
			name := client.Main.Config.FormatDisplayname(DisplaynameParams{
				FirstName: contact.FirstName,
				LastName:  contact.LastName,
				Nickname:  contact.Nickname,
				ID:        stripIdentifierPrefix(portalID),
			})
			if name != "" {
				return name
			}
		}
	}
	// Strip URI prefix for a cleaner display.
	id := strings.TrimPrefix(strings.TrimPrefix(portalID, "mailto:"), "tel:")
	if strings.HasPrefix(portalID, "gid:") {
		trimmed := strings.TrimPrefix(portalID, "gid:")
		if len(trimmed) > 8 {
			trimmed = trimmed[:8]
		}
		return "Group " + trimmed + "…"
	}
	return id
}

func pluralMessages(n int) string {
	if n == 1 {
		return "1 message"
	}
	return fmt.Sprintf("%d messages", n)
}

// restorePortalByID is the programmatic equivalent of the restore-chat command.
func (c *IMClient) restorePortalByID(_ context.Context, portalID string) error {
	portalKey := networkid.PortalKey{
		ID:       networkid.PortalID(portalID),
		Receiver: c.UserLogin.ID,
	}

	if c.Main.Config.UseCloudKitBackfill() && c.cloudStore != nil {
		return c.startRestoreBackfillPipeline(restorePipelineOptions{
			PortalID:       portalID,
			PortalKey:      portalKey,
			Source:         "restore_portal_by_id",
			RecoverOnApple: true,
		})
	} else {
		// chatdb backend — use existing local data.
		c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.ChatResync{
			EventMeta: simplevent.EventMeta{
				Type:         bridgev2.RemoteEventChatResync,
				PortalKey:    portalKey,
				CreatePortal: true,
				Timestamp:    time.Now(),
			},
			GetChatInfoFunc: c.GetChatInfo,
		})
	}

	return nil
}

// cmdRestoreDebug dumps recycle bin + cloud_chat state for diagnosing restore issues.
//
// Usage: !restore-debug
var cmdRestoreDebug = &commands.FullHandler{
	Name:    "restore-debug",
	Aliases: []string{"rdebug", "chat-debug"},
	Func:    fnRestoreDebug,
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "Dump cloud_chat + recycle-bin state for all portals to diagnose missing or failed restores.",
		Args:        "",
	},
	RequiresLogin: true,
}

func fnRestoreDebug(ce *commands.Event) {
	login := ce.User.GetDefaultLogin()
	if login == nil {
		ce.Reply("Not logged in.")
		return
	}
	client, ok := login.Client.(*IMClient)
	if !ok || client == nil {
		ce.Reply("Bridge client not available.")
		return
	}

	if !client.Main.Config.UseCloudKitBackfill() || client.cloudStore == nil {
		ce.Reply("CloudKit backfill is not enabled.")
		return
	}

	var sb strings.Builder
	sb.WriteString("**Restore debug dump**\n\n")

	// ── 1. Recycle bin (live CloudKit query) ──────────────────────────────────
	sb.WriteString("**Recycle bin (ListRecoverableChats)**\n")
	recycleBin, err := client.client.ListRecoverableChats()
	if err != nil {
		sb.WriteString(fmt.Sprintf("  error: %v\n", err))
	} else if len(recycleBin) == 0 {
		sb.WriteString("  (empty — recycle bin is clear)\n")
	} else {
		for i, chat := range recycleBin {
			name := "(no name)"
			if chat.DisplayName != nil && *chat.DisplayName != "" {
				name = *chat.DisplayName
			}
			photo := ""
			if chat.GroupPhotoGuid != nil && *chat.GroupPhotoGuid != "" {
				g := *chat.GroupPhotoGuid
				if len(g) > 8 {
					g = g[:8]
				}
				photo = " photo=" + g + "…"
			}

			portalID := client.resolvePortalIDForCloudChat(chat.Participants, chat.DisplayName, chat.GroupId, chat.Style)
			cacheBackfillable := "?"
			cacheContentful := "?"
			if portalID != "" {
				if n, cntErr := client.cloudStore.countBackfillableMessages(ce.Ctx, portalID, false); cntErr == nil {
					cacheBackfillable = strconv.Itoa(n)
				}
				if n, cntErr := client.cloudStore.countBackfillableMessages(ce.Ctx, portalID, true); cntErr == nil {
					cacheContentful = strconv.Itoa(n)
				}
			}

			sb.WriteString(fmt.Sprintf("  %d. [style=%d del=%v] %q pid=%s cid=%s gid=%s parts=%d cache_msgs=%s contentful=%s%s\n",
				i+1, chat.Style, chat.Deleted,
				name, portalID, chat.CloudChatId, chat.GroupId,
				len(chat.Participants), cacheBackfillable, cacheContentful, photo))
		}
	}

	// ── 2. Soft-deleted portals in cloud_chat ─────────────────────────────────
	sb.WriteString("\n**Soft-deleted portals (cloud_chat)**\n")
	softDel, err := client.cloudStore.listSoftDeletedPortals(ce.Ctx)
	if err != nil {
		sb.WriteString(fmt.Sprintf("  error: %v\n", err))
	} else if len(softDel) == 0 {
		sb.WriteString("  (none)\n")
	} else {
		for _, p := range softDel {
			name, _ := client.cloudStore.getDisplayNameByPortalID(ce.Ctx, p.PortalID)
			cid := p.CloudChatID
			if cid == "" {
				cid = "(none)"
			}
			gid := p.GroupID
			if gid == "" {
				gid = "(none)"
			}
			sb.WriteString(fmt.Sprintf("  %s  name=%q  msgs=%d  cid=%s  gid=%s\n",
				p.PortalID, name, p.Count, cid, gid))
		}
	}

	// ── 3. Restore overrides ──────────────────────────────────────────────────
	sb.WriteString("\n**Restore overrides**\n")
	overrides := client.cloudStore.listRestoreOverrides(ce.Ctx)
	if len(overrides) == 0 {
		sb.WriteString("  (none)\n")
	} else {
		for _, pid := range overrides {
			sb.WriteString(fmt.Sprintf("  %s\n", pid))
		}
	}

	ce.Reply(sb.String())
}

// cmdMsgDebug inspects cloud_message for a given identifier and reports where
// messages ended up, breaking out from-me vs not-from-me counts, and showing
// sibling group portals. This helps diagnose two common bugs:
//
//   - DM has 0 messages in cloud_message (APNs-only or wrong portal_id)
//   - Group chat shows only one side (per-participant UUID routing split)
//
// Usage:
//
//	!msg-debug +19176138320        — phone number (normalized automatically)
//	!msg-debug user@example.com    — email handle
//	!msg-debug gid:abc123-uuid     — explicit group portal ID
var cmdMsgDebug = &commands.FullHandler{
	Name:          "msg-debug",
	Aliases:       []string{"msgdbg"},
	Func:          fnMsgDebug,
	RequiresLogin: true,
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "Check cloud_message sync status and IDS registration for a contact. Shows sync progress, per-participant UUID routing splits, and which handles Apple's IDS confirms as iMessage. Pass an identifier to avoid accidentally opening a chat room.",
		Args:        "[phone|email|gid:uuid]",
	},
}

func fnMsgDebug(ce *commands.Event) {
	login := ce.User.GetDefaultLogin()
	if login == nil {
		ce.Reply("Not logged in.")
		return
	}
	client, ok := login.Client.(*IMClient)
	if !ok || client == nil {
		ce.Reply("Bridge client not available.")
		return
	}
	if !client.Main.Config.UseCloudKitBackfill() || client.cloudStore == nil {
		ce.Reply("CloudKit backfill not enabled.")
		return
	}

	identifier := strings.TrimSpace(ce.RawArgs)

	// Resolve to a canonical portal_id.
	var portalID string
	var inputDesc string

	if identifier == "" {
		// No args: use current room's portal (lets user run from inside a chat room).
		if ce.Portal == nil {
			ce.Reply("Usage: `!msg-debug <phone|email|gid:uuid>`\n\nOr run from inside a bridged room with no arguments.\n\nExamples:\n  `!msg-debug +19176138320`\n  `!msg-debug user@example.com`\n  `!msg-debug gid:abc123`")
			return
		}
		portalID = string(ce.Portal.ID)
		inputDesc = "(current room)"
	} else if strings.HasPrefix(identifier, "gid:") {
		portalID = identifier
		inputDesc = identifier
	} else {
		normalized := normalizeIdentifierForPortalID(identifier)
		if normalized == "" {
			ce.Reply("Could not normalise %q as a phone number or email.", identifier)
			return
		}
		resolved := client.resolveContactPortalID(normalized)
		resolved = client.resolveExistingDMPortalID(string(resolved))
		portalID = string(resolved)
		inputDesc = identifier
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("**msg-debug: `%s`**\n", portalID))
	sb.WriteString(fmt.Sprintf("_input: `%s`_\n\n", inputDesc))

	// ── 0. cloud_chat status (did the chat metadata even sync?) ──────────────
	chatInfo, chatErr := client.cloudStore.debugChatInfo(ce.Ctx, portalID)
	if chatErr != nil {
		sb.WriteString(fmt.Sprintf("cloud_chat lookup error: %v\n\n", chatErr))
	} else if !chatInfo.Found {
		sb.WriteString("⚠️ **cloud_chat: NOT FOUND** — chat metadata hasn't synced yet (or this identifier never appeared in CloudKit)\n\n")
	} else {
		chatStatus := "✓ live"
		if chatInfo.Deleted {
			chatStatus = "soft-deleted"
		}
		filteredNote := ""
		if chatInfo.IsFiltered != 0 {
			filteredNote = fmt.Sprintf(" ⚠️ IS_FILTERED=%d (excluded from portal creation!)", chatInfo.IsFiltered)
		}
		sb.WriteString(fmt.Sprintf("cloud_chat: %s  cid=%s  gid=%s%s\n\n", chatStatus, chatInfo.CloudChatID, chatInfo.GroupID, filteredNote))
	}

	// ── 1. IDS lookup (is this handle registered on iMessage?) ───────────────
	if !strings.HasPrefix(portalID, "gid:") && client.client != nil {
		idsTargets := []string{portalID}
		contact := client.lookupContact(portalID)
		for _, altID := range contactPortalIDs(contact) {
			if altID != portalID {
				idsTargets = append(idsTargets, altID)
			}
		}
		valid := client.client.ValidateTargets(idsTargets, client.handle)
		validSet := make(map[string]bool, len(valid))
		for _, v := range valid {
			validSet[v] = true
		}
		sb.WriteString("**IDS lookup:**\n")
		for _, t := range idsTargets {
			status := "❌ not on iMessage"
			if validSet[t] {
				status = "✅ iMessage"
			}
			note := ""
			if t != portalID {
				note = " (contact alias)"
			}
			sb.WriteString(fmt.Sprintf("  %s — %s%s\n", t, status, note))
		}
		if len(valid) == 0 {
			sb.WriteString("  ⚠️ No handles validated — number may not be on iMessage, or IDS is unavailable\n")
		} else if !validSet[portalID] {
			// Primary ID not valid, but an alias is — this means cloud_message stored under alias!
			sb.WriteString(fmt.Sprintf("  ⚠️ Primary portal ID is NOT on iMessage, but alias(es) above are — messages may be stored under a different portal ID!\n"))
		}
		sb.WriteString("\n")
	}

	// ── 2. Per-portal message stats (primary + group siblings) ────────────────
	stats, err := client.cloudStore.debugMessageStats(ce.Ctx, portalID)
	if err != nil {
		sb.WriteString(fmt.Sprintf("Error querying stats: %v\n", err))
	} else if len(stats) == 0 {
		// Show total sync progress so user knows if sync is still running.
		totalMsgs, _ := client.cloudStore.debugTotalMessageCount(ce.Ctx)
		syncDone := client.isCloudSyncDone()
		client.cloudSyncRunningLock.RLock()
		syncRunning := client.cloudSyncRunning
		client.cloudSyncRunningLock.RUnlock()
		syncStatus := "✅ complete"
		if syncRunning {
			syncStatus = "⏳ running now"
		} else if !syncDone {
			syncStatus = "⚠️ not started / interrupted"
		}
		sb.WriteString(fmt.Sprintf("No cloud_message rows found for this portal (or its group siblings).\nSync: %s — %d messages ingested across all portals\n", syncStatus, totalMsgs))
	} else {
		sb.WriteString("**cloud_message rows by portal:**\n")
		for _, s := range stats {
			marker := ""
			if s.PortalID == portalID {
				marker = " ← target"
			}
			chatSample := ""
			if len(s.SampleChats) > 0 {
				chatSample = "\n    chat_ids=" + strings.Join(s.SampleChats, ", ")
			}
			senderSample := ""
			if len(s.SampleSenders) > 0 {
				senderSample = "\n    senders=" + strings.Join(s.SampleSenders, ", ")
			}
			emptySenderNote := ""
			if s.EmptySender > 0 {
				emptySenderNote = fmt.Sprintf(" ⚠️ empty_sender=%d (will be filtered from backfill!)", s.EmptySender)
			}
			sb.WriteString(fmt.Sprintf("  %s%s\n    total=%d from_me=%d not_from_me=%d%s%s%s\n",
				s.PortalID, marker, s.Total, s.FromMe, s.NotFromMe, emptySenderNote, chatSample, senderSample))
		}
	}

	// ── 3. For DMs: search by identifier suffix (catch normalization splits) ──
	if !strings.HasPrefix(portalID, "gid:") {
		// Strip tel: / mailto: prefix to get the raw identifier.
		suffix := strings.TrimPrefix(strings.TrimPrefix(portalID, "tel:"), "mailto:")
		if suffix != "" && suffix != portalID {
			matches, suffixErr := client.cloudStore.debugFindPortalsByIdentifierSuffix(ce.Ctx, suffix)
			if suffixErr == nil && len(matches) > 0 {
				anyOther := false
				for _, m := range matches {
					if m[0] != portalID {
						anyOther = true
						break
					}
				}
				if anyOther {
					sb.WriteString(fmt.Sprintf("\n**Portals with chat_id containing `%s`:**\n", suffix))
					for _, m := range matches {
						marker := ""
						if m[0] == portalID {
							marker = " ← target"
						}
						sb.WriteString(fmt.Sprintf("  %s  count=%s%s\n", m[0], m[1], marker))
					}
				}
			}
		}
	}

	ce.Reply(sb.String())
}
