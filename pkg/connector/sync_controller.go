package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"

	"github.com/lrhodin/imessage/pkg/rustpushgo"
)

// No periodic polling needed: real-time messages arrive via APNs push
// on com.apple.madrid. CloudKit sync is only used for initial backfill
// of historical messages (bootstrap).

// cloudSyncVersion is bumped when sync logic changes in a way that
// requires a one-time full re-download from CloudKit (e.g. improved PCS
// key handling that can now decrypt previously-skipped records).
// Increment this to trigger a token clear on the next bootstrap.
const cloudSyncVersion = 4

// maxCloudSyncPages is the upper bound on CloudKit pagination loops.
// Each CloudKit zone sync (messages, chats, attachments) breaks after this many
// pages so a runaway response can never loop forever.
const maxCloudSyncPages = 10000

// cloudKitRetryAfterRegex extracts the server-provided retry hint that
// CloudKit includes on transient errors (ZoneBusy, "Blobification error",
// etc). Upstream surfaces the hint via PushError's {:?} Debug formatter
// embedded in the FFI error string; no structured field reaches Go.
var cloudKitRetryAfterRegex = regexp.MustCompile(`retry_after_seconds:\s*Some\((\d+)\)`)

// cloudKitRetryDelay returns the delay to wait before retrying a failed
// CloudKit sync page. Uses the larger of: (a) Apple's explicit
// retry_after_seconds hint parsed from the error string, (b) an
// exponential backoff ramp keyed on consecutive errors (500ms, 1s, 2s,
// 4s, 8s). Clamped to 30s so a malformed hint or long error streak can't
// stall a pagination pass indefinitely — the outer sync controller will
// retry the whole zone after cloudSyncRetryInterval anyway.
func cloudKitRetryDelay(err error, consecutiveErrors int) time.Duration {
	// Exponential fallback: 500ms * 2^(n-1), capped at 30s.
	// n=1 → 500ms, n=2 → 1s, n=3 → 2s, n=4 → 4s, n=5 → 8s.
	fallback := 500 * time.Millisecond
	if consecutiveErrors > 1 {
		shift := consecutiveErrors - 1
		if shift > 6 {
			shift = 6 // cap the multiplier so we don't overflow the Duration math
		}
		fallback = fallback << shift
	}
	if fallback > 30*time.Second {
		fallback = 30 * time.Second
	}

	delay := fallback
	if err != nil {
		if m := cloudKitRetryAfterRegex.FindStringSubmatch(err.Error()); len(m) >= 2 {
			if secs, parseErr := strconv.Atoi(m[1]); parseErr == nil && secs > 0 {
				hint := time.Duration(secs) * time.Second
				if hint > 30*time.Second {
					hint = 30 * time.Second
				}
				if hint > delay {
					delay = hint
				}
			}
		}
	}
	return delay
}

// cloudChatSyncVersion is bumped when chat-specific sync logic changes in a
// way that requires a one-time full re-download of the chatManateeZone only
// (not messages). This avoids the expensive message re-download that
// bumping cloudSyncVersion would cause.
// Current bump: 2 — populate group_photo_guid for all group chats.
const cloudChatSyncVersion = 2

type cloudSyncCounters struct {
	Imported int
	Updated  int
	Skipped  int
	Deleted  int
	Filtered int
}

func (c *cloudSyncCounters) add(other cloudSyncCounters) {
	c.Imported += other.Imported
	c.Updated += other.Updated
	c.Skipped += other.Skipped
	c.Deleted += other.Deleted
	c.Filtered += other.Filtered
}

func (c *IMClient) setCloudSyncDone() {
	c.cloudSyncDoneLock.Lock()
	c.cloudSyncDone = true
	c.cloudSyncDoneLock.Unlock()

	// Flush the APNs reorder buffer once all forward backfills are complete.
	// Messages accumulated during CloudKit sync to avoid interleaving APNs
	// messages before older CloudKit messages in Matrix.
	//
	// If there are pending initial backfills (portals queued by the bootstrap
	// createPortalsFromCloudSync pass), we wait for each FetchMessages call
	// to complete and deliver its batch to Matrix (via CompleteCallback) before
	// flushing. This ensures APNs messages appear AFTER the CloudKit history,
	// not interleaved with it.
	//
	// If no portals were queued (fresh sync with no history, or all portals
	// already up-to-date), flush immediately.
	pending := atomic.LoadInt64(&c.pendingInitialBackfills)
	if pending <= 0 {
		log.Info().Msg("No pending initial backfills — flushing APNs buffer immediately")
		atomic.StoreInt64(&c.apnsBufferFlushedAt, time.Now().UnixMilli())
		if c.msgBuffer != nil {
			c.msgBuffer.flush()
		}
		c.flushPendingPortalMsgs()
		return
	}

	log.Info().Int64("pending", pending).
		Msg("Waiting for initial forward backfills before flushing APNs buffer")

	// Safety-net goroutine: if some FetchMessages calls never complete (e.g.
	// portal deleted before bridgev2 processes the ChatResync event, or a
	// crash/panic inside FetchMessages), force-flush the buffer so APNs
	// messages are never permanently suppressed.
	//
	// Uses an activity-based deadline rather than a fixed one: the timer
	// resets every time any forward backfill completes. This lets slow
	// hardware (or large accounts) take as long as needed, while still
	// eventually force-flushing if nothing makes progress for 5 minutes
	// (i.e. a portal is genuinely stuck and will never finish).
	go func() {
		const noProgressTimeout = 5 * time.Minute
		lastCount := atomic.LoadInt64(&c.pendingInitialBackfills)
		deadline := time.Now().Add(noProgressTimeout)
		for time.Now().Before(deadline) {
			if atomic.LoadInt64(&c.pendingInitialBackfills) <= 0 {
				// onForwardBackfillDone already flushed the buffer.
				return
			}
			time.Sleep(250 * time.Millisecond)
			if current := atomic.LoadInt64(&c.pendingInitialBackfills); current < lastCount {
				// Progress was made — reset the no-progress deadline.
				lastCount = current
				deadline = time.Now().Add(noProgressTimeout)
			}
		}
		remaining := atomic.LoadInt64(&c.pendingInitialBackfills)
		log.Warn().Int64("remaining", remaining).
			Msg("APNs buffer flush timeout: no forward backfill progress in 5 minutes, forcing flush")
		// Mirror what onForwardBackfillDone does: stamp the flush time so the
		// read-receipt grace window (handleReadReceipt) knows the burst is done.
		atomic.StoreInt64(&c.apnsBufferFlushedAt, time.Now().UnixMilli())
		if c.msgBuffer != nil {
			c.msgBuffer.flush()
		}
		c.flushPendingPortalMsgs()
	}()
}

func (c *IMClient) isCloudSyncDone() bool {
	c.cloudSyncDoneLock.RLock()
	defer c.cloudSyncDoneLock.RUnlock()
	return c.cloudSyncDone
}

// recentlyDeletedPortalsTTL is how long entries stay in recentlyDeletedPortals
// before being pruned. 24 hours is generous — tombstones are re-delivered on
// every CloudKit sync so short-lived entries are repopulated anyway.
const recentlyDeletedPortalsTTL = 24 * time.Hour

// trackDeletedChat adds a portal ID to the recently-deleted set so stale APNs
// echoes and CloudKit sync don't recreate the portal.
func (c *IMClient) trackDeletedChat(portalID string) {
	c.recentlyDeletedPortalsMu.Lock()
	if c.recentlyDeletedPortals == nil {
		c.recentlyDeletedPortals = make(map[string]deletedPortalEntry)
	}
	entry := deletedPortalEntry{deletedAt: time.Now()}
	c.recentlyDeletedPortals[portalID] = entry
	// For gid: portals, also track all other portal IDs that share the same
	// group UUID. CloudKit chat_id UUIDs can differ from group_id UUIDs,
	// creating multiple portal_id variants for the same group. Without
	// tracking all variants, createPortalsFromCloudSync recreates the
	// portal under the alternate UUID on restart.
	if strings.HasPrefix(portalID, "gid:") && c.cloudStore != nil {
		uuid := strings.TrimPrefix(portalID, "gid:")
		ctx := context.Background()
		if aliases, err := c.cloudStore.findPortalIDsByGroupID(ctx, uuid); err == nil {
			for _, alias := range aliases {
				if alias != portalID {
					c.recentlyDeletedPortals[alias] = entry
				}
			}
		}
	}
	c.recentlyDeletedPortalsMu.Unlock()
}

// pruneRecentlyDeletedPortals removes entries older than the TTL.
// Called after bootstrap sync to prevent unbounded memory growth.
func (c *IMClient) pruneRecentlyDeletedPortals(log zerolog.Logger) {
	c.recentlyDeletedPortalsMu.Lock()
	defer c.recentlyDeletedPortalsMu.Unlock()
	if len(c.recentlyDeletedPortals) == 0 {
		return
	}
	cutoff := time.Now().Add(-recentlyDeletedPortalsTTL)
	pruned := 0
	for portalID, entry := range c.recentlyDeletedPortals {
		if entry.deletedAt.Before(cutoff) {
			delete(c.recentlyDeletedPortals, portalID)
			pruned++
		}
	}
	if pruned > 0 {
		log.Info().Int("pruned", pruned).Int("remaining", len(c.recentlyDeletedPortals)).
			Msg("Pruned stale entries from recentlyDeletedPortals")
	}
}

