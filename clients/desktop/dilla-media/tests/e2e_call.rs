//! A desktop call end to end through dillad's real call flow (screens wave 1, Task 3; R19b): two
//! desktop clients (`dilla_client::Client`, its call signalling over the gateway socket) join one
//! voice channel's call (the gateway ticket, the call's MLS group registered by one and joined by
//! external commit by the other, the instance's LiveKit token) and each one's media
//! (`dilla_media::Call`, in a `call-peer` process of its own: the frame hook is process-wide) hears
//! the other's tone under the call group's epoch keys, which each client derives from its own core
//! (`call_media_epoch`, through the core's gate).
//!
//! Needs `target/dilla-testhost` (or `DILLA_TESTHOST`) and the WASI core (or `DILLA_CORE_WASI`):
//! `cargo test -p dilla-media --test e2e_call -- --ignored`.

#[path = "../../dilla-client/tests/support/call_world.rs"]
mod world;

use std::time::{Duration, Instant};

use dilla_client::calls::{CallPhase, CallTarget, MediaEpoch};
use world::{MediaPeer, hex};

/// Waits until the client's call group is at `epoch` or later (its engine applied the commits), answering its keys.
async fn epoch_at_least(
    calls: &dilla_client::calls::Calls,
    group: &[u8; 16],
    epoch: u64,
) -> MediaEpoch {
    let deadline = Instant::now() + Duration::from_secs(30);
    loop {
        match calls.media_epoch(group) {
            Ok(m) if m.epoch >= epoch => return m,
            other => assert!(
                Instant::now() < deadline,
                "the call group never reached epoch {epoch}: {other:?}"
            ),
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn desktop_call_through_dillad() {
    let host = tokio::task::spawn_blocking(|| world::start_host("e2e-call"))
        .await
        .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let alice = world::sign_up(&host, &invite, "alice", "e2e-call").await;
    let bob = world::sign_up(&host, &invite, "bob", "e2e-call").await;
    let (host, alice, bob, community, voice) = tokio::task::spawn_blocking(move || {
        let (c, v) = world::voice_community(&host, &alice, &[&bob]);
        world::clear_session(&alice);
        world::clear_session(&bob);
        (host, alice, bob, c, v)
    })
    .await
    .unwrap();
    let (a_dev, b_dev) = (hex(&alice.device_id), hex(&bob.device_id));
    let a = world::start(&alice).await;
    let b = world::start(&bob).await;
    world::connected(&a).await;
    world::connected(&b).await;
    let (a_calls, b_calls) = (a.calls(), b.calls());
    let target = CallTarget {
        channel: voice,
        community: Some(community),
        vdec: String::new(),
    };

    // Alice opens the call: she registers its MLS group and the instance starts the call (201).
    let a_join = a_calls.join(target.clone());
    let a_phase = a_join.phase();
    let a_in = a_join.outcome().await.unwrap().expect("alice joins");
    assert_eq!(*a_phase.borrow(), CallPhase::In);
    assert!(a_in.started.created);
    let group = a_in.group_id;
    let a_url = host.rtc_url(&a_in.started.livekit_url);
    let a_token = a_in.started.token.to_string();
    let a_first = a_in.media;
    let mut a_media = tokio::task::spawn_blocking(move || {
        let mut m = MediaPeer::start(&a_url, &a_token, 440);
        m.epoch(&a_first);
        m.next("connected", Duration::from_secs(30));
        m
    })
    .await
    .unwrap();

    // Bob joins it by external commit: the same group, a roster of both devices.
    let b_in = b_calls
        .join(target.clone())
        .outcome()
        .await
        .unwrap()
        .expect("bob joins");
    assert!(!b_in.started.created);
    assert_eq!(b_in.group_id, group, "one call group");
    let mut roster: Vec<[u8; 16]> = b_in.media.roster.iter().map(|(_, d, _)| *d).collect();
    roster.sort();
    let mut both = vec![alice.device_id, bob.device_id];
    both.sort();
    assert_eq!(roster, both);
    let b_epoch = b_in.media.epoch;

    // Alice's engine applies bob's join commit: both hold the same epoch's key.
    let a_now = epoch_at_least(&a_calls, &group, b_epoch).await;
    assert_eq!(a_now.epoch, b_epoch);
    assert_eq!(*a_now.base_key, *b_in.media.base_key, "one epoch, one key");
    assert_eq!(a_now.code, b_in.media.code, "one safety code");

    let b_url = host.rtc_url(&b_in.started.livekit_url);
    let b_token = b_in.started.token.to_string();
    let b_first = b_in.media;
    let (a_media, b_media) = tokio::task::spawn_blocking(move || {
        a_media.epoch(&a_now);
        let mut b_media = MediaPeer::start(&b_url, &b_token, 660);
        b_media.epoch(&b_first);
        let connected = b_media.next("connected", Duration::from_secs(30));
        assert_eq!(connected["identity"], b_dev.as_str());
        // Each hears the other's tone, from the other's device, decrypted under the call group's keys.
        let heard_b = a_media.hears(&b_dev, 660.0, Duration::from_secs(30));
        let heard_a = b_media.hears(&a_dev, 440.0, Duration::from_secs(30));
        for s in [&heard_a, &heard_b] {
            let failed: u64 = s["media"]["failed"]
                .as_object()
                .map(|o| o.values().filter_map(serde_json::Value::as_u64).sum())
                .unwrap_or(0);
            assert_eq!(failed, 0, "no frame failed to open: {s}");
        }
        (a_media, b_media)
    })
    .await
    .unwrap();

    // Bob leaves (his own Remove, then his media), and alice's client commits it: her keys move on to an epoch whose
    // roster is hers alone, so bob holds no key of it.
    let b_left = b_calls
        .leave(group, b_in.started.call_id, false)
        .await
        .unwrap()
        .expect("client");
    assert!(!b_left.ended, "alice is still in the call: {b_left:?}");
    assert!(b_left.proposed, "{b_left:?}");
    tokio::task::spawn_blocking(move || b_media.quit())
        .await
        .unwrap();
    let deadline = Instant::now() + Duration::from_secs(30);
    let alone = loop {
        let m = a_calls.media_epoch(&group).expect("alice's gate passes");
        if m.roster.len() == 1 {
            break m;
        }
        assert!(
            Instant::now() < deadline,
            "alice never committed bob's Remove: epoch {}, roster {:?}",
            m.epoch,
            m.roster
        );
        tokio::time::sleep(Duration::from_millis(200)).await;
    };
    assert_eq!(alone.roster[0].1, alice.device_id);
    assert!(alone.epoch > b_epoch);

    // Alice, alone, leaves: the call ends.
    let a_left = a_calls
        .leave(group, a_in.started.call_id, true)
        .await
        .unwrap()
        .expect("client");
    assert!(a_left.ended, "{a_left:?}");
    tokio::task::spawn_blocking(move || a_media.quit())
        .await
        .unwrap();
    drop((a, b, host));
}
