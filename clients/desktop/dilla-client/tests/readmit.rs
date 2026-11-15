//! A kicked device that is admitted again receives again, against a real dillad (e2e/web/membership.spec.ts `a kicked
//! browser is told and loses the server, and after re-admission messages flow again`, client half). Alice owns the
//! community and kicks Bob (the production kick, on her authority, through the testhost's `/debug/kick`); Bob's client
//! loses the server; Alice invites him again and he joins; Bob's channel group leaves its resync and a message Alice
//! sends afterwards reaches him.

#[path = "support/world.rs"]
mod world;

use std::time::Duration;

use std::sync::Arc;

use dilla_client::Client;
use dilla_client::state::{AppState, ChannelGroupState, ChannelId, CommunityId, TimelineItemState};
use dilla_client::store::open_store;
use dilla_client::sync::GroupRow;
use world::*;

async fn sends(client: &Client, channel: ChannelId, body: &str) {
    client
        .send_text(channel, body.into())
        .await
        .unwrap()
        .expect("queued");
}

async fn receives(
    client: &Client,
    channel: ChannelId,
    author: [u8; 16],
    body: &str,
    within: Duration,
) {
    wait_for(client, within, &format!("{body:?} received"), |s| {
        bodies_from(s, channel, author).iter().any(|b| b == body)
    })
    .await;
}

/// The production kick (`DELETE /v1/communities/{id}/members/{user}` on the actor's authority).
async fn kick(host: &Host, actor: &Person, target: &Person, community: [u8; 16]) {
    let res = reqwest::Client::new()
        .post(format!("{}/debug/kick", host.control))
        .header("content-type", "application/json")
        .body(format!(
            r#"{{"actor":"{}","target":"{}","community":"{}"}}"#,
            hex(&actor.device_id),
            hex(&target.device_id),
            hex(&community)
        ))
        .send()
        .await
        .unwrap();
    let status = res.status();
    assert_eq!(status.as_u16(), 204, "kick: {}", res.text().await.unwrap());
}

/// `owner` invites `who` again and `who` joins with the invite, both over their sign-up sessions.
async fn readmit(host: &Host, owner: &Person, who: &Person, community: [u8; 16]) {
    let public = host.public.clone();
    let (o, w) = (
        (owner.device_id, owner.token.clone()),
        (who.device_id, who.token.clone()),
    );
    tokio::task::spawn_blocking(move || {
        use dilla_core::ids::DeviceId;
        use dilla_testkit::HttpDs;
        let mut ds = HttpDs::with_session(&public, DeviceId::from_bytes(o.0), o.1).unwrap();
        let code = ds.create_invite(&community, 1, 3600).unwrap();
        let mut ds = HttpDs::with_session(&public, DeviceId::from_bytes(w.0), w.1).unwrap();
        ds.join_community(&community, &code).unwrap();
    })
    .await
    .unwrap();
}

/// Whether `p`'s device holds a leaf in `group`: the instance serves it the group's messages (a removed leaf is 404).
async fn holds_leaf(host: &Host, p: &Person, group: [u8; 16]) -> bool {
    let socket = Socket::open(host, p).await;
    let (status, _) = socket
        .request(
            "GET",
            format!("/v1/groups/{}/messages?from=0&limit=1", hex(&group)),
            Vec::new(),
        )
        .await;
    assert!(status == 200 || status == 404, "group read: {status}");
    status == 200
}

