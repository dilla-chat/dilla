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

#[cfg(feature = "testing")]
/// Bob's device, with the message in its store, goes away; Alice removes it. Answers `(host, bob, channel, seq)`.
async fn removed_while_bob_is_away(tag: &str) -> (Host, Person, ChannelId, u64) {
    let (host, alice, bob, c) = two_people(tag).await;
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
    stop(b).await;
    stop(a).await;
    assert_eq!(remove(&host, &alice, channel, seq).await, 200);
    // Past the instance's resume window, so nothing it parked for Bob reaches his next connection.
    advance_clock(&host, 150).await;
    (host, bob, channel, seq)
}

/// Every snapshot `client` publishes until `pred` holds (at most `within`), each checked to hold no removed body.
#[cfg(feature = "testing")]
async fn every_snapshot_until(
    snapshots: &mut tokio::sync::mpsc::UnboundedReceiver<std::sync::Arc<AppState>>,
    within: Duration,
    what: &str,
    pred: impl Fn(&AppState) -> bool,
) -> (std::sync::Arc<AppState>, usize) {
    let mut seen = 0;
    let deadline = tokio::time::Instant::now() + within;
    loop {
        let s = tokio::time::timeout_at(deadline, snapshots.recv())
            .await
            .unwrap_or_else(|_| panic!("timed out waiting for {what} after {seen} snapshots"))
            .expect("the client publishes");
        seen += 1;
        body_nowhere(&s);
        if pred(&s) {
            return (s, seen);
        }
    }
}

#[cfg(feature = "testing")]
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn removal_while_offline_is_a_tombstone_after_restart() {
    let (_host, bob, channel, seq) = removed_while_bob_is_away("rm-offline").await;
    // Every snapshot from the restart on (the first included): none holds the plaintext, and the first timeline
    // shows the tombstone.
    let (b, mut snapshots) = start_observed(&bob).await;
    let (s, _) = every_snapshot_until(
        &mut snapshots,
        Duration::from_secs(30),
        "bob's timeline",
        |s| s.timelines.contains_key(&channel),
    )
    .await;
    assert!(
        tombstoned(&s, channel, seq),
        "{:#?}",
        s.timelines.get(&channel)
    );
    // And it stays one (the store's record, not a frame), across another restart.
    tokio::time::sleep(Duration::from_secs(2)).await;
    while let Ok(s) = snapshots.try_recv() {
        body_nowhere(&s);
    }
    stop(b).await;
    let (_b, mut snapshots) = start_observed(&bob).await;
    let (s, _) = every_snapshot_until(
        &mut snapshots,
        Duration::from_secs(30),
        "bob's timeline again",
        |s| s.timelines.contains_key(&channel),
    )
    .await;
    assert!(tombstoned(&s, channel, seq));
}

/// Review fix (round 1): the first read of the feed after the restart fails. The channel's timeline is not published
/// (it shows as loading) in any snapshot until the read is retried (5 s, the key-package backoff), and then it shows the
/// tombstone; the plaintext appears in no snapshot at all.
#[cfg(feature = "testing")]
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn a_failed_feed_read_never_publishes_the_body_and_is_retried() {
    let (_host, bob, channel, seq) = removed_while_bob_is_away("rm-retry").await;
    let group = {
        let core = open_store(&bob.cfg, bob.kc.as_ref()).unwrap();
        GroupRow::decode_all(&core.groups().unwrap())
            .unwrap()
            .into_iter()
            .find(|r| r.kind == 0 && r.target_id == channel.0)
            .expect("bob holds the channel's group")
            .group_id
    };
    dilla_client::sync::removals::faults::fail_next_feed_reads(group, 1);
    let started = std::time::Instant::now();
    let (_b, mut snapshots) = start_observed(&bob).await;
    // The structure (the channel list) is published while the feed is unread, without the channel's timeline.
    let (_, before) = every_snapshot_until(
        &mut snapshots,
        Duration::from_secs(30),
        "the channel list",
        |s| s.channel(channel).is_some(),
    )
    .await;
    let (s, after) = every_snapshot_until(
        &mut snapshots,
        Duration::from_secs(40),
        "the tombstone",
        |s| tombstoned(s, channel, seq),
    )
    .await;
    assert!(
        started.elapsed() >= Duration::from_secs(5),
        "the timeline waited for the retry"
    );
    assert!(
        before + after >= 2,
        "every snapshot was checked: {before} + {after}"
    );
    body_nowhere(&s);
}

/// Bob's group for `channel`, read from his store (his client stopped).
#[cfg(feature = "testing")]
fn bob_group(bob: &Person, channel: ChannelId) -> [u8; 16] {
    let core = open_store(&bob.cfg, bob.kc.as_ref()).unwrap();
    GroupRow::decode_all(&core.groups().unwrap())
        .unwrap()
        .into_iter()
        .find(|r| r.kind == 0 && r.target_id == channel.0)
        .expect("bob holds the channel's group")
        .group_id
}

