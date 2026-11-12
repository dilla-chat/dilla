//! Task 22's client tests for the notify area (standard procedure S2), against a real dillad.
//!
//! The ports of `e2e/web/badges.spec.ts` that this area's client answers; the ones that open a DM need Task 13's `dms`
//! and go to Task 28's list (`dm_from_tab_reaches_peer_reply_badges_until_read`,
//! `dm_opened_by_peer_appears_badges_notifies_live`). The unit tables (`features::notify::tests`) port the web's
//! `notify.test.ts` rows; these tests prove the same decisions over what a real gateway delivers.
//!
//! Needs `target/dilla-testhost` and the WASI core, like `tests/sync.rs`; `#[ignore]`d so a plain `cargo test` needs no Go
//! toolchain (the desktop-gpui CI job runs them with `--ignored`).

use std::sync::{Arc, Mutex};
use std::time::Duration;

use dilla_client::Client;
use dilla_client::features::notify::{
    DefaultChoice, NotifyEnv, NotifyMode, Presence, RecordingNotifier, Text, row_counts,
    total_badge,
};
use dilla_client::state::{ChannelId, CommunityId};
use dilla_client::testing::host::{
    Host, Person, clear_session, community_with, hex, sign_up, start, start_host, wait_for,
};

const IGNORE: &str = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)";

/// Alice (the viewer) and bob (the peer) in one community with #general, #random and #loot.
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
        let channels = [(0, "random", None), (0, "loot", None)];
        let c = community_with(&host, &a, "gpui", &[&b], &channels);
        clear_session(&a);
        clear_session(&b);
        (host, a, b, c)
    })
    .await
    .unwrap();
    (host, a, b, c)
}

async fn channel_named(client: &Client, community: [u8; 16], name: &'static str) -> ChannelId {
    let community = CommunityId(community);
    let state = wait_for(client, Duration::from_secs(30), "the channel list", |s| {
        s.channel_named(community, name).is_some()
    })
    .await;
    state.channel_named(community, name).unwrap().id
}

/// Both clients hold the groups of every channel: alice registers each, bob joins it (join-all, as the browser does).
async fn all_in(a: &Client, b: &Client, channels: &[ChannelId]) {
    for channel in channels {
        a.open_channel(*channel)
            .await
            .unwrap()
            .expect("alice opens the channel");
        b.open_channel(*channel)
            .await
            .unwrap()
            .expect("bob joins the channel");
    }
}

struct Env {
    on_screen: Mutex<Option<ChannelId>>,
}

impl NotifyEnv for Env {
    fn visible(&self) -> bool {
        true
    }
    fn on_screen(&self) -> Option<ChannelId> {
        *self.on_screen.lock().unwrap()
    }
    fn presence(&self) -> Presence {
        Presence::Online
    }
}

/// The strings the notifier uses, as the web's `en.ts` has them.
fn text() -> Text {
    Arc::new(|key, vars| {
        let src = match key {
            "notify.title.channel" => "#{channel} · {server}",
            "notify.title.dm" => "{sender}",
            "activity.dm" => "Direct message",
            "activity.newCount" => "{n} new",
            "shell.message.mentionRole" => "a role",
            "shell.message.mentionUnknown" => "@{id}",
            other => panic!("no string for {other}"),
        };
        vars.iter().fold(src.to_owned(), |s, (k, v)| {
            s.replace(&format!("{{{k}}}"), v)
        })
    })
}

