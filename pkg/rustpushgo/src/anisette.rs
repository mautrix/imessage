//! Linux anisette wrapper around upstream's `RemoteAnisetteProviderV3`.
//!
//! Upstream's provisioning has three bugs we work around here:
//!   1. The `ProvisionInput` enum is missing `EndProvisioningError`, so a
//!      transient Apple rejection crashes serde instead of returning an error.
//!   2. The provision() loop (`let Some(Ok(data)) = ... else { continue }`)
//!      spins forever if the WebSocket stream closes.
//!   3. `get_anisette_headers` contains a bare `panic!()` for any
//!      non-`AnisetteNotProvisioned` error from `get_headers` (see
//!      `remote_anisette_v3.rs:417`). If that panic unwinds across the
//!      uniffi FFI boundary while the caller holds the shared
//!      `tokio::sync::Mutex<anisette>` (TokenProvider, CloudKitClient,
//!      KeychainClient all share it), every subsequent anisette-touching
//!      operation deadlocks — including message send.
//!
//! This wrapper catches those failures, retries, and adds a timeout. All
//! Apple-facing requests go through upstream's code unchanged.
//!
//! STATE PRESERVATION: we DO NOT automatically delete state.plist on
//! transient errors. The anisette state carries the device identity
//! (`X-Apple-I-MD-M`, `X-Apple-I-MD-LU`, etc.) that Apple uses to recognize
//! this bridge as a trusted device. Re-provisioning generates a fresh
//! identity, which Apple treats as a new device — forcing 2FA on the next
//! `login_email_pass` call. Historically this wrapper wiped state on any
//! serde error / panic / timeout, which caused identity churn whenever
//! upstream transiently flaked, and that manifested as daily NeedsDevice2FA
//! failures on every CloudKit auth cycle at the 24h PET-expiration boundary.
//!
//! If a user genuinely wants a fresh anisette identity (suspecting state
//! corruption), they can delete `state/anisette/state.plist` manually and
//! accept the one-time 2FA prompt on next auth.

use std::collections::HashMap;
use std::panic::AssertUnwindSafe;
use std::path::PathBuf;
use std::time::Duration;

use futures::FutureExt;
use log::{error, info, warn};
use omnisette::remote_anisette_v3::RemoteAnisetteProviderV3;
use omnisette::{AnisetteError, AnisetteProvider, LoginClientInfo};

const ANISETTE_URL: &str = "https://ani.sidestore.io";
const PROVISION_TIMEOUT: Duration = Duration::from_secs(30);
const MAX_RETRIES: usize = 3;

pub struct BridgeAnisetteProvider {
    info: LoginClientInfo,
    state_path: PathBuf,
}

impl BridgeAnisetteProvider {
    pub fn new(info: LoginClientInfo, state_path: PathBuf) -> Self {
        Self { info, state_path }
    }
}

impl AnisetteProvider for BridgeAnisetteProvider {
    fn get_anisette_headers(
        &mut self,
    ) -> impl std::future::Future<Output = Result<HashMap<String, String>, AnisetteError>> + Send
    {
        async move {
            let mut last_err = None;
            let call_start = std::time::Instant::now();

            for attempt in 0..MAX_RETRIES {
                let attempt_start = std::time::Instant::now();
                info!(
                    "anisette: starting attempt {}/{} (total elapsed {:?})",
                    attempt + 1,
                    MAX_RETRIES,
                    call_start.elapsed()
                );

                // Fresh upstream provider each attempt — it reads state from
                // disk so a cleared state.plist forces re-provisioning.
                let mut upstream = RemoteAnisetteProviderV3::new(
                    ANISETTE_URL.to_string(),
                    self.info.clone(),
                    self.state_path.clone(),
                );

                // AssertUnwindSafe + catch_unwind turns upstream's bare
                // `panic!()` into a caught panic payload. Without this the
                // panic unwinds into the caller's critical section and can
                // leave shared mutexes locked.
                let inner = AssertUnwindSafe(upstream.get_anisette_headers()).catch_unwind();
                // Preserve state.plist across every failure mode — wiping it
                // mid-retry generates a fresh device identity on the next
                // attempt, which Apple sees as a brand-new device and rejects
                // the next login_email_pass with NeedsDevice2FA. The transient
                // upstream errors below don't correlate with genuine state
                // corruption, so we leave state alone and just retry.
                match tokio::time::timeout(PROVISION_TIMEOUT, inner).await {
                    Ok(Ok(Ok(headers))) => {
                        info!(
                            "anisette: attempt {}/{} succeeded in {:?} (total {:?})",
                            attempt + 1,
                            MAX_RETRIES,
                            attempt_start.elapsed(),
                            call_start.elapsed()
                        );
                        return Ok(headers);
                    }
                    Ok(Ok(Err(AnisetteError::SerdeError(ref e)))) => {
                        warn!(
                            "anisette: upstream serde error on attempt {}/{} (state preserved): {}",
                            attempt + 1,
                            MAX_RETRIES,
                            e
                        );
                        last_err = Some(AnisetteError::InvalidArgument(format!(
                            "Anisette provisioning was rejected by the server \
                             (attempt {}/{}). Error: {}",
                            attempt + 1,
                            MAX_RETRIES,
                            e
                        )));
                    }
                    Ok(Ok(Err(e))) => {
                        // Non-serde error — don't retry blindly.
                        return Err(e);
                    }
                    Ok(Err(panic_payload)) => {
                        // Upstream `RemoteAnisetteProviderV3::get_anisette_headers`
                        // contains `panic!()` for non-`AnisetteNotProvisioned`
                        // errors. Convert to a retryable error so the panic
                        // doesn't unwind past this point. State preserved —
                        // the panic is a control-flow bug in upstream, not a
                        // sign that our identity is invalid.
                        let msg = if let Some(s) = panic_payload.downcast_ref::<&'static str>() {
                            (*s).to_string()
                        } else if let Some(s) = panic_payload.downcast_ref::<String>() {
                            s.clone()
                        } else {
                            "unknown panic payload".into()
                        };
                        warn!(
                            "anisette: upstream panicked on attempt {}/{} (state preserved): {}",
                            attempt + 1,
                            MAX_RETRIES,
                            msg
                        );
                        last_err = Some(AnisetteError::InvalidArgument(format!(
                            "Anisette call panicked (attempt {}/{}): {}",
                            attempt + 1,
                            MAX_RETRIES,
                            msg
                        )));
                    }
                    Err(_) => {
                        // Timeout — upstream's infinite-loop bug on WS drop.
                        // Network-layer issue, not state corruption.
                        warn!(
                            "anisette: upstream timed out on attempt {}/{} (state preserved, likely infinite loop on WS drop)",
                            attempt + 1,
                            MAX_RETRIES,
                        );
                        last_err = Some(AnisetteError::InvalidArgument(
                            format!(
                                "Anisette provisioning timed out (attempt {}/{}). \
                                 The anisette server (ani.sidestore.io) may be down.",
                                attempt + 1,
                                MAX_RETRIES,
                            ),
                        ));
                    }
                }
            }

            error!(
                "anisette: all {} attempts exhausted in {:?} — returning error to caller (lock will be released)",
                MAX_RETRIES,
                call_start.elapsed()
            );
            Err(last_err.unwrap_or_else(|| {
                AnisetteError::InvalidArgument("Anisette provisioning failed".into())
            }))
        }
    }
}
