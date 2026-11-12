//! dilla-core: the cryptographic, protocol and storage core of dilla.
//!
//! Every module here is target-agnostic. The two binding crates
//! (`dilla-core-wasm`, `dilla-core-wasi`) hold the target-specific glue.
#![forbid(unsafe_code)]

pub mod attachment;
pub mod bot;
pub mod cbor;
// The client uses the SQLite half of `mls`, unavailable on wasm32-wasip1.
#[cfg(any(not(target_arch = "wasm32"), target_os = "unknown"))]
pub mod client;
pub mod envelope;
pub mod error;
pub mod identity;
pub mod ids;
pub mod mls;
// Native only: keying a store file on SQLite3 Multiple Ciphers (the browser keys OPFS in dilla-core-wasm).
#[cfg(not(target_arch = "wasm32"))]
pub mod native_store;
pub mod public_group;
pub mod sframe;
#[cfg(feature = "vectors")]
pub mod vectors;

pub use error::{CoreError, ProtocolError};

/// The crate version, reported over every binding.
pub const CORE_VERSION: &str = env!("CARGO_PKG_VERSION");
/// `e2ee_version` as recorded in every `dilla_binding` (protocol/07-versioning.md).
pub const E2EE_VERSION: u64 = 1;
/// `media_version` for call groups (protocol/07-versioning.md).
pub const MEDIA_VERSION: u64 = 1;
/// The one negotiated wire version (Bot v2: wire 6, no older clients); HTTP routes remain under `/v1`.
pub const WIRE_VERSION: u64 = 6;
/// The wasi ABI version every request carries as element 0.
///
/// **2** since 2026-09-24: `public_group_process`'s response grew from 6 to 8 elements (the
/// applied-proposal list and `committer_updated`) and `validate_key_package`'s from 5 to 6
/// (`kp_ref`). Both are response-shape changes, and dillad's host is the only consumer, so the
/// version moves instead of a compatibility shim being written (R27, interfaces §3).
///
/// **3** since 2026-09-29 (dillad-1 task 27a, Ruling C): `public_group_process`'s response grew a
/// ninth element, `new_leaf` — the leaf an external commit's joiner lands on — and the module
/// grew `device_list_entries`, the verified decoder of a user's signed device list (NV-B8).
///
/// **4** since 2026-10-05 (hardening C): `validate_key_package`'s response grew a seventh element,
/// the KeyPackage leaf's `signature_key`, and each item of `public_group_process`'s applied list a
/// sixth, the added leaf's `signature_key` (an Add's; null for every other proposal), so the
/// delivery service can bind a new leaf to the device's registered key without parsing MLS.
///
/// **5** since 2026-10-05 (hardening G): `public_group_state`'s response grew a seventh element,
/// `leaf_count`, the number of occupied leaves in the tree. The member list leaves out a leaf whose
/// credential is not a dilla identity or whose key is not 32 bytes, so it cannot say how many
/// leaves a tree holds; group registration and heal's reseed adopt a tree only when every leaf is
/// one the delivery service can name.
///
/// **6** since 2026-10-06 (DS-MEMBERSHIP-01): `public_group_state`'s response grew an eighth
/// element, `external_senders`, the group context's external-sender entries as
/// `[credential_type, credential, signature_key]` each (empty when the extension is absent).
/// Group registration and heal's reseed adopt a `text` or `call` group only when its one entry is
/// this instance's key and `"instance"` credential, and a `pairing` or `interaction` group only
/// when it carries none: a group whose extension names another key is one the instance can never
/// propose into.
///
/// **7** since 2026-10-21 (DILLA-BOT-04): the WASI module adds `verify_credential`, which decodes
/// and strictly verifies a credential with the MLS leaf's DSK and returns only public identity
/// fields. Browser, E2EE, media and negotiated wire versions do not change.
///
/// **8** since 2026-10-21 (DILLA-BOT-04 Task5): `validate_key_package`'s response grew an eighth
/// element, the exact credential bytes validated from the KeyPackage leaf. The Go host bounds
/// and passes those bytes to the managed Bot admission check; public wire versions do not change.
/// **9** adds `public_group_roster_extract`, a bounded export that verifies GroupInfo/tree
/// authentication and every member credential before returning roster descriptors.
/// **10** adds strict MLS Welcome recipient KeyPackageRef inspection.
pub const ABI_VERSION: u64 = 10;