/// Alice and Bob in one channel's group, a message of Bob's read by Alice; then Alice kicks Bob, his client loses the
/// server and Alice's client commits the Remove of his leaf. Answers `(host, alice, bob, a, b, community, channel, group)`.
async fn kicked(
    tag: &str,
) -> (
    Host,
    Person,
    Person,
    Client,
    Client,
    [u8; 16],
    ChannelId,
    [u8; 16],
) {
    let (host, alice, bob, c) = two_people(tag).await;
    let cid = CommunityId(c);
    let a = start(&alice).await;
    let b = start(&bob).await;
    let ch = general(&a, c).await;
    general(&b, c).await;
    both_in(&a, &b, ch).await;
    sends(&b, ch, "before the kick").await;
    receives(
        &a,
        ch,
        bob.user_id,
        "before the kick",
        Duration::from_secs(30),
    )
    .await;
    b.refresh();
    let group = wait_for(
        &b,
        Duration::from_secs(30),
        "the channel's group listed",
        |s| s.channel(ch).is_some_and(|x| x.text_group_id.is_some()),
    )
    .await
    .channel(ch)
    .unwrap()
    .text_group_id
    .unwrap()
    .0;

    kick(&host, &alice, &bob, c).await;
    wait_for(&b, Duration::from_secs(30), "bob loses the server", |s| {
        !s.communities.iter().any(|x| x.id == cid)
    })
    .await;
    // Alice's client commits the instance's Remove of Bob's leaf, which never reaches Bob (membership.spec.ts waits for
    // the group's devices to be one).
    poll(Duration::from_secs(30), "bob's leaf removed", || async {
        (!holds_leaf(&host, &bob, group).await).then_some(())
    })
    .await;
    (host, alice, bob, a, b, c, ch, group)
}

/// Bob is admitted again and his client lists the server again.
async fn readmitted(
    host: &Host,
    alice: &Person,
    bob: &Person,
    b: &Client,
    c: [u8; 16],
    ch: ChannelId,
) {
    readmit(host, alice, bob, c).await;
    b.refresh();
    wait_for(
        b,
        Duration::from_secs(30),
        "bob lists the server again",
        |s| s.communities.iter().any(|x| x.id == CommunityId(c)) && s.channel(ch).is_some(),
    )
    .await;
}

/// The channel is active again (not left in `Resync` or `NotMember`, which the view shows as unwritable).
async fn active(b: &Client, ch: ChannelId) {
    wait_for(
        b,
        Duration::from_secs(10),
        "bob's channel group active",
        |s| {
            s.channel(ch)
                .is_some_and(|x| x.group == ChannelGroupState::Active)
                && s.timelines
                    .get(&ch)
                    .is_some_and(|t| t.group == ChannelGroupState::Active)
        },
    )
    .await;
}

/// membership.spec.ts: re-admitted, Bob opens the channel; the open meets the lost leaf and rejoins the group by
/// external commit. Messages flow both ways, and the rejoin ends the resync the open reported.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn a_readmitted_device_leaves_resync_and_receives_again() {
    let (host, alice, bob, a, b, c, ch, group) = kicked("readmit").await;
    readmitted(&host, &alice, &bob, &b, c, ch).await;
    b.open_channel(ch)
        .await
        .unwrap()
        .expect("bob opens the channel again");
    // The open rejoined the group (membership.spec.ts waits for the group's two devices).
    assert!(
        holds_leaf(&host, &bob, group).await,
        "bob rejoined the group"
    );

    sends(&a, ch, "after the readmission").await;
    receives(
        &b,
        ch,
        alice.user_id,
        "after the readmission",
        Duration::from_secs(30),
    )
    .await;
    sends(&b, ch, "bob after the readmission").await;
    receives(
        &a,
        ch,
        bob.user_id,
        "bob after the readmission",
        Duration::from_secs(30),
    )
    .await;
    active(&b, ch).await;
}

