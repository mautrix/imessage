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
//!     CD_invitationPayload          encrypted_bytes  ← THE PCS-PROTECTED PAYLOAD
//!     CD_channel                    string         FK → CD_Channel.record_id
//!     CD_statusTypeIdentifier       string         "com.apple.focus.status"
//!     CD_dateInvitationCreated      date
//!     CD_invitedHandle              string         our handle
//!     CD_entityName                 string
//!     CD_incomingRatchetState       encrypted_bytes  (separate ratchet — not used for keysharing)
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
//! `CD_invitationPayload` is the same plist-encoded
//! `StatusKitRawSharedDevice` blob the peer iPhone received via APNs (cmd
//! 224/225 keysharing reshare). PCS-unwrap it → plist::from_bytes →
//! base64-decode the embedded `keys` field → SharedMessage protobuf →
//! reconstruct StatusKitSharedDevice via plist serialization round-trip.
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
    bn::BigNumContext,
    ec::{EcGroup, EcKey, EcPoint},
    nid::Nid,
};
use plist::Value as PlistValue;
use prost::Message;
use rustpush::{
    cloud_messages::MESSAGES_SERVICE,
    cloudkit::{
        pcs_keys_for_record, CloudKitContainer, CloudKitOpenContainer, CloudKitSession,
        FetchRecordChangesOperation, FetchZoneChangesOperation, NO_ASSETS,
    },
    cloudkit_proto::{self, record::field::value::Type as FieldType, CloudKitEncryptor},
    pcs::PCSShareProtection,
    statuskit::{
        statuskitp::{SharedKey, SharedMessage},
        StatusKitSharedDevice,
    },
};
use serde::{Deserialize, Serialize};

