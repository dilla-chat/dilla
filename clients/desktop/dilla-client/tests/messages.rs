//! Task 9's client tests for the messages area (standard procedure S2), against a real dillad (dilla-testhost): edit, pin,
//! unpin and delete between two devices, what a peer's edit or delete does to a message that is not theirs, mentions, the
//! typing signal and a link a sender made that the receiver never fetches.
//!
//! Needs `target/dilla-testhost` and the WASI core like `sync.rs` does; the tests are `#[ignore]`d so a plain
//! `cargo test` needs no Go toolchain (`cargo test -p dilla-client --test messages -- --include-ignored`).

use std::time::Duration;

use dilla_client::Client;
use dilla_client::state::{
    AppState, ChannelId, CommunityId, MessageId, TimelineItem, TimelineItemState, UserId,
};
use dilla_client::testing::host::{
    Host, Person, clear_session, community, hex, sign_up, start, start_host, wait_for,
};

const SHORT: Duration = Duration::from_secs(20);

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

/// Both people started and in `general`, which both hold.
async fn both(tag: &str) -> (Host, Person, Person, Client, Client, ChannelId) {
    let (host, pa, pb, c) = two_people(tag).await;
    let a = start(&pa).await;
    let b = start(&pb).await;
    let find = |client: &Client| {
        let state = client.state();
        async move {
            let mut state = state;
            let deadline = std::time::Instant::now() + SHORT;
            loop {
                let s = state.borrow_and_update().clone();
                if let Some(ch) = s.channel_named(CommunityId(c), "general") {
                    return ch.id;
                }
                assert!(std::time::Instant::now() < deadline, "no channel list");
                tokio::time::sleep(Duration::from_millis(100)).await;
            }
        }
    };
    let channel = find(&a).await;
    find(&b).await;
    a.open_channel(channel).await.unwrap().expect("a opens");
    b.open_channel(channel).await.unwrap().expect("b joins");
    (host, pa, pb, a, b, channel)
}

fn item(s: &AppState, channel: ChannelId, id: MessageId) -> Option<&TimelineItem> {
    s.timelines
        .get(&channel)?
        .items
        .iter()
        .find(|i| i.msg_id == Some(id))
}

/// Waits until `who` shows the message with `pred`.
async fn until(
    who: &Client,
    channel: ChannelId,
    id: MessageId,
    what: &str,
    pred: impl Fn(&TimelineItem) -> bool,
) -> TimelineItem {
    let s = wait_for(who, SHORT, what, |s| {
        item(s, channel, id).is_some_and(&pred)
    })
    .await;
    item(&s, channel, id).cloned().unwrap()
}

/// e2e/web/messaging.spec.ts: text both ways, with the sender, and once.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core"]
async fn text_both_ways_mls_sender_and_once_after_update() {
    let (_host, pa, pb, a, b, channel) = both("msg-both-ways").await;
    let from_a = a
        .send_text(channel, "hello bob".into())
        .await
        .unwrap()
        .unwrap();
    let seen = until(&b, channel, from_a, "b to read a's text", |i| {
        i.state == TimelineItemState::Ok && i.body == "hello bob"
    })
    .await;
    assert_eq!(
        seen.sender_user,
        Some(UserId(pa.user_id)),
        "the sender is a"
    );
    assert!(!seen.own);
    let from_b = b
        .send_text(channel, "hello alice".into())
        .await
        .unwrap()
        .unwrap();
    let back = until(&a, channel, from_b, "a to read b's text", |i| {
        i.state == TimelineItemState::Ok && i.body == "hello alice"
    })
    .await;
    assert_eq!(back.sender_user, Some(UserId(pb.user_id)));
    // Once: each text is one item on both sides, however often the timeline was read.
    for who in [&a, &b] {
        let s = who.state().borrow().clone();
        let items = &s.timelines[&channel].items;
        assert_eq!(items.iter().filter(|i| i.body == "hello bob").count(), 1);
        assert_eq!(items.iter().filter(|i| i.body == "hello alice").count(), 1);
    }
}

