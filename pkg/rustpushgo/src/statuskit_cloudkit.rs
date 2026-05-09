//! StatusKit peer-key CloudKit pull (bridge-side).
//!
//! Goal: hydrate StatusKit peer keys from the user's iCloud private database
//! instead of waiting for an APNs reshare from the peer iPhone. APNs reshare
//! is too unreliable in bridge-only-cluster setups.
//!
//! ## Status: Phase 2 — full pull → decrypt → inject pipeline
//!
//! Phase 1 (Session 14) confirmed the container/zone:
//!
//!   bundleid       = "com.apple.StatusKitAgent"
//!   containerid    = "com.apple.statuskit"
//!   database       = PrivateDb (production)
//!   zone           = "com.apple.coredata.cloudkit.zone"
//!
//! Identified by `codesign --entitlements -` on
//! `/System/Library/PrivateFrameworks/StatusKit.framework/StatusKitAgent`
//! (the daemon hosting the `com.apple.aps.StatusKit.CloudKitMirroring` mach
//! service). The zone is the standard NSPersistentCloudKitContainer Core Data
//! mirroring zone — every record-type starts with `CD_`.
//!
//! Schema (confirmed via inline schema dump):
//!
//!   CD_ReceivedInvitation (the one we care about):
//!     CD_senderHandle               string         peer handle (mailto:/tel:)
//!     CD_invitationIdentifier       string         invite UUID
//!     CD_invitationPayload          encrypted_bytes  placeholder bplist `{"a":[]}`
//!                                                    on every record observed —
//!                                                    NOT the key-bearing field
//!     CD_channel                    string         FK → CD_Channel.record_id
//!     CD_statusTypeIdentifier       string         "com.apple.focus.status"
//!     CD_dateInvitationCreated      date
//!     CD_invitedHandle              string         our handle
//!     CD_entityName                 string
//!     CD_incomingRatchetState       encrypted_bytes  ← THE PCS-PROTECTED PAYLOAD
//!                                                    (raw SharedMessage protobuf:
//!                                                    SharedKeys with peer pub_bytes
//!                                                    + sigKey for the channel)
//!
//!   CD_Channel:
//!     CD_channelType                int64
//!     CD_personal                   int64
//!     CD_decomissioned              int64
//!     CD_identifier                 string         the actual base64 channel-id
//!     CD_statusType                 string
//!     CD_entityName                 string
//!     (extended): CD_dateChannelCreated date,
//!                 CD_currentOutgoingRatchetState encrypted_bytes,
//!                 CD_channelToken                encrypted_bytes
//!
//! ## Decryption flow
//!
//! For each `CD_ReceivedInvitation` record:
//!   1. Build a per-record PCS encryptor (zone-key + record protection_info).
//!   2. Decrypt `CD_senderHandle` (encrypted STRING_TYPE → plaintext is an
//!      `EncryptedValue` proto wrapping the actual string).
//!   3. Decrypt `CD_channel` (same shape — gives the CD_Channel record_id).
//!   4. Decrypt `CD_incomingRatchetState` (ENCRYPTED_BYTES_TYPE → raw bytes
//!      of a bare `SharedMessage` protobuf). NSPersistentCloudKitContainer
//!      splits sender/channel/etc. into their own Core Data fields, so the
//!      ratchet-state field carries only the cryptographic material — it
//!      is **not** the IDS-format `StatusKitRawSharedDevice` plist (that
//!      shape is wire-only for cmd 224/225 reshares). The differently-
//!      named `CD_invitationPayload` is a placeholder bplist `{"a":[]}` on
//!      every record observed; it does NOT carry the keys.
//!   5. Look up the channel base64 id via the CD_Channel.CD_identifier map
//!      built from the same fetch (each CD_Channel needs its own per-record
//!      PCS encryptor because they each carry their own `protection_info`).
//!   6. Reconstruct `StatusKitSharedDevice` via plist serialization
//!      round-trip (sender + sigKey-as-DER + protobuf-encoded SharedKey
//!      blobs + empty StatusKitPersonalConfig).
//!
//! ## Why a plist round-trip rather than a direct constructor
//!
//! `StatusKitSharedDevice` has private fields and no public constructor in
//! upstream rustpush. The fields use custom serde adapters (DER-encoded EC
//! key, protobuf-encoded SharedKey blobs). Rather than patching upstream,
//! we synthesize the canonical serialized form (matching the adapters'
//! output shape) and let `plist::from_bytes::<StatusKitSharedDevice>` do
//! the construction for us.
//!
//! ## Logging
//!
//! Everything load-bearing is at `log::info!`. Grep for `StatusKit-CloudKit`
//! to find all output from this module.

use std::collections::HashMap;
use std::io::Cursor;