/// `e2e/web/badges.spec.ts` test 1: unopened channels badge their unread messages and mentions, opening reads them, and a
/// mute hides the count but not the mention.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn unopened_channels_badge_open_reads_mute_hides_count() {
    let _ = IGNORE;
    let (_host, pa, pb, c) = two_people("badges").await;
    let (a, b) = (start(&pa).await, start(&pb).await);
    let one = channel_named(&a, c, "general").await;
    let two = channel_named(&a, c, "random").await;
    let three = channel_named(&a, c, "loot").await;
    channel_named(&b, c, "loot").await;
    all_in(&a, &b, &[one, two, three]).await;
    a.focus(Some(one));
    let prefs = a.prefs();

    // Quiet rows carry no badge.
    let s = a.state().borrow().clone();
    for ch in [one, two, three] {
        assert_eq!(row_counts(&s, &prefs, ch), (0, 0));
    }

    b.send_text(two, "plain in two".into())
        .await
        .unwrap()
        .unwrap();
    let s = wait_for(&a, Duration::from_secs(20), "one unread in two", |s| {
        s.unread_of(two) == 1
    })
    .await;
    assert_eq!(row_counts(&s, &prefs, two), (1, 0));

    let mention = format!("<@{}> look here", hex(&pa.user_id));
    b.send_text(three, mention).await.unwrap().unwrap();
    let s = wait_for(&a, Duration::from_secs(20), "a mention in three", |s| {
        s.unread.get(&three).is_some_and(|b| b.mentions == 1)
    })
    .await;
    assert_eq!(row_counts(&s, &prefs, three), (1, 1));
    assert_eq!(a.notify().total_badge(), 2);
    let community = s.channel(two).unwrap().community_id;
    assert_eq!(
        dilla_client::features::notify::rail_counts(&s, &prefs, community),
        (2, 1)
    );

    // Opening channel two reads it (markRead on open).
    a.focus(Some(two));
    let s = wait_for(&a, Duration::from_secs(20), "two read", |s| {
        s.unread_of(two) == 0
    })
    .await;
    assert_eq!(
        dilla_client::features::notify::rail_counts(&s, &prefs, community),
        (1, 1)
    );

    // Mute channel three: its count goes, its mention stays.
    a.notify().set_channel_muted(three, true).unwrap();
    let s = a.state().borrow().clone();
    assert_eq!(row_counts(&s, &prefs, three), (0, 1));
    assert_eq!(total_badge(&s, &prefs), 1, "only the mention counts");

    // Device-local and kept: a second handle on the same preferences file reads the mute.
    let reread = dilla_client::Prefs::instance(&pa.cfg.data_dir);
    assert_eq!(row_counts(&s, &reread, three), (0, 1));
}

/// `e2e/web/badges.spec.ts` test 2, the client's half: the device-wide default is kept. The menu that sets it (the DM list
/// head's) is the shell's (Tasks 8 and 13); here the handle writes it and a fresh read of the preferences finds it.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn dm_list_menu_sets_device_notify_default() {
    let (_host, pa, _pb, _c) = two_people("default").await;
    let a = start(&pa).await;
    let settings =
        |p: &dilla_client::Prefs| dilla_client::features::notify::Settings(p).default_choice();
    assert_eq!(settings(&a.prefs()), DefaultChoice::DmsMentions);
    a.notify().set_default(DefaultChoice::Everything).unwrap();
    assert_eq!(settings(&a.prefs()), DefaultChoice::Everything);
    drop(a);
    let restarted = start(&pa).await;
    assert_eq!(settings(&restarted.prefs()), DefaultChoice::Everything);
}

