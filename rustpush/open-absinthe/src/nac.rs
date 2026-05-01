use std::cell::{RefCell, UnsafeCell};
use std::collections::HashMap;
use std::fmt;
use std::fs;
use std::io::Read;
use std::rc::Rc;
use std::sync::{Mutex, OnceLock};

use goblin::mach::cputype::CPU_TYPE_X86_64;
use goblin::mach::Mach;
use log::{debug, info, warn};
use serde::{Deserialize, Serialize, Serializer, Deserializer, de};
use sha1::{Sha1, Digest};
use unicorn_engine::unicorn_const::{Arch, Mode, Prot};
use unicorn_engine::{RegisterX86, Unicorn};

use crate::AbsintheError;

// ============================================================================
// NAC relay config (Apple Silicon hardware keys)
// ============================================================================
//
// Hardware keys extracted from Apple Silicon Macs can't be driven by the
// local unicorn x86-64 emulator. For those users, `extract-key` embeds a
// relay URL + bearer token into the hardware-key JSON blob. At runtime,
// the wrapper calls `set_relay_config` to stash the URL + token + optional
// cert fingerprint here, and `ValidationCtx::new()` checks it first. If a
// relay is configured, NAC flows through a 3-step HTTPS protocol against
// the relay server (`tools/nac-relay`, running on a Mac) instead of the
// local emulator. The relay uses native `AAAbsintheContext` via
// `nac-validation` to produce real Apple-accepted bytes at each step.

#[derive(Clone, Debug)]
pub struct RelayConfig {
    pub url: String,
    pub token: Option<String>,
    pub cert_fp: Option<String>,
}

fn relay_config() -> &'static Mutex<Option<RelayConfig>> {
    static CONFIG: OnceLock<Mutex<Option<RelayConfig>>> = OnceLock::new();
    CONFIG.get_or_init(|| Mutex::new(None))
}

/// Install a NAC relay so subsequent `ValidationCtx::new()` calls route
/// validation through the relay's single-shot `/validation-data` endpoint
/// instead of the local x86-64 emulator. Idempotent; overwriting is fine.
///
/// The URL should be the full relay endpoint, e.g.
/// `https://host:5001/validation-data`. If a bare base URL is passed
/// (no `/validation-data` suffix), the suffix is appended automatically.
pub fn set_relay_config(url: String, token: Option<String>, cert_fp: Option<String>) {
    let trimmed = url.trim_end_matches('/');
    let full_url = if trimmed.ends_with("/validation-data") {
        trimmed.to_string()
    } else {
        format!("{}/validation-data", trimmed)
    };
    let cfg = RelayConfig { url: full_url, token, cert_fp };
    let mut slot = match relay_config().lock() {
        Ok(g) => g,
        Err(poisoned) => poisoned.into_inner(),
    };
    info!("NAC relay URL: {}", cfg.url);
    *slot = Some(cfg);
}

fn get_relay_config() -> Option<RelayConfig> {
    let slot = match relay_config().lock() {
        Ok(g) => g,
        Err(poisoned) => poisoned.into_inner(),
    };
    slot.clone()
}

// ---------------------------------------------------------------------------
// Pre-fetched validation data (Apple Silicon relay path)
//
// The bridge pre-fetches validation data from the relay and stashes it here.
// sign() checks this before returning the emulator's NACSign output — if
// set, the relay's data is authoritative and the emulator result is discarded.
// This lets the emulator handle NACInit/KeyEstablishment (producing valid
// request bytes for upstream's Apple handshake) while the relay provides
// the actual signed validation data.
// ---------------------------------------------------------------------------

fn prefetched_data() -> &'static Mutex<Option<Vec<u8>>> {
    static DATA: OnceLock<Mutex<Option<Vec<u8>>>> = OnceLock::new();
    DATA.get_or_init(|| Mutex::new(None))
}

/// Stash pre-fetched validation data from the NAC relay.
/// The next call to `ValidationCtx::sign()` will return this data
/// instead of the emulator's NACSign output.
pub fn set_prefetched_validation_data(data: Vec<u8>) {
    info!("NAC: stashed {} bytes of pre-fetched relay validation data", data.len());
    let mut slot = match prefetched_data().lock() {
        Ok(g) => g,
        Err(p) => p.into_inner(),
    };
    *slot = Some(data);
}

/// Take the pre-fetched validation data (if any). Returns None if
/// nothing was stashed or it was already consumed.
fn take_prefetched_validation_data() -> Option<Vec<u8>> {
    let mut slot = match prefetched_data().lock() {
        Ok(g) => g,
        Err(p) => p.into_inner(),
    };
    slot.take()
}

/// Build a ureq agent that trusts the relay's self-signed TLS certificate
/// if a fingerprint is configured. Matches the danger-accept-invalid-certs
/// behavior the master-branch reqwest client used.
fn relay_agent(cfg: &RelayConfig) -> Result<ureq::Agent, AbsintheError> {
    use native_tls::TlsConnector;
    let mut builder = TlsConnector::builder();
    builder.danger_accept_invalid_certs(true);
    builder.danger_accept_invalid_hostnames(true);
    let connector = builder
        .build()
        .map_err(|e| AbsintheError::Other(format!("relay TLS build failed: {e}")))?;
    let agent = ureq::AgentBuilder::new()
        .tls_connector(std::sync::Arc::new(connector))
        .timeout(std::time::Duration::from_secs(30))
        .build();
    // cert_fp pinning is best-effort: ureq exposes the peer cert only when
    // rustls is used. For now we rely on the bearer token as the primary
    // authenticator (matches the master-branch reqwest path).
    let _ = cfg.cert_fp;
    Ok(agent)
}

// relay_post_json and b64_decode removed — the relay now uses a single-shot
// POST to /validation-data (matching master's approach) instead of a 3-step
// JSON protocol.

// ============================================================================
// XNU IOKit property encryption (x86_64 Linux only)
// ============================================================================

/// FFI binding to the XNU kernel encryption function extracted from the macOS
/// kernel.  Only available when compiled on x86_64-linux (cfg `has_xnu_encrypt`
/// is set by build.rs).
#[cfg(has_xnu_encrypt)]
extern "C" {
    fn sub_ffffff8000ec7320(data: *const u8, size: u64, output: *mut u8);
}

/// Encrypt a plaintext IOKit property value using the XNU kernel function.
/// Returns 17 bytes of encrypted output, or an error if the function is not
/// available on this platform.
#[cfg(has_xnu_encrypt)]
fn encrypt_io_property(data: &[u8]) -> Result<Vec<u8>, AbsintheError> {
    let mut output = vec![0u8; 17];
    unsafe {
        sub_ffffff8000ec7320(data.as_ptr(), data.len() as u64, output.as_mut_ptr());
    }
    Ok(output)
}

/// Parse a UUID string like "XXXXXXXX-XXXX-XXXX-XXXX-XXXXXXXXXXXX" into 16
/// raw bytes.
#[cfg(has_xnu_encrypt)]
fn uuid_str_to_bytes(uuid: &str) -> Result<[u8; 16], AbsintheError> {
    let hex_str: String = uuid.chars().filter(|c| *c != '-').collect();
    if hex_str.len() != 32 {
        return Err(AbsintheError::Other(format!(
            "Invalid UUID length: expected 32 hex chars, got {} from '{}'",
            hex_str.len(),
            uuid
        )));
    }
    let mut bytes = [0u8; 16];
    for i in 0..16 {
        bytes[i] = u8::from_str_radix(&hex_str[i * 2..i * 2 + 2], 16).map_err(|e| {
            AbsintheError::Other(format!("Invalid hex in UUID '{}': {}", uuid, e))
        })?;
    }
    Ok(bytes)
}

