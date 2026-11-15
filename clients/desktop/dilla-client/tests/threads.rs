//! Task 11's client tests for the threads area (standard procedure S2), against a real dillad (dilla-testhost).
//!
//! Ports `e2e/web/threads.spec.ts` (tests 1 to 3; 4 and 5 need search and pins, Task 28) and the reply part of
//! `e2e/web/conversation.spec.ts`. Two Rust clients, A and B, play Ada and Björn: the web spec's browser and its native
//! peer. Needs `target/dilla-testhost` and the WASI core, like `tests/sync.rs`; the tests are `#[ignore]`d.

use std::time::Duration;

use dilla_client::Client;
use dilla_client::state::threads::OpenThread;
use dilla_client::state::{AppState, ChannelId, CommunityId, MessageId, TimelineItemState, UserId};
use dilla_client::testing::host::{
    Host, Person, clear_session, community_with, sign_up, start, start_host, wait_for,
};

const WAIT: Duration = Duration::from_secs(30);

/// Two people in one community with #general and #general-2, signed up, set up and with their sessions cleared.
async fn two_people(tag: &str) -> (Host, Person, Person, [u8; 16]) {
    let host = tokio::task::spawn_blocking({
        let tag = tag.to_owned();
        move || start_host(&tag, "127.0.0.1:0", None)
    })
    .await
    .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let a = sign_up(&host, &invite, "ada", "ada", tag).await;
    let b = sign_up(&host, &invite, "bjorn", "bjorn", tag).await;
    tokio::task::spawn_blocking(move || {
        let c = community_with(&host, &a, "gpui", &[&b], &[(0, "general-2", None)]);
        clear_session(&a);
        clear_session(&b);
        (host, a, b, c)
    })
    .await
    .unwrap()
}

async fn channel_named(client: &Client, community: [u8; 16], name: &'static str) -> ChannelId {
    let s = wait_for(client, WAIT, "the channel list", |s| {
        s.channel_named(CommunityId(community), name).is_some()
    })
    .await;
    s.channel_named(CommunityId(community), name).unwrap().id
}

/// Both clients hold the channel's group.
async fn both_in(a: &Client, b: &Client, channel: ChannelId) {
    a.open_channel(channel).await.unwrap().expect("a opens it");
    b.open_channel(channel).await.unwrap().expect("b joins it");
}

fn item_with<'a>(
    s: &'a AppState,
    channel: ChannelId,
    body: &str,
) -> Option<&'a dilla_client::state::TimelineItem> {
    s.timelines
        .get(&channel)?
        .items
        .iter()
        .find(|i| i.body == body)
}

fn channel_lists(s: &AppState, channel: ChannelId, body: &str) -> bool {
    item_with(s, channel, body).is_some()
}

fn thread_bodies(s: &AppState, root: MessageId) -> Vec<String> {
    s.threads
        .threads
        .get(&root)
        .map(|d| d.replies.iter().map(|r| r.body.clone()).collect())
        .unwrap_or_default()
}

fn count_of(s: &AppState, channel: ChannelId, root: MessageId) -> u32 {
    s.threads.summary(channel, root).map_or(0, |x| x.count)
}

/// A root posted by `a` in `channel`, held by both. Answers its id.
async fn root_by(a: &Client, b: &Client, channel: ChannelId, body: &str) -> MessageId {
    a.send_text(channel, body.into())
        .await
        .unwrap()
        .expect("queued");
    wait_for(b, WAIT, "b to hold the root", |s| {
        item_with(s, channel, body).is_some_and(|i| i.seq.is_some())
    })
    .await;
    let s = wait_for(a, WAIT, "a to store the root", |s| {
        item_with(s, channel, body).is_some_and(|i| i.seq.is_some())
    })
    .await;
    item_with(&s, channel, body).unwrap().msg_id.unwrap()
}