/// engine.ts `rejoined`: Bob's device came back while kicked, so its ready met the lost leaf and its resync was refused
/// (not-member, tried for this ready). Re-admitted, the group resyncs at once, without a reconnect and without Bob
/// opening the channel; a message Alice sends then reaches him.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn a_resync_refused_while_kicked_is_tried_again_on_readmission() {
    let (host, alice, bob, a, b, c, ch, group) = kicked("readmit-ready").await;
    stop(b).await;
    let b = start(&bob).await;
    // The ready's catch-up meets the 404 of the lost leaf and its resync is refused (Bob is not a member). No state
    // shows a channel of a server Bob is not in, so the engine's test probe says when: readmission comes only after.
    poll(
        Duration::from_secs(30),
        "the ready's resync refused",
        || async { (dilla_client::sync::probes::resyncs_refused(group) >= 1).then_some(()) },
    )
    .await;
    assert!(
        !holds_leaf(&host, &bob, group).await,
        "the resync was refused"
    );

    readmitted(&host, &alice, &bob, &b, c, ch).await;
    poll(
        Duration::from_secs(30),
        "bob rejoined the group",
        || async { holds_leaf(&host, &bob, group).await.then_some(()) },
    )
    .await;
    sends(&a, ch, "after the readmission").await;
    receives(
        &b,
        ch,
        alice.user_id,
        "after the readmission",
        Duration::from_secs(30),
    )
    .await;
    active(&b, ch).await;
}

const MODERATED: &str = "before the kick";

/// Alice's signed removal of `seq` in `group` (removals.rs `remove`): the proof signed from her store (her client is
/// stopped), posted over a socket of her sign-up session. Answers the HTTP status.
async fn remove(host: &Host, alice: &Person, group: [u8; 16], seq: u64) -> u16 {
    let now = host_now(host).await;
    let body = {
        let mut core = open_store(&alice.cfg, alice.kc.as_ref()).unwrap();
        let rows = GroupRow::decode_all(&core.groups().unwrap()).unwrap();
        assert!(rows.iter().any(|r| r.group_id == group && r.state == 2));
        let mut request = [0u8; 16];
        getrandom::fill(&mut request).unwrap();
        core.sign_removal(&request, &group, seq, now + 30, now)
            .expect("signed removal")
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

fn shows_moderated(s: &AppState) -> bool {
    format!("{s:?}").contains(MODERATED)
}

/// Jonas (2026-11-13), stricter than the web: history stays hidden until the removal feed is read. A message a
/// moderator removed while Bob was kicked is still in his store; once he is admitted again, no snapshot shows its
/// plaintext, and the channel's timeline after the rejoin shows its tombstone.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn a_message_removed_while_kicked_never_shows_after_readmission() {
    let (host, alice, bob, a, b, c, ch, group) = kicked("readmit-removed").await;
    // The seq of the message Bob wrote before the kick (gone from his state with the server). Its author is Bob: a
    // moderator removes only a message of someone below them in the hierarchy.
    let seq = {
        let s = wait_for(&a, Duration::from_secs(20), "alice's message stored", |s| {
            s.timelines.get(&ch).is_some_and(|t| {
                t.items
                    .iter()
                    .any(|i| i.body == MODERATED && i.seq.is_some())
            })
        })
        .await;
        s.timelines[&ch]
            .items
            .iter()
            .find(|i| i.body == MODERATED)
            .and_then(|i| i.seq)
            .expect("alice holds the message")
    };
    assert!(!shows_moderated(&b.state().borrow()), "bob lost the server");
    stop(a).await;
    assert_eq!(
        remove(&host, &alice, group, seq).await,
        200,
        "the removal is accepted"
    );

    let mut snapshots = b.snapshots();
    readmit(&host, &alice, &bob, c).await;
    b.refresh();
    let deadline = tokio::time::Instant::now() + Duration::from_secs(45);
    let mut seen = 0;
    loop {
        let s: Arc<AppState> = tokio::time::timeout_at(deadline, snapshots.recv())
            .await
            .unwrap_or_else(|_| panic!("no tombstone after {seen} snapshots"))
            .unwrap();
        seen += 1;
        assert!(
            !shows_moderated(&s),
            "snapshot {seen} shows the removed message"
        );
        if s.timelines.get(&ch).is_some_and(|t| {
            t.items
                .iter()
                .any(|i| i.seq == Some(seq) && i.state == TimelineItemState::Deleted)
        }) {
            break;
        }
    }
    assert!(
        holds_leaf(&host, &bob, group).await,
        "bob rejoined the group"
    );
}
