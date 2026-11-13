//! Task 17's client tests for the voice area (standard procedure S2), against a real dillad: the roster (op 50 and the
//! snapshot reads), join and leave through `Client::voice`, the own `voice_self`, and that no key reaches a slice.
//!
//! The media of each device runs in a `call-peer` process of its own (the frame hook is process-wide), fed the
//! `MediaLink` the voice area hands out. `#[ignore]`d: they need `target/dilla-testhost` and the WASI core:
//! `cargo test -p dilla-client --test voice -- --ignored`.

#[path = "support/call_world.rs"]
mod world;

use std::sync::Arc;
use std::time::{Duration, Instant};

use dilla_client::Client;
use dilla_client::calls::CallPhase;
use dilla_client::features::voice::MediaLink;
use dilla_client::state::voice::flags;
use dilla_client::state::{AppState, ChannelId, DeviceId};
use world::{Host, MediaPeer, Person, hex};

const WAIT: Duration = Duration::from_secs(45);

/// Waits until `pred` holds for the client's state, answering that state.
async fn until(client: &Client, what: &str, pred: impl Fn(&AppState) -> bool) -> Arc<AppState> {
    let mut rx = client.state();
    let deadline = Instant::now() + WAIT;
    loop {
        let s = rx.borrow_and_update().clone();
        if pred(&s) {
            return s;
        }
        let left = deadline.saturating_duration_since(Instant::now());
        assert!(!left.is_zero(), "{what}: never; voice {:?}", s.voice);
        let _ = tokio::time::timeout(left.min(Duration::from_millis(500)), rx.changed()).await;
    }
}

struct World {
    host: Host,
    alice: Person,
    bob: Person,
    voice: ChannelId,
}

async fn world(tag: &'static str) -> World {
    let host = tokio::task::spawn_blocking(move || world::start_host(tag))
        .await
        .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let alice = world::sign_up(&host, &invite, "alice", tag).await;
    let bob = world::sign_up(&host, &invite, "bob", tag).await;
    tokio::task::spawn_blocking(move || {
        let (_, v) = world::voice_community(&host, &alice, &[&bob]);
        world::clear_session(&alice);
        world::clear_session(&bob);
        World {
            host,
            alice,
            bob,
            voice: ChannelId(v),
        }
    })
    .await
    .unwrap()
}

/// A started client, its voice area following the instance.
async fn client(p: &Person) -> Client {
    let c = world::start(p).await;
    world::connected(&c).await;
    c.voice().start();
    c
}

/// Waits until the client lists the voice channel (its structure read landed).
async fn listed(c: &Client, voice: ChannelId) {
    until(c, "the voice channel is listed", |s| {
        s.channel(voice).is_some()
    })
    .await;
}

/// Joins `voice` and connects the device's media in a `call-peer` playing `tone`.
async fn join_with_media(host: &Host, c: &Client, voice: ChannelId, tone: u32) -> MediaPeer {
    listed(c, voice).await;
    c.voice()
        .join(voice)
        .await
        .unwrap()
        .expect("the join succeeds");
    let link: Arc<MediaLink> = c.voice().media().borrow().clone().expect("a media link");
    let url = host.rtc_url(&link.url);
    let token = link.token.to_string();
    let first = link.epochs.borrow().clone().expect("the join epoch");
    tokio::task::spawn_blocking(move || {
        let mut m = MediaPeer::start(&url, &token, tone);
        m.epoch(&first);
        m.next("connected", Duration::from_secs(30));
        m
    })
    .await
    .unwrap()
}

fn in_call(s: &AppState, voice: ChannelId, device: [u8; 16], bits: u8) -> bool {
    s.voice
        .participants(voice)
        .iter()
        .any(|p| p.device_id == DeviceId(device) && p.flags & bits == bits)
}