/// e2e/web/threads.spec.ts, "a reply in a thread shows its count on both sides, the pane sits in the aside, and a reload
/// keeps it".
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn thread_reply_count_both_sides_aside_and_kept_after_restart() {
    let (_host, pa, pb, c) = two_people("thread-count").await;
    let a = start(&pa).await;
    let b = start(&pb).await;
    let general = channel_named(&a, c, "general").await;
    channel_named(&b, c, "general").await;
    both_in(&a, &b, general).await;
    let root = root_by(&a, &b, general, "plan").await;

    // Ada opens the thread: the aside's state names it, empty so far.
    a.threads().track(general);
    a.threads()
        .open(general, root)
        .await
        .unwrap()
        .expect("opens");
    let s = wait_for(&a, WAIT, "the thread to be read", |s| {
        s.threads.threads.contains_key(&root)
    })
    .await;
    assert_eq!(
        s.threads.open,
        Some(OpenThread {
            channel: general,
            root
        })
    );
    assert_eq!(
        s.threads.threads[&root]
            .root
            .as_ref()
            .map(|r| r.body.as_str()),
        Some("plan")
    );
    assert!(s.threads.threads[&root].replies.is_empty());

    // Björn replies through the thread id: it shows in the pane and as a count on the root, never in the channel.
    b.threads()
        .send_in_thread(general, root, "ok".into())
        .await
        .unwrap()
        .expect("queued");
    let s = wait_for(&a, WAIT, "Björn's reply in the pane and its count", |s| {
        thread_bodies(s, root) == ["ok"] && count_of(s, general, root) == 1
    })
    .await;
    assert!(
        !channel_lists(&s, general, "ok"),
        "a thread reply is not a row of the channel"
    );
    let reply = &s.threads.threads[&root].replies[0];
    assert_eq!(reply.sender_user, Some(UserId(pb.user_id)));
    assert!(!reply.own, "Björn's reply is not Ada's");

    // Ada replies from the pane: Björn receives it, and both counts read 2.
    let go = a
        .threads()
        .send_in_thread(general, root, "go".into())
        .await
        .unwrap()
        .expect("queued");
    let s = wait_for(&a, WAIT, "Ada's own reply stored and counted", |s| {
        s.threads.threads.get(&root).is_some_and(|d| {
            d.replies.len() == 2
                && d.replies[1].state == TimelineItemState::Ok
                && d.replies[1].seq.is_some()
                && d.replies[1].msg_id == Some(go)
        }) && count_of(s, general, root) == 2
    })
    .await;
    assert!(s.threads.threads[&root].replies[1].own);
    assert!(
        !channel_lists(&s, general, "go"),
        "not even Ada's own reply is a row of the channel"
    );
    b.threads().track(general);
    let s = wait_for(&b, WAIT, "Björn's count of 2", |s| {
        count_of(s, general, root) == 2
    })
    .await;
    assert!(!channel_lists(&s, general, "ok") && !channel_lists(&s, general, "go"));
    assert!(channel_lists(&s, general, "plan"), "the root stays");

    // A second thread closes the first (the worker keeps one open), and closing forgets what was read.
    a.threads().close();
    let s = wait_for(&a, WAIT, "the pane to close", |s| s.threads.open.is_none()).await;
    assert!(s.threads.threads.is_empty());
    assert_eq!(
        s.threads.last_open,
        Some(OpenThread {
            channel: general,
            root
        })
    );
    // Q9: the header button reopens the last thread directly.
    a.threads().reopen_last().await.unwrap().expect("reopens");
    wait_for(&a, WAIT, "the last thread again", |s| {
        thread_bodies(s, root) == ["ok", "go"]
    })
    .await;

    // A thread's draft is its own: it survives a restart and never touches the channel's.
    a.threads().set_draft(root, "half a repl");
    assert_eq!(a.threads().draft(root), "half a repl");
    a.threads().set_draft(root, "");
    assert_eq!(a.threads().draft(root), "");
    a.threads().set_draft(root, "kept");

    // A reload keeps the count and the thread.
    a.stop();
    clear_session(&pa);
    let a = start(&pa).await;
    channel_named(&a, c, "general").await;
    a.threads().track(general);
    let s = wait_for(&a, WAIT, "the count after a restart", |s| {
        count_of(s, general, root) == 2
    })
    .await;
    assert!(
        !channel_lists(&s, general, "ok"),
        "the channel still leaves the reply out"
    );
    a.threads()
        .open(general, root)
        .await
        .unwrap()
        .expect("opens");
    wait_for(&a, WAIT, "both replies after a restart", |s| {
        thread_bodies(s, root) == ["ok", "go"]
    })
    .await;
    assert_eq!(
        a.threads().draft(root),
        "kept",
        "the draft survives the restart"
    );
}

