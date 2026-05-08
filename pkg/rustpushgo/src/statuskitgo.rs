// Thin wrapper that binds the bridge's StatusKit-invite flow to upstream's
// `sk.invite_to_channel`. Mirrors OB's chat-open prelude
// (chat_manager.dart:64-78): madrid IDS query first, then a per-handle
// presence-channel interest token, then the keysharing send. Skipping the
// first two leaves peer iOS treating the inviting account as an inactive
// peer at the moment of invite, which empirically gates unprompted
// reshare. The existing strong-flag keysharing pre-warm is kept because
// it serves the "is peer reachable on keysharing sub-service" check that
// produces NoValidTargets for unregistered peers.

use std::collections::HashMap;
use std::sync::Arc;

use log::info;
use omnisette::AnisetteProvider;

use rustpush::{
    ids::user::QueryOptions,
    statuskit::{StatusKitClient, StatusKitPersonalConfig},
    PushError,
};

const MADRID_TOPIC: &str = "com.apple.madrid";
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
    // OB chat-open step 1: weak-flag madrid IDS query. Matches
    // `validate_targets("com.apple.madrid", participants, ensureHandle())`
    // at chat_manager.dart:68. The semantic signal to peer iOS is that this
    // sender just looked up your iMessage keys — the same evidence-of-active-
    // conversation peer iOS appears to evaluate when deciding whether to
    // reshare keysharing unprompted.
    sk.identity
        .cache_keys(
            MADRID_TOPIC,
            handles,
            sender_handle,
            false,
            &QueryOptions {
                required_for_message: false,
                result_expected: false,
            },
        )
        .await?;

    // OB chat-open step 2: hold a per-handle presence-channel interest
    // token across the invite send. Matches `requestHandles(to: [participants[0]])`
    // at chat_manager.dart:74. For first-time peers (no state.keys entry
    // yet) this resolves to an empty topic list — request_handles filters
    // state.keys by handle, so a peer we have not yet keysharing'd with
    // produces a no-op token. For already-keyed peers it ensures the APNs
    // interest matches what OB's chat-open lifecycle establishes. The
    // token drops at the end of this function (after invite_to_channel
    // returns) — same lifetime as a single OB chat-open transaction.
    let _interest = sk.request_handles(handles).await;

    // OB chat-open step 3 prelude (bridge-only): strong-flag keysharing
    // cache to surface NoValidTargets for peers not registered on the
    // keysharing sub-service. invite_to_channel re-invokes cache_keys
    // with weak flags internally; that call becomes idempotent once our
    // strong-primed entries are present.
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

    // Zero targets means the peer is not registered for the keysharing
    // sub-service right now. Return an error so the caller can move on to
    // the next handle; the 4h periodic re-invite tick will retry later
    // (giving peer iOS time to naturally re-register if they add keysharing
    // via a Focus toggle or similar).
    //
    // Prior versions tried a cache-invalidate-and-retry here, which either
    // corrupted the cache structure (panic in put_keys) or was redundant
    // (same IDS result on the second try). Simpler to let the tick handle
    // it.
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
    info!(
        "StatusKit: invite_to_channel returned Ok for {} handle(s) — IDS dispatched, peer reciprocation NOT yet confirmed",
        handles.len()
    );
    Ok(targets.len())
}
