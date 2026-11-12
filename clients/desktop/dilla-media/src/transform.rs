//! The dilla-sframe/1 transform behind the webrtc-sys hook: the desktop's equivalent of the browser
//! media worker's pipeline (packages/media/src/worker/pipeline.ts), built only from
//! `dilla_core::sframe`. Promoted from Task 2's proof (`experiments/gpui-voice-proof/src/media.rs`).
//!
//! One [`Router`] is installed as the process-wide hook (once, before any PeerConnection). It knows
//! every attached transformer by its opaque handle and routes each frame to the current call's
//! [`CallMedia`]; with no call every frame is dropped (a zero-byte Opus frame aside).
//!
//! Sender: a frame of a transformer bound to a slot ([`CallMedia::expect_local`] before publishing,
//! bound when the transformer attaches with that local track id, or [`CallMedia::bind_local`]) is
//! sealed by `SframeSender` with its own codec (from the frame's mime type), the slot and a layer
//! allocated first-seen per `(KID, slot)` from `(ssrc, spatial index)`. A zero-byte Opus frame is
//! answered `PassUnchanged` (DTX; the C++ glue forwards it only when it is one); anything else that
//! cannot be sealed is dropped, never sent in the clear.
//!
//! Receiver, in protocol/05's order: a zero-byte Opus frame passes; a frame ending in the room's
//! server-injected-frame trailer is dropped before any header is parsed; a transformer not bound
//! yet ([`CallMedia::bind_remote`], by handle, from the subscribed track's own object) holds; then
//! `KeyRing::decrypt` against the bound device and slot. `E_SFRAME_UNKNOWN_KID` holds; every other
//! failure drops and is counted by its code. After a video drop that stalls the decoder (an
//! expired hold, a failed tag, a stale epoch, an unreadable frame) the receiver asks the publisher
//! for a key frame, again at most every 500 ms, until a decrypted key frame passes (protocol/05
//! "Key frames").

use std::collections::{BTreeMap, HashMap, HashSet};
use std::sync::{Arc, Mutex, MutexGuard, OnceLock};
use std::time::Instant;

use dilla_core::sframe::{Codec, KeyRing, Kid, NK, SframeError, SframeSender, Slot};
use serde::Serialize;
use zeroize::Zeroizing;

use crate::MediaError;
use crate::ffi::{self, FrameInfo, FrameTransform, Verdict};

/// protocol/05 "Key frames": a request is repeated at most this often on one track.
pub const KEY_FRAME_REQUEST_INTERVAL_MS: u64 = 500;

/// The KID as the browser's stats spell it (`kidHex`): `((leaf << 8) | epoch mod 256)` in hex.
pub fn kid_hex(leaf: u16, epoch: u64) -> String {
    format!("{:x}", Kid::new(leaf, epoch).value())
}

/// The frame's own codec from libwebrtc's mime type; `None` for anything without a prefix rule.
pub fn codec_from_mime(mime: &str) -> Option<Codec> {
    match mime.to_ascii_lowercase().as_str() {
        "audio/opus" => Some(Codec::Opus),
        "video/vp8" => Some(Codec::Vp8),
        "video/vp9" => Some(Codec::Vp9),
        "video/h264" => Some(Codec::H264),
        _ => None,
    }
}

fn slot_is_audio(slot: Slot) -> bool {
    matches!(slot, Slot::Microphone | Slot::ScreenAudio)
}

/// True when `frame` ends in the room's server-injected-frame trailer (packages/media
/// worker/sif.ts `hasSifSuffix`): an empty trailer matches nothing.
pub fn has_sif_suffix(frame: &[u8], trailer: &[u8]) -> bool {
    !trailer.is_empty() && frame.len() >= trailer.len() && frame.ends_with(trailer)
}

/// Drop reasons after which a video decoder stalls until a key frame (the worker's
/// `KEY_FRAME_REASONS`: bufferTimeout, unknownKid, aeadFail, expiredKid, parse, internal). A replay,
/// sender, leaf or slot mismatch, own-KID refusal, unsupported codec or SIF frame never asks.
fn stalls_decoder(e: &SframeError) -> bool {
    !matches!(
        e,
        SframeError::Replay
            | SframeError::SenderMismatch
            | SframeError::LeafNotInEpoch
            | SframeError::OwnKid
            | SframeError::SlotMismatch
            | SframeError::UnsupportedCodec
            | SframeError::NoVclNal
    )
}

