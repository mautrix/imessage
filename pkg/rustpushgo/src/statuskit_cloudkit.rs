//! StatusKit peer-key CloudKit pull (bridge-side).
//!
//! Goal: hydrate StatusKit peer keys from the user's iCloud private database
//! instead of waiting for an APNs reshare from the peer iPhone. APNs reshare
//! is too unreliable in bridge-only-cluster setups.
//!
//! ## Status: Phase 1 (discovery + diagnostic logging)
//!
//! The exact CloudKit zone name and record schema Apple uses for StatusKit
//! peer keys are NOT yet known. The rustpush developer confirmed the keys
//! are in CloudKit but did not share the schema. This module therefore:
//!
//!   1. Lists every zone in the user's private database via
//!      `FetchZoneChangesOperation::do_sync` (the same call CloudKit zone
//!      sync uses) and logs each at info level so the user can pull
//!      production logs and identify the StatusKit zone.
//!   2. Picks the first zone whose name contains a StatusKit-shaped keyword
//!      (`status`, `presence`, `sharedchannel`, `focus`) and pages records
//!      out of it via `FetchRecordChangesOperation`.
//!   3. Logs each record's full schema (record_type, field names, has_protection_info)
//!      at info level — also for the user to share back.
//!   4. Has a stub `decode_peer_record` that always returns `None` because
//!      we do not yet know how Apple's record fields map onto
//!      `StatusKitSharedDevice { from, signature, keys, personal_config }`.
//!
//! Phase 2 fills in `decode_peer_record` once we have a confirmed schema.
//! Until then, the pass is a high-fidelity diagnostic: every run produces
//! a complete log of zones + record schemas without mutating any state.
//!
//! ## Logging convention
//!
//! Everything load-bearing is at `log::info!`. `warn!` is reserved for
//! genuine errors. Grep for `StatusKit-CloudKit` in production logs to find
//! all output from this module.

use log::{info, warn};
use rustpush::{
    cloud_messages::MESSAGES_SERVICE,
    cloudkit::{
        pcs_keys_for_record, CloudKitSession, FetchRecordChangesOperation,
        FetchZoneChangesOperation, NO_ASSETS,
    },
    cloudkit_proto,
    pcs::PCSShareProtection,
    statuskit::StatusKitSharedDevice,
};

use crate::{persist_plist_state, subsystem_state_path, Client, WrappedError};

/// Result of one CloudKit-pull pass for StatusKit peer keys.
///
/// `resolved_zone` is the actual zone name CloudKit served. The Go side
/// caches it so subsequent passes skip discovery. If the zone fetch failed
/// in a way that suggests the cached name is stale (e.g. ZoneNotFound), the
/// Rust side returns `resolved_zone = None` so the Go side clears its cache
/// and re-discovers next pass.
#[derive(Debug, Clone, uniffi::Record)]
pub struct CloudSyncStatusKitPage {
    pub resolved_zone: Option<String>,
    pub next_token: Option<Vec<u8>>,
    pub fetched: u32,
    pub inserted: u32,
    pub already_known: u32,
    pub decode_failed: u32,
    pub records_seen: u32,
    pub injected_handles: Vec<String>,
    /// Free-form summary surfaced when there was nothing to do or something
    /// abnormal happened (e.g. "no candidate zone found", "fetch failed").
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
    /// Discover (if needed) and pull StatusKit peer keys from CloudKit.
    ///
    /// `cached_zone`: if Some, skip discovery and fetch directly from this
    /// zone. If None, run discovery and pick a candidate.
    ///
    /// `since_token`: continuation token from a previous pass (per-zone),
    /// passed through to `FetchRecordChangesOperation`.
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

        let zone_name = match cached_zone {
            Some(z) => {
                info!("StatusKit-CloudKit pass: using cached zone='{}'", z);
                z
            }
            None => {
                info!(
                    "StatusKit-CloudKit pass: no cached zone — running discovery against private DB"
                );
                match discover_statuskit_zone(self).await {
                    Ok(Some(z)) => {
                        info!(
                            "StatusKit-CloudKit pass: discovery picked zone='{}'",
                            z
                        );
                        z
                    }
                    Ok(None) => {
                        info!(
                            "StatusKit-CloudKit pass: discovery found NO candidate zone — returning empty page (see preceding zone list in logs)"
                        );
                        return Ok(CloudSyncStatusKitPage::empty(
                            None,
                            Some("no candidate zone found".into()),
                        ));
                    }
                    Err(e) => {
                        warn!("StatusKit-CloudKit pass: discovery errored: {}", e);
                        return Ok(CloudSyncStatusKitPage::empty(
                            None,
                            Some(format!("discovery error: {}", e)),
                        ));
                    }
                }
            }
        };

