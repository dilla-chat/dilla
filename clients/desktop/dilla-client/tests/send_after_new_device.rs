//! A send right after a peer's new device joined the group, against a real dillad (dilla-testhost).
//!
//! The sender's cached device list of the peer predates the new device, so the core refuses to seal to that leaf
//! (`E_DEVICE_UNLISTED`) until a trust refresh ingests the newer list. The web client answers that refusal on its own
//! (controller.ts `sendWithTrustRetry` for the queueing, send.ts `refreshTrustThenDrain` for the sealing): the group's
//! leaves are read again and the message is tried once more. The person never retries.
//!
//! Needs `target/dilla-testhost` and the WASI core (see `tests/sync.rs`); `#[ignore]`d like the other real-server suites
//! (`cargo test -p dilla-client --test send_after_new_device -- --ignored`).

use std::sync::Arc;
use std::time::Duration;

use dilla_client::Client;
use dilla_client::features::attachments::NewFile;
use dilla_client::instances::Instances;
use dilla_client::keychain::MemoryKeychain;
use dilla_client::signin::{Owed, SignIn};
use dilla_client::state::attachments::TrayPhase;
use dilla_client::state::{AppState, ChannelId, CommunityId, TimelineItemState, UserId};
use dilla_client::testing::host::{
    Host, PASSWORD, Person, clear_session, community, repo_root, sign_up, start, start_host,
    wait_for,
};

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
        s.channel_named(CommunityId(community), "general").is_some()
    })
    .await;
    s.channel_named(CommunityId(community), "general")
        .unwrap()
        .id
}

fn shows(s: &AppState, channel: ChannelId, author: [u8; 16], body: &str) -> bool {
    s.timelines.get(&channel).is_some_and(|t| {
        t.items.iter().any(|i| {
            i.state == TimelineItemState::Ok
                && i.sender_user == Some(UserId(author))
                && i.body == body
        })
    })
}