use base64::{engine::general_purpose::STANDARD as B64, Engine as _};
use log::info;
use openssl::{
    bn::{BigNum, BigNumContext},
    ec::{EcGroup, EcKey, EcPoint},
    nid::Nid,
};
use plist::Value as PlistValue;
use prost::Message;
use rustpush::{
    cloudkit::{
        pcs_keys_for_record, CloudKitContainer, CloudKitOpenContainer, CloudKitSession,
        FetchRecordChangesOperation, FetchZoneChangesOperation, NO_ASSETS,
    },
    cloudkit_proto::{self, record::field::value::Type as FieldType, CloudKitEncryptor},
    pcs::{PCSService, PCSShareProtection},
    statuskit::{
        statuskitp::{SharedKey, SharedMessage},
        StatusKitSharedDevice,
    },
};

use crate::{persist_plist_state, read_plist_state, subsystem_state_path, Client, WrappedError};

/// On-disk path for the persistent CD_Channel.record_id → CD_identifier
/// map. Lives in the same xdg_data_dir as `statuskit-state.plist` and
/// `facetime-state.plist`. CD_Channel and CD_ReceivedInvitation records
/// arrive in unrelated fetch pages — when an invitation references a
/// channel that landed on a different page, the in-pass map can't
/// resolve the FK and decode fails with "CD_channel FK '...' not
/// present". Persisting the map across passes converges decode_failed
/// to ~0 over a few cycles.
///
/// Pure local state — never sent to Apple, has zero impact on CKKS or
/// CloudKit request volume.
const STATUSKIT_CHANNEL_MAP_FILE: &str = "statuskit-cloud-channel-map.plist";

#[derive(Default, serde::Serialize, serde::Deserialize)]
struct PersistedChannelMap {
    #[serde(default)]
    entries: HashMap<String, String>,
}

/// PCS service constants for the StatusKit zone, transcribed verbatim from
/// `/System/Library/Preferences/ProtectedCloudStorage/Identities/com.apple.statuskit.plist`
/// on macOS:
///
///   ServiceName    = "com.apple.statuskit"
///   ServiceNumber  = 200
///   ViewHint       = "LimitedPeersAllowed"
///   Manatee        = true                   → zone = "Manatee"
///   Classic7       = false                  → v2 = true
///
/// Cross-checked against rustpush's existing FIND_MY_SERVICE constants
/// (which derive from /System/Library/Preferences/ProtectedCloudStorage/
/// Identities/com.apple.icloud.searchparty.plist with the same shape).
/// Without these, get_zone_encryption_config returns ShareKeyNotFound
/// because we'd be syncing the wrong keychain view ("Engram" instead of
/// "Manatee") and looking up the wrong service key id.
const STATUSKIT_SERVICE: PCSService = PCSService {
    name: "com.apple.statuskit",
    // ViewHint in the on-disk plist is "LimitedPeersAllowed" — that's the
    // KEYCHAIN VIEW the items live in. The `Manatee: true` flag is a
    // separate security-domain marker; for StatusKit the two diverge,
    // unlike FIND_MY where both fields are "Manatee".
    //
    // rustpush uses `zone` for both `sync_keychain(&[zone, "ProtectedCloudStorage"])`
    // (which view to sync from server) AND for the in-memory lookup
    // `keychain.items[service.zone]` (per pcs.rs:703). We need both
    // hitting "LimitedPeersAllowed" or the share-key lookup misses.
    view_hint: "LimitedPeersAllowed",
    zone: "LimitedPeersAllowed",
    r#type: 200,
    keychain_type: 200,
    v2: true,
    global_record: false,
};

/// Candidate CloudKit containers, ordered by likelihood. Index encoded in
/// the cached_zone string so subsequent passes skip discovery.
const CANDIDATE_CONTAINERS: &[CloudKitContainer<'static>] = &[
    CloudKitContainer {
        database_type: cloudkit_proto::request_operation::header::Database::PrivateDb,
        bundleid: "com.apple.StatusKitAgent",
        containerid: "com.apple.statuskit",
        env: cloudkit_proto::request_operation::header::ContainerEnvironment::Production,
    },
    CloudKitContainer {
        database_type: cloudkit_proto::request_operation::header::Database::SharedDb,
        bundleid: "com.apple.StatusKitAgent",
        containerid: "com.apple.statuskit",
        env: cloudkit_proto::request_operation::header::ContainerEnvironment::Production,
    },
];

/// Result of one CloudKit-pull pass for StatusKit peer keys.
#[derive(Debug, Clone, uniffi::Record)]
pub struct CloudSyncStatusKitPage {
    /// Encoded as "<container_idx>::<zone_name>" so subsequent passes can
    /// skip discovery and resume from `since_token`.
    pub resolved_zone: Option<String>,
    pub next_token: Option<Vec<u8>>,
    pub fetched: u32,
    pub inserted: u32,
    pub already_known: u32,
    pub decode_failed: u32,
    pub records_seen: u32,
    pub injected_handles: Vec<String>,
    pub discovery_summary: Option<String>,
}

impl CloudSyncStatusKitPage {
    fn empty(resolved_zone: Option<String>, summary: Option<String>) -> Self {
        Self {
            resolved_zone,
            next_token: None,
            fetched: 0,
            inserted: 0,
            already_known: 0,
            decode_failed: 0,
            records_seen: 0,
            injected_handles: Vec::new(),
            discovery_summary: summary,
        }
    }
}

