//! Task 11: the desktop's call media.
//!
//! - Task 2's interop tests 1 to 3, here with keys a real `ClientCore` derives for a real call
//!   group (`call_media_epoch`) instead of fixed ones: alice is this desktop (`ClientCore`, through
//!   the C++ `DillaFrameTransformer` and this crate's router); bob is the web client's side, a
//!   member built on `DillaGroup` whose sender and receiver are `SframeSender` and `KeyRing`, the
//!   code `dilla-core-wasm`'s `MediaSender` and `MediaReceiver` wrap.
//! - `noise_modes_available`: off, standard and enhanced each process a noise fixture; enhanced
//!   (DeepFilterNet3) lowers it by at least 10 dB within the web client's per-frame budget.
//! - `desktop_and_web_in_one_call`: the desktop and the web client in one live call (see its
//!   comment; tests/live.rs runs two desktops through the SFU).

#[allow(dead_code, unused_imports, clippy::all)]
#[path = "../../../../core/dilla-core/tests/client_support/mod.rs"]
mod client_support;

use std::sync::{Mutex, MutexGuard};
use std::time::Instant;

use client_support::{CALL, Core, MediaEpochOut, RawPeer, call_raw_bob, decode_media_epoch};
use dilla_core::sframe::{Codec, KeyRing, NK, SframeSender, Slot};
use dilla_media::ffi::testing;
use dilla_media::noise::{ENHANCED_P99_BUDGET_MS, FRAME_SAMPLES, NoiseMode, NoiseProcessor, RATE};
use dilla_media::transform::{CallMedia, Router, kid_hex};
use dilla_media::{EpochFeed, EpochKeys, MediaEpoch};

static SERIAL: Mutex<()> = Mutex::new(());

fn serial() -> MutexGuard<'static, ()> {
    SERIAL.lock().unwrap_or_else(|p| p.into_inner())
}

/// 20 ms of a 440 Hz tone as one Opus-sized payload: the frames' content does not matter to the
/// transform, only that each is distinct.
fn payload(n: u32) -> Vec<u8> {
    let mut v = vec![0xfc, 0xff, 0xfe];
    v.extend(n.to_be_bytes());
    v.extend([0x44; 40]);
    v
}

/// Alice's media epoch, as her `ClientCore` hands it to the call (decoded `call_media_epoch`).
fn alice_epoch(alice: &Core) -> MediaEpoch {
    let m: MediaEpochOut =
        decode_media_epoch(&alice.core.call_media_epoch(&CALL).expect("gate passes"));
    MediaEpoch {
        epoch: m.epoch,
        base_key: zeroize::Zeroizing::new(m.base_key),
        roster: m
            .roster
            .iter()
            .map(|(leaf, device, _)| (u16::try_from(*leaf).unwrap(), *device))
            .collect(),
        own_leaf: u16::try_from(m.own_leaf).unwrap(),
    }
}

/// Bob's (the web client's) view of the same epoch, from his own MLS state.
struct Web {
    epoch: u64,
    base_key: [u8; NK],
    own_leaf: u16,
    roster: Vec<(u16, [u8; 16])>,
}

fn web_epoch(bob: &RawPeer) -> Web {
    let m = bob
        .group
        .as_ref()
        .unwrap()
        .media_epoch(&bob.provider)
        .unwrap();
    Web {
        epoch: m.epoch,
        base_key: *m.base_key,
        own_leaf: u16::try_from(m.own_leaf).unwrap(),
        roster: m
            .roster
            .iter()
            .map(|r| {
                (
                    u16::try_from(r.leaf_index).unwrap(),
                    *r.device_id.as_bytes(),
                )
            })
            .collect(),
    }
}

/// Alice's call: her `ClientCore`'s keys through an [`EpochFeed`] into a [`CallMedia`] the router
/// routes the real C++ transformers to.
struct Desktop {
    router: std::sync::Arc<Router>,
    media: std::sync::Arc<CallMedia>,
    feed: std::sync::Arc<EpochFeed>,
}