use crate::{persist_plist_state, subsystem_state_path, Client, WrappedError};

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

        let mut hit: Option<DiscoveryHit> = None;
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
            info!("StatusKit-CloudKit pass: probing candidate[{}] ({})", idx, label);
            let opened = match candidate.init(cm.client.clone()).await {
                Ok(c) => c,
                Err(e) => {
                    let cls = classify_error(&format!("{:?}", e));
                    info!(
                        "StatusKit-CloudKit pass: candidate[{}] INIT-{} ({}): {:?}",
                        idx, cls, label, e
                    );
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
                    info!(
                        "StatusKit-CloudKit pass: HIT candidate[{}] zone='{}' record_count={}",
                        idx,
                        h.zone_name,
                        h.records.len()
                    );
                    hit = Some(h);
                    break;
                }
                Ok(None) => {
                    info!(
                        "StatusKit-CloudKit pass: candidate[{}] returned no usable records",
                        idx
                    );
                }
                Err(e) => {
                    info!(
                        "StatusKit-CloudKit pass: candidate[{}] fetch error: {}",
                        idx, e
                    );
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

        // Decode + inject. We need the opened container in scope for PCS
        // unwrap (zone-level encryption config lookup), so re-open it.
        let candidate = &CANDIDATE_CONTAINERS[hit.container_idx];
        let opened = candidate
            .init(cm.client.clone())
            .await
            .map_err(|e| WrappedError::GenericError {
                msg: format!("StatusKit-CloudKit re-open container failed: {:?}", e),
            })?;

        let mut decoded: Vec<DecodedPeer> = Vec::new();
        let mut decode_failed: u32 = 0;

        // Build channel_id map (CD_Channel.record_id → CD_identifier).
        let channel_map = build_channel_id_map(&hit.records);
        info!(
            "StatusKit-CloudKit pass: built channel_id map with {} CD_Channel record(s)",
            channel_map.len()
        );

        for (idx, rec) in hit.records.iter().enumerate() {
            let rec_type = record_type_name(rec).unwrap_or("<no-type>");
            if rec_type != "CD_ReceivedInvitation" {
                continue;
            }
            log_record_schema(idx, &hit.zone_name, rec);
            match decode_invitation_record(self, &opened, &cm, rec, &channel_map).await {
                Ok(Some(p)) => {
                    info!(
                        "StatusKit-CloudKit decode[{}]: zone='{}' OK from='{}' channel_b64_len={}",
                        idx,
                        hit.zone_name,
                        p.from,
                        p.channel_id.len()
                    );
                    decoded.push(p);
                }
                Ok(None) => {
                    info!(
                        "StatusKit-CloudKit decode[{}]: zone='{}' SKIPPED (see preceding log line)",
                        idx, hit.zone_name
                    );
                }
                Err(e) => {
                    decode_failed += 1;
                    info!(
                        "StatusKit-CloudKit decode[{}]: zone='{}' FAILED: {}",
                        idx, hit.zone_name, e
                    );
                }
            }
        }

        let inject_stats = inject_into_state(self, decoded).await?;

        info!(
            "StatusKit-CloudKit pass: DONE container_idx={} zone='{}' fetched={} inserted={} already_known={} decode_failed={} injected_handles={:?}",
            hit.container_idx,
            hit.zone_name,
            hit.records.len(),
            inject_stats.inserted,
            inject_stats.already_known,
            decode_failed,
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
            return Ok(None);
        }
    };
    info!(
        "StatusKit-CloudKit fetch: container='{}' returned {} zone(s)",
        label,
        zones.len()
    );

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
        info!(
            "StatusKit-CloudKit fetch: container='{}' zone='{}' change_type={:?}",
            label, zname, z.change_type
        );
        if zname != "_defaultZone" {
            zone_names.push(zname);
        }
    }

    // Pick zone(s) to try. If we have a cached name, prefer it; else try all.
    let try_zones: Vec<String> = match cached_zone_name {
        Some(name) if zone_names.iter().any(|z| z == name) => vec![name.to_string()],
        Some(name) => {
            info!(
                "StatusKit-CloudKit fetch: cached zone='{}' not found in container — falling back to discovery",
                name
            );
            zone_names.clone()
        }
        None => zone_names.clone(),
    };

    for zname in try_zones {
        let zone_id = opened.private_zone(zname.clone());
        info!(
            "StatusKit-CloudKit fetch: container='{}' zone='{}' attempting record fetch (has_token={})",
            label,
            zname,
            since_token.is_some()
        );
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
                let status = response.status.unwrap_or(0);
                let next_token = response.sync_continuation_token.clone();
                let records: Vec<_> = response.change.into_iter().collect();
                info!(
                    "StatusKit-CloudKit fetch: container='{}' zone='{}' status={} record_count={} next_token_present={}",
                    label,
                    zname,
                    status,
                    records.len(),
                    next_token.is_some()
                );
                if records.is_empty() {
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

fn log_record_schema(
    idx: usize,
    zone: &str,
    rec: &cloudkit_proto::retrieve_changes_response::RecordChange,
) {
    let id = record_id_name(rec).unwrap_or("<no-name>");
    let rec_type = record_type_name(rec).unwrap_or("<no-type>");
    let has_protection = rec
        .record
        .as_ref()
        .map(|r| r.protection_info.is_some())
        .unwrap_or(false);
    let field_count = rec
        .record
        .as_ref()
        .map(|r| r.record_field.len())
        .unwrap_or(0);
    info!(
        "StatusKit-CloudKit schema[{}]: zone='{}' record_id='{}' record_type='{}' has_protection_info={} field_count={}",
        idx, zone, id, rec_type, has_protection, field_count
    );
}

/// Map CD_Channel record_id → CD_identifier (the base64 channel id used as
/// the key in `state.keys`). Used to translate a CD_ReceivedInvitation's
/// CD_channel foreign key into the actual channel id.
fn build_channel_id_map(
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
        if let Some(ident) = extract_string_field(rec, "CD_identifier") {
            map.insert(id, ident);
        }
    }
    map
}

/// Find a string-valued field on the record. Handles the unencrypted
/// STRING_TYPE case only — encrypted strings need PCS unwrap.
fn extract_string_field(
    rec: &cloudkit_proto::retrieve_changes_response::RecordChange,
    name: &str,
) -> Option<String> {
    let record = rec.record.as_ref()?;
    for f in &record.record_field {
        let fname = f.identifier.as_ref().and_then(|i| i.name.as_deref());
        if fname == Some(name) {
            let v = f.value.as_ref()?;
            return v.string_value.clone();
        }
    }
    None
}

/// Find an encrypted-bytes field's raw ciphertext bytes.
fn extract_encrypted_bytes_field<'a>(
    rec: &'a cloudkit_proto::retrieve_changes_response::RecordChange,
    name: &str,
) -> Option<&'a [u8]> {
    let record = rec.record.as_ref()?;
    for f in &record.record_field {
        let fname = f.identifier.as_ref().and_then(|i| i.name.as_deref());
        if fname == Some(name) {
            let v = f.value.as_ref()?;
            // EncryptedBytesType=20 (FieldType::EncryptedBytesType)
            if v.r#type == Some(FieldType::EncryptedBytesType as i32) {
                return v.bytes_value.as_deref();
            }
        }
    }
    None
}