/// e2e/web/conversation.spec.ts "edit, reply, react, pin and delete" (its edit, pin and delete parts).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core"]
async fn edit_pin_delete_between_two_devices() {
    let (_host, _pa, pb, a, b, channel) = both("msg-edit-pin-delete").await;
    let id = a
        .send_text(channel, "first draft".into())
        .await
        .unwrap()
        .unwrap();
    until(&b, channel, id, "b to read it", |i| i.body == "first draft").await;

    // Edit: both devices show the new text, marked edited.
    a.messages()
        .edit(channel, id, "final text".into())
        .await
        .unwrap()
        .expect("queued");
    for who in [&a, &b] {
        let i = until(who, channel, id, "the edit", |i| i.body == "final text").await;
        assert!(i.edited, "edited");
    }
    // A blank edit is refused before anything is queued.
    assert!(
        a.messages()
            .edit(channel, id, "  ".into())
            .await
            .unwrap()
            .is_err()
    );

    // Pin: both see it pinned, and the pins list holds it with its excerpt.
    a.messages()
        .pin(channel, id, true)
        .await
        .unwrap()
        .expect("queued");
    for who in [&a, &b] {
        until(who, channel, id, "the pin", |i| i.pinned).await;
    }
    b.messages()
        .load_pins(channel)
        .await
        .unwrap()
        .expect("pins read");
    let s = wait_for(&b, SHORT, "the pins list", |s| {
        s.messages.pins.get(&channel).is_some_and(|p| p.len() == 1)
    })
    .await;
    let pins = &s.messages.pins[&channel];
    assert_eq!(pins.len(), 1);
    assert_eq!(pins[0].msg_id, id);
    assert_eq!(pins[0].excerpt, "final text");
    assert_eq!(pins[0].pinned_by, UserId(_pa_id(&a)));
    let _ = pb;

    // Unpin: the list is empty again.
    a.messages()
        .pin(channel, id, false)
        .await
        .unwrap()
        .expect("queued");
    until(&b, channel, id, "the unpin", |i| !i.pinned).await;
    b.messages()
        .load_pins(channel)
        .await
        .unwrap()
        .expect("pins read");
    wait_for(&b, SHORT, "the empty pins list", |s| {
        s.messages.pins.get(&channel).is_some_and(Vec::is_empty)
    })
    .await;

    // Delete: both show it deleted, with no text.
    a.messages()
        .delete(channel, id)
        .await
        .unwrap()
        .expect("queued");
    for who in [&a, &b] {
        let i = until(who, channel, id, "the delete", |i| {
            i.state == TimelineItemState::Deleted
        })
        .await;
        assert_eq!(i.body, "");
    }
}

fn _pa_id(a: &Client) -> [u8; 16] {
    a.state().borrow().account.user_id.expect("signed in").0
}

/// e2e/web/conversation.spec.ts "an edit or a delete by someone else changes nothing".
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core"]
async fn foreign_edit_or_delete_changes_nothing() {
    let (_host, _pa, _pb, a, b, channel) = both("msg-foreign").await;
    let id = a.send_text(channel, "mine".into()).await.unwrap().unwrap();
    until(&b, channel, id, "b to read it", |i| i.body == "mine").await;
    // b folds an edit and a delete aimed at a's message, then posts a marker; a marker that has arrived on a means the
    // folds before it have been applied (or ignored).
    let _ = b.messages().edit(channel, id, "hijacked".into()).await;
    let _ = b.messages().delete(channel, id).await;
    let marker = b
        .send_text(channel, "marker".into())
        .await
        .unwrap()
        .unwrap();
    until(&a, channel, marker, "a to read the marker", |i| {
        i.body == "marker"
    })
    .await;
    for who in [&a, &b] {
        let s = who.state().borrow().clone();
        let i = item(&s, channel, id).expect("still there");
        assert_eq!(i.state, TimelineItemState::Ok, "not deleted");
        assert_eq!(i.body, "mine", "not edited");
        assert!(!i.edited);
    }
}

