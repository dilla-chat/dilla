// DILLA PATCH (core/vendor/README.md): this whole file is dilla's, not LiveKit's.
//
// The Rust side of include/livekit/dilla_transform.h: a process-wide encoded-frame hook that sees
// every frame of every RtpSender and RtpReceiver once installed, and decides its fate. LiveKit's
// own FrameCryptor and packet-trailer transformers are never attached while a hook is installed.

use std::panic::{catch_unwind, AssertUnwindSafe};

/// What happens to one encoded frame.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Verdict {
    /// Discard it. Nothing reaches the packetizer or the decoder.
    Drop,
    /// Replace its payload with `out` and pass it on.
    Replace,
    /// Keep it in this track's FIFO and ask again on the next frame (receivers; bounded by
    /// 2 000 ms, 256 frames and 8 MiB, past which it is dropped and `hold_expired` is called).
    Hold,
    /// Pass it on unchanged. Only ever right for a zero-byte audio frame (protocol/05).
    PassUnchanged,
}

/// One encoded frame as libwebrtc hands it to the transformer.
#[derive(Debug)]
pub struct FrameInfo<'a> {
    /// True on a sender (encoder to packetizer), false on a receiver (depacketizer to decoder).
    pub send: bool,
    pub video: bool,
    /// The sender's or receiver's MediaStreamTrack id at the time the transformer was attached
    /// (`MediaStreamTrack::id()` on the Rust side); empty for a sender without a track.
    pub track_id: &'a str,
    /// The frame's own codec, e.g. `audio/opus`, `video/VP8`.
    pub mime_type: &'a str,
    pub ssrc: u32,
    /// The video frame's spatial index, `None` on audio.
    pub spatial_index: Option<i32>,
    pub key_frame: bool,
}

pub trait FrameTransform: Send + Sync + 'static {
    /// Decides the frame's fate. With `Verdict::Replace`, `out` holds the new payload.
    fn transform(&self, info: &FrameInfo<'_>, data: &[u8], out: &mut Vec<u8>) -> Verdict;
    /// A held frame passed a hold limit and was dropped.
    fn hold_expired(&self, send: bool, track_id: &str);
}

pub struct DillaHook(Box<dyn FrameTransform>);

/// Installs the process-wide hook. Call it before the first PeerConnection is created. False when a
/// hook is already installed (the new one is dropped): there is exactly one for the process life.
pub fn install(transform: Box<dyn FrameTransform>) -> bool {
    ffi::dilla_install_hook(Box::new(DillaHook(transform)))
}

/// True once a hook is installed.
pub fn installed() -> bool {
    ffi::dilla_owns_transforms()
}

#[allow(clippy::too_many_arguments)]
fn dilla_transform_frame(
    hook: &DillaHook,
    send: bool,
    video: bool,
    track_id: &str,
    mime_type: &str,
    ssrc: u32,
    spatial_index: i32,
    key_frame: bool,
    data: &[u8],
    out: &mut Vec<u8>,
) -> u8 {
    let info = FrameInfo {
        send,
        video,
        track_id,
        mime_type,
        ssrc,
        spatial_index: if video { Some(spatial_index) } else { None },
        key_frame,
    };
    // A panic must not unwind into libwebrtc; the frame is dropped instead.
    let verdict = catch_unwind(AssertUnwindSafe(|| hook.0.transform(&info, data, out)))
        .unwrap_or(Verdict::Drop);
    match verdict {
        Verdict::Drop => 0,
        Verdict::Replace => 1,
        Verdict::Hold => 2,
        Verdict::PassUnchanged => 3,
    }
}

fn dilla_hold_expired(hook: &DillaHook, send: bool, track_id: &str) {
    let _ = catch_unwind(AssertUnwindSafe(|| hook.0.hold_expired(send, track_id)));
}

#[cxx::bridge(namespace = "livekit_ffi")]
pub mod ffi {
    extern "Rust" {
        type DillaHook;

        #[allow(clippy::too_many_arguments)]
        fn dilla_transform_frame(
            hook: &DillaHook,
            send: bool,
            video: bool,
            track_id: &str,
            mime_type: &str,
            ssrc: u32,
            spatial_index: i32,
            key_frame: bool,
            data: &[u8],
            out: &mut Vec<u8>,
        ) -> u8;

        fn dilla_hold_expired(hook: &DillaHook, send: bool, track_id: &str);
    }

    unsafe extern "C++" {
        include!("livekit/dilla_transform.h");

        fn dilla_install_hook(hook: Box<DillaHook>) -> bool;
        fn dilla_owns_transforms() -> bool;
    }
}