/// Given a HardwareConfig, compute any missing `_enc` fields from their
/// plaintext counterparts using the XNU kernel encryption function.
/// This is needed for keys extracted from macOS High Sierra (10.13) which
/// lacks the encrypted IOKit properties in its kernel.
#[cfg(has_xnu_encrypt)]
fn compute_missing_enc_fields(hw: &mut HardwareConfig) -> Result<(), AbsintheError> {
    // platform_serial_number → Gq3489ugfi (serial as string bytes)
    if hw.platform_serial_number_enc.is_empty() && !hw.platform_serial_number.is_empty() {
        info!("Computing missing platform_serial_number_enc (Gq3489ugfi) from plaintext");
        hw.platform_serial_number_enc =
            encrypt_io_property(hw.platform_serial_number.as_bytes())?;
    }

    // platform_uuid → Fyp98tpgj (UUID as 16 raw bytes)
    if hw.platform_uuid_enc.is_empty() && !hw.platform_uuid.is_empty() {
        info!("Computing missing platform_uuid_enc (Fyp98tpgj) from plaintext");
        let uuid_bytes = uuid_str_to_bytes(&hw.platform_uuid)?;
        hw.platform_uuid_enc = encrypt_io_property(&uuid_bytes)?;
    }

    // root_disk_uuid → kbjfrfpoJU (UUID as 16 raw bytes)
    if hw.root_disk_uuid_enc.is_empty() && !hw.root_disk_uuid.is_empty() {
        info!("Computing missing root_disk_uuid_enc (kbjfrfpoJU) from plaintext");
        let uuid_bytes = uuid_str_to_bytes(&hw.root_disk_uuid)?;
        hw.root_disk_uuid_enc = encrypt_io_property(&uuid_bytes)?;
    }

    // mlb → abKPld1EcMni (MLB as raw string bytes)
    if hw.mlb_enc.is_empty() && !hw.mlb.is_empty() {
        info!("Computing missing mlb_enc (abKPld1EcMni) from plaintext");
        hw.mlb_enc = encrypt_io_property(hw.mlb.as_bytes())?;
    }

    // rom → oycqAZloTNDm (ROM as 6 raw bytes)
    if hw.rom_enc.is_empty() && !hw.rom.is_empty() {
        info!("Computing missing rom_enc (oycqAZloTNDm) from plaintext");
        hw.rom_enc = encrypt_io_property(&hw.rom)?;
    }

    Ok(())
}

/// Public wrapper for `compute_missing_enc_fields`.
/// On x86_64 Linux, derives any absent `_enc` fields from their plaintext
/// counterparts using the XNU kernel encryption function.
/// On other platforms, returns an error.
pub fn enrich_missing_enc_fields(hw: &mut HardwareConfig) -> Result<(), AbsintheError> {
    #[cfg(has_xnu_encrypt)]
    {
        compute_missing_enc_fields(hw)?;
        Ok(())
    }
    #[cfg(not(has_xnu_encrypt))]
    {
        Err(AbsintheError::Other(
            "Missing _enc field derivation is only supported on x86_64 Linux builds".into(),
        ))
    }
}

// ============================================================================
// Serde helpers
// ============================================================================

pub fn bin_serialize<S>(x: &[u8], s: S) -> Result<S::Ok, S::Error>
where
    S: Serializer,
{
    s.serialize_bytes(x)
}

pub fn bin_deserialize_mac<'de, D>(d: D) -> Result<[u8; 6], D::Error>
where
    D: Deserializer<'de>,
{
    let v = bin_deserialize(d)?;
    if v.is_empty() {
        return Ok([0u8; 6]);
    }
    v.try_into().map_err(|v: Vec<u8>| {
        de::Error::custom(format!("expected 6 bytes for MAC address, got {}", v.len()))
    })
}

pub fn bin_deserialize<'de, D>(d: D) -> Result<Vec<u8>, D::Error>
where
    D: Deserializer<'de>,
{
    struct DataVisitor;

    impl<'de> de::Visitor<'de> for DataVisitor {
        type Value = Vec<u8>;

        fn expecting(&self, formatter: &mut fmt::Formatter) -> fmt::Result {
            formatter.write_str("a byte array, sequence of u8, or null")
        }

        fn visit_bytes<E>(self, v: &[u8]) -> Result<Self::Value, E>
        where
            E: de::Error,
        {
            Ok(v.to_owned())
        }

        fn visit_byte_buf<E>(self, v: Vec<u8>) -> Result<Self::Value, E>
        where
            E: de::Error,
        {
            Ok(v)
        }

        // serde_json represents byte arrays as JSON arrays [u8, u8, ...],
        // which calls visit_seq instead of visit_bytes.
        fn visit_seq<A>(self, mut seq: A) -> Result<Self::Value, A::Error>
        where
            A: de::SeqAccess<'de>,
        {
            let mut bytes = Vec::with_capacity(seq.size_hint().unwrap_or(0));
            while let Some(b) = seq.next_element::<u8>()? {
                bytes.push(b);
            }
            Ok(bytes)
        }

        // Handle JSON null → empty Vec (Apple Silicon Macs lack _enc fields)
        fn visit_unit<E>(self) -> Result<Self::Value, E>
        where
            E: de::Error,
        {
            Ok(Vec::new())
        }

        fn visit_none<E>(self) -> Result<Self::Value, E>
        where
            E: de::Error,
        {
            Ok(Vec::new())
        }
    }

    d.deserialize_any(DataVisitor)
}

// ============================================================================
// HardwareConfig
// ============================================================================

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct HardwareConfig {
    pub product_name: String,
    #[serde(serialize_with = "bin_serialize", deserialize_with = "bin_deserialize_mac")]
    pub io_mac_address: [u8; 6],
    pub platform_serial_number: String,
    pub platform_uuid: String,
    pub root_disk_uuid: String,
    pub board_id: String,
    pub os_build_num: String,
    #[serde(serialize_with = "bin_serialize", deserialize_with = "bin_deserialize")]
    pub platform_serial_number_enc: Vec<u8>, // Gq3489ugfi
    #[serde(serialize_with = "bin_serialize", deserialize_with = "bin_deserialize")]
    pub platform_uuid_enc: Vec<u8>, // Fyp98tpgj
    #[serde(serialize_with = "bin_serialize", deserialize_with = "bin_deserialize")]
    pub root_disk_uuid_enc: Vec<u8>, // kbjfrfpoJU
    #[serde(serialize_with = "bin_serialize", deserialize_with = "bin_deserialize")]
    pub rom: Vec<u8>,
    #[serde(serialize_with = "bin_serialize", deserialize_with = "bin_deserialize")]
    pub rom_enc: Vec<u8>, // oycqAZloTNDm
    pub mlb: String,
    #[serde(serialize_with = "bin_serialize", deserialize_with = "bin_deserialize")]
    pub mlb_enc: Vec<u8>, // abKPld1EcMni
}

impl HardwareConfig {
    pub fn from_validation_data(_data: &[u8]) -> Result<HardwareConfig, AbsintheError> {
        Err(AbsintheError::Other(
            "from_validation_data is not supported in emulation mode".into(),
        ))
    }
}

// ============================================================================
// Constants
// ============================================================================

const BINARY_URL: &str =
    "https://github.com/JJTech0130/nacserver/raw/main/IMDAppleServices";
