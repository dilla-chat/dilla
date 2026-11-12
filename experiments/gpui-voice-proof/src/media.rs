//! The dilla-sframe/1 transform behind the webrtc-sys hook: the desktop's equivalent of the browser
//! media worker's pipeline (packages/media/src/worker/pipeline.ts), built only from
//! `dilla_core::sframe`.
//!
//! Sender: a frame of a track bound with [`ProofMedia::bind_local`] is sealed by `SframeSender`
//! with its own codec (from the frame's mime type), the track's slot and a layer allocated
//! first-seen per `(KID, slot)` from `(ssrc, spatial index)` — the worker's `LayerAllocator`. A
//! zero-byte Opus frame passes unchanged (DTX); anything else that cannot be sealed is dropped,
//! never sent in the clear.
//!
//! Receiver: a frame of a track bound with [`ProofMedia::bind_remote`] (device and slot from the
//! LiveKit participant identity and track source) goes through `KeyRing::decrypt`; an unbound
//! track's frames and `E_SFRAME_UNKNOWN_KID` are held (the C++ FIFO), every other failure is
//! dropped and counted by its `E_SFRAME_*` code.

use std::collections::{BTreeMap, HashMap};
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::Instant;

use crate::sframe::{
    Codec, KeyRing, Kid, NK, SframeError, SframeSender, Slot, peek_kid_ctr, unescape_protected,
};
use serde::Serialize;
use webrtc_sys::dilla_transform::{FrameInfo, FrameTransform, Verdict};

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

/// What one side counted, in the browser's vocabulary where one exists.
#[derive(Clone, Debug, Default, Serialize, PartialEq, Eq)]
pub struct Stats {
    /// Frames sealed, by KID hex.
    pub encrypted: BTreeMap<String, u64>,
    /// Frames authenticated and bound to their track's device, by KID hex.
    pub opened: BTreeMap<String, u64>,
    /// Frames refused, by `E_SFRAME_*` code or a local reason (`unbound`, `unsupportedCodec`,
    /// `kindMismatch`, `emptyVideo`, `holdExpired`, `noSender`).
    pub failed: BTreeMap<String, u64>,
    /// Zero-byte audio frames passed through, on each side.
    pub empty_send: u64,
    pub empty_recv: u64,
    /// Times a receiver frame was answered `Hold` (one frame can be asked again).
    pub hold_answers: u64,
    /// Time spent inside the transform per frame (lock, codec checks, seal or open), on each side.
    pub send_latency: Latency,
    pub recv_latency: Latency,
}

/// Per-frame transform time over the last frames seen (at most `LATENCY_WINDOW`).
#[derive(Clone, Debug, Default, Serialize, PartialEq, Eq)]
pub struct Latency {
    pub frames: u64,
    pub p50_us: u64,
    pub p99_us: u64,
    pub max_us: u64,
}

const LATENCY_WINDOW: usize = 20_000;

fn summarise(frames: u64, samples: &[u64]) -> Latency {
    let mut v = samples.to_vec();
    v.sort_unstable();
    let at = |q: f64| {
        v.get(((v.len().saturating_sub(1)) as f64 * q).round() as usize)
            .copied()
            .unwrap_or(0)
    };
    Latency {
        frames,
        p50_us: at(0.5),
        p99_us: at(0.99),
        max_us: v.last().copied().unwrap_or(0),
    }
}

impl Stats {
    pub fn total_opened(&self) -> u64 {
        self.opened.values().sum()
    }
    pub fn total_failed(&self) -> u64 {
        self.failed.values().sum()
    }
}

struct State {
    sender: Option<SframeSender>,
    epoch: u64,
    own_leaf: u16,
    ring: KeyRing,
    local: HashMap<String, Slot>,
    remote: HashMap<String, ([u8; 16], Slot)>,
    layers: HashMap<(u64, u8), HashMap<(u32, i32), u8>>,
    stats: Stats,
    /// (frames, recent per-frame microseconds) for send [0] and receive [1].
    latency: [(u64, std::collections::VecDeque<u64>); 2],
}

/// One device's media keys and track bindings for one call.
pub struct ProofMedia {
    state: Mutex<State>,
    clock: Instant,
}

