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
use dilla_client::features::attachments::{Descriptor, NewFile};
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

/// Alice and bob in one channel, bob's first device has read alice's "before", then bob's second device joins. With
/// `file`, alice's tray holds one ready file before the join, so the post that carries it can go at once after it.
/// Answers the clients (alice, bob, bob's second device), the channel, alice's "before" message and the file.
async fn after_a_new_device(
    tag: &str,
    file: bool,
) -> (
    Host,
    Person,
    Client,
    Client,
    Client,
    Arc<MemoryKeychain>,
    ChannelId,
    dilla_client::state::MessageId,
    Vec<Descriptor>,
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
    let mut descriptors = Vec::new();
    if file {
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
        descriptors = tray.take_all().expect("the file is ready");
    }
    let (b2, kc) = second_device(&host, &mut pb, tag).await;
    general(&b2, c).await;
    b2.open_channel(channel)
        .await
        .unwrap()
        .expect("bob's second device joins");
    (host, pa, a, b, b2, kc, channel, before, descriptors)
}

/// Review I2: a reply sent right after a new device joined goes through the same trust retry as a post (controller.ts
/// `sendWithTrustRetry` wraps every send): it is queued, not refused with `E_DEVICE_UNLISTED`, and both of bob's
/// devices read it.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn a_reply_right_after_a_new_device_joined_is_delivered() {
    let (_host, pa, a, b, b2, _kc, channel, before, _) =
        after_a_new_device("newdevreply", false).await;
    let retries = a.trust_retries();
    a.threads()
        .reply(channel, before, "reply after".into())
        .await
        .unwrap()
        .expect("the reply is queued, not refused for the new device");
    assert_eq!(
        a.trust_retries(),
        retries + 1,
        "the reply was refused for the new device once and queued again"
    );
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
    let (_host, pa, a, b, b2, _kc, channel, _, descriptors) =
        after_a_new_device("newdevfile", true).await;
    let retries = a.trust_retries();
    a.attachments()
        .send(channel, "file after".into(), descriptors, None)
        .await
        .unwrap()
        .expect("the post with a file is queued, not refused for the new device");
    assert_eq!(
        a.trust_retries(),
        retries + 1,
        "the post was refused for the new device once and queued again"
    );
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

/// e2e/web/installed-upgrade-account.spec.ts, its recovery half, with a native peer: bob's only device signs itself out
/// with the recovery key (the instance proposes the Remove of its leaf, alice commits it), then a new device enrols by
/// password, code and key and joins at the blank leaf the old device left. Alice's cached list of bob is still the one
/// that named the old device, so her pass of the group's leaves is keyed exactly as the old device's was: the epoch
/// change must make her read that leaf again (controller.ts `onGroupChanged`), or she holds the new device's messages
/// as `E_DEVICE_UNLISTED` for good and her own sends to the channel stay blocked.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn a_device_enrolled_after_a_self_revocation_is_read_and_sent_to() {
    let tag = "selfrevoke";
    let (host, pa, mut pb, c) = two_people(tag).await;
    let a = start(&pa).await;
    let b = start(&pb).await;
    let channel = general(&a, c).await;
    general(&b, c).await;
    a.open_channel(channel).await.unwrap().expect("a opens");
    b.open_channel(channel).await.unwrap().expect("b joins");
    b.send_text(channel, "from the first device".into())
        .await
        .unwrap()
        .expect("queued");
    wait_for(
        &a,
        Duration::from_secs(20),
        "alice reads bob's first device",
        |s| shows(s, channel, pb.user_id, "from the first device"),
    )
    .await;

    // Bob's only device signs itself out with the recovery key.
    let key = zeroize::Zeroizing::new(pb.recovery_key.clone());
    b.devices()
        .sign_out_and_remove(key)
        .await
        .expect("the client answers")
        .expect("the sign-out succeeds");
    wait_for(&b, Duration::from_secs(10), "the sign-out", |s| {
        s.profile.signed_out
    })
    .await;
    b.stop();

    // A new device enrols with password, code and key, joins the channel and sends.
    let (b2, _kc) = second_device(&host, &mut pb, tag).await;
    general(&b2, c).await;
    b2.open_channel(channel)
        .await
        .unwrap()
        .expect("bob's new device joins");
    b2.send_text(channel, "from the new device".into())
        .await
        .unwrap()
        .expect("queued");
    wait_for(
        &a,
        Duration::from_secs(30),
        "alice reads bob's new device",
        |s| shows(s, channel, pb.user_id, "from the new device"),
    )
    .await;

    // Alice's own send is not blocked, and the new device reads it.
    a.send_text(channel, "to the new device".into())
        .await
        .unwrap()
        .expect("alice's send is queued, not blocked");
    wait_for(
        &b2,
        Duration::from_secs(30),
        "the new device reads alice",
        |s| shows(s, channel, pa.user_id, "to the new device"),
    )
    .await;
}

/// Whether the store holds its own device list as published (the identity's sixth element).
fn list_published(p: &Person) -> bool {
    let core = dilla_client::store::open_store(&p.cfg, p.kc.as_ref()).expect("the store opens");
    dilla_client::sync::api::Identity::decode(&core.identity().unwrap())
        .expect("an identity")
        .list_published
}

