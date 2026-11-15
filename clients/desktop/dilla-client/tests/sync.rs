//! The sync engine against a real dillad (dilla-testhost): two Rust clients in one channel exchange messages, a client
//! that was away catches up exactly once and in order, and unread counts follow what arrives and what is read.
//!
//! Needs `target/dilla-testhost` (`go build -o target/dilla-testhost ./cmd/dilla-testhost`, or `DILLA_TESTHOST`) and
//! the WASI core (`DILLA_CORE_WASI`, else `target/wasm32-wasip1/release/dilla_core_wasi.wasm`); the tests are
//! `#[ignore]`d so a plain `cargo test` needs no Go toolchain (the desktop-gpui CI job has one and runs them on Linux with
//! `cargo test -p dilla-client --test sync -- --ignored`).
//!
//! Every account is made the way the app makes one: a sign-up from the client core's own identity (the instance's
//! bootstrap invite), its device list published, an authenticator app enrolled over the real `/v1/auth/totp/*` routes
//! (every account owes a second factor before a session opens the gateway). The community, its channel and the
//! invite are set up through the testkit's `HttpDs` on the sign-up session; the stored session is then cleared, so each
//! `Client` establishes its own from the device key.

use std::time::{Duration, Instant};

use dilla_client::Client;
use dilla_client::http::routes::{self as r, NoBody};
use dilla_client::http::{AuthToken, ReqwestHttp};
use dilla_client::seams::Http;
use dilla_client::state::{AppState, ChannelId, TimelineItemState, UserId};
use dilla_client::testing::host::{
    Host, Person, clear_session, community, hex, sign_up, start, start_host, wait_for,
};

/// Two people in one community, signed up, set up and with their sessions cleared.
async fn two_people(tag: &str) -> (Host, Person, Person, [u8; 16]) {
    let host = tokio::task::spawn_blocking({
        let tag = tag.to_owned();
        move || start_host(&tag, "127.0.0.1:0", None)
    })
    .await
    .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let a = sign_up(&host, &invite, "alice", "alice", tag).await;
    let b = sign_up(&host, &invite, "bob", "bob", tag).await;
    let (host, a, b, c) = tokio::task::spawn_blocking(move || {
        let c = community(&host, &a, "gpui", &[&b]);
        clear_session(&a);
        clear_session(&b);
        (host, a, b, c)
    })
    .await
    .unwrap();
    (host, a, b, c)
}

async fn general(client: &Client, community: [u8; 16]) -> ChannelId {
    let s = wait_for(client, Duration::from_secs(30), "the channel list", |s| {
        s.channel_named(dilla_client::state::CommunityId(community), "general")
            .is_some()
    })
    .await;
    s.channel_named(dilla_client::state::CommunityId(community), "general")
        .unwrap()
        .id
}

/// Both clients hold the channel's group: `a` registers it, `b` joins it, and `a` has applied `b`'s join.
async fn both_in(a: &Client, b: &Client, channel: ChannelId) {
    a.open_channel(channel)
        .await
        .unwrap()
        .expect("a opens the channel");
    b.open_channel(channel)
        .await
        .unwrap()
        .expect("b joins the channel");
}