/// `e2e/web/badges.spec.ts` test 4: notifications follow the default and the channel on screen.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn notifications_follow_default_and_channel_on_screen() {
    let (_host, pa, pb, c) = two_people("notices").await;
    let (a, b) = (start(&pa).await, start(&pb).await);
    let one = channel_named(&a, c, "general").await;
    let two = channel_named(&a, c, "random").await;
    let three = channel_named(&a, c, "loot").await;
    channel_named(&b, c, "loot").await;
    all_in(&a, &b, &[one, two, three]).await;
    a.focus(Some(one));

    let recorder = RecordingNotifier::new();
    let env = Arc::new(Env {
        on_screen: Mutex::new(Some(one)),
    });
    let _task = a
        .notify()
        .start(&a, Arc::new(recorder.clone()), env.clone(), text());
    let title = |channel: &str| format!("#{channel} · gpui");
    let shown_tags = |r: &RecordingNotifier| r.tags();
    let settle = || tokio::time::sleep(Duration::from_secs(2));

    // (a) DMs and mentions: a plain message stays a badge, a mention notifies.
    b.send_text(two, "plain".into()).await.unwrap().unwrap();
    wait_for(&a, Duration::from_secs(20), "a badge in two", |s| {
        s.unread_of(two) == 1
    })
    .await;
    settle().await;
    assert!(recorder.shown().is_empty());
    let me = hex(&pa.user_id);
    b.send_text(three, format!("<@{me}> look"))
        .await
        .unwrap()
        .unwrap();
    wait_for(&a, Duration::from_secs(20), "a mention in three", |s| {
        s.unread.get(&three).is_some_and(|b| b.mentions == 1)
    })
    .await;
    settle().await;
    let shown = recorder.shown();
    assert_eq!(shown_tags(&recorder), [format!("dilla:{}", three.hex())]);
    assert_eq!(shown[0].notification.title, title("loot"));
    assert_eq!(shown[0].notification.body, "@alice look");

    // (b) Everything: the channel on screen stays quiet, another one notifies, and the click opens it.
    a.notify().set_default(DefaultChoice::Everything).unwrap();
    b.send_text(one, "on screen".into()).await.unwrap().unwrap();
    settle().await;
    assert_eq!(recorder.shown().len(), 1, "the channel on screen is quiet");
    b.send_text(two, "plain again".into())
        .await
        .unwrap()
        .unwrap();
    settle().await;
    assert_eq!(
        shown_tags(&recorder),
        [
            format!("dilla:{}", three.hex()),
            format!("dilla:{}", two.hex())
        ]
    );
    assert_eq!(recorder.shown()[1].notification.title, title("random"));
    assert_eq!(recorder.shown()[1].notification.body, "plain again");
    assert_eq!(recorder.click(1), Some(two));
    assert!(recorder.shown()[1].closed);

    // (c) Nothing: a mention only counts; the control after switching back proves it raised nothing.
    a.notify().set_default(DefaultChoice::Nothing).unwrap();
    *env.on_screen.lock().unwrap() = Some(two);
    b.send_text(three, format!("<@{me}> quiet"))
        .await
        .unwrap()
        .unwrap();
    wait_for(
        &a,
        Duration::from_secs(20),
        "the second mention counted",
        |s| s.unread.get(&three).is_some_and(|b| b.mentions == 2),
    )
    .await;
    a.notify().set_default(DefaultChoice::Everything).unwrap();
    b.send_text(one, "control".into()).await.unwrap().unwrap();
    settle().await;
    assert_eq!(
        shown_tags(&recorder),
        [
            format!("dilla:{}", three.hex()),
            format!("dilla:{}", two.hex()),
            format!("dilla:{}", one.hex()),
        ]
    );

    // A channel's own mode and a mute win over the default; previews off keep the text out.
    a.notify()
        .set_channel_mode(two, Some(NotifyMode::Nothing))
        .unwrap();
    a.notify().set_previews(false).unwrap();
    *env.on_screen.lock().unwrap() = Some(one);
    b.send_text(two, "muted by mode".into())
        .await
        .unwrap()
        .unwrap();
    b.send_text(three, "secret plan".into())
        .await
        .unwrap()
        .unwrap();
    settle().await;
    // The new notification of #loot replaced its earlier one (same tag); #random, set to nothing, added none.
    let shown = recorder.shown();
    assert_eq!(shown.len(), 3, "the channel set to nothing stays quiet");
    let loot = &shown[0].notification;
    assert_eq!(loot.tag, format!("dilla:{}", three.hex()));
    assert_eq!(loot.body, "1 new", "previews are off: {loot:?}");
    assert!(!format!("{shown:?}").contains("secret"));
}