impl Desktop {
    fn join(alice: &Core) -> Self {
        let router = Router::get().expect("hook");
        let media = CallMedia::new();
        router.begin(media.clone()).expect("one call");
        let feed = EpochFeed::new();
        let m = media.clone();
        feed.on_epoch(Box::new(move |epoch, key, roster, own| {
            m.install_epoch(epoch, &key, &roster, own).expect("install");
        }));
        feed.publish(alice_epoch(alice));
        Desktop {
            router,
            media,
            feed,
        }
    }
}

impl Drop for Desktop {
    fn drop(&mut self) {
        self.router.end(&self.media);
    }
}

#[test]
fn rust_to_web_audio() {
    let _s = serial();
    let (_call, alice, bob, _) = call_raw_bob();
    let desk = Desktop::join(&alice);
    let a = alice_epoch(&alice);
    let web = web_epoch(&bob);
    assert_eq!(web.epoch, a.epoch);
    assert_eq!(
        &web.base_key, &*a.base_key,
        "both members derive one exporter key"
    );

    desk.media.expect_local("microphone", Slot::Microphone);
    let tx = testing::attach(true, false, "microphone");
    let mut ring = KeyRing::new();
    ring.install_epoch(web.epoch, &web.base_key, &web.roster, Some(web.own_leaf), 0);
    for n in 0..200u32 {
        testing::frame(tx, "audio/opus", &payload(n), false);
    }
    let sealed = testing::take_emitted(tx);
    assert_eq!(sealed.len(), 200);
    for (n, frame) in sealed.iter().enumerate() {
        assert_ne!(frame, &payload(n as u32), "never in the clear");
        let opened = ring
            .decrypt(Codec::Opus, frame, Some(&alice.device), Slot::Microphone, 0)
            .expect("the web receiver authenticates every frame as alice's");
        assert_eq!(opened.frame, payload(n as u32));
    }
    let stats = desk.media.stats();
    assert_eq!(
        stats.encrypted.get(&kid_hex(a.own_leaf, a.epoch)),
        Some(&200)
    );
    assert_eq!(stats.total_failed(), 0);
    testing::release(tx);
}

#[test]
fn web_to_rust_audio() {
    let _s = serial();
    let (_call, alice, bob, _) = call_raw_bob();
    let desk = Desktop::join(&alice);
    let web = web_epoch(&bob);
    let mut sender = SframeSender::new(&web.base_key, web.own_leaf, web.epoch, web.epoch).unwrap();
    let rx = testing::attach(false, false, "TR_bob_mic");
    desk.media
        .bind_remote(rx, bob.device, Slot::Microphone)
        .unwrap();
    for n in 0..150u32 {
        let sealed = sender
            .encrypt(Codec::Opus, Slot::Microphone, 0, &payload(n))
            .unwrap();
        testing::frame(rx, "audio/opus", &sealed, false);
    }
    let opened = testing::take_emitted(rx);
    assert_eq!(opened.len(), 150, "at least 100 opened, none failed");
    assert!(
        opened
            .iter()
            .enumerate()
            .all(|(n, f)| f == &payload(n as u32))
    );
    let stats = desk.media.stats();
    assert_eq!(stats.total_failed(), 0, "{stats:?}");
    assert_eq!(stats.verified.get(&hex(&bob.device)), Some(&150));
    testing::release(rx);
}