impl ProofMedia {
    pub fn new(own_leaf: u16) -> Arc<Self> {
        Arc::new(Self {
            state: Mutex::new(State {
                sender: None,
                epoch: 0,
                own_leaf,
                ring: KeyRing::new(),
                local: HashMap::new(),
                remote: HashMap::new(),
                layers: HashMap::new(),
                stats: Stats::default(),
                latency: Default::default(),
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

    /// Installs `epoch` on the receive side (`KeyRing::install_epoch`).
    pub fn install_epoch(&self, epoch: u64, base_key: &[u8; NK], roster: &[(u16, [u8; 16])]) {
        let now = self.now_ms();
        let mut s = self.lock();
        let own = Some(s.own_leaf);
        s.ring.install_epoch(epoch, base_key, roster, own, now);
    }

    /// Starts sending under `epoch` (the first call creates the sender with `min_epoch = epoch`;
    /// later calls rekey it, and its counters carry on — N3).
    pub fn send_epoch(&self, epoch: u64, base_key: &[u8; NK]) -> Result<(), SframeError> {
        let mut s = self.lock();
        let leaf = s.own_leaf;
        match s.sender.as_mut() {
            Some(sender) => sender.rekey(base_key, leaf, epoch),
            None => s.sender = Some(SframeSender::new(base_key, leaf, epoch, epoch)?),
        }
        s.epoch = epoch;
        Ok(())
    }

    /// A local track (its `MediaStreamTrack::id()`) and the slot of its source.
    pub fn bind_local(&self, track_id: &str, slot: Slot) {
        self.lock().local.insert(track_id.to_owned(), slot);
    }

    /// A remote track (its `MediaStreamTrack::id()`), its participant's device and its slot.
    pub fn bind_remote(&self, track_id: &str, device: [u8; 16], slot: Slot) {
        self.lock()
            .remote
            .insert(track_id.to_owned(), (device, slot));
    }

    pub fn stats(&self) -> Stats {
        let s = self.lock();
        let mut out = s.stats.clone();
        let [send, recv] = &s.latency;
        out.send_latency = summarise(send.0, &send.1.iter().copied().collect::<Vec<_>>());
        out.recv_latency = summarise(recv.0, &recv.1.iter().copied().collect::<Vec<_>>());
        out
    }

    fn record_latency(&self, send: bool, micros: u64) {
        let mut s = self.lock();
        let (frames, samples) = &mut s.latency[usize::from(!send)];
        *frames += 1;
        if samples.len() == LATENCY_WINDOW {
            samples.pop_front();
        }
        samples.push_back(micros);
    }

    fn fail(s: &mut State, reason: &str) -> Verdict {
        *s.stats.failed.entry(reason.to_owned()).or_default() += 1;
        Verdict::Drop
    }

    fn encrypt(&self, info: &FrameInfo<'_>, data: &[u8], out: &mut Vec<u8>) -> Verdict {
        let mut s = self.lock();
        let Some(&slot) = s.local.get(info.track_id) else {
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
        let leaf = s.own_leaf;
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
        let Some(&(device, slot)) = s.remote.get(info.track_id) else {
            s.stats.hold_answers += 1;
            return Verdict::Hold;
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
                Verdict::Replace
            }
            Err(SframeError::UnknownKid) => {
                s.stats.hold_answers += 1;
                Verdict::Hold
            }
            Err(e) => Self::fail(&mut s, e.code()),
        }
    }
}

/// The hook webrtc-sys calls for every frame.
pub struct Hook(pub Arc<ProofMedia>);

impl FrameTransform for Hook {
    fn transform(&self, info: &FrameInfo<'_>, data: &[u8], out: &mut Vec<u8>) -> Verdict {
        let start = Instant::now();
        let verdict = if info.send {
            self.0.encrypt(info, data, out)
        } else {
            self.0.decrypt(info, data, out)
        };
        let micros = u64::try_from(start.elapsed().as_micros()).unwrap_or(u64::MAX);
        self.0.record_latency(info.send, micros);
        verdict
    }

    fn hold_expired(&self, _handle: u64, _send: bool) {
        let mut s = self.0.lock();
        ProofMedia::fail(&mut s, "holdExpired");
    }
}

/// Negative control only (`voice-proof --control passthrough`): a hook that passes every frame
/// unchanged, i.e. what a missing or broken transform would put on the wire. A dilla receiver must
/// refuse all of it.
pub struct PassthroughControl;

impl FrameTransform for PassthroughControl {
    fn transform(&self, _info: &FrameInfo<'_>, _data: &[u8], _out: &mut Vec<u8>) -> Verdict {
        Verdict::PassUnchanged
    }

    fn hold_expired(&self, _handle: u64, _send: bool) {}
}

/// The KID of a received frame, for statistics only (never an authentication result).
pub fn peek_kid(codec: Codec, frame: &[u8]) -> Option<u64> {
    let (unescaped, prefix) = unescape_protected(codec, frame).ok()?;
    peek_kid_ctr(prefix, &unescaped)
        .ok()
        .map(|(k, _, _)| k.value())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::sframe::{FrameKey, VP8_KEY_PREFIX, open_frame};

    const BASE1: [u8; NK] = [7; NK];
    const BASE2: [u8; NK] = [9; NK];
    const DEV0: [u8; 16] = [1; 16];
    const DEV1: [u8; 16] = [2; 16];
    const ROSTER: [(u16, [u8; 16]); 2] = [(0, DEV0), (1, DEV1)];
    const OPUS: &[u8] = &[0xfc, 0xff, 0xfe, 0x01, 0x02, 0x03, 0x04];
    const VP8_KEY: &[u8] = &[
        0x50, 0x02, 0x00, 0x9d, 0x01, 0x2a, 0x80, 0x02, 0xe0, 0x01, 0xaa, 0xbb, 0xcc,
    ];

    fn info<'a>(send: bool, video: bool, track: &'a str, mime: &'a str) -> FrameInfo<'a> {
        FrameInfo {
            handle: 0,
            send,
            video,
            track_id: track,
            mime_type: mime,
            ssrc: 1234,
            spatial_index: video.then_some(0),
            key_frame: false,
        }
    }

    fn rust_sender() -> Hook {
        let m = ProofMedia::new(0);
        m.send_epoch(1, &BASE1).unwrap();
        m.bind_local("mic", Slot::Microphone);
        m.bind_local("cam", Slot::Camera);
        Hook(m)
    }

    fn rust_receiver() -> Hook {
        let m = ProofMedia::new(1);
        m.install_epoch(1, &BASE1, &ROSTER);
        Hook(m)
    }

    fn seal(h: &Hook, i: &FrameInfo<'_>, data: &[u8]) -> Vec<u8> {
        let mut out = Vec::new();
        assert_eq!(h.transform(i, data, &mut out), Verdict::Replace);
        out
    }

    #[test]
    fn a_mic_frame_is_byte_identical_to_the_cores_sender_and_opens_in_the_browser_ring() {
        let h = rust_sender();
        let sealed = seal(&h, &info(true, false, "mic", "audio/opus"), OPUS);
        // The wasm MediaSender is SframeSender: the same inputs give the same bytes.
        let mut reference = SframeSender::new(&BASE1, 0, 1, 1).unwrap();
        assert_eq!(
            sealed,
            reference
                .encrypt(Codec::Opus, Slot::Microphone, 0, OPUS)
                .unwrap()
        );
        // The wasm MediaReceiver is KeyRing with the track's device bound.
        let mut ring = KeyRing::new();
        ring.install_epoch(1, &BASE1, &ROSTER, Some(1), 0);
        let d = ring
            .decrypt(Codec::Opus, &sealed, Some(&DEV0), Slot::Microphone, 0)
            .unwrap();
        assert_eq!(d.frame, OPUS);
        assert_eq!(h.0.stats().encrypted.get("1"), Some(&1));
    }

    #[test]
    fn a_vp8_key_frame_keeps_its_ten_byte_prefix_clear() {
        let h = rust_sender();
        let sealed = seal(&h, &info(true, true, "cam", "video/VP8"), VP8_KEY);
        assert_eq!(&sealed[..VP8_KEY_PREFIX], &VP8_KEY[..VP8_KEY_PREFIX]);
        let key = FrameKey::derive(&BASE1, Kid::new(0, 1));
        let (_, ctr, plain) = open_frame(&key, VP8_KEY_PREFIX, &sealed).unwrap();
        assert_eq!(plain, VP8_KEY);
        assert_eq!((ctr.slot(), ctr.layer(), ctr.seq()), (1, 0, 0));
    }

    #[test]
    fn video_layers_are_allocated_first_seen_per_ssrc_and_spatial_index() {
        let h = rust_sender();
        let mut a = info(true, true, "cam", "video/VP8");
        let first = seal(&h, &a, VP8_KEY);
        a.ssrc = 99;
        let second = seal(&h, &a, VP8_KEY);
        let key = FrameKey::derive(&BASE1, Kid::new(0, 1));
        assert_eq!(
            open_frame(&key, VP8_KEY_PREFIX, &first).unwrap().1.layer(),
            0
        );
        assert_eq!(
            open_frame(&key, VP8_KEY_PREFIX, &second).unwrap().1.layer(),
            1
        );
    }

    #[test]
    fn a_zero_byte_opus_frame_passes_unchanged_and_spends_no_counter() {
        let h = rust_sender();
        let mut out = Vec::new();
        assert_eq!(
            h.transform(&info(true, false, "mic", "audio/opus"), &[], &mut out),
            Verdict::PassUnchanged
        );
        let sealed = seal(&h, &info(true, false, "mic", "audio/opus"), OPUS);
        let key = FrameKey::derive(&BASE1, Kid::new(0, 1));
        assert_eq!(open_frame(&key, 0, &sealed).unwrap().1.seq(), 0);
        assert_eq!(h.0.stats().empty_send, 1);
    }

    #[test]
    fn nothing_is_sent_in_the_clear() {
        let h = rust_sender();
        let mut out = Vec::new();
        // An unbound track, a codec with no prefix rule, a kind mismatch, an empty video frame, a
        // short VP8 key frame: all dropped.
        for (i, data) in [
            (info(true, false, "other", "audio/opus"), OPUS),
            (info(true, false, "mic", "audio/red"), OPUS),
            (info(true, true, "cam", "video/AV1"), VP8_KEY),
            (info(true, true, "mic", "video/VP8"), VP8_KEY),
            (info(true, false, "cam", "audio/opus"), OPUS),
            (info(true, true, "cam", "video/VP8"), &[][..]),
            (info(true, true, "cam", "video/VP8"), &VP8_KEY[..5]),
        ] {
            assert_eq!(h.transform(&i, data, &mut out), Verdict::Drop, "{i:?}");
        }
        assert!(out.is_empty());
        // No sender yet (no epoch): dropped too.
        let m = ProofMedia::new(0);
        m.bind_local("mic", Slot::Microphone);
        assert_eq!(
            Hook(m).transform(&info(true, false, "mic", "audio/opus"), OPUS, &mut out),
            Verdict::Drop
        );
    }

    #[test]
    fn the_receiver_holds_an_unbound_track_then_opens_it_once_bound() {
        let tx = rust_sender();
        let sealed = seal(&tx, &info(true, false, "mic", "audio/opus"), OPUS);
        let rx = rust_receiver();
        let i = info(false, false, "TR_a", "audio/opus");
        let mut out = Vec::new();
        assert_eq!(rx.transform(&i, &sealed, &mut out), Verdict::Hold);
        rx.0.bind_remote("TR_a", DEV0, Slot::Microphone);
        assert_eq!(rx.transform(&i, &sealed, &mut out), Verdict::Replace);
        assert_eq!(out, OPUS);
        assert_eq!(rx.0.stats().opened.get("1"), Some(&1));
    }

    #[test]
    fn the_receiver_binds_device_and_slot_and_refuses_replays() {
        let tx = rust_sender();
        let sealed = seal(&tx, &info(true, false, "mic", "audio/opus"), OPUS);
        let rx = rust_receiver();
        rx.0.bind_remote("wrong-device", DEV1, Slot::Microphone);
        rx.0.bind_remote("wrong-slot", DEV0, Slot::ScreenAudio);
        rx.0.bind_remote("ok", DEV0, Slot::Microphone);
        let mut out = Vec::new();
        assert_eq!(
            rx.transform(
                &info(false, false, "wrong-device", "audio/opus"),
                &sealed,
                &mut out
            ),
            Verdict::Drop
        );
        assert_eq!(
            rx.transform(
                &info(false, false, "wrong-slot", "audio/opus"),
                &sealed,
                &mut out
            ),
            Verdict::Drop
        );
        assert_eq!(
            rx.transform(&info(false, false, "ok", "audio/opus"), &sealed, &mut out),
            Verdict::Replace
        );
        assert_eq!(
            rx.transform(&info(false, false, "ok", "audio/opus"), &sealed, &mut out),
            Verdict::Drop
        );
        let failed = rx.0.stats().failed;
        // DEV1 is the receiver's own leaf in the roster, so the KID check refuses it as a sender mismatch.
        assert_eq!(failed.get("E_SFRAME_SENDER_MISMATCH"), Some(&1));
        assert_eq!(failed.get("E_SFRAME_SLOT_MISMATCH"), Some(&1));
        assert_eq!(failed.get("E_SFRAME_REPLAY"), Some(&1));
    }

    #[test]
    fn an_epoch_change_holds_until_installed_and_never_accepts_a_wrong_key() {
        let tx = rust_sender();
        let rx = rust_receiver();
        rx.0.bind_remote("TR_a", DEV0, Slot::Microphone);
        let i_tx = info(true, false, "mic", "audio/opus");
        let i_rx = info(false, false, "TR_a", "audio/opus");
        let mut out = Vec::new();
        let e1 = seal(&tx, &i_tx, OPUS);
        assert_eq!(rx.transform(&i_rx, &e1, &mut out), Verdict::Replace);
        tx.0.send_epoch(2, &BASE2).unwrap();
        let e2 = seal(&tx, &i_tx, OPUS);
        assert_eq!(tx.0.stats().encrypted.get("2"), Some(&1));
        // Epoch 2 not installed yet: held, not dropped.
        assert_eq!(rx.transform(&i_rx, &e2, &mut out), Verdict::Hold);
        // A receiver that installs epoch 2 under the wrong base key accepts none of its frames.
        let wrong = rust_receiver();
        wrong.0.bind_remote("TR_a", DEV0, Slot::Microphone);
        wrong.0.install_epoch(2, &[8; NK], &ROSTER);
        assert_eq!(wrong.transform(&i_rx, &e2, &mut out), Verdict::Drop);
        assert_eq!(wrong.0.stats().failed.get("E_SFRAME_AUTH"), Some(&1));
        assert_eq!(wrong.0.stats().total_opened(), 0);
        // The right key opens it.
        rx.0.install_epoch(2, &BASE2, &ROSTER);
        assert_eq!(rx.transform(&i_rx, &e2, &mut out), Verdict::Replace);
        assert_eq!(out, OPUS);
        assert_eq!(rx.0.stats().opened.get("2"), Some(&1));
    }

    #[test]
    fn a_zero_byte_audio_frame_passes_on_receive_even_unbound_but_empty_video_does_not() {
        let rx = rust_receiver();
        let mut out = Vec::new();
        assert_eq!(
            rx.transform(&info(false, false, "x", "audio/opus"), &[], &mut out),
            Verdict::PassUnchanged
        );
        assert_eq!(
            rx.transform(&info(false, true, "x", "video/VP8"), &[], &mut out),
            Verdict::Drop
        );
    }

    #[test]
    fn kid_hex_matches_the_browser_stats_keys() {
        assert_eq!(kid_hex(0, 1), "1");
        assert_eq!(kid_hex(1, 1), "101");
        assert_eq!(kid_hex(1, 2), "102");
        assert_eq!(
            peek_kid(
                Codec::Opus,
                &seal(
                    &rust_sender(),
                    &info(true, false, "mic", "audio/opus"),
                    OPUS
                )
            ),
            Some(1)
        );
    }
}