// seedDeletedChatsFromRecycleBin reads recoverable chat/message data from
// Apple's recycle-bin zones, classifies which portals still look deleted, and
// also auto-recovers portals that were previously soft-deleted locally but
// whose current CloudKit state now looks active again.
func (c *IMClient) seedDeletedChatsFromRecycleBin(log zerolog.Logger) {
	if c.client == nil || c.cloudStore == nil {
		return
	}
	// ListRecoverableChats / ListRecoverableMessageGuids cross into the
	// CloudKit FFI path, which has reachable panic sites upstream
	// (cloudkit.rs type assertions). A leaf-level recover here lets the
	// sync controller goroutine continue to setCloudSyncDone() even if one
	// seed pass panics — unblocking the APNs buffer is more important than
	// crashing the whole process over a seed-pass failure.
	defer func() {
		if r := recover(); r != nil {
			log.Error().Interface("panic", r).Msg("seedDeletedChatsFromRecycleBin panicked — skipped this pass")
		}
	}()

	ctx := context.Background()
	candidateMap := make(map[string]recycleBinCandidate)
	seeded := 0
	isRestoreOverridden := func(portalID string) bool {
		overridden, err := c.cloudStore.hasRestoreOverride(ctx, portalID)
		if err != nil {
			log.Warn().Err(err).Str("portal_id", portalID).
				Msg("DELETE-SEED: failed to check restore override")
			return false
		}
		return overridden
	}

	log.Info().Msg("DELETE-SEED: reading recoverable chats from Apple's recycle bin")
	recoverableChats, chatErr := c.client.ListRecoverableChats()
	if chatErr != nil {
		log.Warn().Err(chatErr).Msg("DELETE-SEED: failed to read recoverable chats")
	} else {
		log.Info().Int("recoverable_chats", len(recoverableChats)).
			Msg("DELETE-SEED: read recoverable chats, matching against local portal IDs")
		for _, chat := range recoverableChats {
			portalID := c.resolvePortalIDForCloudChat(chat.Participants, chat.DisplayName, chat.GroupId, chat.Style)
			if portalID == "" {
				log.Debug().
					Str("record_name", chat.RecordName).
					Str("cloud_chat_id", chat.CloudChatId).
					Str("group_id", chat.GroupId).
					Strs("participants", chat.Participants).
					Msg("DELETE-SEED: skipping recoverable chat with unresolved portal ID")
				continue
			}
			if isRestoreOverridden(portalID) {
				if undeleted, err := c.cloudStore.undeleteCloudMessagesByPortalID(ctx, portalID); err != nil {
					log.Warn().Err(err).Str("portal_id", portalID).
						Msg("DELETE-SEED: failed to honor restore override for recoverable chat")
				} else {
					c.recentlyDeletedPortalsMu.Lock()
					delete(c.recentlyDeletedPortals, portalID)
					c.recentlyDeletedPortalsMu.Unlock()
					log.Info().
						Str("portal_id", portalID).
						Int("undeleted_messages", undeleted).
						Msg("DELETE-SEED: skipping recoverable chat tombstone because user restored it manually")
				}
				continue
			}

			if hasChat, err := c.cloudStore.portalHasChat(ctx, portalID); err == nil && hasChat {
				c.recentlyDeletedPortalsMu.Lock()
				delete(c.recentlyDeletedPortals, portalID)
				c.recentlyDeletedPortalsMu.Unlock()
				log.Info().
					Str("portal_id", portalID).
					Str("record_name", chat.RecordName).
					Msg("DELETE-SEED: recoverable chat already has live CloudKit row, skipping tombstone seed")
				continue
			} else if err != nil {
				log.Warn().Err(err).Str("portal_id", portalID).
					Msg("DELETE-SEED: failed to check live chat state for recoverable chat")
			}

			// If this portal already has messages in the main CloudKit zone (stored
			// during Phase 2 message sync), it is a recycle-bin-only chat whose
			// messages appeared in messageManateeZone — not a true deletion. Seed a
			// live cloud_chat row so createPortalsFromCloudSync picks it up, and skip
			// the tombstone entirely. This fixes fresh-backfill for chats that were
			// deleted before the bridge's first ever sync (they appear in the recycle
			// bin zone but their messages are still in the main zone).
			if hasMessages, msgErr := c.cloudStore.hasPortalMessages(ctx, portalID); msgErr == nil && hasMessages {
				dn := ""
				if chat.DisplayName != nil {
					dn = *chat.DisplayName
				}
				c.cloudStore.seedChatFromRecycleBin(ctx, portalID, chat.CloudChatId, chat.GroupId, dn, "", chat.Participants)
				log.Info().
					Str("portal_id", portalID).
					Str("record_name", chat.RecordName).
					Msg("DELETE-SEED: recycle-bin chat has main-zone messages, seeding live chat row instead of tombstone")
				continue
			} else if msgErr != nil {
				log.Warn().Err(msgErr).Str("portal_id", portalID).
					Msg("DELETE-SEED: failed to check portal messages for recoverable chat")
			}

			participantsJSON, err := json.Marshal(chat.Participants)
			if err != nil {
				log.Warn().Err(err).Str("portal_id", portalID).
					Msg("DELETE-SEED: failed to serialize recoverable chat participants")
				continue
			}
			if err := c.cloudStore.insertDeletedChatTombstone(
				ctx,
				chat.CloudChatId,
				portalID,
				chat.RecordName,
				chat.GroupId,
				chat.Service,
				chat.DisplayName,
				string(participantsJSON),
			); err != nil {
				log.Warn().Err(err).Str("portal_id", portalID).
					Msg("DELETE-SEED: failed to persist recoverable chat tombstone")
				continue
			}
			if err := c.cloudStore.deleteLocalChatByPortalID(ctx, portalID); err != nil {
				log.Warn().Err(err).Str("portal_id", portalID).
					Msg("DELETE-SEED: failed to soft-delete local data for recoverable chat")
			}

			c.recentlyDeletedPortalsMu.Lock()
			if c.recentlyDeletedPortals == nil {
				c.recentlyDeletedPortals = make(map[string]deletedPortalEntry)
			}
			if _, exists := c.recentlyDeletedPortals[portalID]; !exists {
				c.recentlyDeletedPortals[portalID] = deletedPortalEntry{
					deletedAt:   time.Now(),
					isTombstone: true,
				}
				seeded++
			}
			c.recentlyDeletedPortalsMu.Unlock()

			portalKey := networkid.PortalKey{ID: networkid.PortalID(portalID), Receiver: c.UserLogin.ID}
			name := friendlyPortalName(ctx, c.Main.Bridge, c, portalKey, portalID)
			isGroup := strings.HasPrefix(portalID, "gid:") || strings.Contains(portalID, ",")
			if chat.DisplayName != nil && *chat.DisplayName != "" && (name == portalID || name == strings.TrimPrefix(strings.TrimPrefix(portalID, "mailto:"), "tel:") || (isGroup && strings.HasPrefix(name, "Group "))) {
				name = *chat.DisplayName
			}
			if isGroup && strings.HasPrefix(name, "Group ") && len(chat.Participants) > 0 {
				normalized := make([]string, 0, len(chat.Participants))
				for _, p := range chat.Participants {
					if n := normalizeIdentifierForPortalID(p); n != "" {
						normalized = append(normalized, n)
					}
				}
				if len(normalized) > 0 {
					if built := c.buildGroupName(normalized); built != "" && built != "Group Chat" {
						name = built
					}
				}
			}
			candidateKey := groupPortalDedupKey(portalID, chat.GroupId, chat.Participants)
			candidateMap[candidateKey] = recycleBinCandidate{
				portalID:    portalID,
				displayName: name,
			}
			log.Info().
				Str("portal_id", portalID).
				Str("record_name", chat.RecordName).
				Str("name", name).
				Msg("DELETE-SEED: seeded deleted chat from recoverable chat identity")
		}
	}

	log.Info().Msg("DELETE-SEED: reading recoverable message GUIDs from Apple's recycle bin")
	guids, err := c.client.ListRecoverableMessageGuids()
	if err != nil {
		log.Warn().Err(err).Msg("DELETE-SEED: failed to read recoverable message GUIDs")
	} else if len(guids) == 0 {
		log.Info().Msg("DELETE-SEED: no recoverable messages found, nothing to seed")
	} else {
		log.Info().Int("recoverable_guids", len(guids)).
			Msg("DELETE-SEED: read recoverable message GUIDs, matching against cloud_message")
		portalHints := c.buildRecoverableMessagePortalHints(ctx, guids)
		if len(portalHints) > 0 {
			log.Info().Int("portal_hints", len(portalHints)).
				Msg("DELETE-SEED: derived portal hints from recoverable message metadata")
		}

		states, err := c.cloudStore.classifyRecycleBinPortals(ctx, guids)
		if err != nil {
			log.Warn().Err(err).Msg("DELETE-SEED: failed to match GUIDs against cloud_message")
		} else if len(states) == 0 {
			log.Info().Int("recoverable_guids", len(guids)).
				Msg("DELETE-SEED: no portals matched recoverable messages")
		} else {
			var deletedStates []recycleBinPortalState
			autoRecovered := 0
			for _, state := range states {
				if state.NeedsUndelete() {
					undeleted, err := c.cloudStore.undeleteCloudMessagesByPortalID(ctx, state.PortalID)
					if err != nil {
						log.Warn().Err(err).Str("portal_id", state.PortalID).
							Msg("DELETE-SEED: failed to auto-undelete restored portal")
					} else {
						c.recentlyDeletedPortalsMu.Lock()
						delete(c.recentlyDeletedPortals, state.PortalID)
						c.recentlyDeletedPortalsMu.Unlock()
						log.Info().
							Str("portal_id", state.PortalID).
							Int("undeleted_messages", undeleted).
							Int("recoverable", state.Recoverable).
							Int("recoverable_suffix", state.RecoverableSuffix).
							Msg("DELETE-SEED: auto-recovered restored portal before backfill")
						autoRecovered++
					}
				}
				if state.LooksDeleted() {
					if hasChat, err := c.cloudStore.portalHasChat(ctx, state.PortalID); err != nil {
						log.Warn().Err(err).Str("portal_id", state.PortalID).
							Msg("DELETE-SEED: failed to check live chat state for recycle-bin match")
					} else if hasChat {
						c.recentlyDeletedPortalsMu.Lock()
						delete(c.recentlyDeletedPortals, state.PortalID)
						c.recentlyDeletedPortalsMu.Unlock()
						log.Info().
							Str("portal_id", state.PortalID).
							Int("recoverable", state.Recoverable).
							Int("recoverable_suffix", state.RecoverableSuffix).
							Msg("DELETE-SEED: live CloudKit chat row wins over recycle-bin message history")
						continue
					}
					if isRestoreOverridden(state.PortalID) {
						if undeleted, err := c.cloudStore.undeleteCloudMessagesByPortalID(ctx, state.PortalID); err != nil {
							log.Warn().Err(err).Str("portal_id", state.PortalID).
								Msg("DELETE-SEED: failed to honor restore override for recycle-bin match")
						} else {
							c.recentlyDeletedPortalsMu.Lock()
							delete(c.recentlyDeletedPortals, state.PortalID)
							c.recentlyDeletedPortalsMu.Unlock()
							log.Info().
								Str("portal_id", state.PortalID).
								Int("undeleted_messages", undeleted).
								Msg("DELETE-SEED: skipping recycle-bin delete block because user restored it manually")
						}
						continue
					}
					deletedStates = append(deletedStates, state)
				}
			}
			if autoRecovered > 0 {
				log.Info().Int("auto_recovered", autoRecovered).
					Msg("DELETE-SEED: restored portals were undeleted locally before portal creation")
			}
			if len(deletedStates) == 0 {
				log.Info().Int("recoverable_guids", len(guids)).
					Msg("DELETE-SEED: no portals currently look deleted after reconciliation")
			} else {
				deletedPortalIDs := make([]string, 0, len(deletedStates))
				for _, state := range deletedStates {
					deletedPortalIDs = append(deletedPortalIDs, state.PortalID)
				}
				log.Info().Int("deleted_portals", len(deletedStates)).
					Strs("portal_ids", deletedPortalIDs).
					Msg("DELETE-SEED: found portals whose newest messages are still in Apple's recycle bin")

				// Mark these portals as deleted in memory + DB, and store candidates
				// for the post-backfill bridgebot notification.
				c.recentlyDeletedPortalsMu.Lock()
				if c.recentlyDeletedPortals == nil {
					c.recentlyDeletedPortals = make(map[string]deletedPortalEntry)
				}
				for _, state := range deletedStates {
					portalID := state.PortalID
					if _, exists := c.recentlyDeletedPortals[portalID]; exists {
						continue
					}
					if err := c.cloudStore.ensureDeletedChatTombstoneByPortalID(ctx, portalID); err != nil {
						log.Warn().Err(err).Str("portal_id", portalID).
							Msg("DELETE-SEED: failed to persist tombstone for blocked portal")
					}
					c.recentlyDeletedPortals[portalID] = deletedPortalEntry{
						deletedAt:   time.Now(),
						isTombstone: true,
					}
					if err := c.cloudStore.deleteLocalChatByPortalID(ctx, portalID); err != nil {
						log.Warn().Err(err).Str("portal_id", portalID).
							Msg("DELETE-SEED: failed to soft-delete local data for seeded portal")
					}
					portalKey := networkid.PortalKey{ID: networkid.PortalID(portalID), Receiver: c.UserLogin.ID}
					name := friendlyPortalName(ctx, c.Main.Bridge, c, portalKey, portalID)
					stateGroupID := ""
					if strings.HasPrefix(portalID, "gid:") {
						stateGroupID = c.cloudStore.getGroupIDForPortalID(ctx, portalID)
					}
					candidateKey := groupPortalDedupKey(portalID, stateGroupID, nil)
					candidateMap[candidateKey] = recycleBinCandidate{
						portalID:    portalID,
						displayName: name,
						recoverable: state.Recoverable,
						total:       state.Total,
					}
					log.Info().
						Str("portal_id", portalID).
						Str("name", name).
						Int("recoverable", state.Recoverable).
						Int("recoverable_suffix", state.RecoverableSuffix).
						Int("total", state.Total).
						Msg("DELETE-SEED: blocked portal — latest messages are still in Apple's recycle bin")
					seeded++
				}
				c.recentlyDeletedPortalsMu.Unlock()
			}
		}

		hintedSeeded := 0
		for _, hint := range portalHints {
			hintGroupID := ""
			if strings.HasPrefix(hint.PortalID, "gid:") {
				// Cross-reference cloud_chat to find the real group_id.
				// The portal ID's UUID might be a chat_id, not the group_id.
				if resolved := c.cloudStore.getGroupIDForPortalID(ctx, hint.PortalID); resolved != "" {
					hintGroupID = resolved
				} else {
					hintGroupID = strings.TrimPrefix(hint.PortalID, "gid:")
				}
			}
			candidateKey := groupPortalDedupKey(hint.PortalID, hintGroupID, hint.Participants)
			if _, exists := candidateMap[candidateKey]; exists {
				continue
			}
			if isRestoreOverridden(hint.PortalID) {
				if undeleted, err := c.cloudStore.undeleteCloudMessagesByPortalID(ctx, hint.PortalID); err != nil {
					log.Warn().Err(err).Str("portal_id", hint.PortalID).
						Msg("DELETE-SEED: failed to honor restore override for recoverable message hint")
				} else {
					c.recentlyDeletedPortalsMu.Lock()
					delete(c.recentlyDeletedPortals, hint.PortalID)
					c.recentlyDeletedPortalsMu.Unlock()
					log.Info().
						Str("portal_id", hint.PortalID).
						Int("undeleted_messages", undeleted).
						Msg("DELETE-SEED: skipping recoverable message hint because user restored it manually")
				}
				continue
			}

			hasChat, err := c.cloudStore.portalHasChat(ctx, hint.PortalID)
			if err != nil {
				log.Warn().Err(err).Str("portal_id", hint.PortalID).
					Msg("DELETE-SEED: failed to check live chat state for recoverable message hint")
				continue
			}
			if hasChat {
				c.recentlyDeletedPortalsMu.Lock()
				delete(c.recentlyDeletedPortals, hint.PortalID)
				c.recentlyDeletedPortalsMu.Unlock()
				log.Debug().
					Str("portal_id", hint.PortalID).
					Int("recoverable", hint.Count).
					Msg("DELETE-SEED: recoverable message hint already has live CloudKit row, skipping tombstone seed")
				continue
			}

			softDeletedInfo, infoErr := c.cloudStore.getSoftDeletedPortalInfo(ctx, hint.PortalID)
			if infoErr != nil {
				log.Warn().Err(infoErr).Str("portal_id", hint.PortalID).
					Msg("DELETE-SEED: failed to inspect local deleted state for recoverable message hint")
				continue
			}
			if !softDeletedInfo.Deleted && hint.Count < 2 {
				log.Debug().
					Str("portal_id", hint.PortalID).
					Int("recoverable", hint.Count).
					Msg("DELETE-SEED: skipping weak recoverable message hint without local deleted state")
				continue
			}

			participantsJSON, marshalErr := json.Marshal(hint.Participants)
			if marshalErr != nil {
				log.Warn().Err(marshalErr).Str("portal_id", hint.PortalID).
					Msg("DELETE-SEED: failed to serialize recoverable message participants")
				continue
			}

			cloudChatID := hint.CloudChatID
			if cloudChatID == "" {
				cloudChatID = "synthetic:recoverable:" + hint.PortalID
			}
			groupID := ""
			if strings.HasPrefix(hint.PortalID, "gid:") {
				groupID = strings.TrimPrefix(hint.PortalID, "gid:")
			}
			if err = c.cloudStore.insertDeletedChatTombstone(
				ctx,
				cloudChatID,
				hint.PortalID,
				"",
				groupID,
				hint.Service,
				nil,
				string(participantsJSON),
			); err != nil {
				log.Warn().Err(err).Str("portal_id", hint.PortalID).
					Msg("DELETE-SEED: failed to persist recoverable message tombstone")
				continue
			}
			if err = c.cloudStore.deleteLocalChatByPortalID(ctx, hint.PortalID); err != nil {
				log.Warn().Err(err).Str("portal_id", hint.PortalID).
					Msg("DELETE-SEED: failed to soft-delete local data for recoverable message hint")
			}

			added := false
			c.recentlyDeletedPortalsMu.Lock()
			if c.recentlyDeletedPortals == nil {
				c.recentlyDeletedPortals = make(map[string]deletedPortalEntry)
			}
			if _, exists := c.recentlyDeletedPortals[hint.PortalID]; !exists {
				c.recentlyDeletedPortals[hint.PortalID] = deletedPortalEntry{
					deletedAt:   time.Now(),
					isTombstone: true,
				}
				added = true
			}
			c.recentlyDeletedPortalsMu.Unlock()
			if added {
				seeded++
				hintedSeeded++
			}

			portalKey := networkid.PortalKey{ID: networkid.PortalID(hint.PortalID), Receiver: c.UserLogin.ID}
			name := friendlyPortalName(ctx, c.Main.Bridge, c, portalKey, hint.PortalID)
			candidateMap[candidateKey] = recycleBinCandidate{
				portalID:    hint.PortalID,
				displayName: name,
				recoverable: hint.Count,
				total:       hint.Count,
			}
			log.Info().
				Str("portal_id", hint.PortalID).
				Str("name", name).
				Int("recoverable", hint.Count).
				Str("cloud_chat_id", hint.CloudChatID).
				Msg("DELETE-SEED: seeded deleted chat from recoverable message metadata")
		}
		if hintedSeeded > 0 {
			log.Info().Int("seeded", hintedSeeded).
				Msg("DELETE-SEED: seeded deleted chats from recoverable message metadata")
		}
	}

	// Store candidates for post-backfill notification.
	candidates := make([]recycleBinCandidate, 0, len(candidateMap))
	for _, candidate := range candidateMap {
		candidates = append(candidates, candidate)
	}
	if len(candidates) > 0 {
		c.recycleBinCandidatesMu.Lock()
		c.recycleBinCandidates = candidates
		c.recycleBinCandidatesMu.Unlock()
	}

	log.Info().Int("seeded", seeded).Int("total_recoverable_guids", len(guids)).
		Msg("DELETE-SEED: finished — blocked portals will be shown to user via bridgebot")
}

// notifyRecycleBinCandidates sends a bridgebot message listing chats that were
// blocked because their messages appear in Apple's recycle bin. The user can
// restore false positives with !restore-chat.
func (c *IMClient) notifyRecycleBinCandidates(log zerolog.Logger) {
	c.recycleBinCandidatesMu.Lock()
	candidates := c.recycleBinCandidates
	c.recycleBinCandidates = nil
	c.recycleBinCandidatesMu.Unlock()

	if len(candidates) == 0 {
		return
	}

	ctx := context.Background()
	user := c.UserLogin.User
	mgmtRoom, err := user.GetManagementRoom(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to get management room for recycle bin notification")
		return
	}

	var sb strings.Builder
	sb.WriteString("**Chats blocked from Apple's recycle bin:**\n\n")
	sb.WriteString("These chats have messages in Apple's \"Recently Deleted\" and were not created during backfill. ")
	sb.WriteString("If any were restored on your iPhone and should appear here, use `!restore-chat` to bring them back.\n\n")
	for i, c := range candidates {
		sb.WriteString(fmt.Sprintf("%d. **%s**\n", i+1, c.displayName))
	}
	sb.WriteString(fmt.Sprintf("\nUse `!restore-chat` to restore any of these chats."))

	content := format.RenderMarkdown(sb.String(), true, false)
	content.MsgType = event.MsgNotice
	_, err = c.Main.Bridge.Bot.SendMessage(ctx, mgmtRoom, event.EventMessage, &event.Content{
		Parsed: content,
	}, nil)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to send recycle bin notification to management room")
	} else {
		log.Info().Int("count", len(candidates)).Msg("Sent recycle bin notification to user")
	}
}

func (c *IMClient) setContactsReady(log zerolog.Logger) {
	firstTime := false
	c.contactsReadyLock.Lock()
	if !c.contactsReady {
		c.contactsReady = true
		firstTime = true
		readyCh := c.contactsReadyCh
		c.contactsReadyLock.Unlock()
		if readyCh != nil {
			close(readyCh)
		}
		log.Info().Msg("Contacts readiness gate satisfied")
	} else {
		c.contactsReadyLock.Unlock()
	}

	// Re-resolve ghost and group names from contacts on every sync,
	// not just the first time. Contacts may have been added/edited in iCloud.
	if firstTime {
		log.Info().Msg("Running initial contact name resolution for ghosts and group portals")
	} else {
		log.Info().Msg("Re-syncing contact names for ghosts and group portals")
	}
	go c.refreshGhostNamesFromContacts(log)
	go c.refreshGroupPortalNamesFromContacts(log)
	// Presence subscription only needs to run on first-ready and whenever new
	// StatusKit keys arrive (via OnKeysReceived). The ghost set doesn't change
	// per periodic contact-sync tick; re-subscribing every 15 minutes just
	// churns APS interest tokens for no gain and spams the journal. Leave
	// incremental updates to OnKeysReceived.
	if firstTime {
		go c.subscribeToContactPresence(log)
	}
}

