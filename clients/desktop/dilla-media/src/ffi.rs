//! The one door to the dilla-sframe/1 hook in the vendored webrtc-sys (`core/vendor/webrtc-sys`,
//! `dilla_transform`): installing it, the transformer handles of a track, draining the holds and the
//! synthetic-frame test support. Everything below is safe Rust over webrtc-sys's cxx bridge; this
//! module is where any `unsafe` of this crate would have to live, and today there is none.

use std::sync::OnceLock;

use livekit::webrtc::prelude::{RtcAudioTrack, RtcVideoTrack};

pub use webrtc_sys::dilla_transform::{FrameInfo, FrameTransform, HOLD_TICK, Verdict};

use crate::MediaError;

static INSTALLED: OnceLock<bool> = OnceLock::new();

/// Installs `make()`'s hook once for the process, before any PeerConnection exists (the factory
/// refuses to create one without it, `E_DILLA_NO_FRAME_HOOK`). `MediaError::Hook` when another hook
/// was installed first: its frames would not be this crate's.
pub(crate) fn install_once(
    make: impl FnOnce() -> Box<dyn FrameTransform>,
) -> Result<(), MediaError> {
    let ok = *INSTALLED.get_or_init(|| webrtc_sys::dilla_transform::install(make()));
    if ok { Ok(()) } else { Err(MediaError::Hook) }
}

/// Retries every held frame now (after a key install: protocol/05, "the hold drains when an epoch
/// is installed"). Never call it from inside a transform.
pub(crate) fn drain_holds() {
    webrtc_sys::dilla_transform::drain();
}

/// The receiver transformer attached for exactly this remote audio track object.
pub(crate) fn receiver_handle_audio(track: &RtcAudioTrack) -> Option<u64> {
    webrtc_sys::dilla_transform::receiver_handle(&track.sys_handle())
}

/// The receiver transformer attached for exactly this remote video track object.
pub(crate) fn receiver_handle_video(track: &RtcVideoTrack) -> Option<u64> {
    webrtc_sys::dilla_transform::receiver_handle(&track.sys_handle())
}

/// The sender transformer attached for exactly this local audio track object.
pub(crate) fn sender_handle_audio(track: &RtcAudioTrack) -> Option<u64> {
    webrtc_sys::dilla_transform::sender_handle(&track.sys_handle())
}

/// Synthetic frames through a real `DillaFrameTransformer` (tests only).
#[doc(hidden)]
pub mod testing {
    pub use webrtc_sys::dilla_transform::testing::*;
}
