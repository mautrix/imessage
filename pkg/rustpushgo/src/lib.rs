pub mod util;
#[cfg(target_os = "macos")]
pub mod local_config;
#[cfg(not(target_os = "macos"))]
pub mod anisette;
mod statuskitgo;
#[cfg(test)]
mod test_hwinfo;

use std::{collections::HashMap, io::Cursor, path::PathBuf, str::FromStr, sync::Arc, time::Duration, sync::atomic::{AtomicU64, Ordering}};

use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine as _};
use icloud_auth::AppleAccount;
use keystore::{init_keystore, keystore, software::{NoEncryptor, SoftwareKeystore, SoftwareKeystoreState}};
use log::{debug, error, info, warn};
use rustpush::{
    authenticate_apple, login_apple_delegates, register, APSConnectionResource,
    APSState, Attachment, AttachmentType, ConversationData, DeleteTarget, EditMessage,
    IDSNGMIdentity, IDSUser, IMClient, LoginDelegate, MADRID_SERVICE, MMCSFile, Message,
    MessageInst, MessagePart, MessageParts, MessageType, MoveToRecycleBinMessage, NormalMessage, PermanentDeleteMessage,
    OperatedChat, OSConfig, ReactMessage, ReactMessageType, Reaction, RenameMessage,
    ChangeParticipantMessage, IconChangeMessage, UnsendMessage, TypingApp,
    IndexedMessagePart, LinkMeta, LPLinkMetadata, NSURL,
    TextFlags, TextFormat, TextEffect,
    ShareProfileMessage, SharedPoster, PartExtension, UpdateExtensionMessage, UpdateProfileMessage,
    UpdateProfileSharingMessage, SetTranscriptBackgroundMessage,
    TokenProvider,
    ScheduleMode,
    cloudkit::{ZoneDeleteOperation, CloudKitSession},
    ResourceState,
};
use rustpush::cloudkit_proto::request_operation::header::IsolationLevel;
use rustpush::facetime::{FACETIME_SERVICE, VIDEO_SERVICE};
use rustpush::findmy::MULTIPLEX_SERVICE;

use std::sync::RwLock;

// ============================================================================
// Anisette provider selection
// ============================================================================
//
// `BridgeDefaultAnisetteProvider` is the concrete `AnisetteProvider` type used
// by every rustpush client that's parameterized over one (AppleAccount,
// KeychainClient, CloudKitClient, TokenProvider, etc.). On macOS this is
// upstream's `AOSKitAnisetteProvider` (native, untouched). On Linux this is
// our `anisette::BridgeAnisetteProvider`, which wraps upstream's
// `RemoteAnisetteProviderV3` with retry logic, a timeout (upstream's
// provision() loop spins forever on WS close), and error handling for
// upstream's missing `EndProvisioningError` serde variant.
#[cfg(target_os = "macos")]
pub type BridgeDefaultAnisetteProvider = omnisette::DefaultAnisetteProvider;
#[cfg(not(target_os = "macos"))]
pub type BridgeDefaultAnisetteProvider = anisette::BridgeAnisetteProvider;

#[cfg(target_os = "macos")]
fn bridge_default_provider(
    info: omnisette::LoginClientInfo,
    path: PathBuf,
) -> omnisette::ArcAnisetteClient<BridgeDefaultAnisetteProvider> {
    omnisette::default_provider(info, path)
}
#[cfg(not(target_os = "macos"))]
fn bridge_default_provider(
    info: omnisette::LoginClientInfo,
    path: PathBuf,
) -> omnisette::ArcAnisetteClient<BridgeDefaultAnisetteProvider> {
    std::sync::Arc::new(tokio::sync::Mutex::new(omnisette::AnisetteClient::new(
        anisette::BridgeAnisetteProvider::new(info, path),
    )))
}
use tokio::sync::broadcast;
use util::{plist_from_string, plist_to_string};

// Local helpers to replace rustpush's private `util::{base64_decode, encode_hex, ...}`.
// Part of the zero-patch refactor: `rustpush::util` is private in upstream, so we
// reimplement the same semantics (infallible on invalid base64/hex input) using the
// `base64` and `hex` crates directly.
#[inline]
fn base64_decode(s: &str) -> Vec<u8> {
    BASE64_STANDARD.decode(s).unwrap_or_default()
}
#[inline]
fn base64_encode(data: &[u8]) -> String {
    BASE64_STANDARD.encode(data)
}
#[inline]
fn encode_hex(bytes: &[u8]) -> String {
    hex::encode(bytes)
}
#[inline]
fn decode_hex(s: &str) -> Result<Vec<u8>, hex::FromHexError> {
    hex::decode(s)
}

// ============================================================================
// Ford key cache (wrapper-level reimplementation of the 94f7b8e fix)
// ============================================================================
//
// CloudKit videos are Ford-encrypted: each record carries a 32-byte Ford key
// in `lqa.protection_info.protection_info`, and MMCS deduplicates identical
// content at the storage layer. When the same video is uploaded twice, MMCS
// returns ONE encrypted blob encrypted with the original uploader's key — so
// the second record's own Ford key cannot SIV-decrypt it, and upstream
// rustpush's `get_mmcs` panics on the `.unwrap()` of the SIV result.
//
// This cache holds every Ford key the bridge has ever seen (populated during
// CloudKit attachment sync). On a SIV panic during download, the wrapper
// catches the panic and retries `container.get_assets(...)` with each cached
// key in turn by mutating `Asset.protection_info.protection_info` before the
// call. This matches the semantics of the original in-rustpush fix without
// touching upstream source.

fn ford_key_cache() -> &'static std::sync::Mutex<HashMap<Vec<u8>, Vec<u8>>> {
    static CACHE: std::sync::OnceLock<std::sync::Mutex<HashMap<Vec<u8>, Vec<u8>>>> =
        std::sync::OnceLock::new();
    CACHE.get_or_init(|| std::sync::Mutex::new(HashMap::new()))
}

/// Register a Ford key in the process-wide cache so that a later deduplicated
/// download can recover from an SIV decrypt failure. Key is indexed by
/// `fordChecksum = 0x01 || SHA1(key)`. Idempotent; no-op for empty input.
#[uniffi::export]
pub fn register_ford_key(key: Vec<u8>) {
    if key.is_empty() {
        return;
    }
    use sha1::{Digest, Sha1};
    let mut hasher = Sha1::new();
    hasher.update(&key);
    let sha = hasher.finalize();
    let mut checksum = Vec::with_capacity(1 + sha.len());
    checksum.push(0x01);
    checksum.extend_from_slice(&sha);
    let mut cache = match ford_key_cache().lock() {
        Ok(g) => g,
        Err(poisoned) => poisoned.into_inner(),
    };
    if cache.insert(checksum, key).is_some() {
        return;
    }
    let size = cache.len();
    drop(cache);
    debug!("register_ford_key: cached Ford key for dedup fallback (size={})", size);
}

/// Number of Ford keys currently cached. Diagnostic only.
#[uniffi::export]
pub fn ford_key_cache_size() -> u64 {
    let cache = match ford_key_cache().lock() {
        Ok(g) => g,
        Err(poisoned) => poisoned.into_inner(),
    };
    cache.len() as u64
}

/// Snapshot of all cached Ford keys, used by the download recovery path.
fn ford_key_cache_values() -> Vec<Vec<u8>> {
    let cache = match ford_key_cache().lock() {
        Ok(g) => g,
        Err(poisoned) => poisoned.into_inner(),
    };
    cache.values().cloned().collect()
}

/// Check whether a given asset is Ford-encrypted by parsing the CloudKit
/// `AssetGetResponse` body (which is a `mmcsp::AuthorizeGetResponse` proto)
/// and looking up the `wanted_chunks` entry for the asset's file_checksum.
/// If that entry has a `ford_reference`, the asset uses Ford encryption
/// (V2 / videos). If not, it's V1 per-chunk encryption (images, etc.) and
/// the Ford dedup recovery path shouldn't run for it.
///
/// Returns `true` ONLY when we've proven the asset is Ford-encrypted.
/// Returns `false` if the body can't be parsed, the checksum isn't found,
/// or `ford_reference` is None — anything ambiguous is treated as non-Ford
/// to avoid burning retries on records recovery can't help.
fn is_ford_encrypted_asset(
    asset_responses: &[rustpush::cloudkit_proto::AssetGetResponse],
    asset: &rustpush::cloudkit_proto::Asset,
) -> bool {
    use prost::Message;

    let Some(bundled_id) = asset.bundled_request_id.as_ref() else {
        return false;
    };
    let Some(signature) = asset.signature.as_ref() else {
        return false;
    };
    let Some(response) = asset_responses
        .iter()
        .find(|r| r.asset_id.as_ref() == Some(bundled_id))
    else {
        return false;
    };
    let Some(body) = response.body.as_ref() else {
        return false;
    };

    let auth_response = match rustpush::mmcsp::AuthorizeGetResponse::decode(&body[..]) {
        Ok(r) => r,
        Err(_) => return false,
    };
    let Some(f1) = auth_response.f1.as_ref() else {
        return false;
    };
    // Find our file's `wanted_chunks` entry by matching file_checksum
    // (= the asset signature). If it has a ford_reference, the server is
    // telling us this asset's chunks are Ford-wrapped and the Ford key
    // lives in the asset's protection_info.
    f1.references
        .iter()
        .find(|w| &w.file_checksum == signature)
        .and_then(|w| w.ford_reference.as_ref())
        .is_some()
}

// ============================================================================
// Manual Ford download (V1 + V2 support — zero-patch workaround for upstream)
// ============================================================================
//
// Upstream rustpush's `get_mmcs` supports V1 Ford (`FordItem` with a flat
// `chunks: Vec<FordChunkItem>`) only. When Apple's CloudKit serves a
// V2 Ford blob (`FordItemV2` with grouped chunks) — which master's fork
// supports via an extended proto — upstream panics unconditionally at
// `chunks.item.expect("Ford chunks missing?")` (mmcs.rs:1117). The panic
// is post-SIV, so no amount of key brute-forcing in the wrapper can
// recover: upstream crashes before it ever tries to extract per-chunk
// keys.
//
// This module reimplements the Ford download path entirely at the
// wrapper layer using upstream's public primitives:
//   - `rustpush::mmcsp::AuthorizeGetResponse` (the proto bytes we get
//     back from the CloudKit AssetGetResponse body)
//   - `rustpush::mmcs::transfer_mmcs_container` (the public HTTP fetch)
//   - local prost types for `LocalFordChunk` that understand BOTH V1
//     and V2 layouts (fields 1 and 2 of the wire format)
//   - manual SIV decrypt loop (HKDF + CmacSiv) over every cached Ford
//     key, because the dedup case means the record's OWN key isn't
//     necessarily the right one
//   - manual V2 chunk decrypt (AES-256-CTR + HKDF + HMAC verify)
//   - manual V1 chunk decrypt (AES-128-CFB)
//
// The control flow is: upstream's `container.get_assets` is still the
// happy path (fast, no panic on V1 correct-key records). Only when that
// panics or errors do we fall through to `manual_ford_download_asset`.
// That function handles dedup, V2 Ford, V1 Ford, AND plain V1 chunks
// all via the same code path, so recovery is a superset of upstream's
// capabilities.

mod manual_ford {
    use super::*;
    use aes_siv::{siv::CmacSiv, KeyInit};
    use aes::Aes256;
    use hkdf::Hkdf;
    use once_cell::sync::Lazy;
    use openssl::{
        hash::MessageDigest,
        pkey::PKey,
        sign::Signer,
        symm::{decrypt as openssl_decrypt, Cipher},
    };
    use prost::Message;
    use sha2::{Digest as Sha2Digest, Sha256};

    /// Shared reqwest client for HTTP container fetches. We can't use
    /// upstream's private `REQWEST` static, so we maintain our own.
    pub(super) static HTTP_CLIENT: Lazy<reqwest::Client> = Lazy::new(|| {
        reqwest::Client::builder()
            .gzip(true)
            .timeout(Duration::from_secs(120))
            .build()
            .expect("reqwest client build")
    });

    /// Local FordChunk message. Upstream's `mmcsp::FordChunk` only has
    /// field 1 (`item: FordItem`) because upstream's proto doesn't know
    /// about V2. We decode the same bytes through this local type to get
    /// access to the V2 `item_v2` field (tag 2), which carries chunks
    /// grouped by size with multiple keys per group.
    ///
    /// Since prost decoders skip unknown fields by default, decoding the
    /// same wire bytes through either type works regardless of which
    /// version the server actually sent. Presence of `item`/`item_v2`
    /// tells us which layout we got.
    #[derive(Clone, PartialEq, Message)]
    pub(super) struct LocalFordChunk {
        #[prost(message, optional, tag = "1")]
        pub item: Option<LocalFordItem>,
        #[prost(message, optional, tag = "2")]
        pub item_v2: Option<LocalFordItemV2>,
    }

    #[derive(Clone, PartialEq, Message)]
    pub(super) struct LocalFordItem {
        #[prost(message, repeated, tag = "1")]
        pub chunks: Vec<LocalFordChunkItem>,
        #[prost(bytes = "vec", tag = "2")]
        pub checksum: Vec<u8>,
    }

    #[derive(Clone, PartialEq, Message)]
    pub(super) struct LocalFordChunkItem {
        #[prost(bytes = "vec", tag = "1")]
        pub key: Vec<u8>,
        #[prost(bytes = "vec", tag = "2")]
        pub chunk_len: Vec<u8>,
    }

    #[derive(Clone, PartialEq, Message)]
    pub(super) struct LocalFordItemV2 {
        #[prost(bytes = "vec", tag = "1")]
        pub checksum: Vec<u8>,
        #[prost(message, repeated, tag = "2")]
        pub chunks: Vec<LocalFordChunkGroup>,
    }

    #[derive(Clone, PartialEq, Message)]
    pub(super) struct LocalFordChunkGroup {
        #[prost(bytes = "vec", tag = "1")]
        pub chunk_len: Vec<u8>,
        #[prost(message, repeated, tag = "2")]
        pub keys: Vec<LocalFordKeyWrapper>,
    }

    #[derive(Clone, PartialEq, Message)]
    pub(super) struct LocalFordKeyWrapper {
        #[prost(bytes = "vec", tag = "1")]
        pub key: Vec<u8>,
    }

    /// Flatten a LocalFordChunk (V1 or V2) into a sequence of
    /// `(ford_key, chunk_len_bytes)` pairs, one per data chunk, in the
    /// same order as the file's `wanted_chunks.chunk_references`.
    pub(super) fn flatten_ford_entries(chunk: LocalFordChunk) -> Option<Vec<(Vec<u8>, Vec<u8>)>> {
        if let Some(item) = chunk.item {
            Some(item.chunks.into_iter().map(|c| (c.key, c.chunk_len)).collect())
        } else if let Some(item_v2) = chunk.item_v2 {
            let mut out = Vec::new();
            for group in item_v2.chunks {
                let chunk_len = group.chunk_len.clone();
                for kw in group.keys {
                    out.push((kw.key, chunk_len.clone()));
                }
            }
            Some(out)
        } else {
            None
        }
    }

    /// Attempt a single SIV decrypt with the given key over the given
    /// Ford blob. Returns None on failure (wrong key). Matches master's
    /// `try_ford_siv` exactly: HKDF("PCSMMCS2", key) → expand 64 bytes →
    /// CmacSiv::decrypt with headers [iv, version_byte].
    pub(super) fn try_ford_siv(key: &[u8], ford_blob: &[u8]) -> Option<Vec<u8>> {
        if ford_blob.len() < 17 {
            return None;
        }
        let hk = Hkdf::<Sha256>::new(Some(b"PCSMMCS2"), key);
        let mut result = [0u8; 64];
        if hk.expand(&[], &mut result).is_err() {
            return None;
        }
        let cipher = match CmacSiv::<Aes256>::new_from_slice(&result) {
            Ok(c) => c,
            Err(_) => return None,
        };
        // Wrap in catch_unwind — CmacSiv's decrypt has been observed to
        // panic on malformed blobs in some aes-siv versions. Defensive.
        let blob = ford_blob.to_vec();
        let res = std::panic::catch_unwind(std::panic::AssertUnwindSafe(move || {
            let mut cipher = cipher;
            cipher
                .decrypt::<&[&[u8]], &&[u8]>(&[&blob[1..17], &blob[..1]], &blob[17..])
                .ok()
        }));
        match res {
            Ok(Some(plaintext)) => Some(plaintext),
            _ => None,
        }
    }

    /// Try every cached Ford key against the blob. The correct key for a
    /// dedup'd upload is often from a DIFFERENT record than the one we're
    /// currently downloading, so "trying the record's own key" is never
    /// enough. Recency-first ordering makes the happy case (dedup with
    /// recently-seen source) fast.
    pub(super) fn ford_siv_retry(
        record_own_key: &[u8],
        ford_checksum: &[u8],
        ford_blob: &[u8],
        record_name: &str,
    ) -> Option<Vec<u8>> {
        // 1. The record's own declared key — usually the right one for
        //    non-dedup'd uploads.
        if let Some(d) = try_ford_siv(record_own_key, ford_blob) {
            return Some(d);
        }

        // 2. fordChecksum direct lookup — the server gave us a chunk
        //    reference checksum that identifies the original uploader's
        //    key. If we've seen that key in a prior batch, we have an
        //    exact-match hit.
        if !ford_checksum.is_empty() {
            let hit = {
                let cache = ford_key_cache().lock().unwrap_or_else(|p| p.into_inner());
                cache.get(ford_checksum).cloned()
            };
            if let Some(alt) = hit {
                if let Some(d) = try_ford_siv(&alt, ford_blob) {
                    info!(
                        "manual_ford_download {}: SIV succeeded via fordChecksum cache hit",
                        record_name
                    );
                    return Some(d);
                }
            }
        }

        // 3. Brute-force: try every cached key. The cross-batch dedup
        //    case requires this — the right key can be from any record
        //    the account has ever seen.
        let all_keys = ford_key_cache_values();
        let total = all_keys.len();
        for (idx, alt) in all_keys.iter().enumerate() {
            if let Some(d) = try_ford_siv(alt, ford_blob) {
                info!(
                    "manual_ford_download {}: SIV succeeded via brute-force (attempt {}/{})",
                    record_name,
                    idx + 1,
                    total
                );
                return Some(d);
            }
        }

        None
    }

    /// V2 chunk decrypt — ported from upstream's private `ChunkDesc::decrypt`
    /// at mmcs.rs:696. HKDF(key[1..]) → expand 0x60 bytes → split into
    /// (sig_hmac [0..32], auth_hmac [32..64], aes_key [64..96]). IV is
    /// HMAC(auth_hmac, chunk_id_padded_40)[..16]. Decrypts AES-256-CTR,
    /// truncates to declared length, verifies sig HMAC.
    ///
    /// Returns `Err` on HMAC mismatch (wrong key or corrupt data) — the
    /// upstream version has `assert_eq!` there, which panics; we return
    /// a typed error instead so the caller can fall through.
    pub(super) fn decrypt_v2_chunk(
        key33: &[u8],
        chunk_len_bytes: &[u8],
        chunk_id21: &[u8; 21],
        data: &[u8],
    ) -> Result<Vec<u8>, String> {
        if key33.len() != 33 {
            return Err(format!("V2 key wrong length: {}", key33.len()));
        }
        if chunk_len_bytes.len() != 4 {
            return Err(format!("V2 chunk_len wrong length: {}", chunk_len_bytes.len()));
        }

        let hk = Hkdf::<Sha256>::new(None, &key33[1..]);
        let mut expanded = [0u8; 0x60];
        hk.expand(b"signature-key", &mut expanded)
            .map_err(|e| format!("V2 HKDF expand: {e}"))?;

        let auth_hmac_key = &expanded[0x20..0x40];
        let aes_key = &expanded[0x40..0x60];
        let sig_hmac_key = &expanded[0x00..0x20];

        // IV construction: chunk_id[1..] (20 bytes) padded to 40 bytes,
        // with data length (little-endian u32) at offset [32..36].
        let mut id_padded = [0u8; 40];
        id_padded[..20].copy_from_slice(&chunk_id21[1..]);
        id_padded[32..36].copy_from_slice(&(data.len() as u32).to_le_bytes());

        let auth_pkey = PKey::hmac(auth_hmac_key)
            .map_err(|e| format!("V2 auth PKey: {e}"))?;
        let mut signer = Signer::new(MessageDigest::sha256(), &auth_pkey)
            .map_err(|e| format!("V2 auth Signer: {e}"))?;
        let iv_material = signer
            .sign_oneshot_to_vec(&id_padded)
            .map_err(|e| format!("V2 IV sign: {e}"))?;
        let iv = &iv_material[..16];

        let mut result = openssl_decrypt(Cipher::aes_256_ctr(), aes_key, Some(iv), data)
            .map_err(|e| format!("V2 AES-CTR decrypt: {e}"))?;

        let length = u32::from_le_bytes(
            chunk_len_bytes
                .try_into()
                .map_err(|_| "chunk_len bytes->[u8;4]")?,
        ) as usize;
        result.resize(length, 0);

        // HMAC verify — if the decrypted chunk doesn't authenticate, the
        // wrong key was used. This is the "is this chunk really mine"
        // check that lets us fall through to try another key.
        let plaintext_hash = {
            let mut h = Sha256::new();
            h.update(&result);
            h.finalize()
        };
        let sig_pkey = PKey::hmac(sig_hmac_key)
            .map_err(|e| format!("V2 sig PKey: {e}"))?;
        let mut verifier = Signer::new(MessageDigest::sha256(), &sig_pkey)
            .map_err(|e| format!("V2 sig Signer: {e}"))?;
        let computed_id = verifier
            .sign_oneshot_to_vec(&plaintext_hash)
            .map_err(|e| format!("V2 sig sign: {e}"))?;

        if &computed_id[..chunk_id21.len() - 1] != &chunk_id21[1..] {
            return Err("V2 chunk HMAC mismatch".to_string());
        }

        Ok(result)
    }

    /// V1 chunk decrypt — AES-128-CFB with `key[1..]` as the 16-byte
    /// key, no IV. Ported from upstream's mmcs.rs:695.
    pub(super) fn decrypt_v1_chunk(key17: &[u8], data: &[u8]) -> Result<Vec<u8>, String> {
        if key17.len() != 17 {
            return Err(format!("V1 key wrong length: {}", key17.len()));
        }
        openssl_decrypt(Cipher::aes_128_cfb128(), &key17[1..], None, data)
            .map_err(|e| format!("V1 AES-CFB decrypt: {e}"))
    }

    /// Re-authorize a fresh AuthorizeGetResponse body directly from
    /// MMCS. The cached body that came in the original AssetGetResponse
    /// may be tied to a specific auth session that's since expired; on
    /// the recovery path we want to re-issue the authorization to make
    /// sure the container HTTP URLs are still valid.
    ///
    /// We don't currently re-authorize — the cached body from CloudKit
    /// is usually still fresh (CloudKit authorization is bundled with
    /// the AssetGetResponse). This helper is reserved for future use if
    /// we start seeing auth expiry.
    #[allow(dead_code)]
    pub(super) fn noop_reauth() {}

    /// Fetch the entire HTTP body of one container. Returns bytes ready
    /// to slice by offset.
    pub(super) async fn fetch_container_body(
        container: &rustpush::mmcsp::Container,
        user_agent: &str,
    ) -> Result<Vec<u8>, String> {
        let req = container
            .request
            .as_ref()
            .ok_or_else(|| "container has no HttpRequest".to_string())?;
        let url = format!("{}://{}:{}{}", req.scheme, req.domain, req.port, req.path);

        let mut builder = match req.method.as_str() {
            "GET" => HTTP_CLIENT.get(&url),
            other => return Err(format!("unsupported MMCS method: {}", other)),
        }
        .header("x-apple-request-uuid", uuid::Uuid::new_v4().to_string().to_uppercase())
        .header("user-agent", user_agent);

        let complete_at_edge = req.headers.iter().any(|h| {
            h.name == "x-apple-put-complete-at-edge-version" && h.value == "2"
        });

        for header in &req.headers {
            if (header.name == "Content-Length" && complete_at_edge) || header.name == "Host" {
                continue;
            }
            builder = builder.header(header.name.clone(), header.value.clone());
        }

        let resp = builder
            .send()
            .await
            .map_err(|e| format!("MMCS fetch send: {e}"))?;

        if !resp.status().is_success() {
            return Err(format!("MMCS fetch HTTP {}", resp.status()));
        }

        let bytes = resp
            .bytes()
            .await
            .map_err(|e| format!("MMCS fetch body: {e}"))?;
        Ok(bytes.to_vec())
    }

    /// Manual Ford download of one asset. This is the V1+V2-capable
    /// replacement for upstream's `get_assets` → `get_mmcs` chain. It
    /// bypasses all of upstream's panicking code paths by reimplementing
    /// the download primitives at this layer.
    ///
    /// Inputs:
    /// - `asset_response`: the AuthorizeGetResponse body bytes from the
    ///   original CloudKit AssetGetResponse for this bundled_request_id
    /// - `asset_signature`: `record.lqa.signature` — identifies our file
    ///   within the response's `references` list
    /// - `asset_ford_key`: `record.lqa.protection_info.protection_info`
    ///   — the record's own declared Ford key (usually right, but not
    ///   always in the dedup case)
    /// - `user_agent`: the CloudKit user agent string (for HTTP)
    /// - `record_name`: diagnostic only
    ///
    /// Returns the decrypted file bytes.
    pub(super) async fn manual_ford_download_asset(
        asset_response_body: &[u8],
        asset_signature: &[u8],
        asset_ford_key: &[u8],
        user_agent: &str,
        record_name: &str,
    ) -> Result<Vec<u8>, String> {
        let response = rustpush::mmcsp::AuthorizeGetResponse::decode(asset_response_body)
            .map_err(|e| format!("decode AuthorizeGetResponse: {e}"))?;

        let f1 = response.f1.ok_or_else(|| {
            let reason = response
                .error
                .and_then(|e| e.f2)
                .map(|f2| f2.reason)
                .unwrap_or_default();
            format!("MMCS server returned error: {}", reason)
        })?;

        // Find OUR file's chunk references + ford_reference.
        let wanted = f1
            .references
            .iter()
            .find(|r| &r.file_checksum[..] == asset_signature)
            .ok_or_else(|| {
                format!(
                    "no references entry matches signature {}",
                    encode_hex(asset_signature)
                )
            })?;

        // Fetch every container body once up front. Master's approach
        // streams chunks through a matcher; we just pull each container
        // in full (simpler, avoids needing the private MMCSMatcher).
        // For typical attachments this is ONE container per file.
        let mut container_bodies: Vec<Vec<u8>> = Vec::with_capacity(f1.containers.len());
        for container in &f1.containers {
            let body = fetch_container_body(container, user_agent).await?;
            container_bodies.push(body);
        }

        // ---- Build ford_keymap: data_chunk_checksum -> (ford_key, chunk_len) ----
        //
        // If this file has a ford_reference, decrypt the Ford blob and
        // flatten into per-chunk keys. Otherwise, we expect V1 per-chunk
        // keys to come from `chunk.meta.encryption_key`.
        let mut ford_keymap: HashMap<Vec<u8>, (Vec<u8>, Vec<u8>)> = HashMap::new();

        if let Some(ford_ref) = &wanted.ford_reference {
            let container_idx = ford_ref.container_index as usize;
            let chunk_idx = ford_ref.chunk_index as usize;
            let container = f1
                .containers
                .get(container_idx)
                .ok_or_else(|| format!("ford_reference container_idx {} OOB", container_idx))?;
            let body = container_bodies
                .get(container_idx)
                .ok_or_else(|| "ford container body missing".to_string())?;
            let chunk = container
                .chunks
                .get(chunk_idx)
                .ok_or_else(|| format!("ford_reference chunk_idx {} OOB", chunk_idx))?;
            let enc_meta = chunk
                .encryption
                .as_ref()
                .ok_or_else(|| "ford chunk has no encryption meta".to_string())?;

            let blob_offset = enc_meta.offset as usize;
            let blob_size = enc_meta.size as usize;
            if blob_offset + blob_size > body.len() {
                return Err(format!(
                    "ford blob OOB: offset={} size={} body_len={}",
                    blob_offset,
                    blob_size,
                    body.len()
                ));
            }
            let ford_blob = &body[blob_offset..blob_offset + blob_size];

            // SIV retry over record_own_key + cache.
            let plaintext = ford_siv_retry(
                asset_ford_key,
                &wanted.ford_checksum,
                ford_blob,
                record_name,
            )
            .ok_or_else(|| {
                format!(
                    "Ford SIV failed: tried record key + {} cached keys, no match",
                    ford_key_cache_size()
                )
            })?;

            // Decode plaintext as our local V1/V2-capable FordChunk.
            let ford_chunk = LocalFordChunk::decode(&plaintext[..])
                .map_err(|e| format!("decode FordChunk: {e}"))?;

            let ford_entries = flatten_ford_entries(ford_chunk)
                .ok_or_else(|| "FordChunk has neither item nor item_v2".to_string())?;

            if ford_entries.len() != wanted.chunk_references.len() {
                warn!(
                    "manual_ford_download {}: ford_entries={} != chunk_references={} (will map what's present)",
                    record_name,
                    ford_entries.len(),
                    wanted.chunk_references.len()
                );
            }

            for (entry, chunk_ref) in ford_entries.iter().zip(wanted.chunk_references.iter()) {
                let cidx = chunk_ref.container_index as usize;
                let kidx = chunk_ref.chunk_index as usize;
                let chunk = f1
                    .containers
                    .get(cidx)
                    .and_then(|c| c.chunks.get(kidx))
                    .ok_or_else(|| {
                        format!("chunk_ref ({},{}) OOB", cidx, kidx)
                    })?;
                let meta = chunk
                    .meta
                    .as_ref()
                    .ok_or_else(|| "data chunk has no meta".to_string())?;
                ford_keymap.insert(meta.checksum.clone(), (entry.0.clone(), entry.1.clone()));
            }
        }

        // ---- Assemble the file by iterating data chunk refs in order ----
        let mut out = Vec::new();
        for chunk_ref in &wanted.chunk_references {
            let cidx = chunk_ref.container_index as usize;
            let kidx = chunk_ref.chunk_index as usize;
            let container = f1
                .containers
                .get(cidx)
                .ok_or_else(|| format!("chunk_ref container OOB {}", cidx))?;
            let body = container_bodies
                .get(cidx)
                .ok_or_else(|| "container body missing".to_string())?;
            let chunk = container
                .chunks
                .get(kidx)
                .ok_or_else(|| format!("chunk_ref chunk OOB {}", kidx))?;
            let meta = chunk
                .meta
                .as_ref()
                .ok_or_else(|| "chunk has no meta".to_string())?;

            let offset = meta.offset as usize;
            let size = meta.size as usize;
            if offset + size > body.len() {
                return Err(format!(
                    "chunk OOB: offset={} size={} body_len={}",
                    offset,
                    size,
                    body.len()
                ));
            }
            let encrypted = &body[offset..offset + size];

            let plaintext = if let Some((fkey, clen)) = ford_keymap.get(&meta.checksum) {
                // V2: Ford-derived key + chunk_len
                let chunk_id: [u8; 21] = meta
                    .checksum
                    .clone()
                    .try_into()
                    .map_err(|_| "chunk checksum not 21 bytes".to_string())?;
                decrypt_v2_chunk(fkey, clen, &chunk_id, encrypted)?
            } else if let Some(enc_key) = &meta.encryption_key {
                // V1: per-chunk AES-128-CFB key from the authorize response
                decrypt_v1_chunk(enc_key, encrypted)?
            } else {
                // Unencrypted chunk
                encrypted.to_vec()
            };

            out.extend_from_slice(&plaintext);
        }

        Ok(out)
    }
}

// ============================================================================
// NAC relay config (Apple Silicon hardware keys that can't run in the
// x86-64 unicorn emulator on Linux)
// ============================================================================
//
// Some hardware keys (especially those extracted from Apple Silicon Macs)
// cannot be driven by the local unicorn x86-64 NAC emulator. For those
// users, `extract-key` embeds a `nac_relay_url` + bearer token +
// (optional) TLS cert fingerprint into the hardware-key JSON blob.
//
// At runtime, `_create_config_from_hardware_key_inner` calls
// `register_nac_relay` to stash those values here, then forwards them
// into `open_absinthe::nac::set_relay_config` so open-absinthe's
// `ValidationCtx::new()` can use the relay's 3-step NAC protocol
// instead of running the emulator. This mirrors the macOS Local NAC
// wiring (where open-absinthe's Native variant delegates to
// `nac-validation`) — same integration pattern, just over HTTPS to a
// Mac running `tools/nac-relay`.

// ---------------------------------------------------------------------------
// rustls 0.23 CryptoProvider initialization.
//
// Upstream rustpush pulled in rustls 0.23 transitively. Unlike 0.21/0.22,
// rustls 0.23 does not auto-install a process-wide CryptoProvider when
// multiple provider crates are present in the dep graph; the first TLS
// connection panics with "Could not automatically determine the
// process-level CryptoProvider from Rustls crate features." We install
// aws-lc-rs explicitly the first time any FFI entry point that may open
// a TLS connection is called. install_default() returns Err if a provider
// is already installed; that's fine — we ignore it.
// ---------------------------------------------------------------------------
fn ensure_crypto_provider() {
    use std::sync::Once;
    static INIT: Once = Once::new();
    INIT.call_once(|| {
        let _ = rustls::crypto::aws_lc_rs::default_provider().install_default();
    });
}

// ---------------------------------------------------------------------------
// RelayOSConfig — wraps MacOSConfig for Apple Silicon hardware keys.
//
// Copies master's relay logic: generate_validation_data() calls the relay
// directly and returns the data, bypassing the Apple handshake entirely.
// All other OSConfig methods delegate to the inner MacOSConfig.
// ---------------------------------------------------------------------------

struct RelayOSConfig {
    inner: Arc<rustpush::macos::MacOSConfig>,
    relay_url: String,
    relay_token: Option<String>,
}

#[async_trait::async_trait]
impl OSConfig for RelayOSConfig {
    fn build_activation_info(&self, csr: Vec<u8>) -> rustpush::activation::ActivationInfo {
        self.inner.build_activation_info(csr)
    }
    fn get_activation_device(&self) -> String { self.inner.get_activation_device() }
    async fn generate_validation_data(&self) -> Result<Vec<u8>, rustpush::PushError> {
        // Same logic as master's MacOSConfig relay path: call relay, return directly.
        use base64::{Engine, engine::general_purpose::STANDARD};

        let client = reqwest::Client::builder()
            .danger_accept_invalid_certs(true)
            .build()
            .map_err(|e| rustpush::PushError::RelayError(0, format!("Failed to build relay client: {e}")))?;

        let mut req = client.post(&self.relay_url);
        if let Some(ref token) = self.relay_token {
            req = req.header("Authorization", format!("Bearer {token}"));
        }

        let resp = req.send().await
            .map_err(|e| rustpush::PushError::RelayError(0, format!("NAC relay request failed: {e}")))?;

        if !resp.status().is_success() {
            let status = resp.status().as_u16();
            let body = resp.text().await.unwrap_or_default();
            return Err(rustpush::PushError::RelayError(status, format!("NAC relay error: {body}")));
        }
        let b64 = resp.text().await
            .map_err(|e| rustpush::PushError::RelayError(0, format!("NAC relay read error: {e}")))?;
        let data = STANDARD.decode(b64.trim())
            .map_err(|e| rustpush::PushError::RelayError(0, format!("NAC relay base64 decode: {e}")))?;
        info!("NAC relay: got {} bytes of validation data from {}", data.len(), self.relay_url);
        Ok(data)
    }
    fn get_protocol_version(&self) -> u32 { self.inner.get_protocol_version() }
    fn get_register_meta(&self) -> rustpush::RegisterMeta { self.inner.get_register_meta() }
    fn get_normal_ua(&self, item: &str) -> String { self.inner.get_normal_ua(item) }
    fn get_mme_clientinfo(&self, for_item: &str) -> String { self.inner.get_mme_clientinfo(for_item) }
    fn get_version_ua(&self) -> String { self.inner.get_version_ua() }
    fn get_device_name(&self) -> String { self.inner.get_device_name() }
    fn get_device_uuid(&self) -> String { self.inner.get_device_uuid() }
    fn get_private_data(&self) -> plist::Dictionary { self.inner.get_private_data() }
    fn get_debug_meta(&self) -> rustpush::DebugMeta { self.inner.get_debug_meta() }
    fn get_login_url(&self) -> &'static str { self.inner.get_login_url() }
    fn get_serial_number(&self) -> String { self.inner.get_serial_number() }
    fn get_gsa_hardware_headers(&self) -> HashMap<String, String> { self.inner.get_gsa_hardware_headers() }
    fn get_aoskit_version(&self) -> String { self.inner.get_aoskit_version() }
    fn get_udid(&self) -> String { self.inner.get_udid() }
}

// ============================================================================
// Local CloudKit record type that understands the `avid` field.
// ============================================================================
//
// Upstream `rustpush::cloud_messages::CloudAttachment` only declares `cm` and
// `lqa`, so parsed records drop the `avid` Asset (Live Photo MOV companion).
// Our vendored fork added `pub avid: Asset` to support Live Photos.
//
// Instead of patching upstream, we define our own CloudKit record type with
// the same `attachment` record ID and the same field names — CloudKit's
// on-the-wire record format is schema-driven by field name, so a local
// struct that derives `CloudKitRecord` with an extra `avid: Asset` field
// parses the exact same server-side records and populates all three fields.
// Upstream's original `CloudAttachment` still works wherever we don't need
// the avid — this type is used anywhere we DO need it (attachment sync for
// `has_avid` detection, Live Photo MOV download).

// Imports the derive macro sees at its expansion site. The `CloudKitRecord`
// derive emits references to `CloudKitEncryptor` unqualified, and to
// `cloudkit_proto::*` by crate name — both need to resolve in our scope.
use rustpush::cloudkit_derive::CloudKitRecord;
use cloudkit_proto::{Asset, CloudKitEncryptor};

#[derive(CloudKitRecord, Debug, Default, Clone)]
#[cloudkit_record(type = "attachment", encrypted)]
pub struct CloudAttachmentWithAvid {
    pub cm: rustpush::cloud_messages::GZipWrapper<rustpush::cloud_messages::AttachmentMeta>,
    pub lqa: Asset,
    pub avid: Asset,
}

// ============================================================================
// Wrapper types
// ============================================================================

#[derive(uniffi::Object)]
pub struct WrappedAPSState {
    pub inner: Option<APSState>,
}

#[uniffi::export]
impl WrappedAPSState {
    #[uniffi::constructor]
    pub fn new(string: Option<String>) -> Arc<Self> {
        Arc::new(Self {
            inner: string
                .and_then(|s| if s.is_empty() { None } else { Some(s) })
                .and_then(|s| plist_from_string::<APSState>(&s).ok()),
        })
    }

    pub fn to_string(&self) -> String {
        plist_to_string(&self.inner).unwrap_or_default()
    }
}

#[derive(uniffi::Object)]
pub struct WrappedAPSConnection {
    pub inner: rustpush::APSConnection,
}

#[uniffi::export(async_runtime = "tokio")]
impl WrappedAPSConnection {
    pub async fn state(&self) -> Arc<WrappedAPSState> {
        Arc::new(WrappedAPSState {
            inner: Some(self.inner.state.read().await.clone()),
        })
    }
}

#[derive(uniffi::Record)]
pub struct IDSUsersWithIdentityRecord {
    pub users: Arc<WrappedIDSUsers>,
    pub identity: Arc<WrappedIDSNGMIdentity>,
    /// TokenProvider for iCloud services (CardDAV, CloudKit, etc.)
    pub token_provider: Option<Arc<WrappedTokenProvider>>,
    /// Persist data for restoring the TokenProvider after restart.
    pub account_persist: Option<AccountPersistData>,
}

/// Data needed to restore a TokenProvider from persisted state.
/// Stored in session.json so it survives database resets.
#[derive(uniffi::Record)]
pub struct AccountPersistData {
    pub username: String,
    pub hashed_password_hex: String,
    pub pet: String,
    pub adsid: String,
    pub dsid: String,
    pub spd_base64: String,
}

#[derive(uniffi::Object)]
pub struct WrappedIDSUsers {
    pub inner: Vec<IDSUser>,
}

#[uniffi::export]
impl WrappedIDSUsers {
    #[uniffi::constructor]
    pub fn new(string: Option<String>) -> Arc<Self> {
        Arc::new(Self {
            inner: string
                .and_then(|s| if s.is_empty() { None } else { Some(s) })
                .and_then(|s| plist_from_string(&s).ok())
                .unwrap_or_default(),
        })
    }

    pub fn to_string(&self) -> String {
        plist_to_string(&self.inner).unwrap_or_default()
    }

    pub fn login_id(&self, i: u64) -> String {
        self.inner[i as usize].user_id.clone()
    }

    pub fn get_handles(&self) -> Vec<String> {
        self.inner.iter()
            .flat_map(|user| {
                user.registration.get("com.apple.madrid")
                    .map(|reg| reg.handles.clone())
                    .unwrap_or_default()
            })
            .collect()
    }

    /// Check that all keystore keys referenced by the user state actually exist.
    /// Returns false if any auth/id keypair alias is missing from the keystore,
    /// which means the keystore was wiped or never migrated and re-login is needed.
    pub fn validate_keystore(&self) -> bool {
        if self.inner.is_empty() {
            return true;
        }
        for user in &self.inner {
            let alias = &user.auth_keypair.private.0;
            if keystore().get_key_type(alias).ok().flatten().is_none() {
                warn!("Keystore key '{}' not found for user '{}' — keystore/state mismatch", alias, user.user_id);
                return false;
            }
        }
        true
    }
}

#[derive(uniffi::Object)]
pub struct WrappedIDSNGMIdentity {
    pub inner: IDSNGMIdentity,
}

#[uniffi::export]
impl WrappedIDSNGMIdentity {
    #[uniffi::constructor]
    pub fn new(string: Option<String>) -> Arc<Self> {
        Arc::new(Self {
            inner: string
                .and_then(|s| if s.is_empty() { None } else { Some(s) })
                .and_then(|s| plist_from_string(&s).ok())
                .unwrap_or_else(|| IDSNGMIdentity::new().expect("Failed to create new identity")),
        })
    }

    pub fn to_string(&self) -> String {
        plist_to_string(&self.inner).unwrap_or_default()
    }
}

#[derive(uniffi::Object)]
pub struct WrappedOSConfig {
    pub config: Arc<dyn OSConfig>,
    /// True when this config was built from an Apple Silicon hardware key
    /// that requires the NAC relay server to be running during registration.
    pub has_nac_relay: bool,
    /// NAC relay URL for pre-fetching validation data (Apple Silicon keys).
    pub(crate) relay_url: Option<String>,
    /// NAC relay bearer token.
    pub(crate) relay_token: Option<String>,
}

#[uniffi::export]
impl WrappedOSConfig {
    /// Get the device UUID from the underlying OSConfig.
    pub fn get_device_id(&self) -> String {
        self.config.get_device_uuid()
    }

    /// Returns true if this config requires the NAC relay server to be
    /// running during initial registration (Apple Silicon hardware keys).
    pub fn requires_nac_relay(&self) -> bool {
        self.has_nac_relay
    }
}

#[derive(thiserror::Error, uniffi::Error, Debug)]
pub enum WrappedError {
    #[error("{msg}")]
    GenericError { msg: String },
}

impl From<rustpush::PushError> for WrappedError {
    fn from(e: rustpush::PushError) -> Self {
        WrappedError::GenericError { msg: format!("{}", e) }
    }
}

fn is_pcs_recoverable_error(err: &rustpush::PushError) -> bool {
    matches!(
        err,
        rustpush::PushError::ShareKeyNotFound(_)
            | rustpush::PushError::DecryptionKeyNotFound(_)
            | rustpush::PushError::PCSRecordKeyMissing
            | rustpush::PushError::MasterKeyNotFound
    )
}

/// True when the error is a missing-MME-token failure. Upstream's
/// `get_mme_token(name)` returns `TokenMissing` when the seeded delegate
/// lacks the requested name (e.g. `cloudKitToken`), and it only
/// auto-refreshes the delegate if it's older than one week. A partial or
/// stale seeded delegate that's still within that week gets permanently
/// stuck on this error until we force a refresh_mme() ourselves.
fn is_token_missing_error(err: &rustpush::PushError) -> bool {
    matches!(err, rustpush::PushError::TokenMissing)
}

fn keychain_retry_delay(attempt: usize) -> Duration {
    match attempt {
        0..=4 => Duration::from_secs(2),
        5..=11 => Duration::from_secs(4),
        _ => Duration::from_secs(6),
    }
}

/// Try to recover usable CloudKit keys after a NotInClique failure.
///
/// Upstream's `sync_keychain` gates everything behind `is_in_clique()`, which
/// calls `sync_trust()` → Cuttlefish `fetchChanges`. If another device (e.g.
/// an iPhone running Messages) posts a trust-update that excludes us, every
/// `is_in_clique()` call returns false and `sync_keychain` is dead.
///
/// Recovery strategy — all public upstream APIs, no internal field access:
///   1. Try to refresh TLK shares via `fetch_shares_for` + `store_keys`.
///      These hit a different Cuttlefish endpoint that doesn't check clique
///      membership, so they work even when we appear excluded.
///   2. Check `state.items` (public field): if the prior join/sync already
///      populated the CloudKit key cache, those keys are sufficient for
///      decryption. Return Ok — callers proceed with cached keys.
///   3. If cache is empty (fresh install, no prior sync), return the real
///      error so the caller knows it cannot proceed.
///
/// This mirrors what master's rustpush fork achieves by ignoring self-exclusion
/// inside `fast_forward_trust`, but implemented entirely at the wrapper layer.
async fn recover_keychain_after_exclusion(
    keychain: &rustpush::keychain::KeychainClient<BridgeDefaultAnisetteProvider>,
    context: &str,
) -> Result<(), WrappedError> {
    // Step 1: attempt TLK share refresh (best-effort; doesn't need clique membership).
    let identity_opt = {
        let state = keychain.state.read().await;
        state.user_identity.clone()
    };
    if let Some(identity) = identity_opt {
        match keychain.fetch_shares_for(&identity).await {
            Ok(shares) if !shares.is_empty() => {
                match keychain.store_keys(&shares).await {
                    Ok(()) => info!("{}: refreshed {} TLK share(s) despite exclusion", context, shares.len()),
                    Err(e) => warn!("{}: store_keys failed (non-fatal): {}", context, e),
                }
            }
            Ok(_) => info!("{}: no TLK shares returned; using persisted keystore", context),
            Err(e) => warn!("{}: fetch_shares_for failed (non-fatal): {}", context, e),
        }
    }

    // Step 2: check whether the persisted CloudKit key cache is usable.
    let has_cached_keys = {
        let state = keychain.state.read().await;
        state.items.values().any(|zone| !zone.keys.is_empty())
    };

    if has_cached_keys {
        warn!(
            "{}: excluded from Cuttlefish trust circle by another device, \
             but CloudKit key cache is populated — using cached keys. \
             Messages in iCloud sync will work; key rotation may require re-login.",
            context
        );
        return Ok(());
    }

    // Step 3: nothing to fall back on — propagate the error.
    Err(WrappedError::GenericError {
        msg: format!("{} keychain sync failed: not in clique and no cached keys available", context),
    })
}

async fn sync_keychain_with_retries(
    keychain: &rustpush::keychain::KeychainClient<BridgeDefaultAnisetteProvider>,
    max_attempts: usize,
    context: &str,
) -> Result<(), WrappedError> {
    let attempts = max_attempts.max(1);
    let mut last_err: Option<rustpush::PushError> = None;
    for attempt in 0..attempts {
        match keychain.sync_keychain(&rustpush::keychain::KEYCHAIN_ZONES).await {
            Ok(()) => {
                if attempt > 0 {
                    info!("{} keychain sync recovered after {} attempt(s)", context, attempt + 1);
                }
                return Ok(());
            }
            Err(err) => {
                if matches!(err, rustpush::PushError::NotInClique) {
                    // Don't retry — NotInClique won't resolve with more attempts.
                    // Fall back to cached CloudKit keys if available.
                    return recover_keychain_after_exclusion(keychain, context).await;
                }

                let retrying = attempt + 1 < attempts;
                warn!(
                    "{} keychain sync attempt {}/{} failed: {}{}",
                    context,
                    attempt + 1,
                    attempts,
                    err,
                    if retrying { " (retrying)" } else { "" }
                );
                last_err = Some(err);
                if retrying {
                    tokio::time::sleep(keychain_retry_delay(attempt)).await;
                }
            }
        }
    }

    let msg = match last_err {
        Some(err) => format!("{} keychain sync failed after retries: {}", context, err),
        None => format!("{} keychain sync failed", context),
    };
    Err(WrappedError::GenericError { msg })
}

async fn refresh_recoverable_tlk_shares(
    keychain: &Arc<rustpush::keychain::KeychainClient<BridgeDefaultAnisetteProvider>>,
    context: &str,
) -> Result<(), WrappedError> {
    let identity_opt = {
        let state = keychain.state.read().await;
        state.user_identity.clone()
    };

    let Some(identity) = identity_opt else {
        warn!("{}: no keychain user identity available for TLK share refresh", context);
        return Ok(());
    };

    match keychain.fetch_shares_for(&identity).await {
        Ok(shares) => {
            info!("{}: fetched {} recoverable TLK share(s)", context, shares.len());
            if !shares.is_empty() {
                keychain.store_keys(&shares).await?;
            }
        }
        Err(err) => {
            // Best-effort: we still attempt regular keychain sync / CloudKit probes.
            warn!("{}: failed to fetch recoverable TLK shares: {}", context, err);
        }
    }

    Ok(())
}

async fn finalize_keychain_setup_with_probe(
    keychain: Arc<rustpush::keychain::KeychainClient<BridgeDefaultAnisetteProvider>>,
    cloudkit: Arc<rustpush::cloudkit::CloudKitClient<BridgeDefaultAnisetteProvider>>,
    max_attempts: usize,
) -> Result<(), WrappedError> {
    let cloud_messages = rustpush::cloud_messages::CloudMessagesClient::new(cloudkit, keychain.clone());
    let attempts = max_attempts.max(1);

    for attempt in 0..attempts {
        let attempt_no = attempt + 1;
        if attempt == 0 {
            // After join, explicitly refresh recoverable TLK shares for this new peer.
            // Some accounts/devices need an extra fetch before all view keys materialize.
            refresh_recoverable_tlk_shares(&keychain, "Login finalize").await?;
        }
        sync_keychain_with_retries(&keychain, 1, "Login finalize").await?;

        match cloud_messages.sync_chats(None).await {
            Ok((_token, chats, status)) => {
                info!(
                    "CloudKit decrypt probe (chats) succeeded on attempt {} (status={}, records={})",
                    attempt_no,
                    status,
                    chats.len()
                );
            }
            Err(err) => {
                if matches!(err, rustpush::PushError::NotInClique) {
                    return Err(WrappedError::GenericError {
                        msg: format!("CloudKit probe failed: {}", err),
                    });
                }

                let retrying = attempt_no < attempts;
                if is_pcs_recoverable_error(&err) {
                    warn!(
                        "CloudKit decrypt probe (chats) missing PCS keys on attempt {}/{}: {}{}",
                        attempt_no,
                        attempts,
                        err,
                        if retrying { " (retrying)" } else { "" }
                    );
                } else {
                    warn!(
                        "CloudKit decrypt probe (chats) failed on attempt {}/{}: {}{}",
                        attempt_no,
                        attempts,
                        err,
                        if retrying { " (retrying)" } else { "" }
                    );
                }
                if retrying {
                    if is_pcs_recoverable_error(&err) && attempt % 4 == 0 {
                        refresh_recoverable_tlk_shares(&keychain, "Login finalize").await?;
                    }
                    tokio::time::sleep(keychain_retry_delay(attempt)).await;
                    continue;
                }
                return Err(WrappedError::GenericError {
                    msg: format!("CloudKit decrypt probe failed after retries (chats): {}", err),
                });
            }
        }

        match cloud_messages.sync_messages(None).await {
            Ok((_token, messages, status)) => {
                info!(
                    "CloudKit decrypt probe (messages) succeeded on attempt {} (status={}, records={})",
                    attempt_no,
                    status,
                    messages.len()
                );
                return Ok(());
            }
            Err(err) => {
                if matches!(err, rustpush::PushError::NotInClique) {
                    return Err(WrappedError::GenericError {
                        msg: format!("CloudKit probe failed: {}", err),
                    });
                }

                let retrying = attempt_no < attempts;
                if is_pcs_recoverable_error(&err) {
                    warn!(
                        "CloudKit decrypt probe (messages) missing PCS keys on attempt {}/{}: {}{}",
                        attempt_no,
                        attempts,
                        err,
                        if retrying { " (retrying)" } else { "" }
                    );
                } else {
                    warn!(
                        "CloudKit decrypt probe (messages) failed on attempt {}/{}: {}{}",
                        attempt_no,
                        attempts,
                        err,
                        if retrying { " (retrying)" } else { "" }
                    );
                }
                if retrying {
                    if is_pcs_recoverable_error(&err) && attempt % 4 == 0 {
                        refresh_recoverable_tlk_shares(&keychain, "Login finalize").await?;
                    }
                    tokio::time::sleep(keychain_retry_delay(attempt)).await;
                    continue;
                }
                return Err(WrappedError::GenericError {
                    msg: format!("CloudKit decrypt probe failed after retries (messages): {}", err),
                });
            }
        }
    }

    Err(WrappedError::GenericError {
        msg: "CloudKit decrypt probe failed after retries".into(),
    })
}

// ============================================================================
// Token Provider (iCloud auth for CardDAV, CloudKit, etc.)
// ============================================================================

/// Information about a device that has an escrow bottle in the iCloud Keychain
/// trust circle. Used to let the user choose which device's passcode to enter.
#[derive(uniffi::Record)]
pub struct EscrowDeviceInfo {
    /// Index into the bottles list (used when calling join_keychain_clique_for_device).
    pub index: u32,
    /// Human-readable device name (e.g. "Ludvig's iPhone").
    pub device_name: String,
    /// Device model identifier (e.g. "iPhone15,2").
    pub device_model: String,
    /// Device serial number.
    pub serial: String,
    /// When the escrow bottle was created.
    pub timestamp: String,
}

/// Wraps a TokenProvider that manages MobileMe auth tokens with auto-refresh.
/// Used for iCloud services like CardDAV contacts and CloudKit messages.
///
/// This wrapper stores `account`, `os_config`, and `mme_delegate` alongside the
/// inner TokenProvider because OpenBubbles upstream's TokenProvider keeps these
/// as private fields with no public getters. As part of the zero-patch refactor,
/// we pass them in at construction time and read from our own state.
#[derive(uniffi::Object)]
pub struct WrappedTokenProvider {
    inner: Arc<TokenProvider<BridgeDefaultAnisetteProvider>>,
    account: Arc<rustpush::DebugMutex<AppleAccount<BridgeDefaultAnisetteProvider>>>,
    os_config: Arc<dyn OSConfig>,
    // MobileMe delegate stored as opaque plist XML bytes because
    // `rustpush::auth::MobileMeDelegateResponse` is not nameable from outside the
    // crate in OpenBubbles upstream (`mod auth` is private). Call sites that
    // need a typed `&MobileMeDelegateResponse` reconstruct it via
    // `plist::from_bytes(&bytes)?` and let the compiler infer T from the
    // receiving function's signature (e.g. `KeychainClientState::new(_, _, &T)`).
    mme_delegate_bytes: tokio::sync::Mutex<Option<Vec<u8>>>,
    // Cached (KeychainClient, CloudKitClient) pair. The keychain/cloudkit
    // pair is built by `create_keychain_clients` and used by
    // `get_escrow_devices` and `join_keychain_clique_for_device`.
    //
    // We cache it because each fresh pair forces a fresh CloudKit container
    // init (`ckAppInit` POST to `gateway.icloud.com/setup/setup/ck/v1/ckAppInit`
    // inside upstream `CloudKitOpenContainer::init` at
    // `third_party/rustpush-upstream/src/icloud/cloudkit.rs:1309-1340`).
    // Apple has been observed returning empty bodies (401-with-no-body) for
    // a *second* `ckAppInit` call from the same process minutes after the
    // first one succeeded — typically the get_escrow_devices → user enters
    // passcode → join_keychain_clique_for_device sequence in the install
    // flow. Upstream's init handles 401 by calling `refresh_mme` but then
    // still tries to JSON-decode the (empty) 401 response body
    // (cloudkit.rs:1326-1330), surfacing as
    // "HTTP error: error decoding response body: EOF while parsing a value
    // at line 1 column 0".
    //
    // Caching the pair means the second call reuses the working container
    // from the first call and skips the second ckAppInit entirely, which
    // sidesteps the upstream bug and any Apple-side rate limiting on
    // ckAppInit for the same DSID.
    keychain_clients_cache: tokio::sync::Mutex<Option<(
        Arc<rustpush::keychain::KeychainClient<BridgeDefaultAnisetteProvider>>,
        Arc<rustpush::cloudkit::CloudKitClient<BridgeDefaultAnisetteProvider>>,
    )>>,
}

/// Helper: create CloudKit + Keychain clients from a WrappedTokenProvider.
/// Shared by get_escrow_devices, join_keychain_clique, and join_keychain_clique_for_device.
///
/// Reads dsid/adsid/anisette directly from the locally-stored AppleAccount and
/// the cached MobileMe delegate, avoiding the need for getter methods on
/// TokenProvider itself (which upstream does not expose).
async fn create_keychain_clients(
    wp: &WrappedTokenProvider,
) -> Result<(
    Arc<rustpush::keychain::KeychainClient<BridgeDefaultAnisetteProvider>>,
    Arc<rustpush::cloudkit::CloudKitClient<BridgeDefaultAnisetteProvider>>,
), WrappedError> {
    // Fast path: return the cached pair if we've already built one in this
    // process. See the keychain_clients_cache field docstring on
    // WrappedTokenProvider for why caching is required (Apple returns empty
    // body on a second ckAppInit, and upstream's CloudKitOpenContainer::init
    // can't recover from it). Holding the lock across the slow construction
    // path below also serializes concurrent callers, so two parallel
    // `get_escrow_devices` calls don't race into two separate ckAppInit POSTs.
    let mut cache = wp.keychain_clients_cache.lock().await;
    if let Some(pair) = &*cache {
        debug!("create_keychain_clients: reusing cached keychain/cloudkit pair");
        return Ok(pair.clone());
    }

    let (dsid, adsid, anisette) = {
        let account = wp.account.lock().await;
        let spd = account.spd.as_ref().ok_or(WrappedError::GenericError {
            msg: "AppleAccount has no SPD — not fully logged in".into(),
        })?;
        let dsid = spd.get("DsPrsId")
            .and_then(|v| v.as_unsigned_integer())
            .ok_or(WrappedError::GenericError { msg: "SPD missing DsPrsId".into() })?
            .to_string();
        let adsid = spd.get("adsid")
            .and_then(|v| v.as_string())
            .ok_or(WrappedError::GenericError { msg: "SPD missing adsid".into() })?
            .to_string();
        let anisette = account.anisette.clone();
        (dsid, adsid, anisette)
    };
    // Load MobileMe delegate as typed `MobileMeDelegateResponse` via type
    // inference from its downstream usage (the `&mme_delegate` reference passed
    // to `KeychainClientState::new(..., &MobileMeDelegateResponse)` below — Rust
    // propagates that constraint back to `parse_mme_delegate`'s `T`).
    let mme_delegate = wp.parse_mme_delegate().await?;
    let os_config = wp.os_config.clone();
    let token_provider = wp.inner.clone();

    let cloudkit_state = rustpush::cloudkit::CloudKitState::new(dsid.clone())
        .ok_or(WrappedError::GenericError { msg: "Failed to create CloudKitState".into() })?;
    let cloudkit = Arc::new(rustpush::cloudkit::CloudKitClient {
            state: rustpush::DebugRwLock::new(cloudkit_state),
        anisette: anisette.clone(),
        config: os_config.clone(),
        token_provider: token_provider.clone(),
    });
    let keychain_state_path = format!("{}/trustedpeers.plist", resolve_xdg_data_dir());
    let mut keychain_state: Option<rustpush::keychain::KeychainClientState> = match std::fs::read(&keychain_state_path) {
        Ok(data) => match plist::from_bytes(&data) {
            Ok(state) => Some(state),
            Err(e) => {
                warn!("Failed to parse keychain state at {}: {}", keychain_state_path, e);
                None
            }
        },
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => None,
        Err(e) => {
            warn!("Failed to read keychain state at {}: {}", keychain_state_path, e);
            None
        }
    };
    if keychain_state.is_none() {
        keychain_state = Some(
            rustpush::keychain::KeychainClientState::new(dsid.clone(), adsid.clone(), &mme_delegate)
                .ok_or(WrappedError::GenericError { msg: "Missing KeychainSync config in MobileMe delegate".into() })?
        );
    }
    let path_for_closure = keychain_state_path.clone();
    let keychain = Arc::new(rustpush::keychain::KeychainClient {
        anisette: anisette.clone(),
        token_provider: token_provider.clone(),
            state: rustpush::DebugRwLock::new(keychain_state.expect("keychain state missing")),
        config: os_config.clone(),
        update_state: Box::new(move |state| {
            if let Err(e) = plist::to_file_xml(&path_for_closure, state) {
                warn!("Failed to persist keychain state to {}: {}", path_for_closure, e);
            } else {
                info!("Persisted keychain state to {}", path_for_closure);
            }
        }),
        container: tokio::sync::Mutex::new(None),
        security_container: tokio::sync::Mutex::new(None),
        client: cloudkit.clone(),
    });

    let pair = (keychain, cloudkit);
    *cache = Some(pair.clone());
    Ok(pair)
}

/// Extract device name and model from an EscrowMetadata's client_metadata dictionary.
fn extract_device_info(meta: &rustpush::keychain::EscrowMetadata) -> (String, String) {
    let dict = meta.client_metadata.as_dictionary();
    let device_name = dict
        .and_then(|d| d.get("device_name"))
        .and_then(|v| v.as_string())
        .unwrap_or("Unknown Device")
        .to_string();
    let device_model = dict
        .and_then(|d| d.get("device_model"))
        .and_then(|v| v.as_string())
        .unwrap_or("Unknown")
        .to_string();
    (device_name, device_model)
}

/// Core keychain joining logic used by both join_keychain_clique and join_keychain_clique_for_device.
/// If `preferred_index` is Some, the bottle at that index is tried first before falling back to others.
async fn join_keychain_with_bottles(
    keychain: Arc<rustpush::keychain::KeychainClient<BridgeDefaultAnisetteProvider>>,
    cloudkit: Arc<rustpush::cloudkit::CloudKitClient<BridgeDefaultAnisetteProvider>>,
    bottles: &[(rustpush::cloudkit_proto::EscrowData, rustpush::keychain::EscrowMetadata)],
    passcode: &str,
    preferred_index: Option<u32>,
) -> Result<String, WrappedError> {
    let passcode_bytes = passcode.as_bytes();
    let mut last_err = String::new();

    // Build iteration order: preferred bottle first (if specified), then the rest.
    let indices: Vec<usize> = if let Some(pref) = preferred_index {
        let pref = pref as usize;
        let mut order = vec![pref];
        order.extend((0..bottles.len()).filter(|&i| i != pref));
        order
    } else {
        (0..bottles.len()).collect()
    };

    // If there are many escrow bottles, do a quick probe per bottle first,
    // then one extended probe at the end on the latest successful join.
    let per_bottle_probe_attempts = if bottles.len() > 1 { 3 } else { 24 };

    // Outer stability loop: after joining + probe, verify we stay in the clique.
    // Other devices can exclude us within seconds of joining.
    const MAX_REJOIN_ATTEMPTS: usize = 3;
    let mut rejoin_attempt = 0;

    'stability: loop {
        let mut joined_any = false;
        let mut probe_succeeded = false;
        let mut last_joined_meta: Option<(String, String)> = None;

        for &i in &indices {
            let (data, meta) = &bottles[i];
            info!("Trying bottle {} (serial={})...", i, meta.serial);
            match keychain.join_clique_from_escrow(data, passcode_bytes, passcode_bytes).await {
                Ok(()) => {
                    joined_any = true;
                    last_joined_meta = Some((meta.serial.clone(), meta.build.clone()));
                    info!("Successfully joined keychain trust circle via bottle {}", i);
                    info!(
                        "Finalizing keychain setup (sync + CloudKit decrypt probe), attempts={}",
                        per_bottle_probe_attempts
                    );
                    match finalize_keychain_setup_with_probe(keychain.clone(), cloudkit.clone(), per_bottle_probe_attempts).await {
                        Ok(()) => {
                            probe_succeeded = true;
                            break; // probe passed, go to stability check
                        }
                        Err(e) => {
                            warn!(
                                "Bottle {} joined, but CloudKit decrypt probe failed: {}. Trying next bottle...",
                                i,
                                e
                            );
                            last_err = format!("{}", e);
                        }
                    }
                }
                Err(e) => {
                    warn!("Bottle {} failed: {}", i, e);
                    last_err = format!("{}", e);
                }
            }
        }

        if !joined_any {
            return Err(WrappedError::GenericError {
                msg: format!("All {} bottles failed. Last error: {}", bottles.len(), last_err)
            });
        }

        // The CloudKit decrypt probe already verified clique membership
        // (sync_keychain internally checks is_in_clique). Calling is_in_clique()
        // again here triggers another sync_trust() → Cuttlefish fetchChanges,
        // which can transiently fail to include us and reset our local state.
        // Skip this check when the probe already confirmed membership; the
        // stability loop below catches any subsequent exclusion.
        if !probe_succeeded && !keychain.is_in_clique().await {
            if rejoin_attempt >= MAX_REJOIN_ATTEMPTS {
                return Err(WrappedError::GenericError {
                    msg: format!("Excluded from clique after {} rejoin attempts. Last error: {}", rejoin_attempt, last_err)
                });
            }
            rejoin_attempt += 1;
            warn!("Not in clique after bottle probes, rejoin attempt {}/{}", rejoin_attempt, MAX_REJOIN_ATTEMPTS);
            continue 'stability;
        }

        // Stability check: wait, re-sync trust, verify we're still included.
        // Other devices can exclude us within seconds of joining.
        info!("Verifying clique membership stability...");
        for check in 0..3 {
            tokio::time::sleep(std::time::Duration::from_secs(3)).await;
            if !keychain.is_in_clique().await {
                rejoin_attempt += 1;
                warn!(
                    "Excluded from clique during stability check {} — re-joining (attempt {}/{})",
                    check + 1, rejoin_attempt, MAX_REJOIN_ATTEMPTS
                );
                if rejoin_attempt > MAX_REJOIN_ATTEMPTS {
                    return Err(WrappedError::GenericError {
                        msg: "Repeatedly excluded from iCloud Keychain trust circle by another device. \
                              Try disabling and re-enabling 'Messages in iCloud' on your iPhone, then retry."
                            .into()
                    });
                }
                continue 'stability;
            }
        }

        // Still in clique after stability checks
        let (serial, build) = last_joined_meta.unwrap_or(("unknown".into(), "unknown".into()));
        info!("Clique membership stable after {} stability checks", 3);
        return Ok(format!(
            "Joined iCloud Keychain and verified CloudKit access (device: serial={}, build={})",
            serial, build,
        ));
    }
}

/// Shared snapshot/merge body for proactive PET refresh. Used by both
/// `restore_token_provider` (one-shot at startup) and
/// `WrappedTokenProvider::refresh_pet_token` (periodic mid-run).
///
/// Snapshot `account.tokens` before the call, run `login_email_pass`, then
/// merge the snapshot back so any token upstream didn't (re)write is
/// preserved. Upstream's `login_email_pass` can unconditionally overwrite
/// `self.tokens` when SPD contains a `t` dict (client.rs:776), which on a
/// non-LoggedIn return path would partially wipe a good token map. The merge
/// guarantees we never exit with fewer tokens than we entered.
///
/// NeedsExtraStep with a populated PET is treated as success (see upstream
/// LoginSession::finish / client.rs:1039).
///
/// Infallible — any auth/network error is logged and swallowed so callers
/// (especially the periodic tick) can retry on the next cycle.
async fn refresh_pet_with_snapshot(
    account: &mut AppleAccount<BridgeDefaultAnisetteProvider>,
    username: &str,
    hashed_password: &[u8],
    context: &str,
) {
    let snapshot = std::mem::take(&mut account.tokens);
    match account.login_email_pass(username, hashed_password).await {
        Ok(icloud_auth::LoginState::LoggedIn) => {
            info!("{}: proactive PET refresh succeeded", context);
        }
        Ok(state) => {
            if account.tokens.contains_key("com.apple.gs.idms.pet") {
                info!(
                    "{}: PET refresh returned {:?} but PET was populated — treating as success",
                    context, state
                );
            } else {
                warn!(
                    "{}: proactive PET refresh returned non-logged-in state: {:?} — manual re-login may be required",
                    context, state
                );
            }
        }
        Err(err) => {
            warn!("{}: proactive PET refresh failed (non-fatal): {}", context, err);
        }
    }
    for (k, v) in snapshot {
        account.tokens.entry(k).or_insert(v);
    }
}

#[uniffi::export(async_runtime = "tokio")]
impl WrappedTokenProvider {
    /// Get HTTP headers needed for iCloud MobileMe API calls.
    /// Includes Authorization (X-MobileMe-AuthToken) and anisette headers.
    ///
    /// OpenBubbles upstream exposes `TokenProvider::get_mme_token(&str)` which
    /// returns the mmeAuthToken (and auto-refreshes internally). We build the
    /// Apple-required HTTP headers (X-MobileMe-AuthToken, X-Client-UDID,
    /// X-MMe-Client-Info, plus anisette) locally from our stored state.
    pub async fn get_icloud_auth_headers(&self) -> Result<HashMap<String, String>, WrappedError> {
        let token = self.inner.get_mme_token("mmeAuthToken").await
            .map_err(|e| WrappedError::GenericError {
                msg: format!("Failed to fetch mmeAuthToken: {}", e),
            })?;
        let dsid = self.get_dsid().await?;

        // Basic auth: "dsid:mmeAuthToken" base64-encoded in X-MMe-Client-Info? No.
        // The header "Authorization: Basic base64(dsid:token)" is what Apple wants.
        let auth_header_value = format!(
            "Basic {}",
            BASE64_STANDARD.encode(format!("{}:{}", dsid, token)),
        );

        let mut headers: HashMap<String, String> = HashMap::new();
        headers.insert("Authorization".to_string(), auth_header_value);
        headers.insert("X-Client-UDID".to_string(), self.os_config.get_udid().to_lowercase());
        headers.insert(
            "X-MMe-Client-Info".to_string(),
            self.os_config.get_mme_clientinfo("com.apple.AppleAccount/1.0 (com.apple.Preferences/1112.96)"),
        );
        headers.insert("X-MMe-Country".to_string(), "US".to_string());
        headers.insert("X-MMe-Language".to_string(), "en".to_string());

        // Anisette headers from the AppleAccount. `get_headers()` returns a
        // `&HashMap<String, String>` so we clone the entries we need.
        let anisette = {
            let account = self.account.lock().await;
            account.anisette.clone()
        };
        let mut anisette_guard = anisette.lock().await;
        let anisette_headers = anisette_guard.get_headers().await
            .map_err(|e| WrappedError::GenericError {
                msg: format!("Failed to fetch anisette headers: {}", e),
            })?;
        for (k, v) in anisette_headers.iter() {
            headers.insert(k.clone(), v.clone());
        }
        drop(anisette_guard);

        Ok(headers)
    }

    /// Get the contacts CardDAV URL from the MobileMe delegate config.
    /// Reads from our locally-cached MobileMe delegate plist bytes via generic
    /// `plist::Value` traversal (we can't name `MobileMeDelegateResponse` from
    /// outside the rustpush crate, so we use untyped plist parsing here).
    /// Runs the bytes through `normalize_mme_delegate_dict` first so we look
    /// up the URL on a single canonical shape regardless of which of the
    /// three persisted shapes is on disk (see normalize comment for details).
    pub async fn get_contacts_url(&self) -> Result<Option<String>, WrappedError> {
        let bytes_guard = self.mme_delegate_bytes.lock().await;
        let Some(bytes) = bytes_guard.as_ref() else {
            return Ok(None);
        };
        let value: plist::Value = plist::from_bytes(bytes).map_err(|e| {
            WrappedError::GenericError { msg: format!("Invalid MobileMe delegate plist: {}", e) }
        })?;
        let normalized = normalize_mme_delegate_dict(value);
        // After normalize: `{tokens, config: {com.apple.Dataclass.Contacts: {url}, ...}}`.
        let url = normalized
            .as_dictionary()
            .and_then(|d| d.get("config"))
            .and_then(|v| v.as_dictionary())
            .and_then(|d| d.get("com.apple.Dataclass.Contacts"))
            .and_then(|v| v.as_dictionary())
            .and_then(|d| d.get("url"))
            .and_then(|v| v.as_string())
            .map(|s| s.to_string());
        Ok(url)
    }

    /// Get the DSID for this account (from AppleAccount's SPD dictionary).
    pub async fn get_dsid(&self) -> Result<String, WrappedError> {
        let account = self.account.lock().await;
        let spd = account.spd.as_ref().ok_or(WrappedError::GenericError {
            msg: "AppleAccount has no SPD — not fully logged in".into(),
        })?;
        let dsid = spd.get("DsPrsId")
            .and_then(|v| v.as_unsigned_integer())
            .ok_or(WrappedError::GenericError { msg: "SPD missing DsPrsId".into() })?
            .to_string();
        Ok(dsid)
    }

    /// Get the serialized MobileMe delegate as a plist string (for persistence).
    /// Returns None if no delegate is cached. The plist bytes are stored opaquely
    /// and passed through as UTF-8 text.
    pub async fn get_mme_delegate_json(&self) -> Result<Option<String>, WrappedError> {
        let bytes_guard = self.mme_delegate_bytes.lock().await;
        Ok(bytes_guard.as_ref().map(|b| String::from_utf8_lossy(b).to_string()))
    }

    /// Seed the MobileMe delegate from persisted plist string. Validates as a
    /// generic `plist::Value` first, then stores opaque bytes. Call sites that
    /// need a typed `MobileMeDelegateResponse` reconstruct it via
    /// `parse_mme_delegate()`.
    pub async fn seed_mme_delegate_json(&self, json: String) -> Result<(), WrappedError> {
        plist::from_bytes::<plist::Value>(json.as_bytes())
            .map_err(|e| WrappedError::GenericError {
                msg: format!("Failed to deserialize MobileMe delegate: {}", e),
            })?;
        *self.mme_delegate_bytes.lock().await = Some(json.into_bytes());
        Ok(())
    }

    /// Proactively refresh the GSA/PET session by re-running login_email_pass
    /// against the stored credentials. Apple's PET lifetime is ~24h; on a
    /// long-running bridge without restarts, a mid-run refresh prevents the
    /// session from aging out and forcing a manual 2FA.
    ///
    /// Lock scope: holds `self.account` across `login_email_pass` (network
    /// I/O). This matches upstream's own `TokenProvider::get_token` pattern
    /// at `icloud-auth/client.rs:338-360` which also holds this lock across
    /// its auto-refresh `login_email_pass` call. Any concurrent `get_dsid` /
    /// `get_adsid` / `get_gsa_token` caller briefly waits (typically 1-3s on
    /// a warm session) once per 12h tick. Not a new contention pattern —
    /// it's inherent to how upstream serializes access to AppleAccount.
    ///
    /// Non-fatal: any failure is logged inside `refresh_pet_with_snapshot`
    /// and swallowed so the Go-side caller treats an Err return as "try
    /// again next tick" rather than a hard failure.
    pub async fn refresh_pet_token(&self) -> Result<(), WrappedError> {
        let mut account = self.account.lock().await;
        let username = account.username.clone().ok_or(WrappedError::GenericError {
            msg: "refresh_pet_token: account has no username".into(),
        })?;
        let hashed_password = account.hashed_password.clone().ok_or(WrappedError::GenericError {
            msg: "refresh_pet_token: account has no hashed_password".into(),
        })?;
        refresh_pet_with_snapshot(&mut account, &username, &hashed_password, "refresh_pet_token").await;
        Ok(())
    }

    /// Announce this endpoint as a new "Apple Device" via GSA `/circle`, exactly
    /// once per persisted login. Matches OB-Android's `restore_account` call to
    /// `update_postdata("Apple Device", None, &["icloud", "imessage", "facetime"])`
    /// guarded by `postdata_done` in `gsa.plist` (ob/rust/src/api/api.rs:675-679).
    ///
    /// Why: at the IDS-register layer the bridge and OB are byte-identical, yet
    /// peer iOS devices only reshape StatusKit keys to OB automatically. The
    /// difference is this GSA side-channel call, which declares a new
    /// Apple-Device-class identity on the account with cdpStatus/circleStatus=true
    /// across iCloud/iMessage/FaceTime. Apple propagates that device-enrollment
    /// via iCloud account state, and peer iOS uses it to decide which endpoints
    /// to proactively share Focus keys with.
    ///
    /// Idempotent on Apple's side: same device-name + same account dedupes
    /// server-side. Non-fatal: a failure here does not break any other bridge
    /// functionality. Previously guarded by a persisted `postdata-done.flag`;
    /// that guard was removed because empirical evidence (zero
    /// "GSA announce" log lines after a restart in which grep for them came
    /// up empty, despite prior success on older code) suggests an earlier
    /// announce can land on Apple without actually propagating the device
    /// state that peer iOS needs for reshare eligibility. Forcing re-announce
    /// every bridge start is the simplest way to guarantee eventual
    /// propagation; the request itself is cheap and de-duped server-side.
    pub async fn announce_apple_device_if_needed(&self) -> Result<(), WrappedError> {
        // update_postdata needs the "com.apple.gs.idms.hb" (happy-birthday)
        // GSA token. On fresh login that's populated as a side-effect of
        // login_email_pass, but the warm-path restore_token_provider only
        // injects the PET. Running a PET refresh first re-invokes
        // login_email_pass internally, which repopulates the full GSA token
        // set including HB — so update_postdata won't fail with
        // HappyBirthdayError on first warm-path invocation.
        {
            let mut account = self.account.lock().await;
            let username = account.username.clone();
            let hashed_password = account.hashed_password.clone();
            if let (Some(u), Some(h)) = (username, hashed_password) {
                info!("GSA announce: priming GSA tokens via PET refresh");
                refresh_pet_with_snapshot(&mut account, &u, &h, "announce_apple_device").await;
            } else {
                warn!("GSA announce: account has no username/password for prime — update_postdata may fail if HB token is missing");
            }
        }

        info!("GSA announce: calling update_postdata(\"Apple Device\", None, [\"icloud\", \"imessage\", \"facetime\"])");
        let mut account = self.account.lock().await;
        match account
            .update_postdata("Apple Device", None, &["icloud", "imessage", "facetime"])
            .await
        {
            Ok(state) => {
                info!("GSA announce: update_postdata OK (login_state={:?})", state);
                Ok(())
            }
            Err(e) => {
                warn!("GSA announce: update_postdata failed: {:?} — will retry on next restart", e);
                Err(WrappedError::GenericError {
                    msg: format!("update_postdata failed: {:?}", e),
                })
            }
        }
    }
}

/// Reshape a stored MobileMe delegate plist value so that deserializing it as
/// upstream's `MobileMeDelegateResponse` puts the *inner* iCloud service dict
/// (the one keyed by `com.apple.Dataclass.KeychainSync`,
/// `com.apple.Dataclass.Files`, etc.) into the `config` field.
///
/// We accept three stored shapes and normalize to a single output:
///   1. **Current (post-a7fab47 double-wrapped)**:
///      `{tokens, "com.apple.mobileme": {tokens, "com.apple.mobileme": {KeychainSync: ...}}}`
///      — produced by our serialize path below after upstream's "fix" made
///      `mobileme.config` hold the entire delegate service_data.
///   2. **Legacy (pre-a7fab47)**:
///      `{tokens, "com.apple.mobileme": {KeychainSync: ...}}`
///      — produced when `config` was deserialized via
///      `#[serde(rename = "com.apple.mobileme")]` and held the inner dict directly.
///   3. **Already-normalized**:
///      `{tokens, config: {KeychainSync: ...}}` — ideal shape going forward.
///
/// Output: `{tokens, config: {KeychainSync: ...}}` every time, which matches
/// the new `#[serde(default)] pub config: Dictionary` field on
/// `MobileMeDelegateResponse` and keeps `KeychainClientState::new`
/// (which does `delegate.config.get("com.apple.Dataclass.KeychainSync")`)
/// working.
fn normalize_mme_delegate_dict(value: plist::Value) -> plist::Value {
    let Some(mut root) = value.into_dictionary() else {
        return plist::Value::Dictionary(plist::Dictionary::new());
    };
    if root.contains_key("config") {
        return plist::Value::Dictionary(root);
    }
    let Some(outer_mm) = root.remove("com.apple.mobileme") else {
        return plist::Value::Dictionary(root);
    };
    let Some(outer_dict) = outer_mm.as_dictionary() else {
        return plist::Value::Dictionary(root);
    };
    // If the outer has a nested `com.apple.mobileme`, that's shape 1 (double-wrap)
    // and the nested dict is the real iCloud config. Otherwise it's shape 2 (legacy)
    // and the outer IS the iCloud config.
    let inner = outer_dict
        .get("com.apple.mobileme")
        .and_then(|v| v.as_dictionary())
        .cloned()
        .unwrap_or_else(|| outer_dict.clone());
    root.insert("config".to_string(), plist::Value::Dictionary(inner));
    plist::Value::Dictionary(root)
}

// Non-uniffi impl block: internal helpers used by other wrapper code that need
// to read state previously available via `TokenProvider::get_xxx()` methods that
// OpenBubbles upstream doesn't expose. These are not exported as FFI symbols.
impl WrappedTokenProvider {
    /// Get the ADSID from the AppleAccount's SPD dictionary.
    pub(crate) async fn get_adsid(&self) -> Result<String, WrappedError> {
        let account = self.account.lock().await;
        let spd = account.spd.as_ref().ok_or(WrappedError::GenericError {
            msg: "AppleAccount has no SPD — not fully logged in".into(),
        })?;
        let adsid = spd.get("adsid")
            .and_then(|v| v.as_string())
            .ok_or(WrappedError::GenericError { msg: "SPD missing adsid".into() })?
            .to_string();
        Ok(adsid)
    }

    /// Parse the stored MobileMe delegate plist bytes into whatever typed form
    /// the caller context expects (usually `rustpush::auth::MobileMeDelegateResponse`).
    /// `T` is inferred from the receiving function's signature at the call site —
    /// we intentionally do NOT name the type here because it's not reachable from
    /// outside the rustpush crate in OpenBubbles upstream (`mod auth` is private).
    ///
    /// Normalizes the dict shape before deserialization so `config` ends up as
    /// the inner iCloud service dict (the one holding `com.apple.Dataclass.KeychainSync`
    /// etc.) regardless of which shape was stored. See `normalize_mme_delegate_dict`
    /// below for the three shapes we accept.
    pub(crate) async fn parse_mme_delegate<T: serde::de::DeserializeOwned>(&self) -> Result<T, WrappedError> {
        let bytes_guard = self.mme_delegate_bytes.lock().await;
        let bytes = bytes_guard.as_ref().ok_or(WrappedError::GenericError {
            msg: "MobileMe delegate not seeded — call seed_mme_delegate_json first".into(),
        })?;
        let value: plist::Value = plist::from_bytes(bytes).map_err(|e| WrappedError::GenericError {
            msg: format!("Failed to parse MobileMe delegate bytes: {}", e),
        })?;
        let normalized = normalize_mme_delegate_dict(value);
        plist::from_value::<T>(&normalized).map_err(|e| WrappedError::GenericError {
            msg: format!("Failed to deserialize MobileMe delegate: {}", e),
        })
    }

    /// Clone the shared AppleAccount handle.
    pub(crate) fn get_account(&self) -> Arc<rustpush::DebugMutex<AppleAccount<BridgeDefaultAnisetteProvider>>> {
        self.account.clone()
    }

    /// Clone the shared OSConfig handle.
    pub(crate) fn get_os_config(&self) -> Arc<dyn OSConfig> {
        self.os_config.clone()
    }
}

// Second uniffi-exported impl block: the iCloud Keychain clique methods that
// were previously in the single uniffi block before I split out the internal
// helpers above. These MUST be exported to Go so the bridge's login flow can
// call them.
#[uniffi::export(async_runtime = "tokio")]
impl WrappedTokenProvider {
    /// List devices that have escrow bottles in the iCloud Keychain trust circle.
    /// Returns device info (name, model, serial, timestamp) for each bottle.
    /// Call this before join_keychain_clique_for_device to let the user choose.
    pub async fn get_escrow_devices(&self) -> Result<Vec<EscrowDeviceInfo>, WrappedError> {
        info!("Fetching escrow devices...");
        let (keychain, _cloudkit) = create_keychain_clients(self).await?;

        let bottles = keychain.get_viable_bottles().await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to get escrow bottles: {}", e) })?;

        if bottles.is_empty() {
            return Err(WrappedError::GenericError {
                msg: "No escrow bottles found. Make sure Messages in iCloud is enabled on your iPhone/Mac.".into()
            });
        }

        let devices: Vec<EscrowDeviceInfo> = bottles.iter().enumerate().map(|(i, (_data, meta))| {
            let (device_name, device_model) = extract_device_info(meta);
            info!("  [{}] name={:?} model={} serial={} timestamp={}", i, device_name, device_model, meta.serial, meta.timestamp);
            EscrowDeviceInfo {
                index: i as u32,
                device_name,
                device_model,
                serial: meta.serial.clone(),
                timestamp: meta.timestamp.clone(),
            }
        }).collect();

        info!("Found {} escrow device(s)", devices.len());
        Ok(devices)
    }

    /// Join the iCloud Keychain trust circle using a device passcode.
    /// Tries all available escrow bottles in order.
    /// Required before CloudKit Messages can decrypt PCS-encrypted records.
    /// The passcode is the 6-digit PIN or password used to unlock an iPhone/Mac.
    /// Returns a description of the escrow bottle used.
    pub async fn join_keychain_clique(&self, passcode: String) -> Result<String, WrappedError> {
        info!("=== Joining iCloud Keychain Trust Circle ===");
        let (keychain, cloudkit) = create_keychain_clients(self).await?;

        info!("Fetching escrow bottles...");
        let bottles = keychain.get_viable_bottles().await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to get escrow bottles: {}", e) })?;

        if bottles.is_empty() {
            return Err(WrappedError::GenericError {
                msg: "No escrow bottles found. Make sure Messages in iCloud is enabled on your iPhone/Mac.".into()
            });
        }

        info!("Found {} escrow bottle(s)", bottles.len());
        for (i, (_data, meta)) in bottles.iter().enumerate() {
            info!("  [{}] serial={} build={} timestamp={}", i, meta.serial, meta.build, meta.timestamp);
        }

        join_keychain_with_bottles(keychain, cloudkit, &bottles, &passcode, None).await
    }

    /// Join the iCloud Keychain trust circle, trying the specified device first.
    /// `device_index` is the index from get_escrow_devices(). If that bottle fails,
    /// falls back to trying other bottles.
    pub async fn join_keychain_clique_for_device(&self, passcode: String, device_index: u32) -> Result<String, WrappedError> {
        info!("=== Joining iCloud Keychain Trust Circle (preferred device {}) ===", device_index);
        let (keychain, cloudkit) = create_keychain_clients(self).await?;

        info!("Fetching escrow bottles...");
        let bottles = keychain.get_viable_bottles().await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to get escrow bottles: {}", e) })?;

        if bottles.is_empty() {
            return Err(WrappedError::GenericError {
                msg: "No escrow bottles found. Make sure Messages in iCloud is enabled on your iPhone/Mac.".into()
            });
        }

        info!("Found {} escrow bottle(s)", bottles.len());
        for (i, (_data, meta)) in bottles.iter().enumerate() {
            let (name, model) = extract_device_info(meta);
            info!("  [{}] name={:?} model={} serial={} build={} timestamp={}", i, name, model, meta.serial, meta.build, meta.timestamp);
        }

        let preferred = if (device_index as usize) < bottles.len() {
            Some(device_index)
        } else {
            warn!("Device index {} out of range (have {} bottles), trying all", device_index, bottles.len());
            None
        };

        join_keychain_with_bottles(keychain, cloudkit, &bottles, &passcode, preferred).await
    }
}

/// Restore a TokenProvider from persisted account credentials.
/// Used on session restore (when we don't go through the login flow).
#[uniffi::export(async_runtime = "tokio")]
pub async fn restore_token_provider(
    config: &WrappedOSConfig,
    connection: &WrappedAPSConnection,
    username: String,
    hashed_password_hex: String,
    pet: String,
    spd_base64: String,
) -> Result<Arc<WrappedTokenProvider>, WrappedError> {
    let os_config = config.config.clone();
    let conn = connection.inner.clone();

    // Create a fresh anisette provider. On Linux the
    // `BridgeAnisetteProvider` owns provisioning and bypasses upstream's
    // broken `ProvisionInput` enum entirely (see src/anisette.rs). On
    // macOS this is upstream's native AOSKit path, unchanged.
    let client_info = os_config.get_gsa_config(&*conn.state.read().await, false);
    let anisette_state_path = PathBuf::from_str("state/anisette").unwrap();
    let anisette = bridge_default_provider(client_info.clone(), anisette_state_path);

    // Create a new AppleAccount and populate it with persisted state
    let mut account = AppleAccount::new_with_anisette(client_info, anisette)
        .map_err(|e| WrappedError::GenericError { msg: format!("Failed to create account: {}", e) })?;

    account.username = Some(username.clone());

    // Restore hashed password
    let hashed_password = decode_hex(&hashed_password_hex)
        .map_err(|e| WrappedError::GenericError { msg: format!("Invalid hashed_password hex: {}", e) })?;
    account.hashed_password = Some(hashed_password.clone());

    // Restore SPD from base64-encoded plist
    let spd_bytes = base64_decode(&spd_base64);
    let spd: plist::Dictionary = plist::from_bytes(&spd_bytes)
        .map_err(|e| WrappedError::GenericError { msg: format!("Invalid SPD plist: {}", e) })?;

    account.spd = Some(spd);

    // Inject the persisted PET into account.tokens with an already-expired
    // timestamp. Byte-for-byte port of the master-branch restore path
    // (master's pkg/rustpushgo/src/lib.rs line 805).
    //
    // Why: upstream's `get_token` short-circuits and returns None when
    // `tokens.is_empty() == false` AND the requested key is absent. So if
    // we leave tokens empty OR populate it without the PET key, the first
    // `get_gsa_token("com.apple.gs.idms.pet")` call either takes a
    // corner-case path or returns None immediately. Neither gives upstream
    // the opportunity to auto-refresh via `login_email_pass`.
    //
    // With an expired PET entry present, `get_token`'s expiration check
    // fires (`data.expiration.elapsed().is_err()` → false → has_valid_token
    // false), it enters the auto-refresh branch, calls `login_email_pass`
    // with the persisted credentials, and — assuming Apple still trusts
    // the device via consistent anisette state — returns LoggedIn with
    // fresh GSA/HB/PET tokens populated from the SPD response. No 2FA.
    //
    // The 0bbb081 commit that removed this trick said "FetchedToken has
    // private fields so we cannot reconstruct tokens from the persisted
    // SPD" — that was accurate against raw OpenBubbles upstream but no
    // longer applies, because the Makefile's `ensure-rustpush-source`
    // target now sed-patches `pub token`/`pub expiration` onto
    // `FetchedToken` alongside the existing `pub mod activation; pub mod
    // ids;` visibility bumps. See Makefile:208-217.
    //
    // This is the PRIMARY fix for the daily-auth-breaks-at-24h regression
    // that appeared after the refactor migration to raw OpenBubbles and
    // was never fully restored by the subsequent periodic-refresh commits
    // (66d6402, bdd4ebe).
    account.tokens.insert(
        "com.apple.gs.idms.pet".to_string(),
        icloud_auth::FetchedToken {
            token: pet,
            expiration: std::time::UNIX_EPOCH,
        },
    );

    let account = Arc::new(rustpush::DebugMutex::new(account));
    let token_provider = TokenProvider::new(account.clone(), os_config.clone());

    info!("Restored TokenProvider from persisted credentials");

    // Restore path does not have a MobileMe delegate — callers must
    // seed_mme_delegate_json() from persisted state before using keychain/
    // contacts features.
    Ok(Arc::new(WrappedTokenProvider {
        inner: token_provider,
        account,
        os_config,
        mme_delegate_bytes: tokio::sync::Mutex::new(None),
        keychain_clients_cache: tokio::sync::Mutex::new(None),
    }))
}

// ============================================================================
// Message wrapper types (flat structs for uniffi)
// ============================================================================

#[derive(uniffi::Record, Clone, Default)]
pub struct WrappedMessage {
    pub uuid: String,
    pub sender: Option<String>,
    pub text: Option<String>,
    pub subject: Option<String>,
    pub participants: Vec<String>,
    pub group_name: Option<String>,
    pub timestamp_ms: u64,
    pub is_sms: bool,

    // Tapback
    pub is_tapback: bool,
    pub tapback_type: Option<u32>,
    pub tapback_target_uuid: Option<String>,
    pub tapback_target_part: Option<u64>,
    pub tapback_emoji: Option<String>,
    pub tapback_remove: bool,

    // Edit
    pub is_edit: bool,
    pub edit_target_uuid: Option<String>,
    pub edit_part: Option<u64>,
    pub edit_new_text: Option<String>,

    // Unsend
    pub is_unsend: bool,
    pub unsend_target_uuid: Option<String>,
    pub unsend_edit_part: Option<u64>,

    // Rename
    pub is_rename: bool,
    pub new_chat_name: Option<String>,

    // Participant change
    pub is_participant_change: bool,
    pub new_participants: Vec<String>,

    // Attachments
    pub attachments: Vec<WrappedAttachment>,

    // Reply
    pub reply_guid: Option<String>,
    pub reply_part: Option<String>,

    // Typing
    pub is_typing: bool,
    pub typing_app_bundle_id: Option<String>,
    pub typing_app_icon: Option<Vec<u8>>,

    // Read receipt
    pub is_read_receipt: bool,

    // Delivered
    pub is_delivered: bool,

    // Error
    pub is_error: bool,
    pub error_for_uuid: Option<String>,
    pub error_status: Option<u64>,
    pub error_status_str: Option<String>,

    // Peer cache invalidate
    pub is_peer_cache_invalidate: bool,

    // Send delivered flag
    pub send_delivered: bool,

    // Group chat UUID (persistent identifier for the group conversation)
    pub sender_guid: Option<String>,

    // Delete (MoveToRecycleBin / PermanentDelete) and Recover
    pub is_move_to_recycle_bin: bool,
    pub is_permanent_delete: bool,
    pub is_recover_chat: bool,
    pub delete_chat_participants: Vec<String>,
    pub delete_chat_group_id: Option<String>,
    pub delete_chat_guid: Option<String>,
    pub delete_message_uuids: Vec<String>,

    // Stored message detection: true if this message was queued by APNs
    // while the client was offline and delivered on reconnect. Detected by
    // checking if the message arrived (drain-time) within 10 seconds of an
    // APNs reconnect (generated_signal). Drain-time is used instead of
    // process-time so MMCS downloads and retries don't push messages past
    // the window.
    pub is_stored_message: bool,

    // Group photo (icon) change. True when someone sets or clears the
    // group chat photo from their iMessage client.
    // group_photo_cleared: true = photo was removed ("ngp"), false = new photo set.
    // When is_icon_change=true and group_photo_cleared=false, icon_change_photo_data
    // contains the MMCS-downloaded photo bytes (None if download failed).
    pub is_icon_change: bool,
    pub group_photo_cleared: bool,
    // Pre-downloaded photo bytes for IconChange messages.
    // Some(bytes) only when is_icon_change=true, group_photo_cleared=false, and download succeeded.
    pub icon_change_photo_data: Option<Vec<u8>>,

    // HTML-formatted text body. Some(...) when the message contains any non-plain
    // formatting (bold, italic, underline, strikethrough, text effects, mentions).
    pub html: Option<String>,

    // Voice message flag. True for voice memo / audio message attachments.
    pub is_voice: bool,

    // Screen/bubble effect identifier (e.g., "com.apple.MobileSMS.expressivesend.impact").
    pub effect: Option<String>,

    // Scheduled send timestamp in milliseconds. Some(ms) when message is scheduled.
    pub scheduled_ms: Option<u64>,

    // SMS activation: true = activation request, false = deactivation.
    pub is_sms_activation: Option<bool>,

    // SMS confirmed sent status.
    pub is_sms_confirm_sent: Option<bool>,

    // Mark unread flag.
    pub is_mark_unread: bool,

    // Message-read-on-device acknowledgment.
    pub is_message_read_on_device: bool,

    // Unschedule marker for scheduled-message cancellation updates.
    pub is_unschedule: bool,

    // Update extension (sticker/balloon metadata update) targeting a message UUID.
    pub is_update_extension: bool,
    pub update_extension_for_uuid: Option<String>,

    // Profile sharing state sync update.
    pub is_update_profile_sharing: bool,
    pub update_profile_sharing_dismissed: Vec<String>,
    pub update_profile_sharing_all: Vec<String>,
    pub update_profile_sharing_version: Option<u64>,

    // Profile update (share profile card and/or sharing preference).
    pub is_update_profile: bool,
    pub update_profile_share_contacts: Option<bool>,

    // "Notify anyway" control message.
    pub is_notify_anyways: bool,

    // Transcript background (conversation wallpaper) update.
    pub is_set_transcript_background: bool,
    pub transcript_background_remove: Option<bool>,
    pub transcript_background_chat_id: Option<String>,
    pub transcript_background_object_id: Option<String>,
    pub transcript_background_url: Option<String>,
    pub transcript_background_file_size: Option<u64>,

    // Sticker data for sticker tapback reactions (tapback_type=7).
    pub sticker_data: Option<Vec<u8>>,
    pub sticker_mime: Option<String>,

    // Profile sharing: set when a contact shares their name/photo with us.
    // record_key + decryption_key + has_poster identify the CloudKit record;
    // display_name/first_name/last_name/avatar are populated inline by the
    // Rust receive loop via ProfilesClient::get_record so the Go side just
    // reads bytes (mirrors the IconChange MMCS download pattern).
    pub is_share_profile: bool,
    pub share_profile_record_key: Option<String>,
    pub share_profile_decryption_key: Option<Vec<u8>>,
    pub share_profile_has_poster: bool,
    pub share_profile_display_name: Option<String>,
    pub share_profile_first_name: Option<String>,
    pub share_profile_last_name: Option<String>,
    pub share_profile_avatar: Option<Vec<u8>>,
}

#[derive(uniffi::Record, Clone)]
pub struct WrappedAttachment {
    pub mime_type: String,
    pub filename: String,
    pub uti_type: String,
    pub size: u64,
    pub is_inline: bool,
    pub inline_data: Option<Vec<u8>>,
    /// True for Live Photo attachments (Apple's "iris" flag).
    pub iris: bool,
    /// Serialized MMCS descriptor JSON for non-inline attachments; None for
    /// inline attachments and rich-link sidebands. The Go-side
    /// AttachmentRetrier persists this alongside a pending row so it can
    /// re-invoke the download later via retry_mmcs_from_descriptor without
    /// needing the original MessageInst to still be in memory. See
    /// MmcsDescriptor below.
    pub mmcs_descriptor_json: Option<String>,
}

/// Serializable handle for a rustpush MMCSFile. Persisted by Go so the
/// background AttachmentRetrier can reconstruct the handle and call
/// download_one_mmcs_attachment against a freshly healthy APNs connection.
/// Binary fields are base64-encoded so the JSON round-trips through SQLite
/// without escaping headaches.
#[derive(serde::Serialize, serde::Deserialize, Clone, Debug)]
struct MmcsDescriptor {
    url: String,
    object: String,
    signature_b64: String,
    key_b64: String,
    size: usize,
}

impl MmcsDescriptor {
    fn from_file(mmcs: &rustpush::MMCSFile) -> Self {
        use base64::{engine::general_purpose::STANDARD, Engine as _};
        Self {
            url: mmcs.url.clone(),
            object: mmcs.object.clone(),
            signature_b64: STANDARD.encode(&mmcs.signature),
            key_b64: STANDARD.encode(&mmcs.key),
            size: mmcs.size,
        }
    }

    fn to_file(&self) -> Result<rustpush::MMCSFile, String> {
        use base64::{engine::general_purpose::STANDARD, Engine as _};
        let signature = STANDARD
            .decode(&self.signature_b64)
            .map_err(|e| format!("decode signature_b64: {e}"))?;
        let key = STANDARD
            .decode(&self.key_b64)
            .map_err(|e| format!("decode key_b64: {e}"))?;
        Ok(rustpush::MMCSFile {
            url: self.url.clone(),
            object: self.object.clone(),
            signature,
            key,
            size: self.size,
        })
    }
}

/// Result of fetching a shared iMessage profile (Name & Photo Sharing).
#[derive(uniffi::Record, Clone)]
pub struct WrappedProfileRecord {
    pub display_name: String,
    pub first_name: String,
    pub last_name: String,
    pub avatar: Option<Vec<u8>>,
}

#[derive(uniffi::Record, Clone)]
pub struct WrappedStickerExtension {
    pub msg_width: f64,
    pub rotation: f64,
    pub sai: u64,
    pub scale: f64,
    pub sli: u64,
    pub normalized_x: f64,
    pub normalized_y: f64,
    pub version: u64,
    pub hash: String,
    pub safi: u64,
    pub effect_type: i64,
    pub sticker_id: String,
}

#[derive(uniffi::Record, Clone)]
pub struct WrappedShareProfileData {
    pub cloud_kit_record_key: String,
    pub cloud_kit_decryption_record_key: Vec<u8>,
    pub low_res_wallpaper_tag: Option<Vec<u8>>,
    pub wallpaper_tag: Option<Vec<u8>>,
    pub message_tag: Option<Vec<u8>>,
}

#[derive(uniffi::Record, Clone)]
pub struct WrappedStatusKitInviteHandle {
    pub handle: String,
    pub allowed_modes: Vec<String>,
}

#[derive(uniffi::Record, Clone)]
pub struct WrappedPasswordEntryRef {
    pub id: String,
    pub group: Option<String>,
}

#[derive(uniffi::Record, Clone)]
pub struct WrappedPasswordSiteCounts {
    pub website_meta_count: u64,
    pub password_count: u64,
    pub password_meta_count: u64,
    pub passkey_count: u64,
}

#[derive(uniffi::Record, Clone)]
pub struct WrappedLetMeInRequest {
    pub delegation_uuid: String,
    pub pseud: String,
    pub requestor: String,
    pub nickname: Option<String>,
    pub usage: Option<String>,
}

#[derive(uniffi::Record, Clone)]
pub struct WrappedConversation {
    pub participants: Vec<String>,
    pub group_name: Option<String>,
    pub sender_guid: Option<String>,
    pub is_sms: bool,
}

impl From<&ConversationData> for WrappedConversation {
    fn from(c: &ConversationData) -> Self {
        Self {
            participants: c.participants.clone(),
            group_name: c.cv_name.clone(),
            sender_guid: c.sender_guid.clone(),
            is_sms: false,
        }
    }
}

impl From<&WrappedConversation> for ConversationData {
    fn from(c: &WrappedConversation) -> Self {
        ConversationData {
            participants: c.participants.clone(),
            cv_name: c.group_name.clone(),
            sender_guid: c.sender_guid.clone(),
            after_guid: None,
        }
    }
}

#[derive(uniffi::Record, Clone)]
pub struct WrappedCloudSyncChat {
    pub record_name: String,
    pub cloud_chat_id: String,
    pub group_id: String,
    /// CloudKit chat style: 43 = group, 45 = DM
    pub style: i64,
    pub service: String,
    pub display_name: Option<String>,
    pub participants: Vec<String>,
    pub deleted: bool,
    pub updated_timestamp_ms: u64,
    /// CloudKit group photo GUID ("gpid" field). Non-null means a custom photo is set.
    pub group_photo_guid: Option<String>,
    /// CloudKit `filt` field: 0 = normal, 1 = filtered (spam/junk/unknown sender)
    pub is_filtered: i64,
}

fn recoverable_record_field_names(fields: &[rustpush::cloudkit_proto::record::Field]) -> Vec<String> {
    fields
        .iter()
        .filter_map(|field| field.identifier.as_ref()?.name.clone())
        .collect()
}

fn record_looks_chat_like(fields: &[rustpush::cloudkit_proto::record::Field]) -> bool {
    let names = recoverable_record_field_names(fields);
    names.iter().any(|name| matches!(name.as_str(), "stl" | "cid" | "gid" | "ptcpts" | "name" | "lah"))
}

fn wrap_recoverable_chat(record_name: String, mut chat: rustpush::cloud_messages::CloudChat) -> Option<WrappedCloudSyncChat> {
    if chat.participants.is_empty() && chat.style == 45 && !chat.last_addressed_handle.is_empty() {
        chat.participants.push(rustpush::cloud_messages::CloudParticipant {
            uri: chat.last_addressed_handle.clone(),
        });
    }

    let has_identity = !chat.chat_identifier.is_empty()
        || !chat.group_id.is_empty()
        || !chat.participants.is_empty()
        || chat.display_name.as_ref().is_some_and(|name| !name.is_empty());
    if !has_identity || !matches!(chat.style, 43 | 45) {
        return None;
    }

    let cloud_chat_id = if chat.chat_identifier.is_empty() {
        record_name.clone()
    } else {
        chat.chat_identifier.clone()
    };
    Some(WrappedCloudSyncChat {
        record_name,
        cloud_chat_id,
        group_id: chat.group_id,
        style: chat.style,
        service: chat.service_name,
        display_name: chat.display_name,
        participants: chat.participants.into_iter().map(|p| p.uri).collect(),
        deleted: false,
        updated_timestamp_ms: 0,
        group_photo_guid: chat.group_photo_guid,
        is_filtered: chat.is_filtered,
    })
}

#[derive(serde::Serialize)]
struct RecoverableMessageMetadata {
    v: u8,
    record_name: String,
    cloud_chat_id: String,
    sender: String,
    is_from_me: bool,
    service: String,
    timestamp_ms: i64,
}

fn encode_recoverable_message_entry(
    guid: &str,
    record_name: &str,
    msg: &rustpush::cloud_messages::CloudMessage,
) -> String {
    if guid.is_empty() {
        return String::new();
    }

    let metadata = RecoverableMessageMetadata {
        v: 1,
        record_name: record_name.to_string(),
        cloud_chat_id: msg.chat_id.clone(),
        sender: msg.sender.clone(),
        is_from_me: msg.flags.contains(rustpush::cloud_messages::MessageFlags::IS_FROM_ME),
        service: msg.service.clone(),
        timestamp_ms: apple_timestamp_ns_to_unix_ms(msg.time),
    };
    match serde_json::to_vec(&metadata) {
        Ok(payload) => format!("{}|{}", guid, BASE64_STANDARD.encode(payload)),
        Err(err) => {
            debug!("list_recoverable_message_guids: failed to encode metadata for {}: {}", guid, err);
            guid.to_string()
        }
    }
}

#[derive(uniffi::Record, Clone)]
pub struct WrappedCloudSyncMessage {
    pub record_name: String,
    pub guid: String,
    pub cloud_chat_id: String,
    pub sender: String,
    pub is_from_me: bool,
    pub text: Option<String>,
    pub subject: Option<String>,
    pub service: String,
    pub timestamp_ms: i64,
    pub deleted: bool,

    // Tapback/reaction fields (from msg_proto.associatedMessageType/Guid)
    pub tapback_type: Option<u32>,
    pub tapback_target_guid: Option<String>,
    pub tapback_emoji: Option<String>,

    // Attachment GUIDs extracted from messageSummaryInfo / attributedBody.
    // These are matched against the attachment zone to download files.
    pub attachment_guids: Vec<String>,

    // When the recipient read this message (Apple epoch ns → Unix ms).
    // Only meaningful for is_from_me messages. 0 means unread.
    pub date_read_ms: i64,

    // CloudKit msgType field. 0 = regular user message, non-zero = system/service
    // record (group naming, participant changes, etc.). Used by Go to filter
    // non-user-content that slips past IS_SYSTEM/IS_SERVICE_MESSAGE flags.
    pub msg_type: i64,

    // Whether the CloudKit record has an attributedBody (rich text payload).
    // Regular user messages always have attributedBody; system/service messages
    // (group renames, participant changes) never do. Used by Go to filter
    // system messages that slip past flag-based filters.
    pub has_body: bool,
}

/// Metadata for an attachment referenced by a CloudKit message.
/// The actual file data must be downloaded separately via cloud_download_attachment.
#[derive(uniffi::Record, Clone)]
pub struct WrappedCloudAttachmentInfo {
    /// Attachment GUID (from AttachmentMeta.guid / attributedBody __kIMFileTransferGUID)
    pub guid: String,
    /// MIME type (from AttachmentMeta.mime_type)
    pub mime_type: Option<String>,
    /// UTI type (from AttachmentMeta.uti)
    pub uti_type: Option<String>,
    /// Filename (from AttachmentMeta.transfer_name)
    pub filename: Option<String>,
    /// File size in bytes (from AttachmentMeta.total_bytes)
    pub file_size: i64,
    /// CloudKit record name in attachmentManateeZone (needed for download)
    pub record_name: String,
    /// Whether this attachment is hidden (companion transfer, e.g. Live Photo MOV).
    /// When true, this is the video component of a Live Photo — not shown standalone.
    pub hide_attachment: bool,
    /// Whether this attachment record has a Live Photo video in its `avid` asset field.
    /// When true, use cloud_download_attachment_avid to get the MOV instead of the HEIC.
    pub has_avid: bool,
    /// PCS-decrypted `lqa.protection_info` bytes — for Ford-encrypted videos,
    /// this is the raw 32-byte Ford key used to decrypt per-chunk keys from
    /// the MMCS Ford metadata blob. None for attachments without Ford
    /// encryption (images use V1 per-chunk keys in the MMCS chunk metadata).
    ///
    /// Exposed so the Go-side Ford key cache can pre-populate during sync
    /// and fall back to cached keys on MMCS cross-batch dedup. See
    /// `pkg/connector/ford_cache.go`.
    pub ford_key: Option<Vec<u8>>,
    /// PCS-decrypted `avid.protection_info` bytes — the Live Photo MOV
    /// companion's Ford key. Same semantics as `ford_key`.
    pub avid_ford_key: Option<Vec<u8>>,
}

#[derive(uniffi::Record, Clone)]
pub struct WrappedCloudSyncChatsPage {
    pub continuation_token: Option<String>,
    pub status: i32,
    pub done: bool,
    pub chats: Vec<WrappedCloudSyncChat>,
}

#[derive(uniffi::Record, Clone)]
pub struct WrappedCloudSyncMessagesPage {
    pub continuation_token: Option<String>,
    pub status: i32,
    pub done: bool,
    pub messages: Vec<WrappedCloudSyncMessage>,
}

#[derive(uniffi::Record, Clone)]
pub struct WrappedCloudSyncAttachmentsPage {
    pub continuation_token: Option<String>,
    pub status: i32,
    pub done: bool,
    pub attachments: Vec<WrappedCloudAttachmentInfo>,
}

/// A clonable writer backed by a shared Vec<u8>.
/// Used to recover written bytes after passing ownership to a consuming API.
#[derive(Clone)]
struct SharedWriter {
    inner: Arc<std::sync::Mutex<Vec<u8>>>,
}

impl SharedWriter {
    fn new() -> Self {
        Self { inner: Arc::new(std::sync::Mutex::new(Vec::new())) }
    }

    fn into_bytes(self) -> Vec<u8> {
        match Arc::try_unwrap(self.inner) {
            Ok(mutex) => mutex.into_inner().unwrap(),
            Err(arc) => arc.lock().unwrap().clone(),
        }
    }
}

impl std::io::Write for SharedWriter {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        self.inner.lock().unwrap().write(buf)
    }

    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

/// Extract attachment GUIDs from a CloudKit message's attributedBody.
/// The attributedBody is an NSAttributedString containing ranges with
/// __kIMFileTransferGUIDAttributeName → attachment GUID.
fn extract_attachment_guids_from_attributed_body(data: &[u8]) -> Vec<String> {
    use rustpush::{coder_decode_flattened, NSAttributedString, StCollapsedValue};

    let decoded = match std::panic::catch_unwind(|| {
        let flat = coder_decode_flattened(data);
        if flat.is_empty() {
            return None;
        }
        Some(NSAttributedString::decode(&flat[0]))
    }) {
        Ok(Some(attr_str)) => attr_str,
        _ => return vec![],
    };

    let mut guids = Vec::new();
    for (_len, dict) in &decoded.ranges {
        if let Some(StCollapsedValue::Object { fields, .. }) = dict.0.get("__kIMFileTransferGUIDAttributeName") {
            if let Some(first) = fields.first().and_then(|f| f.first()) {
                if let StCollapsedValue::String(s) = first {
                    guids.push(s.clone());
                }
            }
        } else if let Some(StCollapsedValue::String(s)) = dict.0.get("__kIMFileTransferGUIDAttributeName") {
            guids.push(s.clone());
        }
    }
    guids
}

/// Extract attachment GUIDs from a CloudKit message's messageSummaryInfo.
/// messageSummaryInfo is a binary plist containing an "ams" (attachment metadata
/// summary) array. Each entry is a dict with "g" = GUID. This captures GUIDs
/// for companion transfers (e.g. Live Photo MOV components) that are NOT
/// referenced in attributedBody's __kIMFileTransferGUIDAttributeName.
fn extract_attachment_guids_from_summary_info(data: &[u8]) -> Vec<String> {
    let value: plist::Value = match plist::from_bytes(data) {
        Ok(v) => v,
        Err(e) => {
            warn!("messageSummaryInfo: plist parse failed ({} bytes): {}", data.len(), e);
            return vec![];
        }
    };
    let dict = match value.as_dictionary() {
        Some(d) => d,
        None => return vec![],
    };
    let mut guids = Vec::new();
    // "ams" = attachment metadata summary — array of per-attachment dicts
    if let Some(plist::Value::Array(ams)) = dict.get("ams") {
        for entry in ams {
            if let Some(entry_dict) = entry.as_dictionary() {
                // "g" = attachment GUID (file transfer GUID)
                if let Some(plist::Value::String(guid)) = entry_dict.get("g") {
                    if !guid.is_empty() {
                        guids.push(guid.clone());
                    }
                }
            }
        }
    }
    guids
}

fn apple_timestamp_ns_to_unix_ms(timestamp_ns: i64) -> i64 {
    const APPLE_EPOCH_UNIX_MS: i64 = 978_307_200_000;
    APPLE_EPOCH_UNIX_MS.saturating_add(timestamp_ns / 1_000_000)
}

fn decode_continuation_token(token_b64: Option<String>) -> Result<Option<Vec<u8>>, WrappedError> {
    match token_b64 {
        Some(token) if !token.is_empty() => BASE64_STANDARD
            .decode(token)
            .map(Some)
            .map_err(|e| WrappedError::GenericError {
                msg: format!("Invalid continuation token: {}", e),
            }),
        _ => Ok(None),
    }
}

fn encode_continuation_token(token: Vec<u8>) -> Option<String> {
    if token.is_empty() {
        None
    } else {
        Some(BASE64_STANDARD.encode(token))
    }
}

fn convert_reaction(reaction: &Reaction, enable: bool) -> (Option<u32>, Option<String>, bool) {
    let tapback_type = match reaction {
        Reaction::Heart => Some(0),
        Reaction::Like => Some(1),
        Reaction::Dislike => Some(2),
        Reaction::Laugh => Some(3),
        Reaction::Emphasize => Some(4),
        Reaction::Question => Some(5),
        Reaction::Emoji(_) => Some(6),
        Reaction::Sticker { .. } => Some(7),
    };
    let emoji = match reaction {
        Reaction::Emoji(e) => Some(e.clone()),
        _ => None,
    };
    (tapback_type, emoji, !enable)
}

fn populate_delete_target(w: &mut WrappedMessage, target: &DeleteTarget) {
    match target {
        DeleteTarget::Chat(chat) => {
            w.delete_chat_participants = chat.participants.clone();
            w.delete_chat_group_id = if chat.group_id.is_empty() {
                None
            } else {
                Some(chat.group_id.clone())
            };
            w.delete_chat_guid = if chat.guid.is_empty() {
                None
            } else {
                Some(chat.guid.clone())
            };
        }
        DeleteTarget::Messages(uuids) => {
            w.delete_message_uuids = uuids.clone();
        }
    }
}

/// Convert message parts into HTML. Returns Some(html) only if there is any
/// non-plain formatting (bold/italic/underline/strikethrough, text effects,
/// or mentions). Returns None for plain-text-only messages so the Go side can
/// skip HTML encoding.
fn parts_to_html(parts: &MessageParts) -> Option<String> {
    let has_formatting = parts.0.iter().any(|p| match &p.part {
        MessagePart::Text(_, fmt) => !matches!(fmt, TextFormat::Flags(TextFlags { bold: false, italic: false, underline: false, strikethrough: false })),
        MessagePart::Mention(_, _) => true,
        _ => false,
    });
    if !has_formatting {
        return None;
    }

    let mut html = String::new();
    for indexed_part in &parts.0 {
        match &indexed_part.part {
            MessagePart::Text(text, format) => {
                let escaped = html_escape(text);
                match format {
                    TextFormat::Flags(flags) => {
                        let mut open = String::new();
                        let mut close = String::new();
                        if flags.bold { open.push_str("<strong>"); close.insert_str(0, "</strong>"); }
                        if flags.italic { open.push_str("<em>"); close.insert_str(0, "</em>"); }
                        if flags.underline { open.push_str("<u>"); close.insert_str(0, "</u>"); }
                        if flags.strikethrough { open.push_str("<del>"); close.insert_str(0, "</del>"); }
                        html.push_str(&open);
                        html.push_str(&escaped);
                        html.push_str(&close);
                    }
                    TextFormat::Effect(effect) => {
                        let effect_name = match effect {
                            TextEffect::Big => "big",
                            TextEffect::Small => "small",
                            TextEffect::Shake => "shake",
                            TextEffect::Nod => "nod",
                            TextEffect::Explode => "explode",
                            TextEffect::Ripple => "ripple",
                            TextEffect::Bloom => "bloom",
                            TextEffect::Jitter => "jitter",
                        };
                        html.push_str(&format!(
                            "<span data-mx-imessage-effect=\"{}\">",
                            effect_name
                        ));
                        html.push_str(&escaped);
                        html.push_str("</span>");
                    }
                }
            }
            MessagePart::Mention(uri, display) => {
                let escaped_display = html_escape(display);
                let escaped_uri = html_escape(uri);
                html.push_str(&format!(
                    "<a href=\"{}\">@{}</a>",
                    escaped_uri, escaped_display
                ));
            }
            _ => {}
        }
    }

    if html.is_empty() {
        None
    } else {
        Some(html)
    }
}

/// Minimal HTML escaping for user-provided text.
fn html_escape(s: &str) -> String {
    s.replace('&', "&amp;")
     .replace('<', "&lt;")
     .replace('>', "&gt;")
     .replace('"', "&quot;")
}

/// Parse Matrix HTML into iMessage MessageParts with formatting.
/// Returns None if no formatting tags are found (plain text fallback).
fn parse_html_to_parts(html: &str, plain_text: &str) -> Option<MessageParts> {
    if !html.contains('<') {
        return None;
    }

    let mut parts: Vec<IndexedMessagePart> = Vec::new();
    let mut pos = 0;
    let bytes = html.as_bytes();
    let len = bytes.len();
    let mut bold = false;
    let mut italic = false;
    let mut underline = false;
    let mut strikethrough = false;

    while pos < len {
        if bytes[pos] == b'<' {
            let tag_end = match html[pos..].find('>') {
                Some(e) => pos + e + 1,
                None => break,
            };
            let tag_content = &html[pos + 1..tag_end - 1].trim().to_lowercase();
            match tag_content.as_str() {
                "strong" | "b" => bold = true,
                "/strong" | "/b" => bold = false,
                "em" | "i" => italic = true,
                "/em" | "/i" => italic = false,
                "u" => underline = true,
                "/u" => underline = false,
                "del" | "s" | "strike" => strikethrough = true,
                "/del" | "/s" | "/strike" => strikethrough = false,
                "br" | "br/" | "br /" => {
                    let flags = TextFlags { bold, italic, underline, strikethrough };
                    parts.push(IndexedMessagePart {
                        part: MessagePart::Text("\n".to_string(), TextFormat::Flags(flags)),
                        idx: None,
                        ext: None,
                    });
                }
                _ => {
                    // Skip unknown tags (p, div, span without effect, etc.)
                }
            }
            pos = tag_end;
        } else {
            let next_tag = html[pos..].find('<').map(|i| pos + i).unwrap_or(len);
            let chunk = &html[pos..next_tag];
            let decoded = chunk
                .replace("&amp;", "&")
                .replace("&lt;", "<")
                .replace("&gt;", ">")
                .replace("&quot;", "\"");
            if !decoded.is_empty() {
                let flags = TextFlags { bold, italic, underline, strikethrough };
                parts.push(IndexedMessagePart {
                    part: MessagePart::Text(decoded, TextFormat::Flags(flags)),
                    idx: None,
                    ext: None,
                });
            }
            pos = next_tag;
        }
    }

    if parts.is_empty() {
        return None;
    }

    let reconstructed: String = parts.iter().map(|p| match &p.part {
        MessagePart::Text(t, _) => t.as_str(),
        _ => "",
    }).collect();
    if reconstructed == plain_text && !parts.iter().any(|p| matches!(&p.part, MessagePart::Text(_, TextFormat::Flags(f)) if f.bold || f.italic || f.underline || f.strikethrough)) {
        return None;
    }

    Some(MessageParts(parts))
}

/// Lift a ShareProfileMessage onto a WrappedMessage's share-profile keys.
/// Used by both the standalone Message::ShareProfile / UpdateProfile arms
/// and the embedded_profile field on NormalMessage / ReactMessage —
/// iOS partners send profile keys piggybacked on regular text messages
/// when Name & Photo Sharing is enabled, so the standalone control message
/// alone is not sufficient to discover all shared profiles.
///
/// `kind` describes which message variant produced the profile, and is
/// logged so the embedded path is observable. The standalone-variant
/// "received Message::ShareProfile" log already covers those cases; this
/// log catches NormalMessage/ReactMessage embedded arrivals which would
/// otherwise be silent in the receive-loop log stream.
fn populate_share_profile_keys(
    w: &mut WrappedMessage,
    profile: &rustpush::ShareProfileMessage,
    kind: &'static str,
) {
    w.is_share_profile = true;
    w.share_profile_record_key = Some(profile.cloud_kit_record_key.clone());
    w.share_profile_decryption_key = Some(profile.cloud_kit_decryption_record_key.clone());
    w.share_profile_has_poster = profile.poster.is_some();
    info!(
        "populated share_profile_keys from {} (record_key_len={}, has_poster={})",
        kind,
        profile.cloud_kit_record_key.len(),
        profile.poster.is_some()
    );
}

const FACETIME_RING_MARKER: &str = "[[FACETIME_RING]]";
const FACETIME_MISSED_MARKER: &str = "[[FACETIME_MISSED]]";
const FACETIME_ANSWERED_ELSEWHERE_MARKER: &str = "[[FACETIME_ANSWERED_ELSEWHERE]]";

// Tracks FaceTime sessions that should ring a set of targets as soon as
// somebody *other than the caller* joins. Populated by `!im facetime` in a
// portal room: the command creates a session for the user + contact, returns
// a join link, and queues a pending ring here. When the caller later taps
// the link and a JoinEvent arrives on the APNs stream, the receive loop
// fires ft.ring() against the queued targets. We filter out the caller's
// own handle so the implicit self-join emitted during create_session does
// NOT trigger the ring prematurely — the contact's phone must only ring
// after the caller has actually joined.
struct PendingFTRing {
    caller_handle: String,
    targets: Vec<String>,
    expires_at: std::time::Instant,
}

static PENDING_FT_RINGS: std::sync::OnceLock<tokio::sync::Mutex<HashMap<String, PendingFTRing>>> =
    std::sync::OnceLock::new();

fn pending_ft_rings() -> &'static tokio::sync::Mutex<HashMap<String, PendingFTRing>> {
    PENDING_FT_RINGS.get_or_init(|| tokio::sync::Mutex::new(HashMap::new()))
}

async fn maybe_fire_pending_ring(ft: &rustpush::facetime::FTClient, guid: &str, joiner_handle: &str) {
    let targets = {
        let mut map = pending_ft_rings().lock().await;
        let Some(entry) = map.get(guid) else {
            return;
        };
        if entry.expires_at <= std::time::Instant::now() {
            map.remove(guid);
            return;
        }
        if joiner_handle == entry.caller_handle {
            // Creator's own implicit join from create_session — keep the
            // entry pending and wait for a real remote/web joiner.
            return;
        }
        let targets = entry.targets.clone();
        map.remove(guid);
        targets
    };
    let session = {
        let state = ft.state.read().await;
        match state.sessions.get(guid).cloned() {
            Some(s) => s,
            None => {
                warn!("pending ring: session {} not found", guid);
                return;
            }
        }
    };
    let rang = match ft.ring(&session, &targets, false).await {
        Ok(_) => {
            info!(
                "pending ring: rang {} target(s) in session {} (triggered by join from {})",
                targets.len(),
                guid,
                joiner_handle
            );
            // Flip is_ringing_inaccurate=true now that the Invitation is on
            // the wire. create_session_no_ring starts it false to suppress
            // prop_up_conv's RespondedElsewhere diversion; we need it true
            // here so upstream's missed-call detection (facetime.rs:1411)
            // trips if the callee declines / times out — otherwise a
            // no-answer call silently drops instead of surfacing as Missed.
            let mut state = ft.state.write().await;
            if let Some(session) = state.sessions.get_mut(guid) {
                session.is_ringing_inaccurate = true;
            }
            true
        }
        Err(e) => {
            warn!("pending ring: ft.ring failed for session {}: {:?}", guid, e);
            false
        }
    };
    if rang {
        suppress_own_device_ring(ft, guid).await;
    }
}

// Send a RespondedElsewhere FaceTime message scoped to the caller's own
// handle, to silence Apple's server-side Continuity fanout on outgoing calls.
//
// Problem: when the bridge initiates an outgoing FT call (via
// create_session_no_ring + pending-ring fire-on-join, or via respond_letmein
// for web-link taps), Apple's Continuity layer sees "handle X initiated a
// call via web/bridge" and rings handle X's OTHER registered devices (Mac,
// iPad) to offer "join this call on your native device." Native iPhone FT
// doesn't trip this because local CallKit registers the initiating device
// as an active-call device; the bridge can't do that.
//
// Fix: mirror upstream's pattern at facetime.rs:708-725, where
// `prop_up_conv(ring=false)` sends RespondedElsewhere to own-devices when
// the user picks up an incoming call. We emit the same wire after our
// outgoing ring fires so the caller's MacBook/iPad stop ringing. The push
// is scoped to the caller's own handle only — the target (callee) receives
// no RespondedElsewhere, so their ring is unaffected.
//
// Reuses only public upstream APIs (IdentityManager::cache_keys /
// send_message, KeyCache::get_participants_targets, ConversationMessage
// proto). No upstream patching.
async fn suppress_own_device_ring(ft: &rustpush::facetime::FTClient, guid: &str) {
    use prost::Message as _;
    use rustpush::facetime::facetimep::{ConversationMessage, ConversationMessageType};
    use rustpush::ids::identity_manager::{IDSSendMessage, Raw};
    use rustpush::ids::user::QueryOptions;

    let handle = {
        let state = ft.state.read().await;
        let Some(session) = state.sessions.get(guid) else {
            warn!("suppress_own_device_ring: session {} not found", guid);
            return;
        };
        match session.my_handles.first().cloned() {
            Some(h) => h,
            None => {
                warn!("suppress_own_device_ring: session {} has no my_handles", guid);
                return;
            }
        }
    };

    let mut message = ConversationMessage::default();
    message.set_type(ConversationMessageType::RespondedElsewhere);
    message.conversation_group_uuid_string = guid.to_string();
    message.disconnected_reason = 4;

    let relevant_people = vec![handle.clone()];
    let topic = "com.apple.private.alloy.facetime.multi";

    if let Err(e) = ft
        .identity
        .cache_keys(
            topic,
            &relevant_people,
            &handle,
            false,
            &QueryOptions { required_for_message: true, result_expected: true },
        )
        .await
    {
        warn!(
            "suppress_own_device_ring: cache_keys failed for session {}: {:?}",
            guid, e
        );
        return;
    }

    let targets = ft
        .identity
        .cache
        .lock()
        .await
        .get_participants_targets(topic, &handle, &relevant_people);
    if targets.is_empty() {
        // No other own-devices registered — nothing to suppress.
        return;
    }

    let ids_send = IDSSendMessage {
        sender: handle.clone(),
        raw: Raw::Body(message.encode_to_vec()),
        send_delivered: false,
        command: 242,
        no_response: false,
        id: uuid::Uuid::new_v4().to_string().to_uppercase(),
        scheduled_ms: None,
        queue_id: None,
        relay: None,
        extras: Default::default(),
    };

    match ft.identity.send_message(topic, ids_send, targets).await {
        Ok(_) => info!(
            "suppress_own_device_ring: sent RespondedElsewhere to own handle {} for session {}",
            handle, guid
        ),
        Err(e) => warn!(
            "suppress_own_device_ring: send_message failed for session {}: {:?}",
            guid, e
        ),
    }
}

// Overlay around FTClient::handle that tolerates Apple sending cmd 207/209
// wire messages with `ConversationParticipantDidJoinContext.message = None`.
// Upstream's handler at facetime.rs:1272/1344 hard-requires that field and
// returns PushError::BadMsg when it's missing, which means the bridge never
// records the joiner in session.participants. When wife answers an
// outbound call (or when a link-tap joiner completes), her device's
// server-originated 207 trips this path and the session state diverges
// from Apple's — symptom is "this call is not available" on the callee.
//
// Strategy: always try upstream first so successful paths stay on the
// reference implementation. Only on BadMsg do we re-decode the wire
// message locally (using upstream's pub types: FTWireMessage +
// facetimep::ConversationParticipantDidJoinContext) and register the
// joiner ourselves. No upstream source changes — see feedback_no_patch_rustpush.
async fn ft_handle_with_join_recovery(
    ft: &rustpush::facetime::FTClient,
    msg: rustpush::APSMessage,
) -> Result<Option<rustpush::facetime::FTMessage>, rustpush::PushError> {
    // Locally-mirrored wire-message shape. FTWireMessage's fields are
    // private upstream, so we can't deserialize into it directly — but the
    // plist schema is stable (same serde rename attrs as upstream), so a
    // parallel struct here gives us the fields we need without touching
    // upstream source.
    #[derive(serde::Deserialize, Debug)]
    #[serde(crate = "serde", rename_all = "kebab-case")]
    struct LocalFTWire {
        #[serde(rename = "s")]
        session: String,
        #[serde(default)]
        client_context_data_key: Option<plist::Value>,
        #[serde(default)]
        participant_id_key: Option<LocalParticipantId>,
    }

    #[derive(serde::Deserialize, Debug, Clone, Copy)]
    #[serde(crate = "serde", untagged)]
    enum LocalParticipantId {
        Signed(i64),
        Unsigned(u64),
    }
    impl LocalParticipantId {
        fn as_u64(self) -> u64 {
            match self {
                Self::Signed(i) => i as u64,
                Self::Unsigned(i) => i,
            }
        }
    }

    // Try upstream first.
    let upstream_result = ft.handle(msg.clone()).await;
    if !matches!(&upstream_result, Err(rustpush::PushError::BadMsg)) {
        return upstream_result;
    }

    // Re-decrypt to inspect cmd and payload. identity.receive_message has
    // no side effects beyond decryption, so a second call is safe.
    let recv = match ft
        .identity
        .receive_message(
            msg,
            &[
                "com.apple.private.alloy.facetime.multi",
                "com.apple.private.alloy.facetime.video",
            ],
        )
        .await
    {
        Ok(Some(r)) => r,
        _ => return upstream_result,
    };

    // Only recover 207 (JoinEvent) and 209 (GroupUpdate).
    if recv.command != 207 && recv.command != 209 {
        return upstream_result;
    }

    let Some(message_unenc) = recv.message_unenc else { return upstream_result };
    let Some(sender) = recv.sender.clone() else { return upstream_result };
    let Some(target) = recv.target.clone() else { return upstream_result };
    let Some(token_bytes) = recv.token.clone() else { return upstream_result };
    let Some(ns_since_epoch) = recv.ns_since_epoch else { return upstream_result };

    let payload_value: plist::Value = match message_unenc.plist() {
        Ok(v) => v,
        Err(_) => return upstream_result,
    };
    let wire: LocalFTWire = match plist::from_value(&payload_value) {
        Ok(w) => w,
        Err(_) => return upstream_result,
    };

    // Apply minimal state mutation: session lookup/creation + joiner
    // registration. We intentionally skip session.unpack_members (the
    // upstream helper is private) — member-list drift is cosmetic; the
    // load-bearing piece is session.participants so Apple-side state
    // lines up when Apple retries delivery or the callee's device
    // queries participant tokens.
    let mut state = ft.state.write().await;
    let session = state.sessions.entry(wire.session.clone()).or_default();
    session.group_id = wire.session.clone();
    if !session.my_handles.contains(&target) {
        session.my_handles.push(target.clone());
    }
    let guid = session.group_id.clone();

    let emitted = if recv.command == 207 {
        let participant_id = match wire.participant_id_key {
            Some(id) => id.as_u64(),
            None => return upstream_result,
        };
        session.participants.insert(
            participant_id.to_string(),
            rustpush::facetime::FTParticipant {
                token: Some(base64_encode(&token_bytes)),
                participant_id,
                last_join_date: Some(ns_since_epoch / 1_000_000),
                handle: sender.clone(),
                active: None,
            },
        );

        if session.is_propped && sender.starts_with("temp:") {
            // Matches upstream's behavior at facetime.rs:1315 — once someone
            // has joined via link tap, the call is live and the propped-up
            // invitation can retire.
            let _ = ft.unprop_conv(session).await;
        }

        Some(rustpush::facetime::FTMessage::JoinEvent {
            guid: guid.clone(),
            participant: participant_id,
            handle: sender.clone(),
            // ring=false: without the message field we can't tell whether
            // Apple's carrying an Invitation type. Missing the ring flag
            // is strictly better than failing the handshake entirely.
            ring: false,
        })
    } else {
        // 209 (GroupUpdate) — nothing to emit; local state is best-effort.
        None
    };

    info!(
        "FaceTime cmd={} BadMsg recovery: session={} sender={} target={} participant={:?}",
        recv.command, guid, sender, target, wire.participant_id_key,
    );

    Ok(emitted)
}

async fn auto_approve_bridge_letmein(
    facetime: &rustpush::facetime::FTClient,
    request: &rustpush::facetime::LetMeInRequest,
) -> Result<(), rustpush::PushError> {
    // Only auto-approve for links owned by this bridge. Persistent links
    // (from get_link_for_usage) have usage=Some("bridge"); session-specific
    // links (from get_session_link) have usage=None but session_link=Some.
    // Both are bridge-created and safe to auto-approve.
    let (link_handle, linked_group, member_group, ringing_group) = {
        let state = facetime.state.read().await;
        let Some(link) = state.links.get(&request.pseud) else {
            return Ok(());
        };
        let is_bridge_usage = link.usage.as_deref() == Some("bridge");
        let is_session_link = link.session_link.is_some();
        if !is_bridge_usage && !is_session_link {
            return Ok(());
        }

        let linked_group = link
            .session_link
            .clone()
            .filter(|group| state.sessions.contains_key(group));

        let member_group = state.sessions.iter().find_map(|(group, session)| {
            if !session.my_handles.iter().any(|h| h == &link.handle) {
                return None;
            }
            if session.members.iter().any(|member| member.handle == request.requestor) {
                Some(group.clone())
            } else {
                None
            }
        });

        let ringing_group = state.sessions.iter().find_map(|(group, session)| {
            if !session.my_handles.iter().any(|h| h == &link.handle) {
                return None;
            }
            if session.is_ringing_inaccurate {
                Some(group.clone())
            } else {
                None
            }
        });

        (link.handle.clone(), linked_group, member_group, ringing_group)
    };

    // Priority: ringing > linked > member. An actively-ringing session is
    // always the user's immediate concern (inbound-call case); a stale
    // session_link from a prior tap would otherwise win via `linked` and
    // route the tap to the wrong session. `linked` is still preferred over
    // `member` since it's a deliberate pin, and session-specific links
    // (outbound !im facetime) always hit `linked` first because their
    // session is fresh (is_ringing_inaccurate=false until the ring fires).
    let match_kind = if ringing_group.is_some() {
        "ringing"
    } else if linked_group.is_some() {
        "linked"
    } else if member_group.is_some() {
        "member"
    } else {
        "cold-start"
    };
    info!(
        "FaceTime letmein approve: match_kind={} pseud={} requestor={} link_handle={}",
        match_kind, request.pseud, request.requestor, link_handle,
    );

    let approved_group = if let Some(group) = ringing_group.or(linked_group).or(member_group) {
        group
    } else {
        let group = uuid::Uuid::new_v4().to_string().to_uppercase();
        // Cold-start fallback: the tap request doesn't map to any known
        // session via linked/member/ringing. Call upstream directly — the
        // strip-own-devices wrapper caused the callee not to ring (see
        // WrappedFaceTimeClient::create_session for the full writeup).
        facetime
            .create_session(group.clone(), link_handle.clone(), &[request.requestor.clone()])
            .await?;
        group
    };

    {
        let mut state = facetime.state.write().await;
        if let Some(link) = state.links.get_mut(&request.pseud) {
            if link.session_link.as_deref() != Some(approved_group.as_str()) {
                link.session_link = Some(approved_group.clone());
            }
        }
    }

    // respond_letmein: sends LetMeInResponse then add_members/ring over APNs.
    // APNs can flap (early eof → SendTimedOut) especially right after boot.
    //
    // Subtlety: respond_letmein's first action for delegated requests is
    // removing the delegation_uuid from delegated_requests. If the later
    // send fails and we retry with the same request, it hits "Already
    // responded" and no-ops silently — member never added. Strip
    // delegation_uuid on retries so the check is skipped; duplicate
    // LetMeInResponse sends are harmless (web client decrypts the first),
    // and add_members is idempotent (already-present member triggers ring
    // instead of re-add).
    let mut last_err: Option<rustpush::PushError> = None;
    for attempt in 0..3u32 {
        let mut retry_request = request.clone();
        if attempt > 0 {
            retry_request.delegation_uuid = None;
        }
        match facetime.respond_letmein(retry_request, Some(&approved_group)).await {
            Ok(()) => {
                info!(
                    "FaceTime auto-approved LetMeIn request for bridge link: requestor={} group={} usage=bridge",
                    request.requestor,
                    approved_group
                );
                suppress_own_device_ring(facetime, &approved_group).await;
                return Ok(());
            }
            Err(e) => {
                let is_timeout = matches!(&e, rustpush::PushError::SendTimedOut);
                last_err = Some(e);
                if !is_timeout || attempt >= 2 {
                    break;
                }
                warn!(
                    "FaceTime letmein respond_letmein timed out (attempt {}), retrying in {}s",
                    attempt + 1,
                    attempt + 1
                );
                tokio::time::sleep(std::time::Duration::from_secs((attempt + 1) as u64)).await;
            }
        }
    }
    Err(last_err.unwrap_or(rustpush::PushError::SendTimedOut))
}

async fn facetime_event_to_wrapped(
    facetime: &rustpush::facetime::FTClient,
    event: &rustpush::facetime::FTMessage,
) -> Option<WrappedMessage> {
    let (guid, mut sender, marker) = match event {
        rustpush::facetime::FTMessage::Ring { guid } => {
            (guid.clone(), None, FACETIME_RING_MARKER)
        }
        rustpush::facetime::FTMessage::JoinEvent { guid, handle, ring, .. } if *ring => {
            (guid.clone(), Some(handle.clone()), FACETIME_RING_MARKER)
        }
        rustpush::facetime::FTMessage::AddMembers { guid, members, ring } if *ring => {
            let fallback = members.iter().next().map(|member| member.handle.clone());
            (guid.clone(), fallback, FACETIME_RING_MARKER)
        }
        rustpush::facetime::FTMessage::Decline { guid } => {
            (guid.clone(), None, FACETIME_MISSED_MARKER)
        }
        rustpush::facetime::FTMessage::RespondedElsewhere { guid } => {
            (guid.clone(), None, FACETIME_ANSWERED_ELSEWHERE_MARKER)
        }
        _ => return None,
    };

    let state = facetime.state.read().await;
    let (participants, my_handles, link) = state
        .sessions
        .get(&guid)
        .map(|session| {
            let participants = session
                .members
                .iter()
                .map(|member| member.handle.clone())
                .collect::<Vec<_>>();
            let my_handles: std::collections::HashSet<String> =
                session.my_handles.iter().cloned().collect();
            let link = session.link.as_ref().map(|link| {
                let encoded = base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(&link.public_key);
                let pseud = link
                    .pseudonym
                    .strip_prefix("temp:")
                    .unwrap_or(&link.pseudonym);
                format!("https://facetime.apple.com/join#v=1&p={pseud}&k={encoded}")
            });
            (participants, my_handles, link)
        })
        .unwrap_or_else(|| (Vec::new(), std::collections::HashSet::new(), None));
    drop(state);

    if sender.is_none() {
        sender = participants
            .iter()
            .find(|participant| {
                !participant.starts_with("temp:") && !my_handles.contains(*participant)
            })
            .cloned();
    }

    let now_ms = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as u64;
    let event_kind = marker.trim_matches('[').trim_matches(']').to_lowercase();
    let mut wrapped = WrappedMessage::default();
    wrapped.uuid = format!("FACETIME_{}_{}_{}", event_kind.to_uppercase(), guid, now_ms);
    wrapped.sender = sender;
    wrapped.text = Some(match link {
        Some(link) if marker == FACETIME_RING_MARKER => format!("{marker} guid={guid} {link}"),
        _ => format!("{marker} guid={guid}"),
    });
    wrapped.participants = participants;
    wrapped.timestamp_ms = now_ms;
    wrapped.is_notify_anyways = true;
    Some(wrapped)
}

fn message_inst_to_wrapped(msg: &MessageInst) -> WrappedMessage {
    let conv = msg.conversation.as_ref();

    let mut w = WrappedMessage {
        uuid: msg.id.clone(),
        sender: msg.sender.clone(),
        text: None,
        subject: None,
        participants: conv.map(|c| c.participants.clone()).unwrap_or_default(),
        group_name: conv.and_then(|c| c.cv_name.clone()),
        timestamp_ms: msg.sent_timestamp,
        is_sms: false,
        is_tapback: false,
        tapback_type: None,
        tapback_target_uuid: None,
        tapback_target_part: None,
        tapback_emoji: None,
        tapback_remove: false,
        is_edit: false,
        edit_target_uuid: None,
        edit_part: None,
        edit_new_text: None,
        is_unsend: false,
        unsend_target_uuid: None,
        unsend_edit_part: None,
        is_rename: false,
        new_chat_name: None,
        is_participant_change: false,
        new_participants: vec![],
        attachments: vec![],
        reply_guid: None,
        reply_part: None,
        is_typing: false,
        typing_app_bundle_id: None,
        typing_app_icon: None,
        is_read_receipt: false,
        is_delivered: false,
        is_error: false,
        error_for_uuid: None,
        error_status: None,
        error_status_str: None,
        is_peer_cache_invalidate: false,
        send_delivered: msg.send_delivered,
        sender_guid: conv.and_then(|c| c.sender_guid.clone()),
        is_move_to_recycle_bin: false,
        is_permanent_delete: false,
        is_recover_chat: false,
        delete_chat_participants: vec![],
        delete_chat_group_id: None,
        delete_chat_guid: None,
        delete_message_uuids: vec![],
        is_stored_message: false, // set by caller based on connection state
        is_icon_change: false,
        group_photo_cleared: false,
        icon_change_photo_data: None,
        html: None,
        is_voice: false,
        effect: None,
        scheduled_ms: None,
        is_sms_activation: None,
        is_sms_confirm_sent: None,
        is_mark_unread: false,
        is_message_read_on_device: false,
        is_unschedule: false,
        is_update_extension: false,
        update_extension_for_uuid: None,
        is_update_profile_sharing: false,
        update_profile_sharing_dismissed: vec![],
        update_profile_sharing_all: vec![],
        update_profile_sharing_version: None,
        is_update_profile: false,
        update_profile_share_contacts: None,
        is_notify_anyways: false,
        is_set_transcript_background: false,
        transcript_background_remove: None,
        transcript_background_chat_id: None,
        transcript_background_object_id: None,
        transcript_background_url: None,
        transcript_background_file_size: None,
        sticker_data: None,
        sticker_mime: None,
        is_share_profile: false,
        share_profile_record_key: None,
        share_profile_decryption_key: None,
        share_profile_has_poster: false,
        share_profile_display_name: None,
        share_profile_first_name: None,
        share_profile_last_name: None,
        share_profile_avatar: None,
    };

    match &msg.message {
        Message::Message(normal) => {
            w.text = Some(normal.parts.raw_text());
            w.subject = normal.subject.clone();
            w.reply_guid = normal.reply_guid.clone();
            w.reply_part = normal.reply_part.clone();
            w.is_sms = matches!(normal.service, MessageType::SMS { .. });
            // iOS piggybacks Name & Photo Sharing keys on regular text
            // messages from contacts who have sharing enabled; lift them so
            // the receive loop's inline download still fires.
            if let Some(profile) = &normal.embedded_profile {
                populate_share_profile_keys(&mut w, profile, "NormalMessage.embedded_profile");
            }

            for indexed_part in &normal.parts.0 {
                if let MessagePart::Attachment(att) = &indexed_part.part {
                    let (is_inline, inline_data, size, mmcs_descriptor_json) =
                        match &att.a_type {
                            AttachmentType::Inline(data) => {
                                (true, Some(data.clone()), data.len() as u64, None)
                            }
                            AttachmentType::MMCS(mmcs) => {
                                let descriptor = MmcsDescriptor::from_file(mmcs);
                                let json = serde_json::to_string(&descriptor).ok();
                                (false, None, mmcs.size as u64, json)
                            }
                        };
                    w.attachments.push(WrappedAttachment {
                        mime_type: att.mime.clone(),
                        filename: att.name.clone(),
                        uti_type: att.uti_type.clone(),
                        size,
                        is_inline,
                        inline_data,
                        iris: att.iris,
                        mmcs_descriptor_json,
                    });
                }
            }

            // Encode rich link as special attachments for the Go side
            if let Some(ref lm) = normal.link_meta {
                let original_url: String = lm
                    .data
                    .original_url
                    .clone()
                    .map(|u| -> String { u.into() })
                    .unwrap_or_default();
                let url: String = lm.data.url.clone().map(|u| u.into()).unwrap_or_default();
                let title = lm.data.title.clone().unwrap_or_default();
                let summary = lm.data.summary.clone().unwrap_or_default();

                info!("Inbound rich link: original_url={}, url={}, title={:?}, summary={:?}, has_image={}, has_icon={}",
                    original_url, url, title, summary,
                    lm.data.image.is_some(), lm.data.icon.is_some());

                let image_mime = if let Some(ref img) = lm.data.image {
                    img.mime_type.clone()
                } else if let Some(ref icon) = lm.data.icon {
                    icon.mime_type.clone()
                } else {
                    String::new()
                };

                // Metadata: original_url\x01url\x01title\x01summary\x01image_mime
                let meta = format!("{}\x01{}\x01{}\x01{}\x01{}",
                    original_url, url, title, summary, image_mime);
                w.attachments.push(WrappedAttachment {
                    mime_type: "x-richlink/meta".to_string(),
                    filename: String::new(),
                    uti_type: String::new(),
                    size: 0,
                    is_inline: true,
                    inline_data: Some(meta.into_bytes()),
                    iris: false,
                    mmcs_descriptor_json: None,
                });

                // Image data (from image or icon)
                let image_data = if let Some(ref img) = lm.data.image {
                    let idx = img.rich_link_image_attachment_substitute_index as usize;
                    lm.attachments.get(idx).cloned()
                } else if let Some(ref icon) = lm.data.icon {
                    let idx = icon.rich_link_image_attachment_substitute_index as usize;
                    lm.attachments.get(idx).cloned()
                } else {
                    None
                };

                if let Some(img_data) = image_data {
                    w.attachments.push(WrappedAttachment {
                        mime_type: "x-richlink/image".to_string(),
                        filename: String::new(),
                        uti_type: String::new(),
                        size: img_data.len() as u64,
                        is_inline: true,
                        inline_data: Some(img_data),
                        iris: false,
                        mmcs_descriptor_json: None,
                    });
                }
            }

            // HTML formatting
            w.html = parts_to_html(&normal.parts);

            // Voice message flag
            w.is_voice = normal.voice;

            // Screen/bubble effects
            w.effect = normal.effect.as_ref().map(|e| e.to_string());

            // Scheduled send
            if let Some(ref sched) = normal.scheduled {
                w.scheduled_ms = Some(sched.ms);
            }

            // Sticker data from extension balloons (icon field)
            if let Some(ref app) = normal.app {
                if let Some(ref balloon) = app.balloon {
                    if let Some(ref icon_data) = balloon.icon {
                        if !icon_data.is_empty() {
                            w.sticker_data = Some(icon_data.clone());
                            w.sticker_mime = Some("image/png".to_string());
                        }
                    }
                }
            }
        }
        Message::React(react) => {
            w.is_tapback = true;
            w.tapback_target_uuid = Some(react.to_uuid.clone());
            w.tapback_target_part = react.to_part;
            match &react.reaction {
                ReactMessageType::React { reaction, enable } => {
                    let (tt, emoji, remove) = convert_reaction(reaction, *enable);
                    w.tapback_type = tt;
                    w.tapback_emoji = emoji;
                    w.tapback_remove = remove;
                }
                ReactMessageType::Extension { .. } => {
                    // Extension reactions (stickers etc.) — mark as tapback
                    w.tapback_type = Some(7);
                }
            }
            // Reactions can also carry a piggybacked profile (same pattern
            // as text messages). Surface it for inline download.
            if let Some(profile) = &react.embedded_profile {
                populate_share_profile_keys(&mut w, profile, "ReactMessage.embedded_profile");
            }
        }
        Message::Edit(edit) => {
            w.is_edit = true;
            w.edit_target_uuid = Some(edit.tuuid.clone());
            w.edit_part = Some(edit.edit_part);
            w.edit_new_text = Some(edit.new_parts.raw_text());
        }
        Message::Unsend(unsend) => {
            w.is_unsend = true;
            w.unsend_target_uuid = Some(unsend.tuuid.clone());
            w.unsend_edit_part = Some(unsend.edit_part);
        }
        Message::RenameMessage(rename) => {
            w.is_rename = true;
            w.new_chat_name = Some(rename.new_name.clone());
        }
        Message::ChangeParticipants(change) => {
            w.is_participant_change = true;
            w.new_participants = change.new_participants.clone();
        }
        Message::Typing(typing, app) => {
            w.is_typing = *typing;
            if let Some(app) = app {
                w.typing_app_bundle_id = Some(app.bundle_id.clone());
                w.typing_app_icon = Some(app.icon.clone());
            }
        }
        Message::Read => {
            w.is_read_receipt = true;
        }
        Message::Delivered => {
            w.is_delivered = true;
        }
        Message::Error(err) => {
            w.is_error = true;
            w.error_for_uuid = Some(err.for_uuid.clone());
            w.error_status = Some(err.status);
            w.error_status_str = Some(err.status_str.clone());
        }
        Message::PeerCacheInvalidate => {
            w.is_peer_cache_invalidate = true;
        }
        Message::MoveToRecycleBin(del) => {
            w.is_move_to_recycle_bin = true;
            populate_delete_target(&mut w, &del.target);
        }
        Message::PermanentDelete(del) => {
            w.is_permanent_delete = true;
            populate_delete_target(&mut w, &del.target);
        }
        Message::RecoverChat(chat) => {
            w.is_recover_chat = true;
            w.delete_chat_participants = chat.participants.clone();
            w.delete_chat_group_id = Some(chat.group_id.clone()).filter(|s| !s.is_empty());
            w.delete_chat_guid = Some(chat.guid.clone()).filter(|s| !s.is_empty());
        }
        Message::IconChange(change) => {
            w.is_icon_change = true;
            w.group_photo_cleared = change.file.is_none();
        }
        Message::EnableSmsActivation(enable) => {
            w.is_sms_activation = Some(*enable);
        }
        Message::SmsConfirmSent(status) => {
            w.is_sms_confirm_sent = Some(*status);
        }
        Message::MarkUnread => {
            w.is_mark_unread = true;
        }
        Message::MessageReadOnDevice => {
            w.is_message_read_on_device = true;
        }
        Message::Unschedule => {
            w.is_unschedule = true;
        }
        Message::UpdateExtension(update) => {
            w.is_update_extension = true;
            w.update_extension_for_uuid = Some(update.for_uuid.clone());
        }
        Message::NotifyAnyways => {
            w.is_notify_anyways = true;
        }
        Message::ShareProfile(profile) => {
            populate_share_profile_keys(&mut w, profile, "Message::ShareProfile");
        }
        Message::UpdateProfile(update) => {
            w.is_update_profile = true;
            w.update_profile_share_contacts = Some(update.share_contacts);
            if let Some(profile) = &update.profile {
                populate_share_profile_keys(&mut w, profile, "Message::UpdateProfile.profile");
            }
        }
        Message::UpdateProfileSharing(update) => {
            w.is_update_profile_sharing = true;
            w.update_profile_sharing_dismissed = update.shared_dismissed.clone();
            w.update_profile_sharing_all = update.shared_all.clone();
            w.update_profile_sharing_version = Some(update.version);
        }
        Message::SetTranscriptBackground(update) => {
            w.is_set_transcript_background = true;
            match update {
                SetTranscriptBackgroundMessage::Remove { chat_id, remove, .. } => {
                    w.transcript_background_remove = Some(*remove);
                    w.transcript_background_chat_id = chat_id.clone();
                }
                SetTranscriptBackgroundMessage::Set { chat_id, object_id, url, file_size, .. } => {
                    w.transcript_background_remove = Some(false);
                    w.transcript_background_chat_id = chat_id.clone();
                    w.transcript_background_object_id = Some(object_id.clone());
                    w.transcript_background_url = Some(url.clone());
                    w.transcript_background_file_size = Some(*file_size as u64);
                }
            }
        }
        _ => {}
    }

    w
}

// ============================================================================
// Callback interfaces
// ============================================================================

#[uniffi::export(callback_interface)]
pub trait MessageCallback: Send + Sync {
    fn on_message(&self, msg: WrappedMessage);
}

#[uniffi::export(callback_interface)]
pub trait UpdateUsersCallback: Send + Sync {
    fn update_users(&self, users: Arc<WrappedIDSUsers>);
}

/// Callback invoked when an iMessage contact's presence state changes.
/// `user` is an iMessage handle (e.g. "tel:+1..." or "mailto:..."). When
/// `available` is false, `mode` may carry a Focus/DND mode identifier such
/// as "com.apple.donotdisturb.mode.default".
#[uniffi::export(callback_interface)]
pub trait StatusCallback: Send + Sync {
    fn on_status_update(&self, user: String, mode: Option<String>, available: bool);
    /// Called when StatusKit receives a key-sharing message, which adds new
    /// encryption keys to the internal state. The Go side should re-subscribe
    /// to presence so that APNs channels are created for the newly-available keys.
    fn on_keys_received(&self);
    /// Called once per reshare with the `from` handle of the peer device that
    /// sent it. Peer iOS fans reshares to every alias of our account (tel: plus
    /// each mailto:) with the SAME channel id but a DIFFERENT sender handle.
    /// Upstream state keys by channel, so every reshare overwrites the previous
    /// sender — only the last one survives in `state.keys`. Without this hook
    /// the Go side only learns ONE alias per peer and cannot resolve presence
    /// updates that arrive on any of the others. Stamping each sender into the
    /// learned-sender cache preserves all aliases across overwrites.
    fn on_reshare_sender(&self, sender: String);
}

// ============================================================================
// Top-level functions
// ============================================================================

#[uniffi::export]
pub fn init_logger() {
    if std::env::var("RUST_LOG").is_err() {
        // Default log filter: silence the entire `rustpush` crate at WARN
        // (upstream OpenBubbles has ~80+ info! sites across mmcs, cloudkit,
        // keychain, cloud_messages, pcs, aps, util, statuskit, findmy, and
        // more — vastly more chatty than master's vendored tree). Keep our
        // own `rustpushgo` wrapper at INFO so Ford recovery messages,
        // relay wiring, and other wrapper-level diagnostics surface.
        //
        // The Go-side bridge (`component=imessage`, `component=cloud_sync`,
        // etc.) writes through zerolog and is unaffected — those logs come
        // through at whatever level the Go bridge is configured for.
        //
        // If you need to debug rustpush internals, override at invocation
        // time with e.g.:
        //   RUST_LOG=info,rustpush=info          # all rustpush info logs
        //   RUST_LOG=info,rustpush::icloud=debug # deep cloudkit/mmcs/pcs
        //   RUST_LOG=debug                       # everything
        std::env::set_var(
            "RUST_LOG",
            "warn,rustpush=warn,rustpushgo=info",
        );
    }
    let _ = pretty_env_logger::try_init();

    // Install a panic hook that silences upstream's `.unwrap()` /
    // `.expect()` panics inside the CloudKit download path. These panics
    // are intentional — the wrapper's Ford dedup recovery loops wrap
    // each get_assets / download_attachment call in catch_unwind and
    // brute-force cached Ford keys until one decrypts successfully. The
    // default Rust panic hook runs BEFORE catch_unwind catches, so each
    // wrong-key attempt floods stderr with lines like
    //   thread '<unnamed>' panicked at mmcs.rs:1113:101
    //   called `Result::unwrap()` on an `Err` value
    //   thread '<unnamed>' panicked at cloudkit.rs:2075
    //   No bundled asset!
    // This hook silently drops panics from upstream's MMCS + CloudKit
    // download code paths (all caught by the wrapper) and falls through
    // to the default hook for everything else — real panics from other
    // modules still surface normally.
    let default_hook = std::panic::take_hook();
    std::panic::set_hook(Box::new(move |info| {
        if let Some(loc) = info.location() {
            let file = loc.file();
            // Match both Unix and Windows path separators. Files listed
            // here are ones whose panics are intercepted by
            // catch_unwind in pkg/rustpushgo/src/lib.rs download recovery.
            let noisy = [
                "icloud/mmcs.rs",
                "icloud\\mmcs.rs",
                "icloud/cloudkit.rs",
                "icloud\\cloudkit.rs",
            ];
            if noisy.iter().any(|p| file.ends_with(p)) {
                return;
            }
        }
        default_hook(info);
    }));

    // Initialize the keystore with a file-backed software keystore.
    // This must be called before any rustpush operations (APNs connect, login, etc.).
    //
    // The keystore lives alongside session.json in the XDG data directory
    // (~/.local/share/mautrix-imessage/) so that all session state is in one
    // place and easy to migrate between machines.
    let xdg_dir = resolve_xdg_data_dir();
    let state_path = format!("{}/keystore.plist", xdg_dir);
    let _ = std::fs::create_dir_all(&xdg_dir);

    // Migrate from the old location (state/keystore.plist relative to working
    // directory) if the new file doesn't exist yet.
    let legacy_path = "state/keystore.plist";
    if !std::path::Path::new(&state_path).exists() {
        if std::path::Path::new(legacy_path).exists() {
            match std::fs::copy(legacy_path, &state_path) {
                Ok(_) => info!(
                    "Migrated keystore from {} to {}",
                    legacy_path, state_path
                ),
                Err(e) => warn!(
                    "Failed to migrate keystore from {} to {}: {}",
                    legacy_path, state_path, e
                ),
            }
        }
    }

    let state: SoftwareKeystoreState = match std::fs::read(&state_path) {
        Ok(data) => plist::from_bytes(&data).unwrap_or_else(|e| {
            warn!("Failed to parse keystore at {}: {} — starting with empty keystore", state_path, e);
            SoftwareKeystoreState::default()
        }),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            info!("No keystore file at {} — starting fresh", state_path);
            SoftwareKeystoreState::default()
        }
        Err(e) => {
            warn!("Failed to read keystore at {}: {} — starting with empty keystore", state_path, e);
            SoftwareKeystoreState::default()
        }
    };
    let path_for_closure = state_path.clone();
    init_keystore(SoftwareKeystore {
        state: RwLock::new(state),
        update_state: Box::new(move |s| {
            let _ = plist::to_file_xml(&path_for_closure, s);
        }),
        encryptor: NoEncryptor,
    });
}

/// Resolve the XDG data directory for mautrix-imessage session state.
/// Uses $XDG_DATA_HOME if set, otherwise ~/.local/share.
/// Returns the full path: <base>/mautrix-imessage
fn resolve_xdg_data_dir() -> String {
    if let Ok(xdg) = std::env::var("XDG_DATA_HOME") {
        if !xdg.is_empty() {
            return format!("{}/mautrix-imessage", xdg);
        }
    }
    if let Some(home) = std::env::var("HOME").ok().filter(|h| !h.is_empty()) {
        return format!("{}/.local/share/mautrix-imessage", home);
    }
    // Last resort — fall back to old relative path
    warn!("Could not determine HOME or XDG_DATA_HOME, using local state directory");
    "state".to_string()
}

fn subsystem_state_path(file_name: &str) -> String {
    let xdg_dir = resolve_xdg_data_dir();
    let _ = std::fs::create_dir_all(&xdg_dir);
    format!("{}/{}", xdg_dir, file_name)
}

fn read_plist_state<T: serde::de::DeserializeOwned>(path: &str) -> Option<T> {
    match std::fs::read(path) {
        Ok(data) => match plist::from_bytes(&data) {
            Ok(state) => Some(state),
            Err(err) => {
                warn!("Failed to parse state at {}: {}", path, err);
                None
            }
        },
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => None,
        Err(err) => {
            warn!("Failed to read state at {}: {}", path, err);
            None
        }
    }
}

fn persist_plist_state<T: serde::Serialize>(path: &str, state: &T) {
    // Write to a sibling tmp file, then atomic-rename. Sidesteps EACCES
    // when the existing file at `path` is owned by a UID different from
    // the current bridge process (stranded from a prior deploy under a
    // different uid/gid — seen on Beeper-hosted infra where plist files
    // landed as 0644 owned by a previous deploy's user, leaving the
    // current bridge with read-but-not-write access). The tmp file
    // inherits our uid; rename(2) replaces the old inode regardless of
    // its ownership, since we own the parent directory.
    let tmp_path = format!("{}.tmp.{}", path, std::process::id());
    let write_result = (|| -> Result<(), Box<dyn std::error::Error>> {
        let mut file = std::fs::File::create(&tmp_path)?;
        plist::to_writer_xml(&mut file, state)?;
        file.sync_all()?;
        std::fs::rename(&tmp_path, path)?;
        Ok(())
    })();
    if let Err(err) = write_result {
        let _ = std::fs::remove_file(&tmp_path);
        warn!("Failed to persist state to {}: {}", path, err);
    }
}

fn serialize_state_json<T: serde::Serialize>(state: &T) -> Result<String, WrappedError> {
    serde_json::to_string(state).map_err(|err| WrappedError::GenericError {
        msg: format!("Failed to serialize state: {}", err),
    })
}

/// Create a local macOS config that reads hardware info from IOKit
/// and uses AAAbsintheContext for NAC validation (no SIP disable, no relay needed).
/// Only works on macOS — returns an error on other platforms.
#[uniffi::export]
pub fn create_local_macos_config() -> Result<Arc<WrappedOSConfig>, WrappedError> {
    #[cfg(target_os = "macos")]
    {
        let config = local_config::LocalMacOSConfig::new()
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to read hardware info: {}", e) })?
            .into_macos_config();
        Ok(Arc::new(WrappedOSConfig {
            config: Arc::new(config),
            has_nac_relay: false,
            relay_url: None,
            relay_token: None,
        }))
    }
    #[cfg(not(target_os = "macos"))]
    {
        Err(WrappedError::GenericError {
            msg: "Local macOS config is only available on macOS. Use create_config_from_hardware_key instead.".into(),
        })
    }
}

/// Create a local macOS config with a persisted device ID.
/// Only works on macOS — returns an error on other platforms.
#[uniffi::export]
pub fn create_local_macos_config_with_device_id(device_id: String) -> Result<Arc<WrappedOSConfig>, WrappedError> {
    #[cfg(target_os = "macos")]
    {
        let config = local_config::LocalMacOSConfig::new()
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to read hardware info: {}", e) })?
            .with_device_id(device_id)
            .into_macos_config();
        Ok(Arc::new(WrappedOSConfig {
            config: Arc::new(config),
            has_nac_relay: false,
            relay_url: None,
            relay_token: None,
        }))
    }
    #[cfg(not(target_os = "macos"))]
    {
        Err(WrappedError::GenericError {
            msg: "Local macOS config is only available on macOS. Use create_config_from_hardware_key_with_device_id instead.".into(),
        })
    }
}

/// Create a cross-platform config from a base64-encoded JSON hardware key.
///
/// The hardware key is a JSON-serialized `HardwareConfig` extracted once from
/// a real Mac (e.g., via copper's QR code tool). This config uses the
/// open-absinthe NAC emulator to generate fresh validation data on any platform.
///
/// On macOS this is not needed (use `create_local_macos_config` instead).
/// Building with the `hardware-key` feature links open-absinthe + unicorn.
#[uniffi::export]
pub fn create_config_from_hardware_key(base64_key: String) -> Result<Arc<WrappedOSConfig>, WrappedError> {
    _create_config_from_hardware_key_inner(base64_key, None)
}

/// Create a cross-platform config from a base64-encoded JSON hardware key
/// with a persisted device ID.
#[uniffi::export]
pub fn create_config_from_hardware_key_with_device_id(base64_key: String, device_id: String) -> Result<Arc<WrappedOSConfig>, WrappedError> {
    _create_config_from_hardware_key_inner(base64_key, Some(device_id))
}

#[cfg(feature = "hardware-key")]
fn _create_config_from_hardware_key_inner(base64_key: String, device_id: Option<String>) -> Result<Arc<WrappedOSConfig>, WrappedError> {
    use base64::{Engine, engine::general_purpose::STANDARD};
    use rustpush::macos::{MacOSConfig, HardwareConfig};
    use serde::Deserialize;

    // Local wire-compatible struct: upstream MacOSConfig only has
    // inner/version/protocol_version/device_id/icloud_ua/aoskit_version/udid.
    // Our hardware-key JSON contract (shipped to existing users) ALSO carries
    // nac_relay_url/relay_token/relay_cert_fp for the Apple Silicon relay
    // path. Parsing into upstream MacOSConfig would drop the relay fields,
    // so we parse into a local struct that holds everything and then split.
    #[derive(Deserialize)]
    struct FullHardwareKey {
        #[serde(default)]
        inner: Option<HardwareConfig>,
        #[serde(default)]
        version: Option<String>,
        #[serde(default)]
        protocol_version: Option<u32>,
        #[serde(default)]
        device_id: Option<String>,
        #[serde(default)]
        icloud_ua: Option<String>,
        #[serde(default)]
        aoskit_version: Option<String>,
        #[serde(default)]
        udid: Option<String>,
        // NAC relay fields (Apple Silicon hw keys that can't be driven by
        // the unicorn x86-64 emulator). When set, these are stashed into
        // wrapper-level static state via register_nac_relay() for the
        // open-absinthe ValidationCtx Relay variant to consume.
        #[serde(default)]
        nac_relay_url: Option<String>,
        #[serde(default)]
        relay_token: Option<String>,
        #[serde(default)]
        relay_cert_fp: Option<String>,
    }

    // Strip whitespace/newlines that chat clients may insert when pasting
    let clean_key: String = base64_key.chars().filter(|c| !c.is_whitespace()).collect();
    let json_bytes = STANDARD.decode(&clean_key)
        .map_err(|e| WrappedError::GenericError { msg: format!("Invalid base64: {}", e) })?;

    // Try parsing as FullHardwareKey first (extract-key tool output, may be
    // the full MacOSConfig-shaped blob with relay fields). Fall back to
    // bare HardwareConfig for legacy keys that are just the hw blob.
    let (hw, version, protocol_version, icloud_ua, aoskit_version, nac_relay_url, relay_token, relay_cert_fp) =
        if let Ok(full) = serde_json::from_slice::<FullHardwareKey>(&json_bytes) {
            if let Some(hw) = full.inner {
                let version = full.version
                    .filter(|v| !v.trim().is_empty())
                    .unwrap_or_else(|| "13.6.4".to_string());
                let protocol_version = full.protocol_version
                    .filter(|v| *v != 0)
                    .unwrap_or(1660);
                // get_normal_ua() expects icloud_ua to contain whitespace so
                // it can split out the "com.apple.iCloudHelper/..." prefix.
                let icloud_ua = full.icloud_ua
                    .filter(|v| v.split_once(char::is_whitespace).is_some())
                    .unwrap_or_else(|| "com.apple.iCloudHelper/282 CFNetwork/1568.100.1 Darwin/22.5.0".to_string());
                let aoskit_version = full.aoskit_version
                    .filter(|v| !v.trim().is_empty())
                    .unwrap_or_else(|| "com.apple.AOSKit/282 (com.apple.accountsd/113)".to_string());
                (
                    hw,
                    version,
                    protocol_version,
                    icloud_ua,
                    aoskit_version,
                    full.nac_relay_url,
                    full.relay_token,
                    full.relay_cert_fp,
                )
            } else {
                // JSON parsed as FullHardwareKey but has no `inner` field —
                // retry as bare HardwareConfig.  Preserve any relay fields that
                // were already parsed into `full` — dropping them here would
                // silently skip register_nac_relay() for keys that carry relay
                // info at the top level without a nested `inner` object.
                let hw: HardwareConfig = serde_json::from_slice(&json_bytes)
                    .map_err(|e| WrappedError::GenericError { msg: format!("Invalid hardware key JSON: {}", e) })?;
                let version = full.version
                    .filter(|v| !v.trim().is_empty())
                    .unwrap_or_else(|| "13.6.4".to_string());
                let protocol_version = full.protocol_version
                    .filter(|v| *v != 0)
                    .unwrap_or(1660);
                let icloud_ua = full.icloud_ua
                    .filter(|v| v.split_once(char::is_whitespace).is_some())
                    .unwrap_or_else(|| "com.apple.iCloudHelper/282 CFNetwork/1568.100.1 Darwin/22.5.0".to_string());
                let aoskit_version = full.aoskit_version
                    .filter(|v| !v.trim().is_empty())
                    .unwrap_or_else(|| "com.apple.AOSKit/282 (com.apple.accountsd/113)".to_string());
                (
                    hw,
                    version,
                    protocol_version,
                    icloud_ua,
                    aoskit_version,
                    full.nac_relay_url,
                    full.relay_token,
                    full.relay_cert_fp,
                )
            }
        } else {
            let hw: HardwareConfig = serde_json::from_slice(&json_bytes)
                .map_err(|e| WrappedError::GenericError { msg: format!("Invalid hardware key JSON: {}", e) })?;
            (
                hw,
                "13.6.4".to_string(),
                1660,
                "com.apple.iCloudHelper/282 CFNetwork/1568.100.1 Darwin/22.5.0".to_string(),
                "com.apple.AOSKit/282 (com.apple.accountsd/113)".to_string(),
                None,
                None,
                None,
            )
        };

    // Always use the real hardware UUID from the extracted key so the bridge
    // shows up as the original Mac rather than a new phantom device.
    // Ignore any persisted device ID — it may be a stale random UUID.
    let hw_uuid = hw.platform_uuid.to_uppercase();
    if let Some(ref old) = device_id {
        if old != &hw_uuid {
            log::warn!(
                "Ignoring persisted device ID {} — using hardware UUID {} from extracted key",
                old, hw_uuid
            );
        }
    }
    let device_id = hw_uuid;

    let _ = relay_cert_fp;

    // Build upstream's MacOSConfig with only the fields it has — relay
    // fields live in wrapper state, not on the OSConfig.
    let config = Arc::new(MacOSConfig {
        inner: hw,
        version,
        protocol_version,
        device_id: device_id.clone(),
        icloud_ua,
        aoskit_version,
        // Avoid panics in codepaths that expect a UDID (Find My, CloudKit, etc).
        // Using the device UUID is sufficient.
        udid: Some(device_id),
    });

    // For Apple Silicon keys with a relay, wrap in RelayOSConfig so
    // generate_validation_data() calls the relay directly — same as master.
    let os_config: Arc<dyn OSConfig> = if let Some(url) = nac_relay_url {
        let trimmed = url.trim_end_matches('/');
        let relay_url = if trimmed.ends_with("/validation-data") {
            trimmed.to_string()
        } else {
            format!("{}/validation-data", trimmed)
        };
        Arc::new(RelayOSConfig {
            inner: config,
            relay_url,
            relay_token: relay_token.clone(),
        })
    } else {
        config
    };

    Ok(Arc::new(WrappedOSConfig {
        config: os_config,
        has_nac_relay: relay_token.is_some(),
        relay_url: None,
        relay_token: None,
    }))
}

#[cfg(not(feature = "hardware-key"))]
fn _create_config_from_hardware_key_inner(base64_key: String, _device_id: Option<String>) -> Result<Arc<WrappedOSConfig>, WrappedError> {
    let _ = base64_key;
    Err(WrappedError::GenericError {
        msg: "Hardware key support not available in this build. \
              On macOS, use the Apple ID login flow instead (which uses native validation). \
              To enable hardware key support, rebuild with: cargo build --features hardware-key".into(),
    })
}

#[uniffi::export(async_runtime = "tokio")]
pub async fn connect(
    config: &WrappedOSConfig,
    state: &WrappedAPSState,
) -> Arc<WrappedAPSConnection> {
    ensure_crypto_provider();
    let config = config.config.clone();
    let state = state.inner.clone();
    let (connection, error) = APSConnectionResource::new(config, state).await;
    if let Some(error) = error {
        error!("APS connection error (non-fatal, will retry): {}", error);
    }
    Arc::new(WrappedAPSConnection { inner: connection })
}

/// Login session object that holds state between login steps.
#[derive(uniffi::Object)]
pub struct LoginSession {
    account: tokio::sync::Mutex<Option<AppleAccount<BridgeDefaultAnisetteProvider>>>,
    username: String,
    password_hash: Vec<u8>,
    needs_2fa: bool,
}

#[uniffi::export(async_runtime = "tokio")]
pub async fn login_start(
    apple_id: String,
    password: String,
    config: &WrappedOSConfig,
    connection: &WrappedAPSConnection,
) -> Result<Arc<LoginSession>, WrappedError> {
    ensure_crypto_provider();
    let os_config = config.config.clone();
    let conn = connection.inner.clone();

    let user_trimmed = apple_id.trim().to_string();
    // Apple's GSA SRP expects the password to be pre-hashed with SHA-256.
    // See upstream test.rs: sha256(password.as_bytes())
    let pw_bytes = {
        use sha2::{Sha256, Digest};
        let mut hasher = Sha256::new();
        hasher.update(password.trim().as_bytes());
        hasher.finalize().to_vec()
    };

    let client_info = os_config.get_gsa_config(&*conn.state.read().await, false);
    info!("login_start: mme_client_info={}", client_info.mme_client_info);
    info!("login_start: mme_client_info_akd={}", client_info.mme_client_info_akd);
    info!("login_start: akd_user_agent={}", client_info.akd_user_agent);
    info!("login_start: hardware_headers={:?}", client_info.hardware_headers);
    info!("login_start: push_token={:?}", client_info.push_token);
    let anisette_state_path = PathBuf::from_str("state/anisette").unwrap();
    let state_plist = anisette_state_path.join("state.plist");
    info!("login_start: anisette state path={:?} exists={}", state_plist, state_plist.exists());

    let anisette = bridge_default_provider(client_info.clone(), anisette_state_path);

    let mut account = AppleAccount::new_with_anisette(client_info, anisette)
        .map_err(|e| WrappedError::GenericError { msg: format!("Failed to create account: {}", e) })?;

    info!("login_start: calling login_email_pass for {}", user_trimmed);
    let result = account.login_email_pass(&user_trimmed, &pw_bytes).await
        .map_err(|e| WrappedError::GenericError { msg: format!("Login failed: {}", e) })?;
    info!("login_start: login_email_pass returned ok");

    info!("login_email_pass returned: {:?}", result);
    let needs_2fa = match result {
        icloud_auth::LoginState::LoggedIn => {
            info!("Login completed without 2FA");
            false
        }
        icloud_auth::LoginState::Needs2FAVerification => {
            info!("2FA required (Needs2FAVerification — push already sent by Apple)");
            true
        }
        icloud_auth::LoginState::NeedsDevice2FA | icloud_auth::LoginState::NeedsSMS2FA => {
            info!("2FA required — sending trusted device push");
            match account.send_2fa_to_devices().await {
                Ok(_) => info!("send_2fa_to_devices succeeded"),
                Err(e) => error!("send_2fa_to_devices failed: {}", e),
            }
            true
        }
        icloud_auth::LoginState::NeedsSMS2FAVerification(_) => {
            info!("2FA required (NeedsSMS2FAVerification — SMS already sent)");
            true
        }
        icloud_auth::LoginState::NeedsExtraStep(ref step) => {
            if account.get_pet().is_some() {
                info!("Login completed (extra step ignored, PET available)");
                false
            } else {
                return Err(WrappedError::GenericError { msg: format!("Login requires extra step: {}", step) });
            }
        }
        icloud_auth::LoginState::NeedsLogin => {
            return Err(WrappedError::GenericError { msg: "Login failed - bad credentials".to_string() });
        }
    };

    Ok(Arc::new(LoginSession {
        account: tokio::sync::Mutex::new(Some(account)),
        username: user_trimmed,
        password_hash: pw_bytes,
        needs_2fa,
    }))
}

#[uniffi::export(async_runtime = "tokio")]
impl LoginSession {
    pub fn needs_2fa(&self) -> bool {
        self.needs_2fa
    }

    pub async fn submit_2fa(&self, code: String) -> Result<bool, WrappedError> {
        let mut guard = self.account.lock().await;
        let account = guard.as_mut().ok_or(WrappedError::GenericError { msg: "No active session".to_string() })?;

        info!("Verifying 2FA code via trusted device endpoint (verify_2fa)");
        let result = account.verify_2fa(code).await
            .map_err(|e| WrappedError::GenericError { msg: format!("2FA verification failed: {}", e) })?;

        info!("2FA verification returned: {:?}", result);
        info!("PET token available: {}", account.get_pet().is_some());

        match result {
            icloud_auth::LoginState::LoggedIn => Ok(true),
            icloud_auth::LoginState::NeedsExtraStep(_) => {
                Ok(account.get_pet().is_some())
            }
            _ => Ok(false),
        }
    }

    pub async fn finish(
        &self,
        config: &WrappedOSConfig,
        connection: &WrappedAPSConnection,
        existing_identity: Option<Arc<WrappedIDSNGMIdentity>>,
        existing_users: Option<Arc<WrappedIDSUsers>>,
    ) -> Result<IDSUsersWithIdentityRecord, WrappedError> {
        let os_config = config.config.clone();
        let conn = connection.inner.clone();

        let mut guard = self.account.lock().await;
        let account = guard.as_mut().ok_or(WrappedError::GenericError { msg: "No active session".to_string() })?;

        let pet = account.get_pet()
            .ok_or(WrappedError::GenericError { msg: "No PET token available after login".to_string() })?;

        let spd = account.spd.as_ref().expect("No SPD after login");
        let adsid = spd.get("adsid").expect("No adsid").as_string().unwrap().to_string();
        let dsid = spd.get("DsPrsId").or_else(|| spd.get("dsid"))
            .and_then(|v| {
                if let Some(s) = v.as_string() {
                    Some(s.to_string())
                } else if let Some(i) = v.as_signed_integer() {
                    Some(i.to_string())
                } else if let Some(i) = v.as_unsigned_integer() {
                    Some(i.to_string())
                } else {
                    None
                }
            })
            .unwrap_or_default();

        // Build persist data before delegates call (while we have SPD access)
        let hashed_password_hex = account.hashed_password.as_ref()
            .map(|p| encode_hex(p))
            .unwrap_or_default();
        let mut spd_bytes = Vec::new();
        plist::to_writer_binary(&mut spd_bytes, spd)
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to serialize SPD: {}", e) })?;
        let spd_base64 = base64_encode(&spd_bytes);

        let account_persist = AccountPersistData {
            username: self.username.clone(),
            hashed_password_hex,
            pet: pet.clone(),
            adsid: adsid.clone(),
            dsid: dsid.clone(),
            spd_base64,
        };

        // Request both IDS (for messaging) and MobileMe (for contacts CardDAV URL)
        let delegates = login_apple_delegates(
            &*account,
            None,
            &*os_config,
            &[LoginDelegate::IDS, LoginDelegate::MobileMe],
        ).await.map_err(|e| WrappedError::GenericError { msg: format!("Failed to get delegates: {}", e) })?;

        let ids_delegate = delegates.ids.ok_or(WrappedError::GenericError { msg: "No IDS delegate in response".to_string() })?;
        let fresh_user = authenticate_apple(ids_delegate, &*os_config).await
            .map_err(|e| WrappedError::GenericError { msg: format!("IDS authentication failed: {}", e) })?;

        // Resolve identity: reuse existing or generate new
        let identity = match existing_identity {
            Some(wrapped) => {
                info!("Reusing existing identity (avoiding new device notification)");
                wrapped.inner.clone()
            }
            None => {
                info!("Generating new identity (first login)");
                IDSNGMIdentity::new()
                    .map_err(|e| WrappedError::GenericError {
                        msg: format!("Failed to create identity: {}", e)
                    })?
            }
        };

        // Decide whether to reuse existing registration or register fresh.
        let users = match existing_users {
            Some(ref wrapped) if !wrapped.inner.is_empty() => {
                let has_valid_registration = wrapped.inner[0]
                    .registration.get(MADRID_SERVICE.name)
                    .map(|r| r.calculate_rereg_time_s().map(|t| t > 0).unwrap_or(false))
                    .unwrap_or(false);
                let has_required_services = wrapped.inner[0].registration.contains_key(MADRID_SERVICE.name)
                    && wrapped.inner[0].registration.contains_key(MULTIPLEX_SERVICE.name)
                    && wrapped.inner[0].registration.contains_key(FACETIME_SERVICE.name)
                    && wrapped.inner[0].registration.contains_key(VIDEO_SERVICE.name);
                // Also validate MULTIPLEX cert isn't expired and data_hash
                // (sub_services + client_data) hasn't changed since registration.
                // A stale MULTIPLEX registration makes the bridge invisible to
                // contacts querying IDS for keysharing sub-service targets.
                let multiplex_valid = wrapped.inner[0]
                    .registration.get(MULTIPLEX_SERVICE.name)
                    .map(|r| {
                        let not_expired = r.calculate_rereg_time_s().map(|t| t > 0).unwrap_or(false);
                        let hash_matches = r.data_hash == MULTIPLEX_SERVICE.hash_data();
                        if !not_expired { info!("MULTIPLEX registration expired — will re-register"); }
                        if !hash_matches { info!("MULTIPLEX data_hash changed (sub_services or client_data updated) — will re-register"); }
                        not_expired && hash_matches
                    })
                    .unwrap_or(false);

                if has_valid_registration && has_required_services && multiplex_valid {
                    info!("Reusing existing registration (still valid for iMessage services, skipping register endpoint)");
                    let mut existing = wrapped.inner.clone();
                    existing[0].auth_keypair = fresh_user.auth_keypair.clone();
                    existing
                } else {
                    info!(
                        "Existing registration missing required services or expired, must re-register with full 4-service bundle"
                    );
                    let mut users = vec![fresh_user];
                    // Match OB-Android: single register() call with all four
                    // services (MADRID, MULTIPLEX, FACETIME, VIDEO). Peer iOS
                    // gates unprompted StatusKit reshare on seeing a 4-service
                    // IDS identity.
                    register(
                        &*os_config,
                        &*conn.state.read().await,
                        &[&MADRID_SERVICE, &MULTIPLEX_SERVICE, &FACETIME_SERVICE, &VIDEO_SERVICE],
                        &mut users,
                        &identity,
                    ).await.map_err(|e| WrappedError::GenericError { msg: format!("4-service registration failed: {}", e) })?;
                    let registered: Vec<&str> = users[0].registration.keys().map(|s| s.as_str()).collect();
                    info!("Re-registration OK — services registered: [{}]", registered.join(", "));
                    users
                }
            }
            _ => {
                let mut users = vec![fresh_user];
                if users[0].registration.is_empty() {
                    info!("Registering identity (first login) with full 4-service bundle...");
                    register(
                        &*os_config,
                        &*conn.state.read().await,
                        &[&MADRID_SERVICE, &MULTIPLEX_SERVICE, &FACETIME_SERVICE, &VIDEO_SERVICE],
                        &mut users,
                        &identity,
                    ).await.map_err(|e| WrappedError::GenericError { msg: format!("Registration failed: {}", e) })?;
                    let registered: Vec<&str> = users[0].registration.keys().map(|s| s.as_str()).collect();
                    info!("First-login registration OK — services registered: [{}]", registered.join(", "));
                }
                users
            }
        };

        // Take ownership of the account to create a TokenProvider.
        // The MobileMe delegate from `delegates` is seeded into the WrappedTokenProvider
        // so the first get_contacts_url() / create_keychain_clients() call doesn't
        // need to re-fetch.
        let owned_account = guard.take()
            .ok_or(WrappedError::GenericError { msg: "Account already consumed".to_string() })?;
        let account_arc = Arc::new(rustpush::DebugMutex::new(owned_account));
        let token_provider = TokenProvider::new(account_arc.clone(), os_config.clone());

        // Store the MobileMe delegate in the WrappedTokenProvider as opaque plist
        // bytes. Upstream `MobileMeDelegateResponse` is not `Serialize`, so we
        // manually build a `plist::Value` that matches its serde representation
        // (`{"tokens": {...}, "com.apple.mobileme": {...}}`). We can access the
        // public fields of `delegates.mobileme` via field syntax without naming
        // the type — Rust field access on expressions of unnameable types is
        // permitted. The parse_mme_delegate() helper deserializes back into
        // MobileMeDelegateResponse at call sites where a typed value is needed.
        let mme_delegate_bytes = if let Some(mobileme) = &delegates.mobileme {
            let mut tokens_dict = plist::Dictionary::new();
            for (k, v) in &mobileme.tokens {
                tokens_dict.insert(k.clone(), plist::Value::String(v.clone()));
            }
            let mut root = plist::Dictionary::new();
            root.insert("tokens".to_string(), plist::Value::Dictionary(tokens_dict));
            root.insert(
                "com.apple.mobileme".to_string(),
                plist::Value::Dictionary(mobileme.config.clone()),
            );
            let mut buf = Vec::new();
            plist::Value::Dictionary(root).to_writer_xml(&mut buf)
                .map_err(|e| WrappedError::GenericError {
                    msg: format!("Failed to serialize MobileMe delegate: {}", e),
                })?;
            Some(buf)
        } else {
            None
        };

        Ok(IDSUsersWithIdentityRecord {
            users: Arc::new(WrappedIDSUsers { inner: users }),
            identity: Arc::new(WrappedIDSNGMIdentity { inner: identity }),
            token_provider: Some(Arc::new(WrappedTokenProvider {
                inner: token_provider,
                account: account_arc,
                os_config: os_config.clone(),
                mme_delegate_bytes: tokio::sync::Mutex::new(mme_delegate_bytes),
                keychain_clients_cache: tokio::sync::Mutex::new(None),
            })),
            account_persist: Some(account_persist),
        })
    }
}

// ============================================================================
// Attachment download helper
// ============================================================================

/// Fetch an iMessage MMCS `AuthorizeGetResponse` body via the APNs auth
/// dance, without invoking upstream's `MMCSFile::get_attachment` (which
/// calls the panic-prone `get_mmcs`).
///
/// This is the same protocol upstream rustpush uses at
/// `third_party/rustpush-upstream/src/imessage/messages.rs` in
/// `MMCSFile::get_attachment` up through line 1381, inlined into the
/// wrapper so we can stop at the point where upstream would hand off to
/// `get_mmcs` and instead route the bytes through
/// `manual_ford::manual_ford_download_asset`.
///
/// Returns the raw response bytes — the caller decodes them as an
/// `mmcsp::AuthorizeGetResponse` and runs them through the manual
/// download.
async fn fetch_imessage_mmcs_authorize_body(
    mmcs: &rustpush::MMCSFile,
    conn: &rustpush::APSConnectionResource,
) -> Result<Vec<u8>, String> {
    use futures::FutureExt;
    use plist::Value;
    use rand::Rng;
    use serde::{Deserialize, Serialize};

    #[derive(Serialize, Deserialize)]
    struct RequestMMCSDownload {
        #[serde(rename = "mO")]
        object: String,
        #[serde(rename = "mS")]
        signature: plist::Data,
        v: u64,
        ua: String,
        c: u64,
        i: u32,
        #[serde(rename = "cH")]
        headers: String,
        #[serde(rename = "mR")]
        domain: String,
        #[serde(rename = "cV")]
        cv: u32,
    }

    #[derive(Serialize, Deserialize)]
    struct MMCSDownloadResponse {
        #[serde(rename = "cB")]
        response: plist::Data,
        #[serde(rename = "mU")]
        #[allow(dead_code)]
        object: String,
    }

    // User agents / client info strings — must exactly match upstream's
    // `MMCSFile::get_attachment` construction so the MMCS server accepts
    // our auth request.
    let mme_client_info = conn
        .os_config
        .get_mme_clientinfo("com.apple.icloud.content/1950.19 (com.apple.Messenger/1.0)");
    let mini_ua = conn.os_config.get_version_ua();

    // Strip the last `/{object}` segment from the URL to produce the
    // MMCS domain — upstream does this to build the `mR` field.
    let domain = mmcs.url.replace(&format!("/{}", &mmcs.object), "");

    let msg_id: u32 = rand::thread_rng().gen();
    let header = format!("x-mme-client-info:{}", mme_client_info);
    let request_download = RequestMMCSDownload {
        object: mmcs.object.to_string(),
        c: 151,
        ua: mini_ua,
        headers: [
            "x-apple-mmcs-proto-version:5.0",
            "x-apple-mmcs-plist-sha256:fvj0Y/Ybu1pq0r4NxXw3eP51exujUkEAd7LllbkTdK8=",
            "x-apple-mmcs-plist-version:v1.0",
            &header,
            "",
        ]
        .join("\n"),
        v: 8,
        domain,
        cv: 2,
        i: msg_id,
        signature: mmcs.signature.to_vec().into(),
    };

    // Subscribe BEFORE send to avoid the race where the response arrives
    // before the receiver is registered.
    let recv = conn.subscribe().await;

    // Upstream's send_message now takes `impl Serialize` and handles plist
    // encoding internally (was: pre-serialized Vec<u8> + plist_to_bin at the
    // call site). The id parameter is also i32 now, not u32.
    conn.send_message("com.apple.madrid", request_download, Some(msg_id as i32))
        .await
        .map_err(|e| format!("APNs send_message: {e}"))?;

    // Wait for a Notification on com.apple.madrid where `c == 151` and
    // `i == msg_id`. Topic filtering is done by sha1-hash compare, which
    // we can't easily replicate without `rustpush::util::sha1`, so we
    // check the parsed payload fields directly and trust APNs routing
    // to only deliver com.apple.madrid messages on this subscription.
    let predicate = move |msg: rustpush::APSMessage| -> Option<Value> {
        // Upstream APSPackedValue::Into<Value> maps each packed-attribute
        // kind to its corresponding plist::Value variant (aps.rs:31-42),
        // so MMCS auth responses with a Dict-encoded payload arrive as
        // Value::Dictionary, while Data-encoded payloads arrive as
        // Value::Data carrying raw plist bytes. Accept both — matches
        // upstream's get_message helper, which doesn't constrain the
        // variant.
        let rustpush::APSMessage::Notification { payload, .. } = msg else { return None };
        let parsed = match payload {
            plist::Value::Data(bytes) => plist::from_bytes::<Value>(&bytes).ok()?,
            v => v,
        };
        let dict = parsed.as_dictionary()?;
        let c = dict.get("c")?.as_unsigned_integer()?;
        let i_val = dict.get("i")?;
        let i = i_val
            .as_unsigned_integer()
            .map(|v| v as u32)
            .or_else(|| i_val.as_signed_integer().map(|v| v as u32))?;
        if c == 151 && i == msg_id {
            Some(parsed)
        } else {
            None
        }
    };

    // The `wait_for_timeout` future can in principle panic on malformed
    // APNs messages (rustpush's aps internals have `unwrap`s). Wrap in
    // catch_unwind so a panic here returns an Err instead of crashing
    // the bridge.
    let wait_fut = conn.wait_for_timeout(recv, predicate);
    let reader = match std::panic::AssertUnwindSafe(wait_fut).catch_unwind().await {
        Ok(Ok(v)) => v,
        Ok(Err(e)) => return Err(format!("APNs wait_for_timeout: {e}")),
        Err(_) => return Err("APNs wait_for_timeout panicked".to_string()),
    };

    let apns_response: MMCSDownloadResponse =
        plist::from_value(&reader).map_err(|e| format!("plist decode response: {e}"))?;
    let body: Vec<u8> = apns_response.response.into();
    Ok(body)
}

/// Download any MMCS (non-inline) attachments from the message and convert them
/// to inline data in the wrapped message, so the Go side can upload them to Matrix.
///
/// This function reimplements upstream's `MMCSFile::get_attachment` at
/// the wrapper layer so we can use our V1+V2-capable
/// `manual_ford_download_asset` instead of upstream's panicking
/// `get_mmcs`. Master worked because Cameron's rustpush fork had the
/// 94f7b8e Ford fix baked into `get_mmcs`; after the refactor's source
/// swap to OpenBubbles upstream (which doesn't have the fix), calling
/// `att.get_attachment` on V2-Ford or dedup'd records panics. Routing
/// the bytes through the manual path gets us back to master's behavior
/// without patching rustpush.
async fn download_mmcs_attachments(
    wrapped: &mut WrappedMessage,
    msg_inst: &MessageInst,
    conn: &rustpush::APSConnectionResource,
) {
    if let Message::Message(normal) = &msg_inst.message {
        let mut att_idx = 0;
        for indexed_part in &normal.parts.0 {
            if let MessagePart::Attachment(att) = &indexed_part.part {
                if let AttachmentType::MMCS(mmcs) = &att.a_type {
                    if att_idx < wrapped.attachments.len() {
                        match download_one_mmcs_attachment(mmcs, conn, &att.name).await {
                            Ok(buf) => {
                                info!(
                                    "Downloaded MMCS attachment: {} ({} bytes)",
                                    att.name,
                                    buf.len()
                                );
                                wrapped.attachments[att_idx].is_inline = true;
                                wrapped.attachments[att_idx].inline_data = Some(buf);
                            }
                            Err(e) => {
                                error!(
                                    "Failed to download MMCS attachment {}: {}",
                                    att.name, e
                                );
                            }
                        }
                    }
                }
                att_idx += 1;
            }
        }
    }
}

/// Download one MMCS attachment via the wrapper-level path, with bounded
/// retries on transient failures.
///
/// Upstream's `send_message` (aps.rs:1319) schedules a connection reload via
/// `do_reload()` whenever its 15-second ack wait times out (aps.rs:1349). The
/// caller is expected to retry so the reloaded connection can serve the next
/// attempt — not retrying is what made the first transient APNs blip fatal.
///
/// Retries: 3 attempts total with 500 ms / 1 s / 2 s backoff between them.
/// Only genuinely transient errors retry; Ford SIV mismatches, chunk OOB, and
/// decode errors return immediately.
async fn download_one_mmcs_attachment(
    mmcs: &rustpush::MMCSFile,
    conn: &rustpush::APSConnectionResource,
    name: &str,
) -> Result<Vec<u8>, String> {
    const MAX_ATTEMPTS: u32 = 3;
    let mut last_err = String::new();
    for attempt in 1..=MAX_ATTEMPTS {
        match download_one_mmcs_attachment_once(mmcs, conn, name).await {
            Ok(bytes) => {
                if attempt > 1 {
                    info!(
                        "MMCS download succeeded for {} on attempt {}/{}",
                        name, attempt, MAX_ATTEMPTS
                    );
                }
                return Ok(bytes);
            }
            Err(e) => {
                let transient = e.contains("Send timeout")
                    || e.contains("try again")
                    || e.contains("APNs wait_for_timeout")
                    || e.contains("connection")
                    || e.contains("container fetch")
                    || e.contains("HTTP");
                if !transient || attempt == MAX_ATTEMPTS {
                    return Err(e);
                }
                warn!(
                    "MMCS download attempt {}/{} for {} failed: {} — retrying",
                    attempt, MAX_ATTEMPTS, name, e
                );
                last_err = e;
                // Backoff 500 ms, 1 s, 2 s — gives upstream's do_reload()
                // time to re-establish the APS connection between tries.
                let backoff_ms = 500u64 * (1u64 << (attempt - 1));
                tokio::time::sleep(std::time::Duration::from_millis(backoff_ms)).await;
            }
        }
    }
    Err(format!(
        "MMCS download exhausted {} attempts: {}",
        MAX_ATTEMPTS, last_err
    ))
}

/// One attempt of the MMCS download pipeline: APNs auth handshake → manual
/// V1+V2 Ford-capable chunk decode → iMessage outer AES-256-CTR unwrap.
///
/// Equivalent to upstream's `MMCSFile::get_attachment` but bypasses the
/// panicking `get_mmcs` call at its tail.
///
/// iMessage MMCS differs from CloudKit MMCS in one important way: the file
/// bytes are additionally wrapped in an `IMessageContainer` layer (AES-256-CTR
/// with `MMCSFile.key` and a zero nonce). Upstream handles this by passing a
/// `WriteContainer` impl through to `get_mmcs` that decrypts during chunk
/// writes. Since our manual path returns the assembled chunk plaintext without
/// that hook, we apply the outer unwrap here as a post-processing step.
async fn download_one_mmcs_attachment_once(
    mmcs: &rustpush::MMCSFile,
    conn: &rustpush::APSConnectionResource,
    name: &str,
) -> Result<Vec<u8>, String> {
    // Step 1: APNs auth handshake → AuthorizeGetResponse body bytes.
    let body = fetch_imessage_mmcs_authorize_body(mmcs, conn).await?;

    // Step 2: decrypt via the same V1+V2-capable manual path used by
    // CloudKit downloads. Pass an empty Ford key because iMessage MMCS
    // chunk encryption uses the per-chunk `meta.encryption_key` field
    // (V1 AES-128-CFB) — there's no outer Ford SIV layer on the iMessage
    // side.
    let user_agent = conn.os_config.get_normal_ua("IMTransferAgent/1000");
    let chunked_plaintext = manual_ford::manual_ford_download_asset(
        &body,
        &mmcs.signature,
        &[],
        &user_agent,
        name,
    )
    .await?;

    // Step 3: iMessage outer unwrap — AES-256-CTR(mmcs.key, zero_iv)
    // over the full assembled plaintext. This mirrors what upstream's
    // `IMessageContainer` does on the write path during `get_mmcs`.
    // Matches `third_party/rustpush-upstream/src/imessage/messages.rs`
    // `IMessageContainer::new(&self.key, writer, true)` at line 1341.
    if mmcs.key.len() != 32 {
        return Err(format!(
            "iMessage MMCS key has unexpected length {}: expected 32 for AES-256-CTR",
            mmcs.key.len()
        ));
    }
    let iv = [0u8; 16];
    let unwrapped = openssl::symm::decrypt(
        openssl::symm::Cipher::aes_256_ctr(),
        &mmcs.key,
        Some(&iv),
        &chunked_plaintext,
    )
    .map_err(|e| format!("iMessage outer AES-256-CTR unwrap: {e}"))?;

    Ok(unwrapped)
}

/// Download the group photo for an IconChange message via MMCS.
/// Apple delivers group photo changes as MMCS file transfers inside IconChange
/// APNs messages — NOT via CloudKit. This downloads the photo inline so the
/// Go side can use msg.icon_change_photo_data directly without any CloudKit
/// round-trip (which would fail for Apple-set photos anyway).
async fn download_icon_change_photo(
    wrapped: &mut WrappedMessage,
    msg_inst: &MessageInst,
    conn: &rustpush::APSConnectionResource,
) {
    if let Message::IconChange(change) = &msg_inst.message {
        if let Some(mmcs_file) = &change.file {
            let att = Attachment {
                a_type: AttachmentType::MMCS(mmcs_file.clone()),
                part: 0,
                uti_type: String::new(),
                mime: String::from("image/jpeg"),
                name: String::from("group_photo"),
                iris: false,
            };
            let mut buf: Vec<u8> = Vec::new();
            match att.get_attachment(conn, &mut buf, |_, _| {}).await {
                Ok(()) => {
                    info!("Downloaded group photo via MMCS ({} bytes)", buf.len());
                    wrapped.icon_change_photo_data = Some(buf);
                }
                Err(e) => {
                    error!("Failed to download group photo via MMCS: {}", e);
                }
            }
        }
    }
}

// ============================================================================
// Client
// ============================================================================

#[derive(uniffi::Object)]
pub struct WrappedFindMyClient {
    inner: Arc<rustpush::findmy::FindMyClient<BridgeDefaultAnisetteProvider>>,
}

#[uniffi::export(async_runtime = "tokio")]
impl WrappedFindMyClient {
    pub async fn export_state_json(&self) -> Result<String, WrappedError> {
        let state = self.inner.state.state.lock().await;
        serialize_state_json(&*state)
    }

    pub async fn sync_item_positions(&self) -> Result<(), WrappedError> {
        self.inner.sync_item_positions().await?;
        Ok(())
    }

    pub async fn accept_item_share(&self, circle_id: String) -> Result<(), WrappedError> {
        self.inner.accept_item_share(&circle_id).await?;
        Ok(())
    }

    pub async fn update_beacon_name(
        &self,
        associated_beacon: String,
        role_id: i64,
        name: String,
        emoji: String,
    ) -> Result<(), WrappedError> {
        let record = rustpush::findmy::BeaconNamingRecord {
            associated_beacon,
            role_id,
            name,
            emoji,
        };
        self.inner.update_beacon_name(&record).await?;
        Ok(())
    }

    pub async fn delete_shared_item(&self, id: String, remove_beacon: bool) -> Result<(), WrappedError> {
        self.inner.delete_shared_item(&id, remove_beacon).await?;
        Ok(())
    }
}

#[derive(uniffi::Object)]
pub struct WrappedFaceTimeClient {
    inner: Arc<rustpush::facetime::FTClient>,
}

#[uniffi::export(async_runtime = "tokio")]
impl WrappedFaceTimeClient {
    pub async fn export_state_json(&self) -> Result<String, WrappedError> {
        let state = self.inner.state.read().await;
        serialize_state_json(&*state)
    }

    pub async fn use_link_for(&self, old_usage: String, usage: String) -> Result<(), WrappedError> {
        self.inner.use_link_for(&old_usage, &usage).await?;
        Ok(())
    }

    pub async fn get_link_for_usage(&self, handle: String, usage: String) -> Result<String, WrappedError> {
        Ok(self.inner.get_link_for_usage(&handle, &usage).await?)
    }

    pub async fn clear_links(&self) -> Result<(), WrappedError> {
        self.inner.clear_links().await?;
        Ok(())
    }

    pub async fn delete_link(&self, pseud: String) -> Result<(), WrappedError> {
        self.inner.delete_link(&pseud).await?;
        Ok(())
    }

    pub async fn get_session_link(&self, guid: String) -> Result<String, WrappedError> {
        Ok(self.inner.get_session_link(&guid).await?)
    }

    // Deterministically binds the FTLink (matched by handle + usage) to a
    // session group_id by setting link.session_link. Without this, the
    // persistent "bridge" link has session_link=None until the first
    // letmein-tap, and auto_approve_bridge_letmein falls through to
    // member/ringing heuristics. Under cold-start or stale-state conditions
    // those heuristics miss and the approver creates a fresh empty session,
    // producing the "0 people" symptom.
    pub async fn bind_bridge_link_to_session(
        &self,
        handle: String,
        usage: String,
        group_id: String,
    ) -> Result<(), WrappedError> {
        let mut state = self.inner.state.write().await;
        let pseud = state
            .links
            .iter()
            .find(|(_, link)| link.handle == handle && link.usage.as_deref() == Some(&usage))
            .map(|(pseud, _)| pseud.clone())
            .ok_or_else(|| WrappedError::GenericError {
                msg: format!("No FaceTime link found for handle={} usage={}", handle, usage),
            })?;
        if let Some(link) = state.links.get_mut(&pseud) {
            link.session_link = Some(group_id);
        }
        Ok(())
    }

    pub async fn create_session(&self, group_id: String, handle: String, participants: Vec<String>) -> Result<(), WrappedError> {
        // Call upstream directly. We previously wrapped this with a
        // strip-own-from-session.members pattern to stop the wire ring from
        // fanning out to the owner's other Apple devices (Mac, iPad). The
        // motivation was tap-routing fragility: when the Mac auto-answered
        // via Continuity, it sent RespondedElsewhere back to the bridge,
        // which cleared is_ringing_inaccurate, which broke the
        // auto_approve_bridge_letmein ringing-group fallback for link taps.
        //
        // bind_bridge_link_to_session (added alongside this change) pins the
        // bridge FaceTime link's session_link to the outgoing session the
        // moment it's created, so the letmein approver's linked_group branch
        // matches deterministically regardless of is_ringing_inaccurate. The
        // strip's original justification is moot.
        //
        // Empirically the strip also correlated with the callee not ringing
        // (own was absent from update_context.members and fanout_groupmembers
        // in the Invitation wire, which we suspect Apple's FT routing
        // rejected). Straight upstream sends a well-formed Invitation. Side
        // effect: owner's devices ring too; caller dismisses on the device
        // they don't want. Future: suppress own-device ring via a follow-up
        // RespondedElsewhere once we've confirmed the callee ring is stable.
        self.inner
            .create_session(group_id, handle, &participants)
            .await
            .map_err(|e| WrappedError::GenericError {
                msg: format!("FTClient::create_session failed: {:?}", e),
            })?;
        Ok(())
    }

    // Create a FaceTime session without ringing any participant. Used by the
    // portal-room !im facetime flow: session is allocated + registered with
    // Apple's relay (so the join link is valid), but no Invitation wire is
    // fanned out. The contact's phone rings only when the caller taps the
    // link — that JoinEvent triggers maybe_fire_pending_ring which calls
    // ft.ring() with the queued targets.
    //
    // Difference from create_session:
    // - is_ringing_inaccurate starts false, so prop_up_conv's !ring branch
    //   doesn't divert into RespondedElsewhere (facetime.rs:708).
    // - prop_up_conv(session, false) so the wire message carries no
    //   ConversationMessageType::Invitation on any target (facetime.rs:759).
    //
    // Net effect on Apple's side: session allocated + propped (state=live)
    // but nobody's device rings.
    pub async fn create_session_no_ring(
        &self,
        group_id: String,
        handle: String,
        participants: Vec<String>,
    ) -> Result<(), WrappedError> {
        use rustpush::facetime::{FTMember, FTMode, FTSession};
        use std::collections::HashMap;

        let now_ms = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap_or_default()
            .as_millis() as u64;

        let members = participants
            .iter()
            .chain(std::iter::once(&handle))
            .map(|p| FTMember {
                nickname: None,
                handle: p.clone(),
            })
            .collect();

        let session = FTSession {
            group_id: group_id.clone(),
            my_handles: vec![handle.clone()],
            participants: HashMap::new(),
            link: None,
            members,
            report_id: uuid::Uuid::new_v4().to_string().to_uppercase(),
            start_time: Some(now_ms),
            last_rekey: None,
            is_propped: false,
            is_ringing_inaccurate: false,
            mode: Some(FTMode::Outgoing),
            recent_member_adds: HashMap::new(),
        };

        let mut state = self.inner.state.write().await;
        state.sessions.insert(group_id.clone(), session);
        let session = state.sessions.get_mut(&group_id).expect("just inserted");

        self.inner
            .ensure_allocations(session, &[])
            .await
            .map_err(|e| WrappedError::GenericError {
                msg: format!("ensure_allocations failed: {:?}", e),
            })?;
        self.inner
            .prop_up_conv(session, false)
            .await
            .map_err(|e| WrappedError::GenericError {
                msg: format!("prop_up_conv(ring=false) failed: {:?}", e),
            })?;
        Ok(())
    }

    pub async fn add_members(
        &self,
        session_id: String,
        handles: Vec<String>,
        letmein: bool,
        to_members: Option<Vec<String>>,
    ) -> Result<(), WrappedError> {
        let mut session = {
            let state = self.inner.state.read().await;
            state.sessions.get(&session_id).cloned().ok_or(WrappedError::GenericError {
                msg: format!("FaceTime session not found: {}", session_id),
            })?
        };

        let members = handles
            .into_iter()
            .map(|handle| rustpush::facetime::FTMember {
                nickname: None,
                handle,
            })
            .collect::<Vec<_>>();

        self.inner.add_members(&mut session, members, letmein, to_members).await?;

        let mut state = self.inner.state.write().await;
        state.sessions.insert(session_id, session);
        Ok(())
    }

    pub async fn remove_members(&self, session_id: String, handles: Vec<String>) -> Result<(), WrappedError> {
        let mut session = {
            let state = self.inner.state.read().await;
            state.sessions.get(&session_id).cloned().ok_or(WrappedError::GenericError {
                msg: format!("FaceTime session not found: {}", session_id),
            })?
        };

        let members = handles
            .into_iter()
            .map(|handle| rustpush::facetime::FTMember {
                nickname: None,
                handle,
            })
            .collect::<Vec<_>>();

        self.inner.remove_members(&mut session, members).await?;

        let mut state = self.inner.state.write().await;
        state.sessions.insert(session_id, session);
        Ok(())
    }

    pub async fn ring(&self, session_id: String, targets: Vec<String>, letmein: bool) -> Result<(), WrappedError> {
        let session = {
            let state = self.inner.state.read().await;
            state.sessions.get(&session_id).cloned().ok_or(WrappedError::GenericError {
                msg: format!("FaceTime session not found: {}", session_id),
            })?
        };
        self.inner.ring(&session, &targets, letmein).await?;
        Ok(())
    }

    // Queue a ring that fires as soon as a participant *other than the
    // caller* joins this session. The portal !facetime command uses this so
    // the contact's phone doesn't ring until the caller has actually tapped
    // the join link. caller_handle is the session creator's own handle;
    // join events from that handle are ignored so the implicit self-join
    // from create_session does not fire the ring immediately. Entries
    // self-expire after ttl_secs to avoid orphan rings.
    pub async fn register_pending_ring(
        &self,
        session_id: String,
        caller_handle: String,
        targets: Vec<String>,
        ttl_secs: u64,
    ) -> Result<(), WrappedError> {
        let mut map = pending_ft_rings().lock().await;
        map.insert(session_id, PendingFTRing {
            caller_handle,
            targets,
            expires_at: std::time::Instant::now() + std::time::Duration::from_secs(ttl_secs),
        });
        Ok(())
    }

    pub async fn list_delegated_letmein_requests(&self) -> Vec<WrappedLetMeInRequest> {
        let delegated = self.inner.delegated_requests.lock().await;
        delegated
            .iter()
            .map(|(uuid, request)| WrappedLetMeInRequest {
                delegation_uuid: uuid.clone(),
                pseud: request.pseud.clone(),
                requestor: request.requestor.clone(),
                nickname: request.nickname.clone(),
                usage: request.usage.clone(),
            })
            .collect()
    }

    pub async fn respond_delegated_letmein(
        &self,
        delegation_uuid: String,
        approved_group: Option<String>,
    ) -> Result<(), WrappedError> {
        let request = {
            let delegated = self.inner.delegated_requests.lock().await;
            delegated.get(&delegation_uuid).cloned().ok_or(WrappedError::GenericError {
                msg: format!("Delegated LetMeIn request not found: {}", delegation_uuid),
            })?
        };
        self.inner.respond_letmein(request, approved_group.as_deref()).await?;
        Ok(())
    }
}

#[derive(uniffi::Object)]
pub struct WrappedPasswordsClient {
    inner: Arc<rustpush::passwords::PasswordManager<BridgeDefaultAnisetteProvider>>,
}

#[uniffi::export(async_runtime = "tokio")]
impl WrappedPasswordsClient {
    pub async fn export_state_json(&self) -> Result<String, WrappedError> {
        let state = self.inner.state.read().await;
        serialize_state_json(&*state)
    }

    pub async fn sync_passwords(&self) -> Result<(), WrappedError> {
        self.inner.sync_passwords(&self.inner.conn).await?;
        Ok(())
    }

    pub async fn accept_invite(&self, invite_id: String) -> Result<(), WrappedError> {
        self.inner.accept_invite(&invite_id).await?;
        Ok(())
    }

    pub async fn decline_invite(&self, invite_id: String) -> Result<(), WrappedError> {
        self.inner.decline_invite(&invite_id).await?;
        Ok(())
    }

    pub async fn query_handle(&self, handle: String) -> Result<bool, WrappedError> {
        Ok(self.inner.query_handle(&handle).await?)
    }

    pub async fn create_group(&self, name: String) -> Result<String, WrappedError> {
        Ok(self.inner.create_group(&name).await?)
    }

    pub async fn rename_group(&self, id: String, new_name: String) -> Result<(), WrappedError> {
        self.inner.rename_group(&id, &new_name).await?;
        Ok(())
    }

    pub async fn remove_group(&self, id: String) -> Result<(), WrappedError> {
        self.inner.remove_group(&id).await?;
        Ok(())
    }

    pub async fn invite_user(&self, group_id: String, handle: String) -> Result<(), WrappedError> {
        self.inner.invite_user(&group_id, &handle).await?;
        Ok(())
    }

    pub async fn remove_user(&self, group_id: String, handle: String) -> Result<(), WrappedError> {
        self.inner.remove_user(&group_id, &handle).await?;
        Ok(())
    }

    pub async fn list_password_raw_entry_refs(&self) -> Vec<WrappedPasswordEntryRef> {
        self.inner
            .get_password_entries::<rustpush::passwords::PasswordRawEntry>()
            .await
            .into_iter()
            .map(|(id, (group, _))| WrappedPasswordEntryRef { id, group })
            .collect()
    }

    pub async fn get_password_site_counts(&self, site: String) -> WrappedPasswordSiteCounts {
        let cfg = self.inner.get_password_for_site(site).await;
        WrappedPasswordSiteCounts {
            website_meta_count: if cfg.website_meta.is_some() { 1 } else { 0 },
            password_count: cfg.passwords.len() as u64,
            password_meta_count: cfg.passwords_meta.len() as u64,
            passkey_count: cfg.passkeys.len() as u64,
        }
    }

    pub async fn upsert_password_raw_entry(
        &self,
        id: String,
        site: String,
        account: String,
        secret_data: Vec<u8>,
        group: Option<String>,
    ) -> Result<(), WrappedError> {
        let now_ms = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_millis() as u64)
            .unwrap_or(0);
        let entry = rustpush::passwords::PasswordRawEntry {
            cdat: now_ms,
            mdat: now_ms,
            srvr: site,
            acct: account,
            agrp: "com.apple.cfnetwork".to_string(),
            data: secret_data,
        };
        self.inner
            .insert_password_entry::<rustpush::passwords::PasswordRawEntry>(&id, &entry, group)
            .await?;
        Ok(())
    }

    pub async fn delete_password_raw_entry(&self, id: String, group: Option<String>) -> Result<(), WrappedError> {
        self.inner
            .delete_password_entry::<rustpush::passwords::PasswordRawEntry>(&id, group)
            .await?;
        Ok(())
    }
}

#[derive(uniffi::Object)]
pub struct WrappedStatusKitClient {
    inner: Arc<rustpush::statuskit::StatusKitClient<BridgeDefaultAnisetteProvider>>,
    interests: tokio::sync::Mutex<Vec<rustpush::statuskit::ChannelInterestToken>>,
}

#[uniffi::export(async_runtime = "tokio")]
impl WrappedStatusKitClient {
    pub async fn export_state_json(&self) -> Result<String, WrappedError> {
        let state = self.inner.state.read().await;
        serialize_state_json(&*state)
    }

    pub async fn roll_keys(&self) {
        self.inner.roll_keys().await;
    }

    pub async fn reset_keys(&self) {
        self.inner.reset_keys().await;
    }

    pub async fn share_status(&self, active: bool, mode: Option<String>) -> Result<(), WrappedError> {
        let status = if active {
            rustpush::statuskit::StatusKitStatus::new_active()
        } else {
            rustpush::statuskit::StatusKitStatus::new_away(mode.ok_or(WrappedError::GenericError {
                msg: "Mode is required when sharing away status".into(),
            })?)
        };
        self.inner.share_status(&status).await?;
        Ok(())
    }

    pub async fn invite_to_channel(
        &self,
        sender_handle: String,
        handles: Vec<WrappedStatusKitInviteHandle>,
    ) -> Result<(), WrappedError> {
        let mapped = handles
            .into_iter()
            .map(|h| {
                (
                    h.handle,
                    rustpush::statuskit::StatusKitPersonalConfig {
                        allowed_modes: h.allowed_modes,
                    },
                )
            })
            .collect::<HashMap<_, _>>();
        self.inner.invite_to_channel(&sender_handle, mapped).await?;
        Ok(())
    }

    pub async fn request_handles(&self, handles: Vec<String>) {
        let token = self.inner.request_handles(&handles).await;
        self.interests.lock().await.push(token);
    }

    pub async fn clear_interest_tokens(&self) {
        self.interests.lock().await.clear();
    }

    /// Returns contact handles with a confirmed-live StatusKit channel — i.e.
    /// at least one channel for that peer has delivered a real status message
    /// (recent_channels.last_msg_ns > 1). A peer with only placeholder
    /// channels (last_msg_ns == 1) is NOT returned here, because that state
    /// typically indicates the ratchet has drifted: the peer rotated to a
    /// channel id the bridge hasn't discovered, and the stored channels
    /// will never carry a status. Excluding them lets the Go-side re-invite
    /// loop re-key that peer on its next tick (bounded by the per-peer 4h
    /// KV backoff). Peers who have genuinely never toggled Focus since
    /// keying will incur at most one extra invite every 4h, which upstream
    /// processes as a c=227 no-op.
    ///
    /// Each returned entry is a "from" handle (e.g. "mailto:user@icloud.com"
    /// or "tel:+1..."). Used by the Go layer both to suppress re-invites for
    /// already-keyed peers and to resolve mailto:/tel: aliases via the IDS
    /// correlation cache.
    ///
    /// Since upstream's StatusKitSharedDevice::from is private, this reads
    /// the plist file directly.
    pub async fn get_known_handles(&self) -> Vec<String> {
        let state_path = subsystem_state_path("statuskit-state.plist");
        let data = match std::fs::read(&state_path) {
            Ok(d) => d,
            Err(_) => return Vec::new(),
        };
        let value = match plist::from_bytes::<plist::Value>(&data) {
            Ok(v) => v,
            Err(_) => return Vec::new(),
        };
        let Some(dict) = value.as_dictionary() else { return Vec::new() };
        let Some(keys_dict) = dict.get("keys").and_then(|v| v.as_dictionary()) else {
            return Vec::new();
        };

        // Collect channel ids (base64-encoded, matching the keys-dict key
        // format) that have received a real status message. recent_channels
        // stores the id as plist::Data; the keys dict uses its base64 form.
        use base64::{engine::general_purpose::STANDARD as B64, Engine as _};
        use std::collections::HashSet;
        let mut confirmed_ids: HashSet<String> = HashSet::new();
        if let Some(rc) = dict.get("recent_channels").and_then(|v| v.as_array()) {
            for entry in rc {
                let Some(d) = entry.as_dictionary() else { continue };
                let last_ns = d.get("last_msg_ns").and_then(|v| v.as_unsigned_integer()).unwrap_or(0);
                if last_ns <= 1 {
                    continue;
                }
                let Some(ident) = d.get("identifier").and_then(|v| v.as_dictionary()) else { continue };
                let Some(id_data) = ident.get("id").and_then(|v| v.as_data()) else { continue };
                confirmed_ids.insert(B64.encode(id_data));
            }
        }

        // A handle is reported as known if ANY of its channels is confirmed.
        // Dedup by handle so a single confirmed channel still covers peers
        // with duplicate placeholder entries.
        let mut seen: HashSet<String> = HashSet::new();
        let mut handles = Vec::new();
        for (channel_id, entry) in keys_dict {
            if !confirmed_ids.contains(channel_id) {
                continue;
            }
            let Some(entry_dict) = entry.as_dictionary() else { continue };
            let Some(from_str) = entry_dict.get("from").and_then(|v| v.as_string()) else { continue };
            if seen.insert(from_str.to_string()) {
                handles.push(from_str.to_string());
            }
        }
        handles
    }

}

#[derive(uniffi::Record)]
pub struct SharedAlbumInfo {
    pub albumguid: String,
    pub name: Option<String>,
    pub fullname: Option<String>,
    pub email: Option<String>,
}

#[derive(uniffi::Record)]
pub struct SharedAssetInfo {
    pub assetguid: String,
    pub filename: String,
    pub date_created: String,
    pub media_type: String,
    pub width: String,
    pub height: String,
    pub size: String,
}

#[derive(uniffi::Object)]
pub struct WrappedSharedStreamsClient {
    inner: Arc<rustpush::sharedstreams::SharedStreamClient<BridgeDefaultAnisetteProvider>>,
}

#[uniffi::export(async_runtime = "tokio")]
impl WrappedSharedStreamsClient {
    pub async fn export_state_json(&self) -> Result<String, WrappedError> {
        let state = self.inner.state.read().await;
        serialize_state_json(&*state)
    }

    pub async fn list_album_ids(&self) -> Vec<String> {
        let state = self.inner.state.read().await;
        state.albums.iter().map(|album| album.albumguid.clone()).collect()
    }

    pub async fn get_changes(&self) -> Result<Vec<String>, WrappedError> {
        Ok(self.inner.get_changes().await?)
    }

    pub async fn subscribe(&self, album: String) -> Result<(), WrappedError> {
        self.inner.subscribe(&album).await?;
        Ok(())
    }

    pub async fn unsubscribe(&self, album: String) -> Result<(), WrappedError> {
        self.inner.unsubscribe(&album).await?;
        Ok(())
    }

    pub async fn subscribe_token(&self, token: String) -> Result<(), WrappedError> {
        self.inner.subscribe_token(&token).await?;
        Ok(())
    }

    pub async fn get_album_summary(&self, album: String) -> Result<Vec<String>, WrappedError> {
        Ok(self.inner.get_album_summary(&album).await?)
    }

    pub async fn get_assets_json(&self, album: String, assets: Vec<String>) -> Result<String, WrappedError> {
        let details = self.inner.get_assets(&album, &assets).await?;
        serde_json::to_string(&details).map_err(|e| WrappedError::GenericError {
            msg: format!("Failed to encode asset details: {}", e),
        })
    }

    pub async fn delete_assets(&self, album: String, assets: Vec<String>) -> Result<(), WrappedError> {
        self.inner.delete_asset(&album, assets).await?;
        Ok(())
    }

    pub async fn list_albums(&self) -> Vec<SharedAlbumInfo> {
        let state = self.inner.state.read().await;
        state.albums.iter().map(|a| SharedAlbumInfo {
            albumguid: a.albumguid.clone(),
            name: a.name.clone(),
            fullname: a.fullname.clone(),
            email: a.email.clone(),
        }).collect()
    }

    pub async fn get_album_assets(&self, album: String) -> Result<Vec<SharedAssetInfo>, WrappedError> {
        let guids = self.inner.get_album_summary(&album).await?;
        if guids.is_empty() {
            return Ok(vec![]);
        }
        let details = self.inner.get_assets(&album, &guids).await?;
        Ok(details.iter().map(|d| {
            let primary = d.files.iter().max_by_key(|f| f.size.parse::<u64>().unwrap_or(0));
            let media_type = if d.collectionmetadata.video_duration.is_some() {
                "video".to_string()
            } else {
                "image".to_string()
            };
            SharedAssetInfo {
                assetguid: d.assetguid.clone(),
                filename: d.filename.clone(),
                date_created: format!("{:?}", d.collectionmetadata.date_created),
                media_type,
                width: primary.map(|f| f.width.clone()).unwrap_or_default(),
                height: primary.map(|f| f.height.clone()).unwrap_or_default(),
                size: primary.map(|f| f.size.clone()).unwrap_or_default(),
            }
        }).collect())
    }

    pub async fn download_file(&self, album: String, asset_guid: String) -> Result<Vec<u8>, WrappedError> {
        let details = self.inner.get_assets(&album, &[asset_guid.clone()]).await?;
        let asset = details.into_iter().next().ok_or(WrappedError::GenericError {
            msg: format!("Asset {} not found in album {}", asset_guid, album),
        })?;
        let primary = asset.files.into_iter()
            .max_by_key(|f| f.size.parse::<u64>().unwrap_or(0))
            .ok_or(WrappedError::GenericError {
                msg: "Asset has no files".to_string(),
            })?;
        let mut buf = Cursor::new(Vec::new());
        self.inner.get_file(&mut [(&primary, &mut buf)], |_, _| {}).await?;
        Ok(buf.into_inner())
    }
}

#[derive(uniffi::Object)]
pub struct Client {
    client: Arc<IMClient>,
    conn: rustpush::APSConnection,
    os_config: Arc<dyn OSConfig>,
    receive_handle: tokio::sync::Mutex<Option<tokio::task::JoinHandle<()>>>,
    token_provider: Option<Arc<WrappedTokenProvider>>,
    cloud_messages_client: tokio::sync::Mutex<Option<Arc<rustpush::cloud_messages::CloudMessagesClient<BridgeDefaultAnisetteProvider>>>>,
    cloud_keychain_client: tokio::sync::Mutex<Option<Arc<rustpush::keychain::KeychainClient<BridgeDefaultAnisetteProvider>>>>,
    findmy_client: tokio::sync::Mutex<Option<Arc<WrappedFindMyClient>>>,
    facetime_client: tokio::sync::Mutex<Option<Arc<WrappedFaceTimeClient>>>,
    passwords_client: tokio::sync::Mutex<Option<Arc<WrappedPasswordsClient>>>,
    statuskit_client: tokio::sync::Mutex<Option<Arc<WrappedStatusKitClient>>>,
    sharedstreams_client: tokio::sync::Mutex<Option<Arc<WrappedSharedStreamsClient>>>,
    /// Cached Profiles client (Name & Photo Sharing). Initialized lazily the
    /// first time a shared profile needs to be fetched from CloudKit.
    profiles_client: tokio::sync::Mutex<Option<Arc<rustpush::name_photo_sharing::ProfilesClient<BridgeDefaultAnisetteProvider>>>>,
    /// Shared handle to the raw StatusKit client, used by the APNs receive
    /// loop to intercept presence messages before iMessage handling. Set by
    /// `init_statuskit()`; the loop no-ops when unset.
    shared_statuskit: Arc<tokio::sync::RwLock<Option<Arc<rustpush::statuskit::StatusKitClient<BridgeDefaultAnisetteProvider>>>>>,
    /// Shared callback for presence updates delivered by StatusKit. Populated
    /// alongside `shared_statuskit` by `init_statuskit()`.
    status_callback: Arc<tokio::sync::RwLock<Option<Arc<dyn StatusCallback>>>>,
    /// Subscription tokens held to keep presence channels open. Cleared by
    /// `unsubscribe_all_status()`.
    statuskit_interest_tokens: tokio::sync::Mutex<Vec<rustpush::statuskit::ChannelInterestToken>>,
}

#[uniffi::export(async_runtime = "tokio")]
pub async fn new_client(
    connection: &WrappedAPSConnection,
    users: &WrappedIDSUsers,
    identity: &WrappedIDSNGMIdentity,
    config: &WrappedOSConfig,
    token_provider: Option<Arc<WrappedTokenProvider>>,
    message_callback: Box<dyn MessageCallback>,
    update_users_callback: Box<dyn UpdateUsersCallback>,
) -> Result<Arc<Client>, WrappedError> {
    ensure_crypto_provider();
    let conn = connection.inner.clone();
    let users_clone = users.inner.clone();
    let identity_clone = identity.inner.clone();
    let config_clone = config.config.clone();

    let _ = std::fs::create_dir_all("state");

    // FACETIME + VIDEO are in the bundle so the IdentityResource's `services`
    // slice contains them. Without that, upstream's `get_main_service` (called
    // on every FT `cache_keys`) hits its `expect("Topic {topic} not found!")`
    // and panics out of the FFI. The earlier best-effort separate-register
    // workaround registered FT/Video in the IDSUser but didn't update
    // `services`, so the panic still fired.
    //
    // Trade-off: `register()` is all-or-nothing, so an Apple non-zero status
    // for any service in this bundle fails the whole call. For status 6005
    // upstream wraps that in `PushError::DoNotRetry`, and the ResourceManager
    // transitions the shared IdentityResource to `Closed` — which would
    // permanently break iMessage send too. We accept this risk because Apple
    // has not been observed to 6005 FT/Video for this account.
    let client = Arc::new(
        IMClient::new(
            conn.clone(),
            users_clone,
            identity_clone,
            &[&MADRID_SERVICE, &MULTIPLEX_SERVICE, &FACETIME_SERVICE, &VIDEO_SERVICE],
            "state/id_cache.plist".into(),
            config_clone.clone(),
            Box::new(move |updated_keys| {
                update_users_callback.update_users(Arc::new(WrappedIDSUsers {
                    inner: updated_keys,
                }));
                debug!("Updated IDS keys");
            }),
        )
        .await,
    );

    // Start receive loop.
    //
    // Architecture: two tasks connected by an unbounded mpsc channel.
    //
    // 1. **Drain task** — reads from the tokio broadcast channel as fast as
    //    possible and forwards every APSMessage into the mpsc.  Because it does
    //    zero processing, it will almost never lag behind the broadcast.  If it
    //    *does* lag (broadcast capacity 9999 exhausted), it logs the count and
    //    continues — there is nothing we can do about already-dropped broadcast
    //    messages, but we won't compound the loss by being slow.
    //
    // 2. **Process task** — reads from the mpsc (unbounded, so no back-pressure
    //    on the drain task) and handles each message: decrypting, downloading
    //    MMCS attachments, and calling the Go callback.  Transient errors are
    //    retried with exponential back-off.  This task can take as long as it
    //    needs without risking broadcast lag.
    let client_for_recv = client.clone();
    let callback = Arc::new(message_callback);

    // Pre-warm FaceTime so incoming FT APNs notifications are consumed even
    // before any explicit !facetime command initializes the subsystem.
    let facetime_state_path = subsystem_state_path("facetime-state.plist");
    let facetime_state = read_plist_state::<rustpush::facetime::FTState>(&facetime_state_path).unwrap_or_default();
    let facetime_state_path_for_closure = facetime_state_path.clone();
    let prewarmed_facetime = Arc::new(WrappedFaceTimeClient {
        inner: Arc::new(
            rustpush::facetime::FTClient::new(
                facetime_state,
                Box::new(move |state| persist_plist_state(&facetime_state_path_for_closure, state)),
                conn.clone(),
                client.identity.clone(),
                config_clone.clone(),
            )
            .await,
        ),
    });

    // Shared StatusKit state: init_statuskit() populates these Arc<RwLock>s,
    // and the receive loop below reads them on every message. The raw
    // StatusKit client gets first crack at incoming APNs messages so presence
    // updates are consumed before iMessage handling.
    let shared_statuskit_for_recv: Arc<tokio::sync::RwLock<Option<Arc<rustpush::statuskit::StatusKitClient<BridgeDefaultAnisetteProvider>>>>> =
        Arc::new(tokio::sync::RwLock::new(None));
    let status_callback_for_recv: Arc<tokio::sync::RwLock<Option<Arc<dyn StatusCallback>>>> =
        Arc::new(tokio::sync::RwLock::new(None));

    // Shared reconnect timestamp: set when APNs reconnects (generated_signal
    // fires) or when the drain task starts listening (initial connection).
    // Messages arriving within RECONNECT_WINDOW_MS of a (re)connect are
    // marked as stored — they were cached by Apple's servers while offline.
    const RECONNECT_WINDOW_MS: u64 = 30_000; // 30 seconds
    // Initialize to 0 — the drain task sets it to now() right before
    // entering the receive loop. This ensures the window starts when we're
    // actually listening for messages, not when receive() is called (which
    // can be 20-30s earlier during Go startup).
    let reconnected_at = Arc::new(AtomicU64::new(0));

    // Weak handle to OUR Client, set after Client is constructed below.
    // The receive loop upgrades this on each iteration to call back into
    // Client::populate_inline_share_profile (mirrors how IconChange is
    // downloaded inline). Weak avoids the receive task keeping Client alive.
    let client_weak_for_loop: Arc<tokio::sync::OnceCell<std::sync::Weak<Client>>> =
        Arc::new(tokio::sync::OnceCell::new());

    let receive_handle = tokio::spawn({
        let conn = connection.inner.clone();
        let conn_for_download = connection.inner.clone();
        let reconnected_at = reconnected_at.clone();
        let sk_for_recv = shared_statuskit_for_recv.clone();
        let status_cb_for_recv = status_callback_for_recv.clone();
        let ft_for_recv = prewarmed_facetime.inner.clone();
        let client_weak_for_loop = client_weak_for_loop.clone();
        async move {
            let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel::<(rustpush::APSMessage, u64)>();
            let pending = Arc::new(AtomicU64::new(0));

            // --- Drain task: broadcast → mpsc + reconnect detection ------
            // Combines message draining AND reconnect detection in a SINGLE
            // task using select!. This eliminates the scheduling race from
            // the old two-task approach: resource_state changes are handled
            // in the same task that receives messages, so reconnected_at is
            // always updated before the next message is forwarded.
            let drain_pending = pending.clone();
            let drain_handle = tokio::spawn({
                let conn = conn.clone();
                let reconnected_at = reconnected_at.clone();
                async move {
                    let mut recv = conn.messages_cont.subscribe();
                    let mut state = conn.resource_state.subscribe();
                    // Mark the initial resource_state value as seen so we
                    // only trigger on future transitions.
                    state.borrow_and_update();
                    // Set reconnected_at NOW — right when we start listening.
                    // Covers stored messages delivered on first connect.
                    let start_ms = std::time::SystemTime::now()
                        .duration_since(std::time::UNIX_EPOCH)
                        .unwrap_or_default()
                        .as_millis() as u64;
                    reconnected_at.store(start_ms, Ordering::Relaxed);
                    info!("Drain task started, marking messages as stored for {}ms", RECONNECT_WINDOW_MS);
                    loop {
                        tokio::select! {
                            biased;
                            // Reconnect detection: resource_state → Generating
                            // fires BEFORE generate() establishes the new
                            // connection, so reconnected_at is set before any
                            // stored messages can arrive on messages_cont.
                            result = state.changed() => {
                                if result.is_err() {
                                    info!("Resource state sender dropped, stopping drain task");
                                    break;
                                }
                                let current = state.borrow_and_update().clone();
                                if matches!(current, ResourceState::Generating) {
                                    let now = std::time::SystemTime::now()
                                        .duration_since(std::time::UNIX_EPOCH)
                                        .unwrap_or_default()
                                        .as_millis() as u64;
                                    reconnected_at.store(now, Ordering::Relaxed);
                                    info!("APNs reconnecting (Generating), marking messages as stored for {}ms", RECONNECT_WINDOW_MS);
                                }
                            }
                            // Message drain: forward APNs messages to process task
                            result = recv.recv() => {
                                match result {
                                    Ok(msg) => {
                                        drain_pending.fetch_add(1, Ordering::Relaxed);
                                        let drain_ts = std::time::SystemTime::now()
                                            .duration_since(std::time::UNIX_EPOCH)
                                            .unwrap_or_default()
                                            .as_millis() as u64;
                                        if tx.send((msg, drain_ts)).is_err() {
                                            info!("Process task gone, stopping drain");
                                            break;
                                        }
                                    }
                                    Err(broadcast::error::RecvError::Lagged(n)) => {
                                        error!(
                                            "APS broadcast receiver lagged — {} messages were DROPPED by the \
                                             broadcast channel before we could read them. Real-time messages \
                                             may have been lost. Consider increasing broadcast capacity or \
                                             investigating processing backlog (pending={}).",
                                            n,
                                            drain_pending.load(Ordering::Relaxed),
                                        );
                                    }
                                    Err(broadcast::error::RecvError::Closed) => {
                                        info!("Broadcast channel closed, stopping drain task");
                                        break;
                                    }
                                }
                            }
                        }
                    }
                }
            });

            // --- Process task: mpsc → handle + callback -----------------
            const MAX_RETRIES: u32 = 5;
            const INITIAL_BACKOFF: Duration = Duration::from_millis(500);

            while let Some((msg, drain_ts)) = rx.recv().await {
                // Diagnostic — log at the earliest APS dispatch point every
                // keysharing-topic message we receive, BEFORE any workaround
                // or handle() logic. Lets us distinguish "peers aren't sending
                // / APS dropping" (zero of these logs after restart) from
                // "receive path broken after message arrives". Suppresses the
                // well-understood noise commands (c=255 server ACK, c=120 IDS
                // per-target retry error, c=97 keysharing noise) to avoid
                // drowning the real signal in server retry traffic.
                if let rustpush::APSMessage::Notification { topic: t, payload: ref p, ref channel, .. } = msg {
                    let ks_hash: [u8; 20] = openssl::sha::sha1(
                        "com.apple.private.alloy.status.keysharing".as_bytes(),
                    );
                    let ps_hash: [u8; 20] = openssl::sha::sha1(
                        "com.apple.private.alloy.status.personal".as_bytes(),
                    );
                    if t == ks_hash || t == ps_hash {
                        let cmd = if let plist::Value::Dictionary(d) = p {
                            d.get("c").and_then(|v| v.as_unsigned_integer())
                        } else {
                            None
                        };
                        let is_noise = matches!(cmd, Some(255) | Some(120) | Some(97));
                        if !is_noise {
                            let topic_name = if t == ks_hash { "status.keysharing" } else { "status.personal" };
                            let shape = match p {
                                plist::Value::Data(_) => "Data",
                                plist::Value::Dictionary(_) => "Dictionary",
                                _ => "Other",
                            };
                            info!(
                                "StatusKit diag: APS message received — topic={} payload_shape={} c={:?} has_channel={}",
                                topic_name, shape, cmd, channel.is_some()
                            );
                        }
                    }
                }

                // StatusKit gets first crack at the APNs message. If it
                // consumes the message (presence update on a subscribed
                // channel), we dispatch to the Go callback and skip iMessage
                // handling. Only active when init_statuskit() has been
                // called; otherwise falls through.
                let sk_opt = sk_for_recv.read().await.clone();
                if let Some(sk) = sk_opt {
                    // Wrapper-only workaround for upstream silent-drop.
                    // statuskit.rs:719 destructures payload as Value::Data and
                    // returns Ok(None) for keysharing-topic notifications whose
                    // payload arrived as Value::Dictionary — which is the typical
                    // case because aps.rs:880 greedily plist-decodes payload bytes
                    // into a Dictionary before reaching handle(). Without this
                    // intercept, peer keysharing replies are dropped at the
                    // destructure and state.keys never grows.
                    //
                    // We bypass handle() for that exact shape (keysharing topic +
                    // non-Data payload), call identity.receive_message directly
                    // (which accepts any Value variant via plist::from_value at
                    // identity_manager.rs:771), and construct a
                    // StatusKitSharedDevice via serde with a built Dictionary —
                    // mirroring the construction at upstream statuskit.rs:776–781
                    // but routed through Deserialize so the private fields stay
                    // private. The device is inserted into the public
                    // sk.state.keys map and persisted via the same code path
                    // upstream's update_state callback uses.
                    // Normalize StatusKit payloads: upstream statuskit.rs:719
                    // destructures with Value::Data, but aps.rs:880 greedily
                    // decodes plist bytes into Dictionary. The workaround below
                    // handles Dictionary payloads. Conversely, Data payloads
                    // whose bytes ARE valid plist fail in receive_message's
                    // plist::from_value (it sees Data, not a Dictionary). Fix
                    // both directions: decode Data→Dictionary so ALL StatusKit
                    // messages go through the workaround's Dictionary path.
                    // Normalize Data→Dictionary for the keysharing topic only:
                    // the upstream silent-drop bug at statuskit.rs:719 is specific
                    // to keysharing-payload destructuring. The other three StatusKit
                    // topics (presence.mode.status, presence.channel.management,
                    // status.personal) flow through sk.handle() natively and were
                    // working at 31ad87b — don't divert them into our workaround.
                    let sk_msg = {
                        let sk_topics: [[u8; 20]; 1] = [
                            openssl::sha::sha1("com.apple.private.alloy.status.keysharing".as_bytes()),
                        ];
                        if let rustpush::APSMessage::Notification { topic: t, payload: plist::Value::Data(ref bytes), .. } = msg {
                            if sk_topics.contains(&t) {
                                if let Ok(decoded) = plist::from_bytes::<plist::Value>(bytes) {
                                    if matches!(decoded, plist::Value::Dictionary(_)) {
                                        let rustpush::APSMessage::Notification { id, topic, token, channel, .. } = msg.clone() else { unreachable!() };
                                        Some(rustpush::APSMessage::Notification { id, topic, token, payload: decoded, channel })
                                    } else { None }
                                } else { None }
                            } else { None }
                        } else { None }
                    };
                    let sk_msg_ref = sk_msg.as_ref().unwrap_or(&msg);

                    let mut workaround_consumed = false;
                    if let rustpush::APSMessage::Notification {
                        topic: msg_topic,
                        payload,
                        ..
                    } = sk_msg_ref
                    {
                        let keysharing_topic_hash: [u8; 20] = openssl::sha::sha1(
                            "com.apple.private.alloy.status.keysharing".as_bytes(),
                        );
                        if *msg_topic == keysharing_topic_hash
                            && !matches!(payload, plist::Value::Data(_))
                        {
                            // Extract command byte from payload for diagnostics
                            // before the async block moves msg.
                            let diag_cmd_workaround = if let plist::Value::Dictionary(ref d) = payload {
                                d.get("c").and_then(|v| v.as_unsigned_integer()).map(|v| v as u8)
                            } else {
                                None
                            };

                            // c=255 = server ACK for our own outgoing invite.
                            // c=120 = IDS per-target error (recipient unreachable,
                            //         key-refresh needed, etc.) — one per failing
                            //         target, repeated as Apple retries delivery.
                            // Both are non-keysharing envelopes (no P/E/sT), so
                            // the workaround's decrypt path produces nothing
                            // useful. Skip to avoid log spam.
                            if diag_cmd_workaround == Some(255) || diag_cmd_workaround == Some(120) {
                                debug!(
                                    "StatusKit workaround: skipping c={} server ACK/error on keysharing topic",
                                    diag_cmd_workaround.unwrap()
                                );
                                // Don't set workaround_consumed — let it fall
                                // through to upstream handle() which also skips
                                // these via its own suppression path.
                            } else {

                            let diag_has_sP = if let plist::Value::Dictionary(ref d) = payload { d.contains_key("sP") } else { false };
                            let diag_has_P = if let plist::Value::Dictionary(ref d) = payload { d.contains_key("P") } else { false };
                            let diag_has_E = if let plist::Value::Dictionary(ref d) = payload { d.contains_key("E") } else { false };
                            let diag_has_t = if let plist::Value::Dictionary(ref d) = payload { d.contains_key("t") } else { false };

                            let result: Result<bool, String> = async {
                                use prost::Message as _;
                                let recv = sk
                                    .identity
                                    .receive_message(
                                        sk_msg_ref.clone(),
                                        &["com.apple.private.alloy.status.keysharing"],
                                    )
                                    .await
                                    .map_err(|e| format!("identity.receive_message: {e}"))?;
                                let Some(recv) = recv else {
                                    warn!("StatusKit workaround: receive_message returned None (c={:?} sP={} P={} E={} t={}) — topic mismatch or not a Notification",
                                        diag_cmd_workaround, diag_has_sP, diag_has_P, diag_has_E, diag_has_t);
                                    return Ok::<bool, String>(false);
                                };
                                let Some(message_unenc) = recv.message_unenc else {
                                    // TPP-confirmed noise: c=97 on keysharing topic has
                                    // plist keys [cT,h,e,U,c,hs,s,b] — no IDS envelope
                                    // fields (sP/tP/P/t/E). Not a peer keysharing reply.
                                    // Log for future protocol analysis; do NOT fire
                                    // on_keys_received (would cause reciprocate-invite
                                    // storm on every noise packet). Return Ok(false)
                                    // so upstream handle() still gets a chance.
                                    if recv.command == 97 {
                                        let raw_keys = if let plist::Value::Dictionary(ref d) = payload {
                                            d.keys().cloned().collect::<Vec<_>>().join(", ")
                                        } else { "<non-dict>".into() };
                                        debug!("StatusKit c=97 noise observed (sender={:?}, payload keys=[{}])",
                                            recv.sender, raw_keys);
                                        return Ok(false);
                                    }
                                    // c=255 ACKs and other non-encrypted messages lack P/E/t fields,
                                    // so the decryption block in receive_message is skipped entirely.
                                    warn!("StatusKit workaround: message_unenc is None (c={} sender={:?} sP={} P={} E={} t={}) — no decrypted payload; likely server ACK or missing IDS keys",
                                        recv.command, recv.sender, diag_has_sP, diag_has_P, diag_has_E, diag_has_t);
                                    return Ok(false);
                                };
                                let Some(sender) = recv.sender else {
                                    warn!("StatusKit workaround: sender is None (c={}) — message has no sP field", recv.command);
                                    return Ok(false);
                                };

                                #[derive(serde::Deserialize)]
                                struct LocalRawShared {
                                    #[serde(rename = "r")]
                                    keys: String,
                                    #[serde(rename = "p")]
                                    personal_config: String,
                                    #[serde(rename = "c")]
                                    channel: String,
                                }
                                let parsed: LocalRawShared = message_unenc
                                    .plist()
                                    .map_err(|e| format!("plist parse raw shared: {e}"))?;

                                let keys_bytes = BASE64_STANDARD
                                    .decode(&parsed.keys)
                                    .map_err(|e| format!("keys base64: {e}"))?;
                                let pc_bytes = BASE64_STANDARD
                                    .decode(&parsed.personal_config)
                                    .map_err(|e| format!("personal_config base64: {e}"))?;

                                let share_message =
                                    rustpush::statuskit::statuskitp::SharedMessage::decode(
                                        std::io::Cursor::new(keys_bytes),
                                    )
                                    .map_err(|e| format!("SharedMessage decode: {e}"))?;

                                let sig_key_arr: [u8; 32] = share_message
                                    .sig_key
                                    .clone()
                                    .try_into()
                                    .map_err(|v: Vec<u8>| {
                                        format!("sig_key length {} != 32", v.len())
                                    })?;
                                let compact = rustpush::CompactECKey::<openssl::pkey::Public>::decompress(sig_key_arr);
                                let der_bytes = compact
                                    .public_key_to_der()
                                    .map_err(|e| format!("public_key_to_der: {e}"))?;

                                let shared_keys =
                                    share_message.keys.unwrap_or_default().keys;
                                if shared_keys.is_empty() {
                                    return Err("share_message.keys.keys is empty".into());
                                }
                                let key_data_array: Vec<plist::Value> = shared_keys
                                    .iter()
                                    .map(|k| plist::Value::Data(k.encode_to_vec()))
                                    .collect();
                                let key_count = key_data_array.len();

                                let pc_value: plist::Value = plist::from_bytes(&pc_bytes)
                                    .map_err(|e| format!("personal_config plist: {e}"))?;

                                let mut dict = plist::Dictionary::new();
                                dict.insert(
                                    "from".into(),
                                    plist::Value::String(sender.clone()),
                                );
                                dict.insert(
                                    "signature".into(),
                                    plist::Value::Data(der_bytes),
                                );
                                dict.insert(
                                    "keys".into(),
                                    plist::Value::Array(key_data_array),
                                );
                                dict.insert("personal_config".into(), pc_value);

                                let device: rustpush::statuskit::StatusKitSharedDevice =
                                    plist::from_value(&plist::Value::Dictionary(dict))
                                        .map_err(|e| {
                                            format!("StatusKitSharedDevice deserialize: {e}")
                                        })?;

                                let mut state_w = sk.state.write().await;
                                let prev = state_w
                                    .keys
                                    .insert(parsed.channel.clone(), device);
                                let total = state_w.keys.len();
                                drop(state_w);

                                let action = if prev.is_some() { "replaced" } else { "added" };
                                let state_path =
                                    subsystem_state_path("statuskit-state.plist");
                                persist_plist_state(
                                    &state_path,
                                    &*sk.state.read().await,
                                );

                                info!(
                                    "StatusKit workaround {action} channel for {} (keys_in_msg={}, state.keys total={}) — Dictionary-payload bypass",
                                    sender, key_count, total
                                );
                                if let Some(cb) =
                                    status_cb_for_recv.read().await.as_ref()
                                {
                                    // Stamp the sender into the Go-side
                                    // presence→portal cache BEFORE
                                    // on_keys_received triggers the
                                    // resubscribe. Peer iOS fans reshares to
                                    // every handle alias on the same channel;
                                    // upstream's HashMap only keeps the last
                                    // `from`, so without this, presence
                                    // updates for overwritten aliases have
                                    // no portal mapping.
                                    cb.on_reshare_sender(sender.clone());
                                    cb.on_keys_received();
                                }
                                Ok(true)
                            }
                            .await;

                            match result {
                                Ok(true) => {
                                    workaround_consumed = true;
                                }
                                Ok(false) => {
                                    // Detailed diagnostics already logged inside the async block.
                                }
                                Err(e) => {
                                    warn!(
                                        "StatusKit workaround failed: {} — falling back to upstream handle()",
                                        e
                                    );
                                }
                            }
                            } // else (non-c=255)
                        }

                        // --- status.personal handler ---
                        // Contacts broadcast their Focus state directly via IDS on
                        // this topic whenever it changes. Unlike the Shared Channels
                        // path (keysharing + presence.mode.status), this doesn't
                        // require a prior key exchange — the IDS encryption is
                        // sufficient. This catches contacts whose devices distributed
                        // channel keys before the bridge was registered.
                        let personal_topic_hash: [u8; 20] = openssl::sha::sha1(
                            "com.apple.private.alloy.status.personal".as_bytes(),
                        );
                        if *msg_topic == personal_topic_hash
                            && !matches!(payload, plist::Value::Data(_))
                            && !workaround_consumed
                        {
                            let personal_result: Result<bool, String> = async {
                                let recv = sk
                                    .identity
                                    .receive_message(
                                        sk_msg_ref.clone(),
                                        &["com.apple.private.alloy.status.personal"],
                                    )
                                    .await
                                    .map_err(|e| format!("personal receive_message: {e}"))?;
                                let Some(recv) = recv else {
                                    return Ok(false);
                                };
                                let Some(message) = recv.message_unenc else {
                                    return Ok(false);
                                };
                                let Some(sender) = recv.sender else {
                                    return Ok(false);
                                };

                                // Mirror upstream PrivateStatusMessage structs.
                                #[derive(serde::Deserialize, Debug)]
                                struct PTag { #[serde(rename = "a")] bundle: String, #[serde(rename = "b")] id: Option<String> }
                                #[derive(serde::Deserialize, Debug)]
                                struct PState { #[serde(rename = "b")] r#type: String }
                                #[derive(serde::Deserialize, Debug)]
                                struct PActive { #[serde(rename = "d")] tag: PTag, #[serde(rename = "c")] state: PState }
                                #[derive(serde::Deserialize, Debug)]
                                struct PStatusState { #[serde(rename = "a", default)] active: Vec<PActive> }
                                #[derive(serde::Deserialize, Debug)]
                                struct PUpdate { #[serde(rename = "c")] state: PStatusState }
                                #[derive(serde::Deserialize, Debug)]
                                struct PMessage { #[serde(rename = "d")] update: PUpdate }

                                let parsed: PMessage = message.plist()
                                    .map_err(|e| format!("personal plist parse: {e}"))?;

                                // Determine Focus state: if any active mode exists,
                                // user is in Focus; otherwise available.
                                let focus_active = &parsed.update.state.active;
                                let (available, mode) = if focus_active.is_empty() {
                                    (true, None)
                                } else {
                                    // Use the first active Focus mode's identifier.
                                    let mode_id = focus_active[0].tag.id.clone()
                                        .or_else(|| Some(focus_active[0].state.r#type.clone()));
                                    (false, mode_id)
                                };

                                info!(
                                    "StatusKit personal status from {}: available={} mode={:?} (active_modes={})",
                                    sender, available, mode, focus_active.len()
                                );

                                if let Some(cb) = status_cb_for_recv.read().await.as_ref() {
                                    cb.on_status_update(sender, mode, available);
                                }
                                Ok(true)
                            }.await;

                            match personal_result {
                                Ok(true) => { workaround_consumed = true; }
                                Ok(false) => {}
                                Err(e) => {
                                    warn!("StatusKit personal handler failed: {}", e);
                                }
                            }
                        }
                    }

                    if workaround_consumed {
                        pending.fetch_sub(1, Ordering::Relaxed);
                        continue;
                    }

                    // Extract the APNs topic before spawning the thread so we can
                    // check it later for key-sharing detection (msg is moved into
                    // the spawned thread and unavailable afterwards).
                    let msg_topic = if let rustpush::APSMessage::Notification { topic, .. } = &msg {
                        Some(*topic)
                    } else {
                        None
                    };
                    // Snapshot the set of channel IDs in state.keys before handle()
                    // so we can detect whether a keysharing APNs message added a new
                    // channel. Using a HashSet (not len()) so that a re-key — where
                    // invite_to_channel replaces an existing channel id via
                    // HashMap::insert — is also detected even though len() would not
                    // change.
                    let keys_before: std::collections::HashSet<String> =
                        sk.state.read().await.keys.keys().cloned().collect();
                    // handle() is async and may panic (statuskit.rs:736 "Channel not
                    // found!"). Spawn as a tokio task on the main runtime so panics
                    // surface as JoinError rather than crashing the loop, AND so the
                    // task has access to the main runtime's tokio primitives — the
                    // IDS identity manager, reqwest HTTP client, and background
                    // refresh tasks are all bound to this runtime. Running handle()
                    // on a separate runtime (prior implementation) caused
                    // receive_message to silently fail its key lookups because the
                    // HTTP requests and tokio mutexes couldn't complete properly in
                    // an isolated current-thread runtime.
                    let sk_clone = sk.clone();
                    let sk_msg_for_handle = sk_msg_ref.clone();
                    let handle_result = tokio::task::spawn(async move {
                        sk_clone.handle(sk_msg_for_handle).await
                    }).await;
                    // Build a human-readable topic label for diagnostic log lines.
                    let topic_label = if let Some(t) = msg_topic {
                        let ks: [u8; 20] = openssl::sha::sha1("com.apple.private.alloy.status.keysharing".as_bytes());
                        let ps: [u8; 20] = openssl::sha::sha1("com.apple.icloud.presence.mode.status".as_bytes());
                        let cm: [u8; 20] = openssl::sha::sha1("com.apple.icloud.presence.channel.management".as_bytes());
                        let sp: [u8; 20] = openssl::sha::sha1("com.apple.private.alloy.status.personal".as_bytes());
                        if t == ks { "keysharing" } else if t == ps { "presence" } else if t == cm { "channel-mgmt" } else if t == sp { "personal" } else { "unknown" }
                    } else {
                        "no-topic"
                    };
                    let diag_cmd = if let rustpush::APSMessage::Notification { payload: plist::Value::Data(payload), .. } = &msg {
                        plist::from_bytes::<plist::Value>(payload)
                            .ok()
                            .and_then(|v| v.into_dictionary())
                            .and_then(|d| d.get("c").and_then(|c| c.as_unsigned_integer()).map(|v| v as u8))
                    } else {
                        None
                    };
                    match handle_result {
                        Ok(Ok(Some(rustpush::statuskit::StatusKitMessage::StatusChanged { user, mode, allowed }))) => {
                            if let Some(cb) = status_cb_for_recv.read().await.as_ref() {
                                cb.on_status_update(user, mode, allowed);
                            }
                            pending.fetch_sub(1, Ordering::Relaxed);
                            continue;
                        }
                        Ok(Ok(None)) => {
                            // Not a StatusKit presence update — but check whether it
                            // was a key-sharing message. Only fire on_keys_received
                            // if state.keys gained a new channel id.
                            let keysharing_topic: [u8; 20] = openssl::sha::sha1("com.apple.private.alloy.status.keysharing".as_bytes());
                            if let Some(topic) = msg_topic {
                                if topic == keysharing_topic {
                                    let keys_after: std::collections::HashSet<String> =
                                        sk.state.read().await.keys.keys().cloned().collect();
                                    let new_channels: Vec<&String> = keys_after.difference(&keys_before).collect();
                                    if !new_channels.is_empty() {
                                        // Extract sP (sender participant) from the raw IDS
                                        // envelope so we can stamp this reshare's sender
                                        // into the Go-side cache. Peer iOS fans each
                                        // reshare to every alias of our account with the
                                        // SAME channel id but DIFFERENT sP, and upstream
                                        // state.keys overwrites on each — without stamping
                                        // here, the aliases processed by upstream handle()
                                        // (not the Dictionary-payload workaround) get lost
                                        // the same way the workaround case used to, just
                                        // silently. Mirrors the workaround stamping at the
                                        // cb.on_reshare_sender call above.
                                        let upstream_sender = if let rustpush::APSMessage::Notification { payload: plist::Value::Data(payload), .. } = &msg {
                                            plist::from_bytes::<plist::Value>(payload)
                                                .ok()
                                                .and_then(|v| v.into_dictionary())
                                                .and_then(|d| d.get("sP").and_then(|v| v.as_string()).map(|s| s.to_string()))
                                        } else {
                                            None
                                        };
                                        info!("StatusKit key-sharing message received — state.keys gained {} new channel(s), sender={:?}", new_channels.len(), upstream_sender);
                                        if let Some(cb) = status_cb_for_recv.read().await.as_ref() {
                                            if let Some(sender) = upstream_sender {
                                                cb.on_reshare_sender(sender);
                                            }
                                            cb.on_keys_received();
                                        }
                                    } else {
                                        // Parse the command byte and IDS envelope field presence
                                        // from the raw payload for diagnostics.
                                        let (cmd_byte, ids_fields) =
                                            if let rustpush::APSMessage::Notification { payload: plist::Value::Data(payload), .. } = &msg {
                                                if let Ok(plist::Value::Dictionary(d)) = plist::from_bytes::<plist::Value>(payload) {
                                                    let c = d.get("c").and_then(|v| v.as_unsigned_integer()).map(|v| v as u8);
                                                    let sp = d.contains_key("sP");
                                                    let tp = d.contains_key("tP");
                                                    let p  = d.contains_key("P");
                                                    let t  = d.contains_key("t");
                                                    let e  = d.contains_key("E");
                                                    (c, Some((sp, tp, p, t, e)))
                                                } else {
                                                    (None, None)
                                                }
                                            } else {
                                                (None, None)
                                            };
                                        // c=255 = server ACK for our own outgoing invite; suppress.
                                        // c=227 = peer re-invite for an existing channel (expected
                                        //         re-key). HashMap::insert replaces the entry in
                                        //         place, so HashSet diff yields no growth. Log at
                                        //         INFO when keys_before is non-empty.
                                        // c=97  = unknown command on keysharing topic. Log all
                                        //         plist keys at WARN so we can see whether this is
                                        //         a real iOS keysharing message in a different format
                                        //         than the OpenBubbles c=227. Other commands also logged.
                                        let is_silent = cmd_byte.map(|c| c == 255).unwrap_or(false);
                                        let is_reinvite = cmd_byte.map(|c| c == 227).unwrap_or(false)
                                            && !keys_before.is_empty();
                                        // Non-Notification APS frames (ACKs, connection state,
                                        // keepalives) flow through the keysharing subscription and
                                        // reach this path, but they can never carry a real
                                        // keysharing invite payload — cmd_byte and IDS fields are
                                        // all unknown, and the diagnostic just adds noise. Only
                                        // warn when the msg actually is a Notification and we
                                        // managed to parse a command byte (else there is nothing
                                        // informative to say).
                                        let is_notification = matches!(
                                            &msg,
                                            rustpush::APSMessage::Notification { payload: plist::Value::Data(_), .. }
                                        );
                                        if is_reinvite {
                                            info!("StatusKit peer re-invite received for existing channel (c=227) — keys replaced in place");
                                        } else if !is_silent && is_notification && cmd_byte.is_some() {
                                            let all_keys = if let rustpush::APSMessage::Notification { payload: plist::Value::Data(payload), .. } = &msg {
                                                plist::from_bytes::<plist::Value>(payload)
                                                    .ok()
                                                    .and_then(|v| v.into_dictionary())
                                                    .map(|d| d.keys().cloned().collect::<Vec<_>>().join(", "))
                                                    .unwrap_or_else(|| "<unparseable>".into())
                                            } else {
                                                "<not a notification>".into()
                                            };
                                            warn!(
                                                "StatusKit inbound keysharing message (c={}) did not grow state.keys — plist keys: [{}]  IDS fields: sP={} tP={} P={} t={} E={}",
                                                cmd_byte.map(|c| c.to_string()).unwrap_or_else(|| "?".into()),
                                                all_keys,
                                                ids_fields.map(|(sp,_,_,_,_)| if sp {"Y"} else {"N"}).unwrap_or("?"),
                                                ids_fields.map(|(_,tp,_,_,_)| if tp {"Y"} else {"N"}).unwrap_or("?"),
                                                ids_fields.map(|(_,_,p,_,_)|  if p  {"Y"} else {"N"}).unwrap_or("?"),
                                                ids_fields.map(|(_,_,_,t,_)|  if t  {"Y"} else {"N"}).unwrap_or("?"),
                                                ids_fields.map(|(_,_,_,_,e)|  if e  {"Y"} else {"N"}).unwrap_or("?"),
                                            );
                                        }
                                    }
                                }
                            }
                        }
                        Ok(Err(rustpush::PushError::VerificationFailed)) => {
                            warn!("StatusKit signature verification failed (c={:?} topic={}) — key ratchet mismatch, will resolve on next keysharing message", diag_cmd, topic_label);
                        }
                        Ok(Err(e)) => {
                            warn!("StatusKit handle error (c={:?} topic={}): {:?}", diag_cmd, topic_label, e);
                        }
                        Err(_) => {
                            warn!("StatusKit handle panicked (c={:?} topic={}) — channel may lack shared keys", diag_cmd, topic_label);
                        }
                    }
                }

                // Consume FaceTime APNs events to keep FT state live and
                // surface incoming-call attempts to Go as synthetic notice
                // triggers. ft_handle_with_join_recovery wraps upstream's
                // handle() and falls back to local decoding for cmd 207/209
                // when Apple sends them without the context.message field —
                // see the helper's docstring for why.
                //
                // Retry on SendTimedOut: upstream's handle_letmein sends a
                // delegation message_session INSIDE handle() — if APNs flaps
                // at that moment, the entire LetMeIn is dropped before our
                // auto_approve even runs. A single retry after 2s usually
                // lands on the reconnected APNs.
                let ft_result = {
                    let first = ft_handle_with_join_recovery(ft_for_recv.as_ref(), msg.clone()).await;
                    if matches!(&first, Err(rustpush::PushError::SendTimedOut)) {
                        warn!("FaceTime handle SendTimedOut, retrying in 2s");
                        tokio::time::sleep(std::time::Duration::from_secs(2)).await;
                        ft_handle_with_join_recovery(ft_for_recv.as_ref(), msg.clone()).await
                    } else {
                        first
                    }
                };
                match ft_result {
                    Ok(Some(ft_message)) => {
                        if let rustpush::facetime::FTMessage::LetMeInRequest(request) = &ft_message {
                            if let Err(e) = auto_approve_bridge_letmein(ft_for_recv.as_ref(), request).await {
                                warn!("FaceTime auto-approve LetMeIn failed: {:?}", e);
                            }
                        }
                        if let rustpush::facetime::FTMessage::JoinEvent { guid, handle, .. } = &ft_message {
                            maybe_fire_pending_ring(ft_for_recv.as_ref(), guid, handle).await;
                        }
                        if let Some(wrapped) = facetime_event_to_wrapped(ft_for_recv.as_ref(), &ft_message).await {
                            callback.on_message(wrapped);
                        }
                    }
                    Ok(None) => {}
                    Err(e) => {
                        warn!("FaceTime handle error: {:?}", e);
                    }
                }

                let mut retries = 0u32;
                let mut backoff = INITIAL_BACKOFF;

                loop {
                    match client_for_recv.handle(msg.clone()).await {
                        Ok(Some(msg_inst)) => {
                            // Certify delivery back to the sender when the
                            // incoming message carries a certified context.
                            // Without this, the sender's device displays a
                            // "not delivered" indicator even though we
                            // received and decrypted the message successfully.
                            if let Some(ref context) = msg_inst.certified_context {
                                if let Err(e) = client_for_recv
                                    .identity
                                    .certify_delivery("com.apple.madrid", context, msg_inst.send_delivered)
                                    .await
                                {
                                    warn!("Failed to certify delivery for {}: {:?}", msg_inst.id, e);
                                }
                            }

                            // Diagnostic: surface profile-sharing arrivals so we can confirm
                            // APNs is actually delivering them. Logged here, before the
                            // has_payload gate, so a future filter regression can't hide it.
                            match &msg_inst.message {
                                Message::ShareProfile(p) => info!(
                                    "received Message::ShareProfile from {:?} (record_key_len={}, has_poster={})",
                                    msg_inst.sender,
                                    p.cloud_kit_record_key.len(),
                                    p.poster.is_some()
                                ),
                                Message::UpdateProfile(u) => info!(
                                    "received Message::UpdateProfile from {:?} (share_contacts={}, has_embedded_profile={})",
                                    msg_inst.sender,
                                    u.share_contacts,
                                    u.profile.is_some()
                                ),
                                Message::UpdateProfileSharing(s) => info!(
                                    "received Message::UpdateProfileSharing from {:?} (version={}, dismissed={}, all={})",
                                    msg_inst.sender,
                                    s.version,
                                    s.shared_dismissed.len(),
                                    s.shared_all.len()
                                ),
                                _ => {}
                            }

                            if msg_inst.has_payload() || matches!(msg_inst.message, Message::Typing(_, _) | Message::Read | Message::Delivered | Message::Error(_) | Message::PeerCacheInvalidate) {
                                let mut wrapped = message_inst_to_wrapped(&msg_inst);

                                // Mark as stored if within the post-reconnect window.
                                // Uses the drain timestamp (when the message was received
                                // from APNs) instead of now, so MMCS download time and
                                // retry backoff don't push messages past the window.
                                let reconn = reconnected_at.load(Ordering::Relaxed);
                                if reconn > 0 {
                                    let delta = drain_ts.saturating_sub(reconn);
                                    wrapped.is_stored_message = delta < RECONNECT_WINDOW_MS;
                                    if wrapped.is_stored_message {
                                        info!("Stored message detected: uuid={} delta={}ms (window={}ms)", wrapped.uuid, delta, RECONNECT_WINDOW_MS);
                                    }
                                }

                                // Download MMCS attachments so Go receives inline data
                                download_mmcs_attachments(&mut wrapped, &msg_inst, &conn_for_download).await;
                                // Download group photo for IconChange messages via MMCS
                                if wrapped.is_icon_change && !wrapped.group_photo_cleared {
                                    download_icon_change_photo(&mut wrapped, &msg_inst, &conn_for_download).await;
                                }
                                // Download Name & Photo Sharing profile from CloudKit
                                // (mirrors the IconChange MMCS pattern: fetch in Rust,
                                // hand Go ready-to-display name + avatar bytes).
                                if wrapped.is_share_profile {
                                    if let Some(client_arc) = client_weak_for_loop
                                        .get()
                                        .and_then(|w| w.upgrade())
                                    {
                                        client_arc.populate_inline_share_profile(&msg_inst, &mut wrapped).await;
                                    }
                                }
                                callback.on_message(wrapped);
                            }
                            break; // success
                        }
                        Ok(None) => {
                            break; // message intentionally ignored by handle()
                        }
                        Err(e) => {
                            // Classify: retryable vs permanent
                            let is_permanent = matches!(
                                e,
                                rustpush::PushError::BadMsg
                                    | rustpush::PushError::DoNotRetry(_)
                                    | rustpush::PushError::VerificationFailed
                            );

                            if is_permanent || retries >= MAX_RETRIES {
                                error!(
                                    "Failed to handle APS message after {} attempt(s) (permanent={}): {:?}",
                                    retries + 1,
                                    is_permanent,
                                    e
                                );
                                break;
                            }

                            retries += 1;
                            warn!(
                                "Transient error handling APS message (attempt {}/{}), retrying in {:?}: {:?}",
                                retries,
                                MAX_RETRIES,
                                backoff,
                                e
                            );
                            tokio::time::sleep(backoff).await;
                            backoff = std::cmp::min(backoff * 2, Duration::from_secs(15));
                        }
                    }
                }

                pending.fetch_sub(1, Ordering::Relaxed);
            }

            drain_handle.abort();
            info!("Receive loop exited");
        }
    });

    let client = Arc::new(Client {
        client,
        conn: connection.inner.clone(),
        os_config: config_clone,
        receive_handle: tokio::sync::Mutex::new(Some(receive_handle)),
        token_provider,
        cloud_messages_client: tokio::sync::Mutex::new(None),
        cloud_keychain_client: tokio::sync::Mutex::new(None),
        findmy_client: tokio::sync::Mutex::new(None),
        facetime_client: tokio::sync::Mutex::new(Some(prewarmed_facetime)),
        passwords_client: tokio::sync::Mutex::new(None),
        statuskit_client: tokio::sync::Mutex::new(None),
        sharedstreams_client: tokio::sync::Mutex::new(None),
        profiles_client: tokio::sync::Mutex::new(None),
        shared_statuskit: shared_statuskit_for_recv,
        status_callback: status_callback_for_recv,
        statuskit_interest_tokens: tokio::sync::Mutex::new(Vec::new()),
    });
    // Hand the receive loop a Weak<Client> so it can call back into
    // populate_inline_share_profile without preventing Client drop.
    let _ = client_weak_for_loop.set(Arc::downgrade(&client));
    Ok(client)
}

impl Client {
    async fn get_or_init_cloud_messages_client(&self) -> Result<Arc<rustpush::cloud_messages::CloudMessagesClient<BridgeDefaultAnisetteProvider>>, WrappedError> {
        // Fast path: return cached client without doing any slow work.
        // IMPORTANT: the lock is released before any network calls so that
        // tokio timeouts can fire and concurrent callers are never blocked by
        // a slow initialisation in progress.
        {
            let locked = self.cloud_messages_client.lock().await;
            if let Some(client) = &*locked {
                return Ok(client.clone());
            }
        }

        info!("Cloud client init: no cached client, initializing now");

        // All slow work happens here WITHOUT holding cloud_messages_client.
        let tp = self.token_provider.as_ref().ok_or(WrappedError::GenericError {
            msg: "No TokenProvider available".into(),
        })?;

        let dsid = tp.get_dsid().await?;
        let adsid = tp.get_adsid().await?;
        let mme_delegate = tp.parse_mme_delegate().await?;
        let account = tp.get_account();
        let os_config = tp.get_os_config();
        let anisette = account.lock().await.anisette.clone();

        let cloudkit_state = rustpush::cloudkit::CloudKitState::new(dsid.clone()).ok_or(
            WrappedError::GenericError {
                msg: "Failed to create CloudKitState".into(),
            },
        )?;
        let cloudkit = Arc::new(rustpush::cloudkit::CloudKitClient {
            state: rustpush::DebugRwLock::new(cloudkit_state),
            anisette: anisette.clone(),
            config: os_config.clone(),
            token_provider: tp.inner.clone(),
        });

        let keychain_state_path = format!("{}/trustedpeers.plist", resolve_xdg_data_dir());
        let mut keychain_state: Option<rustpush::keychain::KeychainClientState> = match std::fs::read(&keychain_state_path) {
            Ok(data) => match plist::from_bytes(&data) {
                Ok(state) => Some(state),
                Err(e) => {
                    warn!("Failed to parse keychain state at {}: {}", keychain_state_path, e);
                    None
                }
            },
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => None,
            Err(e) => {
                warn!("Failed to read keychain state at {}: {}", keychain_state_path, e);
                None
            }
        };
        if keychain_state.is_none() {
            keychain_state = Some(
                rustpush::keychain::KeychainClientState::new(dsid, adsid, &mme_delegate)
                    .ok_or(WrappedError::GenericError {
                        msg: "Missing KeychainSync config in MobileMe delegate".into(),
                    })?
            );
        }
        let path_for_closure = keychain_state_path.clone();

        let keychain = Arc::new(rustpush::keychain::KeychainClient {
            anisette,
            token_provider: tp.inner.clone(),
            state: rustpush::DebugRwLock::new(keychain_state.expect("keychain state missing")),
            config: os_config,
            update_state: Box::new(move |state| {
                if let Err(e) = plist::to_file_xml(&path_for_closure, state) {
                    warn!("Failed to persist keychain state to {}: {}", path_for_closure, e);
                }
            }),
            container: tokio::sync::Mutex::new(None),
            security_container: tokio::sync::Mutex::new(None),
            client: cloudkit.clone(),
        });

        // All pre-init network steps (TLK share refresh, keychain sync, zone
        // key prewarm, record count) have been removed: rustpush's keychain
        // and CloudKit functions block the tokio executor thread (synchronous
        // network I/O inside async futures), so tokio::time::timeout cannot
        // fire — causing indefinite hangs regardless of the timeout value.
        //
        // The client is constructed immediately with cached on-disk state.
        // PCS key errors encountered during sync_records are handled lazily
        // by recover_cloud_pcs_state, which retries the keychain sync then.

        let cloud_messages = Arc::new(rustpush::cloud_messages::CloudMessagesClient::new(
            cloudkit, keychain.clone(),
        ));
        info!("Cloud client init: complete");

        // Write path: acquire the lock briefly to store the result.
        // If another task raced and initialized first, use their result.
        let mut locked = self.cloud_messages_client.lock().await;
        if let Some(existing) = &*locked {
            info!("Cloud client init: another task initialized first, using their result");
            return Ok(existing.clone());
        }
        *locked = Some(cloud_messages.clone());
        *self.cloud_keychain_client.lock().await = Some(keychain);
        Ok(cloud_messages)
    }

    async fn get_or_init_cloud_keychain_client(&self) -> Result<Arc<rustpush::keychain::KeychainClient<BridgeDefaultAnisetteProvider>>, WrappedError> {
        let _ = self.get_or_init_cloud_messages_client().await?;
        self.cloud_keychain_client
            .lock()
            .await
            .clone()
            .ok_or(WrappedError::GenericError {
                msg: "No keychain client available".into(),
            })
    }

    async fn recover_cloud_pcs_state(&self, context: &str) -> Result<(), WrappedError> {
        info!("{}: starting keychain resync recovery", context);
        let keychain = self.get_or_init_cloud_keychain_client().await?;
        // refresh_recoverable_tlk_shares removed: fetch_shares_for blocks the
        // tokio thread (no timeout possible). sync_keychain covers the same
        // key material via a different Cuttlefish path.
        sync_keychain_with_retries(&keychain, 6, context).await
    }

    async fn get_or_init_profiles_client(&self) -> Result<Arc<rustpush::name_photo_sharing::ProfilesClient<BridgeDefaultAnisetteProvider>>, WrappedError> {
        let mut locked = self.profiles_client.lock().await;
        if let Some(client) = &*locked {
            return Ok(client.clone());
        }
        // ProfilesClient shares the CloudKit handle owned by the keychain
        // client — initialize cloud messages first so both are available.
        let keychain = self.get_or_init_cloud_keychain_client().await?;
        let cloudkit = keychain.client.clone();
        let profiles = Arc::new(rustpush::name_photo_sharing::ProfilesClient::new(cloudkit));
        *locked = Some(profiles.clone());
        info!("ProfilesClient initialized");
        Ok(profiles)
    }

    /// Inline-fetch a Name & Photo Sharing profile from CloudKit during the
    /// receive loop and stuff the decrypted name + avatar bytes onto the
    /// outgoing WrappedMessage. Mirrors `download_icon_change_photo` so the
    /// Go side never has to make a follow-up FFI call to render the ghost.
    /// Failures are logged and left silent — the keys remain on the wrapped
    /// message so a Go-side periodic re-fetch can recover later.
    async fn populate_inline_share_profile(
        &self,
        _msg_inst: &MessageInst,
        wrapped: &mut WrappedMessage,
    ) {
        // Drive off the wrapped fields rather than re-matching on
        // msg_inst.message — populate_share_profile_keys already pulled the
        // keys out of the standalone (Message::ShareProfile / UpdateProfile)
        // and embedded (NormalMessage.embedded_profile / React.embedded_profile)
        // cases, so by the time we get here the keys are uniformly available.
        let (record_key, decryption_key, has_poster) = match (
            wrapped.share_profile_record_key.as_ref(),
            wrapped.share_profile_decryption_key.as_ref(),
        ) {
            (Some(rk), Some(dk)) => (rk.clone(), dk.clone(), wrapped.share_profile_has_poster),
            _ => return,
        };
        let share_msg = rustpush::ShareProfileMessage {
            cloud_kit_record_key: record_key,
            cloud_kit_decryption_record_key: decryption_key,
            poster: if has_poster {
                Some(rustpush::SharedPoster {
                    low_res_wallpaper_tag: vec![],
                    wallpaper_tag: vec![],
                    message_tag: vec![],
                })
            } else {
                None
            },
        };

        let key_prefix: String = share_msg.cloud_kit_record_key.chars().take(8).collect();
        let profiles = match self.get_or_init_profiles_client().await {
            Ok(p) => p,
            Err(e) => {
                warn!(
                    "inline ShareProfile fetch: ProfilesClient init failed (record_key={}…): {:?}",
                    key_prefix, e
                );
                return;
            }
        };
        match profiles.get_record(&share_msg).await {
            Ok(record) => {
                info!(
                    "inline ShareProfile fetch ok (record_key={}…, name='{}', avatar_bytes={})",
                    key_prefix,
                    record.name.name,
                    record.image.as_ref().map(|v| v.len()).unwrap_or(0)
                );
                wrapped.share_profile_display_name = Some(record.name.name);
                wrapped.share_profile_first_name = Some(record.name.first);
                wrapped.share_profile_last_name = Some(record.name.last);
                wrapped.share_profile_avatar = record.image;
            }
            Err(e) => {
                warn!(
                    "inline ShareProfile fetch failed (record_key={}…, has_poster={}): {:?}",
                    key_prefix,
                    share_msg.poster.is_some(),
                    e
                );
            }
        }
    }

    async fn get_or_init_findmy_client(&self) -> Result<Arc<WrappedFindMyClient>, WrappedError> {
        let mut locked = self.findmy_client.lock().await;
        if let Some(client) = &*locked {
            return Ok(client.clone());
        }

        let tp = self.token_provider.as_ref().ok_or(WrappedError::GenericError {
            msg: "No TokenProvider available".into(),
        })?;
        let dsid = tp.get_dsid().await?;
        let keychain = self.get_or_init_cloud_keychain_client().await?;
        let cloudkit = keychain.client.clone();
        let account = tp.get_account();
        let anisette = account.lock().await.anisette.clone();

        let state_path = subsystem_state_path("findmy.state");
        let state_bytes = match std::fs::read(&state_path) {
            Ok(data) => data,
            Err(err) if err.kind() == std::io::ErrorKind::NotFound => rustpush::findmy::FindMyState::new(dsid).encode()?,
            Err(err) => {
                return Err(WrappedError::GenericError {
                    msg: format!("Failed to read Find My state: {}", err),
                })
            }
        };
        let state_path_for_closure = state_path.clone();
        let state_manager = rustpush::findmy::FindMyStateManager::new(
            &state_bytes,
            Box::new(move |data| {
                if let Err(err) = std::fs::write(&state_path_for_closure, data) {
                    warn!("Failed to persist Find My state to {}: {}", state_path_for_closure, err);
                }
            }),
        );

        let wrapped = Arc::new(WrappedFindMyClient {
            inner: Arc::new(
                rustpush::findmy::FindMyClient::new(
                    self.conn.clone(),
                    cloudkit,
                    keychain,
                    self.os_config.clone(),
                    state_manager,
                    tp.inner.clone(),
                    anisette,
                    self.client.identity.clone(),
                )
                .await?,
            ),
        });
        *locked = Some(wrapped.clone());
        Ok(wrapped)
    }

    async fn get_or_init_facetime_client(&self) -> Result<Arc<WrappedFaceTimeClient>, WrappedError> {
        let mut locked = self.facetime_client.lock().await;
        if let Some(client) = &*locked {
            return Ok(client.clone());
        }

        let state_path = subsystem_state_path("facetime-state.plist");
        let state = read_plist_state::<rustpush::facetime::FTState>(&state_path).unwrap_or_default();
        let state_path_for_closure = state_path.clone();

        let wrapped = Arc::new(WrappedFaceTimeClient {
            inner: Arc::new(
                rustpush::facetime::FTClient::new(
                    state,
                    Box::new(move |state| persist_plist_state(&state_path_for_closure, state)),
                    self.conn.clone(),
                    self.client.identity.clone(),
                    self.os_config.clone(),
                )
                .await,
            ),
        });
        *locked = Some(wrapped.clone());
        Ok(wrapped)
    }

    async fn get_or_init_passwords_client(&self) -> Result<Arc<WrappedPasswordsClient>, WrappedError> {
        let mut locked = self.passwords_client.lock().await;
        if let Some(client) = &*locked {
            return Ok(client.clone());
        }

        let keychain = self.get_or_init_cloud_keychain_client().await?;
        let cloudkit = keychain.client.clone();
        let state_path = subsystem_state_path("passwords-state.plist");
        let state = read_plist_state::<rustpush::passwords::PasswordState>(&state_path).unwrap_or_default();
        let state_path_for_closure = state_path.clone();

        let wrapped = Arc::new(WrappedPasswordsClient {
            inner: rustpush::passwords::PasswordManager::new(
                keychain,
                cloudkit,
                self.client.identity.clone(),
                self.conn.clone(),
                state,
                Box::new(move |state| persist_plist_state(&state_path_for_closure, state)),
                Box::new(|_, _| {}),
            )
            .await,
        });
        *locked = Some(wrapped.clone());
        Ok(wrapped)
    }

    async fn get_or_init_statuskit_client(&self) -> Result<Arc<WrappedStatusKitClient>, WrappedError> {
        // Fast path: return cached client without holding the lock during init.
        {
            let locked = self.statuskit_client.lock().await;
            if let Some(client) = &*locked {
                return Ok(client.clone());
            }
        }

        // Slow path: StatusKitClient::new() calls request_topics which can
        // block the tokio thread. Build the client WITHOUT holding the mutex
        // so concurrent callers are not blocked.
        let tp = self.token_provider.as_ref().ok_or(WrappedError::GenericError {
            msg: "No TokenProvider available".into(),
        })?;
        let state_path = subsystem_state_path("statuskit-state.plist");
        let state = match read_plist_state::<rustpush::statuskit::StatusKitState>(&state_path) {
            Some(s) => {
                info!(
                    "StatusKit state: loaded {} peer key(s), my_key={}, path={}",
                    s.keys.len(),
                    s.my_key.is_some(),
                    state_path
                );
                s
            }
            None => {
                info!(
                    "StatusKit state: plist absent or unreadable at {} — starting with empty state",
                    state_path
                );
                rustpush::statuskit::StatusKitState::default()
            }
        };
        let state_path_for_closure = state_path.clone();

        let wrapped = Arc::new(WrappedStatusKitClient {
            inner: rustpush::statuskit::StatusKitClient::new(
                state,
                Box::new(move |state| persist_plist_state(&state_path_for_closure, state)),
                tp.inner.clone(),
                self.conn.clone(),
                self.os_config.clone(),
                self.client.identity.clone(),
            )
            .await,
            interests: tokio::sync::Mutex::new(Vec::new()),
        });

        // Write path: re-acquire briefly to store result; handle race.
        let mut locked = self.statuskit_client.lock().await;
        if let Some(existing) = &*locked {
            return Ok(existing.clone());
        }
        *locked = Some(wrapped.clone());
        Ok(wrapped)
    }

    async fn get_or_init_sharedstreams_client(&self) -> Result<Arc<WrappedSharedStreamsClient>, WrappedError> {
        let mut locked = self.sharedstreams_client.lock().await;
        if let Some(client) = &*locked {
            return Ok(client.clone());
        }

        let tp = self.token_provider.as_ref().ok_or(WrappedError::GenericError {
            msg: "No TokenProvider available".into(),
        })?;
        let dsid = tp.get_dsid().await?;
        let mme_delegate = tp.parse_mme_delegate().await?;
        let account = tp.get_account();
        let anisette = account.lock().await.anisette.clone();
        let state_path = subsystem_state_path("sharedstreams-state.plist");
        let state = read_plist_state::<rustpush::sharedstreams::SharedStreamsState>(&state_path)
            .or_else(|| rustpush::sharedstreams::SharedStreamsState::new(dsid, &mme_delegate))
            .ok_or(WrappedError::GenericError {
                msg: "Failed to initialize Shared Streams state".into(),
            })?;
        let state_path_for_closure = state_path.clone();

        let wrapped = Arc::new(WrappedSharedStreamsClient {
            inner: Arc::new(
                rustpush::sharedstreams::SharedStreamClient::new(
                    state,
                    Box::new(move |state| persist_plist_state(&state_path_for_closure, state)),
                    tp.inner.clone(),
                    self.conn.clone(),
                    anisette,
                    self.os_config.clone(),
                )
                .await,
            ),
        });
        *locked = Some(wrapped.clone());
        Ok(wrapped)
    }
}

// Plain (non-FFI) impl for internal helpers that use upstream rustpush types
// (MessageInst, SendJob, PushError) not safe to cross the uniffi boundary.
impl Client {
    /// Force-refresh the MobileMe delegate and clear the cached CloudKit
    /// clients. Call this when a CloudKit sync fails with TokenMissing.
    ///
    /// Sequence matters here:
    ///
    ///   1. Refresh the PET first. Upstream's `refresh_mme` (auth.rs:176)
    ///      calls `get_gsa_token("com.apple.gs.idms.pet")` on line 179 and
    ///      errors `TokenMissing` if the PET is absent/expired. Apple PETs
    ///      have a ~24h TTL so after an overnight idle + bridge restart the
    ///      PET loaded from disk is stale, making `refresh_mme` always fail
    ///      with Token missing — the exact daily-auth-breaks symptom. The
    ///      persisted `AccountUsername` + `AccountHashedPasswordHex` + SPD
    ///      let us silently re-run `login_email_pass` via
    ///      `refresh_pet_token` to fetch a fresh PET from Apple without
    ///      user interaction.
    ///
    ///   2. Refresh the MME delegate. Now that the PET is fresh this hits
    ///      the normal success path and we get a new `cloudKitToken`.
    ///
    ///   3. Reset the cached CloudKit clients so the next sync picks up
    ///      the new delegate.
    ///
    /// Internal helper: not exposed via FFI. All call sites are inside this
    /// crate's Client impls.
    ///
    /// Rate limiting: `refresh_pet_token` hits Apple's GSA SRP endpoint
    /// (`login_email_pass`). CloudKit's caller loop can retry this helper
    /// many times per minute when Apple rejects the session. Without a
    /// circuit breaker we'd hammer Apple's auth endpoint at dozens of
    /// SRP handshakes per minute, which is exactly the pattern Apple's
    /// anti-abuse flags as credential stuffing — risk of account lockout.
    ///
    /// Two guards:
    ///   * `LAST_PET_REFRESH_MS`: global min-interval. Skip if a PET
    ///     refresh attempt happened in the last 60 s.
    ///   * `PET_REFRESH_STUCK_UNTIL_MS`: set to a future timestamp when
    ///     Apple returns `NeedsDevice2FA`. Further refresh calls no-op
    ///     until the cooldown elapses (1 h) or the process restarts.
    ///     Without this, every CloudKit retry triggers a fresh SRP attempt
    ///     knowing full well Apple is going to demand 2FA again.
    async fn refresh_mme_and_reset_cloud_client(&self) {
        use std::sync::atomic::AtomicU64;

        static LAST_PET_REFRESH_MS: AtomicU64 = AtomicU64::new(0);
        static PET_REFRESH_STUCK_UNTIL_MS: AtomicU64 = AtomicU64::new(0);
        const MIN_INTERVAL_MS: u64 = 60_000;
        const STUCK_COOLDOWN_MS: u64 = 60 * 60 * 1000; // 1 h

        let now_ms = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_millis() as u64)
            .unwrap_or(0);

        if let Some(tp) = &self.token_provider {
            let stuck_until = PET_REFRESH_STUCK_UNTIL_MS.load(Ordering::Relaxed);
            let last = LAST_PET_REFRESH_MS.load(Ordering::Relaxed);
            let skip_pet = now_ms < stuck_until || now_ms.saturating_sub(last) < MIN_INTERVAL_MS;

            if skip_pet {
                if now_ms < stuck_until {
                    warn!(
                        "Skipping refresh_pet_token: Apple returned NeedsDevice2FA recently; \
                         cooldown active for {}s more (manual re-login required)",
                        (stuck_until - now_ms) / 1000
                    );
                } else {
                    info!(
                        "Skipping refresh_pet_token: last attempt {}s ago; min interval {}s",
                        now_ms.saturating_sub(last) / 1000,
                        MIN_INTERVAL_MS / 1000
                    );
                }
            } else {
                LAST_PET_REFRESH_MS.store(now_ms, Ordering::Relaxed);
                // Detect NeedsDevice2FA by inspecting the account state after the
                // refresh call. refresh_pet_token is infallible by design
                // (swallows errors), so we can't rely on its return — we have to
                // look at the resulting token map.
                if let Err(e) = tp.refresh_pet_token().await {
                    warn!("refresh_pet_token before MME refresh failed: {:?}", e);
                }
                // If the PET slot is still empty after refresh, Apple is
                // blocking us (most commonly NeedsDevice2FA). Set the cooldown
                // so subsequent CloudKit TokenMissing retries don't redo SRP.
                // We can't inspect the FetchedToken's expiration (private
                // field) — presence alone is a good proxy because
                // login_email_pass only writes the map on success.
                let pet_present = {
                    let account = tp.account.lock().await;
                    account.tokens.contains_key("com.apple.gs.idms.pet")
                };
                if !pet_present {
                    PET_REFRESH_STUCK_UNTIL_MS
                        .store(now_ms + STUCK_COOLDOWN_MS, Ordering::Relaxed);
                    warn!(
                        "PET refresh produced no valid PET — engaging {}m cooldown to \
                         avoid hammering Apple auth (manual re-login likely required)",
                        STUCK_COOLDOWN_MS / 60_000
                    );
                }
            }

            // Always attempt refresh_mme — it's a cheap MobileMe call that
            // uses whatever PET is currently in memory. If nothing changed
            // it'll just log the same Token missing warning once.
            if let Err(e) = tp.inner.refresh_mme().await {
                warn!("refresh_mme failed during cloud-client recovery: {}", e);
            } else {
                info!("Cloud client recovery: refreshed MobileMe delegate");
            }
        }
        self.reset_cloud_client().await;
    }

    /// Retry client.send on PushError::SendTimedOut, reusing the same
    /// MessageInst (and therefore the same UUID) across attempts. A timed-out
    /// send may have already reached Apple; if we retry with a fresh
    /// MessageInst the second attempt carries a new UUID and Apple ends up
    /// with two messages. The delivery receipt for the first attempt then
    /// references a UUID we never persisted and the bridge drops it as
    /// "target message not in bridge DB". Reusing the msg keeps UUID stable
    /// across retries so at most one UUID ever reaches Apple.
    async fn send_with_flap_retry(
        &self,
        msg: &mut rustpush::MessageInst,
    ) -> Result<rustpush::SendJob, rustpush::PushError> {
        const MAX_ATTEMPTS: u32 = 3;
        for attempt in 0..MAX_ATTEMPTS {
            match self.client.send(msg).await {
                Ok(job) => return Ok(job),
                Err(rustpush::PushError::SendTimedOut) if attempt + 1 < MAX_ATTEMPTS => {
                    warn!(
                        "SendTimedOut on attempt {}/{} for uuid={}; retrying with same MessageInst",
                        attempt + 1,
                        MAX_ATTEMPTS,
                        msg.id
                    );
                    tokio::time::sleep(std::time::Duration::from_millis(
                        1000 + 1000 * attempt as u64,
                    ))
                    .await;
                }
                Err(e) => return Err(e),
            }
        }
        Err(rustpush::PushError::SendTimedOut)
    }
}

#[uniffi::export(async_runtime = "tokio")]
impl Client {
    /// Reset the cached CloudKit client so the next CloudKit operation
    /// re-initializes it with fresh auth tokens.  Call this after a sync
    /// failure (e.g. TokenMissing) before retrying.
    pub async fn reset_cloud_client(&self) {
        *self.cloud_messages_client.lock().await = None;
        *self.cloud_keychain_client.lock().await = None;
        *self.profiles_client.lock().await = None;
    }

    /// Retry an MMCS attachment download from a previously-captured descriptor
    /// blob. Called by the Go-side AttachmentRetrier when a push-time download
    /// failed and the queued row is due for another attempt. Internally reuses
    /// download_one_mmcs_attachment so the Layer-1 3-attempt retry with
    /// exponential backoff applies to each invocation.
    pub async fn retry_mmcs_from_descriptor(
        &self,
        descriptor_json: String,
        name: String,
    ) -> Result<Vec<u8>, WrappedError> {
        let descriptor: MmcsDescriptor =
            serde_json::from_str(&descriptor_json).map_err(|e| WrappedError::GenericError {
                msg: format!("retry_mmcs_from_descriptor: decode descriptor JSON: {e}"),
            })?;
        let mmcs = descriptor
            .to_file()
            .map_err(|e| WrappedError::GenericError {
                msg: format!("retry_mmcs_from_descriptor: rebuild MMCSFile: {e}"),
            })?;
        download_one_mmcs_attachment(&mmcs, &self.conn, &name)
            .await
            .map_err(|e| WrappedError::GenericError {
                msg: format!("retry_mmcs_from_descriptor {name}: {e}"),
            })
    }

    /// Reset the cached Shared Streams client and force-refresh the MME
    /// delegate so the next operation gets fresh auth tokens.
    pub async fn reset_sharedstreams_client(&self) {
        *self.sharedstreams_client.lock().await = None;
        if let Some(tp) = &self.token_provider {
            let _ = tp.inner.refresh_mme().await;
        }
    }

    /// Initialize the StatusKit presence system. Must be called after login
    /// when a TokenProvider is available. The callback receives presence
    /// updates for subscribed handles via the APNs receive loop.
    pub async fn init_statuskit(&self, callback: Box<dyn StatusCallback>) -> Result<(), WrappedError> {
        // Reuse the existing get_or_init_statuskit_client() path so the
        // WrappedStatusKitClient sub-client (used for share_status etc.)
        // and the shared raw handle (used by the receive loop) both point
        // at the same underlying StatusKitClient.
        let wrapped = self.get_or_init_statuskit_client().await?;
        *self.shared_statuskit.write().await = Some(wrapped.inner.clone());
        *self.status_callback.write().await = Some(Arc::from(callback));
        info!("StatusKit initialized — presence system ready");

        // Self-visibility diagnostic: query IDS for our own handle's
        // keysharing identities and compare the returned push tokens against
        // our own APS push token. If our token is absent, peer iOS cannot
        // include us when querying for this handle and will never reshare
        // their status key to us.
        let my_token = self.conn.get_token().await;
        let my_token_b64 = base64_encode(&my_token);
        let handles = self.client.identity.get_handles().await;
        info!(
            "StatusKit self-visibility: our APS push token={}, our handles={:?}",
            my_token_b64, handles
        );
        for handle in &handles {
            match self
                .client
                .identity
                .targets_for_handles(
                    "com.apple.private.alloy.status.keysharing",
                    std::slice::from_ref(handle),
                    handle,
                )
                .await
            {
                Ok(targets) => {
                    let mut self_visible = false;
                    let mut token_list: Vec<String> = Vec::new();
                    for t in &targets {
                        let tok_b64 = base64_encode(&t.delivery_data.push_token);
                        if t.delivery_data.push_token == my_token {
                            self_visible = true;
                            token_list.push(format!("{}(SELF)", tok_b64));
                        } else {
                            token_list.push(tok_b64);
                        }
                    }
                    if self_visible {
                        info!(
                            "StatusKit self-visibility OK for {}: IDS returned {} target(s), our token is present — tokens=[{}]",
                            handle,
                            targets.len(),
                            token_list.join(", ")
                        );
                    } else {
                        warn!(
                            "StatusKit self-visibility FAIL for {}: IDS returned {} target(s), our token is NOT present — peer iOS will never target this bridge when querying this handle. tokens=[{}]",
                            handle,
                            targets.len(),
                            token_list.join(", ")
                        );
                    }
                }
                Err(e) => {
                    warn!(
                        "StatusKit self-visibility: IDS query for {} failed: {:?}",
                        handle, e
                    );
                }
            }
        }

        Ok(())
    }

    /// Publish our own presence status. `active=true` → online;
    /// `active=false` → away/DND (mode defaults to do-not-disturb).
    pub async fn set_status(&self, active: bool) -> Result<(), WrappedError> {
        let sk = self.shared_statuskit.read().await.clone().ok_or(WrappedError::GenericError {
            msg: "StatusKit not initialized".into(),
        })?;
        let status = if active {
            rustpush::statuskit::StatusKitStatus::new_active()
        } else {
            rustpush::statuskit::StatusKitStatus::new_away("com.apple.donotdisturb.mode.default".to_string())
        };
        sk.share_status(&status).await.map_err(|e| WrappedError::GenericError {
            msg: format!("Failed to publish status: {}", e),
        })
    }

    /// Subscribe to presence updates for the given handles (e.g. "tel:+1...",
    /// "mailto:..."). The subscription is held internally until
    /// `unsubscribe_all_status()` is called.
    pub async fn subscribe_to_status(&self, handles: Vec<String>) -> Result<(), WrappedError> {
        let sk = self.shared_statuskit.read().await.clone().ok_or(WrappedError::GenericError {
            msg: "StatusKit not initialized".into(),
        })?;

        // Contacts may key back under a different handle form than their ghost
        // ID (e.g. mailto: vs tel:). Augment the ghost list with every "from"
        // handle persisted in statuskit-state.plist so request_handles matches
        // all available channels regardless of handle form.
        let ghost_count = handles.len();
        let mut augmented = handles;
        {
            let mut extra: Vec<String> = Vec::new();
            let state_path = subsystem_state_path("statuskit-state.plist");
            if let Ok(data) = std::fs::read(&state_path) {
                if let Ok(value) = plist::from_bytes::<plist::Value>(&data) {
                    if let Some(dict) = value.as_dictionary() {
                        if let Some(keys_dict) = dict.get("keys").and_then(|v| v.as_dictionary()) {
                            let handle_set: std::collections::HashSet<&str> =
                                augmented.iter().map(|s| s.as_str()).collect();
                            for (_channel_id, entry) in keys_dict {
                                if let Some(from_str) = entry.as_dictionary()
                                    .and_then(|d| d.get("from"))
                                    .and_then(|v| v.as_string())
                                {
                                    if !handle_set.contains(from_str) {
                                        extra.push(from_str.to_string());
                                    }
                                }
                            }
                        }
                    }
                }
            }
            augmented.extend(extra);
        }

        let token = sk.request_handles(&augmented).await;
        // Replace (not accumulate) interest tokens. subscribe_to_status
        // may be called multiple times (post-init + post-backfill + future
        // re-subscribe paths); without clearing, each call leaks the
        // previous token, pinning stale per-handle subscriptions on
        // StatusKitClient's internal channel-interest map. Callers treat
        // this function as "set the current subscription set", matching
        // OB-Android's `requestHandles(to: [participant])` semantics —
        // one authoritative interest list per chat activation.
        {
            let mut tokens = self.statuskit_interest_tokens.lock().await;
            let prev = tokens.len();
            tokens.clear();
            tokens.push(token);
            if prev > 0 {
                info!("Dropped {} previous interest token(s) before re-subscribing", prev);
            }
        }
        info!(
            "Requested presence subscription for {} handle(s) ({} ghost + {} from keys)",
            augmented.len(),
            ghost_count,
            augmented.len() - ghost_count,
        );
        Ok(())
    }

    /// Drop all presence subscriptions.
    pub async fn unsubscribe_all_status(&self) {
        self.statuskit_interest_tokens.lock().await.clear();
        info!("Unsubscribed from all presence channels");
    }

    /// Resolve a handle to all known handles for the same person by querying
    /// the IDS cache for a matching sender_correlation_identifier. If the handle
    /// isn't in the cache yet, triggers an IDS query to populate it.
    ///
    /// Returns a list of handles that share the same Apple ID as the input.
    /// For example, if the input is "mailto:user@icloud.com", the result might
    /// include "tel:+12012337620" if they belong to the same person.
    ///
    /// The `known_handles` parameter is a list of ghost handles from the bridge
    /// database — we check each one against the IDS cache for a matching
    /// correlation ID.
    pub async fn resolve_handle(&self, handle: String, known_handles: Vec<String>) -> Result<Vec<String>, WrappedError> {
        let my_handles = self.client.identity.get_handles().await;
        let my_handle = my_handles.first().ok_or(WrappedError::GenericError {
            msg: "no handle available".into(),
        })?.clone();

        // Try two IDS services in order:
        //   1. com.apple.madrid (iMessage) — the normal path
        //   2. com.apple.private.alloy.status.keysharing (StatusKit) — fallback
        //
        // Contacts who use iMessage only via their phone number have zero
        // Madrid keys for their Apple ID (mailto:) — IDS reports "zero keys".
        // However, they DO have StatusKit-keysharing keys because their device
        // sent us a key-sharing message using their Apple ID. The keysharing
        // service uses the same sender_correlation_identifier as Madrid, so
        // the same correlation-ID scan works; we just need the right service.
        //
        // Each validate_targets call is bounded to 5 s via tokio timeout so
        // a single-handle IDS query cannot block the goroutine indefinitely.
        const SERVICES: &[&str] = &[
            "com.apple.madrid",
            "com.apple.private.alloy.status.keysharing",
        ];

        for &service in SERVICES {
            match tokio::time::timeout(
                Duration::from_secs(5),
                self.client.identity.validate_targets(&[handle.clone()], service, &my_handle),
            ).await {
                Ok(Ok(_)) => {}
                Ok(Err(e)) => info!("resolve_handle: validate_targets({}) failed for {}: {:?}", service, handle, e),
                Err(_) => info!("resolve_handle: validate_targets({}) timed out after 5s for {}", service, handle),
            }

            let cache = self.client.identity.cache.lock().await;
            let correlation_id = match cache.get_correlation_id(service, &my_handle, &handle) {
                Some(cid) if !cid.is_empty() => cid,
                _ => {
                    info!("resolve_handle: no correlation ID for {} in {} — trying next service", handle, service);
                    continue; // try next service
                }
            };

            // Scan known handles for matching correlation IDs in this service.
            // known_handles are pre-populated from message processing (tel:) and
            // StatusKit invite responses (mailto:), so no extra IDS calls needed.
            let mut aliases = vec![];
            for known_handle in &known_handles {
                if known_handle == &handle {
                    continue;
                }
                if let Some(cid) = cache.get_correlation_id(service, &my_handle, known_handle) {
                    if cid == correlation_id {
                        aliases.push(known_handle.clone());
                    }
                }
            }

            info!("resolve_handle: {} → {} alias(es) via {} (correlation {})", handle, aliases.len(), service, correlation_id);
            return Ok(aliases);
        }

        info!("resolve_handle: no correlation ID for {} in any IDS service", handle);
        Ok(vec![])
    }

    /// Like resolve_handle but reads ONLY from the in-memory IDS cache —
    /// no network call, no validate_targets, no blocking. Returns the
    /// tel: (or other) aliases that share the same sender_correlation_identifier
    /// as `handle` according to data already cached from prior message processing.
    /// Returns an empty Vec if the handle is not in the cache yet.
    ///
    /// Checks both `com.apple.madrid` (populated by iMessage traffic) and
    /// `com.apple.private.alloy.status.keysharing` (populated by StatusKit
    /// invite/receive). At fresh startup the Madrid cache is empty until an
    /// iMessage is sent; the keysharing cache is populated as soon as
    /// invite_to_channel runs. Falling through to keysharing lets the
    /// periodic re-invite path find aliases on subsequent ticks.
    pub async fn resolve_handle_cached(&self, handle: String, known_handles: Vec<String>) -> Vec<String> {
        const SERVICES: &[&str] = &[
            "com.apple.madrid",
            "com.apple.private.alloy.status.keysharing",
        ];

        let my_handles = self.client.identity.get_handles().await;
        let my_handle = match my_handles.first() {
            Some(h) => h.clone(),
            None => return vec![],
        };

        let cache = self.client.identity.cache.lock().await;
        for &service in SERVICES {
            let correlation_id = match cache.get_correlation_id(service, &my_handle, &handle) {
                Some(cid) if !cid.is_empty() => cid,
                _ => continue,
            };

            let mut aliases = vec![];
            for known_handle in &known_handles {
                if known_handle == &handle {
                    continue;
                }
                if let Some(cid) = cache.get_correlation_id(service, &my_handle, known_handle) {
                    if cid == correlation_id {
                        aliases.push(known_handle.clone());
                    }
                }
            }
            info!("resolve_handle_cached: {} → {} alias(es) via {} (correlation {})", handle, aliases.len(), service, correlation_id);
            return aliases;
        }

        info!("resolve_handle_cached: {} not in IDS cache", handle);
        vec![]
    }

    /// Reset all StatusKit APNs channel cursors (last_msg_ns) to 1 in the
    /// persisted state file. Must be called BEFORE init_statuskit() so the
    /// StatusKit client loads the reset cursors on startup. Resetting to 1
    /// causes Apple's APNs server to replay the most recently published status
    /// for each channel (within the 7-day retention window), allowing the
    /// bridge to learn the current presence of contacts on startup instead of
    /// waiting for the next change. Keys and channel identifiers are preserved.
    pub fn reset_statuskit_cursors(&self) {
        let state_path = subsystem_state_path("statuskit-state.plist");
        let mut state = read_plist_state::<rustpush::statuskit::StatusKitState>(&state_path)
            .unwrap_or_default();
        let count = state.recent_channels.len();
        for ch in state.recent_channels.iter_mut() {
            ch.last_msg_ns = 1;
        }
        persist_plist_state(&state_path, &state);
        info!("Reset {} StatusKit channel cursor(s) to 1 for replay", count);
    }

    /// Send our StatusKit key to the specified contact handles (via IDS
    /// keysharing), establishing the mutual key exchange needed to receive
    /// their Focus/DND status updates. Should be called after init_statuskit()
    /// for handles that are not yet in the persisted key state. When the
    /// contact's device receives our key, it should respond by sending its own
    /// key, which the receive loop stores in the StatusKit state.
    pub async fn invite_to_status_sharing(
        &self,
        sender_handle: String,
        handles: Vec<String>,
    ) -> Result<(), WrappedError> {
        let sk = self.shared_statuskit.read().await.clone().ok_or(WrappedError::GenericError {
            msg: "StatusKit not initialized".into(),
        })?;

        statuskitgo::invite_keysharing(&sk, &sender_handle, &handles)
            .await
            .map_err(|e| WrappedError::GenericError {
                msg: format!("StatusKit invite: {:?}", e),
            })?;
        Ok(())
    }


    /// Fetch a shared iMessage profile (Name & Photo Sharing) from CloudKit.
    /// `record_key` and `decryption_key` are obtained from an incoming
    /// ShareProfile / UpdateProfile message.
    pub async fn fetch_profile(
        &self,
        record_key: String,
        decryption_key: Vec<u8>,
        has_poster: bool,
    ) -> Result<WrappedProfileRecord, WrappedError> {
        let key_prefix: String = record_key.chars().take(8).collect();
        let dec_len = decryption_key.len();
        info!(
            "fetch_profile: record_key={}… decryption_key_len={} has_poster={}",
            key_prefix, dec_len, has_poster
        );
        let profiles = self.get_or_init_profiles_client().await?;
        let share_msg = rustpush::ShareProfileMessage {
            cloud_kit_record_key: record_key,
            cloud_kit_decryption_record_key: decryption_key,
            poster: if has_poster {
                // Minimal poster struct — the fetch path only checks
                // `poster.is_some()` to decide whether to pull the wallpaper
                // record alongside the profile record.
                Some(rustpush::SharedPoster {
                    low_res_wallpaper_tag: vec![],
                    wallpaper_tag: vec![],
                    message_tag: vec![],
                })
            } else {
                None
            },
        };
        let record = profiles.get_record(&share_msg).await.map_err(|e| {
            warn!(
                "fetch_profile failed (record_key={}…, has_poster={}): {:?}",
                key_prefix, has_poster, e
            );
            WrappedError::GenericError {
                msg: format!("Failed to fetch profile: {:?}", e),
            }
        })?;
        Ok(WrappedProfileRecord {
            display_name: record.name.name,
            first_name: record.name.first,
            last_name: record.name.last,
            avatar: record.image,
        })
    }

    pub async fn get_handles(&self) -> Vec<String> {
        self.client.identity.get_handles().await
    }

    /// Returns the list of IDS service names present in this user's registration
    /// map (e.g. "com.apple.madrid", "com.apple.private.alloy.multiplex1").
    /// StatusKit key exchange requires "com.apple.private.alloy.multiplex1" to
    /// appear in this list; if it doesn't, we never subscribe to the keysharing
    /// topic and contacts' c=227 replies never reach us.
    pub async fn get_registered_services(&self) -> Vec<String> {
        let users = self.client.identity.users.read().await;
        if users.is_empty() {
            return vec![];
        }
        users[0].registration.keys().cloned().collect()
    }

    /// Get iCloud auth headers (Authorization + anisette) for MobileMe API calls.
    /// Returns None if no token provider is available.
    pub async fn get_icloud_auth_headers(&self) -> Result<Option<HashMap<String, String>>, WrappedError> {
        match &self.token_provider {
            Some(tp) => Ok(Some(tp.get_icloud_auth_headers().await?)),
            None => Ok(None),
        }
    }

    /// Get the contacts CardDAV URL from the MobileMe delegate.
    /// Returns None if no token provider is available.
    pub async fn get_contacts_url(&self) -> Result<Option<String>, WrappedError> {
        match &self.token_provider {
            Some(tp) => Ok(tp.get_contacts_url().await?),
            None => Ok(None),
        }
    }

    /// Get the DSID for this account.
    pub async fn get_dsid(&self) -> Result<Option<String>, WrappedError> {
        match &self.token_provider {
            Some(tp) => Ok(Some(tp.get_dsid().await?)),
            None => Ok(None),
        }
    }

    pub async fn get_findmy_client(&self) -> Result<Arc<WrappedFindMyClient>, WrappedError> {
        self.get_or_init_findmy_client().await
    }

    pub async fn get_facetime_client(&self) -> Result<Arc<WrappedFaceTimeClient>, WrappedError> {
        self.get_or_init_facetime_client().await
    }

    pub async fn get_passwords_client(&self) -> Result<Arc<WrappedPasswordsClient>, WrappedError> {
        self.get_or_init_passwords_client().await
    }

    pub async fn get_statuskit_client(&self) -> Result<Arc<WrappedStatusKitClient>, WrappedError> {
        self.get_or_init_statuskit_client().await
    }

    pub async fn get_sharedstreams_client(&self) -> Result<Arc<WrappedSharedStreamsClient>, WrappedError> {
        self.get_or_init_sharedstreams_client().await
    }

    pub async fn findmy_phone_refresh_json(&self) -> Result<String, WrappedError> {
        let tp = self.token_provider.as_ref().ok_or(WrappedError::GenericError {
            msg: "No TokenProvider available".into(),
        })?;
        let dsid = tp.get_dsid().await?;
        let account = tp.get_account();
        let anisette = account.lock().await.anisette.clone();

        let mut client = rustpush::findmy::FindMyPhoneClient::new(
            self.os_config.as_ref(),
            dsid,
            self.conn.clone(),
            anisette,
            tp.inner.clone(),
        )
        .await?;
        client.refresh(self.os_config.as_ref()).await?;

        serde_json::to_string(&client.devices).map_err(|e| WrappedError::GenericError {
            msg: format!("Failed to encode Find My Phone devices: {}", e),
        })
    }

    pub async fn findmy_friends_refresh_json(&self, daemon: bool) -> Result<String, WrappedError> {
        let tp = self.token_provider.as_ref().ok_or(WrappedError::GenericError {
            msg: "No TokenProvider available".into(),
        })?;
        let dsid = tp.get_dsid().await?;
        let account = tp.get_account();
        let anisette = account.lock().await.anisette.clone();

        let mut client = rustpush::findmy::FindMyFriendsClient::new(
            self.os_config.as_ref(),
            dsid,
            tp.inner.clone(),
            self.conn.clone(),
            anisette,
            daemon,
        )
        .await?;
        client.refresh(self.os_config.as_ref()).await?;

        #[derive(serde::Serialize)]
        struct FriendsSnapshot {
            selected_friend: Option<String>,
            followers: Vec<rustpush::findmy::Follow>,
            following: Vec<rustpush::findmy::Follow>,
        }

        serde_json::to_string(&FriendsSnapshot {
            selected_friend: client.selected_friend,
            followers: client.followers,
            following: client.following,
        })
        .map_err(|e| WrappedError::GenericError {
            msg: format!("Failed to encode Find My Friends snapshot: {}", e),
        })
    }

    pub async fn findmy_friends_import(&self, daemon: bool, url: String) -> Result<(), WrappedError> {
        let tp = self.token_provider.as_ref().ok_or(WrappedError::GenericError {
            msg: "No TokenProvider available".into(),
        })?;
        let dsid = tp.get_dsid().await?;
        let account = tp.get_account();
        let anisette = account.lock().await.anisette.clone();

        let mut client = rustpush::findmy::FindMyFriendsClient::new(
            self.os_config.as_ref(),
            dsid,
            tp.inner.clone(),
            self.conn.clone(),
            anisette,
            daemon,
        )
        .await?;
        client.import(self.os_config.as_ref(), &url).await?;
        Ok(())
    }

    pub async fn validate_targets(
        &self,
        targets: Vec<String>,
        handle: String,
    ) -> Vec<String> {
        self.client
            .identity
            .validate_targets(&targets, "com.apple.madrid", &handle)
            .await
            .unwrap_or_default()
    }

    pub async fn send_message(
        &self,
        conversation: WrappedConversation,
        text: String,
        html: Option<String>,
        handle: String,
        reply_guid: Option<String>,
        reply_part: Option<String>,
        scheduled_ms: Option<u64>,
    ) -> Result<String, WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let service = if conversation.is_sms {
            MessageType::SMS {
                is_phone: false,
                using_number: handle.clone(),
                from_handle: None,
            }
        } else {
            MessageType::IMessage
        };

        // Parse rich link encoded as prefix: \x00RL\x01original_url\x01url\x01title\x01summary\x00actual_text
        let (actual_text, link_meta) = if text.starts_with("\x00RL\x01") {
            let rest = &text[4..]; // skip "\x00RL\x01"
            if let Some(end) = rest.find('\x00') {
                let metadata = &rest[..end];
                let actual = rest[end + 1..].to_string();
                let fields: Vec<&str> = metadata.splitn(4, '\x01').collect();
                let original_url_str = fields.first().copied().unwrap_or("");
                let url_str = fields.get(1).copied().unwrap_or("");
                let title_str = fields.get(2).copied().unwrap_or("");
                let summary_str = fields.get(3).copied().unwrap_or("");

                let original_url = NSURL {
                    base: "$null".to_string(),
                    relative: original_url_str.to_string(),
                };
                let url = if url_str.is_empty() {
                    None
                } else {
                    Some(NSURL {
                        base: "$null".to_string(),
                        relative: url_str.to_string(),
                    })
                };
                let title = if title_str.is_empty() { None } else { Some(title_str.to_string()) };
                let summary = if summary_str.is_empty() { None } else { Some(summary_str.to_string()) };

                info!("Sending rich link: url={}, title={:?}", original_url_str, title);

                let lm = LinkMeta {
                    data: LPLinkMetadata {
                        image_metadata: None,
                        version: 1,
                        icon_metadata: None,
                        original_url: Some(original_url),
                        url,
                        title,
                        summary,
                        image: None,
                        icon: None,
                        images: None,
                        icons: None,
                        is_incomplete: None,
                        uses_activity_pub: None,
                        is_encoded_for_local_use: None,
                        collaboration_type: None,
                        specialization2: None,
                    },
                    attachments: vec![],
                };
                (actual, Some(lm))
            } else {
                (text, None)
            }
        } else {
            (text, None)
        };

        let parts = if let Some(ref html_str) = html {
            parse_html_to_parts(html_str, &actual_text)
        } else {
            None
        };

        let schedule = scheduled_ms.map(|ms| ScheduleMode { ms, schedule: true });

        let normal = if let Some(parts) = parts {
            NormalMessage {
                parts,
                effect: None,
                reply_guid: reply_guid.clone(),
                reply_part: reply_part.clone(),
                service: service.clone(),
                subject: None,
                app: None,
                link_meta,
                voice: false,
                scheduled: schedule,
                embedded_profile: None,
            }
        } else {
            let mut n = NormalMessage::new(actual_text.clone(), service.clone());
            n.link_meta = link_meta;
            n.reply_guid = reply_guid.clone();
            n.reply_part = reply_part.clone();
            n.scheduled = schedule;
            n
        };
        let mut msg = MessageInst::new(
            conv.clone(),
            &handle,
            Message::Message(normal),
        );
        match self.send_with_flap_retry(&mut msg).await {
            Ok(_) => Ok(msg.id.clone()),
            Err(rustpush::PushError::NoValidTargets) if !conversation.is_sms => {
                // iMessage failed — no IDS targets. Retry as SMS (without rich link).
                info!("No IDS targets, falling back to SMS for {:?}", conv.participants);
                let sms_service = MessageType::SMS {
                    is_phone: false,
                    using_number: handle.clone(),
                    from_handle: None,
                };
                let mut sms_msg = MessageInst::new(
                    conv,
                    &handle,
                    Message::Message(NormalMessage::new(actual_text, sms_service)),
                );
                self.send_with_flap_retry(&mut sms_msg).await
                    .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send SMS: {}", e) })?;
                Ok(sms_msg.id.clone())
            }
            Err(e) => Err(WrappedError::GenericError { msg: format!("Failed to send message: {}", e) }),
        }
    }

    pub async fn send_tapback(
        &self,
        conversation: WrappedConversation,
        target_uuid: String,
        target_part: u64,
        reaction: u32,
        emoji: Option<String>,
        remove: bool,
        handle: String,
    ) -> Result<String, WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let reaction_val = match (reaction, &emoji) {
            (0, _) => Reaction::Heart,
            (1, _) => Reaction::Like,
            (2, _) => Reaction::Dislike,
            (3, _) => Reaction::Laugh,
            (4, _) => Reaction::Emphasize,
            (5, _) => Reaction::Question,
            (6, Some(em)) => Reaction::Emoji(em.clone()),
            _ => Reaction::Heart,
        };
        let mut msg = MessageInst::new(
            conv,
            &handle,
            Message::React(ReactMessage {
                to_uuid: target_uuid,
                to_part: Some(target_part),
                reaction: ReactMessageType::React { reaction: reaction_val, enable: !remove },
                to_text: String::new(),
                embedded_profile: None,
            }),
        );
        self.send_with_flap_retry(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send tapback: {}", e) })?;
        Ok(msg.id.clone())
    }

    pub async fn send_typing(
        &self,
        conversation: WrappedConversation,
        typing: bool,
        handle: String,
    ) -> Result<(), WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let mut msg = MessageInst::new(conv, &handle, Message::Typing(typing, None));
        self.client.send(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send typing: {}", e) })?;
        Ok(())
    }

    pub async fn send_typing_with_app(
        &self,
        conversation: WrappedConversation,
        typing: bool,
        handle: String,
        bundle_id: String,
        icon: Vec<u8>,
    ) -> Result<(), WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let app = TypingApp { bundle_id, icon };
        let mut msg = MessageInst::new(conv, &handle, Message::Typing(typing, Some(app)));
        self.client.send(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send typing with app: {}", e) })?;
        Ok(())
    }

    pub async fn send_read_receipt(
        &self,
        conversation: WrappedConversation,
        handle: String,
        for_uuid: Option<String>,
    ) -> Result<(), WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let mut msg = MessageInst::new(conv, &handle, Message::Read);
        if let Some(uuid) = for_uuid {
            msg.id = uuid;
        }
        self.client.send(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send read receipt: {}", e) })?;
        Ok(())
    }

    pub async fn send_delivery_receipt(
        &self,
        conversation: WrappedConversation,
        handle: String,
    ) -> Result<(), WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let mut msg = MessageInst::new(conv, &handle, Message::Delivered);
        self.client.send(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send delivery receipt: {}", e) })?;
        Ok(())
    }

    pub async fn send_edit(
        &self,
        conversation: WrappedConversation,
        target_uuid: String,
        edit_part: u64,
        new_text: String,
        handle: String,
    ) -> Result<String, WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let mut msg = MessageInst::new(
            conv,
            &handle,
            Message::Edit(EditMessage {
                tuuid: target_uuid,
                edit_part,
                new_parts: MessageParts(vec![IndexedMessagePart {
                    part: MessagePart::Text(new_text, Default::default()),
                    idx: None,
                    ext: None,
                }]),
            }),
        );
        self.send_with_flap_retry(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send edit: {}", e) })?;
        Ok(msg.id.clone())
    }

    pub async fn send_unsend(
        &self,
        conversation: WrappedConversation,
        target_uuid: String,
        edit_part: u64,
        handle: String,
    ) -> Result<String, WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let mut msg = MessageInst::new(
            conv,
            &handle,
            Message::Unsend(UnsendMessage {
                tuuid: target_uuid,
                edit_part,
            }),
        );
        self.send_with_flap_retry(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send unsend: {}", e) })?;
        Ok(msg.id.clone())
    }

    /// Send a MoveToRecycleBin message to notify other Apple devices that a chat was deleted.
    pub async fn send_move_to_recycle_bin(
        &self,
        conversation: WrappedConversation,
        handle: String,
        chat_guid: String,
    ) -> Result<(), WrappedError> {
        let conv: ConversationData = (&conversation).into();
        // Strip mailto:/tel: prefixes and exclude the sender's own handle.
        // Apple's OperatedChat.ptcpts contains only the OTHER party's handles,
        // not the user's own handle. Including it causes the Mac to not
        // recognise the chat being deleted.
        let bare_handle = handle.replace("mailto:", "").replace("tel:", "");
        let bare_participants: Vec<String> = conv.participants.iter()
            .map(|p| p.replace("mailto:", "").replace("tel:", ""))
            .filter(|p| p != &bare_handle)
            .collect();
        let operated_chat = OperatedChat {
            participants: bare_participants,
            group_id: conv.sender_guid.clone().unwrap_or_default(),
            guid: chat_guid,
            delete_incoming_messages: None,
            was_reported_as_junk: None,
        };
        let delete_msg = MoveToRecycleBinMessage {
            target: DeleteTarget::Chat(operated_chat.clone()),
            recoverable_delete_date: std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map(|d| d.as_millis() as u64)
                .unwrap_or(0),
        };
        let mut msg = MessageInst::new(conv, &handle, Message::MoveToRecycleBin(delete_msg));
        self.client.send(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send MoveToRecycleBin: {}", e) })?;
        // Note: We intentionally do NOT send PermanentDelete. MoveToRecycleBin
        // moves the chat to Apple's "Recently Deleted" (30-day retention),
        // respecting the user's recycle bin. The chat can be restored with
        // !restore-chat which re-uploads the record to CloudKit.
        Ok(())
    }

    /// Send a RecoverChat APNs message (command 182) to notify other Apple
    /// devices that a chat has been recovered from the recycle bin.
    /// This is the inverse of send_move_to_recycle_bin.
    pub async fn send_recover_chat(
        &self,
        conversation: WrappedConversation,
        handle: String,
        chat_guid: String,
    ) -> Result<(), WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let bare_handle = handle.replace("mailto:", "").replace("tel:", "");
        let bare_participants: Vec<String> = conv.participants.iter()
            .map(|p| p.replace("mailto:", "").replace("tel:", ""))
            .filter(|p| p != &bare_handle)
            .collect();
        let operated_chat = OperatedChat {
            participants: bare_participants,
            group_id: conv.sender_guid.clone().unwrap_or_default(),
            guid: chat_guid,
            delete_incoming_messages: None,
            was_reported_as_junk: None,
        };
        let mut msg = MessageInst::new(conv, &handle, Message::RecoverChat(operated_chat));
        self.client.send(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send RecoverChat: {}", e) })?;
        Ok(())
    }

    /// Restore a chat record to CloudKit so it reappears on Apple devices.
    /// Re-uploads the chat data using save_chats, which creates or overwrites
    /// the record in chatManateeZone. Used by restore-chat to sync un-deletion
    /// back to Apple devices.
    pub async fn restore_cloud_chat(
        &self,
        record_name: String,
        chat_identifier: String,
        group_id: String,
        style: i64,
        service: String,
        display_name: Option<String>,
        participants: Vec<String>,
    ) -> Result<(), WrappedError> {
        let cloud_messages = self.get_or_init_cloud_messages_client().await?;
        let chat = rustpush::cloud_messages::CloudChat {
            style,
            is_filtered: 0,
            successful_query: 1,
            state: 3,
            chat_identifier,
            group_id: group_id.clone(),
            service_name: service,
            original_group_id: group_id,
            properties: None,
            participants: participants.into_iter().map(|uri| rustpush::cloud_messages::CloudParticipant { uri }).collect(),
            prop001: Default::default(),
            last_read_message_timestamp: 0,
            last_addressed_handle: String::new(),
            guid: String::new(),
            display_name,
            proto001: None,
            group_photo_guid: None,
            group_photo: None,
        };
        let mut chats = std::collections::HashMap::new();
        chats.insert(record_name.clone(), chat);
        let results = cloud_messages.save_chats(chats).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to restore CloudKit chat: {}", e) })?;
        // Check individual result
        if let Some(Err(e)) = results.get(&record_name) {
            return Err(WrappedError::GenericError { msg: format!("Failed to save restored chat record: {}", e) });
        }
        Ok(())
    }

    /// Delete chat records from CloudKit so they don't reappear during future syncs.
    pub async fn delete_cloud_chats(
        &self,
        chat_ids: Vec<String>,
    ) -> Result<(), WrappedError> {
        let cloud_messages = self.get_or_init_cloud_messages_client().await?;
        cloud_messages.delete_chats(&chat_ids).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to delete CloudKit chats: {}", e) })?;
        Ok(())
    }

    /// Delete message records from CloudKit so they don't reappear during future syncs.
    pub async fn delete_cloud_messages(
        &self,
        message_ids: Vec<String>,
    ) -> Result<(), WrappedError> {
        let cloud_messages = self.get_or_init_cloud_messages_client().await?;
        cloud_messages.delete_messages(&message_ids).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to delete CloudKit messages: {}", e) })?;
        Ok(())
    }

    /// Purge the recoverable-delete zones from CloudKit to prevent deleted
    /// messages from resurrecting. After MoveToRecycleBin, Apple parks records
    /// in recoverableMessageDeleteZone; nuking that zone ensures they stay dead.
    pub async fn purge_recoverable_zones(&self) -> Result<(), WrappedError> {
        let cloud_messages = self.get_or_init_cloud_messages_client().await?;
        let container = cloud_messages.get_container().await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to get CloudKit container: {}", e) })?;
        container.perform_operations_checked(
            &CloudKitSession::new(),
            &[
                ZoneDeleteOperation::new(container.private_zone("recoverableMessageDeleteZone".to_string())),
                ZoneDeleteOperation::new(container.private_zone("chatBotRecoverableMessageDeleteZone".to_string())),
            ],
            IsolationLevel::Operation,
        ).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to purge recoverable zones: {}", e) })?;
        Ok(())
    }

    /// List chat records from Apple's "Recently Deleted" recycle bin.
    /// Paginates through all known recoverable-delete zones and returns all
    /// non-tombstone chat records found there.
    pub async fn list_recoverable_chats(&self) -> Result<Vec<WrappedCloudSyncChat>, WrappedError> {
        use rustpush::cloudkit::{pcs_keys_for_record, FetchRecordChangesOperation, CloudKitSession, NO_ASSETS};
        use rustpush::cloudkit_proto::CloudKitRecord;
        use rustpush::cloud_messages::{MESSAGES_SERVICE, CloudChat};

        info!("list_recoverable_chats: starting recycle bin query");

        let cloud_messages = self.get_or_init_cloud_messages_client().await?;
        let container = cloud_messages.get_container().await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to get CloudKit container: {}", e) })?;

        let mut all_chats = Vec::new();
        let mut seen_record_names = std::collections::HashSet::new();
        // Accumulate diagnostics so they appear in the final summary log
        // (which we KNOW gets output even if per-page logs are lost).
        let mut zone_diag: Vec<String> = Vec::new();

        for zone_name in ["recoverableMessageDeleteZone", "chatBotRecoverableMessageDeleteZone"] {
            let mut token: Option<Vec<u8>> = None;
            let mut zone_total_changes = 0usize;
            let mut zone_tombstones = 0usize;
            let mut zone_non_chat = 0usize;
            let mut zone_pcs_err = 0usize;
            let mut zone_panic = 0usize;
            let mut zone_incomplete = 0usize;
            let mut zone_accepted = 0usize;
            let mut zone_pages = 0usize;
            let mut zone_last_status = 0i32;

            debug!("list_recoverable_chats: fetching zone encryption config for {}", zone_name);

            for page in 0..256 {
                zone_pages = page + 1;
                let zone_id = container.private_zone(zone_name.to_string());
                let key = container
                    .get_zone_encryption_config(&zone_id, &cloud_messages.keychain, &MESSAGES_SERVICE)
                    .await
                    .map_err(|e| WrappedError::GenericError {
                        msg: format!("Failed to get zone key for {}: {}", zone_name, e),
                    })?;

                debug!("list_recoverable_chats: performing FetchRecordChanges on {} page {}", zone_name, page);

                let (_assets, response) = container
                    .perform(
                        &CloudKitSession::new(),
                        FetchRecordChangesOperation::new(zone_id, token, &NO_ASSETS),
                    )
                    .await
                    .map_err(|e| WrappedError::GenericError {
                        msg: format!("Failed to fetch recoverable chats from {} page {}: {}", zone_name, page, e),
                    })?;

                let changes_count = response.change.len();
                let status = response.status();
                zone_last_status = status;
                zone_total_changes += changes_count;
                debug!("list_recoverable_chats: zone={} page={} changes={} status={}", zone_name, page, changes_count, status);

                for change in &response.change {
                    let id_proto = match change.identifier.as_ref() {
                        Some(p) => p,
                        None => continue,
                    };
                    let id_value = match id_proto.value.as_ref() {
                        Some(v) => v,
                        None => continue,
                    };
                    let identifier = id_value.name().to_string();

                    let record = match &change.record {
                        Some(r) => r,
                        None => {
                            zone_tombstones += 1;
                            debug!("list_recoverable_chats: tombstone record {} in {}", identifier, zone_name);
                            continue;
                        }
                    };

                    let record_type = record
                        .r#type
                        .as_ref()
                        .map(|t| t.name().to_string())
                        .unwrap_or_else(|| "<missing>".to_string());
                    let field_names = recoverable_record_field_names(&record.record_field);
                    if !record_looks_chat_like(&record.record_field) {
                        zone_non_chat += 1;
                        debug!("list_recoverable_chats: skipping non-chat record {} in {} type={} fields={:?}", identifier, zone_name, record_type, field_names);
                        continue;
                    }

                    let pcskey = match pcs_keys_for_record(record, &key) {
                        Ok(k) => k,
                        Err(e) => {
                            zone_pcs_err += 1;
                            warn!("list_recoverable_chats: skipping record {} in {}: PCS key error: {}", identifier, zone_name, e);
                            continue;
                        }
                    };

                    let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                        CloudChat::from_record_encrypted(&record.record_field, Some(&pcskey))
                    }));

                    match result {
                        Ok(chat) => {
                            if !seen_record_names.insert(identifier.clone()) {
                                continue;
                            }
                            if let Some(chat) = wrap_recoverable_chat(identifier.clone(), chat) {
                                zone_accepted += 1;
                                if record_type != CloudChat::record_type() {
                                    info!(
                                        "list_recoverable_chats: accepted recycle-bin record {} in {} with nonstandard type {}",
                                        identifier, zone_name, record_type
                                    );
                                }
                                all_chats.push(chat);
                            } else {
                                zone_incomplete += 1;
                                warn!(
                                    "list_recoverable_chats: skipping record {} in {}: chat-like fields decoded to incomplete chat (type={}, fields={:?})",
                                    identifier,
                                    zone_name,
                                    record_type,
                                    recoverable_record_field_names(&record.record_field),
                                );
                            }
                        }
                        Err(e) => {
                            zone_panic += 1;
                            let msg = if let Some(s) = e.downcast_ref::<String>() { s.clone() }
                                      else if let Some(s) = e.downcast_ref::<&str>() { s.to_string() }
                                      else { "unknown panic".to_string() };
                            warn!(
                                "list_recoverable_chats: skipping record {} in {}: deserialization panic: {} (type={}, fields={:?})",
                                identifier,
                                zone_name,
                                msg,
                                record_type,
                                recoverable_record_field_names(&record.record_field),
                            );
                        }
                    }
                }

                let next_token = response.sync_continuation_token().to_vec();
                if status == 3 || next_token.is_empty() {
                    break;
                }
                token = Some(next_token);
            }

            zone_diag.push(format!(
                "{}:pages={},changes={},status={},tombstones={},non_chat={},pcs_err={},panic={},incomplete={},accepted={}",
                zone_name, zone_pages, zone_total_changes, zone_last_status,
                zone_tombstones, zone_non_chat, zone_pcs_err, zone_panic, zone_incomplete, zone_accepted
            ));
        }

        // Single summary line with ALL diagnostic info embedded.
        // This line is confirmed to appear in logs — embed everything here.
        info!("list_recoverable_chats: found {} chat(s) in recycle bin [{}]", all_chats.len(), zone_diag.join(" | "));
        Ok(all_chats)
    }

    /// Read all message GUIDs from Apple's recoverableMessageDeleteZone.
    /// These are the UUIDs of individually deleted messages (moved to trash).
    /// When an entire chat is deleted, ALL its messages end up here.
    /// Returns the list of message GUIDs that can be matched against
    /// cloud_message.uuid to identify deleted chats.
    /// Reads both recoverable message zones and returns message GUIDs.
    /// These GUIDs match cloud_message.guid in the local DB. Entries may also
    /// include base64-encoded metadata after `guid|...`; GUID-only callers can
    /// safely ignore the suffix.
    pub async fn list_recoverable_message_guids(&self) -> Result<Vec<String>, WrappedError> {
        use std::collections::HashSet;
        use rustpush::cloudkit::{pcs_keys_for_record, FetchRecordChangesOperation, CloudKitSession, NO_ASSETS};
        use rustpush::cloudkit_proto::CloudKitRecord;
        use rustpush::cloud_messages::{MESSAGES_SERVICE, CloudMessage};

        info!("list_recoverable_message_guids: starting");
        let cloud_messages = self.get_or_init_cloud_messages_client().await?;
        let container = cloud_messages.get_container().await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to get CloudKit container: {}", e) })?;

        let mut guids = Vec::new();
        let mut seen = HashSet::new();
        let mut zone_diag = Vec::new();
        let mut successful_zones = 0usize;
        let mut zone_errors = Vec::new();

        for zone_name in ["recoverableMessageDeleteZone", "chatBotRecoverableMessageDeleteZone"] {
            let mut token: Option<Vec<u8>> = None;
            let zone_id = container.private_zone(zone_name.to_string());
            let key = match container
                .get_zone_encryption_config(&zone_id, &cloud_messages.keychain, &MESSAGES_SERVICE)
                .await
            {
                Ok(key) => {
                    successful_zones += 1;
                    key
                }
                Err(e) => {
                    let err = format!("{}: {}", zone_name, e);
                    warn!("list_recoverable_message_guids: failed to get zone key for {}", err);
                    zone_errors.push(err);
                    continue;
                }
            };

            let mut zone_guid_count = 0usize;
            for page in 0..256 {
                let zone_id = container.private_zone(zone_name.to_string());
                let (_assets, response) = match container
                    .perform(
                        &CloudKitSession::new(),
                        FetchRecordChangesOperation::new(zone_id, token, &NO_ASSETS),
                    )
                    .await
                {
                    Ok(result) => result,
                    Err(e) => {
                        let err = format!("{} page {}: {}", zone_name, page, e);
                        warn!("list_recoverable_message_guids: failed to fetch {}", err);
                        zone_errors.push(err);
                        break;
                    }
                };

                let changes_count = response.change.len();
                let status = response.status();

                for change in &response.change {
                    let identifier = change.identifier.as_ref()
                        .and_then(|i| i.value.as_ref())
                        .map(|v| v.name().to_string())
                        .unwrap_or_default();
                    let record = match &change.record {
                        Some(r) => r,
                        None => continue,
                    };

                    let record_type = record.r#type.as_ref()
                        .map(|t| t.name().to_string())
                        .unwrap_or_default();
                    if record_type != "recoverableMessage" {
                        continue;
                    }

                    let pcskey = match pcs_keys_for_record(record, &key) {
                        Ok(k) => k,
                        Err(_) => continue,
                    };
                    let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                        CloudMessage::from_record_encrypted(&record.record_field, Some(&pcskey))
                    }));
                    if let Ok(msg) = result {
                        if !msg.guid.is_empty() && seen.insert(msg.guid.clone()) {
                            guids.push(encode_recoverable_message_entry(&msg.guid, &identifier, &msg));
                            zone_guid_count += 1;
                        }
                    }
                }

                info!("list_recoverable_message_guids: zone={} page={} changes={} status={} guids_so_far={}", zone_name, page, changes_count, status, guids.len());

                let next_token = response.sync_continuation_token().to_vec();
                if status == 3 || next_token.is_empty() {
                    break;
                }
                token = Some(next_token);
            }
            zone_diag.push(format!("{}:{}", zone_name, zone_guid_count));
        }

        if successful_zones == 0 && !zone_errors.is_empty() {
            return Err(WrappedError::GenericError {
                msg: format!("Failed to read recoverable message zones: {}", zone_errors.join(" | ")),
            });
        }

        info!("list_recoverable_message_guids: found {} deleted message GUID(s) [{}]", guids.len(), zone_diag.join(" | "));
        Ok(guids)
    }

    /// Raw diagnostic dump of the recoverable zone. Returns a human-readable
    /// string describing every change record in the zone (not just chat-like
    /// ones). Used by the !debug-recycle-bin bridgebot command.
    pub async fn debug_recoverable_zones(&self) -> Result<String, WrappedError> {
        use rustpush::cloudkit::{FetchRecordChangesOperation, CloudKitSession, NO_ASSETS};
        use rustpush::cloud_messages::MESSAGES_SERVICE;

        let cloud_messages = self.get_or_init_cloud_messages_client().await?;
        let container = cloud_messages.get_container().await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to get CloudKit container: {}", e) })?;

        let mut lines = Vec::new();

        for zone_name in ["recoverableMessageDeleteZone", "chatBotRecoverableMessageDeleteZone"] {
            let mut token: Option<Vec<u8>> = None;
            let mut total_changes = 0usize;

            // Try to get zone encryption config — if this fails, report the error
            let zone_id = container.private_zone(zone_name.to_string());
            let key_result = container
                .get_zone_encryption_config(&zone_id, &cloud_messages.keychain, &MESSAGES_SERVICE)
                .await;
            let _key = match key_result {
                Ok(k) => {
                    lines.push(format!("✅ Zone {} — encryption config OK", zone_name));
                    k
                }
                Err(e) => {
                    lines.push(format!("❌ Zone {} — encryption config FAILED: {}", zone_name, e));
                    continue;
                }
            };

            for page in 0..10 {
                let zone_id = container.private_zone(zone_name.to_string());
                let fetch_result = container
                    .perform(
                        &CloudKitSession::new(),
                        FetchRecordChangesOperation::new(zone_id, token, &NO_ASSETS),
                    )
                    .await;

                let (_assets, response) = match fetch_result {
                    Ok(r) => r,
                    Err(e) => {
                        lines.push(format!("  ❌ Page {} fetch FAILED: {}", page, e));
                        break;
                    }
                };

                let status = response.status();
                let changes = response.change.len();
                total_changes += changes;
                lines.push(format!("  Page {}: {} changes, status={}", page, changes, status));

                for change in &response.change {
                    let id = change.identifier.as_ref()
                        .and_then(|i| i.value.as_ref())
                        .map(|v| v.name().to_string())
                        .unwrap_or_else(|| "<no-id>".to_string());

                    match &change.record {
                        None => {
                            lines.push(format!("    [tombstone] {}", id));
                        }
                        Some(record) => {
                            let rtype = record.r#type.as_ref()
                                .map(|t| t.name().to_string())
                                .unwrap_or_else(|| "<missing>".to_string());
                            let fields = recoverable_record_field_names(&record.record_field);
                            let chat_like = record_looks_chat_like(&record.record_field);
                            lines.push(format!("    [record] {} type={} chat_like={} fields={:?}", id, rtype, chat_like, fields));
                        }
                    }
                }

                let next_token = response.sync_continuation_token().to_vec();
                if status == 3 || next_token.is_empty() {
                    break;
                }
                token = Some(next_token);
            }

            lines.push(format!("  Total: {} changes in {}", total_changes, zone_name));
        }

        Ok(lines.join("\n"))
    }

    pub async fn send_attachment(
        &self,
        conversation: WrappedConversation,
        data: Vec<u8>,
        mime: String,
        uti_type: String,
        filename: String,
        handle: String,
        reply_guid: Option<String>,
        reply_part: Option<String>,
        body: Option<String>,
    ) -> Result<String, WrappedError> {
        let conv: ConversationData = (&conversation).into();
        // Detect voice messages by UTI (CAF files from OGG→CAF remux are voice recordings)
        let is_voice = uti_type == "com.apple.coreaudio-format";
        let service = if conversation.is_sms {
            MessageType::SMS {
                is_phone: false,
                using_number: handle.clone(),
                from_handle: None,
            }
        } else {
            MessageType::IMessage
        };

        // Prepare and upload the attachment via MMCS
        let cursor = Cursor::new(&data);
        let prepared = MMCSFile::prepare_put(cursor).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to prepare MMCS upload: {}", e) })?;

        let cursor2 = Cursor::new(&data);
        let attachment = Attachment::new_mmcs(
            &self.conn,
            &prepared,
            cursor2,
            &mime,
            &uti_type,
            &filename,
            |_current, _total| {},
        ).await.map_err(|e| WrappedError::GenericError { msg: format!("Failed to upload attachment: {}", e) })?;

        let parts = vec![IndexedMessagePart {
            part: MessagePart::Attachment(attachment.clone()),
            idx: None,
            ext: None,
        }];

        // Captions are sent via the subject field in the iMessage plist,
        // not as a separate text part in the XML body.
        let subject = body.clone().filter(|s| !s.is_empty());

        let mut msg = MessageInst::new(
            conv.clone(),
            &handle,
            Message::Message(NormalMessage {
                parts: MessageParts(parts),
                effect: None,
                reply_guid: reply_guid.clone(),
                reply_part: reply_part.clone(),
                service,
                subject,
                app: None,
                link_meta: None,
                voice: is_voice,
                scheduled: None,
                embedded_profile: None,
            }),
        );
        match self.send_with_flap_retry(&mut msg).await {
            Ok(_) => Ok(msg.id.clone()),
            Err(rustpush::PushError::NoValidTargets) if !conversation.is_sms => {
                info!("No IDS targets for attachment, falling back to SMS for {:?}", conv.participants);
                let sms_service = MessageType::SMS {
                    is_phone: false,
                    using_number: handle.clone(),
                    from_handle: None,
                };
                let sms_parts = vec![IndexedMessagePart {
                    part: MessagePart::Attachment(attachment),
                    idx: None,
                    ext: None,
                }];
                let sms_subject = body.filter(|s| !s.is_empty());
                let mut sms_msg = MessageInst::new(
                    conv,
                    &handle,
                    Message::Message(NormalMessage {
                        parts: MessageParts(sms_parts),
                        effect: None,
                        reply_guid: reply_guid,
                        reply_part: reply_part,
                        service: sms_service,
                        subject: sms_subject,
                        app: None,
                        link_meta: None,
                        voice: is_voice,
                        scheduled: None,
                        embedded_profile: None,
                    }),
                );
                self.send_with_flap_retry(&mut sms_msg).await
                    .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send SMS attachment: {}", e) })?;
                Ok(sms_msg.id.clone())
            }
            Err(e) => Err(WrappedError::GenericError { msg: format!("Failed to send attachment: {}", e) }),
        }
    }

    /// Cancel a previously scheduled message.
    pub async fn send_unschedule(
        &self,
        conversation: WrappedConversation,
        handle: String,
    ) -> Result<String, WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let mut msg = MessageInst::new(
            conv,
            &handle,
            Message::Unschedule,
        );
        self.client.send(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send unschedule: {}", e) })?;
        Ok(msg.id.clone())
    }

    /// Rename an iMessage group chat. Delivers a RenameMessage to all participants
    /// so the group name updates on all of the user's Apple devices.
    pub async fn send_rename_group(
        &self,
        conversation: WrappedConversation,
        new_name: String,
        handle: String,
    ) -> Result<String, WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let mut msg = MessageInst::new(
            conv,
            &handle,
            Message::RenameMessage(RenameMessage { new_name }),
        );
        self.client.send(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send rename: {}", e) })?;
        Ok(msg.id.clone())
    }

    /// Update the participant list for an iMessage group chat.
    /// `new_participants` must be the FULL new list of all participants (not
    /// just the delta). `group_version` should be strictly increasing; using
    /// the current Unix timestamp in seconds is a safe default when the exact
    /// protocol counter is unknown.
    pub async fn send_change_participants(
        &self,
        conversation: WrappedConversation,
        new_participants: Vec<String>,
        group_version: u64,
        handle: String,
    ) -> Result<String, WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let mut msg = MessageInst::new(
            conv,
            &handle,
            Message::ChangeParticipants(ChangeParticipantMessage {
                new_participants,
                group_version,
            }),
        );
        self.client.send(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send participant change: {}", e) })?;
        Ok(msg.id.clone())
    }

    /// Upload `photo_data` to MMCS and deliver an IconChange message to set the
    /// group chat photo on all of the user's Apple devices. The image should be
    /// a 570×570 PNG as Apple expects. `group_version` should be strictly
    /// increasing; using the current Unix timestamp in seconds is a safe default.
    pub async fn send_icon_change(
        &self,
        conversation: WrappedConversation,
        photo_data: Vec<u8>,
        group_version: u64,
        handle: String,
    ) -> Result<String, WrappedError> {
        // Prepare the MMCS encryption envelope (computes signature/key).
        let cursor = Cursor::new(&photo_data);
        let prepared = MMCSFile::prepare_put(cursor).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to prepare icon MMCS upload: {}", e) })?;

        // Upload to Apple's MMCS servers and get back the file descriptor.
        let cursor2 = Cursor::new(&photo_data);
        let mmcs = MMCSFile::new(&self.conn, &prepared, cursor2, |_current, _total| {}).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to upload icon to MMCS: {}", e) })?;

        let conv: ConversationData = (&conversation).into();
        let mut msg = MessageInst::new(
            conv,
            &handle,
            Message::IconChange(IconChangeMessage {
                file: Some(mmcs),
                group_version,
            }),
        );
        self.client.send(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send icon change: {}", e) })?;
        Ok(msg.id.clone())
    }

    /// Deliver an IconChange message that clears (removes) the group chat photo
    /// on all of the user's Apple devices.
    pub async fn send_icon_clear(
        &self,
        conversation: WrappedConversation,
        group_version: u64,
        handle: String,
    ) -> Result<String, WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let mut msg = MessageInst::new(
            conv,
            &handle,
            Message::IconChange(IconChangeMessage {
                file: None,
                group_version,
            }),
        );
        self.client.send(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send icon clear: {}", e) })?;
        Ok(msg.id.clone())
    }

    /// Broadcast a PeerCacheInvalidate to all participants in the conversation.
    /// Receiving clients respond by refreshing their IDS key cache for the sender,
    /// which resolves delivery failures caused by stale/rotated identity keys.
    pub async fn send_peer_cache_invalidate(
        &self,
        conversation: WrappedConversation,
        handle: String,
    ) -> Result<(), WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let mut msg = MessageInst::new(conv, &handle, Message::PeerCacheInvalidate);
        self.client.send(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send peer cache invalidate: {}", e) })?;
        Ok(())
    }

    pub async fn send_sms_activation(
        &self,
        conversation: WrappedConversation,
        enable: bool,
        handle: String,
    ) -> Result<String, WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let mut msg = MessageInst::new(conv, &handle, Message::EnableSmsActivation(enable));
        self.client.send(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send SMS activation: {}", e) })?;
        Ok(msg.id.clone())
    }

    pub async fn send_sms_confirm_sent(
        &self,
        conversation: WrappedConversation,
        sms_status: bool,
        handle: String,
    ) -> Result<String, WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let mut msg = MessageInst::new(conv, &handle, Message::SmsConfirmSent(sms_status));
        self.client.send(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send SMS confirm sent: {}", e) })?;
        Ok(msg.id.clone())
    }

    pub async fn send_message_read_on_device(
        &self,
        conversation: WrappedConversation,
        handle: String,
    ) -> Result<String, WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let mut msg = MessageInst::new(conv, &handle, Message::MessageReadOnDevice);
        self.client.send(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send message-read-on-device: {}", e) })?;
        Ok(msg.id.clone())
    }

    pub async fn send_mark_unread(
        &self,
        conversation: WrappedConversation,
        handle: String,
    ) -> Result<String, WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let mut msg = MessageInst::new(conv, &handle, Message::MarkUnread);
        self.client.send(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send mark unread: {}", e) })?;
        Ok(msg.id.clone())
    }

    pub async fn send_error_message(
        &self,
        conversation: WrappedConversation,
        for_uuid: String,
        error_status: u64,
        status_str: String,
        handle: String,
    ) -> Result<String, WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let mut msg = MessageInst::new(
            conv,
            &handle,
            Message::Error(rustpush::ErrorMessage {
                for_uuid,
                status: error_status,
                status_str,
            }),
        );
        self.client.send(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send error message: {}", e) })?;
        Ok(msg.id.clone())
    }

    pub async fn send_update_extension(
        &self,
        conversation: WrappedConversation,
        for_uuid: String,
        extension: WrappedStickerExtension,
        handle: String,
    ) -> Result<String, WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let ext = PartExtension::Sticker {
            msg_width: extension.msg_width,
            rotation: extension.rotation,
            sai: extension.sai,
            scale: extension.scale,
            update: Some(false),
            sli: extension.sli,
            normalized_x: extension.normalized_x,
            normalized_y: extension.normalized_y,
            version: extension.version,
            hash: extension.hash,
            safi: extension.safi,
            effect_type: extension.effect_type,
            sticker_id: extension.sticker_id,
        };
        let mut msg = MessageInst::new(
            conv,
            &handle,
            Message::UpdateExtension(UpdateExtensionMessage { for_uuid, ext }),
        );
        self.client.send(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send update extension: {}", e) })?;
        Ok(msg.id.clone())
    }

    pub async fn send_permanent_delete_messages(
        &self,
        conversation: WrappedConversation,
        message_uuids: Vec<String>,
        is_scheduled: bool,
        handle: String,
    ) -> Result<String, WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let mut msg = MessageInst::new(
            conv,
            &handle,
            Message::PermanentDelete(PermanentDeleteMessage {
                target: DeleteTarget::Messages(message_uuids),
                is_scheduled,
            }),
        );
        self.client.send(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send permanent delete (messages): {}", e) })?;
        Ok(msg.id.clone())
    }

    pub async fn send_permanent_delete_chat(
        &self,
        conversation: WrappedConversation,
        chat_guid: String,
        is_scheduled: bool,
        handle: String,
    ) -> Result<String, WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let bare_handle = handle.replace("mailto:", "").replace("tel:", "");
        let bare_participants: Vec<String> = conv.participants.iter()
            .map(|p| p.replace("mailto:", "").replace("tel:", ""))
            .filter(|p| p != &bare_handle)
            .collect();
        let target = DeleteTarget::Chat(OperatedChat {
            participants: bare_participants,
            group_id: conv.sender_guid.clone().unwrap_or_default(),
            guid: chat_guid,
            delete_incoming_messages: None,
            was_reported_as_junk: None,
        });
        let mut msg = MessageInst::new(
            conv,
            &handle,
            Message::PermanentDelete(PermanentDeleteMessage { target, is_scheduled }),
        );
        self.client.send(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send permanent delete (chat): {}", e) })?;
        Ok(msg.id.clone())
    }

    pub async fn send_update_profile(
        &self,
        conversation: WrappedConversation,
        profile: Option<WrappedShareProfileData>,
        share_contacts: bool,
        handle: String,
    ) -> Result<String, WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let mapped_profile = profile.map(|p| {
            let poster = match (p.low_res_wallpaper_tag, p.wallpaper_tag, p.message_tag) {
                (Some(low), Some(wall), Some(msg)) => Some(SharedPoster {
                    low_res_wallpaper_tag: low,
                    wallpaper_tag: wall,
                    message_tag: msg,
                }),
                _ => None,
            };
            ShareProfileMessage {
                cloud_kit_decryption_record_key: p.cloud_kit_decryption_record_key,
                cloud_kit_record_key: p.cloud_kit_record_key,
                poster,
            }
        });
        let mut msg = MessageInst::new(
            conv,
            &handle,
            Message::UpdateProfile(UpdateProfileMessage {
                profile: mapped_profile,
                share_contacts,
            }),
        );
        self.client.send(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send update profile: {}", e) })?;
        Ok(msg.id.clone())
    }

    pub async fn send_share_profile(
        &self,
        conversation: WrappedConversation,
        cloud_kit_record_key: String,
        cloud_kit_decryption_record_key: Vec<u8>,
        low_res_wallpaper_tag: Option<Vec<u8>>,
        wallpaper_tag: Option<Vec<u8>>,
        message_tag: Option<Vec<u8>>,
        handle: String,
    ) -> Result<String, WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let poster = match (low_res_wallpaper_tag, wallpaper_tag, message_tag) {
            (Some(low), Some(wall), Some(msg)) => Some(SharedPoster {
                low_res_wallpaper_tag: low,
                wallpaper_tag: wall,
                message_tag: msg,
            }),
            _ => None,
        };
        let mut msg = MessageInst::new(
            conv,
            &handle,
            Message::ShareProfile(ShareProfileMessage {
                cloud_kit_decryption_record_key,
                cloud_kit_record_key,
                poster,
            }),
        );
        self.client.send(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send share profile: {}", e) })?;
        Ok(msg.id.clone())
    }

    pub async fn send_update_profile_sharing(
        &self,
        conversation: WrappedConversation,
        shared_dismissed: Vec<String>,
        shared_all: Vec<String>,
        version: u64,
        handle: String,
    ) -> Result<String, WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let mut msg = MessageInst::new(
            conv,
            &handle,
            Message::UpdateProfileSharing(UpdateProfileSharingMessage {
                shared_dismissed,
                shared_all,
                version,
            }),
        );
        self.client.send(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send update profile sharing: {}", e) })?;
        Ok(msg.id.clone())
    }

    pub async fn send_notify_anyways(
        &self,
        conversation: WrappedConversation,
        handle: String,
    ) -> Result<String, WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let mut msg = MessageInst::new(conv, &handle, Message::NotifyAnyways);
        self.client.send(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send notify anyways: {}", e) })?;
        Ok(msg.id.clone())
    }

    pub async fn send_set_transcript_background(
        &self,
        conversation: WrappedConversation,
        group_version: u64,
        image_data: Option<Vec<u8>>,
        handle: String,
    ) -> Result<String, WrappedError> {
        let conv: ConversationData = (&conversation).into();
        let chat_id = conv.sender_guid.clone();
        let mmcs_file = if let Some(data) = image_data {
            let prepared = MMCSFile::prepare_put(Cursor::new(&data)).await
                .map_err(|e| WrappedError::GenericError { msg: format!("Failed to prepare transcript background MMCS upload: {}", e) })?;
            let uploaded = MMCSFile::new(&self.conn, &prepared, Cursor::new(&data), |_current, _total| {}).await
                .map_err(|e| WrappedError::GenericError { msg: format!("Failed to upload transcript background to MMCS: {}", e) })?;
            Some(uploaded)
        } else {
            None
        };
        let update = SetTranscriptBackgroundMessage::from_mmcs(mmcs_file, group_version, chat_id);
        let mut msg = MessageInst::new(conv, &handle, Message::SetTranscriptBackground(update));
        self.client.send(&mut msg).await
            .map_err(|e| WrappedError::GenericError { msg: format!("Failed to send transcript background update: {}", e) })?;
        Ok(msg.id.clone())
    }

    pub async fn cloud_sync_chats(
        &self,
        continuation_token: Option<String>,
    ) -> Result<WrappedCloudSyncChatsPage, WrappedError> {
        let token = decode_continuation_token(continuation_token)?;
        let cloud_messages = self.get_or_init_cloud_messages_client().await?;

        const MAX_SYNC_ATTEMPTS: usize = 4;
        let mut sync_result = None;
        let mut last_pcs_err: Option<rustpush::PushError> = None;
        let mut cm_handle = cloud_messages;

        for attempt in 0..MAX_SYNC_ATTEMPTS {
            match cm_handle.sync_chats(token.clone()).await {
                Ok(result) => {
                    sync_result = Some(result);
                    break;
                }
                Err(err) if is_pcs_recoverable_error(&err) => {
                    let attempt_no = attempt + 1;
                    warn!(
                        "CloudKit chats sync hit PCS key error on attempt {}/{}: {}",
                        attempt_no,
                        MAX_SYNC_ATTEMPTS,
                        err
                    );
                    last_pcs_err = Some(err);
                    if attempt_no < MAX_SYNC_ATTEMPTS {
                        self.recover_cloud_pcs_state("CloudKit chats sync").await?;
                        continue;
                    }
                }
                Err(err) if is_token_missing_error(&err) => {
                    let attempt_no = attempt + 1;
                    warn!(
                        "CloudKit chats sync hit TokenMissing on attempt {}/{} — refreshing MobileMe delegate",
                        attempt_no, MAX_SYNC_ATTEMPTS
                    );
                    if attempt_no < MAX_SYNC_ATTEMPTS {
                        self.refresh_mme_and_reset_cloud_client().await;
                        cm_handle = self.get_or_init_cloud_messages_client().await?;
                        continue;
                    }
                    return Err(WrappedError::GenericError {
                        msg: format!("Failed to sync CloudKit chats: {}", err),
                    });
                }
                Err(err) => {
                    return Err(WrappedError::GenericError {
                        msg: format!("Failed to sync CloudKit chats: {}", err),
                    });
                }
            }
        }

        let (next_token, chats, status) = match sync_result {
            Some(result) => result,
            None => {
                let err = last_pcs_err.map(|e| e.to_string()).unwrap_or_else(|| "unknown error".into());
                return Err(WrappedError::GenericError {
                    msg: format!("Failed to sync CloudKit chats after PCS recovery retries: {}", err),
                });
            }
        };

        let updated_timestamp_ms = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|dur| dur.as_millis() as u64)
            .unwrap_or_default();

        let mut normalized = Vec::with_capacity(chats.len());
        for (record_name, chat_opt) in chats {
            if let Some(chat) = chat_opt {
                let cloud_chat_id = if chat.chat_identifier.is_empty() {
                    record_name.clone()
                } else {
                    chat.chat_identifier.clone()
                };
                normalized.push(WrappedCloudSyncChat {
                    record_name,
                    cloud_chat_id,
                    group_id: chat.group_id,
                    style: chat.style,
                    service: chat.service_name,
                    display_name: chat.display_name,
                    participants: chat.participants.into_iter().map(|p| p.uri).collect(),
                    deleted: false,
                    updated_timestamp_ms,
                    group_photo_guid: chat.group_photo_guid,
                    is_filtered: chat.is_filtered,
                });
            } else {
                normalized.push(WrappedCloudSyncChat {
                    cloud_chat_id: record_name.clone(),
                    group_id: String::new(),
                    style: 0,
                    record_name,
                    service: String::new(),
                    display_name: None,
                    participants: vec![],
                    deleted: true,
                    updated_timestamp_ms,
                    group_photo_guid: None,
                    is_filtered: 0,
                });
            }
        }

        Ok(WrappedCloudSyncChatsPage {
            continuation_token: encode_continuation_token(next_token),
            status,
            done: status == 3,
            chats: normalized,
        })
    }

    /// Dump ALL CloudKit chat records as raw JSON (paginating until done).
    /// Returns a JSON array of objects with record_name + all CloudChat fields.
    pub async fn cloud_dump_chats_json(&self) -> Result<String, WrappedError> {
        let cloud_messages = self.get_or_init_cloud_messages_client().await?;

        let mut all_records: Vec<serde_json::Value> = Vec::new();
        let mut token: Option<Vec<u8>> = None;

        for page in 0..256 {
            let (next_token, chats, status) = cloud_messages.sync_chats(token).await
                .map_err(|e| WrappedError::GenericError {
                    msg: format!("CloudKit chat dump page {} failed: {}", page, e),
                })?;

            for (record_name, chat_opt) in &chats {
                let mut obj = if let Some(chat) = chat_opt {
                    serde_json::to_value(chat).unwrap_or(serde_json::Value::Null)
                } else {
                    serde_json::json!({"deleted": true})
                };
                if let Some(map) = obj.as_object_mut() {
                    map.insert("_record_name".to_string(), serde_json::Value::String(record_name.clone()));
                }
                all_records.push(obj);
            }

            info!("CloudKit chat dump page {}: {} records, status={}", page, chats.len(), status);

            if status == 3 {
                break;
            }
            token = Some(next_token);
        }

        serde_json::to_string_pretty(&all_records).map_err(|e| WrappedError::GenericError {
            msg: format!("JSON serialization failed: {}", e),
        })
    }

    pub async fn cloud_sync_messages(
        &self,
        continuation_token: Option<String>,
    ) -> Result<WrappedCloudSyncMessagesPage, WrappedError> {
        let token = decode_continuation_token(continuation_token)?;
        info!(
            "cloud_sync_messages: token={}, token_bytes={}",
            if token.is_some() { "present" } else { "nil" },
            token.as_ref().map_or(0, |t| t.len())
        );
        let mut cloud_messages = self.get_or_init_cloud_messages_client().await?;

        const MAX_SYNC_ATTEMPTS: usize = 4;
        let mut sync_result = None;
        let mut last_pcs_err: Option<rustpush::PushError> = None;

        for attempt in 0..MAX_SYNC_ATTEMPTS {
            let cm_spawn = Arc::clone(&cloud_messages);
            let tok_spawn = token.clone();
            let spawn_result = tokio::task::spawn(async move {
                cm_spawn.sync_messages(tok_spawn).await
            }).await;

            match spawn_result {
                Ok(Ok(result)) => {
                    info!(
                        "cloud_sync_messages: sync_messages returned {} records, status={}",
                        result.1.len(), result.2
                    );
                    sync_result = Some(result);
                    break;
                }
                Ok(Err(err)) if is_pcs_recoverable_error(&err) => {
                    let attempt_no = attempt + 1;
                    warn!(
                        "CloudKit messages sync hit PCS key error on attempt {}/{}: {}",
                        attempt_no,
                        MAX_SYNC_ATTEMPTS,
                        err
                    );
                    last_pcs_err = Some(err);
                    if attempt_no < MAX_SYNC_ATTEMPTS {
                        self.recover_cloud_pcs_state("CloudKit messages sync").await?;
                        continue;
                    }
                }
                Ok(Err(err)) if is_token_missing_error(&err) => {
                    let attempt_no = attempt + 1;
                    warn!(
                        "CloudKit messages sync hit TokenMissing on attempt {}/{} — refreshing MobileMe delegate",
                        attempt_no, MAX_SYNC_ATTEMPTS
                    );
                    if attempt_no < MAX_SYNC_ATTEMPTS {
                        self.refresh_mme_and_reset_cloud_client().await;
                        cloud_messages = self.get_or_init_cloud_messages_client().await?;
                        continue;
                    }
                    return Err(WrappedError::GenericError {
                        msg: format!("Failed to sync CloudKit messages: {}", err),
                    });
                }
                Ok(Err(err)) => {
                    return Err(WrappedError::GenericError {
                        msg: format!("Failed to sync CloudKit messages: {}", err),
                    });
                }
                Err(join_err) => {
                    warn!(
                        "cloud_sync_messages: sync_messages panicked on attempt {}/{} \
                         (malformed CloudKit record); trying per-record fallback. panic={}",
                        attempt + 1, MAX_SYNC_ATTEMPTS, join_err
                    );
                    match sync_messages_fallback(&cloud_messages, token.clone()).await {
                        Ok(result) => {
                            sync_result = Some(result);
                            break;
                        }
                        Err(e) => {
                            warn!("cloud_sync_messages: fallback also failed: {}; returning empty done page", e);
                            return Ok(WrappedCloudSyncMessagesPage {
                                continuation_token: None,
                                status: 0,
                                done: true,
                                messages: vec![],
                            });
                        }
                    }
                }
            }
        }

        let (next_token, messages, status) = match sync_result {
            Some(result) => result,
            None => {
                let err = last_pcs_err.map(|e| e.to_string()).unwrap_or_else(|| "unknown error".into());
                return Err(WrappedError::GenericError {
                    msg: format!("Failed to sync CloudKit messages after PCS recovery retries: {}", err),
                });
            }
        };

        let mut normalized = Vec::with_capacity(messages.len());
        let mut skipped_messages = 0usize;
        for (record_name, msg_opt) in messages {
            if let Some(msg) = msg_opt {
                // Skip system/service messages (group renames, participant changes,
                // location sharing, etc.). IS_SYSTEM_MESSAGE covers explicit system
                // notifications; IS_SERVICE_MESSAGE covers service-generated inline
                // notifications (e.g. "You named the conversation"). Neither type
                // is user-authored content.
                if msg.flags.intersects(
                    rustpush::cloud_messages::MessageFlags::IS_SYSTEM_MESSAGE
                    | rustpush::cloud_messages::MessageFlags::IS_SERVICE_MESSAGE
                ) {
                    skipped_messages += 1;
                    continue;
                }
                // Wrap per-message normalization in catch_unwind so one bad
                // CloudKit record doesn't fail the entire page.
                let rn = record_name.clone();
                let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                    let guid = if msg.guid.is_empty() {
                        rn.clone()
                    } else {
                        msg.guid.clone()
                    };
                    let text = msg.msg_proto.text.clone();
                    let subject = msg.msg_proto.subject.clone();

                    // Extract tapback/reaction info from proto fields
                    let tapback_type = msg.msg_proto.associated_message_type;
                    let tapback_target_guid = msg.msg_proto.associated_message_guid.clone();
                    let tapback_emoji = msg.msg_proto_4.as_ref()
                        .and_then(|p4| p4.associated_message_emoji.clone());

                    // Extract attachment GUIDs from attributedBody
                    let mut attachment_guids: Vec<String> = msg.msg_proto.attributed_body
                        .as_ref()
                        .map(|body| extract_attachment_guids_from_attributed_body(body))
                        .unwrap_or_default()
                        .into_iter()
                        .filter(|g| !g.is_empty() && g.len() <= 256 && g.is_ascii())
                        .collect();

                    // Also extract from messageSummaryInfo to capture companion
                    // transfers (e.g. Live Photo MOV) not in attributedBody.
                    if let Some(ref summary) = msg.msg_proto.message_summary_info {
                        for sg in extract_attachment_guids_from_summary_info(summary) {
                            if !sg.is_empty() && sg.len() <= 256 && sg.is_ascii()
                                && !attachment_guids.contains(&sg)
                            {
                                attachment_guids.push(sg);
                            }
                        }
                    }

                    let date_read_ms = msg.msg_proto.date_read
                        .map(|dr| apple_timestamp_ns_to_unix_ms(dr as i64))
                        .unwrap_or(0);

                    let has_body = msg.msg_proto.attributed_body.is_some();

                    WrappedCloudSyncMessage {
                        record_name: rn,
                        guid,
                        cloud_chat_id: msg.chat_id.clone(),
                        sender: msg.sender.clone(),
                        is_from_me: msg
                            .flags
                            .contains(rustpush::cloud_messages::MessageFlags::IS_FROM_ME),
                        text,
                        subject,
                        service: msg.service.clone(),
                        timestamp_ms: apple_timestamp_ns_to_unix_ms(msg.time),
                        deleted: false,
                        tapback_type,
                        tapback_target_guid,
                        tapback_emoji,
                        attachment_guids,
                        date_read_ms,
                        msg_type: msg.r#type,
                        has_body,
                    }
                }));

                match result {
                    Ok(wrapped) => normalized.push(wrapped),
                    Err(panic_val) => {
                        let panic_msg = if let Some(s) = panic_val.downcast_ref::<String>() {
                            s.clone()
                        } else if let Some(s) = panic_val.downcast_ref::<&str>() {
                            s.to_string()
                        } else {
                            "unknown panic".to_string()
                        };
                        warn!(
                            "Skipping CloudKit message {} due to normalization panic: {}",
                            record_name, panic_msg
                        );
                        skipped_messages += 1;
                    }
                }
            } else {
                normalized.push(WrappedCloudSyncMessage {
                    guid: record_name.clone(),
                    record_name,
                    cloud_chat_id: String::new(),
                    sender: String::new(),
                    is_from_me: false,
                    text: None,
                    subject: None,
                    service: String::new(),
                    timestamp_ms: 0,
                    deleted: true,
                    tapback_type: None,
                    tapback_target_guid: None,
                    tapback_emoji: None,
                    attachment_guids: vec![],
                    date_read_ms: 0,
                    msg_type: 0,
                    has_body: false,
                });
            }
        }

        if skipped_messages > 0 {
            warn!(
                "CloudKit message sync: skipped {} message(s) due to normalization errors",
                skipped_messages
            );
        }

        info!(
            "CloudKit message sync page: {} messages normalized, {} skipped",
            normalized.len(), skipped_messages
        );

        Ok(WrappedCloudSyncMessagesPage {
            continuation_token: encode_continuation_token(next_token),
            status,
            done: status == 3,
            messages: normalized,
        })
    }

    /// Sync CloudKit attachment zone and return metadata for all attachments.
    /// Returns a page of attachment metadata (record_name → attachment info).
    /// Paginate with continuation_token until done == true.
    ///
    /// Parses records as our local `CloudAttachmentWithAvid` instead of
    /// upstream's `CloudAttachment` so we get the `avid` field (Live Photo
    /// MOV companion) without needing a rustpush patch. Same on-the-wire
    /// record type ("attachment"), same PCS-encrypted record fields —
    /// `cm`, `lqa`, and `avid` are decoded by the CloudKit field-name
    /// matcher on our struct the same way they'd be on upstream's + an
    /// extra field. Ford key registration uses both `lqa.protection_info`
    /// AND `avid.protection_info`.
    pub async fn cloud_sync_attachments(
        &self,
        continuation_token: Option<String>,
    ) -> Result<WrappedCloudSyncAttachmentsPage, WrappedError> {
        use rustpush::cloudkit::{pcs_keys_for_record, FetchRecordChangesOperation, CloudKitSession, NO_ASSETS};
        use rustpush::cloud_messages::MESSAGES_SERVICE;
        use rustpush::pcs::{PCSShareProtection, PCSEncryptor};
        use rustpush::PushError;

        let token = decode_continuation_token(continuation_token)?;
        let cloud_messages = self.get_or_init_cloud_messages_client().await?;

        // Hand-rolled sync with NO_ASSETS — matches master's
        // sync_attachments → sync_records → FetchRecordChangesOperation(..., &NO_ASSETS) path.
        //
        // DO NOT use ALL_ASSETS here. ALL_ASSETS asks the server to include
        // download authorization for every asset field in every returned record.
        // The server silently omits records whose MMCS assets are unavailable
        // (expired, pending GC, or otherwise un-authorizable). Those records ARE
        // present in the zone and returned by NO_ASSETS — they just can't have
        // their asset download URLs generated.
        //
        // NOTE: Investigation confirmed that NO_ASSETS vs ALL_ASSETS does NOT
        // explain the 4 missing records — master (also NO_ASSETS) misses the
        // same 4. The root cause is still under investigation. See the
        // QueryRecords diagnostic at the end of the final page.
        //
        // Ford keys come from lqa.protection_info (PCS-encrypted record field,
        // NOT the asset download content), so NO_ASSETS vs ALL_ASSETS makes no
        // difference to Ford key population.
        //
        // We parse records as CloudAttachmentWithAvid (our local type
        // that declares the avid field) so sync-time has_avid detection
        // AND avid Ford key caching work in one pass.
        // SLEDGEHAMMER: the whole container+zone_key+perform call chain runs
        // inside a spawned task. Upstream omnisette has an unconditional
        // `panic!()` on any non-`AnisetteNotProvisioned` error from
        // `get_headers` (remote_anisette_v3.rs:417), and MMCS/PCS paths
        // below it have their own scattered `.expect()` calls. Any of
        // these can fire mid-sync and abort the entire page, silently
        // losing its records because the continuation token hasn't
        // advanced yet.
        //
        // Retry up to 4 times with the SAME continuation token. Between
        // attempts, on panic (JoinError), fully reset the cloud client
        // (cloud_messages_client + cloud_keychain_client → None) so the
        // next get_or_init builds a fresh omnisette provider with fresh
        // state. On final failure, return an empty done-page so Go's
        // loop breaks without advancing the persisted token — the
        // next sync cycle will retry the same page from DB state.
        const ATT_SYNC_RETRIES: usize = 4;
        let mut cm_handle = cloud_messages;
        // All of these are populated on a successful perform() and consumed
        // after the retry loop. Types are inferred from the first Some(...)
        // assignment so we don't have to hard-code upstream's crate paths.
        let mut perform_result = None;
        let mut last_perform_err: Option<String> = None;
        let mut container_holder = None;
        let mut zone_id_holder = None;
        let mut zone_key_holder = None;
        for attempt in 0..ATT_SYNC_RETRIES {
            // Re-fetch container + zone_key INSIDE the retry so a reset
            // cloud_messages_client picks up a fresh container, fresh keychain,
            // fresh omnisette state on retry.
            let container = match cm_handle.get_container().await {
                Ok(c) => c,
                Err(e) => {
                    let attempt_no = attempt + 1;
                    warn!(
                        "cloud_sync_attachments: get_container() failed on attempt {}/{}: {}",
                        attempt_no, ATT_SYNC_RETRIES, e
                    );
                    last_perform_err = Some(format!("get_container: {}", e));
                    if attempt_no < ATT_SYNC_RETRIES {
                        // TokenMissing means the seeded MME delegate lacks the
                        // token CloudKit init needs (mmeAuthToken). A plain
                        // reset_cloud_client would reload the same delegate
                        // and fail identically; force-refresh the delegate so
                        // the re-init actually sees fresh tokens.
                        if is_token_missing_error(&e) {
                            self.refresh_mme_and_reset_cloud_client().await;
                        } else {
                            self.reset_cloud_client().await;
                        }
                        cm_handle = self.get_or_init_cloud_messages_client().await?;
                        continue;
                    }
                    break;
                }
            };
            let zone_id_local = container.private_zone("attachmentManateeZone".to_string());
            let zone_key_local = match container
                .get_zone_encryption_config(&zone_id_local, &cm_handle.keychain, &MESSAGES_SERVICE)
                .await
            {
                Ok(k) => k,
                Err(e) => {
                    let attempt_no = attempt + 1;
                    warn!(
                        "cloud_sync_attachments: zone_key fetch failed on attempt {}/{}: {}",
                        attempt_no, ATT_SYNC_RETRIES, e
                    );
                    last_perform_err = Some(format!("zone_key: {}", e));
                    if attempt_no < ATT_SYNC_RETRIES {
                        if is_token_missing_error(&e) {
                            self.refresh_mme_and_reset_cloud_client().await;
                        } else {
                            self.reset_cloud_client().await;
                        }
                        cm_handle = self.get_or_init_cloud_messages_client().await?;
                        continue;
                    }
                    break;
                }
            };

            let c = Arc::clone(&container);
            let z = zone_id_local.clone();
            let tok = token.clone();
            let join = tokio::task::spawn(async move {
                // Construct request directly (not via ::new helper) so we can set
                // newest_first: Some(true), matching master's sync_records path.
                // The ::new helper hard-codes newest_first: Some(false), which
                // causes the CloudKit server to return a different (incomplete)
                // record set — empirically 4 records are omitted when false.
                c.perform(
                    &CloudKitSession::new(),
                    FetchRecordChangesOperation(rustpush::cloudkit_proto::RetrieveChangesRequest {
                        sync_continuation_token: tok,
                        zone_identifier: Some(z),
                        requested_changes_types: Some(3),
                        assets_to_download: Some(NO_ASSETS.clone()),
                        newest_first: Some(true),
                        ..Default::default()
                    }),
                )
                .await
            })
            .await;
            match join {
                Ok(Ok(ok)) => {
                    perform_result = Some(ok);
                    container_holder = Some(container);
                    zone_id_holder = Some(zone_id_local);
                    zone_key_holder = Some(zone_key_local);
                    break;
                }
                Ok(Err(e)) => {
                    let attempt_no = attempt + 1;
                    warn!(
                        "cloud_sync_attachments: perform() returned error on attempt {}/{}: {}",
                        attempt_no, ATT_SYNC_RETRIES, e
                    );
                    last_perform_err = Some(format!("{}", e));
                    if attempt_no < ATT_SYNC_RETRIES {
                        if is_token_missing_error(&e) {
                            self.refresh_mme_and_reset_cloud_client().await;
                        } else {
                            self.reset_cloud_client().await;
                        }
                        cm_handle = self.get_or_init_cloud_messages_client().await?;
                        continue;
                    }
                }
                Err(join_err) => {
                    let attempt_no = attempt + 1;
                    warn!(
                        "cloud_sync_attachments: perform() panicked on attempt {}/{} \
                         (likely upstream omnisette/anisette panic); resetting cloud client and retrying with same token. panic={}",
                        attempt_no, ATT_SYNC_RETRIES, join_err
                    );
                    last_perform_err = Some(format!("panic: {}", join_err));
                    if attempt_no < ATT_SYNC_RETRIES {
                        self.reset_cloud_client().await;
                        cm_handle = self.get_or_init_cloud_messages_client().await?;
                        continue;
                    }
                }
            }
        }
        let (_assets, response) = match perform_result {
            Some(r) => r,
            None => {
                let err_msg = last_perform_err.unwrap_or_else(|| "unknown".into());
                warn!(
                    "cloud_sync_attachments: perform() failed after {} attempts ({}); returning empty done page to let caller skip",
                    ATT_SYNC_RETRIES, err_msg
                );
                return Ok(WrappedCloudSyncAttachmentsPage {
                    continuation_token: None,
                    status: 0,
                    done: true,
                    attachments: vec![],
                });
            }
        };
        let container = container_holder.expect("container set on success");
        let zone_id = zone_id_holder.expect("zone_id set on success");
        let mut zone_key = zone_key_holder.expect("zone_key set on success");
        let cloud_messages = cm_handle;

        let status = response.status();
        let next_token = response.sync_continuation_token().to_vec();

        let mut normalized = Vec::with_capacity(response.change.len());
        let mut ford_cached = 0usize;
        let mut refreshed_zone_key_config = false;
        let mut pcs_skipped = 0usize;
        // Silent drop path counters. These three `continue`s used to produce
        // no logs at all, making it impossible to tell from the outside
        // whether a missing attachment was (a) never in the change feed, (b)
        // a tombstone (deletion), or (c) a different record type. Always
        // summarized at the end of the sync page.
        let mut no_identifier = 0usize;
        let mut no_record_tombstone = 0usize;
        let mut wrong_type = 0usize;
        // `cm_decode_fail` = records whose `cm` field (GZipWrapper<AttachmentMeta>)
        // panicked or returned None on decode. A failed `cm` means we have no
        // aguid / no attachment metadata, so the record must be skipped — but
        // we now log WHY and increment a counter, instead of silently dropping
        // inside a whole-record `catch_unwind`.
        //
        // `lqa_decode_fail` = records whose `lqa` field (Asset, the primary
        // MMCS blob) panicked on decode. Unlike `cm`, a failed `lqa` does NOT
        // drop the record — we still emit the record with a missing Ford key,
        // so Go sees the aguid in attachments_json and the download path can
        // attempt Ford recovery. This is the fix for the 4-record regression:
        // upstream's `Asset::from_value_encrypted` unwraps `protection_info`
        // twice (cloudkit-proto/src/lib.rs:273), which panics on any record
        // whose lqa has `protection_info=None` or `protection_info.protection_info=None`.
        // The previous catch_unwind(from_record_encrypted) caught that panic
        // but nuked the whole record, losing `cm` along with `lqa`.
        let mut cm_decode_fail = 0usize;
        let mut lqa_decode_fail = 0usize;
        for change in &response.change {
            let identifier = match change.identifier.as_ref().and_then(|i| i.value.as_ref()) {
                Some(v) => v.name().to_string(),
                None => {
                    no_identifier += 1;
                    continue;
                }
            };
            let record = match &change.record {
                Some(r) => r,
                None => {
                    // CloudKit change feed returns identifier without a record
                    // body when the record has been deleted. The deletion
                    // applies to the zone regardless of record type, so we log
                    // the record_name to help correlate with any missing
                    // attachments in the bridge.
                    no_record_tombstone += 1;
                    info!(
                        "cloud_sync_attachments: tombstone (deleted record) {}",
                        identifier
                    );
                    continue;
                }
            };
            if record.r#type.as_ref().and_then(|t| t.name.as_deref())
                != Some(<rustpush::cloud_messages::CloudAttachment as rustpush::cloudkit_proto::CloudKitRecord>::record_type())
            {
                wrong_type += 1;
                continue;
            }

            // Master's PCS-record-key-missing refresh retry pattern:
            // when `pcs_keys_for_record` returns PCSRecordKeyMissing, the
            // zone encryption config is likely stale (a new key was
            // rotated after our cache was seeded). Clear the zone config
            // cache, re-fetch, and retry once. For other PCS-related
            // errors (ShareKeyNotFound, DecryptionKeyNotFound,
            // MasterKeyNotFound), gracefully skip the record with a warn.
            // Without this, my sync was silently skipping records whose
            // Ford keys master would have recovered — producing gaps in
            // the cache that showed up as "all cached keys exhausted"
            // failures during Ford dedup recovery.
            // Upstream's decode_record_protection uses .unwrap()/.expect() and
            // can panic when zone-key lookup fails. Wrap in catch_unwind so a
            // single bad record skips gracefully instead of aborting the whole
            // page and freezing the continuation token forever.
            let pcs_result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                pcs_keys_for_record(record, &zone_key)
            }));
            let pcskey_opt = match pcs_result {
                Err(_panic) => {
                    // pcs_keys_for_record panicked — upstream's decode_record_protection
                    // uses byte comparison (key.compress() vs stored pub_key) which
                    // fails when the zone was key-rotated and the old key is missing
                    // from zone_keys but still in the keychain.
                    //
                    // Fall back to decrypt_with_keychain, which does a format-agnostic
                    // keychain lookup (base64 of stored pub_key bytes) and succeeds
                    // where the byte comparison fails. This is the wrapper-layer
                    // re-implementation of the aecc7ed fix from the vendored tree.
                    if let Some(protection) = &record.protection_info {
                        let record_protection = PCSShareProtection::from_protection_info(protection);
                        let keychain_state = cloud_messages.keychain.state.read().await;
                        let fallback = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                            record_protection.decrypt_with_keychain(&keychain_state, &MESSAGES_SERVICE, false)
                        }));
                        match fallback {
                            Ok(Ok((pcs_keys, _))) => {
                                let record_id = record.record_identifier.clone().expect("no record id");
                                info!(
                                    "cloud_sync_attachments: fallback decrypt_with_keychain succeeded for {}",
                                    identifier
                                );
                                Some(PCSEncryptor { keys: pcs_keys, record_id })
                            }
                            Ok(Err(e)) => {
                                pcs_skipped += 1;
                                warn!(
                                    "cloud_sync_attachments: pcs panic + fallback failed for {}: {}",
                                    identifier, e
                                );
                                None
                            }
                            Err(_) => {
                                pcs_skipped += 1;
                                warn!(
                                    "cloud_sync_attachments: pcs panic + fallback panicked for {}",
                                    identifier
                                );
                                None
                            }
                        }
                    } else {
                        pcs_skipped += 1;
                        warn!(
                            "cloud_sync_attachments: pcs_keys_for_record panicked for {} (no protection_info for fallback)",
                            identifier
                        );
                        None
                    }
                }
                Ok(Ok(k)) => Some(k),
                Ok(Err(PushError::PCSRecordKeyMissing)) if !refreshed_zone_key_config => {
                    info!("cloud_sync_attachments: PCSRecordKeyMissing for {}, refreshing zone config", identifier);
                    container.clear_cache_zone_encryption_config(&zone_id).await;
                    refreshed_zone_key_config = true;
                    match container
                        .get_zone_encryption_config(&zone_id, &cloud_messages.keychain, &MESSAGES_SERVICE)
                        .await
                    {
                        Ok(new_key) => {
                            zone_key = new_key;
                            let retry_result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                                pcs_keys_for_record(record, &zone_key)
                            }));
                            match retry_result {
                                Err(_panic) => {
                                    pcs_skipped += 1;
                                    warn!(
                                        "cloud_sync_attachments: pcs_keys_for_record panicked on retry for {}, skipping",
                                        identifier
                                    );
                                    None
                                }
                                Ok(Ok(k)) => Some(k),
                                Ok(Err(e)) => {
                                    pcs_skipped += 1;
                                    warn!(
                                        "cloud_sync_attachments: PCS key still missing after refresh for {}: {}",
                                        identifier, e
                                    );
                                    None
                                }
                            }
                        }
                        Err(e) => {
                            warn!("cloud_sync_attachments: zone config refresh failed: {}", e);
                            None
                        }
                    }
                }
                Ok(Err(err))
                    if matches!(
                        err,
                        PushError::PCSRecordKeyMissing
                            | PushError::ShareKeyNotFound(_)
                            | PushError::DecryptionKeyNotFound(_)
                            | PushError::MasterKeyNotFound
                    ) =>
                {
                    pcs_skipped += 1;
                    warn!(
                        "cloud_sync_attachments: skipping {} due to PCS key error: {}",
                        identifier, err
                    );
                    None
                }
                Ok(Err(e)) => {
                    pcs_skipped += 1;
                    warn!("cloud_sync_attachments: unexpected PCS error for {}: {}", identifier, e);
                    None
                }
            };
            let pcskey = match pcskey_opt {
                Some(k) => k,
                None => continue,
            };

            // Per-field manual decode.
            //
            // Previously this call used upstream's derive-generated
            // `<CloudAttachment as CloudKitRecord>::from_record_encrypted`
            // inside a whole-record `catch_unwind`. That path has two
            // load-bearing panic sites in upstream's code that silently
            // nuked entire records:
            //
            //   1. `Asset::from_value_encrypted`
            //      (cloudkit-proto/src/lib.rs:273) double-unwraps
            //      `asset.protection_info` and `.protection_info`. Any
            //      record whose `lqa` Asset is missing either nested Option
            //      panics immediately.
            //   2. `GZipWrapper::from_bytes`
            //      (cloud_messages.rs:211) calls `.expect("ungzip fialed")`.
            //      Any record whose `cm` field decrypts to non-gzip bytes
            //      panics immediately.
            //
            // The old catch_unwind caught the panic but then `continue`d,
            // dropping the entire record — so the aguid never reached Go,
            // `attMap` never saw it, and the bridge ingest code logged
            // "Attachment GUID not found in attachment zone". That's the
            // observed regression for the 4 stubborn aguids.
            //
            // Fix: decode each field independently with its own
            // catch_unwind. `cm` is required (no aguid → skip record);
            // `lqa` and `avid` are best-effort (missing Asset just means
            // we emit the record without a Ford key).
            let find_field = |name: &str| -> Option<&rustpush::cloudkit_proto::record::field::Value> {
                record
                    .record_field
                    .iter()
                    .find(|f| {
                        f.identifier
                            .as_ref()
                            .and_then(|i| i.name.as_deref())
                            == Some(name)
                    })
                    .and_then(|f| f.value.as_ref())
            };

            let field_names: Vec<String> = record
                .record_field
                .iter()
                .filter_map(|f| f.identifier.as_ref().and_then(|i| i.name.clone()))
                .collect();

            // Decode `cm` (GZipWrapper<AttachmentMeta>).
            // We pull the GZipWrapper out, then move the inner AttachmentMeta.
            let cm_opt: Option<rustpush::cloud_messages::AttachmentMeta> = match find_field("cm") {
                None => None,
                Some(v) => match std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                    <rustpush::cloud_messages::GZipWrapper<rustpush::cloud_messages::AttachmentMeta>
                        as rustpush::cloudkit_proto::CloudKitEncryptedValue>::from_value_encrypted(
                        v, &pcskey, "cm",
                    )
                })) {
                    Ok(Some(gzw)) => Some(gzw.0),
                    Ok(None) => None,
                    Err(_panic) => {
                        // from_bytes panic (ungzip or plist parse). Log and
                        // fall through — counted as cm_decode_fail below.
                        warn!(
                            "cloud_sync_attachments: {} — cm decode panicked (likely ungzip/plist). fields={:?}",
                            identifier, field_names
                        );
                        None
                    }
                },
            };
            let cm = match cm_opt {
                Some(cm) => cm,
                None => {
                    cm_decode_fail += 1;
                    warn!(
                        "cloud_sync_attachments: skipping {} — cm field missing or undecodable. fields={:?}",
                        identifier, field_names
                    );
                    continue;
                }
            };

            // Decode `lqa` (Asset). Best-effort: on failure we still emit
            // the record so Go sees the aguid in attMap. The Ford download
            // path will attempt recovery later if needed.
            //
            // `lqa_outcome` distinguishes three states: Ok(asset present),
            // Ok(None) = field absent (fine, not counted), Err = present
            // but decode panicked or returned None (counted + logged).
            let lqa_field = find_field("lqa");
            let lqa_opt: Option<rustpush::cloudkit_proto::Asset> = match lqa_field {
                None => None,
                Some(v) => {
                    let decoded = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                        <rustpush::cloudkit_proto::Asset as rustpush::cloudkit_proto::CloudKitEncryptedValue>::from_value_encrypted(v, &pcskey, "lqa")
                    }));
                    match decoded {
                        Ok(Some(asset)) => Some(asset),
                        Ok(None) => {
                            lqa_decode_fail += 1;
                            warn!(
                                "cloud_sync_attachments: {} aguid={} — lqa present but from_value_encrypted returned None; emitting record without Ford key",
                                identifier, cm.guid
                            );
                            None
                        }
                        Err(_panic) => {
                            lqa_decode_fail += 1;
                            warn!(
                                "cloud_sync_attachments: {} aguid={} — lqa decode panicked (likely None protection_info); emitting record without Ford key. fields={:?}",
                                identifier, cm.guid, field_names
                            );
                            None
                        }
                    }
                }
            };

            // Decode `avid` (Live Photo MOV companion, Asset). Best-effort.
            // Same panic risk as lqa.
            let avid_asset: Option<rustpush::cloudkit_proto::Asset> = match find_field("avid") {
                None => None,
                Some(v) => match std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                    <rustpush::cloudkit_proto::Asset as rustpush::cloudkit_proto::CloudKitEncryptedValue>::from_value_encrypted(v, &pcskey, "avid")
                })) {
                    Ok(parsed) => parsed,
                    Err(_panic) => {
                        warn!(
                            "cloud_sync_attachments: {} aguid={} — avid decode panicked; treating as no Live Photo",
                            identifier, cm.guid
                        );
                        None
                    }
                },
            };

            let has_avid = avid_asset
                .as_ref()
                .and_then(|a| a.size)
                .unwrap_or(0)
                > 0;
            let avid_ford_key = avid_asset
                .as_ref()
                .and_then(|a| a.protection_info.as_ref())
                .and_then(|p| p.protection_info.as_ref())
                .cloned();

            let ford_key = lqa_opt
                .as_ref()
                .and_then(|lqa| lqa.protection_info.as_ref())
                .and_then(|p| p.protection_info.as_ref())
                .cloned();

            // Register BOTH lqa and avid Ford keys into the wrapper cache
            // immediately. Matches master's sync_attachments cache-population
            // loop. Also propagated to Go's cache via the Go-side register.
            if let Some(ref k) = ford_key {
                register_ford_key(k.clone());
                ford_cached += 1;
            }
            if let Some(ref k) = avid_ford_key {
                register_ford_key(k.clone());
                ford_cached += 1;
            }

            // Per-record log at INFO so the exact aguids reaching Go can be
            // grep'd. `grep cloud_sync_attachments: att` against the journal
            // will list every aguid → record_name pair the bridge normalized.
            // Missing a specific aguid from this list = CloudKit change feed
            // isn't returning it, and the fix is not in the sync loop.
            info!(
                "cloud_sync_attachments: att guid={} record={} mime={:?} size={} lqa_ok={} avid_ok={}",
                cm.guid,
                identifier,
                cm.mime_type,
                cm.total_bytes,
                lqa_opt.is_some(),
                avid_asset.is_some(),
            );
            normalized.push(WrappedCloudAttachmentInfo {
                guid: cm.guid.clone(),
                mime_type: cm.mime_type.clone(),
                uti_type: cm.uti.clone(),
                filename: cm.transfer_name.clone().or_else(|| cm.filename.clone()),
                file_size: cm.total_bytes,
                record_name: identifier,
                hide_attachment: cm.hide_attachment,
                has_avid,
                ford_key,
                avid_ford_key,
            });
        }

        info!(
            "cloud_sync_attachments: {} normalized, {} ford_cached, {} pcs_skipped, {} no_id, {} tombstones, {} wrong_type, {} cm_decode_fail, {} lqa_decode_fail, {} total_change_entries",
            normalized.len(),
            ford_cached,
            pcs_skipped,
            no_identifier,
            no_record_tombstone,
            wrong_type,
            cm_decode_fail,
            lqa_decode_fail,
            response.change.len()
        );

        Ok(WrappedCloudSyncAttachmentsPage {
            continuation_token: encode_continuation_token(next_token),
            status,
            done: status == 3,
            attachments: normalized,
        })
    }

    /// QueryRecords fallback for attachmentManateeZone.
    ///
    /// FetchRecordChanges misses some live records — confirmed on both master and
    /// refactor, same count regardless of newest_first/NO_ASSETS. QueryRecords
    /// queries current zone state without relying on the change-feed token and
    /// returns records the feed omits.
    ///
    /// Call once after the full cloud_sync_attachments loop completes, passing
    /// the record_names of all attachments already collected from the change feed.
    /// Returns only records NOT in known_record_names, processed with the same
    /// PCS + cm/lqa/avid decode logic as cloud_sync_attachments.
    pub async fn cloud_query_attachments_fallback(
        &self,
        known_record_names: Vec<String>,
    ) -> Result<WrappedCloudSyncAttachmentsPage, WrappedError> {
        use rustpush::cloudkit::{pcs_keys_for_record, CloudKitOp, CloudKitSession, NO_ASSETS};
        use rustpush::cloud_messages::MESSAGES_SERVICE;
        use rustpush::pcs::{PCSShareProtection, PCSEncryptor};
        use rustpush::PushError;
        use rustpush::cloudkit_proto;

        struct RawQueryOp(cloudkit_proto::QueryRetrieveRequest);
        impl CloudKitOp for RawQueryOp {
            type Response = cloudkit_proto::QueryRetrieveResponse;
            fn set_request(&self, output: &mut cloudkit_proto::RequestOperation) {
                output.query_retrieve_request = Some(self.0.clone());
            }
            fn retrieve_response(response: &cloudkit_proto::ResponseOperation) -> Self::Response {
                response.query_retrieve_response.clone().unwrap_or_default()
            }
            fn flow_control_key() -> &'static str { "CKDQueryOperation" }
            fn link() -> &'static str { "https://gateway.icloud.com/ckdatabase/api/client/query/retrieve" }
            fn operation() -> cloudkit_proto::operation::Type {
                cloudkit_proto::operation::Type::QueryRetrieveType
            }
            fn tags() -> bool { false }
        }

        let seen_ids: std::collections::HashSet<String> = known_record_names.into_iter().collect();

        let mut cloud_messages = self.get_or_init_cloud_messages_client().await?;
        let container = match cloud_messages.get_container().await {
            Ok(c) => c,
            Err(e) if is_token_missing_error(&e) => {
                warn!("cloud_query_attachments_fallback: get_container TokenMissing — refreshing MobileMe delegate and retrying");
                self.refresh_mme_and_reset_cloud_client().await;
                cloud_messages = self.get_or_init_cloud_messages_client().await?;
                cloud_messages.get_container().await.map_err(|e| WrappedError::GenericError {
                    msg: format!("cloud_query_attachments_fallback: get_container failed after MME refresh: {}", e),
                })?
            }
            Err(e) => return Err(WrappedError::GenericError { msg: format!("cloud_query_attachments_fallback: get_container failed: {}", e) }),
        };
        let zone_id = container.private_zone("attachmentManateeZone".to_string());
        let mut zone_key = match container
            .get_zone_encryption_config(&zone_id, &cloud_messages.keychain, &MESSAGES_SERVICE)
            .await
        {
            Ok(k) => k,
            Err(e) => return Err(WrappedError::GenericError { msg: format!("cloud_query_attachments_fallback: zone_key failed: {}", e) }),
        };

        let mut normalized: Vec<WrappedCloudAttachmentInfo> = Vec::new();
        let mut pcs_skipped = 0usize;
        let mut cm_decode_fail = 0usize;
        let mut lqa_decode_fail = 0usize;
        let mut ford_cached = 0usize;
        let mut refreshed_zone_key_config = false;
        let mut qr_continuation: Option<Vec<u8>> = None;
        let mut qr_page = 0usize;

        loop {
            qr_page += 1;
            let c = Arc::clone(&container);
            let z = zone_id.clone();
            let qr_cont = qr_continuation.clone();
            let join = tokio::task::spawn(async move {
                c.perform(&CloudKitSession::new(), RawQueryOp(cloudkit_proto::QueryRetrieveRequest {
                    query: Some(cloudkit_proto::Query {
                        types: vec![cloudkit_proto::record::Type { name: Some("attachment".to_string()) }],
                        filters: vec![],
                        sorts: vec![],
                        ..Default::default()
                    }),
                    zone_identifier: Some(z),
                    assets_to_download: Some(NO_ASSETS.clone()),
                    continuation_marker: qr_cont,
                    ..Default::default()
                })).await
            }).await;

            let qr = match join {
                Ok(Ok(r)) => r,
                Ok(Err(e)) => {
                    warn!("cloud_query_attachments_fallback: page {} failed: {}", qr_page, e);
                    break;
                }
                Err(e) => {
                    warn!("cloud_query_attachments_fallback: page {} panicked: {}", qr_page, e);
                    break;
                }
            };
            let next_marker = qr.continuation_marker.clone();

            for qresult in &qr.query_results {
                let record = match &qresult.record { Some(r) => r, None => continue };
                let identifier = match record.record_identifier.as_ref()
                    .and_then(|i| i.value.as_ref())
                    .and_then(|v| v.name.as_deref())
                {
                    Some(n) => n.to_string(),
                    None => continue,
                };
                if seen_ids.contains(&identifier) {
                    continue;
                }
                info!("cloud_query_attachments_fallback: new record not in change feed: {}", identifier);

                let pcs_result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                    pcs_keys_for_record(record, &zone_key)
                }));
                let pcskey_opt = match pcs_result {
                    Err(_panic) => {
                        if let Some(protection) = &record.protection_info {
                            let record_protection = PCSShareProtection::from_protection_info(protection);
                            let keychain_state = cloud_messages.keychain.state.read().await;
                            let fallback = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                                record_protection.decrypt_with_keychain(&keychain_state, &MESSAGES_SERVICE, false)
                            }));
                            match fallback {
                                Ok(Ok((pcs_keys, _))) => {
                                    let record_id = record.record_identifier.clone().expect("no record id");
                                    Some(PCSEncryptor { keys: pcs_keys, record_id })
                                }
                                Ok(Err(e)) => { pcs_skipped += 1; warn!("cloud_query_attachments_fallback: pcs panic + fallback failed for {}: {}", identifier, e); None }
                                Err(_) => { pcs_skipped += 1; warn!("cloud_query_attachments_fallback: pcs panic + fallback panicked for {}", identifier); None }
                            }
                        } else {
                            pcs_skipped += 1;
                            warn!("cloud_query_attachments_fallback: pcs panicked for {} (no protection_info)", identifier);
                            None
                        }
                    }
                    Ok(Ok(k)) => Some(k),
                    Ok(Err(PushError::PCSRecordKeyMissing)) if !refreshed_zone_key_config => {
                        container.clear_cache_zone_encryption_config(&zone_id).await;
                        refreshed_zone_key_config = true;
                        match container.get_zone_encryption_config(&zone_id, &cloud_messages.keychain, &MESSAGES_SERVICE).await {
                            Ok(new_key) => {
                                zone_key = new_key;
                                match std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| pcs_keys_for_record(record, &zone_key))) {
                                    Err(_) => { pcs_skipped += 1; None }
                                    Ok(Ok(k)) => Some(k),
                                    Ok(Err(e)) => { pcs_skipped += 1; warn!("cloud_query_attachments_fallback: PCS still missing after refresh for {}: {}", identifier, e); None }
                                }
                            }
                            Err(e) => { warn!("cloud_query_attachments_fallback: zone config refresh failed: {}", e); None }
                        }
                    }
                    Ok(Err(e)) => { pcs_skipped += 1; warn!("cloud_query_attachments_fallback: PCS error for {}: {}", identifier, e); None }
                };
                let pcskey = match pcskey_opt { Some(k) => k, None => continue };

                let find_field = |name: &str| -> Option<&rustpush::cloudkit_proto::record::field::Value> {
                    record.record_field.iter()
                        .find(|f| f.identifier.as_ref().and_then(|i| i.name.as_deref()) == Some(name))
                        .and_then(|f| f.value.as_ref())
                };
                let field_names: Vec<String> = record.record_field.iter()
                    .filter_map(|f| f.identifier.as_ref().and_then(|i| i.name.clone()))
                    .collect();

                let cm_opt: Option<rustpush::cloud_messages::AttachmentMeta> = match find_field("cm") {
                    None => None,
                    Some(v) => match std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                        <rustpush::cloud_messages::GZipWrapper<rustpush::cloud_messages::AttachmentMeta>
                            as rustpush::cloudkit_proto::CloudKitEncryptedValue>::from_value_encrypted(v, &pcskey, "cm")
                    })) {
                        Ok(Some(gzw)) => Some(gzw.0),
                        Ok(None) => None,
                        Err(_panic) => { warn!("cloud_query_attachments_fallback: {} cm decode panicked. fields={:?}", identifier, field_names); None }
                    },
                };
                let cm = match cm_opt {
                    Some(cm) => cm,
                    None => { cm_decode_fail += 1; warn!("cloud_query_attachments_fallback: skipping {} — cm missing/undecodable. fields={:?}", identifier, field_names); continue; }
                };

                let lqa_opt: Option<rustpush::cloudkit_proto::Asset> = match find_field("lqa") {
                    None => None,
                    Some(v) => match std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                        <rustpush::cloudkit_proto::Asset as rustpush::cloudkit_proto::CloudKitEncryptedValue>::from_value_encrypted(v, &pcskey, "lqa")
                    })) {
                        Ok(Some(asset)) => Some(asset),
                        Ok(None) => { lqa_decode_fail += 1; None }
                        Err(_panic) => { lqa_decode_fail += 1; warn!("cloud_query_attachments_fallback: {} lqa decode panicked", identifier); None }
                    },
                };

                let avid_asset: Option<rustpush::cloudkit_proto::Asset> = match find_field("avid") {
                    None => None,
                    Some(v) => match std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                        <rustpush::cloudkit_proto::Asset as rustpush::cloudkit_proto::CloudKitEncryptedValue>::from_value_encrypted(v, &pcskey, "avid")
                    })) {
                        Ok(parsed) => parsed,
                        Err(_panic) => { warn!("cloud_query_attachments_fallback: {} avid decode panicked", identifier); None }
                    },
                };

                let has_avid = avid_asset.as_ref().and_then(|a| a.size).unwrap_or(0) > 0;
                let avid_ford_key = avid_asset.as_ref().and_then(|a| a.protection_info.as_ref()).and_then(|p| p.protection_info.as_ref()).cloned();
                let ford_key = lqa_opt.as_ref().and_then(|lqa| lqa.protection_info.as_ref()).and_then(|p| p.protection_info.as_ref()).cloned();

                if let Some(ref k) = ford_key { register_ford_key(k.clone()); ford_cached += 1; }
                if let Some(ref k) = avid_ford_key { register_ford_key(k.clone()); ford_cached += 1; }

                info!(
                    "cloud_query_attachments_fallback: att guid={} record={} mime={:?} size={} lqa_ok={} avid_ok={}",
                    cm.guid, identifier, cm.mime_type, cm.total_bytes, lqa_opt.is_some(), avid_asset.is_some(),
                );
                normalized.push(WrappedCloudAttachmentInfo {
                    guid: cm.guid.clone(),
                    mime_type: cm.mime_type.clone(),
                    uti_type: cm.uti.clone(),
                    filename: cm.transfer_name.clone().or_else(|| cm.filename.clone()),
                    file_size: cm.total_bytes,
                    record_name: identifier,
                    hide_attachment: cm.hide_attachment,
                    has_avid,
                    ford_key,
                    avid_ford_key,
                });
            }

            match next_marker {
                Some(m) if !m.is_empty() => { qr_continuation = Some(m); }
                _ => break,
            }
        }

        info!(
            "cloud_query_attachments_fallback: {} new records across {} pages, {} pcs_skipped, {} cm_fail, {} lqa_fail, {} ford_cached",
            normalized.len(), qr_page, pcs_skipped, cm_decode_fail, lqa_decode_fail, ford_cached
        );

        Ok(WrappedCloudSyncAttachmentsPage {
            continuation_token: None,
            status: 3,
            done: true,
            attachments: normalized,
        })
    }

    /// Download an attachment from CloudKit by its record name.
    /// Returns the raw file bytes.
    ///
    /// # Ford dedup recovery
    ///
    /// First tries upstream's `download_attachment`. If that path panics
    /// (which happens when MMCS serves a deduplicated Ford blob encrypted
    /// with a different record's key — the `.unwrap()` at the top of
    /// `get_mmcs`'s SIV decrypt), the panic is caught here and the wrapper
    /// retries manually by iterating cached Ford keys. For each cached key,
    /// we fetch the record, mutate `lqa.protection_info.protection_info` to
    /// inject the candidate key, and call `container.get_assets(...)`
    /// directly. This matches the 94f7b8e fix's semantics without touching
    /// upstream rustpush source.
    pub async fn cloud_download_attachment(
        &self,
        record_name: String,
    ) -> Result<Vec<u8>, WrappedError> {
        use futures::FutureExt;
        use rustpush::cloudkit::{FetchRecordOperation, FetchedRecords, ALL_ASSETS};
        use rustpush::cloud_messages::MESSAGES_SERVICE;

        let cloud_messages = self.get_or_init_cloud_messages_client().await?;

        // Hand-rolled mirror of upstream's `download_attachment` that
        // replicates master's Ford-registration-before-get_assets pattern:
        //
        //   1. perform_operations(FetchRecordOperation::many(ALL_ASSETS))
        //   2. parse records as CloudAttachmentWithAvid (lqa + avid)
        //   3. register BOTH lqa.protection_info and avid.protection_info
        //      Ford keys into the wrapper cache BEFORE get_assets runs
        //   4. call container.get_assets(&records.assets, &record.lqa)
        //
        // This ensures that every attachment download contributes its
        // record's own Ford keys to the cache even if sync never reached
        // this record yet (new attachments, cross-device dedup where the
        // source record hasn't been synced). Matches master's behavior
        // at rustpush/src/imessage/cloud_messages.rs:download_attachment.
        //
        // Wrapped in catch_unwind so any SIV panic deep in get_mmcs (Ford
        // dedup with a key not yet in the cache) falls through to
        // `cloud_download_attachment_ford_recovery` for brute-force retry.
        let container = cloud_messages.get_container().await.map_err(|e| WrappedError::GenericError {
            msg: format!("cloud_download_attachment {}: get_container: {e}", record_name),
        })?;
        let zone = container.private_zone("attachmentManateeZone".to_string());
        let zone_key = container
            .get_zone_encryption_config(&zone, &cloud_messages.keychain, &MESSAGES_SERVICE)
            .await
            .map_err(|e| WrappedError::GenericError {
                msg: format!("cloud_download_attachment {}: zone key: {e}", record_name),
            })?;

        let invoke = container
            .perform_operations(
                &CloudKitSession::new(),
                &FetchRecordOperation::many(&ALL_ASSETS, &zone, &[record_name.clone()]),
                IsolationLevel::Operation,
            )
            .await
            .map_err(|e| WrappedError::GenericError {
                msg: format!("cloud_download_attachment {}: perform_operations: {e}", record_name),
            })?;
        let records = FetchedRecords::new(&invoke);
        let record: CloudAttachmentWithAvid = records.get_record(&record_name, Some(&zone_key));

        // Register this record's Ford keys (lqa + avid) into the cache
        // BEFORE the get_assets call — matches master's download_attachment
        // behavior so the fordChecksum / brute-force fallback always has
        // at least the current record's keys available.
        if let Some(pi) = record
            .lqa
            .protection_info
            .as_ref()
            .and_then(|p| p.protection_info.as_ref())
        {
            register_ford_key(pi.clone());
        }
        if let Some(pi) = record
            .avid
            .protection_info
            .as_ref()
            .and_then(|p| p.protection_info.as_ref())
        {
            register_ford_key(pi.clone());
        }

        // Determine upfront whether this asset is Ford-encrypted by parsing
        // the AuthorizeGetResponse body and checking if our file's
        // wanted_chunks entry has a `ford_reference`. Only Ford-encrypted
        // assets (typically videos) benefit from Ford dedup recovery —
        // image attachments use V1 per-chunk encryption and DON'T have a
        // Ford blob, so running recovery on them would brute-force cached
        // keys against bytes that aren't Ford ciphertext (guaranteed to
        // fail, pure wasted work).
        //
        // Master's retry was inside get_mmcs, scoped to the Ford container
        // loop, so it naturally only fired on real Ford failures. The
        // wrapper has to do this check explicitly.
        let is_ford_asset = is_ford_encrypted_asset(&records.assets, &record.lqa);
        // Diagnostic log so we can verify from logs that only Ford-encrypted
        // records are entering the recovery path. If this ever shows
        // is_ford=false for a record that still panics through Ford
        // recovery, something's wrong with the gating.
        info!(
            "cloud_download_attachment {}: is_ford={} mime={:?} filename={:?}",
            record_name,
            is_ford_asset,
            record.cm.0.mime_type.as_deref().unwrap_or("<none>"),
            record.cm.0.transfer_name.as_deref().or(record.cm.0.filename.as_deref()).unwrap_or("<none>")
        );

        // Attempt 1 — get_assets with the record's own lqa, wrapped in
        // catch_unwind so a panic doesn't take down the bridge.
        let shared = SharedWriter::new();
        let assets_tuple: Vec<(&rustpush::cloudkit_proto::Asset, SharedWriter)> =
            vec![(&record.lqa, shared.clone())];
        let fut = container.get_assets(&records.assets, assets_tuple);
        let wrapped = std::panic::AssertUnwindSafe(fut).catch_unwind().await;

        match wrapped {
            Ok(Ok(())) => return Ok(shared.into_bytes()),
            Ok(Err(e)) => {
                return Err(WrappedError::GenericError {
                    msg: format!("Failed to download CloudKit attachment {}: {}", record_name, e),
                });
            }
            Err(_panic) => {
                if !is_ford_asset {
                    // Non-Ford asset (image / V1 per-chunk encryption) —
                    // the panic is NOT a Ford dedup issue. Brute-forcing
                    // cached keys can't help: there's no Ford blob, and
                    // our cached keys are for Ford-encrypted records that
                    // will never match. Return the panic as a terminal
                    // error instead of burning retries.
                    warn!(
                        "cloud_download_attachment {}: non-Ford asset panic, skipping recovery (image or other V1-encrypted)",
                        record_name
                    );
                    return Err(WrappedError::GenericError {
                        msg: format!(
                            "Failed to download CloudKit attachment {} (non-Ford panic, likely bundled_request_id or PCS issue)",
                            record_name
                        ),
                    });
                }
                warn!(
                    "cloud_download_attachment {}: Ford SIV panic, attempting recovery with {} cached Ford keys",
                    record_name,
                    ford_key_cache_size()
                );
            }
        }

        // Attempt 2 — Ford dedup recovery: iterate cached keys in parallel
        // batches, mutate protection_info per attempt, re-try get_assets.
        // Only reached when is_ford_asset == true.
        self.cloud_download_attachment_ford_recovery_with_record(
            container,
            records,
            record,
            &record_name,
        )
        .await
    }

    // Ford recovery helper lives in a separate non-uniffi impl block below
    // (`impl WrappedClient { ford_recovery_download ... }`) — it takes
    // non-FFI reference types and uniffi can't codegen wrappers for it.

    /// Whether this build supports CloudKit avid (Live Photo MOV) downloads.
    /// Always true now — the wrapper hand-rolls the avid path using our
    /// local `CloudAttachmentWithAvid` record type and `container.get_assets`,
    /// so it doesn't depend on any rustpush cargo feature or patch.
    pub fn cloud_supports_avid_download(&self) -> bool {
        true
    }

    /// Download the Live Photo video (avid asset) from a CloudKit attachment record.
    ///
    /// Upstream rustpush's `CloudAttachment` type doesn't have an `avid`
    /// field, so we fetch the record using our local `CloudAttachmentWithAvid`
    /// (which declares the same `attachment` CloudKit record type plus the
    /// extra `avid: Asset` field), register the record's Ford keys into
    /// the wrapper cache, and call `container.get_assets` with
    /// `&record.avid` as the target asset. Wrapped in catch_unwind for
    /// SIV panic safety, and falls through to `cloud_download_avid_ford_recovery`
    /// if the initial attempt panics on a MMCS dedup SIV failure.
    pub async fn cloud_download_attachment_avid(
        &self,
        record_name: String,
    ) -> Result<Vec<u8>, WrappedError> {
        use futures::FutureExt;
        use rustpush::cloudkit::{FetchRecordOperation, FetchedRecords, ALL_ASSETS};
        use rustpush::cloud_messages::MESSAGES_SERVICE;

        let cloud_messages = self.get_or_init_cloud_messages_client().await?;
        let container = cloud_messages.get_container().await.map_err(|e| WrappedError::GenericError {
            msg: format!("cloud_download_attachment_avid {}: get_container: {e}", record_name),
        })?;
        let zone = container.private_zone("attachmentManateeZone".to_string());
        let zone_key = container
            .get_zone_encryption_config(&zone, &cloud_messages.keychain, &MESSAGES_SERVICE)
            .await
            .map_err(|e| WrappedError::GenericError {
                msg: format!("cloud_download_attachment_avid {}: zone key: {e}", record_name),
            })?;

        let invoke = container
            .perform_operations(
                &CloudKitSession::new(),
                &FetchRecordOperation::many(&ALL_ASSETS, &zone, &[record_name.clone()]),
                IsolationLevel::Operation,
            )
            .await
            .map_err(|e| WrappedError::GenericError {
                msg: format!("cloud_download_attachment_avid {}: perform_operations: {e}", record_name),
            })?;
        let records = FetchedRecords::new(&invoke);
        let base_record: CloudAttachmentWithAvid = records.get_record(&record_name, Some(&zone_key));

        // Register this record's Ford keys (both lqa and avid) into the
        // wrapper cache so recovery has visibility if the initial attempt
        // panics on a dedup mismatch.
        if let Some(pi) = base_record
            .lqa
            .protection_info
            .as_ref()
            .and_then(|p| p.protection_info.as_ref())
        {
            register_ford_key(pi.clone());
        }
        if let Some(pi) = base_record
            .avid
            .protection_info
            .as_ref()
            .and_then(|p| p.protection_info.as_ref())
        {
            register_ford_key(pi.clone());
        }

        // Attempt 1 — record's own avid key.
        let shared = SharedWriter::new();
        let assets_tuple: Vec<(&rustpush::cloudkit_proto::Asset, SharedWriter)> =
            vec![(&base_record.avid, shared.clone())];
        let fut = container.get_assets(&records.assets, assets_tuple);
        let result = std::panic::AssertUnwindSafe(fut).catch_unwind().await;
        match result {
            Ok(Ok(())) => return Ok(shared.into_bytes()),
            Ok(Err(e)) => {
                return Err(WrappedError::GenericError {
                    msg: format!("cloud_download_attachment_avid {}: get_assets: {e}", record_name),
                });
            }
            Err(_panic) => {
                warn!(
                    "cloud_download_attachment_avid {}: SIV panic, attempting Ford recovery \
                     with {} cached keys",
                    record_name,
                    ford_key_cache_size()
                );
            }
        }

        // Attempt 2+ — Ford dedup recovery, same pattern as
        // cloud_download_attachment_ford_recovery but targets record.avid.
        self.cloud_download_avid_ford_recovery(container, &records, base_record, &record_name)
            .await
    }

    /// Download a group photo from CloudKit by the chat's record name.
    /// Returns the raw image bytes. The record_name is the CloudKit record
    /// in chatManateeZone where the chat's "gp" asset is stored.
    pub async fn cloud_download_group_photo(
        &self,
        record_name: String,
    ) -> Result<Vec<u8>, WrappedError> {
        let cloud_messages = self.get_or_init_cloud_messages_client().await?;

        let shared = SharedWriter::new();
        let mut files = HashMap::new();
        files.insert(record_name.clone(), shared.clone());
        cloud_messages.download_group_photo(files).await
            .map_err(|e| WrappedError::GenericError {
                msg: format!("Failed to download CloudKit group photo {}: {}", record_name, e),
            })?;
        Ok(shared.into_bytes())
    }

    /// Diagnostic: do a full fresh sync from scratch (no continuation token)
    /// and return total record count + the newest message timestamps per chat.
    /// This bypasses any stored token to check what CloudKit actually has.
    pub async fn cloud_diag_full_count(
        &self,
    ) -> Result<String, WrappedError> {
        let cloud_messages = self.get_or_init_cloud_messages_client().await?;
        
        let mut token: Option<Vec<u8>> = None;
        let mut total_records: usize = 0;
        let mut total_deleted: usize = 0;
        let mut chat_id_counts: HashMap<String, usize> = HashMap::new();
        let mut newest_ts: i64 = 0;
        let mut newest_guid = String::new();
        let mut newest_chat = String::new();
        
        for page in 0..512 {
            let (next_token, messages, status) = cloud_messages.sync_messages(token).await
                .map_err(|e| WrappedError::GenericError { msg: format!("diag sync page {} failed: {}", page, e) })?;
            
            let page_total = messages.len();
            for (_record_name, msg_opt) in &messages {
                if let Some(msg) = msg_opt {
                    total_records += 1;
                    let ts = apple_timestamp_ns_to_unix_ms(msg.time);
                    let chat = &msg.chat_id;
                    *chat_id_counts.entry(chat.clone()).or_insert(0) += 1;
                    if ts > newest_ts {
                        newest_ts = ts;
                        newest_guid = msg.guid.clone();
                        newest_chat = chat.clone();
                    }
                } else {
                    total_deleted += 1;
                }
            }
            info!("diag page {} => {} records (status={})", page, page_total, status);
            
            if status == 3 {
                break;
            }
            token = Some(next_token);
        }
        
        let unique_chats = chat_id_counts.len();
        let result = format!(
            "total_records={} deleted={} unique_chats={} newest_ts={} newest_guid={} newest_chat={}",
            total_records, total_deleted, unique_chats, newest_ts, newest_guid, newest_chat
        );
        info!("CloudKit diag: {}", result);
        Ok(result)
    }

    pub async fn cloud_fetch_recent_messages(
        &self,
        since_timestamp_ms: u64,
        chat_id: Option<String>,
        max_pages: u32,
        max_results: u32,
    ) -> Result<Vec<WrappedCloudSyncMessage>, WrappedError> {
        let cloud_messages = self.get_or_init_cloud_messages_client().await?;
        let since = since_timestamp_ms as i64;
        let max_pages = if max_pages == 0 { 1 } else { max_pages };
        let max_results = if max_results == 0 { 1 } else { max_results as usize };

        let mut token: Option<Vec<u8>> = None;
        let mut deduped: HashMap<String, WrappedCloudSyncMessage> = HashMap::new();

        'pages: for _ in 0..max_pages {
            const MAX_SYNC_ATTEMPTS: usize = 4;
            let mut sync_result = None;
            let mut last_pcs_err: Option<rustpush::PushError> = None;

            for attempt in 0..MAX_SYNC_ATTEMPTS {
                match fetch_main_zone_page_newest_first(&cloud_messages, token.clone()).await {
                    Ok(result) => {
                        sync_result = Some(result);
                        break;
                    }
                    Err(err) if is_pcs_recoverable_error(&err) => {
                        let attempt_no = attempt + 1;
                        warn!(
                            "CloudKit recent fetch hit PCS key error on attempt {}/{}: {}",
                            attempt_no,
                            MAX_SYNC_ATTEMPTS,
                            err
                        );
                        last_pcs_err = Some(err);
                        if attempt_no < MAX_SYNC_ATTEMPTS {
                            self.recover_cloud_pcs_state("CloudKit recent fetch").await?;
                            continue;
                        }
                    }
                    Err(err) => {
                        return Err(WrappedError::GenericError {
                            msg: format!("Failed to sync CloudKit messages: {}", err),
                        });
                    }
                }
            }

            let (next_token, messages, status) = match sync_result {
                Some(result) => result,
                None => {
                    let err = last_pcs_err.map(|e| e.to_string()).unwrap_or_else(|| "unknown error".into());
                    return Err(WrappedError::GenericError {
                        msg: format!("Failed to sync CloudKit messages after PCS recovery retries: {}", err),
                    });
                }
            };

            for (record_name, msg_opt) in messages {
                let Some(msg) = msg_opt else {
                    continue;
                };

                // Skip system/service messages (group renames, participant changes, etc.)
                if msg.flags.intersects(
                    rustpush::cloud_messages::MessageFlags::IS_SYSTEM_MESSAGE
                    | rustpush::cloud_messages::MessageFlags::IS_SERVICE_MESSAGE
                ) {
                    continue;
                }

                if let Some(ref wanted_chat) = chat_id {
                    if &msg.chat_id != wanted_chat {
                        continue;
                    }
                }

                let timestamp_ms = apple_timestamp_ns_to_unix_ms(msg.time);
                if timestamp_ms < since {
                    continue;
                }

                let guid = if msg.guid.is_empty() {
                    record_name.clone()
                } else {
                    msg.guid.clone()
                };

                let tapback_type = msg.msg_proto.associated_message_type;
                let tapback_target_guid = msg.msg_proto.associated_message_guid.clone();
                let tapback_emoji = msg.msg_proto_4.as_ref()
                    .and_then(|p4| p4.associated_message_emoji.clone());

                let date_read_ms = msg.msg_proto.date_read
                    .map(|dr| apple_timestamp_ns_to_unix_ms(dr as i64))
                    .unwrap_or(0);

                // Extract attachment GUIDs from attributedBody + messageSummaryInfo
                let mut attachment_guids: Vec<String> = msg.msg_proto.attributed_body
                    .as_ref()
                    .map(|body| extract_attachment_guids_from_attributed_body(body))
                    .unwrap_or_default()
                    .into_iter()
                    .filter(|g| !g.is_empty() && g.len() <= 256 && g.is_ascii())
                    .collect();
                if let Some(ref summary) = msg.msg_proto.message_summary_info {
                    for sg in extract_attachment_guids_from_summary_info(summary) {
                        if !sg.is_empty() && sg.len() <= 256 && sg.is_ascii()
                            && !attachment_guids.contains(&sg)
                        {
                            attachment_guids.push(sg);
                        }
                    }
                }

                deduped.insert(
                    guid.clone(),
                    WrappedCloudSyncMessage {
                        record_name,
                        guid,
                        cloud_chat_id: msg.chat_id,
                        sender: msg.sender,
                        is_from_me: msg
                            .flags
                            .contains(rustpush::cloud_messages::MessageFlags::IS_FROM_ME),
                        text: msg.msg_proto.text.clone(),
                        subject: msg.msg_proto.subject.clone(),
                        service: msg.service,
                        timestamp_ms,
                        deleted: false,
                        tapback_type,
                        tapback_target_guid,
                        tapback_emoji,
                        attachment_guids,
                        date_read_ms,
                        msg_type: msg.r#type,
                        has_body: msg.msg_proto.attributed_body.is_some(),
                    },
                );

                if deduped.len() >= max_results {
                    break 'pages;
                }
            }

            if status == 3 {
                break;
            }
            token = Some(next_token);
        }

        let mut output = deduped.into_values().collect::<Vec<_>>();
        output.sort_by(|a, b| {
            a.timestamp_ms
                .cmp(&b.timestamp_ms)
                .then_with(|| a.guid.cmp(&b.guid))
        });
        if output.len() > max_results {
            output = output[output.len() - max_results..].to_vec();
        }

        Ok(output)
    }

    /// Test CloudKit Messages access: creates CloudKitClient + KeychainClient + CloudMessagesClient,
    /// then tries to sync chats and messages. Logs results. Returns a summary string.
    pub async fn test_cloud_messages(&self) -> Result<String, WrappedError> {
        let tp = self.token_provider.as_ref()
            .ok_or(WrappedError::GenericError { msg: "No TokenProvider available".into() })?;

        info!("=== CloudKit Messages Test ===");

        // Get needed credentials
        let dsid = tp.get_dsid().await?;
        let adsid = tp.get_adsid().await?;
        let mme_delegate = tp.parse_mme_delegate().await?;
        let account = tp.get_account();
        let os_config = tp.get_os_config();

        info!("DSID: {}, ADSID: {}", dsid, adsid);

        // Get anisette client from the account
        let anisette = account.lock().await.anisette.clone();

        // Create CloudKitState
        let cloudkit_state = rustpush::cloudkit::CloudKitState::new(dsid.clone())
            .ok_or(WrappedError::GenericError { msg: "Failed to create CloudKitState".into() })?;

        // Create CloudKitClient
        let cloudkit = Arc::new(rustpush::cloudkit::CloudKitClient {
            state: rustpush::DebugRwLock::new(cloudkit_state),
            anisette: anisette.clone(),
            config: os_config.clone(),
            token_provider: tp.inner.clone(),
        });

        // Create KeychainClientState
        let keychain_state = rustpush::keychain::KeychainClientState::new(dsid.clone(), adsid.clone(), &mme_delegate)
            .ok_or(WrappedError::GenericError { msg: "Failed to create KeychainClientState — missing KeychainSync config in MobileMe delegate".into() })?;

        info!("KeychainClientState created successfully");

        // Create KeychainClient
        let keychain = Arc::new(rustpush::keychain::KeychainClient {
            anisette: anisette.clone(),
            token_provider: tp.inner.clone(),
            state: rustpush::DebugRwLock::new(keychain_state),
            config: os_config.clone(),
            update_state: Box::new(|_state| {
                // For now, don't persist keychain state
                info!("Keychain state updated (not persisted yet)");
            }),
            container: tokio::sync::Mutex::new(None),
            security_container: tokio::sync::Mutex::new(None),
            client: cloudkit.clone(),
        });

        // Try to sync the keychain (needed for PCS decryption keys)
        info!("Syncing iCloud Keychain...");
        match keychain.sync_keychain(&rustpush::keychain::KEYCHAIN_ZONES).await {
            Ok(()) => info!("Keychain sync successful"),
            Err(e) => {
                let msg = format!("Keychain sync failed: {}. This likely means we need to join the trust circle first.", e);
                warn!("{}", msg);
                return Ok(msg);
            }
        }

        // Create CloudMessagesClient
        let cloud_messages = rustpush::cloud_messages::CloudMessagesClient::new(cloudkit.clone(), keychain.clone());

        // Try counting records first
        info!("Counting CloudKit message records...");
        match cloud_messages.count_records().await {
            Ok(summary) => {
                info!("CloudKit record counts — messages: {}, chats: {}, attachments: {}",
                    summary.messages_summary.len(), summary.chat_summary.len(), summary.attachment_summary.len());
            }
            Err(e) => {
                warn!("Failed to count records: {}", e);
            }
        }

        // Try syncing chats
        info!("Syncing CloudKit chats...");
        let mut total_chats = 0;
        let mut chat_names: Vec<String> = Vec::new();
        match cloud_messages.sync_chats(None).await {
            Ok((_token, chats, status)) => {
                info!("Chat sync returned {} chats (status={})", chats.len(), status);
                for (id, chat_opt) in &chats {
                    if let Some(chat) = chat_opt {
                        let name = chat.display_name.as_deref().unwrap_or("(unnamed)");
                        let participants: Vec<&str> = chat.participants.iter().map(|p| p.uri.as_str()).collect();
                        info!("  Chat: {} | id={} | svc={} | participants={:?}", name, chat.chat_identifier, chat.service_name, participants);
                        chat_names.push(format!("{}: {} [{}]", id, name, chat.chat_identifier));
                    } else {
                        info!("  Chat {} deleted", id);
                    }
                    total_chats += 1;
                }
            }
            Err(e) => {
                let msg = format!("Chat sync failed: {}", e);
                warn!("{}", msg);
                return Ok(msg);
            }
        }

        // Try syncing messages (first page)
        info!("Syncing CloudKit messages (first page)...");
        let mut total_messages = 0;
        match cloud_messages.sync_messages(None).await {
            Ok((_token, messages, status)) => {
                info!("Message sync returned {} messages (status={})", messages.len(), status);
                for (id, msg_opt) in messages.iter().take(20) {
                    if let Some(msg) = msg_opt {
                        let from_me = msg.flags.contains(rustpush::cloud_messages::MessageFlags::IS_FROM_ME);
                        info!("  Msg: {} | chat={} | sender={} | from_me={} | svc={} | guid={}",
                            id, msg.chat_id, msg.sender, from_me, msg.service, msg.guid);
                    } else {
                        info!("  Msg {} deleted", id);
                    }
                    total_messages += 1;
                }
                if messages.len() > 20 {
                    info!("  ... and {} more messages", messages.len() - 20);
                    total_messages = messages.len();
                }
            }
            Err(e) => {
                let msg = format!("Message sync failed: {}", e);
                warn!("{}", msg);
                return Ok(format!("Chats OK ({} chats), but message sync failed: {}", total_chats, e));
            }
        }

        let summary = format!("CloudKit sync OK: {} chats, {} messages (first page)", total_chats, total_messages);
        info!("{}", summary);
        Ok(summary)
    }

    pub async fn stop(&self) {
        let mut handle = self.receive_handle.lock().await;
        if let Some(h) = handle.take() {
            h.abort();
        }
    }
}

// Non-uniffi-exported helpers on `Client`. Methods here take reference types
// (like `&CloudMessagesClient<...>` or `&str`) that uniffi can't generate FFI
// wrappers for, so they live in a plain impl block that the FFI codegen
// ignores.
impl Client {
    /// Ford dedup recovery using an already-fetched record + FetchedRecords.
    /// This variant is called by `cloud_download_attachment` after its first
    /// `get_assets` attempt panics — it reuses the existing record/assets
    /// instead of re-fetching, which saves a CloudKit round-trip per download.
    async fn cloud_download_attachment_ford_recovery_with_record(
        &self,
        container: Arc<rustpush::cloudkit::CloudKitOpenContainer<'static, BridgeDefaultAnisetteProvider>>,
        records: rustpush::cloudkit::FetchedRecords,
        base_record: CloudAttachmentWithAvid,
        record_name: &str,
    ) -> Result<Vec<u8>, WrappedError> {
        // Fully-manual Ford download. Bypasses upstream's V1-only
        // `get_mmcs` (which panics on V2 Ford records) and handles V1,
        // V2, and dedup (cross-batch brute force via the cached key set)
        // in a single pure-crypto pass.
        //
        // The outer `cloud_download_attachment` attempted upstream's
        // happy path first and catch_unwind'd its panic; we're here
        // because that failed. The ONLY path upstream takes to its
        // panic is the Ford path, so we know this is a Ford-encrypted
        // asset (is_ford_asset was also pre-checked by the caller).
        //
        // What this function does NOT do that upstream does:
        //   - getComplete confirmation (CloudKit passes empty url, so
        //     upstream skips it too — see mmcs.rs:1260-1272)
        //   - chunk-level HTTP range requests with streaming matcher
        //     (we fetch each container body in full — simpler, same
        //     result for typical attachment sizes)

        let lqa = &base_record.lqa;
        let bundled_id = lqa.bundled_request_id.as_ref().ok_or_else(|| {
            WrappedError::GenericError {
                msg: format!("manual_ford_download {}: lqa.bundled_request_id is None", record_name),
            }
        })?;
        let signature = lqa.signature.as_ref().ok_or_else(|| {
            WrappedError::GenericError {
                msg: format!("manual_ford_download {}: lqa.signature is None", record_name),
            }
        })?;
        let ford_key = lqa
            .protection_info
            .as_ref()
            .and_then(|pi| pi.protection_info.as_ref())
            .cloned()
            .unwrap_or_default();

        // Find the AssetGetResponse for this asset's bundled_request_id.
        let asset_response = records
            .assets
            .iter()
            .find(|r| r.asset_id.as_ref() == Some(bundled_id))
            .ok_or_else(|| WrappedError::GenericError {
                msg: format!(
                    "manual_ford_download {}: no AssetGetResponse for bundled_request_id",
                    record_name
                ),
            })?;
        let body = asset_response.body.as_ref().ok_or_else(|| {
            WrappedError::GenericError {
                msg: format!(
                    "manual_ford_download {}: AssetGetResponse.body is None",
                    record_name
                ),
            }
        })?;

        // User agent for MMCS fetches — CloudKit style (matches what
        // upstream's `get_assets` constructs at cloudkit.rs:2080).
        let user_agent = container
            .client
            .config
            .get_normal_ua("CloudKit/1970");

        info!(
            "manual_ford_download {}: starting V1+V2 Ford path (ford_key_len={} cache_size={})",
            record_name,
            ford_key.len(),
            ford_key_cache_size()
        );

        match manual_ford::manual_ford_download_asset(
            body,
            signature,
            &ford_key,
            &user_agent,
            record_name,
        )
        .await
        {
            Ok(bytes) => {
                warn!(
                    "manual_ford_download {}: SUCCESS bytes={}",
                    record_name,
                    bytes.len()
                );
                Ok(bytes)
            }
            Err(e) => {
                warn!("manual_ford_download {}: FAILED: {}", record_name, e);
                Err(WrappedError::GenericError {
                    msg: format!("Manual Ford download for {}: {}", record_name, e),
                })
            }
        }
    }

    /// Ford dedup recovery path: fetch the CloudAttachment record, iterate
    /// over every cached Ford key, mutate `Asset.protection_info` per attempt,
    /// and retry `container.get_assets(...)` wrapped in `catch_unwind` until
    /// one candidate SIV-decrypts cleanly. Matches the 94f7b8e fix's
    /// cross-batch recovery semantics without modifying upstream rustpush.
    #[allow(dead_code)]
    async fn cloud_download_attachment_ford_recovery(
        &self,
        cloud_messages: &rustpush::cloud_messages::CloudMessagesClient<BridgeDefaultAnisetteProvider>,
        record_name: &str,
    ) -> Result<Vec<u8>, WrappedError> {
        use futures::FutureExt;
        use rustpush::cloudkit::{FetchRecordOperation, FetchedRecords, ALL_ASSETS};
        use rustpush::cloud_messages::{CloudAttachment, MESSAGES_SERVICE};

        let container = cloud_messages.get_container().await.map_err(|e| WrappedError::GenericError {
            msg: format!("Ford recovery: get_container failed: {e}"),
        })?;
        let zone = container.private_zone("attachmentManateeZone".to_string());
        let zone_key = container
            .get_zone_encryption_config(&zone, &cloud_messages.keychain, &MESSAGES_SERVICE)
            .await
            .map_err(|e| WrappedError::GenericError {
                msg: format!("Ford recovery: get_zone_encryption_config failed: {e}"),
            })?;

        let invoke = container
            .perform_operations(
                &CloudKitSession::new(),
                &FetchRecordOperation::many(&ALL_ASSETS, &zone, &[record_name.to_string()]),
                IsolationLevel::Operation,
            )
            .await
            .map_err(|e| WrappedError::GenericError {
                msg: format!("Ford recovery: perform_operations failed: {e}"),
            })?;
        let records = FetchedRecords::new(&invoke);
        let base_record: CloudAttachment = records.get_record(record_name, Some(&zone_key));

        // Register the record's own key in case sync hasn't touched it yet
        // (e.g. direct download of a just-arrived attachment).
        if let Some(pi) = base_record
            .lqa
            .protection_info
            .as_ref()
            .and_then(|p| p.protection_info.as_ref())
        {
            register_ford_key(pi.clone());
        }

        let cached_keys = ford_key_cache_values();
        info!(
            "Ford recovery {}: trying {} cached keys",
            record_name,
            cached_keys.len()
        );

        // Pre-flight: if the record's lqa has no bundled_request_id, upstream's
        // `get_assets` would panic immediately at cloudkit.rs:2075 with "No
        // bundled asset!" — no point iterating 900 keys. Bail out early with
        // a clear error instead.
        if base_record.lqa.bundled_request_id.is_none() {
            warn!(
                "Ford recovery {}: lqa.bundled_request_id is None (record not ALL_ASSETS-authorized?), skipping recovery",
                record_name
            );
            return Err(WrappedError::GenericError {
                msg: format!("Ford dedup recovery for {}: lqa asset has no bundled_request_id", record_name),
            });
        }

        for (idx, alt_key) in cached_keys.iter().enumerate() {
            // Clone per attempt so we can mutate `protection_info`
            // independently and retry cleanly on panic.
            let mut record = base_record.clone();
            if let Some(pi) = record.lqa.protection_info.as_mut() {
                pi.protection_info = Some(alt_key.clone());
            } else {
                continue;
            }

            let shared = SharedWriter::new();
            let assets_tuple: Vec<(&rustpush::cloudkit_proto::Asset, SharedWriter)> =
                vec![(&record.lqa, shared.clone())];

            let fut = container.get_assets(&records.assets, assets_tuple);
            let result = std::panic::AssertUnwindSafe(fut).catch_unwind().await;
            match result {
                Ok(Ok(())) => {
                    let bytes = shared.into_bytes();
                    warn!(
                        "Ford dedup recovery SUCCESS: record={} attempt={}/{} bytes={}",
                        record_name,
                        idx + 1,
                        cached_keys.len(),
                        bytes.len()
                    );
                    return Ok(bytes);
                }
                Ok(Err(e)) => {
                    debug!("Ford recovery attempt {} returned error: {}", idx + 1, e);
                }
                Err(_panic) => {
                    debug!(
                        "Ford recovery attempt {} panicked (wrong key, retrying)",
                        idx + 1
                    );
                }
            }
        }

        warn!(
            "Ford dedup recovery FAILED: record={} tried={} all cached keys exhausted",
            record_name,
            cached_keys.len()
        );
        Err(WrappedError::GenericError {
            msg: format!(
                "Ford dedup recovery for {}: all {} cached keys failed SIV decrypt",
                record_name,
                cached_keys.len()
            ),
        })
    }

    /// Ford dedup recovery for the Live Photo MOV (`avid`) asset. Same
    /// algorithm as `cloud_download_attachment_ford_recovery` but operates
    /// on `record.avid` instead of `record.lqa`. Takes an already-fetched
    /// `CloudAttachmentWithAvid` and `FetchedRecords` so we don't re-hit
    /// the CloudKit record endpoint per attempt.
    async fn cloud_download_avid_ford_recovery(
        &self,
        container: Arc<rustpush::cloudkit::CloudKitOpenContainer<'static, BridgeDefaultAnisetteProvider>>,
        records: &rustpush::cloudkit::FetchedRecords,
        base_record: CloudAttachmentWithAvid,
        record_name: &str,
    ) -> Result<Vec<u8>, WrappedError> {
        // Manual V1+V2 Ford download for the Live Photo (avid) asset.
        // Same algorithm as the lqa recovery path, but targeting
        // `base_record.avid` instead of `base_record.lqa`.
        let avid = &base_record.avid;
        let bundled_id = avid.bundled_request_id.as_ref().ok_or_else(|| {
            WrappedError::GenericError {
                msg: format!(
                    "manual_ford_download (avid) {}: avid.bundled_request_id is None",
                    record_name
                ),
            }
        })?;
        let signature = avid.signature.as_ref().ok_or_else(|| {
            WrappedError::GenericError {
                msg: format!("manual_ford_download (avid) {}: avid.signature is None", record_name),
            }
        })?;
        let ford_key = avid
            .protection_info
            .as_ref()
            .and_then(|pi| pi.protection_info.as_ref())
            .cloned()
            .unwrap_or_default();

        let asset_response = records
            .assets
            .iter()
            .find(|r| r.asset_id.as_ref() == Some(bundled_id))
            .ok_or_else(|| WrappedError::GenericError {
                msg: format!(
                    "manual_ford_download (avid) {}: no AssetGetResponse for bundled_request_id",
                    record_name
                ),
            })?;
        let body = asset_response.body.as_ref().ok_or_else(|| {
            WrappedError::GenericError {
                msg: format!(
                    "manual_ford_download (avid) {}: AssetGetResponse.body is None",
                    record_name
                ),
            }
        })?;

        let user_agent = container
            .client
            .config
            .get_normal_ua("CloudKit/1970");

        info!(
            "manual_ford_download (avid) {}: starting V1+V2 Ford path",
            record_name
        );

        match manual_ford::manual_ford_download_asset(
            body,
            signature,
            &ford_key,
            &user_agent,
            record_name,
        )
        .await
        {
            Ok(bytes) => {
                warn!(
                    "manual_ford_download (avid) {}: SUCCESS bytes={}",
                    record_name,
                    bytes.len()
                );
                Ok(bytes)
            }
            Err(e) => {
                warn!("manual_ford_download (avid) {}: FAILED: {}", record_name, e);
                Err(WrappedError::GenericError {
                    msg: format!("Manual Ford download (avid) for {}: {}", record_name, e),
                })
            }
        }
    }
}

/// Fetch one page of messageManateeZone with newest-first ordering.
/// Returns (next_token, messages_map, status) — same shape as sync_messages().
///
/// Standalone (not a method on Client) so uniffi doesn't try to generate FFI
/// bindings for it. We call the CloudKit container directly (same pattern as
/// list_recoverable_chats) so we can set newest_first=true without touching the
/// vendored sync_messages path. This lets cloud_fetch_recent_messages find recent
/// messages in the first ~50 pages instead of requiring 200+ oldest-first pages.
async fn fetch_main_zone_page_newest_first(
    cloud_messages: &rustpush::cloud_messages::CloudMessagesClient<BridgeDefaultAnisetteProvider>,
    token: Option<Vec<u8>>,
) -> Result<(Vec<u8>, HashMap<String, Option<rustpush::cloud_messages::CloudMessage>>, i32), rustpush::PushError> {
    use rustpush::cloudkit::{pcs_keys_for_record, FetchRecordChangesOperation, CloudKitSession, NO_ASSETS};
    use rustpush::cloudkit_proto::CloudKitRecord;
    use rustpush::cloud_messages::{MESSAGES_SERVICE, CloudMessage};

    let container = cloud_messages.get_container().await?;
    let zone_id = container.private_zone("messageManateeZone".to_string());
    let mut key = container
        .get_zone_encryption_config(&zone_id, &cloud_messages.keychain, &MESSAGES_SERVICE)
        .await?;

    // Create the op then flip newest_first — the inner proto field is pub, so we
    // can mutate it here without touching the vendored FetchRecordChangesOperation::new.
    let mut op = FetchRecordChangesOperation::new(zone_id.clone(), token, &NO_ASSETS);
    op.0.newest_first = Some(true);

    let (_assets, response) = container.perform(&CloudKitSession::new(), op).await?;

    let mut results = HashMap::new();
    let mut refreshed = false;
    for change in &response.change {
        let identifier = change.identifier.as_ref().unwrap()
            .value.as_ref().unwrap().name().to_string();
        let Some(record) = &change.record else {
            results.insert(identifier, None);
            continue;
        };
        if record.r#type.as_ref().unwrap().name() != CloudMessage::record_type() {
            continue;
        }
        let pcskey = match pcs_keys_for_record(record, &key) {
            Ok(k) => Some(k),
            Err(rustpush::PushError::PCSRecordKeyMissing) if !refreshed => {
                container.clear_cache_zone_encryption_config(&zone_id).await;
                key = container.get_zone_encryption_config(&zone_id, &cloud_messages.keychain, &MESSAGES_SERVICE).await?;
                refreshed = true;
                match pcs_keys_for_record(record, &key) {
                    Ok(k) => Some(k),
                    Err(rustpush::PushError::PCSRecordKeyMissing) => {
                        warn!("Skipping record {}: PCS key missing (newest-first fetch)", identifier);
                        None
                    }
                    Err(e) => return Err(e),
                }
            }
            Err(e) if matches!(e,
                rustpush::PushError::PCSRecordKeyMissing
                | rustpush::PushError::ShareKeyNotFound(_)
                | rustpush::PushError::DecryptionKeyNotFound(_)
                | rustpush::PushError::MasterKeyNotFound
            ) => {
                warn!("Skipping record {} due to PCS key error: {}", identifier, e);
                None
            }
            Err(e) => return Err(e),
        };
        let Some(pcskey) = pcskey else { continue };
        let item = CloudMessage::from_record_encrypted(
            &record.record_field,
            Some(&pcskey),
        );
        results.insert(identifier, Some(item));
    }
    Ok((response.sync_continuation_token().to_vec(), results, response.status()))
}

impl Drop for Client {
    fn drop(&mut self) {
        if let Ok(mut handle) = self.receive_handle.try_lock() {
            if let Some(h) = handle.take() {
                h.abort();
            }
        }
    }
}

/// Fallback for cloud_sync_messages when sync_messages panics on a malformed CloudKit record.
/// Replicates sync_records logic for "messageManateeZone" but handles None proto fields
/// gracefully (no .unwrap()) and wraps from_record_encrypted in catch_unwind per record.
async fn sync_messages_fallback(
    cloud_messages: &Arc<rustpush::cloud_messages::CloudMessagesClient<BridgeDefaultAnisetteProvider>>,
    token: Option<Vec<u8>>,
) -> Result<(Vec<u8>, HashMap<String, Option<rustpush::cloud_messages::CloudMessage>>, i32), rustpush::PushError> {
    use rustpush::cloudkit::{pcs_keys_for_record, FetchRecordChangesOperation, CloudKitSession, NO_ASSETS};
    use rustpush::cloudkit_proto::CloudKitRecord;
    use rustpush::cloud_messages::{MESSAGES_SERVICE, CloudMessage};

    let container = cloud_messages.get_container().await?;
    let zone_id = container.private_zone("messageManateeZone".to_string());
    let key = container
        .get_zone_encryption_config(&zone_id, &cloud_messages.keychain, &MESSAGES_SERVICE)
        .await?;

    let (_assets, response) = container
        .perform(
            &CloudKitSession::new(),
            FetchRecordChangesOperation::new(zone_id, token, &NO_ASSETS),
        )
        .await?;

    let mut results: HashMap<String, Option<CloudMessage>> = HashMap::new();
    let mut skipped = 0usize;

    for change in &response.change {
        // Graceful identifier extraction — no .unwrap()
        let id_proto = match change.identifier.as_ref() {
            Some(p) => p,
            None => { warn!("sync_messages_fallback: skipping change with missing identifier"); skipped += 1; continue; }
        };
        let id_value = match id_proto.value.as_ref() {
            Some(v) => v,
            None => { warn!("sync_messages_fallback: skipping change with missing identifier value"); skipped += 1; continue; }
        };
        let identifier = id_value.name().to_string();

        let record = match &change.record {
            Some(r) => r,
            None => { results.insert(identifier, None); continue; } // deleted record
        };

        // Graceful type check — no .unwrap()
        if record.r#type.as_ref().map_or(true, |t| t.name() != CloudMessage::record_type()) {
            continue;
        }

        let pcskey = match pcs_keys_for_record(record, &key) {
            Ok(k) => k,
            Err(e) => {
                warn!("sync_messages_fallback: skipping record {}: PCS key error: {}", identifier, e);
                skipped += 1;
                continue;
            }
        };

        // from_record_encrypted may panic on corrupt field data — catch it
        let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            CloudMessage::from_record_encrypted(&record.record_field, Some(&pcskey))
        }));

        match result {
            Ok(msg) => { results.insert(identifier, Some(msg)); }
            Err(e) => {
                let msg = if let Some(s) = e.downcast_ref::<String>() { s.clone() }
                          else if let Some(s) = e.downcast_ref::<&str>() { s.to_string() }
                          else { "unknown panic".to_string() };
                warn!("sync_messages_fallback: skipping record {}: deserialization panic: {}", identifier, msg);
                skipped += 1;
            }
        }
    }

    if skipped > 0 {
        warn!("sync_messages_fallback: skipped {} malformed record(s)", skipped);
    }

    Ok((
        response.sync_continuation_token().to_vec(),
        results,
        response.status(),
    ))
}

uniffi::setup_scaffolding!();