        let (records, next_token, status) =
            match fetch_zone_records(self, &zone_name, since_token).await {
                Ok(v) => v,
                Err(e) => {
                    warn!(
                        "StatusKit-CloudKit pass: fetch zone='{}' failed: {} — clearing cached zone so next pass re-discovers",
                        zone_name, e
                    );
                    // Surface to caller via resolved_zone=None so Go clears
                    // its cache.
                    return Ok(CloudSyncStatusKitPage {
                        resolved_zone: None,
                        next_token: None,
                        fetched: 0,
                        inserted: 0,
                        already_known: 0,
                        decode_failed: 0,
                        records_seen: 0,
                        injected_handles: Vec::new(),
                        discovery_summary: Some(format!(
                            "fetch failed (clearing cached zone): {}",
                            e
                        )),
                    });
                }
            };

        info!(
            "StatusKit-CloudKit pass: zone='{}' response_status={} fetched_records={} has_next_token={}",
            zone_name,
            status,
            records.len(),
            next_token.is_some()
        );

        let mut decoded: Vec<DecodedPeer> = Vec::new();
        let mut decode_failed: u32 = 0;
        for (idx, rec) in records.iter().enumerate() {
            log_record_schema(idx, &zone_name, rec);
            match decode_peer_record(self, rec).await {
                Ok(Some(p)) => {
                    info!(
                        "StatusKit-CloudKit decode[{}]: zone='{}' OK from='{}' channel_b64_len={}",
                        idx,
                        zone_name,
                        p.from,
                        p.channel_id.len()
                    );
                    decoded.push(p);
                }
                Ok(None) => {
                    info!(
                        "StatusKit-CloudKit decode[{}]: zone='{}' SKIPPED (record didn't carry decodable StatusKit shape — see schema dump above)",
                        idx, zone_name
                    );
                }
                Err(e) => {
                    decode_failed += 1;
                    info!(
                        "StatusKit-CloudKit decode[{}]: zone='{}' FAILED: {}",
                        idx, zone_name, e
                    );
                }
            }
        }

        let inject_stats = match inject_into_state(self, decoded).await {
            Ok(s) => s,
            Err(e) => {
                warn!(
                    "StatusKit-CloudKit pass: state injection failed: {:?}",
                    e
                );
                return Err(e);
            }
        };

        info!(
            "StatusKit-CloudKit pass: DONE zone='{}' fetched={} inserted={} already_known={} decode_failed={} injected_handles={:?}",
            zone_name,
            records.len(),
            inject_stats.inserted,
            inject_stats.already_known,
            decode_failed,
            inject_stats.injected_handles
        );

        Ok(CloudSyncStatusKitPage {
            resolved_zone: Some(zone_name),
            next_token,
            fetched: records.len() as u32,
            inserted: inject_stats.inserted,
            already_known: inject_stats.already_known,
            decode_failed,
            records_seen: records.len() as u32,
            injected_handles: inject_stats.injected_handles,
            discovery_summary: None,
        })
    }
}

/// Iterate every zone in the user's private database, log each, and pick
/// one whose name suggests StatusKit storage. Returns `None` if nothing
/// matches the keyword heuristic — the user can then read the zone list
/// out of the logs and supply a hint.
async fn discover_statuskit_zone(client: &Client) -> Result<Option<String>, String> {
    let cm = client
        .get_or_init_cloud_messages_client()
        .await
        .map_err(|e| format!("cloud_messages init failed: {:?}", e))?;
    let container = cm
        .get_container()
        .await
        .map_err(|e| format!("get_container failed: {:?}", e))?;

    info!(
        "StatusKit-CloudKit discovery: requesting zone-changes from CloudKit private DB"
    );
    let (zones, _final_token) = FetchZoneChangesOperation::do_sync(&*container, None)
        .await
        .map_err(|e| format!("FetchZoneChangesOperation failed: {:?}", e))?;

    info!(
        "StatusKit-CloudKit discovery: CloudKit returned {} zone(s) in private DB",
        zones.len()
    );

    // Zones we already know are for non-StatusKit data — exclude them from
    // candidates but still log them.
    let known_messages_zones = [
        "chatManateeZone",
        "messageManateeZone",
        "attachmentManateeZone",
        "recoverableMessageDeleteZone",
        "chatBotRecoverableMessageDeleteZone",
        "_defaultZone",
    ];

    let mut candidates: Vec<String> = Vec::new();
    let mut all_names: Vec<String> = Vec::new();

    for z in &zones {
        let zname = match z
            .identifier
            .as_ref()
            .and_then(|i| i.value.as_ref())
            .and_then(|v| v.name.as_deref())
        {
            Some(n) => n.to_string(),
            None => {
                info!(
                    "StatusKit-CloudKit discovery: zone with no zone_name (change_type={:?})",
                    z.change_type
                );
                continue;
            }
        };
        all_names.push(zname.clone());
        let nm_lc = zname.to_lowercase();
        let is_known = known_messages_zones.contains(&zname.as_str());
        let is_status_kw = nm_lc.contains("status")
            || nm_lc.contains("presence")
            || nm_lc.contains("sharedchannel")
            || nm_lc.contains("focus");

        info!(
            "StatusKit-CloudKit discovery: zone='{}' known_messages_zone={} status_keyword_match={} change_type={:?} is_anonymous={:?}",
            zname,
            is_known,
            is_status_kw,
            z.change_type,
            z.is_annonymous
        );

        if is_status_kw && !is_known {
            candidates.push(zname);
        }
    }

    info!(
        "StatusKit-CloudKit discovery: full zone list ({} total): [{}]",
        all_names.len(),
        all_names.join(", ")
    );

    if candidates.is_empty() {
        info!(
            "StatusKit-CloudKit discovery: NO keyword-matching candidate zone found; user inspection of the full zone list above may identify the right one"
        );
        return Ok(None);
    }

    info!(
        "StatusKit-CloudKit discovery: {} keyword-matching candidate(s): {:?}; picking first",
        candidates.len(),
        candidates
    );
    Ok(Some(candidates.into_iter().next().unwrap()))
}