/// What one call counted, in the browser's vocabulary where one exists.
#[derive(Clone, Debug, Default, Serialize, PartialEq, Eq)]
pub struct Stats {
    /// Frames sealed, by KID hex.
    pub encrypted: BTreeMap<String, u64>,
    /// Frames authenticated and bound to their transformer's device, by KID hex.
    pub opened: BTreeMap<String, u64>,
    /// Frames authenticated per device (hex): the device is verified (protocol/05).
    pub verified: BTreeMap<String, u64>,
    /// Frames refused, by `E_SFRAME_*` code or a local reason (`unbound`, `unsupportedCodec`,
    /// `kindMismatch`, `emptyVideo`, `holdExpired`, `noSender`, `noCall`, `sif`, `passRefused`).
    pub failed: BTreeMap<String, u64>,
    /// Zero-byte audio frames passed through, on each side.
    pub empty_send: u64,
    pub empty_recv: u64,
    /// Times a receiver frame was answered `Hold` (one frame can be asked again).
    pub hold_answers: u64,
    /// Key-frame requests sent.
    pub key_frame_requests: u64,
}

impl Stats {
    pub fn total_opened(&self) -> u64 {
        self.opened.values().sum()
    }
    pub fn total_failed(&self) -> u64 {
        self.failed.values().sum()
    }
}

/// What a transformer handle is bound to.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Binding {
    Local(Slot),
    Remote { device: [u8; 16], slot: Slot },
}

/// A receiving video transformer waiting for a decrypted key frame.
#[derive(Clone, Copy, Debug)]
struct KeyFrameWait {
    /// When the last request went out (ms on the call's clock); `None` before the first.
    last: Option<u64>,
}

struct State {
    closed: bool,
    own_leaf: Option<u16>,
    epoch: u64,
    sender: Option<SframeSender>,
    ring: KeyRing,
    expected_local: HashMap<String, Slot>,
    bindings: HashMap<u64, Binding>,
    sif: Vec<u8>,
    layers: HashMap<(u64, u8), HashMap<(u32, i32), u8>>,
    key_frames: HashMap<u64, KeyFrameWait>,
    stats: Stats,
}

/// One device's media keys and transformer bindings for one call.
pub struct CallMedia {
    state: Mutex<State>,
    clock: Instant,
}

impl CallMedia {
    pub fn new() -> Arc<Self> {
        Arc::new(Self {
            state: Mutex::new(State {
                closed: false,
                own_leaf: None,
                epoch: 0,
                sender: None,
                ring: KeyRing::new(),
                expected_local: HashMap::new(),
                bindings: HashMap::new(),
                sif: Vec::new(),
                layers: HashMap::new(),
                key_frames: HashMap::new(),
                stats: Stats::default(),
            }),
            clock: Instant::now(),
        })
    }

