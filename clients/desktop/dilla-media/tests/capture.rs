//! Parity plan Package 1: a share's source through the real media path (beta.5 `screen-capture.spec.ts`).
//!
//! - A window share (a screen-slot source with no sound) reaches a viewer through the real SFU, sealed under the
//!   sharer's key and opened as the sharer's verified screen, and nothing else of it travels.
//! - A source that closes ends its track: the viewer's stream goes, the sharer is told.
//! - An unbound screen sender sends nothing while its input stays live (synthetic frames, no SFU).
//!
//! The live tests need dilla-testhost and the WASI core, as tests/live.rs:
//! `cargo test -p dilla-media --test capture -- --ignored`.

#[allow(dead_code, unused_imports, clippy::all)]
#[path = "../../../../core/dilla-core/tests/client_support/mod.rs"]
mod client_support;

mod support {
    pub mod live;
}

use std::time::{Duration, Instant};

use client_support::{CALL, call_raw_bob};
use dilla_core::sframe::{NK, Slot};
use dilla_media::transform::{CallMedia, Router};
use serde_json::Value;
use support::live::*;
use webrtc_sys::dilla_transform_testing as testing;

/// Waits until `peer`'s `videos` holds `key` with frames that are the test pattern, and returns that stream.
fn sees(peer: &mut Peer, key: &str, timeout: Duration) -> Value {
    let deadline = Instant::now() + timeout;
    loop {
        peer.send("videos");
        let v = peer.next("videos", Duration::from_secs(5));
        let stream = &v["videos"][key];
        if stream["frames"].as_u64().is_some_and(|n| n > 5) && stream["bars"] == true {
            return v;
        }
        assert!(Instant::now() < deadline, "no {key} stream: {v}");
        std::thread::sleep(Duration::from_millis(250));
    }
}

/// screen-capture.spec.ts "a selected native application window stays video-only and encrypted through the real media
/// worker": alice shares a window (a screen-slot source that has no sound). Bob sees exactly one stream of hers, her
/// screen, as the pattern she sent, every frame of it opened under her key id with nothing refused; alice's sender sealed
/// what went out, and it went out as VP8.
#[test]
#[ignore = "needs dilla-testhost (Go) and the WASI core"]
fn a_window_source_is_video_only_and_encrypted() {
    let host = start_host();
    let (_call, alice, bob, _) = call_raw_bob();
    let (a_dev, b_dev) = (hex(&alice.device), hex(&bob.device));
    let room = hex(&CALL);
    let (url, a_token) = token(&host.control, &room, &a_dev);
    let (_, b_token) = token(&host.control, &room, &b_dev);
    let mut a = Peer::start(&url, &a_token, 0);
    let mut b = Peer::start(&url, &b_token, 0);
    let e = alice_epoch(&alice);
    a.epoch(&e);
    b.epoch(&bob_epoch(&bob));
    a.next("connected", Duration::from_secs(30));
    b.next("connected", Duration::from_secs(30));

    a.send("share window");
    assert_eq!(
        a.next("share", Duration::from_secs(15))["error"],
        Value::Null
    );
    let seen = sees(&mut b, &format!("{a_dev}:Screen"), Duration::from_secs(30));
    let streams = seen["videos"].as_object().unwrap();
    assert_eq!(
        streams.keys().collect::<Vec<_>>(),
        vec![&format!("{a_dev}:Screen")],
        "one stream: the window, in the screen slot ({seen})"
    );
    let a_kid = format!("{:x}", (e.own_leaf << 8) | (e.epoch & 0xff));
    let (sa, sb) = (a.stats(), b.stats());
    assert!(
        sa["media"]["encrypted"][&a_kid].as_u64().unwrap_or(0) > 5,
        "{sa}"
    );
    assert!(opened(&sb, &a_kid) > 5, "{sb}");
    assert_eq!(failed(&sb), 0, "{sb}");
    assert_eq!(failed(&sa), 0, "{sa}");
    // Video only: one outbound video stream (the window), as VP8; a window source carries no sound of its own.
    a.send("sent");
    let sent = a.next("sent", Duration::from_secs(5));
    let video = sent["video"].as_array().unwrap();
    assert_eq!(video.len(), 1, "{sent}");
    assert_eq!(video[0][0], "video/VP8", "{sent}");
    assert!(video[0][1].as_u64().unwrap() > 0, "{sent}");
}