const BINARY_SHA1: &str = "e1181ccad82e6629d52c6a006645ad87ee59bd13";

const HOOK_BASE: u64 = 0xD0_0000;
const HOOK_SIZE: u64 = 0x1000;
const STACK_BASE: u64 = 0x30_0000;
const STACK_SIZE: u64 = 0x10_0000;
const HEAP_BASE: u64 = 0x40_0000;
const HEAP_SIZE: u64 = 0x10_0000;
const STOP_ADDR: u64 = 0x90_0000;

const NAC_INIT: u64 = 0xB_1DB0;
const NAC_KEY_EST: u64 = 0xB_1DD0;
const NAC_SIGN: u64 = 0xB_1DF0;

/// x86-64 SysV integer argument registers, in order.
const ARG_REGS: [RegisterX86; 6] = [
    RegisterX86::RDI,
    RegisterX86::RSI,
    RegisterX86::RDX,
    RegisterX86::RCX,
    RegisterX86::R8,
    RegisterX86::R9,
];

// ============================================================================
// CF Object types for the emulated CoreFoundation
// ============================================================================

#[derive(Clone, Debug)]
enum CfObj {
    Data(Vec<u8>),
    Str(String),
    Dict(HashMap<String, usize>), // values are 1-based cf_objects indices
}

// ============================================================================
// Emulator shared state
// ============================================================================

struct EmuState {
    cf_objects: Vec<CfObj>,
    heap_use: u64,
    hook_map: HashMap<u64, String>, // hook addr → symbol name
    hw_iokit: HashMap<String, CfObj>,
    root_disk_uuid: String,
    eth_iterator_hack: bool,
}

impl EmuState {
    fn new(hw: &HardwareConfig) -> Self {
        let mut iokit = HashMap::new();

        // Data (raw bytes) properties
        iokit.insert(
            "4D1EDE05-38C7-4A6A-9CC6-4BCCA8B38C14:MLB".into(),
            CfObj::Data(hw.mlb.as_bytes().to_vec()),
        );
        iokit.insert(
            "4D1EDE05-38C7-4A6A-9CC6-4BCCA8B38C14:ROM".into(),
            CfObj::Data(hw.rom.clone()),
        );
        // Encrypted/obfuscated IOKit properties — present on Intel Macs.
        // On Apple Silicon these are empty; skip them so the binary gets NULL
        // (same as a real Apple Silicon Mac's IOKit registry).
        if !hw.platform_uuid_enc.is_empty() {
            iokit.insert("Fyp98tpgj".into(), CfObj::Data(hw.platform_uuid_enc.clone()));
        }
        if !hw.platform_serial_number_enc.is_empty() {
            iokit.insert(
                "Gq3489ugfi".into(),
                CfObj::Data(hw.platform_serial_number_enc.clone()),
            );
        }
        iokit.insert("IOMACAddress".into(), CfObj::Data(hw.io_mac_address.to_vec()));
        if !hw.mlb_enc.is_empty() {
            iokit.insert("abKPld1EcMni".into(), CfObj::Data(hw.mlb_enc.clone()));
        }
        if !hw.root_disk_uuid_enc.is_empty() {
            iokit.insert("kbjfrfpoJU".into(), CfObj::Data(hw.root_disk_uuid_enc.clone()));
        }
        if !hw.rom_enc.is_empty() {
            iokit.insert("oycqAZloTNDm".into(), CfObj::Data(hw.rom_enc.clone()));
        }

        // String properties
        iokit.insert(
            "IOPlatformSerialNumber".into(),
            CfObj::Str(hw.platform_serial_number.clone()),
        );
        iokit.insert("IOPlatformUUID".into(), CfObj::Str(hw.platform_uuid.clone()));

        // Null-terminated C-string data
        let mut pn = hw.product_name.as_bytes().to_vec();
        pn.push(0);
        iokit.insert("product-name".into(), CfObj::Data(pn));
        let mut bi = hw.board_id.as_bytes().to_vec();
        bi.push(0);
        iokit.insert("board-id".into(), CfObj::Data(bi));

        Self {
            cf_objects: Vec::new(),
            heap_use: 0,
            hook_map: HashMap::new(),
            hw_iokit: iokit,
            root_disk_uuid: hw.root_disk_uuid.clone(),
            eth_iterator_hack: false,
        }
    }

    fn heap_alloc(&mut self, size: u64) -> u64 {
        // Align to 16 bytes
        let aligned = (size + 15) & !15;
        let addr = HEAP_BASE + self.heap_use;
        self.heap_use += aligned;
        if self.heap_use > HEAP_SIZE {
            panic!("NAC emulator heap overflow ({} > {})", self.heap_use, HEAP_SIZE);
        }
        addr
    }

    /// Add a CF object, return its 1-based "pointer".
    fn cf_add(&mut self, obj: CfObj) -> u64 {
        self.cf_objects.push(obj);
        self.cf_objects.len() as u64
    }

    fn cf_get(&self, id: u64) -> Option<&CfObj> {
        if id == 0 || (id as usize) > self.cf_objects.len() {
            None
        } else {
            Some(&self.cf_objects[(id as usize) - 1])
        }
    }
}

// ============================================================================
// Binary management
// ============================================================================

/// Get the NAC binary bytes, downloading and caching if necessary.
fn get_binary() -> Result<Vec<u8>, AbsintheError> {
    let cache_path = "state/IMDAppleServices";

    // Try reading from cache first
    if let Ok(data) = fs::read(cache_path) {
        let hash = hex::encode(Sha1::digest(&data));
        if hash == BINARY_SHA1 {
            debug!("Using cached IMDAppleServices binary");
            return Ok(data);
        }
        warn!("Cached binary hash mismatch ({}), re-downloading", hash);
    }

    info!("Downloading IMDAppleServices binary from {}", BINARY_URL);
    let resp = ureq::get(BINARY_URL)
        .call()
        .map_err(|e| AbsintheError::Other(format!("Failed to download NAC binary: {}", e)))?;

    let mut data = Vec::new();
    resp.into_reader()
        .read_to_end(&mut data)
        .map_err(|e| AbsintheError::Other(format!("Failed to read NAC binary: {}", e)))?;

    let hash = hex::encode(Sha1::digest(&data));
    if hash != BINARY_SHA1 {
        return Err(AbsintheError::Other(format!(
            "NAC binary SHA1 mismatch: expected {}, got {}",
            BINARY_SHA1, hash
        )));
    }

    let _ = fs::create_dir_all("state");
    if let Err(e) = fs::write(cache_path, &data) {
        warn!("Failed to cache NAC binary: {}", e);
    }

    info!("Downloaded and verified IMDAppleServices binary ({} bytes)", data.len());
    Ok(data)
}

/// Extract the x86_64 slice from a (possibly fat) Mach-O binary.
fn extract_x86_64_slice(data: &[u8]) -> Result<Vec<u8>, AbsintheError> {
    match Mach::parse(data).map_err(|e| AbsintheError::Other(format!("Mach-O parse error: {}", e)))? {
        Mach::Fat(fat) => {
            for arch in fat.iter_arches() {
                let arch = arch.map_err(|e| {
                    AbsintheError::Other(format!("Fat arch parse error: {}", e))
                })?;
                if arch.cputype == CPU_TYPE_X86_64 {
                    let start = arch.offset as usize;
                    let end = start + arch.size as usize;
                    if end > data.len() {
                        return Err(AbsintheError::Other(
                            "x86_64 slice extends beyond file".into(),
                        ));
                    }
                    return Ok(data[start..end].to_vec());
                }
            }
            Err(AbsintheError::Other("No x86_64 slice in fat binary".into()))
        }
        Mach::Binary(_) => {
            // Already a single-arch binary, use as-is.
            Ok(data.to_vec())
        }
    }
}

