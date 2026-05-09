package connector

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
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

// statusKitFailureBackoffBase is the base delay used when the previous
// pass failed. The actual delay scales exponentially with consecutive
// errors (15m → 30m → 1h → 2h capped).
const statusKitFailureBackoffBase = 15 * time.Minute

// statusKitSuccessFloor is the steady-state minimum interval between
// successful passes. New peer keys arrive at human-event rates (a
// contact opening Focus sharing for the first time, a peer rotating
// devices) — typically a handful per week across a contact list. The
// outer cloud-sync orchestrator fires multiple times per hour on a
// chatty cluster; without this floor we'd pull ~100 records of CKKS
// churn per cycle to discover changes that statistically aren't there.
// 12h is well above the natural event rate while still picking up new
// peers within a day, and an order-of-magnitude reduction in CKKS
// volume on the LimitedPeersAllowed view (the surface that produced
// the clique-kick during early testing).
const statusKitSuccessFloor = 12 * time.Hour

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
	LastAttempt     time.Time
	ConsecutiveErrs int
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
// On a successful previous pass with no Apple retry-after hint, applies
// the statusKitSuccessFloor (12h) — new peer keys are rare and polling
// tighter wastes CKKS budget. On failure, applies exponential backoff
// (15m → 30m → 1h → 2h capped) honoring any larger retry-after hint.
func (m statusKitPassMeta) nextAllowedAt(retryAfterHint time.Duration) time.Time {
	if m.LastAttempt.IsZero() {
		return time.Time{}
	}
	if m.ConsecutiveErrs == 0 && retryAfterHint == 0 {
		return m.LastAttempt.Add(statusKitSuccessFloor)
	}
	delay := statusKitFailureBackoffBase
	if m.ConsecutiveErrs > 0 {
		// 15m * 2^(n-1), capped. n=1 → 15m, n=2 → 30m, n=3 → 1h, n=4 → 2h.
		shift := m.ConsecutiveErrs - 1
		if shift > 6 {
			shift = 6
		}
		delay = statusKitFailureBackoffBase << shift
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

	// Don't run during initial forward backfill or for a settling window
	// after it completes. On a cold bootstrap, presence broadcasts that
	// arrive while messages are still being inserted into the bridge DB
	// hit the OnStatusUpdate `lastMsg=nil` fallback (now-1ms) and bump
	// chats to the top of the room list in random presence-arrival order.
	// A warm restart works fine because the DB already has prior-session
	// messages to anchor against. apnsBufferFlushedAt is set the moment
	// the last forward-backfill batch's CompleteCallback fires (in
	// onForwardBackfillDone at counter==0); we add a settling window on
	// top of that to let any straggler DB writes commit.
	const statusKitBackfillSettleWindow = 60 * time.Second
	flushedAt := atomic.LoadInt64(&c.apnsBufferFlushedAt)
	if flushedAt == 0 {
		log.Info().Msg("StatusKit-CloudKit pass: skipped (initial forward backfill not yet complete)")
		return nil
	}
	if elapsed := time.Since(time.UnixMilli(flushedAt)); elapsed < statusKitBackfillSettleWindow {
		log.Info().
			Dur("elapsed_since_backfill", elapsed.Round(time.Second)).
			Dur("settle_window", statusKitBackfillSettleWindow).
			Msg("StatusKit-CloudKit pass: skipped (waiting for backfill DB writes to settle)")
		return nil
	}

	// Inter-pass backoff gate. The outer cloud-sync orchestrator can fire
	// runCloudSyncOnce several times in the first minutes after a restart
	// (bootstrap + delayed-resync schedule + !resync) and on every APNs
	// nudge thereafter. Hammering Apple's CKKS for the LimitedPeersAllowed
	// view at that cadence is what got us kicked out of the trust circle
	// during early testing. Two gates apply here: a 12h success floor
	// (statusKitSuccessFloor) for steady-state, since new peer keys
	// arrive at human-event rates rather than per-cycle; and exponential
	// failure backoff (15m → 30m → 1h → 2h capped) honoring Apple's
	// `retry_after_seconds` hint when larger.
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

	// Drain all pages within this single pass. Each FFI call returns one
	// page with at most one Apple-side fetch round-trip; the continuation
	// token in the response tells us whether more pages exist. Looping
	// here means a single bootstrap pass ingests every available record
	// instead of stranding pages behind the 12h success floor (the
	// chat/message backfill paths get hit hundreds of times per session
	// so they drain naturally; StatusKit doesn't, and architecturally
	// pretending it does was the bug).
	//
	// Safety cap of 30 pages with a 1s pause between iterations keeps
	// the per-pass CKKS round-trip count bounded even if Apple keeps
	// returning continuation tokens — far below the per-pass budget that
	// produced the early-testing clique-kick.
	const (
		maxPagesPerPass = 30
		interPageDelay  = 1 * time.Second
	)
	var (
		totalFetched      uint32
		totalInserted     uint32
		totalAlreadyKnown uint32
		totalDecodeFailed uint32
		totalRecordsSeen  uint32
		injectedHandles   []string
	)
	currentZone := cachedZone
	currentToken := sinceToken

	for pageNum := 1; pageNum <= maxPagesPerPass; pageNum++ {
		page, err := safeFFICall("CloudSyncStatuskitPeers", func() (rustpushgo.CloudSyncStatusKitPage, error) {
			return c.client.CloudSyncStatuskitPeers(currentZone, currentToken)
		})
		if err != nil {
			ffiErr = err
			hint := extractRetryAfterHint(err)
			nextErrs := meta.ConsecutiveErrs + 1
			nextMeta := statusKitPassMeta{LastAttempt: attemptStart, ConsecutiveErrs: nextErrs}
			log.Info().
				Err(err).
				Int("page", pageNum).
				Int("consecutive_errors", nextErrs).
				Dur("retry_after_hint", hint).
				Time("next_allowed", nextMeta.nextAllowedAt(hint)).
				Msg("StatusKit-CloudKit pass: FFI returned error — applying inter-pass backoff")
			return fmt.Errorf("CloudSyncStatuskitPeers (page %d): %w", pageNum, err)
		}

		totalFetched += page.Fetched
		totalInserted += page.Inserted
		totalAlreadyKnown += page.AlreadyKnown
		totalDecodeFailed += page.DecodeFailed
		totalRecordsSeen += page.RecordsSeen
		if len(page.InjectedHandles) > 0 {
			injectedHandles = append(injectedHandles, page.InjectedHandles...)
		}

		// Feed every successfully-decoded (channel_id, sender_handle) pair
		// from this page into the persistent alias-cluster store. Catches
		// peers whose only reshare arrived while the bridge was offline —
		// those bypass the live OnReshareSender callback and would otherwise
		// be invisible to alias correlation.
		for _, obs := range page.ClusterObservations {
			c.recordReshareObservation(ctx, log, obs.ChannelId, obs.SenderHandle)
		}

		pageLog := log.Info().
			Int("page", pageNum).
			Uint32("fetched", page.Fetched).
			Uint32("inserted", page.Inserted).
			Uint32("already_known", page.AlreadyKnown).
			Uint32("decode_failed", page.DecodeFailed).
			Uint32("records_seen", page.RecordsSeen).
			Int("injected_handles_count", len(page.InjectedHandles))
		if page.ResolvedZone != nil {
			pageLog = pageLog.Str("resolved_zone", *page.ResolvedZone)
		}
		if page.DiscoverySummary != nil {
			pageLog = pageLog.Str("discovery_summary", *page.DiscoverySummary)
		}
		if len(page.InjectedHandles) > 0 {
			pageLog = pageLog.Strs("injected_handles", page.InjectedHandles)
		}
		pageLog.Msg("StatusKit-CloudKit pass: page returned")

		// Persist resolved zone name. If FFI returned ResolvedZone=nil,
		// treat that as a directive to clear the cached zone (discovery
		// found nothing, or the cached zone is stale and fetch failed).
		if page.ResolvedZone == nil {
			if currentZone != nil {
				if err := c.cloudStore.clearZoneToken(ctx, statusKitCloudZoneRow); err != nil {
					log.Info().Err(err).Msg("StatusKit-CloudKit pass: failed to clear cached zone row")
				} else {
					log.Info().Msg("StatusKit-CloudKit pass: cleared cached zone (FFI returned ResolvedZone=nil)")
				}
				currentZone = nil
			}
		} else if currentZone == nil || *currentZone != *page.ResolvedZone {
			zoneStr := *page.ResolvedZone
			if err := c.cloudStore.setSyncStateSuccess(ctx, statusKitCloudZoneRow, &zoneStr); err != nil {
				log.Info().Err(err).Msg("StatusKit-CloudKit pass: failed to persist resolved zone name")
			} else {
				log.Info().Str("zone", zoneStr).Msg("StatusKit-CloudKit pass: cached resolved zone name for future passes")
			}
			currentZone = &zoneStr
		}

		// Persist (or clear) the continuation token after every page so a
		// crash mid-drain doesn't lose progress — the next pass resumes
		// from the persisted token.
		if page.NextToken != nil && len(*page.NextToken) > 0 {
			encoded := base64.StdEncoding.EncodeToString(*page.NextToken)
			if err := c.cloudStore.setSyncStateSuccess(ctx, statusKitCloudTokenRow, &encoded); err != nil {
				log.Info().Err(err).Msg("StatusKit-CloudKit pass: failed to persist next continuation token")
			}
		} else if page.ResolvedZone == nil && currentToken != nil {
			// FFI signalled re-discovery. Drop the stale token so the
			// next pass starts fresh.
			if err := c.cloudStore.clearZoneToken(ctx, statusKitCloudTokenRow); err != nil {
				log.Info().Err(err).Msg("StatusKit-CloudKit pass: failed to clear stale continuation token")
			}
		}

		// Stop draining when there are no more pages, or when this page
		// returned no records (steady-state empty fetch).
		if page.NextToken == nil || len(*page.NextToken) == 0 || page.Fetched == 0 {
			break
		}
		currentToken = page.NextToken

		// Pace the next page fetch — keeps the per-pass CKKS round-trip
		// rate gentle even on a long initial drain.
		select {
		case <-time.After(interPageDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	log.Info().
		Uint32("total_fetched", totalFetched).
		Uint32("total_inserted", totalInserted).
		Uint32("total_already_known", totalAlreadyKnown).
		Uint32("total_decode_failed", totalDecodeFailed).
		Uint32("total_records_seen", totalRecordsSeen).
		Int("total_injected_handles", len(injectedHandles)).
		Msg("StatusKit-CloudKit pass: drain complete")

	// If we injected any new peer keys (across all drained pages), fire a
	// presence-resubscribe so APNs interest tokens cover the new channels
	// immediately rather than waiting for the next subscribeAfterInit cycle.
	if totalInserted > 0 {
		log.Info().Uint32("total_inserted", totalInserted).Msg("StatusKit-CloudKit pass: triggering presence resubscribe for newly-injected peers")
		c.subscribeToContactPresence(log)
	}

	return nil
}
