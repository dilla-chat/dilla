// DILLA PATCH (core/vendor/README.md): this whole file is dilla's, not LiveKit's.
//
// The Rust side of include/livekit/dilla_transform.h: a process-wide encoded-frame hook that sees
// every frame of every RtpSender and RtpReceiver once installed, and decides its fate. LiveKit's
// own FrameCryptor and packet-trailer transformers are never attached while a hook is installed.

use std::panic::{catch_unwind, AssertUnwindSafe};
use std::sync::{Arc, OnceLock};
use std::time::Duration;

use cxx::SharedPtr;

use crate::media_stream_track::ffi::MediaStreamTrack;

/// What happens to one encoded frame.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Verdict {
    /// Discard it. Nothing reaches the packetizer or the decoder.
    Drop,
    /// Replace its payload with `out` and pass it on.
    Replace,
    /// Keep it in this transformer's FIFO and ask again later (receivers; bounded by 2 000 ms, 256
    /// frames and 8 MiB, past which it is dropped and `hold_expired` is called). Asked again on the
    /// next frame, on every [`drain`], and on the hook's own timer ([`HOLD_TICK`]).
    Hold,
    /// Pass it on unchanged. Honoured by the C++ glue only for a zero-byte Opus frame on an audio
    /// transformer (protocol/05); for any other frame it is a drop and `pass_refused` is called.
    PassUnchanged,
}

/// One encoded frame as libwebrtc hands it to the transformer.
#[derive(Debug)]
pub struct FrameInfo<'a> {
    /// The transformer's handle: process-unique, never reused, given to
    /// [`FrameTransform::attached`] before its first frame.
    pub handle: u64,
    /// True on a sender (encoder to packetizer), false on a receiver (depacketizer to decoder).
    pub send: bool,
    pub video: bool,
    /// The sender's or receiver's MediaStreamTrack id when the transformer was attached; empty for
    /// a sender without a track. For a receiver it is chosen by the SFU: statistics only, never a
    /// binding (bind by [`receiver_handle`]).
    pub track_id: &'a str,
    /// The frame's own codec, e.g. `audio/opus`, `video/VP8`.
    pub mime_type: &'a str,
    pub ssrc: u32,
    /// The video frame's spatial index, `None` on audio.
    pub spatial_index: Option<i32>,
    pub key_frame: bool,
}

pub trait FrameTransform: Send + Sync + 'static {
    /// A transformer was attached to a sender or receiver; `handle` names it from now on.
    fn attached(&self, _handle: u64, _send: bool, _video: bool, _track_id: &str) {}
    /// The transformer is gone: no frame of `handle` will come again.
    fn detached(&self, _handle: u64) {}
    /// Decides the frame's fate. With `Verdict::Replace`, `out` holds the new payload. Never call
    /// [`drain`] or [`request_key_frame`] from here.
    fn transform(&self, info: &FrameInfo<'_>, data: &[u8], out: &mut Vec<u8>) -> Verdict;
    /// A held frame passed a hold limit and was dropped.
    fn hold_expired(&self, handle: u64, send: bool);
    /// The glue refused a `PassUnchanged` answer (the frame was not a zero-byte Opus frame).
    fn pass_refused(&self, _handle: u64, _send: bool) {}
    /// The room's server-injected-frame trailer (LiveKit's join response), replacing any earlier one.
    fn sif_trailer(&self, _trailer: &[u8]) {}
    /// Receiving video transformers whose publisher should be asked for a key frame now; the hook's
    /// timer asks this on every tick and sends one request per handle.
    fn key_frame_requests(&self) -> Vec<u64> {
        Vec::new()
    }
}

pub struct DillaHook(Arc<dyn FrameTransform>);

/// How often the hook's own thread retries every held frame (and expires those past a limit) and
/// sends the key-frame requests the hook asks for.
pub const HOLD_TICK: Duration = Duration::from_millis(20);

static INSTALLED: OnceLock<Arc<dyn FrameTransform>> = OnceLock::new();

/// Installs the process-wide hook and starts its timer thread. Call it before the first
/// PeerConnection is created. False when a hook is already installed (the new one is dropped):
/// there is exactly one for the process life.
pub fn install(transform: Box<dyn FrameTransform>) -> bool {
    let transform: Arc<dyn FrameTransform> = Arc::from(transform);
    if !ffi::dilla_install_hook(Box::new(DillaHook(transform.clone()))) {
        return false;
    }
    let _ = INSTALLED.set(transform.clone());
    let spawned = std::thread::Builder::new().name("dilla-hold-tick".into()).spawn(move || loop {
        std::thread::sleep(HOLD_TICK);
        ffi::dilla_drain_all();
        let wanted = catch_unwind(AssertUnwindSafe(|| transform.key_frame_requests()))
            .unwrap_or_default();
        for handle in wanted {
            ffi::dilla_request_key_frame(handle);
        }
    });
    // Without the timer a held frame still drains on its track's next frame and on every
    // `drain`; it is never released unauthenticated either way.
    if spawned.is_err() {
        log::error!("dilla: the hold timer thread could not start");
    }
    true
}

/// True once a hook is installed.
pub fn installed() -> bool {
    ffi::dilla_owns_transforms()
}