/// e2e/voice/join-talk.spec.ts: two devices join one voice channel, each sees the other in the roster and hears the
/// other's tone under the call group's keys; no key and no token is in any slice.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn two_clients_join_see_and_hear_no_key_in_state() {
    let w = world("voice-talk").await;
    let a = client(&w.alice).await;
    let b = client(&w.bob).await;
    let mut a_media = join_with_media(&w.host, &a, w.voice, 440).await;
    let s = until(&a, "alice is in", |s| s.voice.call.phase == CallPhase::In).await;
    assert_eq!(s.voice.call.channel, Some(w.voice));
    let b_media = join_with_media(&w.host, &b, w.voice, 660).await;
    // Bob's join commit moves alice's keys; her media gets the new epoch.
    let b_epoch = until(&b, "bob is in", |s| s.voice.call.phase == CallPhase::In)
        .await
        .voice
        .call
        .epoch
        .unwrap();
    until(&a, "alice reaches bob's epoch", |s| {
        s.voice.call.epoch.is_some_and(|e| e >= b_epoch)
    })
    .await;
    let a_link = a.voice().media().borrow().clone().unwrap();
    let now = a_link.epochs.borrow().clone().unwrap();
    let (ad, bd) = (w.alice.device_id, w.bob.device_id);
    let (a_hex, b_hex) = (hex(&ad), hex(&bd));
    let (a_media, b_media) = tokio::task::spawn_blocking(move || {
        a_media.epoch(&now);
        a_media.hears(&b_hex, 660.0, Duration::from_secs(30));
        let mut b_media = b_media;
        b_media.hears(&a_hex, 440.0, Duration::from_secs(30));
        (a_media, b_media)
    })
    .await
    .unwrap();
    for c in [&a, &b] {
        let s = until(c, "both in the roster", |s| {
            in_call(s, w.voice, ad, flags::IN_CALL) && in_call(s, w.voice, bd, flags::IN_CALL)
        })
        .await;
        let shown = format!("{s:?}");
        let link = c.voice().media().borrow().clone().unwrap();
        let key = link.epochs.borrow().clone().unwrap();
        assert!(
            !shown.contains(&hex(&*key.base_key)),
            "a media key in a slice"
        );
        assert!(
            !shown.contains(link.token.as_str()),
            "the LiveKit token in a slice"
        );
        assert!(!format!("{link:?}").contains(link.token.as_str()));
    }
    // The own device is marked in each roster.
    let s = a.state().borrow().clone();
    assert!(
        s.voice
            .participants(w.voice)
            .iter()
            .any(|p| p.own && p.device_id == DeviceId(ad))
    );
    tokio::task::spawn_blocking(move || {
        a_media.quit();
        b_media.quit();
    })
    .await
    .unwrap();
}

/// e2e/voice/join-muted.spec.ts (the client half): mute pressed before the join, the join starts muted and the others'
/// roster carries the device's self_mute (the `voice_self` re-sent until the instance's `voice_state` shows it).
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn muted_join_shows_glyph_and_self_mute() {
    let w = world("voice-muted").await;
    let a = client(&w.alice).await;
    let b = client(&w.bob).await;
    a.voice().set_muted(true);
    until(&a, "muted before the join", |s| s.voice.self_mute).await;
    let a_media = join_with_media(&w.host, &a, w.voice, 440).await;
    let ad = w.alice.device_id;
    until(&b, "bob sees alice muted", |s| {
        in_call(s, w.voice, ad, flags::IN_CALL | flags::SELF_MUTE)
    })
    .await;
    // Deafen shows both bits, undeafen restores the mute it had.
    a.voice().set_deafened(true);
    until(&b, "bob sees alice deafened", |s| {
        in_call(s, w.voice, ad, flags::SELF_MUTE | flags::SELF_DEAF)
    })
    .await;
    a.voice().set_deafened(false);
    until(&b, "bob sees alice muted only", |s| {
        s.voice
            .participants(w.voice)
            .iter()
            .any(|p| p.device_id == DeviceId(ad) && p.muted() && !p.deafened())
    })
    .await;
    a.voice().set_muted(false);
    until(&b, "bob sees alice unmuted", |s| {
        s.voice
            .participants(w.voice)
            .iter()
            .any(|p| p.device_id == DeviceId(ad) && !p.muted())
    })
    .await;
    tokio::task::spawn_blocking(move || a_media.quit())
        .await
        .unwrap();
}

/// e2e/web/channel-list.spec.ts (the client half): a device that never joined sees who is in a voice channel from the
/// community read and the frames; the people leave the roster when they leave, and the call ends with the last one.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn voice_roster_follows_join_and_leave() {
    let w = world("voice-roster").await;
    let a = client(&w.alice).await;
    let a_media = join_with_media(&w.host, &a, w.voice, 440).await;
    // Bob starts after alice is in: his community read lists her.
    let b = client(&w.bob).await;
    let ad = w.alice.device_id;
    until(&b, "bob's read lists alice", |s| {
        in_call(s, w.voice, ad, flags::IN_CALL)
    })
    .await;
    a.voice().leave().await.unwrap().expect("alice leaves");
    assert!(
        a.voice().media().borrow().is_none(),
        "the link goes with the call"
    );
    let s = until(&a, "alice is out", |s| {
        s.voice.call.phase == CallPhase::Idle
    })
    .await;
    assert!(s.voice.call.call_id.is_none());
    tokio::task::spawn_blocking(move || a_media.quit())
        .await
        .unwrap();
    until(&b, "the call is gone for bob", |s| {
        s.voice.participants(w.voice).is_empty()
    })
    .await;
}