/// Bob signs in on a second device the way the app does (password, authenticator code, recovery key): the device is
/// registered, recovers the account from its sealed backup and publishes the device list that names it.
async fn second_device(host: &Host, bob: &mut Person, tag: &str) -> (Client, Arc<MemoryKeychain>) {
    let root = repo_root().join(format!(
        "target/testing-client-{tag}-bob2-{}",
        std::process::id()
    ));
    let _ = std::fs::remove_dir_all(&root);
    let cfg = Instances::load(&root)
        .unwrap()
        .add(url::Url::parse(&host.public).unwrap())
        .unwrap();
    let kc = Arc::new(MemoryKeychain::new());
    let mut signin = SignIn::new(cfg.clone(), kc.clone()).expect("sign-in");
    assert_eq!(
        signin.login(&bob.username, PASSWORD).await.expect("login"),
        Owed::Code
    );
    // The code reads the host's clock with a blocking request.
    let code = tokio::task::block_in_place(|| bob.next_code(host));
    signin.code(&code).await.expect("code");
    signin
        .add_device(&bob.recovery_key)
        .await
        .expect("the second device is added");
    drop(signin);
    let client = tokio::task::spawn_blocking({
        let (cfg, kc) = (cfg, kc.clone());
        move || Client::start(cfg, kc).expect("the second device starts")
    })
    .await
    .unwrap();
    (client, kc)
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn a_send_right_after_a_new_device_joined_is_delivered() {
    let tag = "newdev";
    let (host, pa, mut pb, c) = two_people(tag).await;
    let a = start(&pa).await;
    let b = start(&pb).await;
    let channel = general(&a, c).await;
    general(&b, c).await;
    a.open_channel(channel).await.unwrap().expect("a opens");
    b.open_channel(channel).await.unwrap().expect("b joins");
    // Alice has read bob's (one-device) list: a first message reaches bob.
    a.send_text(channel, "before".into())
        .await
        .unwrap()
        .expect("queued");
    wait_for(&b, Duration::from_secs(10), "bob reads the first", |s| {
        shows(s, channel, pa.user_id, "before")
    })
    .await;

    // Bob's second device joins the channel's group.
    let (b2, _kc) = second_device(&host, &mut pb, tag).await;
    general(&b2, c).await;
    b2.open_channel(channel)
        .await
        .unwrap()
        .expect("bob's second device joins");

    // Alice sends at once; she never retries.
    a.send_text(channel, "after".into())
        .await
        .unwrap()
        .expect("the send is queued, not refused for the new device");
    for (who, client) in [("bob", &b), ("bob's second device", &b2)] {
        wait_for(
            client,
            Duration::from_secs(20),
            &format!("{who} reads the message sent after the join"),
            |s| shows(s, channel, pa.user_id, "after"),
        )
        .await;
    }
    let s = wait_for(&a, Duration::from_secs(10), "alice's message stored", |s| {
        s.timelines.get(&channel).is_some_and(|t| {
            t.items
                .iter()
                .any(|i| i.body == "after" && i.own && i.seq.is_some())
        })
    })
    .await;
    assert!(
        !s.timelines[&channel]
            .items
            .iter()
            .any(|i| i.state == TimelineItemState::Failed),
        "nothing failed"
    );
}

/// Alice and bob in one channel, bob's first device has read alice's "before", then bob's second device joins. Answers
/// the clients (alice, bob, bob's second device), the channel and alice's "before" message.
async fn after_a_new_device(
    tag: &str,
) -> (
    Host,
    Person,
    Client,
    Client,
    Client,
    Arc<MemoryKeychain>,
    ChannelId,
    dilla_client::state::MessageId,
) {
    let (host, pa, mut pb, c) = two_people(tag).await;
    let a = start(&pa).await;
    let b = start(&pb).await;
    let channel = general(&a, c).await;
    general(&b, c).await;
    a.open_channel(channel).await.unwrap().expect("a opens");
    b.open_channel(channel).await.unwrap().expect("b joins");
    a.send_text(channel, "before".into())
        .await
        .unwrap()
        .expect("queued");
    wait_for(&b, Duration::from_secs(10), "bob reads the first", |s| {
        shows(s, channel, pa.user_id, "before")
    })
    .await;
    let s = wait_for(
        &a,
        Duration::from_secs(10),
        "alice's first is stored",
        |s| {
            s.timelines.get(&channel).is_some_and(|t| {
                t.items
                    .iter()
                    .any(|i| i.body == "before" && i.seq.is_some() && i.msg_id.is_some())
            })
        },
    )
    .await;
    let before = s.timelines[&channel]
        .items
        .iter()
        .find(|i| i.body == "before")
        .and_then(|i| i.msg_id)
        .unwrap();
    let (b2, kc) = second_device(&host, &mut pb, tag).await;
    general(&b2, c).await;
    b2.open_channel(channel)
        .await
        .unwrap()
        .expect("bob's second device joins");
    (host, pa, a, b, b2, kc, channel, before)
}

/// Review I2: a reply sent right after a new device joined goes through the same trust retry as a post (controller.ts
/// `sendWithTrustRetry` wraps every send): it is queued, not refused with `E_DEVICE_UNLISTED`, and both of bob's
/// devices read it.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn a_reply_right_after_a_new_device_joined_is_delivered() {
    let (_host, pa, a, b, b2, _kc, channel, before) = after_a_new_device("newdevreply").await;
    a.threads()
        .reply(channel, before, "reply after".into())
        .await
        .unwrap()
        .expect("the reply is queued, not refused for the new device");
    for (who, client) in [("bob", &b), ("bob's second device", &b2)] {
        wait_for(
            client,
            Duration::from_secs(20),
            &format!("{who} reads the reply sent after the join"),
            |s| shows(s, channel, pa.user_id, "reply after"),
        )
        .await;
    }
}

/// Review I2: a post with a file sent right after a new device joined is queued behind the trust retry too.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn a_post_with_a_file_right_after_a_new_device_joined_is_delivered() {
    let (_host, pa, a, b, b2, _kc, channel, _) = after_a_new_device("newdevfile").await;
    let tray = a.attachments().tray(channel);
    tray.add(vec![NewFile::from_bytes(
        "notes.bin",
        "application/octet-stream",
        vec![7u8; 2048],
    )])
    .await
    .expect("the tray takes the file");
    wait_for(&a, Duration::from_secs(30), "the file is ready", |s| {
        let t = s.attachments.tray(channel);
        t.len() == 1 && t.iter().all(|e| e.phase == TrayPhase::Ready)
    })
    .await;
    let descriptors = tray.take_all().expect("the file is ready");
    a.attachments()
        .send(channel, "file after".into(), descriptors)
        .await
        .unwrap()
        .expect("the post with a file is queued, not refused for the new device");
    for (who, client) in [("bob", &b), ("bob's second device", &b2)] {
        wait_for(
            client,
            Duration::from_secs(20),
            &format!("{who} reads the post with a file sent after the join"),
            |s| shows(s, channel, pa.user_id, "file after"),
        )
        .await;
    }
}
