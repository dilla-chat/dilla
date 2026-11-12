//! Proof (Task 2): LiveKit's Rust SDK carrying dilla-sframe/1 frames that the browser's
//! `MediaReceiver` / `MediaSender` (core/dilla-core-wasm) accept, through the vendored webrtc-sys
//! encoded-frame hook (core/vendor/webrtc-sys, `dilla_transform`). See REPORT.md.

/// `dilla_core::sframe`, compiled from dilla-core's own source files — the same code, not a copy —
/// because the dilla-core crate cannot be linked beside libwebrtc yet (REPORT.md, "Link
/// conflict"). Its own unit tests, including the protocol/vectors/sframe.json runner, run here too.
#[path = "../../../core/dilla-core/src/sframe/mod.rs"]
pub mod sframe;

/// The two helpers of `dilla_core::identity` that the sframe module calls, written as dilla-core
/// writes them (core/dilla-core/src/identity/mod.rs).
mod identity {
    use hkdf::Hkdf;
    use hmac::{Hmac, KeyInit, Mac};
    use sha2::Sha256;

    pub fn hmac_sha256(key: &[u8], data: &[u8]) -> [u8; 32] {
        let mut mac =
            Hmac::<Sha256>::new_from_slice(key).expect("HMAC accepts a key of any length");
        mac.update(data);
        let mut out = [0u8; 32];
        out.copy_from_slice(&mac.finalize().into_bytes());
        out
    }

    /// HKDF-SHA256 (RFC 5869): Expand(Extract(salt, ikm), info) into `okm`.
    pub fn hkdf_sha256(
        salt: Option<&[u8]>,
        ikm: &[u8],
        info: &[u8],
        okm: &mut [u8],
    ) -> Result<(), hkdf::InvalidLength> {
        Hkdf::<Sha256>::new(salt, ikm).expand(info, okm)
    }
}

pub mod media;
pub mod signal;