/// Single-page fetch of `FetchRecordChangesOperation` against the named zone.
/// Returns the records (as RecordChange entries), the next continuation
/// token, and the response status.
async fn fetch_zone_records(
    client: &Client,
    zone_name: &str,
    since_token: Option<Vec<u8>>,
) -> Result<
    (
        Vec<cloudkit_proto::retrieve_changes_response::RecordChange>,
        Option<Vec<u8>>,
        i32,
    ),
    String,
> {
    let cm = client
        .get_or_init_cloud_messages_client()
        .await
        .map_err(|e| format!("cloud_messages init failed: {:?}", e))?;
    let container = cm
        .get_container()
        .await
        .map_err(|e| format!("get_container failed: {:?}", e))?;
    let zone_id = container.private_zone(zone_name.to_string());

    info!(
        "StatusKit-CloudKit fetch: zone='{}' has_continuation_token={}",
        zone_name,
        since_token.is_some()
    );

    let (_assets, response) = container
        .perform(
            &CloudKitSession::new(),
            FetchRecordChangesOperation(cloudkit_proto::RetrieveChangesRequest {
                sync_continuation_token: since_token,
                zone_identifier: Some(zone_id),
                requested_changes_types: Some(3),
                assets_to_download: Some(NO_ASSETS.clone()),
                newest_first: Some(true),
                ..Default::default()
            }),
        )
        .await
        .map_err(|e| format!("perform(FetchRecordChangesOperation) failed: {:?}", e))?;

    let status = response.status.unwrap_or(0);
    let next_token = response.sync_continuation_token.clone();
    let records = response.change.into_iter().collect();
    Ok((records, next_token, status))
}