// subscribeToContactPresence subscribes to iMessage presence updates for all
// known ghosts via StatusKit. Presence changes are delivered via
// OnStatusUpdate and mapped to Matrix ghost presence.
func (c *IMClient) subscribeToContactPresence(log zerolog.Logger) {
	if c.client == nil {
		return
	}
	// Guard against panics inside the SubscribeToStatus FFI call (upstream
	// StatusKit path has several panic sites, e.g. statuskit.rs:736).
	defer func() {
		if r := recover(); r != nil {
			log.Warn().Interface("panic", r).Msg("subscribeToContactPresence panicked — skipped")
		}
	}()
	// Serialize concurrent calls so each one reads the freshest state.keys
	// snapshot from the Rust side. A previous leading-edge debounce was
	// swallowing re-subscriptions fired by OnKeysReceived: startup ran an
	// initial subscribe with zero keys in state, then the key-sharing
	// response arrived within the debounce window and the follow-up
	// subscribe (which would have picked up the new channel) was skipped.
	// No further key-sharing arrives for a contact who already shared, so
	// the channel stayed unsubscribed until the next restart.
	c.lastPresenceSubscribeLock.Lock()
	defer c.lastPresenceSubscribeLock.Unlock()
	c.lastPresenceSubscribe = time.Now()

	ctx := context.Background()
	rows, err := c.Main.Bridge.DB.RawDB.QueryContext(ctx, "SELECT id FROM ghost WHERE bridge_id=$1", c.Main.Bridge.ID)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to query ghosts for presence subscription")
		return
	}
	defer rows.Close()

	selfHandles := make(map[string]struct{}, len(c.allHandles))
	for _, h := range c.allHandles {
		selfHandles[h] = struct{}{}
	}
	var handles []string
	for rows.Next() {
		var ghostID string
		if err := rows.Scan(&ghostID); err != nil {
			continue
		}
		if _, isSelf := selfHandles[ghostID]; isSelf {
			continue
		}
		handles = append(handles, ghostID)
	}
	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("Ghost row iteration error during presence subscription — subscription may be incomplete")
		return
	}
	if len(handles) == 0 {
		return
	}
	if err := c.client.SubscribeToStatus(handles); err != nil {
		log.Warn().Err(err).Int("count", len(handles)).Msg("Failed to subscribe to presence")
	} else {
		log.Info().Int("count", len(handles)).Msg("Subscribed to iMessage presence for known ghosts")
	}
}

// statusKitInterInviteDelay paces per-handle StatusKit keysharing invites.
// OB-Android's pattern is one invite per chat activation — implicitly
// one-handle-at-a-time as the user opens chats. We approximate with a
// paced sweep (startup + periodic), one handle per IDS send, with this
// delay between sends so we don't recreate the all-at-once batch shape
// that appears to trigger peer-side filtering / spam heuristics.
const statusKitInterInviteDelay = 1500 * time.Millisecond

// statusKitLastInviteKeyPrefix is the KV store prefix for per-handle
// last-ATTEMPT timestamps (RFC3339). Used only on the periodic tick path
// to bound failed-retry cadence; successful sends are latched separately
// and never re-attempted.
const statusKitLastInviteKeyPrefix = "statuskit.last_invite."

// statusKitInvitedOkKeyPrefix marks handles that received a StatusKit
// invite which IDS accepted (no error, targets > 0). Once marked, bridge
// never re-invites that handle. Matches OB-Android's one-shot latch via
// `config == zenModeIsShared` — invite per chat activation, never again.
// Peer iOS may treat repeat invites from a device it already has keys
// for as spam.
const statusKitInvitedOkKeyPrefix = "statuskit.invited_ok."

// statusKitPerHandleMinSpacing is the minimum time between failed-retry
// invites. Only applies on the periodic tick path to handles that did NOT
// succeed; successful sends are latched and never retried.
const statusKitPerHandleMinSpacing = 4 * time.Hour

// inviteContactsToStatusSharing sends our StatusKit key to the peer
// handles of every 1:1 iMessage portal that has not yet keyed us back,
// one handle per IDS message, with a short delay between sends. See
// inviteContactsToStatusSharingOpts for the spacing-gate flag.
//
// Uses 1:1 portal participants — NOT the ghost table — as the source set,
// matching OB-Android's `chat.participants.length == 1` gate
// (chat_manager.dart:70). Ghosts include group-chat-only contacts whose
// iOS devices have no 1:1 keysharing relationship with us; peer iOS
// appears to filter invites from devices it hasn't had 1:1 iMessage
// interaction with, so inviting group-only ghosts is spam that produces
// zero reshares and likely contributes to peer-side throttling.
func (c *IMClient) inviteContactsToStatusSharing(log zerolog.Logger) {
	c.inviteContactsToStatusSharingOpts(log, false, false)
}

// inviteContactsToStatusSharingOpts is the core invite sweep.
//
//   - respectSpacing (periodic tick path): skip handles invited within
//     statusKitPerHandleMinSpacing — bounds worst-case re-invite rate for
//     an unresponsive peer. Startup / post-backfill paths pass false.
//   - bypassLatch (user-invoked retry): ignore the invited_ok one-shot
//     latch so every pending 1:1 portal target is re-invited, even peers
//     that previously got an accepted invite. Use for !statuskit-invite-all;
//     automatic paths leave it false to preserve the "invite once per peer"
//     contract with peer iOS.
func (c *IMClient) inviteContactsToStatusSharingOpts(log zerolog.Logger, respectSpacing bool, bypassLatch bool) {
	if c.client == nil || c.handle == "" {
		log.Warn().Bool("client_nil", c.client == nil).Str("handle", c.handle).Msg("StatusKit invite: skipped (client or handle not ready)")
		return
	}
	log.Info().Str("handle", c.handle).Msg("StatusKit invite: starting")
	defer func() {
		if r := recover(); r != nil {
			log.Warn().Interface("panic", r).Msg("inviteContactsToStatusSharing panicked — skipped")
		}
	}()

	ctx := context.Background()
	portals, err := c.Main.Bridge.GetAllPortalsWithMXID(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("StatusKit invite: failed to query portals")
		return
	}
	selfHandles := make(map[string]struct{}, len(c.allHandles))
	for _, h := range c.allHandles {
		selfHandles[h] = struct{}{}
	}

	// Iterate 1:1 DM portals only. Portal.ID for a DM IS the peer's handle
	// (tel:+1... or mailto:...). Group portals have `gid:` prefix or a
	// comma-joined participant ID; both are rejected by isGroupPortalID.
	// De-dupe via a set in case two portals ever share a handle.
	targetSet := make(map[string]struct{})
	var rawCount, groupSkipped, selfSkipped int
	for _, portal := range portals {
		if portal == nil {
			continue
		}
		// Only consider portals owned by this login.
		if portal.Receiver != c.UserLogin.ID {
			continue
		}
		rawCount++
		id := string(portal.ID)
		if isGroupPortalID(id) {
			groupSkipped++
			continue
		}
		if _, isSelf := selfHandles[id]; isSelf {
			selfSkipped++
			continue
		}
		targetSet[id] = struct{}{}
	}
	allGhosts := make([]string, 0, len(targetSet))
	for h := range targetSet {
		allGhosts = append(allGhosts, h)
	}
	log.Info().
		Int("portals_raw", rawCount).
		Int("group_skipped", groupSkipped).
		Int("self_skipped", selfSkipped).
		Int("one_to_one_targets", len(allGhosts)).
		Msg("StatusKit invite: portal sweep")
	if len(allGhosts) == 0 {
		log.Warn().Msg("StatusKit invite: no 1:1 portal targets — nothing to invite")
		return
	}

	// Subtract peers who've already keyed us back. GetKnownHandles reads
	// from the in-memory StatusKit state, which is hydrated from
	// statuskit-state.plist at startup.
	knownSet := make(map[string]struct{})
	if sk, skErr := c.client.GetStatuskitClient(); skErr == nil && sk != nil {
		for _, h := range sk.GetKnownHandles() {
			knownSet[h] = struct{}{}
		}
	}

	now := time.Now()
	var pending []string
	var skippedKnown, skippedSpacing, skippedAlreadySent int
	for _, h := range allGhosts {
		if _, known := knownSet[h]; known {
			skippedKnown++
			continue
		}
		// One-shot latch: if we've previously sent this handle an invite
		// that IDS accepted, never re-send. Matches OB's one-invite-per-
		// chat-activation model; repeated invites to the same peer
		// produce no additional reshares and may trip spam heuristics.
		// User-invoked retry (bypassLatch) intentionally overrides this
		// so `!statuskit-invite-all` can re-drive every known peer.
		if !bypassLatch && c.Main.Bridge.DB.KV.Get(ctx, database.Key(statusKitInvitedOkKeyPrefix+h)) != "" {
			skippedAlreadySent++
			continue
		}
		if respectSpacing {
			last := c.Main.Bridge.DB.KV.Get(ctx, database.Key(statusKitLastInviteKeyPrefix+h))
			if last != "" {
				if ts, parseErr := time.Parse(time.RFC3339, last); parseErr == nil && now.Sub(ts) < statusKitPerHandleMinSpacing {
					skippedSpacing++
					continue
				}
			}
		}
		pending = append(pending, h)
	}

	log.Info().
		Int("total_targets", len(allGhosts)).
		Int("already_keyed", skippedKnown).
		Int("already_invited_ok", skippedAlreadySent).
		Int("spacing_skip", skippedSpacing).
		Int("pending", len(pending)).
		Bool("respect_spacing", respectSpacing).
		Msg("StatusKit invite: plan")

	if len(pending) == 0 {
		return
	}

	// One sender only — OB calls invite_to_channel with a single
	// `ensureHandle()` result per chat, not with every registered handle.
	sender := c.handle
	var okCount, failCount, timeoutCount int
	nowStr := now.Format(time.RFC3339)

	// Per-invite timeout. An invite_to_channel call that blocks (e.g. on a
	// poisoned anisette mutex, or an IDS cache lock held by another task)
	// used to freeze the whole sweep — one bad handle and the remaining
	// 20 never got invited, summary log never fired. Run each invite in a
	// goroutine and race it against a 30s deadline; on timeout, log and
	// move on. Rust side keeps running in the background and will finish
	// or fail at its own pace; we just don't wait on it.
	const perInviteTimeout = 30 * time.Second

	for i, h := range pending {
		inviteDone := make(chan error, 1)
		go func(handle string) {
			inviteDone <- c.client.InviteToStatusSharing(sender, []string{handle})
		}(h)

		select {
		case err := <-inviteDone:
			if err != nil {
				failCount++
				log.Warn().Err(err).Str("sender", sender).Str("handle", h).Int("i", i+1).Int("total", len(pending)).Msg("StatusKit invite: failed for handle")
			} else {
				okCount++
				// Write both keys:
				// - invited_ok: one-shot latch, never re-invite this peer
				// - last_invite: periodic-tick spacing timestamp (no effect
				//   because invited_ok is checked first, but harmless and
				//   useful for debug timelines)
				c.Main.Bridge.DB.KV.Set(ctx, database.Key(statusKitInvitedOkKeyPrefix+h), nowStr)
				c.Main.Bridge.DB.KV.Set(ctx, database.Key(statusKitLastInviteKeyPrefix+h), nowStr)
				log.Info().Str("sender", sender).Str("handle", h).Int("i", i+1).Int("total", len(pending)).Msg("StatusKit invite: ok for handle")
			}
		case <-time.After(perInviteTimeout):
			timeoutCount++
			log.Warn().Str("sender", sender).Str("handle", h).Int("i", i+1).Int("total", len(pending)).Dur("timeout", perInviteTimeout).Msg("StatusKit invite: timed out for handle — abandoning this handle, continuing sweep")
		case <-c.stopChan:
			log.Info().Int("done", i).Int("total", len(pending)).Msg("StatusKit invite: bridge stopping, aborting sweep")
			return
		}

		// Skip the delay after the last handle.
		if i < len(pending)-1 {
			select {
			case <-time.After(statusKitInterInviteDelay):
			case <-c.stopChan:
				log.Info().Int("done", i+1).Int("total", len(pending)).Msg("StatusKit invite: bridge stopping, aborting sweep")
				return
			}
		}
	}
	log.Info().Int("pending", len(pending)).Int("ok", okCount).Int("failed", failCount).Int("timed_out", timeoutCount).Str("sender", sender).Msg("Sent StatusKit key invites one-per-handle (pending-only, paced)")
}

func (c *IMClient) refreshGhostNamesFromContacts(log zerolog.Logger) {
	if c.contacts == nil {
		return
	}
	ctx := context.Background()

	// Use bridge_id-scoped query and fetch the current name for diff-gating.
	rows, err := c.Main.Bridge.DB.Database.Query(ctx,
		"SELECT id, COALESCE(name, '') FROM ghost WHERE bridge_id=$1",
		c.Main.Bridge.ID,
	)
	if err != nil {
		log.Err(err).Msg("Failed to query ghosts for contact name refresh")
		return
	}
	defer rows.Close()

	type ghostEntry struct {
		id   networkid.UserID
		name string
	}
	var ghosts []ghostEntry
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			log.Err(err).Msg("Failed to scan ghost ID")
			continue
		}
		ghosts = append(ghosts, ghostEntry{networkid.UserID(id), name})
	}
	if err := rows.Err(); err != nil {
		log.Err(err).Msg("Ghost ID row iteration error")
	}
	rows.Close()

	updated := 0
	for _, g := range ghosts {
		// Skip ghosts with no matching contact (efficiency: avoids loading
		// the full ghost object for participants who aren't in the address book).
		localID := stripIdentifierPrefix(string(g.id))
		if localID == "" {
			continue
		}
		contact, _ := c.contacts.GetContactInfo(localID)
		if contact == nil || !contact.HasName() {
			continue
		}
		// Diff-gate: compute the expected displayname and skip if it matches
		// the stored name. This prevents unnecessary Matrix profile update API
		// calls on every contact refresh cycle (AggressiveUpdateInfo=true means
		// UpdateInfo always makes an API call; diffing here is our only guard).
		expectedName := c.Main.Config.FormatDisplayname(DisplaynameParams{
			FirstName: contact.FirstName,
			LastName:  contact.LastName,
			Nickname:  contact.Nickname,
			ID:        localID,
		})
		if g.name == expectedName {
			continue
		}
		ghost, err := c.Main.Bridge.GetGhostByID(ctx, g.id)
		if err != nil || ghost == nil {
			log.Warn().Err(err).Str("ghost_id", string(g.id)).Msg("Failed to load ghost for name refresh")
			continue
		}
		// Use the full GetUserInfo → UpdateInfo cycle (same as refreshAllGhosts)
		// to ensure name, avatar, and identifiers are all propagated to Matrix.
		info, err := c.GetUserInfo(ctx, ghost)
		if err != nil || info == nil {
			continue
		}
		ghost.UpdateInfo(ctx, info)
		updated++
	}
	log.Info().Int("updated", updated).Int("total", len(ghosts)).Msg("Refreshed ghost names from contacts")
}

// refreshGroupPortalNamesFromContacts re-resolves group portal names using
// contact data. Portals created before contacts loaded may have raw phone
// numbers / email addresses as the room name. This also picks up contact
// edits on subsequent periodic syncs.
func (c *IMClient) refreshGroupPortalNamesFromContacts(log zerolog.Logger) {
	if c.contacts == nil {
		return
	}
	ctx := context.Background()

	portals, err := c.Main.Bridge.GetAllPortalsWithMXID(ctx)
	if err != nil {
		log.Err(err).Msg("Failed to load portals for group name refresh")
		return
	}

	updated := 0
	total := 0
	for _, portal := range portals {
		if portal.Receiver != c.UserLogin.ID {
			continue
		}
		portalID := string(portal.ID)
		isGroup := strings.HasPrefix(portalID, "gid:") || strings.Contains(portalID, ",")
		if !isGroup {
			continue
		}
		total++

		newName, authoritative := c.resolveGroupName(ctx, portalID)
		if newName == "" || newName == portal.Name {
			continue
		}
		// Don't overwrite an existing portal name with a contact-derived
		// fallback — only authoritative sources (user-set iMessage group
		// names from the in-memory cache or CloudKit) should rename.
		if !authoritative && portal.Name != "" {
			continue
		}

		c.UserLogin.QueueRemoteEvent(&simplevent.ChatInfoChange{
			EventMeta: simplevent.EventMeta{
				Type: bridgev2.RemoteEventChatInfoChange,
				PortalKey: networkid.PortalKey{
					ID:       portal.ID,
					Receiver: c.UserLogin.ID,
				},
				LogContext: func(lc zerolog.Context) zerolog.Context {
					return lc.Str("portal_id", portalID).Str("source", "group_name_refresh")
				},
			},
			ChatInfoChange: &bridgev2.ChatInfoChange{
				ChatInfo: &bridgev2.ChatInfo{
					Name: &newName,
					// Exclude from timeline so Beeper doesn't render
					// contact-based name resolution as a bridge bot message.
					ExcludeChangesFromTimeline: true,
				},
			},
		})
		updated++
	}
	log.Info().Int("updated", updated).Int("total_groups", total).Msg("Refreshed group portal names from contacts")
}

