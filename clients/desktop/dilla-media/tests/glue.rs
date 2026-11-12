//! The transform behind the real C++ `DillaFrameTransformer` (synthetic frames, no PeerConnection
//! needed for the FIFO), with this crate's router installed as the process hook: the binding
//! requirements of Task 11.
//!
//! - a receiver is bound per transformer handle, never by its track id; a second bind is refused;
//! - the hold drains when a key is installed, when a track is bound, and on the hook's own timer
//!   (an expired hold is dropped without a later frame), never released unauthenticated;
//! - a frame ending in the room's server-injected-frame trailer is dropped before any parse;
//! - a dropped video frame asks for a key frame, at most every 500 ms, until a decrypted key frame;
//! - a sender's transformer is never replaced: `set_track` to another track is refused.

use std::sync::{Arc, Mutex, MutexGuard};
use std::time::{Duration, Instant};

use dilla_core::sframe::{Codec, KeyRing, NK, SframeSender, Slot};
use dilla_media::MediaError;
use dilla_media::ffi::testing;
use dilla_media::transform::{CallMedia, Router};

const EPOCH: u64 = 4;
const BASE: [u8; NK] = [7; NK];
const BASE2: [u8; NK] = [9; NK];
const ME: [u8; 16] = [0xaa; 16];
const PEER: [u8; 16] = [0xbb; 16];
const ROSTER: [(u16, [u8; 16]); 2] = [(0, ME), (1, PEER)];
const OPUS: &[u8] = &[0xfc, 0xff, 0xfe, 0x01, 0x02, 0x03, 0x04];
const VP8_KEY: &[u8] = &[
    0x50, 0x02, 0x00, 0x9d, 0x01, 0x2a, 0x80, 0x02, 0xe0, 0x01, 0xaa, 0xbb, 0xcc,
];
const VP8_DELTA: &[u8] = &[0x31, 0x02, 0x00, 0x11, 0x22, 0x33, 0x44];

static SERIAL: Mutex<()> = Mutex::new(());

/// One call at a time: the router has one current call.
struct Fixture {
    _serial: MutexGuard<'static, ()>,
    router: Arc<Router>,
    media: Arc<CallMedia>,
}

impl Drop for Fixture {
    fn drop(&mut self) {
        self.router.end(&self.media);
    }
}

fn call() -> Fixture {
    let serial = SERIAL.lock().unwrap_or_else(|p| p.into_inner());
    let router = Router::get().expect("hook");
    let media = CallMedia::new();
    router.begin(media.clone()).expect("no other call");
    media.install_epoch(EPOCH, &BASE, &ROSTER, 0).expect("keys");
    Fixture {
        _serial: serial,
        router,
        media,
    }
}

/// The peer's own sender (leaf 1): what the web client's MediaSender produces.
fn peer_sender(epoch: u64, base: &[u8; NK]) -> SframeSender {
    SframeSender::new(base, 1, epoch, epoch).unwrap()
}

fn wait_until(what: &str, timeout: Duration, mut done: impl FnMut() -> bool) {
    let deadline = Instant::now() + timeout;
    while !done() {
        assert!(Instant::now() < deadline, "timed out waiting for {what}");
        std::thread::sleep(Duration::from_millis(10));
    }
}

#[test]
fn a_held_frame_drains_when_its_track_is_bound_and_when_its_key_is_installed() {
    let f = call();
    let rx = testing::attach(false, false, "TR_audio");
    let mut peer = peer_sender(EPOCH, &BASE);
    let sealed = peer
        .encrypt(Codec::Opus, Slot::Microphone, 0, OPUS)
        .unwrap();
    testing::frame(rx, "audio/opus", &sealed, false);
    assert_eq!(testing::held(rx), 1, "unbound: held");
    assert!(testing::take_emitted(rx).is_empty());

    f.media.bind_remote(rx, PEER, Slot::Microphone).unwrap();
    assert_eq!(testing::held(rx), 0, "binding drains at once");
    assert_eq!(testing::take_emitted(rx), vec![OPUS.to_vec()]);

    // The peer moves to the next epoch before this device has its key: held, then released by
    // the install itself, without any later frame.
    peer.rekey(&BASE2, 1, EPOCH + 1);
    let next = peer
        .encrypt(Codec::Opus, Slot::Microphone, 0, OPUS)
        .unwrap();
    testing::frame(rx, "audio/opus", &next, false);
    assert_eq!(testing::held(rx), 1);
    f.media
        .install_epoch(EPOCH + 1, &BASE2, &ROSTER, 0)
        .unwrap();
    assert_eq!(testing::held(rx), 0);
    assert_eq!(testing::take_emitted(rx), vec![OPUS.to_vec()]);
    assert_eq!(f.media.stats().opened.get("105"), Some(&1));
    testing::release(rx);
}