/// macOS CI: a real screen share from one macOS client to another. Alice shares the main display, captured through
/// ScreenCaptureKit (call-peer `share display`: its picker off, needs Screen Recording, which the runner grants the
/// shell); bob sees her screen stream move, opened under her key, with nothing refused.
#[cfg(target_os = "macos")]
#[test]
#[ignore = "needs dilla-testhost (Go) and the WASI core"]
fn a_real_display_share_reaches_a_viewer() {
    let host = start_host();
    let (_call, alice, bob, _) = call_raw_bob();
    let (a_dev, b_dev) = (hex(&alice.device), hex(&bob.device));
    let room = hex(&CALL);
    let (url, a_token) = token(&host.control, &room, &a_dev);
    let (_, b_token) = token(&host.control, &room, &b_dev);
    let mut a = Peer::start(&url, &a_token, 0);
    let mut b = Peer::start(&url, &b_token, 0);
    let e = alice_epoch(&alice);
    a.epoch(&e);
    b.epoch(&bob_epoch(&bob));
    a.next("connected", Duration::from_secs(30));
    b.next("connected", Duration::from_secs(30));

    a.send("share display");
    let shared = a.next("share", Duration::from_secs(30));
    assert_eq!(shared["error"], Value::Null, "the main display: {shared}");
    let key = format!("{a_dev}:Screen");
    let deadline = Instant::now() + Duration::from_secs(30);
    let seen = loop {
        b.send("videos");
        let v = b.next("videos", Duration::from_secs(5));
        if v["videos"][&key]["frames"].as_u64().is_some_and(|n| n > 5) {
            break v;
        }
        assert!(Instant::now() < deadline, "no {key} stream: {v}");
        std::thread::sleep(Duration::from_millis(250));
    };
    let streams = seen["videos"].as_object().unwrap();
    assert_eq!(streams.keys().collect::<Vec<_>>(), vec![&key], "{seen}");
    let a_kid = format!("{:x}", (e.own_leaf << 8) | (e.epoch & 0xff));
    let (sa, sb) = (a.stats(), b.stats());
    assert!(
        sa["media"]["encrypted"][&a_kid].as_u64().unwrap_or(0) > 5,
        "{sa}"
    );
    assert!(opened(&sb, &a_kid) > 5, "{sb}");
    assert_eq!(failed(&sb), 0, "{sb}");
}

/// screen-capture.spec.ts "closing the selected source ends its consumer track and destroys the broker": alice's
/// source closes after 45 frames (3 s at 15 fps). Her call says the share ended, and bob's stream of it goes.
#[test]
#[ignore = "needs dilla-testhost (Go) and the WASI core"]
fn a_closed_source_ends_the_track() {
    let host = start_host();
    let (_call, alice, bob, _) = call_raw_bob();
    let (a_dev, b_dev) = (hex(&alice.device), hex(&bob.device));
    let room = hex(&CALL);
    let (url, a_token) = token(&host.control, &room, &a_dev);
    let (_, b_token) = token(&host.control, &room, &b_dev);
    let mut a = Peer::start(&url, &a_token, 0);
    let mut b = Peer::start(&url, &b_token, 0);
    a.epoch(&alice_epoch(&alice));
    b.epoch(&bob_epoch(&bob));
    a.next("connected", Duration::from_secs(30));
    b.next("connected", Duration::from_secs(30));

    a.send("share screen 45");
    assert_eq!(
        a.next("share", Duration::from_secs(15))["error"],
        Value::Null
    );
    let key = format!("{a_dev}:Screen");
    sees(&mut b, &key, Duration::from_secs(30));
    let deadline = Instant::now() + Duration::from_secs(20);
    loop {
        a.send("ended");
        let ended = a.next("shares_ended", Duration::from_secs(5));
        if ended["kinds"].as_array().is_some_and(|k| !k.is_empty()) {
            assert_eq!(ended["kinds"], serde_json::json!(["Screen"]));
            break;
        }
        assert!(Instant::now() < deadline, "the share never ended");
        std::thread::sleep(Duration::from_millis(250));
    }
    let deadline = Instant::now() + Duration::from_secs(20);
    loop {
        b.send("videos");
        let v = b.next("videos", Duration::from_secs(5));
        if v["videos"][&key].is_null() {
            break;
        }
        assert!(Instant::now() < deadline, "the viewer's stream stayed: {v}");
        std::thread::sleep(Duration::from_millis(250));
    }
    // Nothing is sent after the end (the statistics keep the stopped sender's total: it no longer grows).
    let mut total = || {
        a.send("sent");
        let sent = a.next("sent", Duration::from_secs(5));
        sent["video"]
            .as_array()
            .map_or(0, |v| v.iter().filter_map(|s| s[1].as_u64()).sum::<u64>())
    };
    let before = total();
    std::thread::sleep(Duration::from_secs(2));
    assert_eq!(total(), before, "nothing is sent after the end");
}

/// screen-capture.spec.ts "an unbound native screen source is blocked while its real input remains live": a screen
/// sender that is not bound to this call's screen slot (its track was never announced) drops every frame its live input
/// gives it; none leaves, sealed or not.
#[test]
fn an_unbound_screen_source_sends_nothing() {
    let router = Router::get().expect("hook");
    let media = CallMedia::new();
    router.begin(media.clone()).expect("no other call");
    media
        .install_epoch(3, &[0x44; NK], &[(0, [0xaa; 16]), (1, [0xbb; 16])], 0)
        .expect("keys");
    // The call expects its own screen track; this sender is another one.
    media.expect_local("screen-announced", Slot::ScreenVideo);
    let tx = testing::attach(true, true, "screen-unannounced");
    let vp8: &[u8] = &[
        0x50, 0x02, 0x00, 0x9d, 0x01, 0x2a, 0x80, 0x02, 0xe0, 0x01, 0xaa,
    ];
    for n in 0..30 {
        testing::frame(tx, "video/VP8", vp8, n % 10 == 0);
    }
    assert_eq!(testing::emitted_count(tx), 0, "nothing left");
    assert!(testing::take_emitted(tx).is_empty());
    assert_eq!(media.stats().failed.get("unbound"), Some(&30));
    assert!(media.stats().encrypted.is_empty());
    testing::release(tx);
    router.end(&media);
}