// contactsWaitTimeout is how long CloudKit sync waits for the contacts
// readiness gate before proceeding without contacts. Contacts are nice-to-have
// for name resolution but shouldn't block backfill indefinitely.
const contactsWaitTimeout = 30 * time.Second

func (c *IMClient) waitForContactsReady(log zerolog.Logger) bool {
	c.contactsReadyLock.RLock()
	alreadyReady := c.contactsReady
	readyCh := c.contactsReadyCh
	c.contactsReadyLock.RUnlock()
	if alreadyReady {
		return true
	}

	log.Info().Dur("timeout", contactsWaitTimeout).Msg("Waiting for contacts readiness gate before CloudKit sync")
	select {
	case <-readyCh:
		log.Info().Msg("Contacts readiness gate opened")
		return true
	case <-time.After(contactsWaitTimeout):
		log.Warn().Msg("Contacts readiness timed out — proceeding with CloudKit sync without contacts")
		return true
	case <-c.stopChan:
		return false
	}
}

func (c *IMClient) startCloudSyncController(log zerolog.Logger) {
	if c.cloudStore == nil {
		log.Warn().Msg("CloudKit sync controller not started: cloud store is nil")
		return
	}
	if c.client == nil {
		log.Warn().Msg("CloudKit sync controller not started: client is nil")
		return
	}
	log.Info().Msg("Starting CloudKit sync controller goroutine")
	go c.runCloudSyncController(log.With().Str("component", "cloud_sync").Logger())
}

// cloudSyncRetryInterval is the interval used when a bootstrap sync attempt
// fails, so recovery happens quickly.
const cloudSyncRetryInterval = 1 * time.Minute

func (c *IMClient) runCloudSyncController(log zerolog.Logger) {
	// NOTE: no defer recover() here intentionally. A panic in this goroutine
	// must crash the process so the bridge restarts and re-runs CloudKit sync.
	// Swallowing the panic would leave setCloudSyncDone() uncalled, permanently
	// blocking the APNs message buffer and dropping all incoming messages.

	// Derive a cancellable context from stopChan so that preUploadCloudAttachments
	// and other long-running operations can be interrupted promptly on shutdown
	// instead of running to completion (worst case: 48 min of leaked downloads).
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-c.stopChan:
			cancel()
		case <-ctx.Done():
		}
	}()
	defer cancel()
	controllerStart := time.Now()
	if !c.waitForContactsReady(log) {
		c.setCloudSyncDone() // unblock APNs portal creation
		return
	}
	log.Info().Dur("contacts_wait", time.Since(controllerStart)).Msg("Contacts ready, proceeding with CloudKit sync")

	// Repopulate recentlyDeletedPortals from DB before CloudKit sync.
	// This ensures tombstones from prior sessions survive bridge restarts
	// even if CloudKit's incremental sync (with a saved continuation token)
	// doesn't re-deliver the tombstone records.
	if c.cloudStore != nil {
		if deletedPortals, err := c.cloudStore.listDeletedPortalIDs(ctx); err != nil {
			log.Warn().Err(err).Msg("Failed to load deleted portals from DB")
		} else if len(deletedPortals) > 0 {
			c.recentlyDeletedPortalsMu.Lock()
			if c.recentlyDeletedPortals == nil {
				c.recentlyDeletedPortals = make(map[string]deletedPortalEntry)
			}
			for _, portalID := range deletedPortals {
				c.recentlyDeletedPortals[portalID] = deletedPortalEntry{
					deletedAt:   time.Now(),
					isTombstone: true,
				}
			}
			c.recentlyDeletedPortalsMu.Unlock()
			log.Info().Int("count", len(deletedPortals)).
				Msg("Repopulated recentlyDeletedPortals from DB (restart safety)")
		}
	}

	// Bootstrap: download CloudKit data with retries until successful.
	// IMPORTANT: Do NOT set cloudSyncDone on failure. The APNs gate must
	// stay closed until sync succeeds — otherwise APNs echoes can create
	// portals for deleted chats because cloud_message has no UUID data
	// and recentlyDeletedPortals has no tombstone entries.
	// Messages for EXISTING portals are still delivered during this time
	// (handleMessage only checks cloudSyncDone for NEW portal creation).
	c.cloudSyncRunningLock.Lock()
	c.cloudSyncRunning = true
	c.cloudSyncRunningLock.Unlock()
	// Ensure cloudSyncRunning is cleared on all exit paths (early return,
	// panic, stopChan) so handleChatRecover doesn't defer forever.
	defer func() {
		c.cloudSyncRunningLock.Lock()
		c.cloudSyncRunning = false
		c.cloudSyncRunningLock.Unlock()
	}()
	for {
		err := c.runCloudSyncOnceSerialized(ctx, log, true)
		if err != nil {
			log.Error().Err(err).
				Dur("retry_in", cloudSyncRetryInterval).
				Msg("CloudKit sync failed, will retry (APNs gate stays closed)")
			select {
			case <-time.After(cloudSyncRetryInterval):
				continue
			case <-c.stopChan:
				return
			}
		}
		break
	}

	// Soft-delete cloud records for portals that should be dead:
	// Tombstoned portals — chat tombstones processed before messages,
	// so message import creates cloud_message rows for deleted chats.
	// deleteLocalChatByPortalID marks cloud_message deleted=TRUE (preserving
	// UUIDs for echo detection) and removes cloud_chat rows.
	skipPortals := make(map[string]bool)
	c.recentlyDeletedPortalsMu.RLock()
	for portalID := range c.recentlyDeletedPortals {
		skipPortals[portalID] = true
	}
	c.recentlyDeletedPortalsMu.RUnlock()
	for portalID := range skipPortals {
		if err := c.cloudStore.deleteLocalChatByPortalID(ctx, portalID); err != nil {
			log.Warn().Err(err).Str("portal_id", portalID).Msg("Failed to soft-delete records for dead portal")
		}
	}
	if len(skipPortals) > 0 {
		log.Info().Int("count", len(skipPortals)).
			Msg("Soft-deleted re-imported records for dead portals")
	}

	// Pre-upload all CloudKit attachments to Matrix before triggering portal
	// creation. This populates attachmentContentCache so that FetchMessages
	// (which runs inside the portal event loop goroutine) gets instant cache
	// hits instead of blocking on CloudKit for 30+ minutes.
	c.preUploadCloudAttachments(ctx)

	// Seed delete knowledge from Apple's recycle bin BEFORE creating portals.
	// Must run after runCloudSyncOnce (PCS keys needed) but before
	// createPortalsFromCloudSync so deleted chats don't get portals.
	// IMPORTANT: Must also run BEFORE normalizeGroupChatPortalIDs, because
	// normalization unifies portal_ids (gid:<chat_id> → gid:<group_id>),
	// which would make portalHasChat match live CloudKit rows for chats
	// that are actually in the recycle bin.
	c.seedDeletedChatsFromRecycleBin(log)

	// Normalize inconsistent group portal IDs in cloud_chat: unify all
	// rows for the same group to use gid:<group_id> as the canonical portal_id.
	// This must run after seedDeletedChatsFromRecycleBin (which needs the
	// pre-normalization mismatch to correctly detect deleted chats) but
	// before createPortalsFromCloudSync (which needs unified portal_ids).
	if normalized, err := c.cloudStore.normalizeGroupChatPortalIDs(ctx); err != nil {
		log.Warn().Err(err).Msg("Failed to normalize group chat portal IDs")
	} else if normalized > 0 {
		log.Info().Int64("normalized", normalized).
			Msg("Normalized inconsistent group chat portal IDs (cloud_chat)")
	}

	// Normalize inconsistent group portal IDs in cloud_message.
	// resolveConversationID historically used CloudKit chat_id UUIDs before
	// the getChatPortalID-first fix (2eace9a), creating gid:<chat_id> rows
	// that differ from the canonical gid:<group_id>. This fixes those rows
	// using cloud_chat as the authoritative mapping, preventing duplicate
	// portal creation on startup and restore.
	if normalized, err := c.cloudStore.normalizeGroupMessagePortalIDs(ctx); err != nil {
		log.Warn().Err(err).Msg("Failed to normalize group message portal IDs")
	} else if normalized > 0 {
		log.Info().Int64("normalized", normalized).
			Msg("Normalized inconsistent group message portal IDs using cloud_chat mappings")
	}

	// Create portals and queue forward backfill for all of them.
	// Skip portals that are tombstoned or recently deleted this session.
	portalStart := time.Now()
	c.createPortalsFromCloudSync(ctx, log, skipPortals)
	c.setCloudSyncDone()

	log.Info().
		Dur("portal_creation_elapsed", time.Since(portalStart)).
		Dur("total_elapsed", time.Since(controllerStart)).
		Msg("CloudKit bootstrap complete — all portals queued, APNs portal creation enabled")

	// Notify the user about chats blocked from Apple's recycle bin.
	c.notifyRecycleBinCandidates(log)

	// Clean up stale entries in recentlyDeletedPortals to prevent unbounded
	// memory growth over long-running sessions.
	c.pruneRecentlyDeletedPortals(log)

	// Housekeeping: prune orphaned data that accumulates over time.
	// Run after bootstrap so the cleanup doesn't delay portal creation.
	c.runPostSyncHousekeeping(ctx, log)

	// Delayed incremental re-syncs: catch CloudKit messages that propagated
	// after the bootstrap sync completed. Messages sent in the last few minutes
	// may not be in the CloudKit changes feed yet (propagation delay). APNs
	// delivers them with CreatePortal=false (gate closed), so bridgev2 drops
	// them for non-existent portals. Multiple re-sync passes at increasing
	// intervals ensure we catch them as CloudKit propagates.
	//
	// IMPORTANT: These run inline (not in a goroutine) so that the defer
	// cancel() on the parent context doesn't fire until all re-syncs are
	// done. Previously these ran in a goroutine, but runCloudSyncController
	// returned immediately after launching it — the defer cancel() killed
	// the context, causing every re-sync to fail with "context canceled".
	delays := []time.Duration{15 * time.Second, 60 * time.Second, 3 * time.Minute}
	for i, delay := range delays {
		select {
		case <-time.After(delay):
		case <-c.stopChan:
			return
		}
		resyncLog := log.With().
			Str("source", "delayed_resync").
			Int("pass", i+1).
			Int("total_passes", len(delays)).
			Logger()
		resyncLog.Info().Msg("Running delayed incremental CloudKit re-sync")
		c.cloudSyncRunningLock.Lock()
		c.cloudSyncRunning = true
		c.cloudSyncRunningLock.Unlock()
		if err := c.runCloudSyncOnceSerialized(ctx, resyncLog, false); err != nil {
			c.cloudSyncRunningLock.Lock()
			c.cloudSyncRunning = false
			c.cloudSyncRunningLock.Unlock()
			resyncLog.Warn().Err(err).Msg("Delayed incremental re-sync failed")
			continue
		}
		// Extend skipPortals with any portals deleted since bootstrap.
		// The delayed re-sync may have imported new cloud_message records
		// for these portals (deleted=FALSE). Soft-delete them now so they
		// don't persist in the DB and resurrect the portal on next restart.
		c.recentlyDeletedPortalsMu.RLock()
		for portalID := range c.recentlyDeletedPortals {
			if !skipPortals[portalID] {
				skipPortals[portalID] = true
				if err := c.cloudStore.deleteLocalChatByPortalID(ctx, portalID); err != nil {
					resyncLog.Warn().Err(err).Str("portal_id", portalID).
						Msg("Failed to soft-delete re-imported records for recently deleted portal")
				} else {
					resyncLog.Debug().Str("portal_id", portalID).
						Msg("Soft-deleted re-imported records for portal deleted after bootstrap")
				}
			}
		}
		c.recentlyDeletedPortalsMu.RUnlock()
		// Pre-upload any new attachments discovered by this re-sync;
		// already-cached record_names are skipped instantly.
		c.preUploadCloudAttachments(ctx)
		c.createPortalsFromCloudSync(ctx, resyncLog, skipPortals)
		c.cloudSyncRunningLock.Lock()
		c.cloudSyncRunning = false
		c.cloudSyncRunningLock.Unlock()
	}
}

// runPostSyncHousekeeping cleans up accumulated dead data after a successful
// CloudKit sync. Each operation is independent and best-effort — failures are
// logged but don't block the bridge.
func (c *IMClient) runPostSyncHousekeeping(ctx context.Context, log zerolog.Logger) {
	if c.cloudStore == nil {
		return
	}
	log = log.With().Str("component", "housekeeping").Logger()

	// Prune cloud_attachment_cache entries not referenced by any live message.
	if pruned, err := c.cloudStore.pruneOrphanedAttachmentCache(ctx); err != nil {
		log.Warn().Err(err).Msg("Failed to prune orphaned attachment cache")
	} else if pruned > 0 {
		log.Info().Int64("pruned", pruned).Msg("Pruned orphaned attachment cache entries")
	}

	// Delete cloud_message rows whose portal_id has no cloud_chat entry.
	if deleted, err := c.cloudStore.deleteOrphanedMessages(ctx); err != nil {
		log.Warn().Err(err).Msg("Failed to delete orphaned cloud messages")
	} else if deleted > 0 {
		log.Info().Int64("deleted", deleted).Msg("Deleted orphaned cloud_message rows (no matching cloud_chat)")
	}
}

func (c *IMClient) runCloudSyncOnceSerialized(ctx context.Context, log zerolog.Logger, isBootstrap bool) error {
	c.cloudSyncRunMu.Lock()
	defer c.cloudSyncRunMu.Unlock()
	return c.runCloudSyncOnce(ctx, log, isBootstrap)
}

