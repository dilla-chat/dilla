//! Task 10's client tests for the reactions area (standard procedure S2), against a real dillad (dilla-testhost).
//!
//! Needs `target/dilla-testhost` and the WASI core, like `tests/sync.rs`; the tests are `#[ignore]`d so a plain
//! `cargo test` needs no Go toolchain (the desktop-gpui CI job runs them with `--ignored` on Linux).

use std::sync::Arc;
use std::time::Duration;

use dilla_client::instances::Instances;
use dilla_client::keychain::MemoryKeychain;
use dilla_client::seams::InstanceConfig;
use dilla_client::signin::{Owed, SignIn};
use dilla_client::state::{AppState, ChannelId, CommunityId, MessageId};
use dilla_client::testing::host::{
    Host, PASSWORD, Person, clear_session, community, repo_root, sign_up, start, start_host,
    wait_for,
};
use dilla_client::{Client, ClientError};

const THUMBS_UP: &str = "👍";
const WAIT: Duration = Duration::from_secs(30);

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
    tokio::task::spawn_blocking(move || {
        let c = community(&host, &a, "gpui", &[&b]);
        clear_session(&a);
        clear_session(&b);
        (host, a, b, c)
    })
    .await
    .unwrap()
}

async fn general(client: &Client, community: [u8; 16]) -> ChannelId {
    let s = wait_for(client, WAIT, "the channel list", |s| {
        s.channel_named(CommunityId(community), "general").is_some()
    })
    .await;
    s.channel_named(CommunityId(community), "general")
        .unwrap()
        .id
}

/// What the timeline shows under the message with `body`: `(emoji, count, mine)` per chip, in the order shown.
fn chips(s: &AppState, channel: ChannelId, body: &str) -> Vec<(String, u32, bool)> {
    s.timelines
        .get(&channel)
        .and_then(|t| t.items.iter().find(|i| i.body == body))
        .map(|i| {
            i.reactions
                .iter()
                .map(|r| (r.emoji.clone(), r.count, r.mine))
                .collect()
        })
        .unwrap_or_default()
}

fn message_id(s: &AppState, channel: ChannelId, body: &str) -> MessageId {
    s.timelines[&channel]
        .items
        .iter()
        .find(|i| i.body == body)
        .and_then(|i| i.msg_id)
        .expect("the message has an id")
}

/// Waits until the chips under `body` are `want`.
async fn chips_are(client: &Client, channel: ChannelId, body: &str, want: Vec<(&str, u32, bool)>) {
    let want: Vec<(String, u32, bool)> = want
        .into_iter()
        .map(|(e, n, m)| (e.to_owned(), n, m))
        .collect();
    let (b, w) = (body.to_owned(), want.clone());
    wait_for(client, WAIT, &format!("the chips {want:?}"), move |s| {
        chips(s, channel, &b) == w
    })
    .await;
}

/// e2e/web/conversation.spec.ts, "edit, reply, react, pin and delete between the browser and the peer" (its react part:
/// "Reactions are per user"): the peer reacts, the browser's chip shows 1 and is not pressed; the browser reacts, the chip
/// shows 2 and is pressed, and the peer sees 2; unreacting takes the browser's out again. A repeated reaction of one user
/// is still one (controller.ts ruling 28: the core folds them per user).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn react_and_unreact_between_two_devices() {
    let (_host, pa, pb, c) = two_people("react").await;
    let (alice, bob) = (start(&pa).await, start(&pb).await);
    let channel = general(&alice, c).await;
    general(&bob, c).await;
    alice.open_channel(channel).await.unwrap().expect("opens");
    bob.open_channel(channel).await.unwrap().expect("joins");

    let body = "react to this";
    alice
        .send_text(channel, body.into())
        .await
        .unwrap()
        .expect("queued");
    let s = wait_for(&bob, WAIT, "bob to show the message", |s| {
        s.timelines
            .get(&channel)
            .is_some_and(|t| t.items.iter().any(|i| i.body == body && i.msg_id.is_some()))
    })
    .await;
    let bobs_msg = message_id(&s, channel, body);
    let s = wait_for(&alice, WAIT, "alice to store the message", |s| {
        s.timelines
            .get(&channel)
            .is_some_and(|t| t.items.iter().any(|i| i.body == body && i.seq.is_some()))
    })
    .await;
    let alices_msg = message_id(&s, channel, body);
    assert_eq!(alices_msg, bobs_msg, "both devices name the message alike");

    // The core refuses what is not an emoji of 1..=32 bytes, and nothing is queued.
    for bad in ["", "x".repeat(33).as_str()] {
        let refused = alice
            .reactions()
            .react(channel, alices_msg, bad, true)
            .await;
        assert!(
            matches!(refused, Ok(Err(ClientError::Sync(ref code))) if code == "E_BAD_INPUT"),
            "{bad:?}: {refused:?}"
        );
    }

    // The peer reacts: the browser's chip counts 1 and is not pressed.
    bob.reactions()
        .react(channel, bobs_msg, THUMBS_UP, true)
        .await
        .unwrap()
        .expect("queued");
    chips_are(&alice, channel, body, vec![(THUMBS_UP, 1, false)]).await;
    chips_are(&bob, channel, body, vec![(THUMBS_UP, 1, true)]).await;

    // The browser clicks the chip: 2, pressed; the peer's own stays pressed and counts 2.
    alice
        .reactions()
        .react(channel, alices_msg, THUMBS_UP, true)
        .await
        .unwrap()
        .expect("queued");
    chips_are(&alice, channel, body, vec![(THUMBS_UP, 2, true)]).await;
    chips_are(&bob, channel, body, vec![(THUMBS_UP, 2, true)]).await;

    // Twice from one person is still one.
    alice
        .reactions()
        .react(channel, alices_msg, THUMBS_UP, true)
        .await
        .unwrap()
        .expect("queued");
    alice
        .reactions()
        .react(channel, alices_msg, "🎉", true)
        .await
        .unwrap()
        .expect("queued");
    chips_are(
        &alice,
        channel,
        body,
        vec![(THUMBS_UP, 2, true), ("🎉", 1, true)],
    )
    .await;

    // Unreact: alice's 👍 and 🎉 go, the peer's 👍 stays.
    for emoji in [THUMBS_UP, "🎉"] {
        alice
            .reactions()
            .react(channel, alices_msg, emoji, false)
            .await
            .unwrap()
            .expect("queued");
    }
    chips_are(&alice, channel, body, vec![(THUMBS_UP, 1, false)]).await;
    chips_are(&bob, channel, body, vec![(THUMBS_UP, 1, true)]).await;
}

