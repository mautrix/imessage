package connector

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/rs/zerolog"

	"github.com/lrhodin/imessage/pkg/rustpushgo"
)

// StatusKit-CloudKit pull (bridge-side orchestration).
//
// The Rust side does the actual zone discovery, record fetch, and state
// injection (see pkg/rustpushgo/src/statuskit_cloudkit.rs). This file:
//   1. Reads the cached discovered-zone-name and continuation-token from
//      the existing cloud_sync_state table.
//   2. Calls the FFI cloud_sync_statuskit_peers.
//   3. Persists the resolved zone name and the next continuation token.
//   4. If any peers were injected, fires subscribeToContactPresence so the
//      bridge subscribes to the new peers' StatusKit channels immediately.
//
// All logging is at log.Info() level so production output can be inspected
// without raising the log level. Phase 1 of the feature is intentionally
// diagnostic-heavy: the Rust side dumps every record's schema so the user
// can identify the StatusKit zone and field names from logs and feed them
// back to refine the decoder.

// Reserved cloud_sync_state row names. Both reuse the existing table so no
// migration is required. The discovery row stores the discovered zone name
// in the continuation_token column; the token row stores the actual sync
// token (base64-encoded since the column is TEXT and tokens are bytes).
const (
	statusKitCloudZoneRow  = "_statuskit_zone"
	statusKitCloudTokenRow = "_statuskit_token"
)