// runCloudSyncOnce performs a single CloudKit sync pass. On the first run
// (isBootstrap=true) it detects fresh vs. interrupted state and clears stale
// data if needed. On subsequent runs it's purely incremental — the saved
// continuation tokens mean CloudKit only returns changes since last sync.
func (c *IMClient) runCloudSyncOnce(ctx context.Context, log zerolog.Logger, isBootstrap bool) error {
	if isBootstrap {
		isFresh := false
		hasOwnPortal := false
		if portals, err := c.Main.Bridge.GetAllPortalsWithMXID(ctx); err == nil {
			for _, p := range portals {
				if p.Receiver == c.UserLogin.ID {
					hasOwnPortal = true
					break
				}
			}
		}

		if !hasOwnPortal {
			hasSyncState := false
			if has, err := c.cloudStore.hasAnySyncState(ctx); err == nil {
				hasSyncState = has
			}
			if !hasSyncState {
				// No portals AND no sync state — truly fresh. Clear everything.
				if err := c.cloudStore.clearAllData(ctx); err != nil {
					log.Warn().Err(err).Msg("Failed to clear stale cloud data")
				} else {
					log.Info().Msg("Fresh database detected, cleared cloud cache for full bootstrap")
				}
				isFresh = true
			} else {
				// Sync state exists but no portals. Two sub-cases:
				// (a) CloudKit data download was interrupted → resume from saved tokens
				// (b) Data download completed but portal creation was interrupted
				//     → backfill returns ~0 new records, portal creation re-runs
				// Both cases are handled correctly: backfill resumes or no-ops,
				// then createPortalsFromCloudSync reads from the DB tables.
				chatTok, _ := c.cloudStore.getSyncState(ctx, cloudZoneChats)
				msgTok, _ := c.cloudStore.getSyncState(ctx, cloudZoneMessages)
				log.Info().
					Bool("has_chat_token", chatTok != nil).
					Bool("has_msg_token", msgTok != nil).
					Msg("Resuming interrupted CloudKit sync (sync state exists but no portals yet)")
			}
		} else {
			// Existing portals — check sync version. If the sync logic has
			// been upgraded (e.g. better PCS key handling), do a one-time
			// full re-sync to pick up previously-undecryptable records.
			// Subsequent restarts use incremental sync (cheap).
			savedVersion, _ := c.cloudStore.getSyncVersion(ctx)
			if savedVersion < cloudSyncVersion {
				if err := c.cloudStore.clearSyncTokens(ctx); err != nil {
					log.Warn().Err(err).Msg("Failed to clear sync tokens for version upgrade re-sync")
				} else {
					log.Info().
						Int("old_version", savedVersion).
						Int("new_version", cloudSyncVersion).
						Msg("Sync version upgraded — cleared tokens for one-time full re-sync")
				}
			} else {
				log.Info().Int("sync_version", savedVersion).Msg("Sync version current — using incremental CloudKit sync")

				// Check chat-specific sync version. A bump here only clears
				// the chatManateeZone token, leaving the (expensive) message
				// token intact. Used for changes that only require re-fetching
				// chat metadata (e.g. populating group_photo_guid).
				savedChatVersion, _ := c.cloudStore.getChatSyncVersion(ctx)
				if savedChatVersion < cloudChatSyncVersion {
					if err := c.cloudStore.clearZoneToken(ctx, cloudZoneChats); err != nil {
						log.Warn().Err(err).Msg("Failed to clear chat zone token for chat sync version upgrade")
					} else {
						log.Info().
							Int("old_version", savedChatVersion).
							Int("new_version", cloudChatSyncVersion).
							Msg("Chat sync version upgraded — cleared chat zone token for one-time full chat re-sync")
					}
				}
			}
		}

		log.Info().Bool("fresh", isFresh).Msg("CloudKit sync start")
	}

	backfillStart := time.Now()
	counts, err := c.runCloudKitBackfill(ctx, log)
	if err != nil {
		return fmt.Errorf("CloudKit sync failed after %s: %w", time.Since(backfillStart).Round(time.Second), err)
	}

	log.Info().
		Int("imported", counts.Imported).
		Int("updated", counts.Updated).
		Int("skipped", counts.Skipped).
		Int("deleted", counts.Deleted).
		Bool("bootstrap", isBootstrap).
		Dur("elapsed", time.Since(backfillStart)).
		Msg("CloudKit sync pass complete")

	// Only persist sync version if the sync actually received data.
	// If the re-sync returned 0 records (e.g. CloudKit changes feed
	// considers records already delivered), leave the version unsaved
	// so the next restart will clear tokens and try again.
	totalRecords := counts.Imported + counts.Updated + counts.Deleted
	if isBootstrap && totalRecords > 0 {
		if err := c.cloudStore.setSyncVersion(ctx, cloudSyncVersion); err != nil {
			log.Warn().Err(err).Msg("Failed to persist sync version")
		}
		if err := c.cloudStore.setChatSyncVersion(ctx, cloudChatSyncVersion); err != nil {
			log.Warn().Err(err).Msg("Failed to persist chat sync version")
		}
	} else if !isBootstrap {
		// For existing portals, always persist the chat sync version so that
		// a targeted chat re-sync (cloudChatSyncVersion bump) is only done once.
		if err := c.cloudStore.setChatSyncVersion(ctx, cloudChatSyncVersion); err != nil {
			log.Warn().Err(err).Msg("Failed to persist chat sync version")
		}
	} else if isBootstrap && totalRecords == 0 {
		log.Warn().
			Int("sync_version", cloudSyncVersion).
			Msg("CloudKit sync returned 0 records — NOT saving version (will retry on next restart)")
		// (Previously ran CloudDiagFullCount here as a diagnostic. Removed:
		// it spans hundreds of anisette-authed pages and tripped an
		// upstream `panic!()` in omnisette's remote_anisette_v3 that, when
		// unwound across the FFI boundary, left the shared anisette
		// tokio::sync::Mutex in an inconsistent state and deadlocked every
		// subsequent operation that needed anisette — including message
		// send. The diagnostic was non-essential; dropping it entirely
		// avoids the trigger.)
	}

	return nil
}

func (c *IMClient) runCloudKitBackfill(ctx context.Context, log zerolog.Logger) (cloudSyncCounters, error) {
	var total cloudSyncCounters
	backfillStart := time.Now()

	// Always restart attachment sync from scratch. The attachment map
	// (attMap) is built in-memory during sync and used to enrich messages.
	// On crash/restart the map is lost, so resuming from a saved token
	// would produce an incomplete map — messages referencing pre-crash
	// attachments would lose their metadata. Attachment zones are small
	// relative to messages, so the overhead is minimal.
	if err := c.cloudStore.clearZoneToken(ctx, cloudZoneAttachments); err != nil {
		log.Warn().Err(err).Msg("Failed to clear attachment zone token for fresh sync")
	}

	// Check which zones have saved continuation tokens (for diagnostic logging).
	savedChatTok, _ := c.cloudStore.getSyncState(ctx, cloudZoneChats)
	savedMsgTok, _ := c.cloudStore.getSyncState(ctx, cloudZoneMessages)
	log.Info().
		Bool("chat_token_saved", savedChatTok != nil).
		Bool("msg_token_saved", savedMsgTok != nil).
		Msg("CloudKit backfill starting (attachment zone always fresh)")

	// Phase 0: Sync attachments first (with ALL_ASSETS) to populate the
	// Ford key cache before any downloads happen. MMCS deduplication can
	// serve Ford blobs encrypted with a different record's key, so the
	// cache must be fully populated before downloads start.
	phase0Start := time.Now()
	var attMap map[string]cloudAttachmentRow
	var attToken *string
	var attErr error
	{
		attMap, attToken, attErr = c.syncCloudAttachments(ctx)
		attCount := 0
		if attMap != nil {
			attCount = len(attMap)
		}
		log.Info().
			Dur("elapsed", time.Since(phase0Start)).
			Int("attachments", attCount).
			Err(attErr).
			Msg("CloudKit attachment sync complete (Ford key cache populated)")
	}

	// Phase 1: Sync chats. Messages depend on chats (portal ID resolution)
	// and attachments (GUID→record_name mapping), so they must wait.
	phase1Start := time.Now()

	var chatCounts cloudSyncCounters
	var chatToken *string
	var chatErr error
	{
		chatStart := time.Now()
		chatCounts, chatToken, chatErr = c.syncCloudChats(ctx)
		log.Info().
			Dur("elapsed", time.Since(chatStart)).
			Int("imported", chatCounts.Imported).
			Int("updated", chatCounts.Updated).
			Int("skipped", chatCounts.Skipped).
			Err(chatErr).
			Msg("CloudKit chat sync complete")
	}
	log.Info().Dur("phase1_elapsed", time.Since(phase1Start)).Msg("CloudKit phase 1 (chats) complete")

	if chatErr != nil {
		_ = c.cloudStore.setSyncStateError(ctx, cloudZoneChats, chatErr.Error())
		return total, chatErr
	}
	// Only persist the chat token if non-nil — a nil token from a "no changes"
	// response would overwrite the saved watermark and force a full re-download
	// on the next restart.
	if chatToken != nil {
		if err := c.cloudStore.setSyncStateSuccess(ctx, cloudZoneChats, chatToken); err != nil {
			log.Warn().Err(err).Msg("Failed to persist chat sync token")
		}
	}
	total.add(chatCounts)

	if attErr != nil {
		log.Warn().Err(attErr).Msg("Failed to sync CloudKit attachments (continuing without)")
	} else if attToken != nil {
		if err := c.cloudStore.setSyncStateSuccess(ctx, cloudZoneAttachments, attToken); err != nil {
			log.Warn().Err(err).Msg("Failed to persist attachment sync token")
		}
	}

	// Phase 2: Sync messages (depends on chats + attachments).
	phase2Start := time.Now()
	msgCounts, msgToken, err := c.syncCloudMessages(ctx, attMap)
	if err != nil {
		_ = c.cloudStore.setSyncStateError(ctx, cloudZoneMessages, err.Error())
		return total, err
	}
	if msgToken != nil {
		if err = c.cloudStore.setSyncStateSuccess(ctx, cloudZoneMessages, msgToken); err != nil {
			log.Warn().Err(err).Msg("Failed to persist message sync token")
		}
	}
	total.add(msgCounts)

	log.Info().
		Dur("phase2_elapsed", time.Since(phase2Start)).
		Int("imported", msgCounts.Imported).
		Int("updated", msgCounts.Updated).
		Int("skipped", msgCounts.Skipped).
		Dur("total_elapsed", time.Since(backfillStart)).
		Msg("CloudKit phase 2 (messages) complete")

	return total, nil
}

// syncCloudAttachments syncs the attachment zone and builds a GUID→attachment info map.
func (c *IMClient) syncCloudAttachments(ctx context.Context) (map[string]cloudAttachmentRow, *string, error) {
	attMap := make(map[string]cloudAttachmentRow)
	token, err := c.cloudStore.getSyncState(ctx, cloudZoneAttachments)
	if err != nil {
		return attMap, nil, err
	}

	log := c.Main.Bridge.Log.With().Str("component", "cloud_sync").Logger()
	consecutiveErrors := 0
	const maxConsecutiveAttErrors = 3
	for page := 0; page < maxCloudSyncPages; page++ {
		resp, syncErr := safeCloudSyncAttachments(c.client, token)
		if syncErr != nil {
			consecutiveErrors++
			retryDelay := cloudKitRetryDelay(syncErr, consecutiveErrors)
			log.Warn().Err(syncErr).
				Int("page", page).
				Int("imported_so_far", len(attMap)).
				Int("consecutive_errors", consecutiveErrors).
				Dur("retry_after", retryDelay).
				Msg("CloudKit attachment sync page failed (FFI error)")
			if consecutiveErrors >= maxConsecutiveAttErrors {
				log.Error().
					Int("page", page).
					Int("imported_so_far", len(attMap)).
					Msg("CloudKit attachment sync: too many consecutive FFI errors, stopping pagination")
				break
			}
			time.Sleep(retryDelay)
			continue
		}
		consecutiveErrors = 0

		for _, att := range resp.Attachments {
			mime := ""
			if att.MimeType != nil {
				mime = *att.MimeType
			}
			uti := ""
			if att.UtiType != nil {
				uti = *att.UtiType
			}
			filename := ""
			if att.Filename != nil {
				filename = *att.Filename
			}
			attMap[att.Guid] = cloudAttachmentRow{
				GUID:       att.Guid,
				MimeType:   mime,
				UTIType:    uti,
				Filename:   filename,
				FileSize:   att.FileSize,
				RecordName: att.RecordName,
				HasAvid:    att.HasAvid,
			}

			// Populate the Ford key cache from this record's
			// PCS-decrypted protection_info. Mirrors the sync-side
			// half of the 94f7b8e Ford dedup fix — every video's
			// Ford key is available for future cross-batch lookup,
			// regardless of upload order. Registered on BOTH the
			// Go cache (used by Go-side lookups and tests) AND the
			// wrapper's process-wide cache (used by the Ford
			// recovery path inside cloud_download_attachment when
			// an SIV panic is caught).
			if att.FordKey != nil {
				if c.fordCache != nil {
					c.fordCache.Register(*att.FordKey)
				}
				rustpushgo.RegisterFordKey(*att.FordKey)
			}
			if att.AvidFordKey != nil {
				if c.fordCache != nil {
					c.fordCache.Register(*att.AvidFordKey)
				}
				rustpushgo.RegisterFordKey(*att.AvidFordKey)
			}
		}

		prev := ptrStringOr(token, "")
		token = resp.ContinuationToken

		// Persist token after each page for crash-safe resume.
		if token != nil {
			if saveErr := c.cloudStore.setSyncStateSuccess(ctx, cloudZoneAttachments, token); saveErr != nil {
				log.Warn().Err(saveErr).Int("page", page).Msg("Failed to persist attachment sync token mid-page")
			}
		}

		if resp.Done || (page > 0 && prev == ptrStringOr(token, "")) {
			break
		}
	}

	// QueryRecords fallback: query attachmentManateeZone directly for records
	// the FetchRecordChanges feed missed. Pass all record_names already in attMap
	// so Rust only returns genuinely new records.
	knownNames := make([]string, 0, len(attMap))
	for _, row := range attMap {
		knownNames = append(knownNames, row.RecordName)
	}
	if fallback, fallbackErr := safeCloudQueryAttachmentsFallback(c.client, knownNames); fallbackErr != nil {
		log.Warn().Err(fallbackErr).Msg("CloudKit attachment QueryRecords fallback failed")
	} else {
		for _, att := range fallback.Attachments {
			mime := ""
			if att.MimeType != nil {
				mime = *att.MimeType
			}
			uti := ""
			if att.UtiType != nil {
				uti = *att.UtiType
			}
			filename := ""
			if att.Filename != nil {
				filename = *att.Filename
			}
			attMap[att.Guid] = cloudAttachmentRow{
				GUID:       att.Guid,
				MimeType:   mime,
				UTIType:    uti,
				Filename:   filename,
				FileSize:   att.FileSize,
				RecordName: att.RecordName,
				HasAvid:    att.HasAvid,
			}
			if att.FordKey != nil {
				if c.fordCache != nil {
					c.fordCache.Register(*att.FordKey)
				}
				rustpushgo.RegisterFordKey(*att.FordKey)
			}
			if att.AvidFordKey != nil {
				if c.fordCache != nil {
					c.fordCache.Register(*att.AvidFordKey)
				}
				rustpushgo.RegisterFordKey(*att.AvidFordKey)
			}
		}
		if len(fallback.Attachments) > 0 {
			log.Info().Int("count", len(fallback.Attachments)).Msg("CloudKit attachment QueryRecords fallback added records")
		}
	}

	return attMap, token, nil
}

func (c *IMClient) syncCloudChats(ctx context.Context) (cloudSyncCounters, *string, error) {
	var counts cloudSyncCounters
	token, err := c.cloudStore.getSyncState(ctx, cloudZoneChats)
	if err != nil {
		return counts, nil, err
	}

	log := c.Main.Bridge.Log.With().Str("component", "cloud_sync").Logger()
	totalPages := 0
	consecutiveErrors := 0
	const maxConsecutiveChatErrors = 3
	for page := 0; page < maxCloudSyncPages; page++ {
		resp, syncErr := safeCloudSyncChats(c.client, token)
		if syncErr != nil {
			consecutiveErrors++
			retryDelay := cloudKitRetryDelay(syncErr, consecutiveErrors)
			log.Warn().Err(syncErr).
				Int("page", page).
				Int("imported_so_far", counts.Imported).
				Int("consecutive_errors", consecutiveErrors).
				Dur("retry_after", retryDelay).
				Msg("CloudKit chat sync page failed (FFI error)")
			if consecutiveErrors >= maxConsecutiveChatErrors {
				log.Error().
					Int("page", page).
					Int("imported_so_far", counts.Imported).
					Msg("CloudKit chat sync: too many consecutive FFI errors, stopping pagination")
				break
			}
			time.Sleep(retryDelay)
			continue
		}
		consecutiveErrors = 0

		log.Info().
			Int("page", page).
			Int("chats_on_page", len(resp.Chats)).
			Int32("status", resp.Status).
			Bool("done", resp.Done).
			Msg("CloudKit chat sync page")

		ingestCounts, ingestErr := c.ingestCloudChats(ctx, resp.Chats)
		if ingestErr != nil {
			return counts, token, ingestErr
		}
		counts.add(ingestCounts)
		totalPages = page + 1

		prev := ptrStringOr(token, "")
		token = resp.ContinuationToken

		// Persist token after each page for crash-safe resume.
		if token != nil {
			if saveErr := c.cloudStore.setSyncStateSuccess(ctx, cloudZoneChats, token); saveErr != nil {
				log.Warn().Err(saveErr).Int("page", page).Msg("Failed to persist chat sync token mid-page")
			}
		}

		if resp.Done || (page > 0 && prev == ptrStringOr(token, "")) {
			log.Info().Int("page", page).Bool("api_done", resp.Done).Bool("token_unchanged", prev == ptrStringOr(token, "")).
				Msg("CloudKit chat sync pagination stopped")
			break
		}
	}

	log.Info().Int("total_pages", totalPages).Int("imported", counts.Imported).Int("updated", counts.Updated).
		Int("skipped", counts.Skipped).Int("deleted", counts.Deleted).Int("filtered", counts.Filtered).
		Msg("CloudKit chat sync finished")

	return counts, token, nil
}