#[uniffi::export(async_runtime = "tokio")]
impl Client {
    pub async fn cloud_sync_statuskit_peers(
        &self,
        cached_zone: Option<String>,
        since_token: Option<Vec<u8>>,
    ) -> Result<CloudSyncStatusKitPage, WrappedError> {
        info!(
            "StatusKit-CloudKit pass: starting cached_zone={:?} has_continuation_token={}",
            cached_zone,
            since_token.is_some()
        );

        let cm = self.get_or_init_cloud_messages_client().await?;

        // Decide which candidate(s) to try.
        let parsed_cache: Option<(usize, String)> = cached_zone
            .as_deref()
            .and_then(parse_cached_zone);
        let candidates_to_try: Vec<usize> = match &parsed_cache {
            Some((idx, _)) => vec![*idx],
            None => (0..CANDIDATE_CONTAINERS.len()).collect(),
        };
        let cached_zone_name: Option<String> = parsed_cache.map(|(_, n)| n);

        // Hold the winning `opened` container across the discovery loop and
        // the decode pass. Doing one ckAppInit per pass — instead of two —
        // halves the CKKS request volume per cycle, which directly reduces
        // the per-pass burst that today's testing iteration tripped Apple's
        // rate-limit defense on.
        let mut hit: Option<DiscoveryHit> = None;
        let mut opened_hit: Option<CloudKitOpenContainer<'_, _>> = None;
        for &idx in &candidates_to_try {
            let candidate = &CANDIDATE_CONTAINERS[idx];
            let scope = match candidate.database_type {
                cloudkit_proto::request_operation::header::Database::PrivateDb => "PrivateDb",
                cloudkit_proto::request_operation::header::Database::SharedDb => "SharedDb",
                cloudkit_proto::request_operation::header::Database::PublicDb => "PublicDb",
            };
            let label = format!(
                "{} / {} [{}]",
                candidate.bundleid, candidate.containerid, scope
            );
            let opened = match candidate.init(cm.client.clone()).await {
                Ok(c) => c,
                Err(e) => {
                    let cls = classify_error(&format!("{:?}", e));
                    info!(
                        "StatusKit-CloudKit pass: candidate[{}] INIT-{} ({}): {:?}",
                        idx, cls, label, e
                    );
                    // Init failure on a cached-path candidate is the same
                    // shape as DOSYNC/FETCH errors below — Apple-side
                    // signal against a previously-working configuration
                    // (auth/PET, container reachability, rate-limit). With
                    // a cached path, candidates_to_try is just this one
                    // candidate, so `continue` exits the loop into an
                    // empty success page that would apply the 12h floor.
                    // Propagate so the Go-side gate applies failure
                    // backoff. In discovery mode (no cached_zone_name)
                    // continuing to the next candidate is the correct
                    // fallback.
                    if cached_zone_name.is_some() {
                        return Err(WrappedError::GenericError {
                            msg: format!(
                                "StatusKit-CloudKit cached-path init failed (idx={}, INIT-{}): {:?}",
                                idx, cls, e
                            ),
                        });
                    }
                    continue;
                }
            };
            match try_fetch_zone(
                &opened,
                idx,
                &label,
                cached_zone_name.as_deref(),
                since_token.clone(),
            )
            .await
            {
                Ok(Some(h)) => {
                    hit = Some(h);
                    opened_hit = Some(opened);
                    break;
                }
                Ok(None) => {}
                Err(e) => {
                    // Cached-path Apple-side error. Surface as a real
                    // FFI error so the Go-side inter-pass backoff fires
                    // (failure backoff + retry-after honoring) rather
                    // than recording this as a clean no-records pass
                    // that would apply the 12h success floor.
                    info!(
                        "StatusKit-CloudKit pass: candidate[{}] aborting due to cached-path error: {}",
                        idx, e
                    );
                    return Err(WrappedError::GenericError {
                        msg: format!(
                            "StatusKit-CloudKit cached-path failed (idx={}): {}",
                            idx, e
                        ),
                    });
                }
            }
        }

        let hit = match hit {
            Some(h) => h,
            None => {
                return Ok(CloudSyncStatusKitPage::empty(
                    None,
                    Some("no candidate container/zone produced records".into()),
                ));
            }
        };
        // Both Options are set together when a hit lands in the loop above,
        // so this expect() is unreachable in practice.
        let opened = opened_hit.expect("opened_hit set whenever hit is set");

        let mut decoded: Vec<DecodedPeer> = Vec::new();
        let mut decode_failed: u32 = 0;

        // Load persisted CD_Channel.record_id → CD_identifier map from
        // disk. Apple paginates invitations and their parent channels
        // separately, so an invitation on this page may reference a
        // channel that arrived several passes ago. Without persistence,
        // those FK lookups fail and the records are reported as
        // decode_failed even though the data is reachable.
        let channel_map_path = subsystem_state_path(STATUSKIT_CHANNEL_MAP_FILE);
        let persisted: PersistedChannelMap =
            read_plist_state(&channel_map_path).unwrap_or_default();

        // Build channel_id map (CD_Channel.record_id → CD_identifier). Each
        // CD_Channel record carries its own protection_info, so we decrypt
        // CD_identifier through a per-record PCS encryptor inside the helper.
        let in_pass_map = build_channel_id_map(&opened, &cm, &hit.records).await;

        // Union: persisted ∪ in-pass. In-pass wins on collision so a
        // freshly-decoded value overrides any stale persisted one (Apple
        // can roll channel ids; trust the newer fetch).
        let mut channel_map: HashMap<String, String> = persisted.entries.clone();
        for (k, v) in &in_pass_map {
            channel_map.insert(k.clone(), v.clone());
        }
        let persisted_size_before = persisted.entries.len();

        // Per-record decode failures are summarized in the DONE line at
        // the end. No per-record success/skip logging — we'd otherwise emit
        // hundreds of lines per pass.
        let mut failure_examples: Vec<String> = Vec::new();
        for rec in hit.records.iter() {
            let rec_type = record_type_name(rec).unwrap_or("<no-type>");
            if rec_type != "CD_ReceivedInvitation" {
                continue;
            }
            match decode_invitation_record(self, &opened, &cm, rec, &channel_map).await {
                Ok(Some(p)) => decoded.push(p),
                Ok(None) => {}
                Err(e) => {
                    decode_failed += 1;
                    if failure_examples.len() < 3 {
                        failure_examples.push(e);
                    }
                }
            }
        }

        let inject_stats = inject_into_state(self, decoded).await?;

        // Persist the merged map for the next pass. Skip the write when
        // nothing changed — avoids touching disk on a pass with no new
        // CD_Channel records and an unchanged persisted set.
        let channel_map_grew = channel_map.len() > persisted_size_before;
        if channel_map_grew {
            let to_persist = PersistedChannelMap {
                entries: channel_map.clone(),
            };
            persist_plist_state(&channel_map_path, &to_persist);
        }

        info!(
            "StatusKit-CloudKit pass: DONE container_idx={} zone='{}' fetched={} channel_map={} (persisted={}, in_pass={}) inserted={} already_known={} decode_failed={} failure_examples={:?} injected_handles={:?}",
            hit.container_idx,
            hit.zone_name,
            hit.records.len(),
            channel_map.len(),
            persisted_size_before,
            in_pass_map.len(),
            inject_stats.inserted,
            inject_stats.already_known,
            decode_failed,
            failure_examples,
            inject_stats.injected_handles
        );

        Ok(CloudSyncStatusKitPage {
            resolved_zone: Some(format!("{}::{}", hit.container_idx, hit.zone_name)),
            next_token: hit.next_token,
            fetched: hit.records.len() as u32,
            inserted: inject_stats.inserted,
            already_known: inject_stats.already_known,
            decode_failed,
            records_seen: hit.records.len() as u32,
            injected_handles: inject_stats.injected_handles,
            discovery_summary: None,
        })
    }
}