// syncCloudStatusKitPeers runs one StatusKit-CloudKit pull pass. Called as
// the fourth phase of runCloudSyncOnce, after the existing chats / messages
// / attachments backfill completes.
//
// Behavior:
//   - Reads cached zone name + continuation token from cloud_sync_state.
//   - Defers (no-op) if a StatusKit invite sweep is currently running, so
//     we don't fight the rust-side StatusKit mutex.
//   - Refreshes the PET token (matches the startup-share pattern).
//   - Calls the FFI; the Rust side discovers the zone on first call.
//   - Persists the resolved zone name + next token. If the FFI returns
//     resolved_zone=None alongside an explicit summary, that signals
//     "clear cached zone so we re-discover next pass".
//   - If injected_handles > 0, fires subscribeToContactPresence so the
//     newly-keyed peers get APNs subscriptions immediately rather than
//     waiting for the next periodic loop.
func (c *IMClient) syncCloudStatusKitPeers(ctx context.Context, log zerolog.Logger) error {
	if c.client == nil {
		log.Info().Msg("StatusKit-CloudKit pass: skipped (rust client not ready)")
		return nil
	}
	if c.UserLogin == nil {
		log.Info().Msg("StatusKit-CloudKit pass: skipped (no UserLogin)")
		return nil
	}

	// Avoid running concurrently with an invite sweep — both touch the
	// rust-side StatusKit state mutex. Sweep wins; we'll pick up next pass.
	if c.statusKitSweepRunning.Load() {
		log.Info().Msg("StatusKit-CloudKit pass: deferred (StatusKit invite sweep is running)")
		return nil
	}

	// Refresh PET so the CloudKit container init has a fresh delegate.
	// Matches the startup-share path pattern in client.go.
	if err := c.safeRefreshPetTokenThrottled(); err != nil {
		log.Info().Err(err).Msg("StatusKit-CloudKit pass: PET refresh skipped (continuing — refresh is best-effort)")
	}

	// Read cached zone name (if any). The discovery row stores the
	// resolved zone name in the continuation_token column.
	cachedZonePtr, err := c.cloudStore.getSyncState(ctx, statusKitCloudZoneRow)
	if err != nil {
		log.Info().Err(err).Msg("StatusKit-CloudKit pass: cloud_sync_state read failed for zone-row (continuing without cache)")
		cachedZonePtr = nil
	}
	var cachedZone *string
	if cachedZonePtr != nil && *cachedZonePtr != "" {
		cachedZone = cachedZonePtr
	}

	// Read continuation token. The token is opaque bytes; we base64-encode
	// it for storage in the TEXT column.
	cachedTokenPtr, err := c.cloudStore.getSyncState(ctx, statusKitCloudTokenRow)
	if err != nil {
		log.Info().Err(err).Msg("StatusKit-CloudKit pass: cloud_sync_state read failed for token-row (continuing without token)")
		cachedTokenPtr = nil
	}
	var sinceToken *[]byte
	if cachedTokenPtr != nil && *cachedTokenPtr != "" {
		decoded, decErr := base64.StdEncoding.DecodeString(*cachedTokenPtr)
		if decErr != nil {
			log.Info().Err(decErr).Str("raw", *cachedTokenPtr).Msg("StatusKit-CloudKit pass: stored token failed base64 decode — clearing and continuing without token")
			if clrErr := c.cloudStore.clearZoneToken(ctx, statusKitCloudTokenRow); clrErr != nil {
				log.Info().Err(clrErr).Msg("StatusKit-CloudKit pass: failed to clear malformed token row")
			}
		} else {
			sinceToken = &decoded
		}
	}

	log.Info().
		Bool("has_cached_zone", cachedZone != nil).
		Interface("cached_zone", cachedZone).
		Bool("has_cached_token", sinceToken != nil).
		Msg("StatusKit-CloudKit pass: invoking FFI")

	// Wrap in safeFFICall to match the panic-isolation pattern used by
	// the other cloud-sync FFIs (sync_controller.go safeCloudSyncMessages etc).
	// UniFFI deserialization panics on malformed buffers; without this wrap
	// a bad page would crash the bridge instead of just failing the pass.
	page, err := safeFFICall("CloudSyncStatuskitPeers", func() (rustpushgo.CloudSyncStatusKitPage, error) {
		return c.client.CloudSyncStatuskitPeers(cachedZone, sinceToken)
	})
	if err != nil {
		log.Info().Err(err).Msg("StatusKit-CloudKit pass: FFI returned error — leaving cache untouched, will retry next pass")
		return fmt.Errorf("CloudSyncStatuskitPeers: %w", err)
	}

	logEntry := log.Info().
		Uint32("fetched", page.Fetched).
		Uint32("inserted", page.Inserted).
		Uint32("already_known", page.AlreadyKnown).
		Uint32("decode_failed", page.DecodeFailed).
		Uint32("records_seen", page.RecordsSeen).
		Int("injected_handles_count", len(page.InjectedHandles))
	if page.ResolvedZone != nil {
		logEntry = logEntry.Str("resolved_zone", *page.ResolvedZone)
	}
	if page.DiscoverySummary != nil {
		logEntry = logEntry.Str("discovery_summary", *page.DiscoverySummary)
	}
	if len(page.InjectedHandles) > 0 {
		logEntry = logEntry.Strs("injected_handles", page.InjectedHandles)
	}
	logEntry.Msg("StatusKit-CloudKit pass: FFI returned")

	// Persist resolved zone name. If FFI returned ResolvedZone=nil, treat
	// that as a directive to clear the cached zone (e.g. discovery found
	// nothing, or the cached zone is stale and fetch failed).
	if page.ResolvedZone == nil {
		if cachedZone != nil {
			if err := c.cloudStore.clearZoneToken(ctx, statusKitCloudZoneRow); err != nil {
				log.Info().Err(err).Msg("StatusKit-CloudKit pass: failed to clear cached zone row")
			} else {
				log.Info().Msg("StatusKit-CloudKit pass: cleared cached zone (FFI returned ResolvedZone=nil)")
			}
		}
	} else if cachedZone == nil || *cachedZone != *page.ResolvedZone {
		zoneStr := *page.ResolvedZone
		if err := c.cloudStore.setSyncStateSuccess(ctx, statusKitCloudZoneRow, &zoneStr); err != nil {
			log.Info().Err(err).Msg("StatusKit-CloudKit pass: failed to persist resolved zone name")
		} else {
			log.Info().Str("zone", zoneStr).Msg("StatusKit-CloudKit pass: cached resolved zone name for future passes")
		}
	}

	// Persist (or clear) continuation token. We always update because the
	// FFI may return a fresh token even when no records were injected.
	if page.NextToken != nil && len(*page.NextToken) > 0 {
		encoded := base64.StdEncoding.EncodeToString(*page.NextToken)
		if err := c.cloudStore.setSyncStateSuccess(ctx, statusKitCloudTokenRow, &encoded); err != nil {
			log.Info().Err(err).Msg("StatusKit-CloudKit pass: failed to persist next continuation token")
		} else {
			log.Info().Int("token_b64_len", len(encoded)).Msg("StatusKit-CloudKit pass: persisted next continuation token")
		}
	} else if page.ResolvedZone == nil && sinceToken != nil {
		// FFI signalled re-discovery. Drop the stale token so the next
		// pass starts fresh.
		if err := c.cloudStore.clearZoneToken(ctx, statusKitCloudTokenRow); err != nil {
			log.Info().Err(err).Msg("StatusKit-CloudKit pass: failed to clear stale continuation token")
		} else {
			log.Info().Msg("StatusKit-CloudKit pass: cleared stale continuation token after re-discovery signal")
		}
	}

	// If we injected new peer keys, fire a presence-resubscribe so APNs
	// interest tokens cover the new channels immediately. Without this,
	// status updates from CloudKit-injected peers would only land after
	// the next periodic re-subscribe (subscribeAfterInit / connect).
	if page.Inserted > 0 {
		log.Info().Uint32("inserted", page.Inserted).Msg("StatusKit-CloudKit pass: triggering presence resubscribe for newly-injected peers")
		// subscribeToContactPresence is async-friendly; run inline to
		// keep ordering deterministic with the surrounding sync cycle.
		c.subscribeToContactPresence(log)
	}

	return nil
}
