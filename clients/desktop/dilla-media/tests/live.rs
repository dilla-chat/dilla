//! Live calls through a real LiveKit SFU (dilla-testhost's in-process one, as the e2e media suite
//! runs it), each participant a desktop `dilla_media::Call` in its own process
//! (examples/call-peer.rs; the hook is process-wide), with the keys a real `ClientCore` and a real
//! `DillaGroup` member derive for one call group. Covers what the synthetic-frame tests cannot:
//! LiveKit's publish and subscribe path through the vendored glue, the receiver bound by its own
//! track object in `OnTrack`, the room's SIF trailer arriving from the join response, and an epoch
//! change mid-call.
//!
//! Needs Go (to build `cmd/dilla-testhost`, or `DILLA_TESTHOST`) and the WASI core
//! (`internal/mlswasi/testdata/dilla_core_wasi.wasm`, or `DILLA_CORE_WASI`):
//! `cargo test -p dilla-media --test live -- --ignored`.

#[allow(dead_code, unused_imports, clippy::all)]
#[path = "../../../../core/dilla-core/tests/client_support/mod.rs"]
mod client_support;

mod support {
    pub mod live;
}

use std::time::{Duration, Instant};

use client_support::{CALL, call_raw_bob};
use serde_json::Value;
use support::live::*;

#[test]
#[ignore = "needs dilla-testhost (Go) and the WASI core"]
fn desktop_to_desktop_through_the_sfu_with_client_core_keys_and_an_epoch_change() {
    let host = start_host();
    let (mut call, mut alice, mut bob, _) = call_raw_bob();
    let (a_dev, b_dev) = (hex(&alice.device), hex(&bob.device));
    let room = hex(&CALL);
    let (url, a_token) = token(&host.control, &room, &a_dev);
    let (_, b_token) = token(&host.control, &room, &b_dev);

    let mut a = Peer::start(&url, &a_token, 440);
    let mut b = Peer::start(&url, &b_token, 1000);
    a.epoch(&alice_epoch(&alice));
    b.epoch(&bob_epoch(&bob));
    assert_eq!(
        a.next("connected", Duration::from_secs(30))["identity"],
        a_dev
    );
    assert_eq!(
        b.next("connected", Duration::from_secs(30))["identity"],
        b_dev
    );

    // A PCM microphone cannot be switched like a device one (review M3).
    a.send("mic default");
    let switched = a.next("mic", Duration::from_secs(5));
    assert!(
        switched["error"]
            .as_str()
            .is_some_and(|e| e.starts_with("E_UNSUPPORTED")),
        "{switched}"
    );

    // Each hears the other's tone, from the other's verified device.
    hears(&mut a, &b_dev, 1000.0, Duration::from_secs(20));
    hears(&mut b, &a_dev, 440.0, Duration::from_secs(20));
    // Two more seconds of audio (50 Opus frames a second) before counting.
    std::thread::sleep(Duration::from_secs(2));
    let (sa, sb) = (a.stats(), b.stats());
    let e1 = alice_epoch(&alice);
    let a_kid = format!("{:x}", (e1.own_leaf << 8) | (e1.epoch & 0xff));
    let b_leaf = e1.roster.iter().find(|(_, d)| hex(d) == b_dev).unwrap().0;
    let b_kid = format!("{:x}", (b_leaf << 8) | (e1.epoch & 0xff));
    assert!(opened(&sa, &b_kid) > 50, "{sa}");
    assert!(opened(&sb, &a_kid) > 50, "{sb}");
    // The room's SIF trailer reached the hook from LiveKit's join response (43 to 52 bytes).
    for s in [&sa, &sb] {
        let len = s["media"]["sif_trailer_len"].as_u64().unwrap();
        assert!((43..=52).contains(&len), "{s}");
    }
    assert_eq!(failed(&sa), 0, "{sa}");
    assert_eq!(failed(&sb), 0, "{sb}");
    // The link's ping (the dock's and the call bar's "{ms} ms"): measured on a live link, here a few ms at most.
    let deadline = Instant::now() + Duration::from_secs(15);
    let rtt = loop {
        let s = a.stats();
        if let Some(ms) = s["rtt_ms"].as_u64() {
            break ms;
        }
        assert!(Instant::now() < deadline, "no ping measured: {s}");
        std::thread::sleep(Duration::from_millis(500));
    };
    assert!(rtt < 1000, "a local link's ping: {rtt} ms");

    // An epoch change mid-call: bob commits, both install the new epoch; audio carries on under
    // the new KIDs with no frame accepted under another key.
    bob.self_update(&mut call);
    alice.sync(&call);
    let (ae2, be2) = (alice_epoch(&alice), bob_epoch(&bob));
    assert_eq!(ae2.epoch, be2.epoch);
    assert_eq!(ae2.key, be2.key);
    b.epoch(&be2);
    a.epoch(&ae2);
    let kid2 = |leaf: u64| format!("{:x}", (leaf << 8) | (ae2.epoch & 0xff));
    let deadline = Instant::now() + Duration::from_secs(15);
    let (sa2, sb2) = loop {
        let (sa2, sb2) = (a.stats(), b.stats());
        if opened(&sa2, &kid2(b_leaf)) > 50 && opened(&sb2, &kid2(ae2.own_leaf)) > 50 {
            break (sa2, sb2);
        }
        assert!(
            Instant::now() < deadline,
            "no audio under epoch {}: {sa2} {sb2}",
            ae2.epoch
        );
        std::thread::sleep(Duration::from_millis(250));
    };
    assert_eq!(failed(&sa2), 0, "{sa2}");
    assert_eq!(failed(&sb2), 0, "{sb2}");
    hears(&mut a, &b_dev, 1000.0, Duration::from_secs(5));
    hears(&mut b, &a_dev, 440.0, Duration::from_secs(5));
    println!("LIVE alice {sa2}");
    println!("LIVE bob {sb2}");
}