/// e2e/web/threads.spec.ts, "a mention in a thread badges the channel, and the reply stays out of the channel".
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn thread_mention_badges_channel_reply_stays_out() {
    let (_host, pa, pb, c) = two_people("thread-mention").await;
    let a = start(&pa).await;
    let b = start(&pb).await;
    let general = channel_named(&a, c, "general").await;
    let second = channel_named(&a, c, "general-2").await;
    channel_named(&b, c, "general").await;
    channel_named(&b, c, "general-2").await;
    both_in(&a, &b, general).await;
    both_in(&a, &b, second).await;
    let root = root_by(&a, &b, general, "plan").await;
    // Ada sits in the other channel, so the mention in #general's thread has to badge #general's row.
    a.focus(Some(second));
    a.mark_read(general).await.unwrap().expect("read");
    let s = wait_for(&a, WAIT, "no mention badge yet", |s| {
        s.unread.get(&general).is_none_or(|b| b.mentions == 0)
    })
    .await;
    assert_eq!(s.unread.get(&general).map_or(0, |b| b.mentions), 0);

    // Control: a mention-free thread reply in a channel Ada is not viewing gives no mention badge.
    b.threads()
        .send_in_thread(general, root, "plain".into())
        .await
        .unwrap()
        .expect("queued");
    let s = wait_for(&a, WAIT, "an unread count without a mention", |s| {
        s.unread.get(&general).is_some_and(|b| b.unread > 0)
    })
    .await;
    assert_eq!(s.unread[&general].mentions, 0);

    let ada = dilla_client::testing::host::hex(&pa.user_id);
    b.threads()
        .send_in_thread(general, root, format!("<@{ada}> ok"))
        .await
        .unwrap()
        .expect("queued");
    let s = wait_for(
        &a,
        WAIT,
        "the mention in the thread to badge the channel",
        |s| s.unread.get(&general).is_some_and(|b| b.mentions == 1),
    )
    .await;
    assert!(
        !s.timelines[&general]
            .bodies()
            .iter()
            .any(|b| b.contains("ok")),
        "the reply stays out"
    );

    // Control: a mention in the other channel adds to that row, not to #general's.
    b.send_text(second, format!("<@{ada}> side"))
        .await
        .unwrap()
        .expect("queued");
    let s = wait_for(&a, WAIT, "the other channel's own mention", |s| {
        s.timelines
            .get(&second)
            .is_some_and(|t| t.items.iter().any(|i| i.body.ends_with("side")))
    })
    .await;
    assert_eq!(s.unread[&general].mentions, 1);

    // Ada opens #general: its thread chip reads 2 and reading clears the badge.
    a.focus(Some(general));
    a.threads().track(general);
    let s = wait_for(&a, WAIT, "the chip's count of 2 and a read channel", |s| {
        count_of(s, general, root) == 2 && s.unread.get(&general).is_none_or(|b| b.mentions == 0)
    })
    .await;
    assert!(
        !s.timelines[&general]
            .bodies()
            .iter()
            .any(|b| b.contains("ok") || *b == "plain")
    );
}