// safeFFICall wraps an FFI call with panic recovery.
// UniFFI deserialization panics on malformed buffers; this prevents bridge crashes.
func safeFFICall[T any](name string, fn func() (T, error)) (result T, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Error().Str("ffi_method", name).Str("stack", string(debug.Stack())).Msgf("FFI panic recovered: %v", r)
			err = fmt.Errorf("FFI panic in %s: %v", name, r)
		}
	}()
	return fn()
}

func safeCloudSyncMessages(client *rustpushgo.Client, token *string) (rustpushgo.WrappedCloudSyncMessagesPage, error) {
	return safeFFICall("CloudSyncMessages", func() (rustpushgo.WrappedCloudSyncMessagesPage, error) {
		return client.CloudSyncMessages(token)
	})
}

func safeCloudSyncChats(client *rustpushgo.Client, token *string) (rustpushgo.WrappedCloudSyncChatsPage, error) {
	return safeFFICall("CloudSyncChats", func() (rustpushgo.WrappedCloudSyncChatsPage, error) {
		return client.CloudSyncChats(token)
	})
}

func safeCloudSyncAttachments(client *rustpushgo.Client, token *string) (rustpushgo.WrappedCloudSyncAttachmentsPage, error) {
	return safeFFICall("CloudSyncAttachments", func() (rustpushgo.WrappedCloudSyncAttachmentsPage, error) {
		return client.CloudSyncAttachments(token)
	})
}

func safeCloudQueryAttachmentsFallback(client *rustpushgo.Client, knownRecordNames []string) (rustpushgo.WrappedCloudSyncAttachmentsPage, error) {
	return safeFFICall("CloudQueryAttachmentsFallback", func() (rustpushgo.WrappedCloudSyncAttachmentsPage, error) {
		return client.CloudQueryAttachmentsFallback(knownRecordNames)
	})
}

func (c *IMClient) syncCloudMessages(ctx context.Context, attMap map[string]cloudAttachmentRow) (cloudSyncCounters, *string, error) {
	var counts cloudSyncCounters
	token, err := c.cloudStore.getSyncState(ctx, cloudZoneMessages)
	if err != nil {
		return counts, nil, err
	}

	log := c.Main.Bridge.Log.With().Str("component", "cloud_sync").Logger()
	log.Info().
		Bool("token_nil", token == nil).
		Msg("CloudKit message sync starting")

	consecutiveErrors := 0
	const maxConsecutiveErrors = 3
	totalPages := 0
	for page := 0; page < maxCloudSyncPages; page++ {
		resp, syncErr := safeCloudSyncMessages(c.client, token)
		if syncErr != nil {
			consecutiveErrors++
			retryDelay := cloudKitRetryDelay(syncErr, consecutiveErrors)
			log.Warn().Err(syncErr).
				Int("page", page).
				Int("imported_so_far", counts.Imported).
				Int("consecutive_errors", consecutiveErrors).
				Dur("retry_after", retryDelay).
				Msg("CloudKit message sync page failed (FFI error)")
			if consecutiveErrors >= maxConsecutiveErrors {
				log.Error().
					Int("page", page).
					Int("imported_so_far", counts.Imported).
					Msg("CloudKit message sync: too many consecutive FFI errors, stopping pagination")
				break
			}
			// Skip this page and retry with the same token on next iteration.
			// The server may return different records on retry.
			time.Sleep(retryDelay)
			continue
		}
		consecutiveErrors = 0

		log.Info().
			Int("page", page).
			Int("messages", len(resp.Messages)).
			Int32("status", resp.Status).
			Bool("done", resp.Done).
			Bool("has_token", resp.ContinuationToken != nil).
			Msg("CloudKit message sync page")

		if err = c.ingestCloudMessages(ctx, resp.Messages, "", &counts, attMap); err != nil {
			return counts, token, err
		}

		prev := ptrStringOr(token, "")
		token = resp.ContinuationToken
		totalPages = page + 1

		// Only persist the continuation token if records were received.
		// Persisting an empty-page token on a 0-record re-sync would
		// prevent future retries from starting fresh.
		if token != nil && len(resp.Messages) > 0 {
			if saveErr := c.cloudStore.setSyncStateSuccess(ctx, cloudZoneMessages, token); saveErr != nil {
				log.Warn().Err(saveErr).Int("page", page).Msg("Failed to persist message sync token mid-page")
			}
		}

		if resp.Done || (page > 0 && prev == ptrStringOr(token, "")) {
			log.Info().
				Int("page", page).
				Bool("api_done", resp.Done).
				Bool("token_unchanged", prev == ptrStringOr(token, "")).
				Msg("CloudKit message sync pagination stopped")
			break
		}
	}

	log.Info().
		Int("total_pages", totalPages).
		Int("imported", counts.Imported).
		Int("updated", counts.Updated).
		Int("skipped", counts.Skipped).
		Int("deleted", counts.Deleted).
		Int("filtered", counts.Filtered).
		Msg("CloudKit message sync finished")

	return counts, token, nil
}

func (c *IMClient) ingestCloudChats(ctx context.Context, chats []rustpushgo.WrappedCloudSyncChat) (cloudSyncCounters, error) {
	var counts cloudSyncCounters
	log := c.Main.Bridge.Log.With().Str("component", "cloud_sync").Logger()

	// Batch existence check for all non-deleted chat IDs.
	chatIDs := make([]string, 0, len(chats))
	for _, chat := range chats {
		if !chat.Deleted {
			chatIDs = append(chatIDs, chat.CloudChatId)
		}
	}
	existingSet, err := c.cloudStore.hasChatBatch(ctx, chatIDs)
	if err != nil {
		return counts, fmt.Errorf("batch chat existence check failed: %w", err)
	}

	// Collect deleted record_names for tombstone handling.
	// Tombstones only carry the record_name (CloudChatId is set to
	// record_name by the FFI layer since there's no data to extract
	// the real chat identifier). We must look up by record_name.
	var deletedRecordNames []string

	// Collect portal IDs for chats that have a group photo GUID so we can
	// warm the photo cache after upserting the batch.
	var photoPortalIDs []string

	// Build batch of rows.
	batch := make([]cloudChatUpsertRow, 0, len(chats))
	for _, chat := range chats {
		if chat.Deleted {
			counts.Deleted++
			deletedRecordNames = append(deletedRecordNames, chat.RecordName)
			continue
		}

		portalID := c.resolvePortalIDForCloudChat(chat.Participants, chat.DisplayName, chat.GroupId, chat.Style)
		if portalID == "" {
			counts.Skipped++
			continue
		}

		// Update SMS flag from CloudKit chat service type. If a stale
		// CloudKit record briefly clears the flag for a legitimately-SMS
		// portal, the next live SMS message will re-set it immediately
		// via handleMessage's unconditional updatePortalSMS call.
		if strings.EqualFold(chat.Service, "SMS") {
			c.updatePortalSMS(portalID, true)
		} else if strings.EqualFold(chat.Service, "iMessage") {
			c.updatePortalSMS(portalID, false)
		}

		participantsJSON, jsonErr := json.Marshal(chat.Participants)
		if jsonErr != nil {
			return counts, jsonErr
		}

		if chat.GroupPhotoGuid != nil && *chat.GroupPhotoGuid != "" {
			log.Info().
				Str("portal_id", portalID).
				Str("record_name", chat.RecordName).
				Str("group_photo_guid", *chat.GroupPhotoGuid).
				Msg("CloudKit chat sync: group photo GUID found")
			photoPortalIDs = append(photoPortalIDs, portalID)
		}

		batch = append(batch, cloudChatUpsertRow{
			CloudChatID:      chat.CloudChatId,
			RecordName:       chat.RecordName,
			GroupID:          chat.GroupId,
			PortalID:         portalID,
			Service:          chat.Service,
			DisplayName:      nullableString(chat.DisplayName),
			GroupPhotoGuid:   nullableString(chat.GroupPhotoGuid),
			ParticipantsJSON: string(participantsJSON),
			UpdatedTS:        int64(chat.UpdatedTimestampMs),
			IsFiltered:       chat.IsFiltered,
		})

		if chat.IsFiltered != 0 {
			counts.Filtered++
			log.Debug().
				Str("cloud_chat_id", chat.CloudChatId).
				Str("portal_id", portalID).
				Int64("is_filtered", chat.IsFiltered).
				Msg("Stored filtered/junk chat from CloudKit (will skip messages)")
		} else if existingSet[chat.CloudChatId] {
			counts.Updated++
		} else {
			counts.Imported++
		}
	}

	// Batch insert all non-deleted chats. The upsert's ON CONFLICT clause
	// preserves the existing deleted flag, so soft-deleted (Beeper-deleted)
	// chats stay deleted through CloudKit re-sync. Only explicit recovery
	// (RecoverChat, !restore-chat) un-deletes via undeleteCloudChatByPortalID.
	if err := c.cloudStore.upsertChatBatch(ctx, batch); err != nil {
		return counts, err
	}

	// Remove un-tombstoned portals from recentlyDeletedPortals — but only
	// tombstone entries (seeded from recycle bin). Beeper-initiated deletes
	// (isTombstone=false) must NOT be cleared here: the CloudKit record may
	// still exist because the async CloudKit deletion hasn't completed yet.
	// Clearing the in-memory block prematurely lets createPortalsFromCloudSync
	// resurrect the portal as a zombie.
	var tombstonesCleared []string
	c.recentlyDeletedPortalsMu.Lock()
	for _, row := range batch {
		if entry, ok := c.recentlyDeletedPortals[row.PortalID]; ok && entry.isTombstone {
			delete(c.recentlyDeletedPortals, row.PortalID)
			tombstonesCleared = append(tombstonesCleared, row.PortalID)
		}
	}
	c.recentlyDeletedPortalsMu.Unlock()

	// For each tombstone portal that CloudKit just confirmed is live, also
	// clear the deleted=TRUE flag in the DB. upsertChatBatch preserves
	// cloud_chat.deleted intentionally for Beeper-initiated deletes, but
	// tombstone portals that re-appear as live should be fully undeleted so
	// ingestCloudMessages/portalHasChat doesn't silently filter their messages.
	for _, portalID := range tombstonesCleared {
		if err := c.cloudStore.undeleteCloudChatByPortalID(ctx, portalID); err != nil {
			log.Warn().Err(err).Str("portal_id", portalID).
				Msg("Failed to undelete tombstone portal DB row after live CloudKit record received")
		} else {
			log.Info().Str("portal_id", portalID).
				Msg("Tombstone portal confirmed live by CloudKit — undeleted in DB")
		}
	}

	// Proactively warm the group_photo_cache for chats that have a photo GUID
	// but no cached bytes yet. This ensures GetChatInfo can serve the avatar
	// immediately on first portal open without waiting for an APNs IconChange.
	// Runs in a background goroutine so it doesn't block the sync sweep.
	// Each download attempt is best-effort: failures are logged at debug level
	// since the CloudKit "gp" asset field is often unpopulated by Apple clients.
	if len(photoPortalIDs) > 0 {
		bgCtx, cancelBg := context.WithCancel(context.Background())
		// Wire bgCtx to the client lifecycle: cancel it when the client
		// disconnects. The warmup goroutine also cancels on exit via defer,
		// so this watcher exits promptly in either case.
		go func() {
			select {
			case <-c.stopChan:
				cancelBg()
			case <-bgCtx.Done():
			}
		}()
		portalIDs := photoPortalIDs
		go func() {
			defer cancelBg()
			photoLog := log.With().Str("component", "cloud_photo_warmup").Logger()
			for _, portalID := range portalIDs {
				_, existing, cacheErr := c.cloudStore.getGroupPhoto(bgCtx, portalID)
				if cacheErr == nil && len(existing) > 0 {
					continue // already cached
				}
				c.fetchAndCacheGroupPhoto(bgCtx,
					photoLog.With().Str("portal_id", portalID).Logger(),
					portalID)
			}
		}()
	}

	// Handle tombstoned (deleted) chats. Tombstones only carry the
	// record_name, so we look up portal_ids from the local cloud_chat
	// table, then:
	// 1. Soft-delete cloud_chat rows (deleted=TRUE) — preserves data for
	//    portalHasChat checks and potential restore-chat
	// 2. Soft-delete cloud_message rows (deleted=TRUE) — preserves UUIDs
	//    for echo detection via hasMessageUUID
	// 3. Queue bridge portal deletion for any existing portal
	// 4. Mark in recentlyDeletedPortals to block APNs echo resurrection
	if len(deletedRecordNames) > 0 {
		// Resolve record_names → portal_ids before soft-deleting.
		portalMap, lookupErr := c.cloudStore.lookupPortalIDsByRecordNames(ctx, deletedRecordNames)
		if lookupErr != nil {
			log.Warn().Err(lookupErr).Msg("Failed to look up portal IDs for tombstoned chats")
		}

		// Soft-delete cloud_chat entries by record_name (correct for tombstones).
		if err := c.cloudStore.deleteChatsByRecordNames(ctx, deletedRecordNames); err != nil {
			return counts, fmt.Errorf("failed to soft-delete tombstoned chats by record_name: %w", err)
		}

		// Soft-delete cloud_message rows for resolved portal_ids.
		// The cloud_chat rows were already soft-deleted above by record_name.
		// deleteLocalChatByPortalID sets deleted=TRUE on both tables (idempotent
		// for cloud_chat, but catches cloud_message rows too). UUIDs are preserved
		// so hasMessageUUID can still detect stale APNs echoes.
		for recordName, portalID := range portalMap {
			if err := c.cloudStore.deleteLocalChatByPortalID(ctx, portalID); err != nil {
				log.Warn().Err(err).Str("portal_id", portalID).Msg("Failed to soft-delete messages for tombstoned chat")
			}

			// Mark as deleted in memory so createPortalsFromCloudSync skips
			// this portal and FetchMessages returns empty.
			c.recentlyDeletedPortalsMu.Lock()
			if c.recentlyDeletedPortals == nil {
				c.recentlyDeletedPortals = make(map[string]deletedPortalEntry)
			}
			c.recentlyDeletedPortals[portalID] = deletedPortalEntry{
				deletedAt:   time.Now(),
				isTombstone: true,
			}
			c.recentlyDeletedPortalsMu.Unlock()

			// Queue bridge portal deletion if it exists.
			portalKey := networkid.PortalKey{
				ID:       networkid.PortalID(portalID),
				Receiver: c.UserLogin.ID,
			}
			existing, _ := c.Main.Bridge.GetExistingPortalByKey(ctx, portalKey)
			if existing != nil && existing.MXID != "" {
				log.Info().
					Str("portal_id", portalID).
					Str("record_name", recordName).
					Msg("CloudKit tombstone: deleting bridge portal for chat in recycle bin")
				c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.ChatDelete{
					EventMeta: simplevent.EventMeta{
						Type:      bridgev2.RemoteEventChatDelete,
						PortalKey: portalKey,
						Timestamp: time.Now(),
						LogContext: func(lc zerolog.Context) zerolog.Context {
							return lc.Str("source", "cloudkit_tombstone").Str("record_name", recordName)
						},
					},
					OnlyForMe: true,
				})
			}
		}
	}

	return counts, nil
}

// uuidPattern matches a UUID string (xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx).
var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// hexGroupPattern matches bare hex strings of 40+ characters (SHA1 hashes
// used as CloudKit group identifiers for SMS groups).
var hexGroupPattern = regexp.MustCompile(`(?i)^[0-9a-f]{40,128}$`)