/// e2e/voice/share.spec.ts (the media half): a screen share and a camera, published under the call's keys, reach the
/// other device verified (its decoded frames are the test pattern), the roster shows the device's `screen` and `video`
/// bits, and both stop.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn share_and_camera_reach_peer_verified_and_stop() {
    use dilla_client::state::voice::LocalShare;
    let w = world("voice-share").await;
    let a = client(&w.alice).await;
    let b = client(&w.bob).await;
    let a_media = join_with_media(&w.host, &a, w.voice, 440).await;
    let b_media = join_with_media(&w.host, &b, w.voice, 660).await;
    let b_epoch = until(&b, "bob is in", |s| s.voice.call.phase == CallPhase::In)
        .await
        .voice
        .call
        .epoch
        .unwrap();
    until(&a, "alice reaches bob's epoch", |s| {
        s.voice.call.epoch.is_some_and(|e| e >= b_epoch)
    })
    .await;
    let link = a.voice().media().borrow().clone().unwrap();
    let now = link.epochs.borrow().clone().unwrap();
    a.voice()
        .set_share(LocalShare {
            camera: true,
            screen: true,
            screen_audio: false,
        })
        .await
        .unwrap()
        .expect("the instance gives alice a sharing slot");
    let ad = w.alice.device_id;
    let a_hex = hex(&ad);
    let (a_media, b_media) = tokio::task::spawn_blocking(move || {
        let (mut a_media, mut b_media) = (a_media, b_media);
        a_media.epoch(&now);
        for kind in ["screen", "camera"] {
            a_media.send(&format!("share {kind}"));
            let r = a_media.next("share", Duration::from_secs(20));
            assert!(r["error"].is_null(), "{r}");
        }
        let deadline = Instant::now() + Duration::from_secs(30);
        loop {
            b_media.send("videos");
            let v = b_media.next("videos", Duration::from_secs(5));
            let ok = |kind: &str| {
                let s = &v["videos"][format!("{a_hex}:{kind}")];
                s["frames"].as_u64().is_some_and(|n| n >= 5) && s["bars"] == true
            };
            if ok("Screen") && ok("Camera") {
                break;
            }
            assert!(
                Instant::now() < deadline,
                "bob never saw alice's shares: {v}"
            );
            std::thread::sleep(Duration::from_millis(250));
        }
        (a_media, b_media)
    })
    .await
    .unwrap();
    until(&b, "bob's roster shows alice sharing", |s| {
        in_call(s, w.voice, ad, flags::SCREEN | flags::VIDEO)
    })
    .await;
    // Both stop: the streams leave bob's view and the bits leave the roster.
    let a_hex = hex(&ad);
    let (a_media, b_media) = tokio::task::spawn_blocking(move || {
        let (mut a_media, mut b_media) = (a_media, b_media);
        for kind in ["screen", "camera"] {
            a_media.send(&format!("unshare {kind}"));
            a_media.next("unshare", Duration::from_secs(10));
        }
        let deadline = Instant::now() + Duration::from_secs(30);
        loop {
            b_media.send("videos");
            let v = b_media.next("videos", Duration::from_secs(5));
            if !v["videos"]
                .as_object()
                .is_some_and(|m| m.keys().any(|k| k.starts_with(&a_hex)))
            {
                break;
            }
            assert!(
                Instant::now() < deadline,
                "alice's streams never stopped for bob: {v}"
            );
            std::thread::sleep(Duration::from_millis(250));
        }
        (a_media, b_media)
    })
    .await
    .unwrap();
    a.voice()
        .set_share(LocalShare::default())
        .await
        .unwrap()
        .expect("the slot is freed");
    until(&b, "the bits leave bob's roster", |s| {
        s.voice
            .participants(w.voice)
            .iter()
            .any(|p| p.device_id == DeviceId(ad) && !p.screen() && !p.video())
    })
    .await;
    tokio::task::spawn_blocking(move || {
        a_media.quit();
        b_media.quit();
    })
    .await
    .unwrap();
}
