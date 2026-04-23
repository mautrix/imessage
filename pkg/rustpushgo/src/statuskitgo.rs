// Thin wrapper that binds the bridge's StatusKit-invite flow to upstream's
// `sk.invite_to_channel`. All the real work — batch construction, per-target
// encryption, c=255 collection, 5032 refresh_handles retry — stays inside
// upstream's InnerSendJob::send_targets. This file's only job is to prime
// the IDS cache with strong QueryOptions before handing off, so upstream's
// own weak-flag `cache_keys` call inside `invite_to_channel` doesn't see
// stale empty cache entries.

use std::collections::HashMap;
use std::sync::Arc;

use log::{info, warn};
use omnisette::AnisetteProvider;

use rustpush::{
    statuskit::{StatusKitClient, StatusKitPersonalConfig},
    PushError,
};

const KEYSHARING_TOPIC: &str = "com.apple.private.alloy.status.keysharing";

/// Send StatusKit keysharing invites to `handles` from `sender_handle`.
/// Returns `Ok(n)` with the count of IDS delivery targets the invite was
/// dispatched to, or `Err` if the IDS cache never resolves any targets
/// (peers not registered for the keysharing sub-service) or the upstream
/// send itself fails.
pub async fn invite_keysharing<T: AnisetteProvider + Send + Sync + 'static>(
    sk: &Arc<StatusKitClient<T>>,
    sender_handle: &str,
    handles: &[String],
) -> Result<usize, PushError> {
    // Prime IDS cache with strong flags (required_for_message=true,
    // result_expected=true). invite_to_channel internally re-invokes
    // cache_keys with refresh=false and weak flags; that call becomes
    // idempotent once our strong-primed entries are present.
    let targets = sk
        .identity
        .targets_for_handles(KEYSHARING_TOPIC, handles, sender_handle)
        .await?;
    let reachable: std::collections::HashSet<&str> =
        targets.iter().map(|t| t.participant.as_str()).collect();
    let unreachable_handles: Vec<&str> = handles
        .iter()
        .map(|h| h.as_str())
        .filter(|h| !reachable.contains(h))
        .collect();
    info!(
        "StatusKit: IDS found {} delivery target(s) for {}/{} handle(s) ({} unreachable)",
        targets.len(),
        reachable.len(),
        handles.len(),
        unreachable_handles.len()
    );
    if !unreachable_handles.is_empty() {
        info!(
            "StatusKit: unreachable handles (peer not registered for keysharing sub-service): {:?}",
            unreachable_handles
        );
    }

    // Zero targets usually means the cache still has stale-empty entries
    // from a prior session (dirty cutoff is ~1 hour). One invalidate + retry
    // forces a live IDS query.
    //
    // Scoped to the keysharing topic only — upstream's `invalidate_id_cache`
    // (identity_manager.rs:910) calls `invalidate_all` (line 256) which wipes
    // MADRID's key cache too, forcing an IDS re-query before the next
    // iMessage send. Dropping just the keysharing entry from the cache map
    // is functionally equivalent for this service (no private_data/env_hash
    // to preserve — those are MADRID-only per line 636/650/662/669) and
    // leaves iMessage's cache untouched.
    let targets = if targets.is_empty() {
        warn!(
            "StatusKit: zero targets for keysharing topic — invalidating keysharing cache and retrying"
        );
        {
            let mut cache_guard = sk.identity.cache.lock().await;
            cache_guard.cache.remove(KEYSHARING_TOPIC);
            cache_guard.save();
        }
        sk.identity
            .targets_for_handles(KEYSHARING_TOPIC, handles, sender_handle)
            .await?
    } else {
        targets
    };
    if targets.is_empty() {
        return Err(PushError::NoValidTargets);
    }

    // allowed_modes empty matches TPP-confirmed-working 31ad87b. Populating
    // it with hard-coded Focus mode IDs (as f079364 did) appears to cause
    // peer iOS to silently drop the invite.
    let config_map: HashMap<String, StatusKitPersonalConfig> = handles
        .iter()
        .map(|h| {
            (
                h.clone(),
                StatusKitPersonalConfig {
                    allowed_modes: vec![],
                },
            )
        })
        .collect();

    info!(
        "StatusKit: invoking upstream invite_to_channel ({} handle(s), {} target(s))",
        handles.len(),
        targets.len()
    );
    sk.invite_to_channel(sender_handle, config_map).await?;
    Ok(targets.len())
}