// ============================================================================
// Hex encoding helper (avoid adding another dep)
// ============================================================================
mod hex {
    pub fn encode(data: impl AsRef<[u8]>) -> String {
        data.as_ref().iter().map(|b| format!("{:02x}", b)).collect()
    }
}

// ============================================================================
// Unicorn helpers
// ============================================================================

fn uc_read_u64(uc: &Unicorn<()>, addr: u64) -> u64 {
    let buf = uc.mem_read_as_vec(addr, 8).unwrap_or_default();
    u64::from_le_bytes(buf.try_into().unwrap_or([0u8; 8]))
}

fn uc_write_u64(uc: &mut Unicorn<()>, addr: u64, val: u64) {
    uc.mem_write(addr, &val.to_le_bytes()).unwrap();
}

fn uc_read_cstr(uc: &Unicorn<()>, addr: u64, max: usize) -> String {
    let buf = uc.mem_read_as_vec(addr, max).unwrap_or_default();
    let end = buf.iter().position(|&b| b == 0).unwrap_or(buf.len());
    String::from_utf8_lossy(&buf[..end]).into_owned()
}

/// Parse a __builtin_CFString pointer: { isa, flags, str_ptr, length }
fn uc_read_cfstr(uc: &Unicorn<()>, ptr: u64) -> String {
    let buf = uc.mem_read_as_vec(ptr, 32).unwrap_or_default();
    if buf.len() < 32 {
        return String::new();
    }
    // isa = bytes 0..8 (ignored)
    // flags = bytes 8..16 (ignored)
    let str_ptr = u64::from_le_bytes(buf[16..24].try_into().unwrap());
    let length = u64::from_le_bytes(buf[24..32].try_into().unwrap());
    let str_data = uc.mem_read_as_vec(str_ptr, length as usize).unwrap_or_default();
    String::from_utf8_lossy(&str_data).into_owned()
}

fn arg(uc: &Unicorn<()>, n: usize) -> u64 {
    uc.reg_read(ARG_REGS[n]).unwrap_or(0)
}

fn set_ret(uc: &mut Unicorn<()>, val: u64) {
    uc.reg_write(RegisterX86::RAX, val).unwrap();
}

// ============================================================================
// Hook dispatch
// ============================================================================

/// Master hook dispatcher — called by the Unicorn code hook for every
/// instruction in the HOOK_BASE region.
fn dispatch(uc: &mut Unicorn<()>, state: &mut EmuState, addr: u64) {
    let name = match state.hook_map.get(&addr) {
        Some(n) => n.clone(),
        None => return,
    };

    let ret: Option<u64> = match name.as_str() {
        // Memory / system
        "_malloc" => {
            let size = arg(uc, 0);
            Some(state.heap_alloc(size.max(1)))
        }
        "_free" => Some(0),
        "___stack_chk_guard" => Some(0),
        "___memset_chk" => {
            let dest = arg(uc, 0);
            let c = arg(uc, 1) as u8;
            let len = arg(uc, 2) as usize;
            let buf = vec![c; len];
            uc.mem_write(dest, &buf).unwrap();
            Some(0)
        }
        "_memcpy" => {
            let dest = arg(uc, 0);
            let src = arg(uc, 1);
            let len = arg(uc, 2) as usize;
            let buf = uc.mem_read_as_vec(src, len).unwrap_or_default();
            uc.mem_write(dest, &buf).unwrap();
            Some(0)
        }
        "___bzero" => {
            let ptr = arg(uc, 0);
            let len = arg(uc, 1) as usize;
            let buf = vec![0u8; len];
            uc.mem_write(ptr, &buf).unwrap();
            Some(0)
        }
        "_sysctlbyname" => Some(0),
        "_statfs$INODE64" => Some(0),
        "_arc4random" => Some(rand::random::<u32>() as u64),

        // IOKit
        "_kIOMasterPortDefault" => Some(0), // data symbol, value doesn't matter
        "_IORegistryEntryFromPath" => Some(1),
        "_IORegistryEntryCreateCFProperty" => {
            // (entry, key_cfstr, allocator, options) -> cf_id
            let key_ptr = arg(uc, 1);
            let key_name = uc_read_cfstr(uc, key_ptr);
            debug!("IORegistryEntryCreateCFProperty: key={}", key_name);
            if let Some(obj) = state.hw_iokit.get(&key_name).cloned() {
                Some(state.cf_add(obj))
            } else {
                debug!("  -> key not found, returning NULL");
                Some(0)
            }
        }
        "_IOServiceMatching" => {
            let name_ptr = arg(uc, 0);
            let name = uc_read_cstr(uc, name_ptr, 256);
            debug!("IOServiceMatching: {}", name);
            let str_id = state.cf_add(CfObj::Str(name));
            let dict_id = {
                let mut d = HashMap::new();
                d.insert("IOProviderClass".into(), str_id as usize);
                state.cf_add(CfObj::Dict(d))
            };
            Some(dict_id)
        }
        "_IOServiceGetMatchingService" => Some(92),
        "_IOServiceGetMatchingServices" => {
            state.eth_iterator_hack = true;
            // Write iterator handle (93) to the output pointer (arg 2)
            let existing_ptr = arg(uc, 2);
            uc.mem_write(existing_ptr, &[93u8]).unwrap();
            Some(0)
        }
        "_IOIteratorNext" => {
            if state.eth_iterator_hack {
                state.eth_iterator_hack = false;
                Some(94)
            } else {
                Some(0)
            }
        }
        "_IORegistryEntryGetParentEntry" => {
            let entry = arg(uc, 0) as u8;
            let parent_ptr = arg(uc, 2);
            uc.mem_write(parent_ptr, &[entry.wrapping_add(100)]).unwrap();
            Some(0)
        }
        "_IOObjectRelease" => Some(0),

        // DiskArbitration
        "_DASessionCreate" => Some(201),
        "_DADiskCreateFromBSDName" => Some(202),
        "_DADiskCopyDescription" => {
            // Create a dictionary containing the volume UUID
            let mut d = HashMap::new();
            let uuid_id = state.cf_add(CfObj::Str(state.root_disk_uuid.clone()));
            d.insert("DADiskDescriptionVolumeUUIDKey".into(), uuid_id as usize);
            Some(state.cf_add(CfObj::Dict(d)))
        }
        "_kDADiskDescriptionVolumeUUIDKey" => Some(0), // data symbol

        // CoreFoundation allocator / boolean
        "_kCFAllocatorDefault" => Some(0),
        "_kCFBooleanTrue" => Some(0),

        // CoreFoundation type IDs
        "_CFGetTypeID" => {
            let obj_id = arg(uc, 0);
            match state.cf_get(obj_id) {
                Some(CfObj::Data(_)) => Some(1),
                Some(CfObj::Str(_)) => Some(2),
                _ => Some(0),
            }
        }
        "_CFStringGetTypeID" => Some(2),
        "_CFDataGetTypeID" => Some(1),

        // CFData
        "_CFDataGetLength" => {
            let obj_id = arg(uc, 0);
            match state.cf_get(obj_id) {
                Some(CfObj::Data(d)) => Some(d.len() as u64),
                _ => Some(0),
            }
        }
        "_CFDataGetBytes" => {
            let obj_id = arg(uc, 0);
            let range_start = arg(uc, 1) as usize;
            let range_end = arg(uc, 2) as usize;
            let buf_ptr = arg(uc, 3);
            if let Some(CfObj::Data(d)) = state.cf_get(obj_id).cloned() {
                let slice = &d[range_start..range_end.min(d.len())];
                uc.mem_write(buf_ptr, slice).unwrap();
                Some(slice.len() as u64)
            } else {
                Some(0)
            }
        }

        // CFString
        "_CFStringGetLength" => {
            let obj_id = arg(uc, 0);
            match state.cf_get(obj_id) {
                Some(CfObj::Str(s)) => Some(s.len() as u64),
                _ => Some(0),
            }
        }
        "_CFStringGetMaximumSizeForEncoding" => {
            let length = arg(uc, 0);
            Some(length) // UTF-8 max = length for ASCII
        }
        "_CFStringGetCString" => {
            let obj_id = arg(uc, 0);
            let buf_ptr = arg(uc, 1);
            if let Some(CfObj::Str(s)) = state.cf_get(obj_id).cloned() {
                uc.mem_write(buf_ptr, s.as_bytes()).unwrap();
                Some(s.len() as u64)
            } else {
                Some(0)
            }
        }
        "_CFUUIDCreateString" => {
            // (allocator, uuid) -> uuid  (pass-through)
            Some(arg(uc, 1))
        }

        // CFDictionary
        "_CFDictionaryCreateMutable" => {
            Some(state.cf_add(CfObj::Dict(HashMap::new())))
        }
        "_CFDictionarySetValue" => {
            let dict_id = arg(uc, 0);
            let key_raw = arg(uc, 1);
            let val_raw = arg(uc, 2);
            // Resolve key to string
            let key_str = resolve_cf_or_raw(state, key_raw);
            let val_idx = val_raw as usize;
            if dict_id > 0 && (dict_id as usize) <= state.cf_objects.len() {
                if let CfObj::Dict(ref mut d) = state.cf_objects[(dict_id as usize) - 1] {
                    d.insert(key_str, val_idx);
                }
            }
            None // void return
        }
        "_CFDictionaryGetValue" => {
            let dict_id = arg(uc, 0);
            let key_raw = arg(uc, 1);
            // The _kDADiskDescriptionVolumeUUIDKey data symbol resolves to a
            // hook address filled with 0xC3 bytes, so reading 8 bytes yields
            // 0xC3C3C3C3C3C3C3C3.  Recognise that sentinel.
            let key_str = if key_raw == 0xC3C3_C3C3_C3C3_C3C3 {
                "DADiskDescriptionVolumeUUIDKey".to_string()
            } else {
                resolve_cf_or_raw(state, key_raw)
            };
            debug!("CFDictionaryGetValue dict={} key={}", dict_id, key_str);
            if let Some(CfObj::Dict(d)) = state.cf_get(dict_id).cloned() {
                if let Some(&val_idx) = d.get(&key_str) {
                    // Re-add the referenced object so we return a fresh id
                    if let Some(obj) = state.cf_objects.get(val_idx - 1).cloned() {
                        Some(state.cf_add(obj))
                    } else {
                        Some(val_idx as u64)
                    }
                } else {
                    warn!("CFDictionaryGetValue: key '{}' not found", key_str);
                    Some(0)
                }
            } else {
                Some(0)
            }
        }

        // CFRelease
        "_CFRelease" => Some(0),

        _ => {
            debug!("Unhandled hook: {}", name);
            Some(0)
        }
    };

    if let Some(v) = ret {
        set_ret(uc, v);
    }
}