/// Parse `"<idx>::<zone_name>"`. Backwards compatible: a bare zone name
/// (no `::`) is treated as missing — so any cached value from the Phase-1
/// build gets discarded and re-discovered.
fn parse_cached_zone(s: &str) -> Option<(usize, String)> {
    let (idx_str, zone) = s.split_once("::")?;
    let idx: usize = idx_str.parse().ok()?;
    if idx >= CANDIDATE_CONTAINERS.len() {
        return None;
    }
    Some((idx, zone.to_string()))
}

fn classify_error(dbg: &str) -> &'static str {
    if dbg.contains("missing field `cloudKitUserId`") {
        "BAD-CONTAINER-ID"
    } else if dbg.contains("InvalidBundleId") {
        "INVALID-BUNDLE"
    } else if dbg.contains("ZoneNotFound") {
        "ZONE-NOT-FOUND"
    } else if dbg.contains("NotSupported") {
        "NOT-SUPPORTED"
    } else {
        "OTHER"
    }
}

/// Holds the records pulled from a successful zone fetch. Threaded out of
/// discovery so the decode pipeline doesn't re-fetch.
struct DiscoveryHit {
    container_idx: usize,
    zone_name: String,
    records: Vec<cloudkit_proto::retrieve_changes_response::RecordChange>,
    next_token: Option<Vec<u8>>,
}