/// Log a record's full schema (record_type, field names, protection_info presence)
/// at info level so the user can identify the StatusKit record shape.
fn log_record_schema(
    idx: usize,
    zone: &str,
    rec: &cloudkit_proto::retrieve_changes_response::RecordChange,
) {
    let id = rec
        .identifier
        .as_ref()
        .and_then(|i| i.value.as_ref())
        .and_then(|v| v.name.as_deref())
        .unwrap_or("<no-name>");
    let rec_type = rec
        .record
        .as_ref()
        .and_then(|r| r.r#type.as_ref())
        .and_then(|t| t.name.as_deref())
        .unwrap_or("<no-type>");
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
    let field_names: Vec<&str> = rec
        .record
        .as_ref()
        .map(|r| {
            r.record_field
                .iter()
                .filter_map(|f| f.identifier.as_ref().and_then(|i| i.name.as_deref()))
                .collect()
        })
        .unwrap_or_default();
    let field_types: Vec<i32> = rec
        .record
        .as_ref()
        .map(|r| {
            r.record_field
                .iter()
                .filter_map(|f| {
                    f.value
                        .as_ref()
                        .and_then(|v| v.r#type)
                })
                .collect()
        })
        .unwrap_or_default();
    let has_tombstone = rec.record.is_none();
    info!(
        "StatusKit-CloudKit schema[{}]: zone='{}' record_id='{}' record_type='{}' tombstone={} has_protection_info={} field_count={} field_names={:?} field_value_types={:?}",
        idx, zone, id, rec_type, has_tombstone, has_protection, field_count, field_names, field_types
    );
}

/// Captured during decode so we can log + persist without needing access to
/// `StatusKitSharedDevice`'s private fields.
struct DecodedPeer {
    channel_id: String,
    from: String,
    device: StatusKitSharedDevice,
}

/// Decode one CloudKit record into a StatusKit peer entry.
///
/// Phase-1 stub: returns `Ok(None)` for every record. This is intentional.
/// The schema dumps logged by `log_record_schema` produce the information
/// needed to write a real decoder; until that schema is confirmed, this
/// function refuses to fabricate `StatusKitSharedDevice` instances. PCS
/// unwrap is wired up but the field-mapping is left for Phase 2.
async fn decode_peer_record(
    client: &Client,
    rec: &cloudkit_proto::retrieve_changes_response::RecordChange,
) -> Result<Option<DecodedPeer>, String> {
    let _record = match &rec.record {
        Some(r) => r,
        None => return Ok(None), // tombstone — not an error
    };

    // PCS unwrap scaffolding for Phase 2. We try to obtain a per-record PCS
    // key here so that, when the real decoder lands, it has the unwrapped
    // material in hand and the failure modes are already logged.
    if let Some(record) = &rec.record {
        if let Some(_protection) = &record.protection_info {
            let cm = match client.get_or_init_cloud_messages_client().await {
                Ok(c) => c,
                Err(e) => {
                    info!(
                        "StatusKit-CloudKit decode: cloud_messages unavailable for PCS unwrap: {:?}",
                        e
                    );
                    return Ok(None);
                }
            };
            let container = match cm.get_container().await {
                Ok(c) => c,
                Err(e) => {
                    info!(
                        "StatusKit-CloudKit decode: get_container failed for PCS unwrap: {:?}",
                        e
                    );
                    return Ok(None);
                }
            };
            // Try the messages-service zone-key first since it's our existing
            // PCS context. If StatusKit needs a different service constant,
                // we'll see the failure here and surface it for diagnosis.
            let zone_id = match rec
                .identifier
                .as_ref()
                .and_then(|i| i.zone_identifier.as_ref())
            {
                Some(z) => z.clone(),
                None => {
                    info!(
                        "StatusKit-CloudKit decode: record has no zone_identifier; skipping PCS unwrap probe"
                    );
                    return Ok(None);
                }
            };
            let zone_key_attempt = container
                .get_zone_encryption_config(&zone_id, &cm.keychain, &MESSAGES_SERVICE)
                .await;
            match zone_key_attempt {
                Ok(zone_key) => {
                    let pcs_attempt = std::panic::catch_unwind(std::panic::AssertUnwindSafe(
                        || pcs_keys_for_record(record, &zone_key),
                    ));
                    match pcs_attempt {
                        Ok(Ok(_pcs_key)) => {
                            info!(
                                "StatusKit-CloudKit decode: PCS unwrap OK via MESSAGES_SERVICE; record decode still pending Phase-2 schema"
                            );
                        }
                        Ok(Err(e)) => {
                            info!(
                                "StatusKit-CloudKit decode: PCS unwrap returned error via MESSAGES_SERVICE: {:?}",
                                e
                            );
                            // Fallback path mirrors cloud_sync_attachments.
                            if let Some(protection) = &record.protection_info {
                                let record_protection =
                                    PCSShareProtection::from_protection_info(protection);
                                let keychain_state =
                                    cm.keychain.state.read().await;
                                let fallback = std::panic::catch_unwind(
                                    std::panic::AssertUnwindSafe(|| {
                                        record_protection.decrypt_with_keychain(
                                            &keychain_state,
                                            &MESSAGES_SERVICE,
                                            false,
                                        )
                                    }),
                                );
                                match fallback {
                                    Ok(Ok(_)) => info!(
                                        "StatusKit-CloudKit decode: keychain fallback PCS unwrap OK"
                                    ),
                                    Ok(Err(e2)) => info!(
                                        "StatusKit-CloudKit decode: keychain fallback PCS unwrap also failed: {:?}",
                                        e2
                                    ),
                                    Err(_) => info!(
                                        "StatusKit-CloudKit decode: keychain fallback panicked"
                                    ),
                                }
                            }
                        }
                        Err(_) => {
                            info!(
                                "StatusKit-CloudKit decode: pcs_keys_for_record panicked — likely a different PCS service class is needed"
                            );
                        }
                    }
                }
                Err(e) => {
                    info!(
                        "StatusKit-CloudKit decode: get_zone_encryption_config(MESSAGES_SERVICE) failed; StatusKit may use a different PCS service: {:?}",
                        e
                    );
                }
            }
        }
    }

    // Phase 2 lands here.
    Ok(None)
}

struct InjectStats {
    inserted: u32,
    already_known: u32,
    injected_handles: Vec<String>,
}

/// Acquire a write lock on the in-memory StatusKit state, insert decoded
/// peers, and persist via the same plist path that the upstream
/// `update_state` callback uses. Skips peers whose channel id is already
/// known so reshare-derived state always wins on conflict.
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
