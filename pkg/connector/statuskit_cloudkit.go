package connector

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

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

// Reserved cloud_sync_state row names. All three reuse the existing table so
// no migration is required.
//   - statusKitCloudZoneRow stores the resolved zone name in
//     continuation_token.
//   - statusKitCloudTokenRow stores the FFI continuation token (base64) in
//     continuation_token.
//   - statusKitCloudPassMetaRow stores "<last_attempt_unix_ms>:<consec_err>"
//     in continuation_token. Used for the inter-pass backoff gate so the
//     bridge doesn't spam Apple's CKKS — empirically, an unpaced burst of
//     LimitedPeersAllowed-zone fetches gets the device kicked out of the
//     iCloud trust circle ("Not in clique" responses on every CloudKit op).
const (
	statusKitCloudZoneRow     = "_statuskit_zone"
	statusKitCloudTokenRow    = "_statuskit_token"
	statusKitCloudPassMetaRow = "_statuskit_pass_meta"
)

// statusKitPassMinInterval is the minimum spacing between two successful
// passes. Apple updates the LimitedPeersAllowed view far less frequently
// than the bridge's outer cloud-sync cadence (which can fire several times
// in the first minutes after restart and again on every !resync). 15 min is
// well below Apple's expected refresh-on-status-change latency for peer iOS
// reshares while staying conservative on CKKS request volume.
const statusKitPassMinInterval = 15 * time.Minute

// statusKitPassMaxBackoff caps the exponential backoff after consecutive
// errors. Two hours is long enough that a transient CKKS rate-limit window
// (which typically resets in tens of minutes) clears between attempts, and
// short enough that recovery after a self-healing trust event is automatic
// rather than requiring a bridge restart.
const statusKitPassMaxBackoff = 2 * time.Hour

// statuskitPassRetryAfterRegex extracts an Apple-provided "retry_after_seconds"
// hint from FFI error strings. Same shape as the chat/message backfill path
// uses (see cloudKitRetryAfterRegex in sync_controller.go) — we honor the
// larger of the hint or our exponential fallback, capped to
// statusKitPassMaxBackoff.
var statuskitPassRetryAfterRegex = cloudKitRetryAfterRegex

// statusKitPassMeta is the parsed form of statusKitCloudPassMetaRow. Stored
// flat in the existing TEXT column so no schema change is needed.
type statusKitPassMeta struct {
	LastAttempt      time.Time
	ConsecutiveErrs  int
}

func parseStatusKitPassMeta(raw *string) statusKitPassMeta {
	if raw == nil || *raw == "" {
		return statusKitPassMeta{}
	}
	parts := strings.SplitN(*raw, ":", 2)
	if len(parts) != 2 {
		return statusKitPassMeta{}
	}
	tsMS, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || tsMS <= 0 {
		return statusKitPassMeta{}
	}
	errs, err := strconv.Atoi(parts[1])
	if err != nil || errs < 0 {
		errs = 0
	}
	return statusKitPassMeta{
		LastAttempt:     time.UnixMilli(tsMS),
		ConsecutiveErrs: errs,
	}
}

func (m statusKitPassMeta) encode() string {
	return fmt.Sprintf("%d:%d", m.LastAttempt.UnixMilli(), m.ConsecutiveErrs)
}

// nextAllowedAt returns the earliest time at which a new pass may run.
// Successful passes use the steady-state min interval; failed passes use
// exponential backoff (15m → 30m → 1h → 2h capped) plus any retry-after
// hint extracted from the last error.
func (m statusKitPassMeta) nextAllowedAt(retryAfterHint time.Duration) time.Time {
	if m.LastAttempt.IsZero() {
		return time.Time{}
	}
	delay := statusKitPassMinInterval
	if m.ConsecutiveErrs > 0 {
		// 15m * 2^(n-1), capped. n=1 → 15m, n=2 → 30m, n=3 → 1h, n=4 → 2h.
		shift := m.ConsecutiveErrs - 1
		if shift > 6 {
			shift = 6
		}
		delay = statusKitPassMinInterval << shift
		if delay > statusKitPassMaxBackoff {
			delay = statusKitPassMaxBackoff
		}
	}
	if retryAfterHint > delay {
		delay = retryAfterHint
		if delay > statusKitPassMaxBackoff {
			delay = statusKitPassMaxBackoff
		}
	}
	return m.LastAttempt.Add(delay)
}