/// Open container has already been initialized. Lists zones, picks one
/// (cached name if provided, else the keyword-matching or first non-default
/// zone), and fetches records. Returns `Ok(Some(hit))` if records came back.
async fn try_fetch_zone<P: omnisette::AnisetteProvider>(
    opened: &CloudKitOpenContainer<'_, P>,
    container_idx: usize,
    label: &str,
    cached_zone_name: Option<&str>,
    since_token: Option<Vec<u8>>,
) -> Result<Option<DiscoveryHit>, String> {
    let zones = match FetchZoneChangesOperation::do_sync(opened, None).await {
        Ok((z, _tok)) => z,
        Err(e) => {
            let cls = classify_error(&format!("{:?}", e));
            info!(
                "StatusKit-CloudKit fetch: container='{}' DOSYNC-{}: {:?}",
                label, cls, e
            );
            // Zone-list error against a cached path is an Apple-side
            // back-off signal (rate-limit, trust eject, transient
            // server error). Propagate so the Go-side gate applies
            // failure backoff (15m → 30m → 1h → 2h, retry-after
            // honoring) instead of treating this as a clean empty
            // pass that would apply the 12h success floor and clear
            // the cached zone (forcing fresh-discovery next pass —
            // the burstiest pattern). In discovery mode (no
            // cached_zone_name) the outer loop's next-candidate
            // fallback is the right behavior, so still Ok(None).
            if cached_zone_name.is_some() {
                return Err(format!(
                    "DOSYNC-{} on cached container '{}': {:?}",
                    cls, label, e
                ));
            }
            return Ok(None);
        }
    };

    let mut zone_names: Vec<String> = Vec::new();
    for z in &zones {
        let zname = match z
            .identifier
            .as_ref()
            .and_then(|i| i.value.as_ref())
            .and_then(|v| v.name.as_deref())
        {
            Some(n) => n.to_string(),
            None => continue,
        };
        if zname != "_defaultZone" {
            zone_names.push(zname);
        }
    }

    // Pick zone(s) to try. If we have a cached name, prefer it; else try all.
    let try_zones: Vec<String> = match cached_zone_name {
        Some(name) if zone_names.iter().any(|z| z == name) => vec![name.to_string()],
        Some(_) => zone_names.clone(),
        None => zone_names.clone(),
    };

    for zname in try_zones {
        let is_cached_zone = cached_zone_name == Some(zname.as_str());
        let zone_id = opened.private_zone(zname.clone());
        let fetch_result = opened
            .perform(
                &CloudKitSession::new(),
                FetchRecordChangesOperation(cloudkit_proto::RetrieveChangesRequest {
                    sync_continuation_token: since_token.clone(),
                    zone_identifier: Some(zone_id),
                    requested_changes_types: Some(3),
                    assets_to_download: Some(NO_ASSETS.clone()),
                    newest_first: Some(true),
                    ..Default::default()
                }),
            )
            .await;
        match fetch_result {
            Ok((_assets, response)) => {
                let next_token = response.sync_continuation_token.clone();
                let records: Vec<_> = response.change.into_iter().collect();
                // Empty page on a discovery candidate (no cache, or trying a
                // non-cached fallback) means "this zone has nothing for us"
                // — fall through to the next candidate. Empty page on the
                // cached zone is a legitimate "no new changes since
                // since_token" response, so return it as a successful no-op
                // page that preserves zone+token state. Without this, the
                // Go-side ResolvedZone=None branch would clear the cached
                // zone row and force the next pass into fresh discovery.
                if records.is_empty() && !is_cached_zone {
                    continue;
                }
                return Ok(Some(DiscoveryHit {
                    container_idx,
                    zone_name: zname,
                    records,
                    next_token,
                }));
            }
            Err(e) => {
                let cls = classify_error(&format!("{:?}", e));
                info!(
                    "StatusKit-CloudKit fetch: container='{}' zone='{}' FETCH-{}: {:?}",
                    label, zname, cls, e
                );
                // Fetch error on the cached zone is an Apple-side
                // back-off signal — propagate so the Go-side gate
                // applies failure backoff. Errors against discovery
                // candidates (non-cached zones) still fall through
                // to the next zone in the list.
                if is_cached_zone {
                    return Err(format!(
                        "FETCH-{} on cached zone '{}' (container '{}'): {:?}",
                        cls, zname, label, e
                    ));
                }
            }
        }
    }
    Ok(None)
}

fn record_type_name(
    rec: &cloudkit_proto::retrieve_changes_response::RecordChange,
) -> Option<&str> {
    rec.record
        .as_ref()
        .and_then(|r| r.r#type.as_ref())
        .and_then(|t| t.name.as_deref())
}

fn record_id_name(
    rec: &cloudkit_proto::retrieve_changes_response::RecordChange,
) -> Option<&str> {
    rec.identifier
        .as_ref()
        .and_then(|i| i.value.as_ref())
        .and_then(|v| v.name.as_deref())
}

/// Map CD_Channel.record_id → CD_identifier (the base64 channel id used as
/// the key in `state.keys`). Used to translate a CD_ReceivedInvitation's
/// CD_channel foreign-key string into the actual channel id we'll insert
/// under in `statuskit-state.plist`.
///
/// `CD_identifier` is itself an encrypted STRING_TYPE field, and each
/// CD_Channel record carries its own `protection_info`, so we have to
/// build a per-record PCS encryptor to read it.
async fn build_channel_id_map<P: omnisette::AnisetteProvider>(
    opened: &CloudKitOpenContainer<'_, P>,
    cm: &rustpush::cloud_messages::CloudMessagesClient<P>,
    records: &[cloudkit_proto::retrieve_changes_response::RecordChange],
) -> HashMap<String, String> {
    let mut map = HashMap::new();
    for rec in records {
        if record_type_name(rec) != Some("CD_Channel") {
            continue;
        }
        let id = match record_id_name(rec) {
            Some(s) => s.to_string(),
            None => continue,
        };
        let record = match &rec.record {
            Some(r) => r,
            None => continue,
        };
        let zone_id = match rec
            .identifier
            .as_ref()
            .and_then(|i| i.zone_identifier.clone())
        {
            Some(z) => z,
            None => continue,
        };
        let zone_key = match opened
            .get_zone_encryption_config(&zone_id, &cm.keychain, &STATUSKIT_SERVICE)
            .await
        {
            Ok(k) => k,
            Err(_) => continue,
        };
        let encryptor = match std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            pcs_keys_for_record(record, &zone_key)
        })) {
            Ok(Ok(enc)) => enc,
            Ok(Err(_)) | Err(_) => match decrypt_with_keychain_fallback(record, cm).await {
                Some(enc) => enc,
                None => continue,
            },
        };
        if let Some(ident) = decrypt_string_field(rec, "CD_identifier", &encryptor) {
            map.insert(id, ident);
        }
    }
    map
}