#[test]
fn epoch_change_mid_stream() {
    let _s = serial();
    let (mut call, mut alice, mut bob, _) = call_raw_bob();
    let desk = Desktop::join(&alice);
    desk.media.expect_local("microphone", Slot::Microphone);
    let tx = testing::attach(true, false, "microphone");
    let rx = testing::attach(false, false, "TR_bob_mic");
    desk.media
        .bind_remote(rx, bob.device, Slot::Microphone)
        .unwrap();
    let e1 = web_epoch(&bob);
    let alice_device = alice.device;
    let mut web_tx = SframeSender::new(&e1.base_key, e1.own_leaf, e1.epoch, e1.epoch).unwrap();
    let mut web_rx = KeyRing::new();
    web_rx.install_epoch(e1.epoch, &e1.base_key, &e1.roster, Some(e1.own_leaf), 0);

    let mut sent = 0u32;
    let mut stream = |web_tx: &mut SframeSender, web_rx: &mut KeyRing, n: u32| -> (usize, usize) {
        let mut desk_opened = 0;
        let mut web_opened = 0;
        for _ in 0..n {
            testing::frame(tx, "audio/opus", &payload(sent), false);
            let sealed = web_tx
                .encrypt(Codec::Opus, Slot::Microphone, 0, &payload(sent))
                .unwrap();
            testing::frame(rx, "audio/opus", &sealed, false);
            sent += 1;
        }
        desk_opened += testing::take_emitted(rx).len();
        for f in testing::take_emitted(tx) {
            if web_rx
                .decrypt(Codec::Opus, &f, Some(&alice_device), Slot::Microphone, 0)
                .is_ok()
            {
                web_opened += 1;
            }
        }
        (desk_opened, web_opened)
    };
    assert_eq!(stream(&mut web_tx, &mut web_rx, 100), (100, 100));

    // Bob commits a new epoch (an UpdatePath). The web side switches first: its sender seals under
    // epoch 2 before the desktop has processed the commit, so the desktop holds those frames.
    bob.self_update(&mut call);
    let e2 = web_epoch(&bob);
    assert_eq!(e2.epoch, e1.epoch + 1);
    web_rx.install_epoch(e2.epoch, &e2.base_key, &e2.roster, Some(e2.own_leaf), 1);
    web_tx.rekey(&e2.base_key, e2.own_leaf, e2.epoch);
    let (held_phase, _) = stream(&mut web_tx, &mut web_rx, 20);
    assert_eq!(
        held_phase, 0,
        "epoch 2 not installed on the desktop yet: held, not dropped"
    );
    assert_eq!(testing::held(rx), 20);

    // Alice's ClientCore processes the commit; its new epoch reaches the call: the hold drains.
    alice.sync(&call);
    let a2 = alice_epoch(&alice);
    assert_eq!(a2.epoch, e2.epoch);
    desk.feed.publish(a2);
    assert_eq!(testing::held(rx), 0);
    assert_eq!(
        testing::take_emitted(rx).len(),
        20,
        "released in order once authenticated"
    );
    assert_eq!(desk.media.sending_epoch(), Some(e2.epoch));
    assert_eq!(stream(&mut web_tx, &mut web_rx, 100), (100, 100));

    let stats = desk.media.stats();
    assert_eq!(
        stats.total_failed(),
        0,
        "no frame dropped across the change: {stats:?}"
    );
    // 0 frames accepted under a wrong key: a receiver holding epoch 2 under another key opens none.
    let mut wrong = KeyRing::new();
    wrong.install_epoch(e2.epoch, &[0x5a; NK], &e2.roster, Some(e2.own_leaf), 0);
    testing::frame(tx, "audio/opus", &payload(9_999), false);
    let last = testing::take_emitted(tx).pop().unwrap();
    assert_eq!(
        wrong
            .decrypt(Codec::Opus, &last, Some(&alice.device), Slot::Microphone, 0)
            .map(|_| ())
            .unwrap_err()
            .code(),
        "E_SFRAME_AUTH"
    );
    testing::release(tx);
    testing::release(rx);
}

fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

/// A deterministic broadband noise fixture: 4 s of white noise at about -26 dBFS RMS.
fn noise_fixture() -> Vec<i16> {
    let mut x: u64 = 0x9e37_79b9_7f4a_7c15;
    (0..RATE as usize * 4)
        .map(|_| {
            x ^= x << 13;
            x ^= x >> 7;
            x ^= x << 17;
            ((x >> 48) as i16) / 12
        })
        .collect()
}