/// Review fix (round 2): a feed the instance refuses for good (a permanent 403) is terminal. The channel shows that
/// state (a typed reason in `messages.unavailable`, no timeline, no plaintext in any snapshot) and the read is not
/// retried: no further feed read is made over the whole backoff schedule's first steps.
#[cfg(feature = "testing")]
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn a_refused_feed_is_the_channels_state_and_is_not_retried() {
    use dilla_client::state::messages::TimelineUnavailable;
    use dilla_client::sync::removals::faults;
    let (_host, bob, channel, _seq) = removed_while_bob_is_away("rm-refused").await;
    let group = bob_group(&bob, channel);
    faults::fail_next_feed_reads_with(group, 1_000, 403, "E_FORBIDDEN");
    let (_b, mut snapshots) = start_observed(&bob).await;
    let (s, _) = every_snapshot_until(
        &mut snapshots,
        Duration::from_secs(30),
        "the channel's refused state",
        |s| s.messages.unavailable.contains_key(&channel),
    )
    .await;
    assert_eq!(
        s.messages.unavailable[&channel],
        TimelineUnavailable::FeedRefused {
            status: 403,
            code: "E_FORBIDDEN".into()
        }
    );
    assert!(!s.timelines.contains_key(&channel), "no timeline is shown");
    let reads = faults::failed_reads(group);
    // Past the first three retry steps (5 s, 10 s): nothing reads the feed again.
    tokio::time::sleep(Duration::from_secs(16)).await;
    while let Ok(s) = snapshots.try_recv() {
        body_nowhere(&s);
        assert!(s.messages.unavailable.contains_key(&channel));
    }
    assert_eq!(
        faults::failed_reads(group),
        reads,
        "no retry of a refused feed"
    );
    drop(_b);
}

/// Review fix (round 2): the feed answers the 404 of a lost leaf on an active group. As engine.ts `openChannel` does,
/// the group is resynced and its feed read once more, at once (not after the 5 s backoff), and the message is a
/// tombstone; no snapshot shows its plaintext.
#[cfg(feature = "testing")]
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn a_lost_leaf_is_resynced_and_the_feed_read_again() {
    use dilla_client::sync::removals::faults;
    let (_host, bob, channel, seq) = removed_while_bob_is_away("rm-lost").await;
    let group = bob_group(&bob, channel);
    faults::fail_next_feed_reads_with(group, 1, 404, "E_NOT_FOUND");
    let started = std::time::Instant::now();
    let (_b, mut snapshots) = start_observed(&bob).await;
    let (s, _) = every_snapshot_until(
        &mut snapshots,
        Duration::from_secs(30),
        "the tombstone",
        |s| tombstoned(s, channel, seq),
    )
    .await;
    assert_eq!(faults::failed_reads(group), 1, "the 404 was answered once");
    assert!(
        started.elapsed() < Duration::from_secs(5),
        "read again after the resync, not after the backoff ({:?})",
        started.elapsed()
    );
    assert!(!s.messages.unavailable.contains_key(&channel));
    drop(_b);
}

/// Review fix (round 3, Jonas 2026-11-13: history stays hidden until the removals are read, stricter than the web): Bob's
/// group is waiting for a resync (state 3) when he comes back, and its store still holds the message removed while he
/// was away. No snapshot shows a timeline for the channel (nor the plaintext) until the group is active again and its
/// feed is read; then the first timeline shows the tombstone. Meanwhile the channel shows the group's own state.
#[cfg(feature = "testing")]
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn a_resyncing_group_stays_hidden_until_its_feed_is_read() {
    let (_host, bob, channel, seq) = removed_while_bob_is_away("rm-resync").await;
    let group = bob_group(&bob, channel);
    {
        // The group as a lost leaf leaves it: waiting for a resync.
        let conn = dilla_client::store::open_connection(&bob.cfg, bob.kc.as_ref()).unwrap();
        let conn = conn.lock().unwrap();
        let n = conn
            .execute(
                "UPDATE app_groups SET state=3 WHERE group_id=?1",
                [group.as_slice()],
            )
            .unwrap();
        assert_eq!(n, 1);
    }
    let started = std::time::Instant::now();
    let (_b, mut snapshots) = start_observed(&bob).await;
    let mut seen = 0;
    let deadline = tokio::time::Instant::now() + Duration::from_secs(45);
    loop {
        let s = tokio::time::timeout_at(deadline, snapshots.recv())
            .await
            .unwrap_or_else(|_| panic!("no tombstone after {seen} snapshots"))
            .unwrap();
        seen += 1;
        body_nowhere(&s);
        if let Some(t) = s.timelines.get(&channel) {
            assert!(
                tombstoned(&s, channel, seq),
                "the first timeline shown is after the feed read: {t:#?}"
            );
            break;
        }
    }
    assert!(seen >= 2, "{seen} snapshots");
    // The group's activation reads its feed at once: no wait for the publisher's first backoff tick (5 s).
    assert!(
        started.elapsed() < Duration::from_secs(5),
        "shown {:?} after the start",
        started.elapsed()
    );
}