/// Locate a record field by identifier name.
fn find_field<'a>(
    rec: &'a cloudkit_proto::retrieve_changes_response::RecordChange,
    name: &str,
) -> Option<&'a cloudkit_proto::record::Field> {
    let record = rec.record.as_ref()?;
    record.record_field.iter().find(|f| {
        f.identifier
            .as_ref()
            .and_then(|i| i.name.as_deref())
            == Some(name)
    })
}

/// Decrypt an encrypted STRING_TYPE field through the PCS encryptor.
/// Apple's Core Data CloudKit Mirroring marks every string field as
/// `is_encrypted=true` with the ciphertext stored in `bytes_value`. The
/// decrypted plaintext is an `EncryptedValue` proto whose `string_value`
/// is the actual string.
fn decrypt_string_field(
    rec: &cloudkit_proto::retrieve_changes_response::RecordChange,
    name: &str,
    encryptor: &rustpush::pcs::PCSEncryptor,
) -> Option<String> {
    let f = find_field(rec, name)?;
    let v = f.value.as_ref()?;
    // Plaintext fast-path.
    if let Some(s) = &v.string_value {
        if v.is_encrypted != Some(true) {
            return Some(s.clone());
        }
    }
    let ct = v.bytes_value.as_deref()?;
    let plaintext = match std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        encryptor.decrypt_data(ct, name)
    })) {
        Ok(b) if !b.is_empty() => b,
        Ok(_) => return None,
        Err(_) => return None,
    };
    let ev = cloudkit_proto::record::field::EncryptedValue::decode(&plaintext[..]).ok()?;
    ev.string_value
}

/// Decrypt an ENCRYPTED_BYTES_TYPE field through the PCS encryptor.
/// The plaintext is the raw decrypted bytes (no inner protobuf wrapper).
fn decrypt_bytes_field(
    rec: &cloudkit_proto::retrieve_changes_response::RecordChange,
    name: &str,
    encryptor: &rustpush::pcs::PCSEncryptor,
) -> Option<Vec<u8>> {
    let f = find_field(rec, name)?;
    let v = f.value.as_ref()?;
    if v.r#type != Some(FieldType::EncryptedBytesType as i32) {
        return None;
    }
    let ct = v.bytes_value.as_deref()?;
    match std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        encryptor.decrypt_data(ct, name)
    })) {
        Ok(b) if !b.is_empty() => Some(b),
        _ => None,
    }
}

/// Decoded peer ready for injection.
struct DecodedPeer {
    channel_id: String,
    from: String,
    device: StatusKitSharedDevice,
}