    fn lock(&self) -> MutexGuard<'_, State> {
        self.state.lock().unwrap_or_else(|p| p.into_inner())
    }

    fn now_ms(&self) -> u64 {
        u64::try_from(self.clock.elapsed().as_millis()).unwrap_or(u64::MAX)
    }

    /// Installs `epoch` (the call group's epoch from `ClientCore::call_media_epoch`): first on the
    /// receive side (`KeyRing::install_epoch`), then the sender switches to it. The first install
    /// creates the sender with `min_epoch = epoch` (N1: the join epoch); later ones rekey it and its
    /// counters carry on (N3). An install older than the sender's epoch changes only the ring. Held
    /// frames are retried at once (protocol/05: the hold drains when an epoch is installed).
    pub fn install_epoch(
        &self,
        epoch: u64,
        base_key: &[u8; NK],
        roster: &[(u16, [u8; 16])],
        own_leaf: u16,
    ) -> Result<(), MediaError> {
        let now = self.now_ms();
        {
            let mut s = self.lock();
            if s.closed {
                return Err(MediaError::Closed);
            }
            s.ring
                .install_epoch(epoch, base_key, roster, Some(own_leaf), now);
            let current = s.epoch;
            match s.sender.as_mut() {
                Some(sender) if epoch > current => {
                    sender.rekey(base_key, own_leaf, epoch);
                    s.epoch = epoch;
                    s.own_leaf = Some(own_leaf);
                }
                Some(_) => {}
                None => {
                    s.sender = Some(
                        SframeSender::new(base_key, own_leaf, epoch, epoch)
                            .map_err(|e| MediaError::Keys(e.code().into()))?,
                    );
                    s.epoch = epoch;
                    s.own_leaf = Some(own_leaf);
                }
            }
        }
        // Outside the lock: a drain runs every transform, which takes it.
        ffi::drain_holds();
        Ok(())
    }

    /// The epoch the sender seals under, once one is installed.
    pub fn sending_epoch(&self) -> Option<u64> {
        let s = self.lock();
        s.sender.as_ref().map(|_| s.epoch)
    }

    /// A local track this call is about to publish: the sender transformer attached for it is
    /// bound to `slot` as it attaches, before its first frame.
    pub fn expect_local(&self, track_id: &str, slot: Slot) {
        self.lock().expected_local.insert(track_id.to_owned(), slot);
    }

    /// Binds a sender transformer to a slot. Refused when the handle is already bound.
    pub fn bind_local(&self, handle: u64, slot: Slot) -> Result<(), MediaError> {
        self.bind(handle, Binding::Local(slot))
    }

    /// Binds a receiver transformer (found from its subscribed track's own object) to its
    /// participant's device and its publication's slot. Refused when the handle is already bound.
    /// Held frames are retried at once.
    pub fn bind_remote(&self, handle: u64, device: [u8; 16], slot: Slot) -> Result<(), MediaError> {
        self.bind(handle, Binding::Remote { device, slot })?;
        ffi::drain_holds();
        Ok(())
    }

    fn bind(&self, handle: u64, binding: Binding) -> Result<(), MediaError> {
        let mut s = self.lock();
        if s.closed {
            return Err(MediaError::Closed);
        }
        if s.bindings.contains_key(&handle) {
            return Err(MediaError::AlreadyBound(handle));
        }
        s.bindings.insert(handle, binding);
        Ok(())
    }

    /// The room's server-injected-frame trailer; replaces any earlier one (protocol/05).
    pub fn set_sif_trailer(&self, trailer: &[u8]) {
        if !trailer.is_empty() {
            self.lock().sif = trailer.to_vec();
        }
    }

    pub fn stats(&self) -> Stats {
        self.lock().stats.clone()
    }

    /// The devices (hex) at least one of whose frames authenticated.
    pub fn verified_devices(&self) -> HashSet<String> {
        self.lock().stats.verified.keys().cloned().collect()
    }

    /// Ends the call's media: the keys are dropped (the sender's key is zeroized by dilla-core's
    /// `SframeSender`; the ring with them) and every later frame is dropped.
    pub fn close(&self) {
        let mut s = self.lock();
        s.closed = true;
        s.sender = None;
        s.ring = KeyRing::new();
        s.bindings.clear();
        s.expected_local.clear();
        s.key_frames.clear();
    }

    fn attached(&self, handle: u64, send: bool, track_id: &str) {
        let mut s = self.lock();
        if s.closed || !send {
            return;
        }
        if let Some(slot) = s.expected_local.get(track_id).copied() {
            s.bindings.entry(handle).or_insert(Binding::Local(slot));
        }
    }

    fn detached(&self, handle: u64) {
        let mut s = self.lock();
        s.bindings.remove(&handle);
        s.key_frames.remove(&handle);
    }

    fn fail(s: &mut State, reason: &str) -> Verdict {
        *s.stats.failed.entry(reason.to_owned()).or_default() += 1;
        Verdict::Drop
    }

    /// A receiving video transformer dropped a frame its decoder needed: request a key frame now
    /// (on the next tick) and keep requesting until a decrypted key frame passes.
    fn want_key_frame(s: &mut State, handle: u64) {
        s.key_frames
            .entry(handle)
            .or_insert(KeyFrameWait { last: None });
    }

    fn key_frame_requests(&self) -> Vec<u64> {
        let now = self.now_ms();
        let mut s = self.lock();
        let mut due = Vec::new();
        for (handle, wait) in s.key_frames.iter_mut() {
            if wait
                .last
                .is_none_or(|last| now.saturating_sub(last) >= KEY_FRAME_REQUEST_INTERVAL_MS)
            {
                wait.last = Some(now);
                due.push(*handle);
            }
        }
        s.stats.key_frame_requests += due.len() as u64;
        due
    }

    fn encrypt(&self, info: &FrameInfo<'_>, data: &[u8], out: &mut Vec<u8>) -> Verdict {
        let mut s = self.lock();
        if s.closed {
            return Self::fail(&mut s, "noCall");
        }
        let Some(Binding::Local(slot)) = s.bindings.get(&info.handle).copied() else {
            return Self::fail(&mut s, "unbound");
        };
        let Some(codec) = codec_from_mime(info.mime_type) else {
            return Self::fail(&mut s, "unsupportedCodec");
        };
        if slot_is_audio(slot) != (codec == Codec::Opus) || info.video == slot_is_audio(slot) {
            return Self::fail(&mut s, "kindMismatch");
        }
        if data.is_empty() {
            if codec == Codec::Opus {
                s.stats.empty_send += 1;
                return Verdict::PassUnchanged;
            }
            return Self::fail(&mut s, "emptyVideo");
        }
        let epoch = s.epoch;
        let Some(leaf) = s.own_leaf else {
            return Self::fail(&mut s, "noSender");
        };
        let layer = if info.video {
            let kid = Kid::new(leaf, epoch).value();
            let scope = s.layers.entry((kid, slot as u8)).or_default();
            let key = (info.ssrc, info.spatial_index.unwrap_or(-1));
            let next = u8::try_from(scope.len()).unwrap_or(u8::MAX);
            let layer = *scope.entry(key).or_insert(next);
            if layer > 0xf {
                return Self::fail(&mut s, "E_SFRAME_LAYER_RANGE");
            }
            layer
        } else {
            0
        };
        let Some(sender) = s.sender.as_mut() else {
            return Self::fail(&mut s, "noSender");
        };
        match sender.encrypt(codec, slot, layer, data) {
            Ok(sealed) => {
                *out = sealed;
                *s.stats.encrypted.entry(kid_hex(leaf, epoch)).or_default() += 1;
                Verdict::Replace
            }
            Err(e) => Self::fail(&mut s, e.code()),
        }
    }

    fn decrypt(&self, info: &FrameInfo<'_>, data: &[u8], out: &mut Vec<u8>) -> Verdict {
        let now = self.now_ms();
        let mut s = self.lock();
        let codec = codec_from_mime(info.mime_type);
        if data.is_empty() {
            if !info.video && codec == Some(Codec::Opus) {
                s.stats.empty_recv += 1;
                return Verdict::PassUnchanged;
            }
            return Self::fail(&mut s, "emptyVideo");
        }
        if s.closed {
            return Self::fail(&mut s, "noCall");
        }
        // Before any SFrame header is parsed (protocol/05 "Authenticity"); never a key-frame request.
        if has_sif_suffix(data, &s.sif) {
            return Self::fail(&mut s, "sif");
        }
        let Some(binding) = s.bindings.get(&info.handle).copied() else {
            s.stats.hold_answers += 1;
            return Verdict::Hold;
        };
        let Binding::Remote { device, slot } = binding else {
            return Self::fail(&mut s, "kindMismatch");
        };
        let Some(codec) = codec else {
            return Self::fail(&mut s, "unsupportedCodec");
        };
        if slot_is_audio(slot) != (codec == Codec::Opus) || info.video == slot_is_audio(slot) {
            return Self::fail(&mut s, "kindMismatch");
        }
        match s.ring.decrypt(codec, data, Some(&device), slot, now) {
            Ok(d) => {
                *out = d.frame;
                *s.stats
                    .opened
                    .entry(kid_hex(d.leaf_index, d.epoch))
                    .or_default() += 1;
                *s.stats.verified.entry(hex(&device)).or_default() += 1;
                if info.video && info.key_frame {
                    s.key_frames.remove(&info.handle);
                }
                Verdict::Replace
            }
            Err(SframeError::UnknownKid) => {
                s.stats.hold_answers += 1;
                Verdict::Hold
            }
            Err(e) => {
                if info.video && stalls_decoder(&e) {
                    Self::want_key_frame(&mut s, info.handle);
                }
                Self::fail(&mut s, e.code())
            }
        }
    }

    fn hold_expired(&self, handle: u64, video: bool) {
        let mut s = self.lock();
        Self::fail(&mut s, "holdExpired");
        if video {
            Self::want_key_frame(&mut s, handle);
        }
    }

    fn pass_refused(&self) {
        let mut s = self.lock();
        Self::fail(&mut s, "passRefused");
    }
}