/// e2e/web/threads.spec.ts, "Ada edits and deletes her own reply in the thread".
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn edit_and_delete_own_thread_reply() {
    let (_host, pa, pb, c) = two_people("thread-edit").await;
    let a = start(&pa).await;
    let b = start(&pb).await;
    let general = channel_named(&a, c, "general").await;
    channel_named(&b, c, "general").await;
    both_in(&a, &b, general).await;
    let root = root_by(&a, &b, general, "plan").await;
    a.threads().track(general);
    a.threads()
        .open(general, root)
        .await
        .unwrap()
        .expect("opens");
    b.threads().track(general);
    b.threads()
        .open(general, root)
        .await
        .unwrap()
        .expect("opens");

    let first = a
        .threads()
        .send_in_thread(general, root, "first".into())
        .await
        .unwrap()
        .expect("queued");
    wait_for(&a, WAIT, "Ada's reply stored", |s| {
        s.threads.threads.get(&root).is_some_and(|d| {
            d.replies
                .iter()
                .any(|r| r.msg_id == Some(first) && r.seq.is_some())
        })
    })
    .await;
    wait_for(&b, WAIT, "Björn to read it", |s| {
        thread_bodies(s, root) == ["first"]
    })
    .await;

    a.threads()
        .edit(general, first, "edited".into())
        .await
        .unwrap()
        .expect("queued");
    let s = wait_for(&a, WAIT, "the edit shown", |s| {
        s.threads.threads.get(&root).is_some_and(|d| {
            d.replies.len() == 1 && d.replies[0].edited && d.replies[0].body == "edited"
        })
    })
    .await;
    assert_eq!(thread_bodies(&s, root), ["edited"], "the old text is gone");
    wait_for(&b, WAIT, "Björn to read the edit", |s| {
        thread_bodies(s, root) == ["edited"]
    })
    .await;

    a.threads()
        .delete(general, first)
        .await
        .unwrap()
        .expect("queued");
    let s = wait_for(&a, WAIT, "the delete shown", |s| {
        s.threads.threads.get(&root).is_some_and(|d| {
            d.replies.len() == 1 && d.replies[0].state == TimelineItemState::Deleted
        })
    })
    .await;
    assert_eq!(
        s.threads.threads[&root].replies[0].body, "",
        "a deleted reply keeps no text"
    );
    // The channel never listed the reply, edited or not, and the root survives.
    assert!(!channel_lists(&s, general, "edited") && !channel_lists(&s, general, "first"));
    assert!(channel_lists(&s, general, "plan"));
}

/// e2e/web/conversation.spec.ts, "edit, reply, react, pin and delete" (its reply part): the post names the original, the
/// peer's timeline resolves it, and the original does not become a thread.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn reply_chip_jumps_to_original() {
    let (_host, pa, pb, c) = two_people("reply-chip").await;
    let a = start(&pa).await;
    let b = start(&pb).await;
    let general = channel_named(&a, c, "general").await;
    channel_named(&b, c, "general").await;
    both_in(&a, &b, general).await;
    let sent = root_by(&a, &b, general, "what time is it").await;

    b.threads()
        .reply(general, sent, "noon".into())
        .await
        .unwrap()
        .expect("queued");
    let s = wait_for(&a, WAIT, "Björn's reply with its original", |s| {
        item_with(s, general, "noon").is_some_and(|i| i.reply.is_some())
    })
    .await;
    let answer = item_with(&s, general, "noon").unwrap();
    let reply = answer.reply.as_ref().unwrap();
    assert_eq!(reply.msg_id, sent, "the reply line names the original");
    assert_eq!(reply.state, 0, "the original is held");
    assert_eq!(reply.sender_user, Some(UserId(pa.user_id)));
    assert_eq!(reply.excerpt, "what time is it");
    assert!(reply.seq.is_some(), "the jump knows the row to scroll to");
    assert!(
        s.threads
            .summaries
            .get(&general)
            .is_none_or(|m| m.is_empty()),
        "a reply is not a thread"
    );

    // The reply bar's target: set, taken once by the send of its channel, never by another's.
    use dilla_client::state::threads::ReplyTarget;
    let target = ReplyTarget {
        channel: general,
        msg_id: sent,
        sender: Some(UserId(pa.user_id)),
        excerpt: "what time is it".into(),
    };
    a.threads().set_replying(Some(target.clone()));
    wait_for(&a, WAIT, "the target", |s| {
        s.threads.replying.as_ref() == Some(&target)
    })
    .await;
    let other = channel_named(&a, c, "general-2").await;
    assert!(a.threads().take_replying(other).is_none());
    assert_eq!(a.threads().take_replying(general), Some(target));
    wait_for(&a, WAIT, "the target cleared", |s| {
        s.threads.replying.is_none()
    })
    .await;
}