async fn decode_invitation_record<P: omnisette::AnisetteProvider>(
    _client: &Client,
    opened: &CloudKitOpenContainer<'_, P>,
    cm: &rustpush::cloud_messages::CloudMessagesClient<P>,
    rec: &cloudkit_proto::retrieve_changes_response::RecordChange,
    channel_map: &HashMap<String, String>,
) -> Result<Option<DecodedPeer>, String> {
    let record = match &rec.record {
        Some(r) => r,
        None => return Ok(None),
    };
    let record_id = rec
        .identifier
        .as_ref()
        .ok_or("record missing identifier")?
        .clone();
    let zone_id = record_id
        .zone_identifier
        .clone()
        .ok_or("record missing zone_identifier")?;

    let has_sender = find_field(rec, "CD_senderHandle").is_some();
    let has_channel = find_field(rec, "CD_channel").is_some();
    let has_payload = find_field(rec, "CD_invitationPayload").is_some();
    if !has_sender || !has_channel {
        return Ok(None);
    }

    // 10-field variant (CD_peerKey/CD_serverKey/CD_channelToken instead
    // of CD_invitationPayload + CD_incomingRatchetState) has no assembly
    // path yet — skip before any PCS work so we don't waste per-record
    // unwrap CPU on records we'll discard anyway.
    if !has_payload {
        return Ok(None);
    }

    let zone_key = match opened
        .get_zone_encryption_config(&zone_id, &cm.keychain, &STATUSKIT_SERVICE)
        .await
    {
        Ok(k) => k,
        Err(e) => {
            return Err(format!(
                "get_zone_encryption_config(STATUSKIT_SERVICE) failed: {:?}",
                e
            ));
        }
    };

    // Try pcs_keys_for_record first; if it panics or fails, silently fall
    // back to direct keychain decrypt. Mirrors cloud_sync_attachments
    // pattern. Only the final outcome surfaces in the DONE summary.
    let encryptor = match std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        pcs_keys_for_record(record, &zone_key)
    })) {
        Ok(Ok(enc)) => enc,
        Ok(Err(_)) | Err(_) => match decrypt_with_keychain_fallback(record, cm).await {
            Some(enc) => enc,
            None => return Err("PCS unwrap failed (both paths)".into()),
        },
    };

    // Decrypt the load-bearing fields through the encryptor.
    // - CD_senderHandle / CD_channel: encrypted STRING_TYPE (is_encrypted,
    //   ciphertext in bytes_value, plaintext is an EncryptedValue proto
    //   whose string_value is the actual string).
    // - CD_incomingRatchetState: ENCRYPTED_BYTES_TYPE; the plaintext is a
    //   raw `SharedMessage` protobuf carrying the peer's `sigKey` and
    //   `keys[].pub_bytes`. This is the field NSPersistentCloudKitContainer
    //   actually populates with key material — `CD_invitationPayload`
    //   turned out to be a placeholder bplist `{"a":[]}` on every record
    //   we observed.
    let sender_handle = match decrypt_string_field(rec, "CD_senderHandle", &encryptor) {
        Some(s) => s,
        None => return Err("decrypt CD_senderHandle returned None".into()),
    };
    let channel_fk = match decrypt_string_field(rec, "CD_channel", &encryptor) {
        Some(s) => s,
        None => return Err("decrypt CD_channel returned None".into()),
    };
    let plaintext = match decrypt_bytes_field(rec, "CD_incomingRatchetState", &encryptor) {
        Some(b) => b,
        None => return Err("decrypt CD_incomingRatchetState returned None".into()),
    };
    let share_message = SharedMessage::decode(Cursor::new(&plaintext))
        .map_err(|e| format!("SharedMessage::decode failed: {:?}", e))?;

    if share_message.sig_key.len() != 32 {
        return Err(format!(
            "unexpected sig_key length {}",
            share_message.sig_key.len()
        ));
    }

    // Resolve channel_id via the CD_Channel.CD_identifier map. The base64
    // channel id is the key under which `state.keys` indexes peer devices,
    // so without this lookup we have no way to file the entry.
    let channel_id = match channel_map.get(&channel_fk) {
        Some(s) => s.clone(),
        None => {
            return Err(format!(
                "CD_channel FK '{}' not present in channel_map ({} entries)",
                channel_fk,
                channel_map.len()
            ));
        }
    };

    // CloudKit doesn't store the personal_config string (it lives only on
    // the live IDS payload). Pass empty — `build_shared_device` synthesizes
    // an empty StatusKitPersonalConfig in that case.
    let device = build_shared_device(sender_handle.clone(), &share_message, "")?;

    Ok(Some(DecodedPeer {
        channel_id,
        from: sender_handle,
        device,
    }))
}

/// Keychain-decrypt fallback that cloud_sync_attachments uses when
/// `pcs_keys_for_record` panics or errors. Returns None on any failure;
/// the surrounding decode reports the failure via the DONE summary.
async fn decrypt_with_keychain_fallback<P: omnisette::AnisetteProvider>(
    record: &cloudkit_proto::Record,
    cm: &rustpush::cloud_messages::CloudMessagesClient<P>,
) -> Option<rustpush::pcs::PCSEncryptor> {
    let protection = record.protection_info.as_ref()?;
    let record_protection = PCSShareProtection::from_protection_info(protection);
    let keychain_state = cm.keychain.state.read().await;
    let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        record_protection.decrypt_with_keychain(&keychain_state, &STATUSKIT_SERVICE, false)
    }));
    match result {
        Ok(Ok((pcs_keys, _))) => {
            let record_id = record.record_identifier.clone()?;
            Some(rustpush::pcs::PCSEncryptor {
                keys: pcs_keys,
                record_id,
            })
        }
        _ => None,
    }
}