pub(crate) fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

/// What the router knows about one attached transformer.
#[derive(Clone, Debug)]
struct Attached {
    send: bool,
    video: bool,
}

/// The process-wide hook: every transformer by handle, and the call whose keys its frames use.
pub struct Router {
    attached: Mutex<HashMap<u64, Attached>>,
    call: Mutex<Option<Arc<CallMedia>>>,
    /// The last trailer LiveKit announced, for a call that becomes current after it.
    sif: Mutex<Zeroizing<Vec<u8>>>,
}

static ROUTER: OnceLock<Arc<Router>> = OnceLock::new();

impl Router {
    /// The router, installing it as the hook on first use. Must run before the process's first
    /// PeerConnection (the factory refuses one without a hook).
    pub fn get() -> Result<Arc<Router>, MediaError> {
        let router = ROUTER
            .get_or_init(|| {
                Arc::new(Router {
                    attached: Mutex::new(HashMap::new()),
                    call: Mutex::new(None),
                    sif: Mutex::new(Zeroizing::new(Vec::new())),
                })
            })
            .clone();
        let hook = router.clone();
        ffi::install_once(move || Box::new(Hook(hook)))?;
        Ok(router)
    }

    /// Makes `media` the call every frame is routed to. `MediaError::Busy` while another call is
    /// current: one call per process (N2: one publisher per device and call).
    pub fn begin(&self, media: Arc<CallMedia>) -> Result<(), MediaError> {
        let mut call = self.call.lock().unwrap_or_else(|p| p.into_inner());
        if call.is_some() {
            return Err(MediaError::Busy);
        }
        let sif = self.sif.lock().unwrap_or_else(|p| p.into_inner()).clone();
        media.set_sif_trailer(&sif);
        *call = Some(media);
        Ok(())
    }