fn bodies_from(s: &AppState, channel: ChannelId, author: [u8; 16]) -> Vec<String> {
    s.timelines
        .get(&channel)
        .map(|t| {
            t.items
                .iter()
                .filter(|i| {
                    i.state == TimelineItemState::Ok && i.sender_user == Some(UserId(author))
                })
                .map(|i| i.body.clone())
                .collect()
        })
        .unwrap_or_default()
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn two_devices_exchange_text() {
    let (_host, pa, pb, c) = two_people("exchange").await;
    let a = start(&pa).await;
    let b = start(&pb).await;
    let channel = general(&a, c).await;
    general(&b, c).await;
    both_in(&a, &b, channel).await;

    a.send_text(channel, "hello from gpui".into())
        .await
        .unwrap()
        .expect("queued");
    let sent = Instant::now();
    let s = wait_for(&b, Duration::from_secs(5), "b to show a's message", |s| {
        bodies_from(s, channel, pa.user_id) == ["hello from gpui"]
    })
    .await;
    assert!(sent.elapsed() <= Duration::from_secs(5));
    let item = s.timelines[&channel]
        .items
        .iter()
        .find(|i| i.body == "hello from gpui")
        .unwrap();
    assert_eq!(
        item.sender_user,
        Some(UserId(pa.user_id)),
        "a is the author"
    );
    assert!(!item.own);
    // And a sees its own message confirmed (stored, with a seq), once.
    wait_for(
        &a,
        Duration::from_secs(5),
        "a's message to be stored",
        |s| {
            s.timelines.get(&channel).is_some_and(|t| {
                t.items
                    .iter()
                    .filter(|i| i.body == "hello from gpui")
                    .count()
                    == 1
                    && t.items
                        .iter()
                        .any(|i| i.body == "hello from gpui" && i.own && i.seq.is_some())
            })
        },
    )
    .await;
    // The way back: b answers, a reads it.
    b.send_text(channel, "hello from b".into())
        .await
        .unwrap()
        .expect("queued");
    wait_for(&a, Duration::from_secs(5), "a to show b's message", |s| {
        s.timelines
            .get(&channel)
            .is_some_and(|t| t.items.iter().any(|i| i.body == "hello from b" && !i.own))
    })
    .await;
}

/// Controller ruling: a session the instance stops taking (here: expired on its clock) is re-established from the
/// device key and the gateway started again on it; the account is not signed out.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn expired_session_reauthenticates() {
    use dilla_client::state::{AccountPhase, ConnectionStatus};
    let (host, pa, pb, c) = two_people("reauth").await;
    let a = start(&pa).await;
    let b = start(&pb).await;
    let channel = general(&a, c).await;
    general(&b, c).await;
    both_in(&a, &b, channel).await;
    tokio::time::sleep(Duration::from_secs(2)).await;
    a.disconnect();
    // Past every session lifetime on the instance's clock.
    let advanced = reqwest::Client::new()
        .post(format!("{}/debug/clock", host.control))
        .header("content-type", "application/json")
        .body(r#"{"seconds":3456000}"#)
        .send()
        .await
        .unwrap();
    assert!(
        advanced.status().is_success(),
        "clock: {}",
        advanced.status()
    );
    a.reconnect();
    let s = wait_for(&a, Duration::from_secs(30), "a online again", |s| {
        s.connection.status == ConnectionStatus::Online || s.account.phase != AccountPhase::Ready
    })
    .await;
    assert_eq!(s.account.phase, AccountPhase::Ready, "not signed out");
    // The new session carries traffic: a's post reaches b.
    a.send_text(channel, "after the renewal".into())
        .await
        .unwrap()
        .expect("queued");
    wait_for(&b, Duration::from_secs(20), "b to read it", |s| {
        bodies_from(s, channel, pa.user_id) == ["after the renewal"]
    })
    .await;
}

/// Fix round 2 (N1): a start that fails, called straight from an async task, answers its error instead of panicking
/// (the runtime it built is shut down in the background, never dropped there). The store is signed up and its session
/// cleared, so the start reaches the network: the server it names does not answer.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn start_offline_from_an_async_task_is_an_error() {
    let (_host, pa, _pb, _c) = two_people("offline").await;
    let mut cfg = pa.cfg.clone();
    cfg.server = url::Url::parse("http://127.0.0.1:9").unwrap();
    let started = Client::start(cfg, pa.kc.clone());
    assert!(
        matches!(started, Err(dilla_client::ClientError::Network(_))),
        "{:?}",
        started.err()
    );
}

/// Task 28 item 23: a running client answers the instance it serves (the desktop's step-up and factor screens read it
/// from the client, not from a registry keyed by the client's address).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn a_client_answers_its_instance() {
    let (_host, pa, _pb, _c) = two_people("instance").await;
    let a = Client::start(pa.cfg.clone(), pa.kc.clone()).expect("a starts");
    let cfg = a.instance();
    assert_eq!(cfg.id, pa.cfg.id);
    assert_eq!(cfg.server, pa.cfg.server);
    assert_eq!(cfg.data_dir, pa.cfg.data_dir);
}

