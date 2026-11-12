//! Task 2 (screens plan): a moderator's signed removal against a real dillad, mirroring
//! `e2e/web/moderation-removal-recovery.spec.ts`. Alice owns the community (so she holds `manage_messages`); Bob writes
//! the message. Alice signs the removal proof with her device key (`ClientCore::sign_removal`, from her store) and posts
//! it to `POST /v1/groups/{group}/moderation/removals` over a gateway socket, as the web client does. In both tests the
//! removed body ends up in neither Bob's `AppState` nor any `TimelineItem.body`.

#[path = "support/world.rs"]
mod world;

use std::time::Duration;

use dilla_client::Client;
use dilla_client::state::{AppState, ChannelId, TimelineItemState};
use dilla_client::store::open_store;
use dilla_client::sync::GroupRow;
use world::*;

const BODY: &str = "late-history removal target";

/// The message's seq in `client`'s timeline, once it is stored.
async fn stored_seq(client: &Client, channel: ChannelId) -> u64 {
    let s = wait_for(client, Duration::from_secs(20), "the message stored", |s| {
        s.timelines
            .get(&channel)
            .is_some_and(|t| t.items.iter().any(|i| i.body == BODY && i.seq.is_some()))
    })
    .await;
    s.timelines[&channel]
        .items
        .iter()
        .find(|i| i.body == BODY)
        .and_then(|i| i.seq)
        .unwrap()
}

/// Alice's removal of `seq` in `channel`'s group: the proof signed from her store (her client is stopped), then posted
/// over a socket of her sign-up session. Answers the HTTP status of the post.
async fn remove(host: &Host, alice: &Person, channel: ChannelId, seq: u64) -> u16 {
    let now = host_now(host).await;
    let (group, body) = {
        let mut core = open_store(&alice.cfg, alice.kc.as_ref()).unwrap();
        let rows = GroupRow::decode_all(&core.groups().unwrap()).unwrap();
        let group = rows
            .iter()
            .find(|r| r.kind == 0 && r.target_id == channel.0 && r.state == 2)
            .expect("alice holds the channel's group")
            .group_id;
        let mut request = [0u8; 16];
        getrandom::fill(&mut request).unwrap();
        let body = core
            .sign_removal(&request, &group, seq, now + 30, now)
            .expect("signed removal");
        (group, body)
    };
    let socket = Socket::open(host, alice).await;
    let (status, _) = socket
        .request(
            "POST",
            format!("/v1/groups/{}/moderation/removals", hex(&group)),
            body,
        )
        .await;
    status
}

/// The removed message is a tombstone, and its plaintext is nowhere in the published state.
fn tombstoned(s: &AppState, channel: ChannelId, seq: u64) -> bool {
    let Some(t) = s.timelines.get(&channel) else {
        return false;
    };
    let gone = t
        .items
        .iter()
        .filter(|i| i.seq == Some(seq))
        .all(|i| i.state == TimelineItemState::Deleted && i.body.is_empty());
    gone && t.items.iter().all(|i| !i.body.contains(BODY))
}

fn body_nowhere(s: &AppState) {
    assert!(
        !format!("{s:?}").contains(BODY),
        "the removed body is still in the state"
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn removal_reaches_live_client_as_tombstone() {
    let (host, alice, bob, c) = two_people("rm-live").await;
    let a = start(&alice).await;
    let b = start(&bob).await;
    let channel = general(&a, c).await;
    general(&b, c).await;
    both_in(&a, &b, channel).await;
    b.send_text(channel, BODY.into())
        .await
        .unwrap()
        .expect("queued");
    let seq = stored_seq(&b, channel).await;
    stored_seq(&a, channel).await;
    // Alice signs from her store: her client lets go of it first.
    stop(a).await;
    assert_eq!(
        remove(&host, &alice, channel, seq).await,
        200,
        "the removal is accepted"
    );
    let s = wait_for(&b, Duration::from_secs(30), "bob's tombstone", |s| {
        tombstoned(s, channel, seq)
    })
    .await;
    body_nowhere(&s);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn removal_while_offline_is_a_tombstone_after_restart() {
    let (host, alice, bob, c) = two_people("rm-offline").await;
    let a = start(&alice).await;
    let b = start(&bob).await;
    let channel = general(&a, c).await;
    general(&b, c).await;
    both_in(&a, &b, channel).await;
    b.send_text(channel, BODY.into())
        .await
        .unwrap()
        .expect("queued");
    let seq = stored_seq(&b, channel).await;
    stored_seq(&a, channel).await;
    // Bob's device goes away with the message in its store; the removal is made while it is gone.
    stop(b).await;
    stop(a).await;
    assert_eq!(remove(&host, &alice, channel, seq).await, 200);
    // Past the instance's resume window, so nothing it parked for Bob reaches his next connection.
    advance_clock(&host, 150).await;
    let b = start(&bob).await;
    // From his first published timeline on, the message is a tombstone: never its plaintext.
    let mut rx = b.state();
    let s = wait_for(&b, Duration::from_secs(30), "bob's timeline", |s| {
        s.timelines.contains_key(&channel)
    })
    .await;
    body_nowhere(&s);
    assert!(
        tombstoned(&s, channel, seq),
        "{:#?}",
        s.timelines.get(&channel)
    );
    // And it stays one (the store's record, not a frame).
    tokio::time::sleep(Duration::from_secs(2)).await;
    body_nowhere(&rx.borrow_and_update());
    stop(b).await;
    let b = start(&bob).await;
    let s = wait_for(&b, Duration::from_secs(30), "bob's timeline again", |s| {
        s.timelines.contains_key(&channel)
    })
    .await;
    assert!(tombstoned(&s, channel, seq));
    body_nowhere(&s);
}