#[test]
fn a_held_frame_expires_on_the_timer_without_a_later_frame_and_is_never_released() {
    let f = call();
    let rx = testing::attach(false, false, "TR_quiet");
    f.media.bind_remote(rx, PEER, Slot::Microphone).unwrap();
    // A key this device will never get.
    let mut peer = peer_sender(EPOCH + 2, &BASE2);
    let sealed = peer
        .encrypt(Codec::Opus, Slot::Microphone, 0, OPUS)
        .unwrap();
    testing::frame(rx, "audio/opus", &sealed, false);
    assert_eq!(testing::held(rx), 1);
    // RED (Task 2's glue): the FIFO only ran on the next frame, so this stayed held for ever.
    wait_until(
        "the timer to expire the hold",
        Duration::from_secs(4),
        || testing::held(rx) == 0,
    );
    assert!(
        testing::take_emitted(rx).is_empty(),
        "never released unauthenticated"
    );
    assert_eq!(f.media.stats().failed.get("holdExpired"), Some(&1));
    testing::release(rx);
}

#[test]
fn a_receiver_is_bound_by_its_handle_never_by_its_track_id_and_only_once() {
    let f = call();
    // Two receivers whose SFU-chosen track ids collide.
    let a = testing::attach(false, false, "TR_same");
    let b = testing::attach(false, false, "TR_same");
    assert_ne!(a, b);
    f.media.bind_remote(a, PEER, Slot::Microphone).unwrap();
    assert!(matches!(
        f.media.bind_remote(a, ME, Slot::Microphone),
        Err(MediaError::AlreadyBound(h)) if h == a
    ));
    let mut peer = peer_sender(EPOCH, &BASE);
    let one = peer
        .encrypt(Codec::Opus, Slot::Microphone, 0, OPUS)
        .unwrap();
    let two = peer
        .encrypt(Codec::Opus, Slot::Microphone, 0, OPUS)
        .unwrap();
    testing::frame(a, "audio/opus", &one, false);
    testing::frame(b, "audio/opus", &two, false);
    assert_eq!(testing::take_emitted(a), vec![OPUS.to_vec()]);
    assert!(
        testing::take_emitted(b).is_empty(),
        "the other handle inherits nothing"
    );
    assert_eq!(testing::held(b), 1);
    testing::release(a);
    testing::release(b);
}

#[test]
fn a_frame_ending_in_the_sif_trailer_is_dropped_before_any_header_is_parsed() {
    let f = call();
    let trailer = b"LKROOM-SIF-TRAILER-0123456789abcdef0123456789".to_vec();
    f.media.set_sif_trailer(&trailer);
    let rx = testing::attach(false, false, "TR_sif");
    // Unbound on purpose: a SIF frame is dropped even before the binding is known.
    // LiveKit's injected Opus silence: f8 ff fe, zeros, then the trailer.
    let mut injected = vec![0xf8, 0xff, 0xfe];
    injected.extend([0u8; 77]);
    injected.extend(&trailer);
    testing::frame(rx, "audio/opus", &injected, false);
    assert_eq!(testing::held(rx), 0, "not held");
    assert!(testing::take_emitted(rx).is_empty());
    let stats = f.media.stats();
    assert_eq!(stats.failed.get("sif"), Some(&1));
    assert_eq!(stats.failed.len(), 1, "no parse failure counted: {stats:?}");
    assert_eq!(stats.key_frame_requests, 0);
    testing::release(rx);
}

#[test]
fn a_dropped_video_frame_asks_for_key_frames_until_a_decrypted_key_frame_passes() {
    let f = call();
    let rx = testing::attach(false, true, "TR_cam");
    f.media.bind_remote(rx, PEER, Slot::Camera).unwrap();
    let mut peer = peer_sender(EPOCH, &BASE);
    let mut forged = SframeSender::new(&[3; NK], 1, EPOCH, EPOCH).unwrap();
    let bad = forged
        .encrypt(Codec::Vp8, Slot::Camera, 0, VP8_DELTA)
        .unwrap();
    testing::frame(rx, "video/VP8", &bad, false);
    assert!(testing::take_emitted(rx).is_empty());
    assert_eq!(f.media.stats().failed.get("E_SFRAME_AUTH"), Some(&1));
    // The hook's timer sends the first request at once and repeats it at most every 500 ms.
    wait_until("a key-frame request", Duration::from_secs(2), || {
        f.media.stats().key_frame_requests >= 1
    });
    std::thread::sleep(Duration::from_millis(1_200));
    let repeated = f.media.stats().key_frame_requests;
    assert!((2..=4).contains(&repeated), "{repeated} requests in ~1.2 s");
    // A decrypted delta frame does not end the episode; a decrypted key frame does.
    let delta = peer
        .encrypt(Codec::Vp8, Slot::Camera, 0, VP8_DELTA)
        .unwrap();
    testing::frame(rx, "video/VP8", &delta, false);
    let key = peer.encrypt(Codec::Vp8, Slot::Camera, 0, VP8_KEY).unwrap();
    testing::frame(rx, "video/VP8", &key, true);
    assert_eq!(testing::take_emitted(rx).len(), 2);
    let after = f.media.stats().key_frame_requests;
    std::thread::sleep(Duration::from_millis(1_100));
    assert_eq!(
        f.media.stats().key_frame_requests,
        after,
        "no request after the key frame"
    );
    // A replay of a frame is refused without asking for a key frame.
    testing::frame(rx, "video/VP8", &key, true);
    std::thread::sleep(Duration::from_millis(100));
    assert_eq!(f.media.stats().failed.get("E_SFRAME_REPLAY"), Some(&1));
    assert_eq!(f.media.stats().key_frame_requests, after);
    testing::release(rx);
}