/// Decoded peer ready for injection.
struct DecodedPeer {
    channel_id: String,
    from: String,
    device: StatusKitSharedDevice,
}

/// Mirror of the private upstream `StatusKitRawSharedDevice` used as the
/// IDS-payload wire format for keysharing reshares (cmd 224). The same
/// plist is what the iPhone stored in `CD_invitationPayload`.
#[derive(Serialize, Deserialize, Debug, Clone)]
struct InvitationPayloadMirror {
    #[serde(rename = "r")]
    keys: String, // base64 of SharedMessage protobuf
    #[serde(rename = "d")]
    time_sent_s: f64,
    #[serde(rename = "p")]
    personal_config: String, // base64 of plist of StatusKitPersonalConfig
    #[serde(rename = "s")]
    bundle: String,
    #[serde(rename = "c")]
    channel: String, // base64 channel id
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

    let sender_handle = extract_string_field(rec, "CD_senderHandle");
    let channel_fk = extract_string_field(rec, "CD_channel");
    let payload_ct = extract_encrypted_bytes_field(rec, "CD_invitationPayload");
    if sender_handle.is_none() || channel_fk.is_none() || payload_ct.is_none() {
        info!(
            "StatusKit-CloudKit decode: missing required fields (sender={}, channel_fk={}, payload={})",
            sender_handle.is_some(),
            channel_fk.is_some(),
            payload_ct.is_some()
        );
        return Ok(None);
    }
    let sender_handle = sender_handle.unwrap();
    let channel_fk = channel_fk.unwrap();
    let payload_ct = payload_ct.unwrap();

    // PCS-unwrap CD_invitationPayload.
    let zone_key = match opened
        .get_zone_encryption_config(&zone_id, &cm.keychain, &MESSAGES_SERVICE)
        .await
    {
        Ok(k) => k,
        Err(e) => {
            return Err(format!(
                "get_zone_encryption_config(MESSAGES_SERVICE) failed — likely a different PCS service is needed for StatusKit: {:?}",
                e
            ));
        }
    };