/// e2e/media/canary.spec.ts, "a canary connected straight to the SFU is decoded by nobody (MD-13, SP-13, DEV-63)": two
/// canaries join the call's room with tokens of their own and publish a tone while the members talk. Neither is in the
/// call group: one seals under a key and leaf nobody holds, the other forges alice's key id under a wrong key. Bob joins
/// after they publish (the first subscription fails closed too). Over five seconds and after, no member opens a canary
/// frame, verifies a canary device or hears a canary tone; the canary frames are refused; the members still hear each
/// other.
#[test]
#[ignore = "needs dilla-testhost (Go) and the WASI core"]
fn a_canary_connected_straight_to_the_sfu_is_decoded_by_nobody() {
    let host = start_host();
    let (_call, alice, bob, _) = call_raw_bob();
    let (a_dev, b_dev) = (hex(&alice.device), hex(&bob.device));
    let room = hex(&CALL);
    let (url, a_token) = token(&host.control, &room, &a_dev);
    let (_, b_token) = token(&host.control, &room, &b_dev);
    let e = alice_epoch(&alice);
    let a_leaf = e.own_leaf;
    let b_leaf = e.roster.iter().find(|(_, d)| hex(d) == b_dev).unwrap().0;
    let kid = |leaf: u64| format!("{:x}", (leaf << 8) | (e.epoch & 0xff));
    let member_kids = [kid(a_leaf), kid(b_leaf)];

    let mut a = Peer::start(&url, &a_token, 440);
    a.epoch(&e);
    a.next("connected", Duration::from_secs(30));
    // The canaries: (device, tone, the leaf its frames claim).
    let canaries = [([0xc1u8; 16], 700u32, 9u64), ([0xc2u8; 16], 1300, a_leaf)];
    let mut canary_peers = Vec::new();
    for (device, tone, leaf) in canaries {
        let (_, t) = token(&host.control, &room, &hex(&device));
        let mut c = Peer::start(&url, &t, tone);
        c.epoch(&Epoch {
            epoch: e.epoch,
            key: [0x5a; 16],
            own_leaf: leaf,
            roster: vec![(leaf, device)],
        });
        c.next("connected", Duration::from_secs(30));
        canary_peers.push(c);
    }
    let mut b = Peer::start(&url, &b_token, 1000);
    b.epoch(&bob_epoch(&bob));
    b.next("connected", Duration::from_secs(30));
    hears(&mut a, &b_dev, 1000.0, Duration::from_secs(20));
    hears(&mut b, &a_dev, 440.0, Duration::from_secs(20));

    let check = |s: &Value| {
        for kid in s["media"]["opened"].as_object().unwrap().keys() {
            assert!(
                member_kids.contains(kid),
                "a canary frame was opened under {kid}: {s}"
            );
        }
        for device in s["media"]["verified"].as_object().unwrap().keys() {
            assert!(
                *device == a_dev || *device == b_dev,
                "a canary device was verified: {s}"
            );
        }
        for (device, tone, _) in canaries {
            let heard = s["peaks"][hex(&device)].as_f64();
            assert!(
                heard.is_none_or(|p| (p - f64::from(tone)).abs() > 20.0),
                "a canary's tone was heard: {s}"
            );
        }
        // A forged frame under alice's key id never decodes as her tone at the canary's pitch.
        assert!(
            s["peaks"][a_dev.as_str()]
                .as_f64()
                .is_none_or(|p| (p - 1300.0).abs() > 20.0),
            "{s}"
        );
    };
    // Twenty 250 ms ticks: at least five seconds of the canaries publishing to both members.
    for _ in 0..20 {
        check(&a.stats());
        check(&b.stats());
        std::thread::sleep(Duration::from_millis(250));
    }
    for m in [&mut a, &mut b] {
        let s = m.stats();
        check(&s);
        let refused: u64 = s["media"]["failed"]
            .as_object()
            .unwrap()
            .iter()
            .filter(|(code, _)| *code != "sif")
            .filter_map(|(_, n)| n.as_u64())
            .sum();
        assert!(refused > 0, "the canary frames were refused: {s}");
    }
    hears(&mut a, &b_dev, 1000.0, Duration::from_secs(5));
    hears(&mut b, &a_dev, 440.0, Duration::from_secs(5));
    drop(canary_peers);
}