#[test]
fn the_microphone_sender_is_bound_as_it_attaches_and_an_unbound_sender_sends_nothing() {
    let f = call();
    f.media.expect_local("mic-track", Slot::Microphone);
    let tx = testing::attach(true, false, "mic-track");
    let other = testing::attach(true, false, "someone-elses-track");
    testing::frame(tx, "audio/opus", OPUS, false);
    testing::frame(other, "audio/opus", OPUS, false);
    let sealed = testing::take_emitted(tx);
    assert_eq!(sealed.len(), 1);
    assert!(
        testing::take_emitted(other).is_empty(),
        "unbound: dropped, never sent"
    );
    // What the web client's MediaReceiver (KeyRing) makes of it, as the peer at leaf 1.
    let mut ring = KeyRing::new();
    ring.install_epoch(EPOCH, &BASE, &ROSTER, Some(1), 0);
    let opened = ring
        .decrypt(Codec::Opus, &sealed[0], Some(&ME), Slot::Microphone, 0)
        .unwrap();
    assert_eq!(opened.frame, OPUS);
    assert_eq!(f.media.stats().failed.get("unbound"), Some(&1));
    testing::release(tx);
    testing::release(other);
}

#[test]
fn after_the_call_ends_no_frame_is_sent_or_decoded() {
    let f = call();
    f.media.expect_local("mic-after", Slot::Microphone);
    let tx = testing::attach(true, false, "mic-after");
    let rx = testing::attach(false, false, "TR_after");
    f.media.bind_remote(rx, PEER, Slot::Microphone).unwrap();
    f.router.end(&f.media);
    testing::frame(tx, "audio/opus", OPUS, false);
    let sealed = peer_sender(EPOCH, &BASE)
        .encrypt(Codec::Opus, Slot::Microphone, 0, OPUS)
        .unwrap();
    testing::frame(rx, "audio/opus", &sealed, false);
    assert!(testing::take_emitted(tx).is_empty());
    assert!(testing::take_emitted(rx).is_empty());
    assert!(matches!(
        f.media.install_epoch(EPOCH + 1, &BASE2, &ROSTER, 0),
        Err(MediaError::Closed)
    ));
    testing::release(tx);
    testing::release(rx);
}

#[test]
fn a_sender_keeps_its_transformer_and_refuses_a_track_it_was_not_attached_for() {
    use livekit::webrtc::audio_source::AudioSourceOptions;
    use livekit::webrtc::audio_source::native::NativeAudioSource;
    use livekit::webrtc::media_stream_track::MediaStreamTrack;
    use livekit::webrtc::peer_connection_factory::native::PeerConnectionFactoryExt;
    use livekit::webrtc::peer_connection_factory::{PeerConnectionFactory, RtcConfiguration};

    let _f = call();
    let factory = PeerConnectionFactory::default();
    let pc = factory
        .create_peer_connection(RtcConfiguration::default())
        .expect("the hook is installed");
    let source = || NativeAudioSource::new(AudioSourceOptions::default(), 48_000, 1, 100);
    let first = factory.create_audio_track("first", source());
    let second = factory.create_audio_track("second", source());
    let sender = pc
        .add_track(MediaStreamTrack::Audio(first.clone()), &["s"])
        .expect("add_track");
    // RED (Task 2's glue): set_track attached a second transformer, which libwebrtc's ChannelSend
    // takes as a second delegate.
    assert!(
        sender
            .set_track(Some(MediaStreamTrack::Audio(second)))
            .is_err(),
        "another track would need another transformer"
    );
    assert!(sender.set_track(None).is_ok(), "stopping sends nothing");
    assert!(
        sender
            .set_track(Some(MediaStreamTrack::Audio(first)))
            .is_ok(),
        "its own track comes back under the same transformer"
    );
    pc.close();
}