/// Resolve a value that might be a CF object id or a raw pointer.
fn resolve_cf_or_raw(state: &EmuState, val: u64) -> String {
    if val > 0 && (val as usize) <= state.cf_objects.len() {
        match &state.cf_objects[(val as usize) - 1] {
            CfObj::Str(s) => return s.clone(),
            CfObj::Data(d) => {
                // Try interpreting as a C string
                let end = d.iter().position(|&b| b == 0).unwrap_or(d.len());
                return String::from_utf8_lossy(&d[..end]).into_owned();
            }
            _ => {}
        }
    }
    // Return the raw value as a string for use as a dict key
    format!("0x{:x}", val)
}

// ============================================================================
// Hook symbol list
// ============================================================================

/// All external symbols that the NAC binary might reference.
const HOOK_SYMBOLS: &[&str] = &[
    "_malloc",
    "_free",
    "___stack_chk_guard",
    "___memset_chk",
    "_memcpy",
    "___bzero",
    "_sysctlbyname",
    "_statfs$INODE64",
    "_arc4random",
    "_kIOMasterPortDefault",
    "_IORegistryEntryFromPath",
    "_IORegistryEntryCreateCFProperty",
    "_IOServiceMatching",
    "_IOServiceGetMatchingService",
    "_IOServiceGetMatchingServices",
    "_IOIteratorNext",
    "_IORegistryEntryGetParentEntry",
    "_IOObjectRelease",
    "_DASessionCreate",
    "_DADiskCreateFromBSDName",
    "_DADiskCopyDescription",
    "_kDADiskDescriptionVolumeUUIDKey",
    "_kCFAllocatorDefault",
    "_kCFBooleanTrue",
    "_CFGetTypeID",
    "_CFStringGetTypeID",
    "_CFDataGetTypeID",
    "_CFDataGetLength",
    "_CFDataGetBytes",
    "_CFStringGetLength",
    "_CFStringGetMaximumSizeForEncoding",
    "_CFStringGetCString",
    "_CFUUIDCreateString",
    "_CFDictionaryCreateMutable",
    "_CFDictionarySetValue",
    "_CFDictionaryGetValue",
    "_CFRelease",
];

// ============================================================================
// ValidationCtx
// ============================================================================

/// Public validation context used by rustpush's `MacOSConfig` to generate
/// APNs validation data. Two internal modes:
///
/// - **Emulator** (default): runs Apple's NAC binary inside a Unicorn x86-64
///   emulator, feeding it IOKit-equivalent hardware identifiers (with optional
///   `_enc` fields) and handshaking with Apple's validation servers.
///
/// - **Native** (macOS + `native-nac` feature): delegates each NAC step to
///   Apple's private `AAAbsintheContext` class via our `nac-validation`
///   crate's three-step API (`NacContext::init` → `key_establishment` →
///   `sign`). `new()` calls `NACInit(cert)` and returns the real request
///   bytes that upstream `MacOSConfig::generate_validation_data` then POSTs
///   to `id-initialize-validation`; `key_establishment()` feeds Apple's
///   response back into the same context; `sign()` produces the final
///   validation bytes. Upstream rustpush remains entirely unmodified — the
///   exact same HTTP flow runs, it just happens to be driven by
///   `AAAbsintheContext` instead of the unicorn emulator. `_enc` hardware
///   fields are irrelevant on this path because AAAbsintheContext reads the
///   real hardware identifiers from IOKit itself.
pub struct ValidationCtx {
    inner: ValidationCtxInner,
}

enum ValidationCtxInner {
    Emulator {
        uc: UnsafeCell<Unicorn<'static, ()>>,
        state: Rc<RefCell<EmuState>>,
        validation_ctx_addr: u64,
    },
    #[cfg(all(target_os = "macos", feature = "native-nac"))]
    Native {
        // UnsafeCell so `sign(&self)` can call NacContext's `&mut self`
        // methods without violating aliasing rules. ValidationCtx is only
        // used from a single task — the `unsafe impl Send` below mirrors
        // that invariant.
        ctx: UnsafeCell<nac_validation::NacContext>,
    },
    /// Relay mode: single-shot POST to a `tools/nac-relay` server running
    /// on a real Mac. The relay does the full NACInit → Apple POST →
    /// NACKeyEstablishment → NACSign dance internally and returns the
    /// final validation data. Used when the hardware-key JSON carries a
    /// `nac_relay_url` (Apple Silicon Macs whose keys can't run in the
    /// unicorn x86-64 emulator).
    Relay {
        /// Complete validation data returned by the relay's single-shot
        /// `/validation-data` endpoint.
        validation_data: Vec<u8>,
    },
}