/// Fix round 1: the instance's session sweep closes a live connection 4004 (not resumable) whose session no longer
/// resolves: deleted, pruned or past its hard expiry (protocol/02 § Device sessions item 6, internal/gateway/heartbeat.go
/// `sweepSessions`). As controller.ts `onRevokedSocket` does, the client establishes a new session from the device key
/// and carries on; only a device whose challenge is refused would be signed out.
///
/// The session rows are deleted and the clock moved one second (one liveness tick, which runs the sweep) while the
/// connection is up: a clock jump past the expiry itself trips the heartbeat deadline first, and the reconnect then
/// meets 4003 at identify (`expired_session_reauthenticates` covers that path).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn live_expiry_sweep_reauthenticates() {
    use dilla_client::state::{AccountPhase, ConnectionStatus};
    let (host, pa, pb, c) = two_people("sweep").await;
    // Called straight from the async test: start must not panic inside a runtime (fix round 1, ruling 3).
    let a = Client::start(pa.cfg.clone(), pa.kc.clone()).expect("a starts from inside a runtime");
    let b = start(&pb).await;
    let channel = general(&a, c).await;
    general(&b, c).await;
    both_in(&a, &b, channel).await;
    tokio::time::sleep(Duration::from_secs(2)).await;
    assert_eq!(
        a.state().borrow().connection.status,
        ConnectionStatus::Online
    );
    let readies = a.state().borrow().connection.readies;
    // Every session of a's device goes (the sign-up session's token still authorises the delete).
    let base = url::Url::parse(&host.public).unwrap();
    ReqwestHttp::new(base, AuthToken::new(pa.token.as_str()))
        .unwrap()
        .call::<r::DeleteSessions>(&[&hex(&pa.device_id)], NoBody)
        .await
        .expect("sessions deleted");
    // One liveness tick on the instance's clock, with a still connected: its sweep closes the socket 4004.
    let advanced = reqwest::Client::new()
        .post(format!("{}/debug/clock", host.control))
        .header("content-type", "application/json")
        .body(r#"{"seconds":1,"sweep":true}"#)
        .send()
        .await
        .unwrap();
    assert!(
        advanced.status().is_success(),
        "clock: {}",
        advanced.status()
    );
    // The sweep cuts a's socket and a comes back on a renewed session: a new ready (the renewal can be quicker than
    // the published "connecting" in between, so the count is the proof, not that transient state).
    let s = wait_for(
        &a,
        Duration::from_secs(40),
        "a's ready on a renewed session",
        |s| {
            (s.connection.readies > readies && s.connection.status == ConnectionStatus::Online)
                || s.account.phase != AccountPhase::Ready
        },
    )
    .await;
    assert_eq!(s.account.phase, AccountPhase::Ready, "a is not signed out");
    wait_for(&b, Duration::from_secs(40), "b online again", |s| {
        s.connection.status == ConnectionStatus::Online && s.account.phase == AccountPhase::Ready
    })
    .await;
    a.send_text(channel, "after the sweep".into())
        .await
        .unwrap()
        .expect("queued");
    wait_for(&b, Duration::from_secs(20), "b to read it", |s| {
        bodies_from(s, channel, pa.user_id) == ["after the sweep"]
    })
    .await;
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn resume_after_sleep_no_gap_no_dup() {
    let (host, pa, pb, c) = two_people("resume").await;
    let a = start(&pa).await;
    let b = start(&pb).await;
    let channel = general(&a, c).await;
    general(&b, c).await;
    both_in(&a, &b, channel).await;

    // Let b's join settle (its activation catch-up done), so what it gets later comes from the catch-up after its ready.
    tokio::time::sleep(Duration::from_secs(2)).await;
    // b's gateway goes away for 30 s while a sends five messages.
    b.disconnect();
    wait_for(&b, Duration::from_secs(5), "b offline", |s| {
        s.connection.status != dilla_client::state::ConnectionStatus::Online
    })
    .await;
    let want: Vec<String> = (1..=5).map(|i| format!("while you slept {i}")).collect();
    for text in &want {
        a.send_text(channel, text.clone())
            .await
            .unwrap()
            .expect("queued");
    }
    wait_for(&a, Duration::from_secs(10), "a's five stored", |s| {
        s.timelines.get(&channel).is_some_and(|t| {
            want.iter()
                .all(|w| t.items.iter().any(|i| &i.body == w && i.seq.is_some()))
        })
    })
    .await;
    // The instance parks b's connection for its resume window (120 s on its clock) and would hand the parked frames to
    // b's next connection; past the window they are gone, as after a real sleep, and b must catch up on its own.
    let advanced = reqwest::Client::new()
        .post(format!("{}/debug/clock", host.control))
        .header("content-type", "application/json")
        .body(r#"{"seconds":150}"#)
        .send()
        .await
        .unwrap();
    assert!(
        advanced.status().is_success(),
        "clock: {}",
        advanced.status()
    );
    tokio::time::sleep(Duration::from_secs(30)).await;
    assert!(
        bodies_from(&b.state().borrow(), channel, pa.user_id).is_empty(),
        "nothing arrived while b was away"
    );

    b.reconnect();
    let s = wait_for(&b, Duration::from_secs(20), "b to catch up", |s| {
        bodies_from(s, channel, pa.user_id).len() >= 5
    })
    .await;
    // Give any duplicate a moment to show, then check exactly those five, in order.
    tokio::time::sleep(Duration::from_secs(2)).await;
    let got = bodies_from(&b.state().borrow(), channel, pa.user_id);
    assert_eq!(
        got,
        want,
        "exactly the five, in order, no duplicates (first read: {:?})",
        bodies_from(&s, channel, pa.user_id)
    );
    let seqs: Vec<u64> = b.state().borrow().timelines[&channel]
        .items
        .iter()
        .filter(|i| i.sender_user == Some(UserId(pa.user_id)))
        .map(|i| i.seq.unwrap())
        .collect();
    assert!(
        seqs.windows(2).all(|w| w[0] < w[1]),
        "ascending seqs: {seqs:?}"
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn unread_counts() {
    let (_host, pa, pb, c) = two_people("unread").await;
    let a = start(&pa).await;
    let b = start(&pb).await;
    let channel = general(&a, c).await;
    general(&b, c).await;
    both_in(&a, &b, channel).await;
    // b looks at nothing: the channel is unfocused.
    b.focus(None);
    for i in 1..=3 {
        a.send_text(channel, format!("unread {i}"))
            .await
            .unwrap()
            .expect("queued");
    }
    wait_for(&b, Duration::from_secs(10), "three unread", |s| {
        bodies_from(s, channel, pa.user_id).len() == 3 && s.unread_of(channel) == 3
    })
    .await;
    b.mark_read(channel).await.unwrap().expect("marked read");
    wait_for(&b, Duration::from_secs(5), "nothing unread", |s| {
        s.unread_of(channel) == 0
    })
    .await;
}