/// e2e/web/conversation.spec.ts "mentions": a mention of the viewer is flagged, and badges the channel.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core"]
async fn mention_highlights_and_badges() {
    let (_host, _pa, pb, a, b, channel) = both("msg-mention").await;
    // b looks at another place, so the channel can badge.
    b.focus(None);
    let plain = a
        .send_text(channel, "no one named".into())
        .await
        .unwrap()
        .unwrap();
    let named = a
        .send_text(channel, format!("hey <@{}> look", hex(&pb.user_id)))
        .await
        .unwrap()
        .unwrap();
    let flagged = until(&b, channel, named, "the mention", |i| i.mention).await;
    assert!(flagged.body.contains(&hex(&pb.user_id)));
    let unflagged = until(&b, channel, plain, "the plain text", |i| {
        i.body == "no one named"
    })
    .await;
    assert!(!unflagged.mention);
    let s = wait_for(&b, SHORT, "the mention badge", |s| {
        s.unread.get(&channel).is_some_and(|b| b.mentions >= 1)
    })
    .await;
    assert_eq!(s.unread[&channel].mentions, 1);
    assert!(s.unread[&channel].unread >= 2);
}

/// e2e/web/conversation.spec.ts "B sees what A makes": what the author does shows on the peer, in the same order.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core"]
async fn peer_sees_what_author_makes() {
    let (_host, _pa, _pb, a, b, channel) = both("msg-peer").await;
    let one = a.send_text(channel, "one".into()).await.unwrap().unwrap();
    let two = a
        .send_text(channel, "two *bold*".into())
        .await
        .unwrap()
        .unwrap();
    until(&b, channel, two, "b to read both", |i| {
        i.body == "two *bold*"
    })
    .await;
    a.messages().pin(channel, two, true).await.unwrap().unwrap();
    until(&b, channel, two, "b to see the pin", |i| i.pinned).await;
    let s = b.state().borrow().clone();
    let bodies: Vec<&str> = s.timelines[&channel]
        .items
        .iter()
        .map(|i| i.body.as_str())
        .collect();
    assert_eq!(bodies, ["one", "two *bold*"], "in order, text as sent");
    assert!(item(&s, channel, one).is_some_and(|i| !i.pinned));
}

/// The typing signal both ways: an "on" shows the person typing on the peer, an "off" clears it, and the peer's own
/// signal never shows on the sender.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core"]
async fn typing_is_sent_and_received() {
    let (_host, pa, _pb, a, b, channel) = both("msg-typing").await;
    // The gateway has to be up on both sides.
    for who in [&a, &b] {
        wait_for(who, SHORT, "online", |s| {
            s.connection.status == dilla_client::state::ConnectionStatus::Online
        })
        .await;
    }
    // The signal is only taken once the receiver holds the channel's timeline, so it is sent again (the 3 s throttle
    // allows one every 3 s) until the receiver shows it.
    wait_for(&b, SHORT, "b holds the channel", |s| {
        s.timelines.contains_key(&channel)
    })
    .await;
    let mut seen = None;
    for _ in 0..6 {
        a.messages().typing(channel, true);
        if let Ok(s) = tokio::time::timeout(
            Duration::from_millis(3500),
            wait_for(&b, SHORT, "b to see a typing", |s| {
                s.messages
                    .typing
                    .get(&channel)
                    .is_some_and(|t| !t.is_empty())
            }),
        )
        .await
        {
            seen = Some(s);
            break;
        }
    }
    let s = seen.expect("b sees a typing");
    assert_eq!(s.messages.typing[&channel][0].user, UserId(pa.user_id));
    assert!(!s.messages.typing[&channel][0].name.is_empty(), "named");
    // The sender never sees its own signal.
    assert!(
        a.state()
            .borrow()
            .messages
            .typing
            .get(&channel)
            .is_none_or(Vec::is_empty)
    );
    a.messages().typing(channel, false);
    wait_for(&b, SHORT, "the off", |s| {
        s.messages.typing.get(&channel).is_none_or(Vec::is_empty)
    })
    .await;
}