/// unsupported-sfu-codec.spec.ts "the real SFU offers AV1 but the client negotiates and sends VP8": dilla-testhost's SFU
/// enables AV1 (`-sfu-av1`). Alice shares her screen; what goes out is VP8 (the publisher's negotiated codec, with bytes
/// sent), and bob opens it as her verified screen with nothing refused.
#[test]
#[ignore = "needs dilla-testhost (Go) and the WASI core"]
fn sfu_offers_av1_client_sends_vp8() {
    let host = start_host_with(&["-sfu-av1"]);
    let (_call, alice, bob, _) = call_raw_bob();
    let (a_dev, b_dev) = (hex(&alice.device), hex(&bob.device));
    let room = hex(&CALL);
    let (url, a_token) = token(&host.control, &room, &a_dev);
    let (_, b_token) = token(&host.control, &room, &b_dev);
    let mut a = Peer::start(&url, &a_token, 440);
    let mut b = Peer::start(&url, &b_token, 1000);
    a.epoch(&alice_epoch(&alice));
    b.epoch(&bob_epoch(&bob));
    a.next("connected", Duration::from_secs(30));
    b.next("connected", Duration::from_secs(30));
    a.send("share screen");
    assert_eq!(
        a.next("share", Duration::from_secs(15))["error"],
        Value::Null
    );
    let key = format!("{a_dev}:Screen");
    let deadline = Instant::now() + Duration::from_secs(30);
    loop {
        b.send("videos");
        let v = b.next("videos", Duration::from_secs(5));
        if v["videos"][&key]["frames"].as_u64().is_some_and(|n| n > 5) {
            assert_eq!(v["videos"][&key]["bars"], true, "{v}");
            break;
        }
        assert!(
            Instant::now() < deadline,
            "bob never saw alice's screen: {v}"
        );
        std::thread::sleep(Duration::from_millis(250));
    }
    a.send("sent");
    let sent = a.next("sent", Duration::from_secs(5));
    let video = sent["video"].as_array().unwrap();
    assert!(!video.is_empty(), "{sent}");
    for (mime, bytes) in video.iter().map(|v| (&v[0], &v[1])) {
        assert_eq!(mime, "video/VP8", "{sent}");
        assert!(bytes.as_u64().unwrap() > 0, "{sent}");
    }
    let (sa, sb) = (a.stats(), b.stats());
    assert!(
        sa["media"]["failed"]["unsupportedCodec"].is_null(),
        "no AV1 frame reached the sender: {sa}"
    );
    assert_eq!(failed(&sb), 0, "{sb}");
}