/// A second device of one account: signs in the way the app does (password, the authenticator code, the recovery key).
async fn second_device(host: &Host, person: &mut Person, tag: &str) -> Client {
    let root = repo_root().join(format!(
        "target/testing-client-{tag}-{}-second-{}",
        person.username,
        std::process::id()
    ));
    let _ = std::fs::remove_dir_all(&root);
    let cfg: InstanceConfig = Instances::load(&root)
        .expect("an instance list")
        .add(person.cfg.server.clone())
        .expect("an instance");
    let kc = Arc::new(MemoryKeychain::new());
    let mut sign_in = SignIn::new(cfg.clone(), kc.clone()).expect("a sign-in");
    let owed = sign_in
        .login(&person.username, PASSWORD)
        .await
        .expect("the password");
    assert_eq!(owed, Owed::Code);
    sign_in
        .code(&person.next_code(host))
        .await
        .expect("the code");
    sign_in
        .add_device(&person.recovery_key)
        .await
        .expect("the recovery key");
    drop(sign_in);
    tokio::task::spawn_blocking(move || Client::start(cfg, kc).expect("the second device starts"))
        .await
        .unwrap()
}

/// e2e/web/core-worker.spec.ts, "two devices of one account fold edits, reactions, replies, pins and deletes" (its
/// reactions step): the account's second device, which joined the channel after the first, shows the first device's
/// reaction as its own (`mine`), counts it once, and the peer's reaction makes it 2.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn own_second_device_sees_reaction() {
    let (host, mut pa, pb, c) = two_people("own2").await;
    let (alice, bob) = (start(&pa).await, start(&pb).await);
    let channel = general(&alice, c).await;
    general(&bob, c).await;
    alice.open_channel(channel).await.unwrap().expect("opens");
    bob.open_channel(channel).await.unwrap().expect("joins");

    let alice2 = second_device(&host, &mut pa, "own2").await;
    general(&alice2, c).await;
    alice2.open_channel(channel).await.unwrap().expect("joins");

    // A message after the second device joined: both of alice's devices can read it.
    let body = "first words";
    // The first device learns of the second one's entry on the account's device list from the instance: until it has, the
    // core refuses its sends as from an unlisted device.
    for attempt in 0..30 {
        match alice.send_text(channel, body.into()).await.unwrap() {
            Ok(_) => break,
            Err(ClientError::Sync(code)) if code == "E_DEVICE_UNLISTED" && attempt < 29 => {
                tokio::time::sleep(Duration::from_secs(1)).await;
            }
            Err(e) => panic!("queued: {e:?}"),
        }
    }
    let (b, ch) = (body.to_owned(), channel);
    for client in [&alice, &alice2, &bob] {
        let b = b.clone();
        wait_for(client, WAIT, "the message", move |s| {
            s.timelines
                .get(&ch)
                .is_some_and(|t| t.items.iter().any(|i| i.body == b && i.msg_id.is_some()))
        })
        .await;
    }
    let s = alice.state().borrow().clone();
    let msg = message_id(&s, channel, body);

    // Alice's first device reacts: her second device shows it as hers, once.
    alice
        .reactions()
        .react(channel, msg, THUMBS_UP, true)
        .await
        .unwrap()
        .expect("queued");
    chips_are(&alice2, channel, body, vec![(THUMBS_UP, 1, true)]).await;
    chips_are(&bob, channel, body, vec![(THUMBS_UP, 1, false)]).await;

    // The second device reacts with the same emoji: still one (the core folds per user, not per device).
    alice2
        .reactions()
        .react(channel, msg, THUMBS_UP, true)
        .await
        .unwrap()
        .expect("queued");
    // Bob's reaction makes it 2 everywhere; a later message is the positive control that every earlier row is folded.
    bob.reactions()
        .react(channel, msg, THUMBS_UP, true)
        .await
        .unwrap()
        .expect("queued");
    for client in [&alice, &alice2] {
        chips_are(client, channel, body, vec![(THUMBS_UP, 2, true)]).await;
    }
    chips_are(&bob, channel, body, vec![(THUMBS_UP, 2, true)]).await;
}