/// Own, a guard: the receiver never fetches a link preview. The web client has no link-preview feature (a link is drawn as
/// its own text and never opened, `markdown.ts`), and neither has this one, so nothing here may start one.
///
/// Every outgoing request of a client reaches the world through the process's HTTP proxy settings. The proxy here is a
/// recording double: it answers nothing and writes down the request line of whatever reaches it, so it is both the
/// recording `Http` and, for a hostname that resolves nowhere else, the recording resolver (a name only a proxy can
/// resolve). The positive control makes a real `reqwest` request to the link and sees it recorded; then the sender posts
/// the link, the receiver reads it, and the record holds nothing for the link's host.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core"]
async fn receiver_never_fetches_link_preview() {
    let seen = recorder();
    let link = "http://preview.recorded.invalid/card?x=1";
    // The control: a fetch of the link by any reqwest-based code is seen by the double.
    let _ = reqwest::Client::new().get(link).send().await;
    assert!(
        seen.lock()
            .unwrap()
            .iter()
            .any(|l| l.contains("preview.recorded.invalid")),
        "the double sees a fetch of the link (control)"
    );
    seen.lock().unwrap().clear();

    let (_host, _pa, _pb, a, b, channel) = both("msg-preview").await;
    let id = a
        .send_text(
            channel,
            format!("look {link} and https://preview.recorded.invalid/s"),
        )
        .await
        .unwrap()
        .unwrap();
    until(&b, channel, id, "b to read the link", |i| {
        i.body.contains(link)
    })
    .await;
    // Give a fetch every chance to happen.
    tokio::time::sleep(Duration::from_secs(3)).await;
    let record = seen.lock().unwrap().clone();
    assert!(
        record
            .iter()
            .all(|l| !l.contains("preview.recorded.invalid")),
        "nothing asked for the link's host: {record:?}"
    );
}

type Seen = std::sync::Arc<std::sync::Mutex<Vec<String>>>;

/// The recording proxy, started once for the process; every client of this file reaches the instance (127.0.0.1) around
/// it (`NO_PROXY`) and anything else through it.
fn recorder() -> Seen {
    static SEEN: std::sync::OnceLock<Seen> = std::sync::OnceLock::new();
    SEEN.get_or_init(|| {
        use std::io::{BufRead, BufReader, Write};
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port();
        let seen: Seen = Seen::default();
        let log = seen.clone();
        std::thread::spawn(move || {
            for stream in listener.incoming().flatten() {
                let log = log.clone();
                std::thread::spawn(move || {
                    let mut line = String::new();
                    let mut reader = BufReader::new(stream);
                    if reader.read_line(&mut line).is_ok() {
                        log.lock().unwrap().push(line.trim().to_owned());
                    }
                    let _ = reader
                        .into_inner()
                        .write_all(b"HTTP/1.1 502 Bad Gateway\r\ncontent-length: 0\r\n\r\n");
                });
            }
        });
        // SAFETY: set once, before this test starts any client or request, in a process whose other tests only read it.
        #[allow(unsafe_code)]
        unsafe {
            for (k, v) in [
                ("HTTP_PROXY", format!("http://127.0.0.1:{port}")),
                ("http_proxy", format!("http://127.0.0.1:{port}")),
                ("HTTPS_PROXY", format!("http://127.0.0.1:{port}")),
                ("https_proxy", format!("http://127.0.0.1:{port}")),
                ("NO_PROXY", "127.0.0.1,localhost,::1".to_owned()),
                ("no_proxy", "127.0.0.1,localhost,::1".to_owned()),
            ] {
                std::env::set_var(k, v);
            }
        }
        seen
    })
    .clone()
}