unsafe impl Send for ValidationCtx {}

impl ValidationCtx {
    /// Initialise NAC validation.
    ///
    /// * `cert_chain`        — certificate bytes from Apple's validation cert endpoint
    /// * `out_request_bytes` — filled with the session-info-request to send to Apple
    /// * `hw_config`         — hardware identifiers (extracted from a real Mac)
    pub fn new(
        cert_chain: &[u8],
        out_request_bytes: &mut Vec<u8>,
        hw_config: &HardwareConfig,
    ) -> Result<ValidationCtx, AbsintheError> {
        // ====================================================================
        // Relay path — Apple Silicon hardware keys routed through a Mac-hosted
        // `tools/nac-relay` server. Single POST to `/validation-data` returns
        // the complete validation data (the relay does the full NACInit →
        // Apple POST → NACKeyEstablishment → NACSign dance internally).
        // We store the result and return it from sign(); key_establishment
        // is a no-op. out_request_bytes is left empty since the relay
        // already completed the Apple handshake server-side.
        // ====================================================================
        if let Some(cfg) = get_relay_config() {
            info!("NAC relay: single-shot POST to {}", cfg.url);
            let agent = relay_agent(&cfg)?;
            let mut req = agent.post(&cfg.url);
            if let Some(ref tok) = cfg.token {
                req = req.set("Authorization", &format!("Bearer {tok}"));
            }
            match req.call() {
                Ok(resp) => {
                    use base64::Engine;
                    let body = resp.into_string()
                        .map_err(|e| AbsintheError::Other(format!("relay read error: {e}")))?;
                    let validation_data = base64::engine::general_purpose::STANDARD
                        .decode(body.trim())
                        .map_err(|e| AbsintheError::Other(format!("relay base64 decode: {e}")))?;
                    info!("NAC relay: got {} bytes of validation data", validation_data.len());
                    // Stash the relay data for sign() and pre-fetch.
                    // out_request_bytes left empty — the bridge
                    // pre-fetches and uses RelayOSConfig to return
                    // relay data directly from generate_validation_data(),
                    // bypassing the Apple handshake entirely (same as master).
                    return Ok(ValidationCtx {
                        inner: ValidationCtxInner::Relay { validation_data },
                    });
                }
                Err(e) => {
                    return Err(AbsintheError::Other(format!(
                        "NAC relay failed ({}): {e}. \
                         Ensure the nac-relay server is running on the Mac that provided \
                         this hardware key.",
                        cfg.url
                    )));
                }
            }
        }

        // ====================================================================
        // Native NAC fast path — macOS only, requires `native-nac` feature.
        // Delegates to AAAbsintheContext via the `nac-validation` crate. The
        // unicorn emulator is entirely skipped; hardware `_enc` fields are
        // irrelevant because Apple's native framework reads real hardware
        // identifiers from IOKit itself.
        //
        // True 3-step path: NacContext::init produces NACInit request bytes
        // for upstream MacOSConfig's POST to `id-initialize-validation`,
        // key_establishment() consumes Apple's response on the SAME context,
        // and sign() returns the validation data correlated with that
        // exchange. Apple's signin/v2/login correlates the validation data
        // with the session_info we just exchanged — using a one-shot result
        // (which runs its own internal exchange) breaks that correlation
        // and triggers `MobileMeError`.
        // ====================================================================
        #[cfg(all(target_os = "macos", feature = "native-nac"))]
        {
            match nac_validation::NacContext::init(cert_chain) {
                Ok((ctx, request_bytes)) => {
                    info!(
                        "NAC native path: NacContext::init produced {} request bytes",
                        request_bytes.len()
                    );
                    *out_request_bytes = request_bytes;
                    return Ok(ValidationCtx {
                        inner: ValidationCtxInner::Native {
                            ctx: UnsafeCell::new(ctx),
                        },
                    });
                }
                Err(e) => {
                    warn!(
                        "NAC native path: NacContext::init failed, falling back to emulator: {}",
                        e
                    );
                    // Fall through to emulator path below.
                }
            }
        }

        // Log hardware key diagnostics
        info!("NAC init: product={} serial={} board={} build={}",
            hw_config.product_name, hw_config.platform_serial_number,
            hw_config.board_id, hw_config.os_build_num);
        info!("NAC init: uuid={} rom={} bytes mlb={} mac={} bytes",
            hw_config.platform_uuid, hw_config.rom.len(),
            hw_config.mlb, hw_config.io_mac_address.len());
        info!("NAC init: _enc fields: serial_enc={} uuid_enc={} disk_enc={} rom_enc={} mlb_enc={}",
            hw_config.platform_serial_number_enc.len(),
            hw_config.platform_uuid_enc.len(),
            hw_config.root_disk_uuid_enc.len(),
            hw_config.rom_enc.len(),
            hw_config.mlb_enc.len());

        // 0. Compute missing _enc fields if we have the XNU encrypt function
        //    (needed for keys extracted from macOS High Sierra 10.13)
        #[allow(unused_mut)]
        let hw_config = {
            let mut hw = hw_config.clone();
            #[cfg(has_xnu_encrypt)]
            compute_missing_enc_fields(&mut hw)?;
            hw
        };

        // 1. Load the NAC binary
        let raw = get_binary()?;
        let binary = extract_x86_64_slice(&raw)?;

        // 2. Create emulator state
        let state = Rc::new(RefCell::new(EmuState::new(&hw_config)));

        // 3. Create Unicorn x86-64 engine
        let mut uc = Unicorn::new(Arch::X86, Mode::MODE_64)
            .map_err(|e| AbsintheError::Other(format!("Unicorn init failed: {:?}", e)))?;

        // 4. Map binary at address 0
        let bin_pages = round_up(binary.len(), 0x1000) as u64;
        uc.mem_map(0, bin_pages, Prot::ALL)
            .map_err(|e| AbsintheError::Other(format!("Failed to map binary: {:?}", e)))?;
        uc.mem_write(0, &binary)
            .map_err(|e| AbsintheError::Other(format!("Failed to write binary: {:?}", e)))?;

        // 5. Map hook space (filled with RET instructions)
        uc.mem_map(HOOK_BASE, HOOK_SIZE, Prot::ALL)
            .map_err(|e| AbsintheError::Other(format!("Failed to map hooks: {:?}", e)))?;
        uc.mem_write(HOOK_BASE, &vec![0xC3u8; HOOK_SIZE as usize])
            .map_err(|e| AbsintheError::Other(format!("Failed to write hooks: {:?}", e)))?;

        // 6. Map stack
        uc.mem_map(STACK_BASE, STACK_SIZE, Prot::ALL)
            .map_err(|e| AbsintheError::Other(format!("Failed to map stack: {:?}", e)))?;
        uc.reg_write(RegisterX86::RSP, STACK_BASE + STACK_SIZE)
            .map_err(|e| AbsintheError::Other(format!("Failed to set RSP: {:?}", e)))?;
        uc.reg_write(RegisterX86::RBP, STACK_BASE + STACK_SIZE)
            .map_err(|e| AbsintheError::Other(format!("Failed to set RBP: {:?}", e)))?;

        // 7. Map heap
        uc.mem_map(HEAP_BASE, HEAP_SIZE, Prot::ALL)
            .map_err(|e| AbsintheError::Other(format!("Failed to map heap: {:?}", e)))?;

        // 8. Map stop page
        uc.mem_map(STOP_ADDR, 0x1000, Prot::ALL)
            .map_err(|e| AbsintheError::Other(format!("Failed to map stop page: {:?}", e)))?;
        uc.mem_write(STOP_ADDR, &vec![0xC3u8; 0x1000])
            .map_err(|e| AbsintheError::Other(format!("Failed to write stop page: {:?}", e)))?;

        // 9. Assign hook addresses and build lookup table
        {
            let mut st = state.borrow_mut();
            for (i, &sym) in HOOK_SYMBOLS.iter().enumerate() {
                let addr = HOOK_BASE + i as u64;
                st.hook_map.insert(addr, sym.to_string());
            }
        }

        // 10. Resolve Mach-O imports → write hook addresses into GOT
        resolve_imports(&mut uc, &binary, &state.borrow())?;

        // 11. Add code hook for the hook space
        let hook_state = state.clone();
        uc.add_code_hook(HOOK_BASE, HOOK_BASE + HOOK_SIZE - 1, move |uc, addr, _size| {
            let mut st = hook_state.borrow_mut();
            dispatch(uc, &mut st, addr);
        })
        .map_err(|e| AbsintheError::Other(format!("Failed to add code hook: {:?}", e)))?;

        // 12. Call nac_init
        let cert_addr;
        let out_ctx_ptr;
        let out_req_ptr;
        let out_req_len_ptr;
        {
            let mut st = state.borrow_mut();
            cert_addr = st.heap_alloc(cert_chain.len() as u64);
            out_ctx_ptr = st.heap_alloc(8);
            out_req_ptr = st.heap_alloc(8);
            out_req_len_ptr = st.heap_alloc(8);
        }
        uc.mem_write(cert_addr, cert_chain)
            .map_err(|e| AbsintheError::Other(format!("Failed to write cert: {:?}", e)))?;

        let ret = call_func(
            &mut uc,
            NAC_INIT,
            &[
                cert_addr,
                cert_chain.len() as u64,
                out_ctx_ptr,
                out_req_ptr,
                out_req_len_ptr,
            ],
        )?;

        if ret != 0 {
            return Err(AbsintheError::NacError(-(ret as i64 as i32)));
        }

        // Read outputs
        let validation_ctx_addr = uc_read_u64(&uc, out_ctx_ptr);
        let req_bytes_addr = uc_read_u64(&uc, out_req_ptr);
        let req_len = uc_read_u64(&uc, out_req_len_ptr) as usize;

        debug!(
            "nac_init: ctx=0x{:x} req_addr=0x{:x} req_len={}",
            validation_ctx_addr, req_bytes_addr, req_len
        );

        let request_data = uc
            .mem_read_as_vec(req_bytes_addr, req_len)
            .map_err(|e| AbsintheError::Other(format!("Failed to read request: {:?}", e)))?;
        *out_request_bytes = request_data;

        Ok(ValidationCtx {
            inner: ValidationCtxInner::Emulator {
                uc: UnsafeCell::new(uc),
                state,
                validation_ctx_addr,
            },
        })
    }