fn rms_db(samples: &[i16]) -> f64 {
    let ms = samples
        .iter()
        .map(|s| (f64::from(*s) / 32768.0).powi(2))
        .sum::<f64>()
        / samples.len().max(1) as f64;
    10.0 * ms.max(1e-12).log10()
}

#[test]
fn noise_modes_available() {
    let input = noise_fixture();
    // Measured over the last 2 s, like the output.
    let input_db = rms_db(&input[input.len() / 2..]);
    let mut out_db = Vec::new();
    for mode in NoiseMode::ALL {
        let mut p = NoiseProcessor::new(mode).expect("every mode is available");
        let mut out = Vec::with_capacity(input.len());
        let mut times = Vec::new();
        for chunk in input.chunks(FRAME_SAMPLES) {
            let mut frame = chunk.to_vec();
            let start = Instant::now();
            p.process(&mut frame).expect("processes");
            times.push(start.elapsed().as_secs_f64() * 1_000.0);
            out.extend(frame);
        }
        // The last 2 s: past the enhanced model's 30 ms delay and every adaptation.
        let db = rms_db(&out[out.len() / 2..]);
        times.sort_by(f64::total_cmp);
        let p99 = times[(times.len() - 1) * 99 / 100];
        println!(
            "NOISE {mode:?} ({}): in {input_db:.1} dBFS, out {db:.1} dBFS, p99 {p99:.2} ms/frame",
            mode.processor()
        );
        if mode == NoiseMode::Enhanced && !cfg!(debug_assertions) {
            assert!(
                p99 < ENHANCED_P99_BUDGET_MS,
                "p99 {p99:.2} ms over the 8 ms budget"
            );
        }
        if mode == NoiseMode::Off {
            assert_eq!(out, input, "off passes the samples unchanged");
        }
        out_db.push(db);
    }
    assert!(out_db[1] < input_db, "standard suppresses some noise");
    assert!(
        input_db - out_db[2] >= 10.0,
        "enhanced lowers the noise by {:.1} dB, not 10",
        input_db - out_db[2]
    );
    // A frame of the wrong size is refused and silenced, never passed through.
    let mut p = NoiseProcessor::new(NoiseMode::Enhanced).unwrap();
    let mut short = vec![1000i16; FRAME_SAMPLES - 1];
    assert!(p.process(&mut short).is_err());
    assert!(short.iter().all(|s| *s == 0));
}

/// The desktop and the web client in one call: browser/desktop-web-call.spec.ts, through the e2e
/// media suite's own host (dilla-testhost with its in-process LiveKit, the packages/media harness).
/// A desktop `Call` (examples/call-peer.rs) and the web client's real `DillaE2EEManager`, media
/// worker and dilla-core-wasm hear each other's tones under a real call group's keys (the desktop's
/// from its `ClientCore`), then change epoch mid-call with no frame dropped or accepted under a
/// wrong key. dillad's call routes are not in this path: the desktop has no signed-in sync engine
/// yet (Task 9), so the token comes from the host's debug route; `dilla_client::calls` covers the
/// routes. Needs Node with the repository's dependencies, the packages/media WASM build, Go and the
/// WASI core (the media suite's prerequisites).
#[test]
#[ignore = "needs the e2e media suite's prerequisites (npm ci, packages/media wasm, Go, WASI core)"]
fn desktop_and_web_in_one_call() {
    let root = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("../../..");
    let status = std::process::Command::new("npx")
        .args([
            "playwright",
            "test",
            "-c",
            "clients/desktop/dilla-media/browser/playwright.config.ts",
        ])
        .current_dir(root)
        .status()
        .expect("npx runs");
    assert!(status.success(), "browser/desktop-web-call.spec.ts failed");
}