/// signup.ts `publishDeviceList`'s 409 branch at the start: the device's list PUT landed but its answer was lost, so the
/// store still holds the list as unpublished. The start publishes it again; the instance answers 409 for the version it
/// already holds, and the start reads the served list, has the core verify and adopt it, finds this device listed and
/// goes on, never refusing the device as a list race.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn a_start_after_a_lost_list_answer_adopts_the_served_list() {
    let host = tokio::task::spawn_blocking(|| start_host("lostlist", "127.0.0.1:0", None))
        .await
        .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let p = dilla_client::testing::host::sign_up_answer_lost(
        &host, &invite, "alice", "alice", "lostlist",
    )
    .await;
    let p = tokio::task::spawn_blocking(move || {
        clear_session(&p);
        p
    })
    .await
    .unwrap();
    assert!(
        !list_published(&p),
        "the lost answer left the list unpublished"
    );
    let client = start(&p).await;
    wait_for(&client, Duration::from_secs(30), "online", |s| {
        s.connection.status == dilla_client::state::ConnectionStatus::Online
    })
    .await;
    tokio::task::spawn_blocking(move || client.stop())
        .await
        .unwrap();
    tokio::time::sleep(Duration::from_millis(300)).await;
    assert!(list_published(&p), "the adopted list is the published one");
    drop(host);
}

/// Whether the store at `cfg` holds its own device list as published.
fn published_at(cfg: &dilla_client::seams::InstanceConfig, kc: &MemoryKeychain) -> bool {
    let core = dilla_client::store::open_store(cfg, kc).expect("the store opens");
    dilla_client::sync::api::Identity::decode(&core.identity().unwrap())
        .expect("an identity")
        .list_published
}

/// Wave 2 H (recovery re-review): the recovery path's own lost answer. A new device signs in with password, code and
/// recovery key; its list PUT lands but the answer is lost, so the key step leaves the start to finish
/// (`Enrolled::FinishAtStart`). The start publishes again, the instance answers 409 for the version it holds, and the
/// start has the core verify and adopt the served list, finds this device on it and goes online.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn a_recovery_start_after_a_lost_list_answer_adopts_the_served_list() {
    let tag = "reclost";
    let (host, _pa, mut pb, _c) = two_people(tag).await;
    let (cfg, kc, root) = dilla_client::testing::host::second_device_faulted(
        &host,
        &mut pb,
        tag,
        dilla_client::signin::ListPutFault::AnswerLost,
    )
    .await;
    assert!(
        !published_at(&cfg, &kc),
        "the lost answer left the list unpublished"
    );
    let client = tokio::task::spawn_blocking({
        let (cfg, kc) = (cfg.clone(), kc.clone());
        move || Client::start(cfg, kc)
    })
    .await
    .unwrap()
    .expect("the start adopts the served list and goes on");
    wait_for(&client, Duration::from_secs(30), "online", |s| {
        s.connection.status == dilla_client::state::ConnectionStatus::Online
    })
    .await;
    tokio::task::spawn_blocking(move || client.stop())
        .await
        .unwrap();
    tokio::time::sleep(Duration::from_millis(300)).await;
    assert!(
        published_at(&cfg, &kc),
        "the adopted list is the published one"
    );
    let _ = std::fs::remove_dir_all(root);
}

/// Wave 2 H (recovery re-review): the unlisted refusal at the start, which the web treats as a revocation
/// (controller.ts `enterReady`: `E_DEVICE_UNLISTED` at phase 2 is `enterRevoked`). Bob's new device signs in by recovery
/// key, and its list PUT never reaches the instance. Meanwhile his first device removes a third one, publishing a list of
/// the same version that does not name the new device. The new device's start publishes, gets 409, has the core verify
/// and adopt the served list, and finds itself left out: the start fails as revoked, never as a passing refusal code.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn a_start_left_off_the_served_list_is_revoked() {
    let tag = "recunlisted";
    let (host, _pa, mut pb, _c) = two_people(tag).await;
    let b1 = start(&pb).await;
    let (third, _third_kc) = second_device(&host, &mut pb, tag).await;
    let third_device = third.state().borrow().account.device_id.expect("a device");
    tokio::task::spawn_blocking(move || third.stop())
        .await
        .unwrap();
    let (cfg, kc, root) = dilla_client::testing::host::second_device_faulted(
        &host,
        &mut pb,
        tag,
        dilla_client::signin::ListPutFault::NeverSent,
    )
    .await;
    b1.devices()
        .revoke(
            third_device,
            Some(zeroize::Zeroizing::new(pb.recovery_key.clone())),
        )
        .await
        .expect("the client answers")
        .expect("the first device removes the third");
    let started = tokio::task::spawn_blocking({
        let (cfg, kc) = (cfg.clone(), kc.clone());
        move || Client::start(cfg, kc)
    })
    .await
    .unwrap();
    match started {
        Err(dilla_client::ClientError::Revoked) => {}
        Err(e) => panic!("the unlisted start fails as revoked, not {e:?}"),
        Ok(_) => panic!("a device the served list leaves out never starts"),
    }
    tokio::task::spawn_blocking(move || b1.stop())
        .await
        .unwrap();
    let _ = std::fs::remove_dir_all(root);
}