// extractRetryAfterHint parses Apple's `retry_after_seconds` from an FFI
// error string, mirroring sync_controller.go's chat/message path. Returns
// zero if no hint present.
func extractRetryAfterHint(err error) time.Duration {
	if err == nil {
		return 0
	}
	m := statuskitPassRetryAfterRegex.FindStringSubmatch(err.Error())
	if len(m) < 2 {
		return 0
	}
	secs, parseErr := strconv.Atoi(m[1])
	if parseErr != nil || secs <= 0 {
		return 0
	}
	hint := time.Duration(secs) * time.Second
	if hint > statusKitPassMaxBackoff {
		hint = statusKitPassMaxBackoff
	}
	return hint
}

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

	// Inter-pass backoff gate. The outer cloud-sync orchestrator can fire
	// runCloudSyncOnce several times in the first minutes after a restart
	// (bootstrap + delayed-resync schedule + !resync) and on every APNs
	// nudge thereafter. Hammering Apple's CKKS for the LimitedPeersAllowed
	// view at that cadence is what got us kicked out of the trust circle
	// today. Mirrors the bridge's chat/message-backfill retry shape from
	// sync_controller.go: exponential backoff on consecutive errors,
	// honoring Apple's `retry_after_seconds` hint when larger, capped at
	// statusKitPassMaxBackoff (2h).
	//
	// Skipping the FFI here intentionally does NOT touch the meta row —
	// the gate is read-only when it blocks. Bumping last_attempt on a
	// blocked attempt would push the deadline forward forever and the
	// pass would never fire.
	metaPtr, err := c.cloudStore.getSyncState(ctx, statusKitCloudPassMetaRow)
	if err != nil {
		log.Info().Err(err).Msg("StatusKit-CloudKit pass: cloud_sync_state read failed for meta-row (continuing — treats as fresh state)")
		metaPtr = nil
	}
	meta := parseStatusKitPassMeta(metaPtr)
	// First pass after process startup bypasses the gate so a bridge restart
	// is a natural retry signal — the user may have fixed trust externally
	// and is rebooting to recover. CompareAndSwap ensures only one caller
	// per process gets the bypass; subsequent calls fall through to the
	// gate normally.
	bypassForFirstCall := c.statusKitCloudPassFirstCallDone.CompareAndSwap(false, true)
	if !bypassForFirstCall && !meta.LastAttempt.IsZero() {
		nextAllowed := meta.nextAllowedAt(0)
		if time.Now().Before(nextAllowed) {
			log.Info().
				Time("last_attempt", meta.LastAttempt).
				Int("consecutive_errors", meta.ConsecutiveErrs).
				Time("next_allowed", nextAllowed).
				Dur("wait", time.Until(nextAllowed).Round(time.Second)).
				Msg("StatusKit-CloudKit pass: skipped by inter-pass backoff (Apple CKKS rate-limit guard)")
			return nil
		}
	} else if bypassForFirstCall && meta.ConsecutiveErrs > 0 {
		log.Info().
			Int("prior_consecutive_errors", meta.ConsecutiveErrs).
			Time("prior_last_attempt", meta.LastAttempt).
			Msg("StatusKit-CloudKit pass: bypassing prior backoff for first pass after restart (one chance to recover)")
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
	//
	// We commit a meta-row update either way (success: errors=0, failure:
	// errors+1 + retry-after extension). Done via defer so a panic that
	// escapes safeFFICall still leaves the gate in a recoverable state
	// rather than stranding the bridge in "always run, no backoff".
	attemptStart := time.Now()
	var ffiErr error
	defer func() {
		newMeta := statusKitPassMeta{LastAttempt: attemptStart}
		if ffiErr == nil {
			newMeta.ConsecutiveErrs = 0
		} else {
			newMeta.ConsecutiveErrs = meta.ConsecutiveErrs + 1
		}
		encoded := newMeta.encode()
		if persistErr := c.cloudStore.setSyncStateSuccess(ctx, statusKitCloudPassMetaRow, &encoded); persistErr != nil {
			log.Info().Err(persistErr).Msg("StatusKit-CloudKit pass: failed to persist pass-meta row (next pass may not honor backoff)")
		}
	}()

	page, err := safeFFICall("CloudSyncStatuskitPeers", func() (rustpushgo.CloudSyncStatusKitPage, error) {
		return c.client.CloudSyncStatuskitPeers(cachedZone, sinceToken)
	})
	if err != nil {
		ffiErr = err
		hint := extractRetryAfterHint(err)
		nextErrs := meta.ConsecutiveErrs + 1
		nextMeta := statusKitPassMeta{LastAttempt: attemptStart, ConsecutiveErrs: nextErrs}
		log.Info().
			Err(err).
			Int("consecutive_errors", nextErrs).
			Dur("retry_after_hint", hint).
			Time("next_allowed", nextMeta.nextAllowedAt(hint)).
			Msg("StatusKit-CloudKit pass: FFI returned error — applying inter-pass backoff")
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