    /// Process the session-info response from Apple.
    pub fn key_establishment(&mut self, response: &[u8]) -> Result<(), AbsintheError> {
        match &mut self.inner {
            ValidationCtxInner::Relay { .. } => {
                // No-op — the relay already completed the full handshake
                // in the single-shot /validation-data call during new().
                Ok(())
            }
            #[cfg(all(target_os = "macos", feature = "native-nac"))]
            ValidationCtxInner::Native { ctx, .. } => {
                debug!(
                    "NAC key_establishment: native path, {} bytes -> AAAbsintheContext",
                    response.len()
                );
                // Drive key_establishment on ctx so it reaches a valid state
                // (needed in case the 3-step NACSign path is used as fallback).
                // Safety: single-task use; see ValidationCtx struct doc.
                let ctx = unsafe { &mut *ctx.get() };
                ctx.key_establishment(response)
                    .map_err(|e| AbsintheError::Other(format!("NACKeyEstablishment: {e}")))?;
                Ok(())
            }
            ValidationCtxInner::Emulator { uc, state, validation_ctx_addr } => {
                let resp_addr = {
                    let mut st = state.borrow_mut();
                    st.heap_alloc(response.len() as u64)
                };
                let uc = uc.get_mut();
                uc.mem_write(resp_addr, response)
                    .map_err(|e| AbsintheError::Other(format!("Failed to write response: {:?}", e)))?;

                let ret = call_func(
                    uc,
                    NAC_KEY_EST,
                    &[*validation_ctx_addr, resp_addr, response.len() as u64],
                )?;

                if ret != 0 {
                    return Err(AbsintheError::NacError(-(ret as i64 as i32)));
                }
                Ok(())
            }
        }
    }

    /// Generate signed validation data.
    pub fn sign(&self) -> Result<Vec<u8>, AbsintheError> {
        // Check for pre-fetched relay data first — if the bridge stashed
        // validation data from the NAC relay, it's authoritative.
        if let Some(data) = take_prefetched_validation_data() {
            info!("NAC sign: returning pre-fetched relay validation data ({} bytes)", data.len());
            return Ok(data);
        }

        let (uc, state, validation_ctx_addr) = match &self.inner {
            ValidationCtxInner::Relay { validation_data } => {
                info!("NAC sign: returning relay validation data ({} bytes)", validation_data.len());
                return Ok(validation_data.clone());
            }
            #[cfg(all(target_os = "macos", feature = "native-nac"))]
            ValidationCtxInner::Native { ctx } => {
                // 3-step NACSign on the context whose key_establishment was
                // driven by the upstream HTTP flow — produces validation
                // data correlated with this connection's session_info.
                // Safety: single-task use; see ValidationCtx struct doc.
                let ctx = unsafe { &mut *ctx.get() };
                let bytes = ctx
                    .sign()
                    .map_err(|e| AbsintheError::Other(format!("NACSign: {e}")))?;
                debug!("NAC sign: native 3-step path produced {} bytes", bytes.len());
                return Ok(bytes);
            }
            ValidationCtxInner::Emulator { uc, state, validation_ctx_addr } => {
                (uc, state, *validation_ctx_addr)
            }
        };
        // sign() takes &self but we need &mut for the emulator.
        // Safety: ValidationCtx is only used from one thread (enforced by
        // the single-threaded usage pattern and unsafe Send impl).
        let uc = unsafe { &mut *uc.get() };

        let out_data_ptr;
        let out_data_len_ptr;
        {
            let mut st = state.borrow_mut();
            out_data_ptr = st.heap_alloc(8);
            out_data_len_ptr = st.heap_alloc(8);
        }

        let ret = call_func(
            uc,
            NAC_SIGN,
            &[
                validation_ctx_addr,
                0,
                0,
                out_data_ptr,
                out_data_len_ptr,
            ],
        )?;

        if ret != 0 {
            return Err(AbsintheError::NacError(-(ret as i64 as i32)));
        }

        let data_addr = uc_read_u64(uc, out_data_ptr);
        let data_len = uc_read_u64(uc, out_data_len_ptr) as usize;

        debug!("nac_sign: data_addr=0x{:x} len={}", data_addr, data_len);

        let validation_data = uc
            .mem_read_as_vec(data_addr, data_len)
            .map_err(|e| AbsintheError::Other(format!("Failed to read validation data: {:?}", e)))?;

        Ok(validation_data)
    }
}