// isNumericSuffix returns true if s is non-empty and contains only ASCII digits.
// Used to identify CloudKit self-chat identifiers of the form "chat<digits>".
func isNumericSuffix(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// resolveConversationID determines the canonical portal ID for a cloud message.
//
// Rule 1: If chat_id is a UUID → it's a group conversation → "gid:<lowercase-uuid>"
// Rule 2: Otherwise derive from sender (DM) → "tel:+..." or "mailto:..."
// Rule 3: Messages create conversations. Never discard a message because
//
//	we haven't seen the chat record yet.
func (c *IMClient) resolveConversationID(ctx context.Context, msg rustpushgo.WrappedCloudSyncMessage) string {
	// Try to look up the chat record first. This is authoritative because
	// getChatPortalID matches on cloud_chat_id, record_name, AND group_id,
	// so it returns the correct gid:<group_id> even when the message's
	// chat_id UUID differs from the chat's group_id UUID.
	if msg.CloudChatId != "" {
		if portalID, err := c.cloudStore.getChatPortalID(ctx, msg.CloudChatId); err == nil && portalID != "" {
			return portalID
		}
	}

	// Fallback: if no chat record exists yet, a UUID chat_id means group.
	if msg.CloudChatId != "" && uuidPattern.MatchString(msg.CloudChatId) {
		return "gid:" + strings.ToLower(msg.CloudChatId)
	}

	// Fallback: bare hex hash (40+ chars) — SMS group identifiers use SHA1
	// hashes as their CloudKit chatID. These are always group chats.
	if msg.CloudChatId != "" && hexGroupPattern.MatchString(msg.CloudChatId) {
		return "gid:" + strings.ToLower(msg.CloudChatId)
	}

	// Group chat with non-UUID hex suffix format: "any;+;5851dc712a4b..." or
	// "iMessage;+;chat<hex>". The ";+;" separator marks a group (vs ";-;" for
	// DMs). Extract the suffix and use it as the gid: portal ID so group
	// messages (including is_from_me=true) don't leak to the self-chat portal.
	if msg.CloudChatId != "" && strings.Contains(msg.CloudChatId, ";+;") {
		parts := strings.SplitN(msg.CloudChatId, ";+;", 2)
		if len(parts) == 2 && parts[1] != "" {
			return "gid:" + strings.ToLower(parts[1])
		}
	}

	// DM: derive from sender
	if msg.Sender != "" && !msg.IsFromMe {
		normalized := normalizeIdentifierForPortalID(msg.Sender)
		if normalized != "" {
			resolved := c.resolveContactPortalID(normalized)
			resolved = c.resolveExistingDMPortalID(string(resolved))
			return string(resolved)
		}
	}

	// is_from_me DMs: derive from destination
	if msg.IsFromMe && msg.CloudChatId != "" {
		// chat_id for DMs is like "iMessage;-;+16692858317" or bare "email@example.com".
		var recipient string
		parts := strings.Split(msg.CloudChatId, ";")
		if len(parts) == 3 {
			// Standard service-prefixed format: "iMessage;-;recipient"
			recipient = parts[2]
		} else if !strings.Contains(msg.CloudChatId, ";") {
			// Bare chatId (no service prefix) — legacy format or recycle-bin
			// restored messages. Treat the whole CloudChatId as the recipient.
			recipient = msg.CloudChatId
		}
		if recipient != "" {
			normalized := normalizeIdentifierForPortalID(recipient)
			// Only use recipient if it's a real phone/email.
			// CloudKit uses "chat<numeric>" identifiers for self-chat, which
			// normalizeIdentifierForPortalID returns unchanged (no tel:/mailto:
			// prefix). Using those raw creates junk portals.
			if normalized != "" && (strings.HasPrefix(normalized, "tel:") || strings.HasPrefix(normalized, "mailto:")) {
				resolved := c.resolveContactPortalID(normalized)
				resolved = c.resolveExistingDMPortalID(string(resolved))
				return string(resolved)
			}
		}
	}

	// Self-chat fallback: is_from_me messages where the recipient couldn't be
	// determined. CloudKit uses unique "chat<numeric>" identifiers for self-chat
	// instead of the user's phone number, so the handler above can't extract a
	// valid recipient. Route to the user's own handle (Notes to Self portal).
	//
	// Guard: only fall back to self-chat when the CloudChatId looks like an
	// actual self-chat identifier (empty, or starts with "chat" + digits).
	// If it contains "@", ";", or matches UUID/phone patterns it belongs to a
	// remote DM or group — return "" so the message is skipped rather than
	// misrouted to self-chat. This prevents deleted/splintered remote-chat
	// messages from polluting the Notes to Self portal.
	if msg.IsFromMe {
		chatID := msg.CloudChatId
		// Require an explicit "chat<digits>" identifier to route to self-chat.
		// Empty chat_id is too broad — from-me messages from any conversation can
		// arrive with no chat_id and must be skipped rather than dumped into Notes
		// to Self. Genuine Notes-to-Self messages use Apple's "chat<digits>" format.
		isSelfChatID := strings.HasPrefix(strings.ToLower(chatID), "chat") && isNumericSuffix(chatID[4:])
		if !isSelfChatID {
			return ""
		}
		normalized := normalizeIdentifierForPortalID(c.handle)
		if normalized != "" {
			return normalized
		}
	}

	return ""
}

func (c *IMClient) ingestCloudMessages(
	ctx context.Context,
	messages []rustpushgo.WrappedCloudSyncMessage,
	preferredPortalID string,
	counts *cloudSyncCounters,
	attMap map[string]cloudAttachmentRow,
) error {
	log := c.Main.Bridge.Log.With().Str("component", "cloud_sync").Logger()

	// Separate deleted messages from live messages up front.
	// Deleted messages are removed from the DB (they may have been stored
	// in a previous sync before the user deleted them in iCloud).
	var deletedGUIDs []string
	var liveMessages []rustpushgo.WrappedCloudSyncMessage
	for _, msg := range messages {
		if msg.Deleted {
			counts.Deleted++
			if msg.Guid != "" {
				deletedGUIDs = append(deletedGUIDs, msg.Guid)
			}
			continue
		}
		liveMessages = append(liveMessages, msg)
	}

	// Remove deleted messages from DB.
	if err := c.cloudStore.deleteMessageBatch(ctx, deletedGUIDs); err != nil {
		return fmt.Errorf("failed to delete messages: %w", err)
	}

	// Snapshot recentlyDeletedPortals so we can mark re-imported messages for
	// deleted portals as deleted=TRUE. This closes the race where a periodic
	// re-sync downloads messages for a portal that was just deleted: without
	// this check, those messages would be inserted with deleted=FALSE and could
	// trigger portal resurrection or spurious backfill on a future restart.
	c.recentlyDeletedPortalsMu.RLock()
	deletedPortalsSnapshot := make(map[string]bool, len(c.recentlyDeletedPortals))
	for id := range c.recentlyDeletedPortals {
		deletedPortalsSnapshot[id] = true
	}
	c.recentlyDeletedPortalsMu.RUnlock()

	// Phase 1: Resolve portal IDs and build rows for live messages (no DB writes yet).
	guids := make([]string, 0, len(liveMessages))
	for _, msg := range liveMessages {
		if msg.Guid != "" {
			guids = append(guids, msg.Guid)
		}
	}
	existingSet, err := c.cloudStore.hasMessageBatch(ctx, guids)
	if err != nil {
		return fmt.Errorf("batch existence check failed: %w", err)
	}

	// Reverse index: message_guid -> []attachment_guid, derived from attMap.
	//
	// CloudAttachment.AttachmentMeta.guid (aguid) is always shaped
	// `at_<index>_<MESSAGE_GUID>` (see upstream rustpush
	// imessage/cloud_messages.rs:445, serde rename "aguid"), so we can
	// recover which message owns an attachment purely from the attachment
	// record's own metadata, without relying on the parent message's
	// attributedBody parse.
	//
	// This is the load-bearing fix for a class of silently-dropped
	// attachments: when the wrapper's extract_attachment_guids_from_attributed_body
	// returns [] (either because the body is in a format the
	// NSAttributedString decoder doesn't understand, or because
	// coder_decode_flattened panicked and got swallowed by catch_unwind),
	// the pre-fix code path skipped the entire enrichment block below and
	// wrote `attachments_json=""` into the DB — resulting in a Matrix event
	// with just a space character where an image should have been. By
	// merging in any attachments the aguid-suffix lookup finds, we recover
	// every attachment we actually have in attMap regardless of what the
	// NSAttributedString parser could see.
	attachmentsByMessage := make(map[string][]string, len(attMap))
	// Case-insensitive companion index for attMap lookups. Rust's
	// plist/serde decode preserves whatever casing Apple's backend wrote
	// into `cm.aguid`, while extract_attachment_guids_from_attributed_body
	// preserves whatever casing the NSAttributedString recorded. On some
	// records these diverge (upper-case UUID in the message, lower-case in
	// the attachment record, or vice versa), so a direct map lookup
	// misses even though both sides refer to the same attachment. Keyed
	// by strings.ToLower(aguid) → original aguid string used as attMap
	// key, so the fallback below can resolve either casing.
	attMapLowercased := make(map[string]string, len(attMap))
	for attGUID := range attMap {
		if !strings.HasPrefix(attGUID, "at_") {
			continue
		}
		attMapLowercased[strings.ToLower(attGUID)] = attGUID
		// Split at_<index>_<message_guid> by finding the second underscore.
		rest := attGUID[len("at_"):]
		underscore := strings.IndexByte(rest, '_')
		if underscore <= 0 || underscore == len(rest)-1 {
			continue
		}
		msgGUID := rest[underscore+1:]
		attachmentsByMessage[msgGUID] = append(attachmentsByMessage[msgGUID], attGUID)
		// ALSO index by the lowercased msg_guid suffix so attributedBody
		// parses that extract the UUID in the opposite case still hit.
		msgGUIDLower := strings.ToLower(msgGUID)
		if msgGUIDLower != msgGUID {
			attachmentsByMessage[msgGUIDLower] = append(attachmentsByMessage[msgGUIDLower], attGUID)
		}
	}

	batch := make([]cloudMessageRow, 0, len(liveMessages))
	for _, msg := range liveMessages {
		if msg.Guid == "" {
			log.Warn().
				Str("cloud_chat_id", msg.CloudChatId).
				Str("sender", msg.Sender).
				Bool("is_from_me", msg.IsFromMe).
				Int64("timestamp_ms", msg.TimestampMs).
				Msg("Skipping message with empty GUID")
			counts.Skipped++
			continue
		}

		// NOTE: msgType=0 is a REGULAR user message in CloudKit — do NOT filter
		// it. System/service messages are already filtered on the Rust side using
		// IS_SYSTEM_MESSAGE / IS_SERVICE_MESSAGE flags. A previous version of this
		// code incorrectly assumed msgType=0 was a system record and filtered all
		// regular messages, causing cloud_message to remain empty.

		portalID := c.resolveConversationID(ctx, msg)
		if portalID == "" {
			portalID = preferredPortalID
		}
		if portalID == "" {
			log.Warn().
				Str("guid", msg.Guid).
				Str("cloud_chat_id", msg.CloudChatId).
				Str("sender", msg.Sender).
				Bool("is_from_me", msg.IsFromMe).
				Int64("timestamp_ms", msg.TimestampMs).
				Str("service", msg.Service).
				Msg("Skipping message: could not resolve portal ID")
			counts.Skipped++
			continue
		}

		// Skip messages for portals that have been explicitly deleted/tombstoned.
		// A cloud_chat row with deleted=TRUE means the user or Apple deleted this
		// conversation. CloudKit re-sync always re-delivers stale messages for
		// deleted chats; we must NOT revive them here — only explicit recovery
		// (RecoverChat APNs, !restore-chat) should un-delete a portal.
		//
		// Portals with NO cloud_chat row at all are allowed through when the
		// message has a non-empty CloudChatId. This covers recycle-bin-only chats
		// on fresh backfill: the chat was deleted before the bridge's first sync
		// so it never appeared in the main chat zone, but its messages are still
		// in the main message zone. seedDeletedChatsFromRecycleBin (runs after
		// this sync) seeds live cloud_chat rows for them.
		if c.cloudStore != nil {
			if isTombstoned, err := c.cloudStore.portalIsExplicitlyDeleted(ctx, portalID); err == nil && isTombstoned {
				log.Debug().
					Str("guid", msg.Guid).
					Str("portal_id", portalID).
					Str("sender", msg.Sender).
					Str("cloud_chat_id", msg.CloudChatId).
					Msg("Skipping message for tombstoned/explicitly-deleted chat")
				counts.Filtered++
				continue
			}
		}

		// Skip orphaned messages: no CloudChatId AND no cloud_chat record for the
		// resolved portal. Apple omits filtered/junk chats from the chat zone
		// entirely; messages from those chats have no chat_id and represent spam
		// or unknown-sender conversations the user never sees in iMessage.
		// Messages with a non-empty CloudChatId are allowed through even without
		// a cloud_chat row — they may belong to recycle-bin-only chats.
		if msg.CloudChatId == "" && c.cloudStore != nil {
			if hasChat, err := c.cloudStore.portalHasChat(ctx, portalID); err == nil && !hasChat {
				log.Debug().
					Str("guid", msg.Guid).
					Str("portal_id", portalID).
					Str("sender", msg.Sender).
					Msg("Skipping orphaned message (no chat_id, no chat record)")
				counts.Filtered++
				continue
			}
		}

		text := ""
		if msg.Text != nil {
			text = *msg.Text
		}
		subject := ""
		if msg.Subject != nil {
			subject = *msg.Subject
		}
		timestampMS := msg.TimestampMs
		if timestampMS <= 0 {
			timestampMS = time.Now().UnixMilli()
		}

		tapbackTargetGUID := ""
		if msg.TapbackTargetGuid != nil {
			tapbackTargetGUID = *msg.TapbackTargetGuid
		}
		tapbackEmoji := ""
		if msg.TapbackEmoji != nil {
			tapbackEmoji = *msg.TapbackEmoji
		}

		// Enrich and serialize attachment metadata.
		//
		// Merge two sources of attachment GUIDs so we don't silently drop
		// any attachment the account actually has:
		//   1. msg.AttachmentGuids — from the wrapper's NSAttributedString
		//      parse of the message proto's attributedBody. Authoritative
		//      for ORDER and may include guids for which the attachment
		//      record itself didn't sync (e.g. inline/tiny blobs).
		//   2. attachmentsByMessage[msg.Guid] — from the attachment zone's
		//      own aguid field (`at_<N>_<msg.Guid>`). Load-bearing fallback
		//      for messages where the attributedBody parse returned []:
		//      without this, those messages ship to Matrix as an empty
		//      " " text event and the attachment is silently lost.
		//
		// Dedup by guid, preserving attributedBody order first.
		attachmentsJSON := ""
		if attMap != nil {
			seen := make(map[string]struct{}, len(msg.AttachmentGuids)+4)
			mergedGuids := make([]string, 0, len(msg.AttachmentGuids)+4)
			for _, g := range msg.AttachmentGuids {
				if g == "" {
					continue
				}
				if _, ok := seen[g]; ok {
					continue
				}
				seen[g] = struct{}{}
				mergedGuids = append(mergedGuids, g)
			}
			fallback := attachmentsByMessage[msg.Guid]
			if len(fallback) > 0 {
				// Deterministic order for the fallback set — sorting by
				// the aguid string (which starts with `at_<index>_`) keeps
				// the original attachment order for messages that have
				// more than one attachment.
				sort.Strings(fallback)
				for _, g := range fallback {
					if _, ok := seen[g]; ok {
						continue
					}
					seen[g] = struct{}{}
					mergedGuids = append(mergedGuids, g)
					log.Info().
						Str("msg_guid", msg.Guid).
						Str("att_guid", g).
						Msg("Recovered attachment via aguid suffix match (attributedBody parse missed it)")
				}
			}

			// Also probe attachmentsByMessage by the lowercased msg.Guid
			// suffix. Fresh reverse-index entries created during the
			// case-insensitive indexing pass above pick up aguids that
			// differ from the message's casing.
			msgGuidLower := strings.ToLower(msg.Guid)
			if msgGuidLower != msg.Guid {
				for _, g := range attachmentsByMessage[msgGuidLower] {
					if _, ok := seen[g]; ok {
						continue
					}
					seen[g] = struct{}{}
					mergedGuids = append(mergedGuids, g)
					log.Info().
						Str("msg_guid", msg.Guid).
						Str("att_guid", g).
						Msg("Recovered attachment via case-insensitive aguid suffix match")
				}
			}

			if len(mergedGuids) > 0 {
				var attRows []cloudAttachmentRow
				for _, guid := range mergedGuids {
					if enriched, ok := attMap[guid]; ok {
						attRows = append(attRows, enriched)
					} else if origKey, ok := attMapLowercased[strings.ToLower(guid)]; ok {
						// Case mismatch: the message's attributedBody has one
						// casing, the attachment record has the other. Look up
						// the actual attMap entry under its original key.
						log.Info().
							Str("msg_guid", msg.Guid).
							Str("att_guid_from_message", guid).
							Str("att_guid_in_attmap", origKey).
							Msg("Recovered attachment via case-insensitive attMap lookup")
						attRows = append(attRows, attMap[origKey])
					} else {
						log.Warn().Str("msg_guid", msg.Guid).Str("att_guid", guid).
							Msg("Attachment GUID not found in attachment zone")
					}
				}
				if len(attRows) > 0 {
					if attJSON, jsonErr := json.Marshal(attRows); jsonErr == nil {
						attachmentsJSON = string(attJSON)
					}
				}
			}
		}

		// Mark as deleted if the portal is currently being deleted, so
		// concurrent or future re-syncs don't resurrect the portal.
		isDeleted := deletedPortalsSnapshot[portalID]

		batch = append(batch, cloudMessageRow{
			GUID:              msg.Guid,
			RecordName:        msg.RecordName,
			CloudChatID:       msg.CloudChatId,
			PortalID:          portalID,
			TimestampMS:       timestampMS,
			Sender:            msg.Sender,
			IsFromMe:          msg.IsFromMe,
			Text:              text,
			Subject:           subject,
			Service:           msg.Service,
			Deleted:           isDeleted,
			TapbackType:       msg.TapbackType,
			TapbackTargetGUID: tapbackTargetGUID,
			TapbackEmoji:      tapbackEmoji,
			AttachmentsJSON:   attachmentsJSON,
			DateReadMS:        msg.DateReadMs,
			HasBody:           msg.HasBody,
		})

		if existingSet[msg.Guid] {
			counts.Updated++
		} else {
			counts.Imported++
		}
	}

	// Phase 2: Batch insert all live rows in a single transaction.
	if err := c.cloudStore.upsertMessageBatch(ctx, batch); err != nil {
		return err
	}

	return nil
}

func (c *IMClient) resolvePortalIDForCloudChat(participants []string, displayName *string, groupID string, style int64) string {
	normalizedParticipants := make([]string, 0, len(participants))
	for _, participant := range participants {
		normalized := normalizeIdentifierForPortalID(participant)
		if normalized == "" {
			continue
		}
		normalizedParticipants = append(normalizedParticipants, normalized)
	}
	if len(normalizedParticipants) == 0 {
		return ""
	}

	// CloudKit chat style: 43 = group, 45 = DM.
	// Use style as the authoritative group/DM signal. The group_id (gid)
	// field is set for ALL CloudKit chats, even DMs, so we can't use its
	// presence alone.
	isGroup := style == 43

	// For groups with a persistent group UUID, use gid:<UUID> as portal ID
	if isGroup && groupID != "" {
		normalizedGID := strings.ToLower(groupID)
		return "gid:" + normalizedGID
	}

	// For DMs: use the single remote participant as the portal ID
	// (e.g., "tel:+15551234567" or "mailto:user@example.com").
	// Filter out our own handle so only the remote side remains.
	remoteParticipants := make([]string, 0, len(normalizedParticipants))
	for _, p := range normalizedParticipants {
		if !c.isMyHandle(p) {
			remoteParticipants = append(remoteParticipants, p)
		}
	}

	if len(remoteParticipants) == 1 {
		// Standard DM — portal ID is the remote participant.
		// Use contact merging so that separate CloudKit chat records for
		// the same contact (one per handle) resolve to a single portal.
		handle := remoteParticipants[0]
		resolved := c.resolveContactPortalID(handle)
		if string(resolved) == handle {
			resolved = networkid.PortalID(c.canonicalContactHandle(handle))
		}
		resolved = c.resolveExistingDMPortalID(string(resolved))
		return string(resolved)
	}

	// Self-chat (Notes to Self): all participants are our own handle.
	// Use our handle as the portal ID directly.
	if len(remoteParticipants) == 0 && len(normalizedParticipants) > 0 {
		return normalizedParticipants[0]
	}

	// Fallback for edge cases (unknown style, multi-participant without group style)
	groupName := displayName
	var senderGuidPtr *string
	if isGroup && groupID != "" {
		senderGuidPtr = &groupID
	}
	portalKey := c.makePortalKey(normalizedParticipants, groupName, nil, senderGuidPtr)
	return string(portalKey.ID)
}

func (c *IMClient) createPortalsFromCloudSync(ctx context.Context, log zerolog.Logger, pendingDeletePortals map[string]bool) {
	if c.cloudStore == nil {
		return
	}

	// Get portal IDs sorted by newest message timestamp (most recent first).
	// This includes both portals that have messages AND chat-only portals
	// from cloud_chat (with 0 messages). Chat-only portals are included so
	// conversations synced from CloudKit without any resolved messages still
	// get bridge portals created.
	portalInfos, err := c.cloudStore.listPortalIDsWithNewestTimestamp(ctx)
	if err != nil {
		log.Err(err).Msg("Failed to list cloud portal IDs with timestamps")
		return
	}

	if len(portalInfos) == 0 {
		return
	}

	// Tombstoned (deleted) chats are already removed from cloud_chat/cloud_message
	// during ingestCloudChats, so they won't appear in portalInfos. No separate
	// deleted_portal filter needed — CloudKit tombstones are the authoritative signal.

	// Check recentlyDeletedPortals live (not a snapshot) so that recoveries
	// that arrive during CloudKit paging are respected. If a portal is removed
	// from the map by handleChatRecover between now and when we check it below,
	// the portal won't be skipped.

	// Count how many portals have messages vs chat-only (diagnostic).
	chatOnlyPortals := 0
	for _, p := range portalInfos {
		if p.MessageCount == 0 {
			chatOnlyPortals++
		}
	}
	log.Info().
		Int("total_portals", len(portalInfos)).
		Int("with_messages", len(portalInfos)-chatOnlyPortals).
		Int("chat_only", chatOnlyPortals).
		Msg("Portal candidates from cloud sync (messages + chat-only)")

	// Skip portals already queued this session with the same newest timestamp.
	// If CloudKit has newer messages, the timestamp changes and we re-queue.
	if c.queuedPortals == nil {
		c.queuedPortals = make(map[string]int64)
	}
	ordered := make([]string, 0, len(portalInfos))
	newestTSByPortal := make(map[string]int64, len(portalInfos))
	forwardBackfillPortals := 0
	alreadyQueued := 0
	pendingDeleteSkipped := 0
	groupDedupSkipped := 0
	seenGroupKeys := make(map[string]string) // dedup key → chosen portal_id
	for _, p := range portalInfos {
		newestTSByPortal[p.PortalID] = p.NewestTS
		// Skip portals that are recently deleted this session.
		// Checked live (not from a static snapshot) so that mid-sync
		// recoveries via handleChatRecover are respected immediately.
		c.recentlyDeletedPortalsMu.RLock()
		_, isDeleted := c.recentlyDeletedPortals[p.PortalID]
		c.recentlyDeletedPortalsMu.RUnlock()
		if isDeleted {
			pendingDeleteSkipped++
			continue
		}
		// Dedup group portals: the same group can appear under multiple
		// portal_ids (gid:<chat_id> vs gid:<group_id>). Use the group
		// dedup key to detect duplicates and keep only the portal_id
		// that already has a bridge portal, or the first one (most
		// recent messages since list is sorted by newest_ts DESC).
		if isGroupPortalID(p.PortalID) {
			groupID := ""
			if strings.HasPrefix(p.PortalID, "gid:") {
				groupID = c.cloudStore.getGroupIDForPortalID(ctx, p.PortalID)
			}
			key := groupPortalDedupKey(p.PortalID, groupID, nil)
			if existingPortalID, seen := seenGroupKeys[key]; seen {
				// Already have a candidate for this group. Prefer
				// whichever has an existing bridge portal.
				existingKey := networkid.PortalKey{ID: networkid.PortalID(existingPortalID), Receiver: c.UserLogin.ID}
				newKey := networkid.PortalKey{ID: networkid.PortalID(p.PortalID), Receiver: c.UserLogin.ID}
				existingPortal, _ := c.UserLogin.Bridge.GetExistingPortalByKey(ctx, existingKey)
				newPortal, _ := c.UserLogin.Bridge.GetExistingPortalByKey(ctx, newKey)
				existingHasRoom := existingPortal != nil && existingPortal.MXID != ""
				newHasRoom := newPortal != nil && newPortal.MXID != ""
				if newHasRoom && !existingHasRoom {
					// Swap: the new one has the bridge portal, use it instead
					log.Info().
						Str("kept_portal_id", p.PortalID).
						Str("skipped_portal_id", existingPortalID).
						Str("dedup_key", key).
						Msg("Group dedup: swapping to portal with existing bridge room")
					seenGroupKeys[key] = p.PortalID
					// Replace in ordered list
					for j, id := range ordered {
						if id == existingPortalID {
							ordered[j] = p.PortalID
							break
						}
					}
				} else {
					log.Info().
						Str("kept_portal_id", existingPortalID).
						Str("skipped_portal_id", p.PortalID).
						Str("dedup_key", key).
						Msg("Group dedup: skipping duplicate group portal")
				}
				groupDedupSkipped++
				continue
			}
			seenGroupKeys[key] = p.PortalID
		}
		if lastTS, ok := c.queuedPortals[p.PortalID]; ok && lastTS >= p.NewestTS {
			alreadyQueued++
			continue
		}
		portalKey := networkid.PortalKey{ID: networkid.PortalID(p.PortalID), Receiver: c.UserLogin.ID}
		existingPortal, _ := c.UserLogin.Bridge.GetExistingPortalByKey(ctx, portalKey)
		if p.MessageCount > 0 && (existingPortal == nil || existingPortal.MXID == "") {
			forwardBackfillPortals++
		}
		ordered = append(ordered, p.PortalID)
	}
	if pendingDeleteSkipped > 0 {
		log.Info().Int("skipped", pendingDeleteSkipped).Msg("Skipped tombstoned or recently deleted portals")
	}
	if groupDedupSkipped > 0 {
		log.Info().Int("skipped", groupDedupSkipped).Msg("Skipped duplicate group portal IDs (same group, different UUID)")
	}

	portalStart := time.Now()
	log.Info().
		Int("total_candidates", len(portalInfos)).
		Int("already_queued", alreadyQueued).
		Int("group_dedup_skipped", groupDedupSkipped).
		Int("to_process", len(ordered)).
		Msg("Creating portals from cloud sync")

	// Set pendingInitialBackfills BEFORE queuing any portals.
	// bridgev2 processes ChatResync events concurrently (QueueRemoteEvent is
	// async), so FetchMessages(Forward=true) calls — and their
	// onForwardBackfillDone decrements — can fire during the queuing loop.
	// If we set the counter AFTER the loop (StoreInt64 at the end), early
	// decrements land on 0 (making it negative), then the Store overwrites
	// those decrements with N, leaving the counter stuck at the number of
	// early completions rather than reaching 0. Setting it up-front ensures
	// every decrement is counted correctly.
	//
	// Count ONLY portals that actually have message history. Chat-only portals
	// still get ChatResync events so rooms are created, but GetChatInfo sets
	// CanBackfill=false for them, so bridgev2 never calls FetchMessages(Forward)
	// and they must not hold the APNs buffer open.
	if !c.isCloudSyncDone() {
		atomic.StoreInt64(&c.pendingInitialBackfills, int64(forwardBackfillPortals))
		log.Debug().
			Int("count", forwardBackfillPortals).
			Int("chat_only_or_existing", len(ordered)-forwardBackfillPortals).
			Msg("Set pendingInitialBackfills for APNs buffer hold")
	}

	created := 0
	for i, portalID := range ordered {
		portalKey := networkid.PortalKey{
			ID:       networkid.PortalID(portalID),
			Receiver: c.UserLogin.ID,
		}

		newestTS := newestTSByPortal[portalID]
		var latestMessageTS time.Time
		if newestTS > 0 {
			latestMessageTS = time.UnixMilli(newestTS)
		}
		log.Debug().
			Str("portal_id", portalID).
			Int("index", i).
			Int("total", len(ordered)).
			Int64("newest_ts", newestTS).
			Msg("Queuing ChatResync for portal")
		c.UserLogin.QueueRemoteEvent(&simplevent.ChatResync{
			EventMeta: simplevent.EventMeta{
				Type:         bridgev2.RemoteEventChatResync,
				PortalKey:    portalKey,
				CreatePortal: true,
				LogContext: func(lc zerolog.Context) zerolog.Context {
					return lc.
						Str("portal_id", portalID).
						Str("source", "cloud_sync")
				},
			},
			GetChatInfoFunc: c.GetChatInfo,
			LatestMessageTS: latestMessageTS,
		})
		c.queuedPortals[portalID] = newestTS
		created++
		if (i+1)%25 == 0 {
			log.Info().
				Int("progress", i+1).
				Int("total", len(ordered)).
				Dur("elapsed", time.Since(portalStart)).
				Msg("Portal queuing progress")
		}
		// Stagger portal processing to avoid overwhelming Matrix server
		// with concurrent ghost updates and room state changes.
		if (i+1)%5 == 0 {
			time.Sleep(500 * time.Millisecond)
		}
	}

	log.Info().
		Int("queued", created).
		Int("total", len(ordered)).
		Dur("elapsed", time.Since(portalStart)).
		Msg("Finished queuing portals from cloud sync")

	// pendingInitialBackfills was already set BEFORE the loop
	// so that early-completing FetchMessages(Forward=true) calls are counted
	// correctly (see comment above the loop).  Nothing more to do here.

	// Reset backward backfill tasks for portals that already have Matrix rooms.
	// This handles the version-upgrade re-sync case where rooms exist but the
	// backfill task may be marked done from a previous incomplete sync.
	//
	// For NEW portals (fresh DB, no rooms yet), we do NOT create tasks here.
	// bridgev2 automatically creates backfill tasks when it creates the Matrix
	// room during ChatResync processing (portal.go calls BackfillTask.Upsert
	// when CanBackfill=true). Creating tasks before rooms exist causes a race:
	// the backfill queue picks up the task, finds portal.MXID=="", and
	// permanently deletes it via deleteBackfillQueueTaskIfRoomDoesNotExist.
	resetCount := 0
	skippedNoRoom := 0
	for _, portalID := range ordered {
		portalKey := networkid.PortalKey{
			ID:       networkid.PortalID(portalID),
			Receiver: c.UserLogin.ID,
		}
		portal, err := c.Main.Bridge.GetExistingPortalByKey(ctx, portalKey)
		if err != nil {
			log.Warn().Err(err).Str("portal_id", portalID).Msg("Failed to look up portal for backfill task")
			continue
		}
		if portal == nil || portal.MXID == "" {
			skippedNoRoom++
			continue
		}
		if err := c.Main.Bridge.DB.BackfillTask.Upsert(ctx, &database.BackfillTask{
			PortalKey:         portalKey,
			UserLoginID:       c.UserLogin.ID,
			BatchCount:        -1,
			IsDone:            false,
			Cursor:            "",
			OldestMessageID:   "",
			NextDispatchMinTS: time.Now().Add(5 * time.Second),
		}); err != nil {
			log.Warn().Err(err).Str("portal_id", portalID).Msg("Failed to reset backfill task")
		} else {
			resetCount++
		}
	}
	if resetCount > 0 || skippedNoRoom > 0 {
		log.Info().
			Int("reset_count", resetCount).
			Int("skipped_no_room", skippedNoRoom).
			Msg("Backward backfill task setup (rooms without tasks reset, new portals deferred to ChatResync)")
		if resetCount > 0 {
			c.Main.Bridge.WakeupBackfillQueue()
		}
	}
}

func (c *IMClient) ensureCloudSyncStore(ctx context.Context) error {
	if c.cloudStore == nil {
		return fmt.Errorf("cloud store not initialized")
	}
	return c.cloudStore.ensureSchema(ctx)
}

// filenameBase returns the filename without its extension.
// e.g., "IMG_5551.HEIC" → "IMG_5551", "photo.MOV" → "photo"
func filenameBase(name string) string {
	if idx := strings.LastIndex(name, "."); idx > 0 {
		return name[:idx]
	}
	return ""
}