/// Retries every transformer's held frames now. Call it after installing a key (protocol/05: the
/// hold drains when an epoch is installed), never from inside [`FrameTransform::transform`].
pub fn drain() {
    ffi::dilla_drain_all();
}

/// Asks the publisher of the receiving video transformer `handle` for a key frame. Blocks on
/// libwebrtc's worker thread; never call it from inside [`FrameTransform::transform`].
pub fn request_key_frame(handle: u64) {
    ffi::dilla_request_key_frame(handle);
}

/// The handle of the live receiver transformer attached for exactly this track object (compared by
/// identity, not by id); `None` when there is none.
pub fn receiver_handle(track: &SharedPtr<MediaStreamTrack>) -> Option<u64> {
    let track = track.as_ref()?;
    Some(ffi::dilla_receiver_handle(track)).filter(|h| *h != 0)
}

/// The handle of the live sender transformer attached for exactly this track object.
pub fn sender_handle(track: &SharedPtr<MediaStreamTrack>) -> Option<u64> {
    let track = track.as_ref()?;
    Some(ffi::dilla_sender_handle(track)).filter(|h| *h != 0)
}

/// Synthetic frames through a real `DillaFrameTransformer` with no PeerConnection (tests only).
#[doc(hidden)]
pub mod testing {
    use super::ffi;

    /// A transformer like one a sender or receiver would get; 0 without an installed hook.
    pub fn attach(send: bool, video: bool, track_id: &str) -> u64 {
        ffi::dilla_test_attach(send, video, track_id)
    }

    pub fn frame(handle: u64, mime: &str, data: &[u8], key_frame: bool) {
        ffi::dilla_test_frame(handle, mime, data, key_frame);
    }

    /// The frames the transformer passed on since the last call, in order.
    pub fn take_emitted(handle: u64) -> Vec<Vec<u8>> {
        let flat = ffi::dilla_test_take_emitted(handle);
        let mut out = Vec::new();
        let mut rest = flat.as_slice();
        while rest.len() >= 4 {
            let n = u32::from_be_bytes([rest[0], rest[1], rest[2], rest[3]]) as usize;
            out.push(rest[4..4 + n].to_vec());
            rest = &rest[4 + n..];
        }
        out
    }

    pub fn held(handle: u64) -> usize {
        ffi::dilla_test_held(handle)
    }

    /// Drops the transformer (the hook hears `detached`).
    pub fn release(handle: u64) {
        ffi::dilla_test_release(handle);
    }
}

fn dilla_transformer_attached(hook: &DillaHook, handle: u64, send: bool, video: bool, track_id: &str) {
    let _ = catch_unwind(AssertUnwindSafe(|| hook.0.attached(handle, send, video, track_id)));
}

fn dilla_transformer_detached(hook: &DillaHook, handle: u64) {
    let _ = catch_unwind(AssertUnwindSafe(|| hook.0.detached(handle)));
}

#[allow(clippy::too_many_arguments)]
fn dilla_transform_frame(
    hook: &DillaHook,
    handle: u64,
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
        handle,
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

fn dilla_hold_expired(hook: &DillaHook, handle: u64, send: bool) {
    let _ = catch_unwind(AssertUnwindSafe(|| hook.0.hold_expired(handle, send)));
}

fn dilla_pass_refused(hook: &DillaHook, handle: u64, send: bool) {
    let _ = catch_unwind(AssertUnwindSafe(|| hook.0.pass_refused(handle, send)));
}

fn dilla_sif_trailer(hook: &DillaHook, trailer: &[u8]) {
    let _ = catch_unwind(AssertUnwindSafe(|| hook.0.sif_trailer(trailer)));
}

#[cxx::bridge(namespace = "livekit_ffi")]
pub mod ffi {
    extern "Rust" {
        type DillaHook;

        fn dilla_transformer_attached(
            hook: &DillaHook,
            handle: u64,
            send: bool,
            video: bool,
            track_id: &str,
        );
        fn dilla_transformer_detached(hook: &DillaHook, handle: u64);

        #[allow(clippy::too_many_arguments)]
        fn dilla_transform_frame(
            hook: &DillaHook,
            handle: u64,
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

        fn dilla_hold_expired(hook: &DillaHook, handle: u64, send: bool);
        fn dilla_pass_refused(hook: &DillaHook, handle: u64, send: bool);
        fn dilla_sif_trailer(hook: &DillaHook, trailer: &[u8]);
    }

    unsafe extern "C++" {
        include!("livekit/dilla_transform.h");

        type MediaStreamTrack = crate::media_stream_track::ffi::MediaStreamTrack;

        fn dilla_install_hook(hook: Box<DillaHook>) -> bool;
        fn dilla_owns_transforms() -> bool;
        fn dilla_receiver_handle(track: &MediaStreamTrack) -> u64;
        fn dilla_sender_handle(track: &MediaStreamTrack) -> u64;
        fn dilla_drain_all();
        fn dilla_request_key_frame(handle: u64);

        fn dilla_test_attach(send: bool, video: bool, track_id: &str) -> u64;
        fn dilla_test_frame(handle: u64, mime: &str, data: &[u8], key_frame: bool);
        fn dilla_test_take_emitted(handle: u64) -> Vec<u8>;
        fn dilla_test_emitted_count(handle: u64) -> usize;
        fn dilla_test_held(handle: u64) -> usize;
        fn dilla_test_release(handle: u64);
    }
}