// ============================================================================
// Function calling helper
// ============================================================================

/// Call a function at `addr` with the given arguments, using the x86-64 SysV
/// calling convention. Returns the value in RAX.
fn call_func(uc: &mut Unicorn<'static, ()>, addr: u64, args: &[u64]) -> Result<u64, AbsintheError> {
    // Push return address (STOP_ADDR)
    let mut rsp = uc.reg_read(RegisterX86::RSP).unwrap();
    rsp -= 8;
    uc.reg_write(RegisterX86::RSP, rsp).unwrap();
    uc_write_u64(uc, rsp, STOP_ADDR);

    // Set argument registers
    for (i, &val) in args.iter().enumerate() {
        if i < 6 {
            uc.reg_write(ARG_REGS[i], val).unwrap();
        } else {
            // Push remaining args on stack (right to left order already handled)
            rsp -= 8;
            uc.reg_write(RegisterX86::RSP, rsp).unwrap();
            uc_write_u64(uc, rsp, val);
        }
    }

    // Run emulation
    uc.emu_start(addr, STOP_ADDR, 0, 0)
        .map_err(|e| AbsintheError::Other(format!("Emulation error at 0x{:x}: {:?}", addr, e)))?;

    Ok(uc.reg_read(RegisterX86::RAX).unwrap())
}

// ============================================================================
// Mach-O import resolution
// ============================================================================

fn resolve_imports(
    uc: &mut Unicorn<'static, ()>,
    binary: &[u8],
    state: &EmuState,
) -> Result<(), AbsintheError> {
    let macho = match goblin::mach::MachO::parse(binary, 0) {
        Ok(m) => m,
        Err(e) => return Err(AbsintheError::Other(format!("Mach-O parse error: {}", e))),
    };

    // Build symbol → hook address lookup
    let mut sym_to_hook: HashMap<&str, u64> = HashMap::new();
    for (&addr, name) in &state.hook_map {
        // hook_map has full names like "_malloc", import names also have "_malloc"
        sym_to_hook.insert(name.as_str(), addr);
    }

    // Process all imports (both lazy and non-lazy binds)
    match macho.imports() {
        Ok(imports) => {
            for import in &imports {
                if let Some(&hook_addr) = sym_to_hook.get(import.name) {
                    // import.offset is the file offset where the GOT pointer lives
                    uc.mem_write(import.offset as u64, &hook_addr.to_le_bytes())
                        .map_err(|e| {
                            AbsintheError::Other(format!(
                                "Failed to write import {} at 0x{:x}: {:?}",
                                import.name, import.offset, e
                            ))
                        })?;
                    debug!("Bound {} at offset 0x{:x} → hook 0x{:x}", import.name, import.offset, hook_addr);
                }
            }
        }
        Err(e) => {
            warn!("Failed to parse Mach-O imports, trying section-based resolution: {}", e);
        }
    }

    Ok(())
}

// ============================================================================
// Utility
// ============================================================================

fn round_up(val: usize, align: usize) -> usize {
    (val + align - 1) & !(align - 1)
}

// Deterministic `_enc` derivation tests for the x86 Linux assembly path.
//
// Why these exist:
// - Some Intel keys are extracted without encrypted IOKit properties.
// - In that case we derive `_enc` fields locally via the XNU routine from
//   `src/asm/encrypt.s`.
// - These tests protect that behavior with known-good vectors and ensure
//   repeated computation is idempotent.
#[cfg(all(test, has_xnu_encrypt))]
mod tests {
    use super::*;
    use base64::{engine::general_purpose::STANDARD, Engine};

    fn b64_dec(s: &str) -> Vec<u8> {
        STANDARD.decode(s).expect("invalid base64 test vector")
    }

    fn b64_enc(bytes: &[u8]) -> String {
        STANDARD.encode(bytes)
    }

    fn sample_hw_missing_enc() -> HardwareConfig {
        HardwareConfig {
            product_name: "MacBookAir8,1".into(),
            io_mac_address: [0xa4, 0x83, 0xe7, 0x11, 0x47, 0x1c],
            platform_serial_number: "C02YT1YMJK7M".into(),
            platform_uuid: "11D299A5-CF0B-544D-BAD3-7AC7A6E452D7".into(),
            root_disk_uuid: "FCDB63B5-D208-4AEE-B368-3DE952B911FF".into(),
            board_id: "Mac-827FAC58A8FDFA22".into(),
            os_build_num: "22G513".into(),
            platform_serial_number_enc: vec![],
            platform_uuid_enc: vec![],
            root_disk_uuid_enc: vec![],
            rom: b64_dec("V9BNndaG"),
            rom_enc: vec![],
            mlb: "C02923200KVKN3YAG".into(),
            mlb_enc: vec![],
        }
    }

    #[test]
    fn test_compute_missing_enc_fields_matches_known_vectors() {
        let mut hw = sample_hw_missing_enc();
        compute_missing_enc_fields(&mut hw).expect("compute_missing_enc_fields failed");

        println!("computed platform_serial_number_enc={}", b64_enc(&hw.platform_serial_number_enc));
        println!("computed platform_uuid_enc={}", b64_enc(&hw.platform_uuid_enc));
        println!("computed root_disk_uuid_enc={}", b64_enc(&hw.root_disk_uuid_enc));
        println!("computed rom_enc={}", b64_enc(&hw.rom_enc));
        println!("computed mlb_enc={}", b64_enc(&hw.mlb_enc));

        assert_eq!(
            hw.platform_serial_number_enc,
            b64_dec("c3kZ7+WofxcjaBTInJCwSV0="),
            "platform_serial_number_enc mismatch"
        );
        assert_eq!(
            hw.platform_uuid_enc,
            b64_dec("jGguP3mQH+Vw6dMAWrqZOnk="),
            "platform_uuid_enc mismatch"
        );
        assert_eq!(
            hw.root_disk_uuid_enc,
            b64_dec("VvJODAsuSRdGQlhB5kPgf2M="),
            "root_disk_uuid_enc mismatch"
        );
        assert_eq!(hw.rom_enc, b64_dec("wWF12gciXzN/96bIt/ufTB0="), "rom_enc mismatch");
        assert_eq!(hw.mlb_enc, b64_dec("CKp4ROiInBYAdvnbrbNjjkM="), "mlb_enc mismatch");
    }

    #[test]
    fn test_compute_missing_enc_fields_is_idempotent() {
        let mut hw = sample_hw_missing_enc();
        compute_missing_enc_fields(&mut hw).expect("first compute_missing_enc_fields failed");

        let expected_serial = hw.platform_serial_number_enc.clone();
        let expected_uuid = hw.platform_uuid_enc.clone();
        let expected_disk = hw.root_disk_uuid_enc.clone();
        let expected_rom = hw.rom_enc.clone();
        let expected_mlb = hw.mlb_enc.clone();

        compute_missing_enc_fields(&mut hw).expect("second compute_missing_enc_fields failed");

        assert_eq!(hw.platform_serial_number_enc, expected_serial);
        assert_eq!(hw.platform_uuid_enc, expected_uuid);
        assert_eq!(hw.root_disk_uuid_enc, expected_disk);
        assert_eq!(hw.rom_enc, expected_rom);
        assert_eq!(hw.mlb_enc, expected_mlb);
    }
}