/// The refresh loop's cost: a publish of something else (a reply target, presence) reads no timeline and no thread; a
/// peer's thread reply, which moves the group, is read.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn an_unrelated_publish_triggers_no_reads() {
    let (_host, pa, pb, c) = two_people("thread-reads").await;
    let a = start(&pa).await;
    let b = start(&pb).await;
    let general = channel_named(&a, c, "general").await;
    channel_named(&b, c, "general").await;
    both_in(&a, &b, general).await;
    let root = root_by(&a, &b, general, "plan").await;
    a.threads().track(general);
    a.threads()
        .open(general, root)
        .await
        .unwrap()
        .expect("opens");
    wait_for(&a, WAIT, "the thread", |s| {
        s.threads.threads.contains_key(&root)
    })
    .await;
    // Let the loop settle: everything the opening published has been looked at.
    tokio::time::sleep(Duration::from_secs(2)).await;
    let before = a.threads().reads();
    for _ in 0..5 {
        a.threads().set_replying(None);
        tokio::time::sleep(Duration::from_millis(400)).await;
    }
    assert_eq!(
        a.threads().reads(),
        before,
        "an unrelated publish read the core"
    );
    b.threads()
        .send_in_thread(general, root, "ok".into())
        .await
        .unwrap()
        .expect("queued");
    wait_for(&a, WAIT, "the reply", |s| thread_bodies(s, root) == ["ok"]).await;
    assert!(a.threads().reads() > before, "a thread reply is read");
    // Two clients in one process: b's sends are its own.
    assert_eq!(a.threads().pending_sends(), 0);
}

/// ThreadView's missing root: a thread of a group this device does not hold publishes an empty thread, so the pane says
/// the root cannot be shown.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn opening_a_thread_of_an_unheld_group_publishes_an_empty_thread() {
    let (_host, pa, _pb, c) = two_people("thread-unheld").await;
    let a = start(&pa).await;
    let second = channel_named(&a, c, "general-2").await;
    let root = MessageId([5; 16]);
    let _ = a.threads().open(second, root).await.unwrap();
    let s = wait_for(&a, WAIT, "the empty thread", |s| {
        s.threads.threads.contains_key(&root)
    })
    .await;
    let data = &s.threads.threads[&root];
    assert!(data.root.is_none() && data.replies.is_empty() && data.count == 0);
    assert_eq!(
        s.threads.open,
        Some(OpenThread {
            channel: second,
            root
        })
    );
}

/// e2e/web/threads.spec.ts, "two people thread in a DM": Ada posts a root in the DM with Björn; Björn answers it in its
/// thread. Ada's DM shows the root's count, the reply is held out of the DM's timeline, and opening the thread shows the
/// reply in the pane while the timeline still holds it out.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn two_people_thread_in_a_dm() {
    let (_host, pa, pb, _c) = two_people("thread-dm").await;
    let a = start(&pa).await;
    let b = start(&pb).await;
    a.dms().start();
    b.dms().start();
    let dm = a
        .dms()
        .open_dm(UserId(pb.user_id))
        .await
        .unwrap()
        .expect("the DM opens");
    a.dms()
        .open(dm)
        .await
        .unwrap()
        .expect("a registers the DM's group");
    wait_for(&b, WAIT, "b to list and join the DM", |s| {
        s.dms
            .get(dm)
            .is_some_and(|d| d.group == dilla_client::state::ChannelGroupState::Active)
    })
    .await;
    a.threads().track(dm);
    a.dms()
        .send_text(dm, "dm root".into())
        .await
        .unwrap()
        .expect("queued");
    let s = wait_for(&a, WAIT, "the root stored", |s| {
        item_with(s, dm, "dm root")
            .is_some_and(|i| i.seq.is_some() && i.state == TimelineItemState::Ok)
    })
    .await;
    let root = item_with(&s, dm, "dm root").unwrap().msg_id.unwrap();
    // Björn holds the root (heard), then answers it in its thread.
    wait_for(&b, WAIT, "b holds the root", |s| {
        item_with(s, dm, "dm root").is_some_and(|i| i.seq.is_some())
    })
    .await;
    b.threads()
        .send_in_thread(dm, root, "dm reply".into())
        .await
        .unwrap()
        .expect("the reply is queued");
    let s = wait_for(&a, WAIT, "the root's count", |s| count_of(s, dm, root) == 1).await;
    assert!(
        !channel_lists(&s, dm, "dm reply"),
        "the reply is held out of the DM timeline"
    );
    a.threads().open(dm, root).await.unwrap().expect("opens");
    let s = wait_for(&a, WAIT, "the reply in the pane", |s| {
        thread_bodies(s, root) == ["dm reply"]
    })
    .await;
    assert_eq!(s.threads.open, Some(OpenThread { channel: dm, root }));
    assert!(
        !channel_lists(&s, dm, "dm reply"),
        "still out of the timeline with the pane open"
    );
}