    // Try pcs_keys_for_record first; if it panics or fails, fall back to
    // direct keychain decrypt. Mirrors cloud_sync_attachments pattern.
    let encryptor = match std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        pcs_keys_for_record(record, &zone_key)
    })) {
        Ok(Ok(enc)) => enc,
        Ok(Err(e)) => {
            info!(
                "StatusKit-CloudKit decode: pcs_keys_for_record returned error: {:?} — trying keychain fallback",
                e
            );
            match decrypt_with_keychain_fallback(record, cm).await {
                Some(enc) => enc,
                None => return Err(format!("PCS unwrap failed (both paths)")),
            }
        }
        Err(_) => {
            info!(
                "StatusKit-CloudKit decode: pcs_keys_for_record panicked — trying keychain fallback"
            );
            match decrypt_with_keychain_fallback(record, cm).await {
                Some(enc) => enc,
                None => return Err(format!("PCS unwrap panicked, keychain fallback failed")),
            }
        }
    };

    // Decrypt the encrypted-bytes payload field.
    let plaintext = match std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        encryptor.decrypt_data(payload_ct, "CD_invitationPayload")
    })) {
        Ok(b) => b,
        Err(_) => return Err("decrypt_data panicked on CD_invitationPayload".into()),
    };
    if plaintext.is_empty() {
        return Err("decrypted CD_invitationPayload is empty".into());
    }
    info!(
        "StatusKit-CloudKit decode: PCS-decrypted CD_invitationPayload ({} bytes)",
        plaintext.len()
    );

    // Parse the IDS-format plist.
    let parsed: InvitationPayloadMirror = plist::from_bytes(&plaintext)
        .map_err(|e| format!("plist::from_bytes(InvitationPayloadMirror) failed: {:?}", e))?;
    info!(
        "StatusKit-CloudKit decode: parsed payload bundle='{}' time_sent_s={} channel_b64_len={}",
        parsed.bundle,
        parsed.time_sent_s,
        parsed.channel.len()
    );

    // Decode the SharedMessage protobuf.
    let share_message_bytes = B64
        .decode(&parsed.keys)
        .map_err(|e| format!("base64 decode SharedMessage: {:?}", e))?;
    let share_message = SharedMessage::decode(Cursor::new(&share_message_bytes))
        .map_err(|e| format!("SharedMessage::decode failed: {:?}", e))?;

    if share_message.sig_key.len() != 32 {
        return Err(format!(
            "unexpected sig_key length {}",
            share_message.sig_key.len()
        ));
    }

    // The base64 channel from the payload is the canonical channel id.
    // CD_channel FK + channel_map is a sanity cross-check; log if they
    // disagree but trust the embedded value (it's what the keysharing
    // protocol uses on the wire).
    if let Some(mapped) = channel_map.get(&channel_fk) {
        if mapped != &parsed.channel {
            info!(
                "StatusKit-CloudKit decode: CD_channel FK→identifier mismatch (mapped='{}', payload='{}') — trusting payload",
                mapped, parsed.channel
            );
        }
    } else {
        info!(
            "StatusKit-CloudKit decode: CD_channel FK '{}' not in channel_map (have {} entries)",
            channel_fk,
            channel_map.len()
        );
    }

    // Reconstruct the StatusKitSharedDevice via plist round-trip.
    let device = build_shared_device(
        sender_handle.clone(),
        &share_message,
        &parsed.personal_config,
    )?;

    Ok(Some(DecodedPeer {
        channel_id: parsed.channel,
        from: sender_handle,
        device,
    }))
}

/// Try the same keychain-decrypt fallback that cloud_sync_attachments uses
/// when pcs_keys_for_record panics. Returns None on any failure.
async fn decrypt_with_keychain_fallback<P: omnisette::AnisetteProvider>(
    record: &cloudkit_proto::Record,
    cm: &rustpush::cloud_messages::CloudMessagesClient<P>,
) -> Option<rustpush::pcs::PCSEncryptor> {
    let protection = record.protection_info.as_ref()?;
    let record_protection = PCSShareProtection::from_protection_info(protection);
    let keychain_state = cm.keychain.state.read().await;
    let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        record_protection.decrypt_with_keychain(&keychain_state, &MESSAGES_SERVICE, false)
    }));
    match result {
        Ok(Ok((pcs_keys, _))) => {
            info!("StatusKit-CloudKit decode: keychain fallback PCS unwrap OK");
            let record_id = record.record_identifier.clone()?;
            Some(rustpush::pcs::PCSEncryptor {
                keys: pcs_keys,
                record_id,
            })
        }
        Ok(Err(e)) => {
            info!(
                "StatusKit-CloudKit decode: keychain fallback PCS unwrap returned error: {:?}",
                e
            );
            None
        }
        Err(_) => {
            info!("StatusKit-CloudKit decode: keychain fallback PCS unwrap panicked");
            None
        }
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
    let sig_der = compressed_pubkey_to_der(&share_message.sig_key)
        .map_err(|e| format!("compressed_pubkey_to_der: {:?}", e))?;

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

/// Take a 32-byte X9.62 raw EC public key (P-256), prepend the 0x03 compressed
/// prefix, decompress to an `EcPoint`, then output X9.62 DER. Mirrors
/// `CompactECKey::decompress(...).public_key_to_der()`.
fn compressed_pubkey_to_der(raw_32: &[u8]) -> Result<Vec<u8>, openssl::error::ErrorStack> {
    let mut compressed = [0u8; 33];
    compressed[0] = 0x03;
    compressed[1..].copy_from_slice(raw_32);
    let group = EcGroup::from_curve_name(Nid::X9_62_PRIME256V1)?;
    let mut ctx = BigNumContext::new()?;
    let point = EcPoint::from_bytes(&group, &compressed, &mut ctx)?;
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