    /// Ends `media` (if it is current): every later frame is dropped.
    pub fn end(&self, media: &Arc<CallMedia>) {
        media.close();
        let mut call = self.call.lock().unwrap_or_else(|p| p.into_inner());
        if call.as_ref().is_some_and(|c| Arc::ptr_eq(c, media)) {
            *call = None;
        }
    }

    fn current(&self) -> Option<Arc<CallMedia>> {
        self.call.lock().unwrap_or_else(|p| p.into_inner()).clone()
    }

    fn is_video(&self, handle: u64) -> bool {
        self.attached
            .lock()
            .unwrap_or_else(|p| p.into_inner())
            .get(&handle)
            .is_some_and(|a| a.video && !a.send)
    }
}

struct Hook(Arc<Router>);

impl FrameTransform for Hook {
    fn attached(&self, handle: u64, send: bool, video: bool, track_id: &str) {
        self.0
            .attached
            .lock()
            .unwrap_or_else(|p| p.into_inner())
            .insert(handle, Attached { send, video });
        if let Some(call) = self.0.current() {
            call.attached(handle, send, track_id);
        }
    }

    fn detached(&self, handle: u64) {
        self.0
            .attached
            .lock()
            .unwrap_or_else(|p| p.into_inner())
            .remove(&handle);
        if let Some(call) = self.0.current() {
            call.detached(handle);
        }
    }

    fn transform(&self, info: &FrameInfo<'_>, data: &[u8], out: &mut Vec<u8>) -> Verdict {
        let Some(call) = self.0.current() else {
            // No call: nothing is sent or rendered. A zero-byte Opus frame carries no content.
            let empty_opus = data.is_empty()
                && !info.video
                && codec_from_mime(info.mime_type) == Some(Codec::Opus);
            return if empty_opus {
                Verdict::PassUnchanged
            } else {
                Verdict::Drop
            };
        };
        if info.send {
            call.encrypt(info, data, out)
        } else {
            call.decrypt(info, data, out)
        }
    }

    fn hold_expired(&self, handle: u64, send: bool) {
        if let Some(call) = self.0.current() {
            call.hold_expired(handle, !send && self.0.is_video(handle));
        }
    }

    fn pass_refused(&self, _handle: u64, _send: bool) {
        if let Some(call) = self.0.current() {
            call.pass_refused();
        }
    }

    fn sif_trailer(&self, trailer: &[u8]) {
        if trailer.is_empty() {
            return;
        }
        *self.0.sif.lock().unwrap_or_else(|p| p.into_inner()) = Zeroizing::new(trailer.to_vec());
        if let Some(call) = self.0.current() {
            call.set_sif_trailer(trailer);
        }
    }

    fn key_frame_requests(&self) -> Vec<u64> {
        self.0
            .current()
            .map(|c| c.key_frame_requests())
            .unwrap_or_default()
    }
}