/// Reconstruct a `StatusKitSharedDevice` from its IDS-payload representation
/// via plist serialization round-trip. Avoids needing public field
/// access on the upstream type.
fn build_shared_device(
    sender: String,
    share_message: &SharedMessage,
    personal_config_b64: &str,
) -> Result<StatusKitSharedDevice, String> {
    // signature: convert 32-byte X9.62 raw → 33-byte compressed → DER bytes.
    let sig_der = compact_pubkey_to_der(&share_message.sig_key)
        .map_err(|e| format!("compact_pubkey_to_der: {:?}", e))?;

    // keys: each SharedKey re-encoded as protobuf; wrapped as plist::Data.
    let keys_protos: Vec<Vec<u8>> = match &share_message.keys {
        Some(k) => k.keys.iter().map(SharedKey::encode_to_vec).collect(),
        None => return Err("SharedMessage missing inner SharedKeys".into()),
    };

    // personal_config: deserialize from base64-of-plist into a plist::Value
    // so we can embed it nested in the synthetic device plist.
    let personal_config_value: PlistValue = if personal_config_b64.is_empty() {
        // Some payloads send an empty `p` (no personal config); StatusKitPersonalConfig
        // has `#[serde(default)]` on its only field so an empty dict deserializes fine.
        PlistValue::Dictionary(plist::Dictionary::new())
    } else {
        let pc_bytes = B64
            .decode(personal_config_b64)
            .map_err(|e| format!("base64 decode personal_config: {:?}", e))?;
        plist::from_bytes(&pc_bytes)
            .map_err(|e| format!("plist::from_bytes(personal_config): {:?}", e))?
    };

    // Build the canonical serialized form of StatusKitSharedDevice.
    let mut dict = plist::Dictionary::new();
    dict.insert("from".into(), PlistValue::String(sender));
    dict.insert(
        "signature".into(),
        PlistValue::Data(sig_der),
    );
    dict.insert(
        "keys".into(),
        PlistValue::Array(keys_protos.into_iter().map(PlistValue::Data).collect()),
    );
    dict.insert("personal_config".into(), personal_config_value);

    let device_value = PlistValue::Dictionary(dict);
    let mut buf = Vec::new();
    plist::to_writer_binary(Cursor::new(&mut buf), &device_value)
        .map_err(|e| format!("plist::to_writer_binary: {:?}", e))?;
    plist::from_bytes::<StatusKitSharedDevice>(&buf)
        .map_err(|e| format!("plist::from_bytes(StatusKitSharedDevice) round-trip: {:?}", e))
}

/// Take a 32-byte X9.62 raw EC public key (P-256, "compact" form: just the
/// X coord) and convert to X9.62 DER. Mirrors upstream `CompactECKey::
/// decompress(...).public_key_to_der()` exactly — including the parity
/// flip that enforces the `2y < p` compact-key invariant.
///
/// Without the flip, the round-trip fails upstream's `try_from(EcKey)`
/// validation with `BadCompactECKey` (util.rs:1664-1681) for ~half of all
/// inputs (those whose canonical decompressed y lands in the upper half
/// of the field). The flip negates y (y → p - y) when needed.
fn compact_pubkey_to_der(raw_32: &[u8]) -> Result<Vec<u8>, openssl::error::ErrorStack> {
    let mut compressed = [0u8; 33];
    compressed[0] = 0x03;
    compressed[1..].copy_from_slice(raw_32);
    let group = EcGroup::from_curve_name(Nid::X9_62_PRIME256V1)?;
    let mut ctx = BigNumContext::new()?;
    let mut point = EcPoint::from_bytes(&group, &compressed, &mut ctx)?;

    let mut x = BigNum::new()?;
    let mut y = BigNum::new()?;
    let mut p = BigNum::new()?;
    let mut a_unused = BigNum::new()?;
    let mut b_unused = BigNum::new()?;
    group.components_gfp(&mut p, &mut a_unused, &mut b_unused, &mut ctx)?;
    point.affine_coordinates(&group, &mut x, &mut y, &mut ctx)?;

    let mut doubled = BigNum::new()?;
    doubled.checked_add(&y, &y)?;
    if doubled >= p {
        let mut flipped = BigNum::new()?;
        flipped.checked_sub(&p, &y)?;
        point.set_affine_coordinates_gfp(&group, &x, &flipped, &mut ctx)?;
    }

    let key = EcKey::from_public_key(&group, &point)?;
    key.public_key_to_der()
}

struct InjectStats {
    inserted: u32,
    already_known: u32,
    injected_handles: Vec<String>,
}

async fn inject_into_state(
    client: &Client,
    peers: Vec<DecodedPeer>,
) -> Result<InjectStats, WrappedError> {
    if peers.is_empty() {
        return Ok(InjectStats {
            inserted: 0,
            already_known: 0,
            injected_handles: Vec::new(),
        });
    }

    let sk = client.get_or_init_statuskit_client().await?;
    let mut state = sk.inner.state.write().await;

    let mut inserted: u32 = 0;
    let mut already_known: u32 = 0;
    let mut injected_handles: Vec<String> = Vec::new();

    for p in peers {
        if state.keys.contains_key(&p.channel_id) {
            already_known += 1;
            continue;
        }
        injected_handles.push(p.from.clone());
        state.keys.insert(p.channel_id, p.device);
        inserted += 1;
    }

    if inserted > 0 {
        let path = subsystem_state_path("statuskit-state.plist");
        info!(
            "StatusKit-CloudKit inject: persisting {} new peer(s) to {}",
            inserted, path
        );
        persist_plist_state(&path, &*state);
    }

    Ok(InjectStats {
        inserted,
        already_known,
        injected_handles,
    })
}
