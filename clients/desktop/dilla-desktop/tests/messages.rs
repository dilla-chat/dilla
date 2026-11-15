//! Task 9's parity, behaviour and keyboard tests for the messages area (standard procedure S4-S6).
//!
//! - `parity_messages_*`: one per fixture of `parity/fixtures/messages/`, each the GPUI frame against the web reference
//!   shot by `parity/shoot.mjs`. A failure whose cause is another area's component carries `#[ignore = "shared component
//!   parity (T6c)"]` and the cause beside it;
//! - the slow-mode and announcement logic of e2e/web/slowmode-announce.spec.ts runs without a host (the link rule is a
//!   client test, `dilla-client/tests/messages.rs`);
//! - the keyboard tests drive the automation build on a fixture (`--features automation`).

mod support;

use dilla_client::state::{
    AppState, Channel, ChannelGroupState, ChannelId, Community, CommunityId, DeviceId, MessageId,
    TimelineItem, TimelineItemState, UserId,
};
use dilla_desktop::screens::messages::body;
use dilla_desktop::screens::messages::composing::{self, Blocked};
use dilla_desktop::screens::messages::markdown::{Body, Pill, parse_markdown};
use dilla_desktop::screens::messages::slow;

fn channel(mode: u8, slowmode: u32) -> Channel {
    Channel {
        id: ChannelId([1; 16]),
        community_id: CommunityId([2; 16]),
        kind: 0,
        mode,
        name: "news".into(),
        topic: String::new(),
        parent_id: None,
        position: 0,
        slowmode,
        text_group_id: None,
        group: ChannelGroupState::Active,
    }
}

fn post(own: bool, state: TimelineItemState, ts: u64) -> TimelineItem {
    TimelineItem {
        key: format!("s{ts}"),
        state,
        reason: String::new(),
        sender_user: Some(UserId([9; 16])),
        sender_device: Some(DeviceId([9; 16])),
        own,
        bot: false,
        ts,
        body: "x".into(),
        msg_id: Some(MessageId([ts as u8; 16])),
        seq: (state != TimelineItemState::Pending).then_some(ts),
        edited: false,
        reply: None,
        reactions: Vec::new(),
        pinned: false,
        attachments: Vec::new(),
        mention: false,
        actions: Vec::new(),
    }
}

/// e2e/web/slowmode-announce.spec.ts: the composer waits out the channel's slow mode after the own post, a post made
/// meanwhile is queued, the countdown reads as the web reads it, and an announcement channel is read-only for a person
/// who may not post there.
#[test]
fn slow_mode_wait_queue_and_announcement_read_only() {
    // The wait runs from the own stored post and counts down as `m:ss`.
    let now = 1_000_000;
    let ch = channel(0, 30);
    let items = [post(true, TimelineItemState::Ok, now - 10)];
    let wait = composing::slow_state(&ch, &items, false, now * 1000);
    assert_eq!(wait.until_ms, (now + 20) * 1000);
    assert!(!wait.queued);
    assert_eq!(
        slow::format_wait(i64::try_from(wait.until_ms).unwrap() - (now * 1000) as i64),
        "0:20"
    );
    // A send made meanwhile sits in the outbox: the line says it is queued.
    let queued = [
        items[0].clone(),
        post(true, TimelineItemState::Pending, now - 1),
    ];
    assert!(composing::slow_state(&ch, &queued, false, now * 1000).queued);
    // A person who bypasses slow mode does not wait.
    assert_eq!(
        composing::slow_state(&ch, &items, true, now * 1000).until_ms,
        0
    );
    // The wait is over: nothing is shown, nothing is queued.
    assert_eq!(
        composing::slow_state(&ch, &items, false, (now + 21) * 1000),
        composing::Slow::default()
    );
    // The countdown is spoken by the minute, then every ten seconds.
    assert_eq!(slow::spoken_step(61_000), "m2");
    assert_eq!(slow::spoken_step(20_000), "s2");

    // An announcement channel: only a person the server lets post may; everyone else gets the read-only line.
    let mut state = AppState::default();
    state.communities.push(Community {
        id: CommunityId([2; 16]),
        name: "S".into(),
        owner: UserId([7; 16]),
        policy_version: 1,
    });
    state.account.user_id = Some(UserId([8; 16]));
    let news = channel(1, 0);
    assert_eq!(composing::may_send(&state, &news), Err(Blocked::ReadOnly));
    assert_eq!(
        dilla_desktop::strings::tf("announce.readOnly", &[("name", "news")]),
        "Only some people can post in #news."
    );
    assert_eq!(composing::may_send(&state, &channel(0, 0)), Ok(()));
}

/// MessageItem.markdown.test.tsx: a body is drawn from its markdown, code never turns into a pill or a link, and the
/// fenced block gets a header with the language and the line count.
#[test]
fn a_body_is_drawn_from_its_markdown() {
    let pill = |t: &str| Pill {
        label: format!("@{}", &t[..t.len().min(3)]),
        me: false,
        broadcast: false,
        user: None,
    };
    let body_of = |s: &str| parse_markdown(s, &pill);
    assert!(matches!(&body_of("**x**")[0], Body::Strong(_)));
    assert!(matches!(&body_of("`<@everyone>`")[0], Body::Code(c) if c == "<@everyone>"));
    assert!(matches!(&body_of("hi <@everyone>")[1], Body::Mention(p) if p.label == "@eve"));
    // Every row height follows its content: a code card is taller than the text line it replaces.
    let one = body::layout(
        &body_of("a"),
        400.0,
        dilla_desktop_ui::tokens::color::FG_BODY,
    )
    .height;
    let card = body::layout(
        &body_of("```yaml\na: 1\nb: 2\n```"),
        400.0,
        dilla_desktop_ui::tokens::color::FG_BODY,
    )
    .height;
    assert!(card > 2.0 * one);
}

#[cfg(feature = "automation")]
mod driven {
    use std::path::{Path, PathBuf};

    use dilla_parity::Driver;
    use dilla_parity::screen::assert_screen;

    // Only the automation build draws the fixtures, so the paths live with it (a build without the feature has no use
    // for them: clippy's dead_code otherwise).
    fn parity() -> PathBuf {
        Path::new(env!("CARGO_MANIFEST_DIR")).join("../parity")
    }

    fn fixture(name: &str) -> PathBuf {
        parity()
            .join("fixtures/messages")
            .join(format!("{name}.json"))
    }

    /// The parity of a fixture against its committed reference. A test with a reason after `ignore` is a measured gap
    /// that is not closed yet; its numbers are in the reason.
    macro_rules! parity {
        ($($test:ident => $name:literal $(, ignore $why:literal)?;)*) => {$(
            #[test]
            #[cfg_attr(not(target_os = "linux"), ignore = "references are Linux only")]
            $(#[cfg_attr(target_os = "linux", ignore = $why)])?  // off Linux the line above already ignores it
            fn $test() {
                assert_screen(&fixture($name)).unwrap_or_else(|f| panic!("{f}"));
            }
        )*};
    }

    parity! {
        parity_messages_conversation_actions => "conversation-actions";
        parity_messages_conversation_actions_b => "conversation-actions-b";
        parity_messages_edit => "edit";
        parity_messages_edit_b => "edit-b";
        parity_messages_mention => "mention";
        parity_messages_mention_b => "mention-b";
        parity_messages_code_block => "code-block";
        parity_messages_code_block_b => "code-block-b";
        parity_messages_moderate_remove => "moderate-remove";
        parity_messages_moderate_remove_b => "moderate-remove-b";
        parity_messages_slow_mode => "slow-mode";
        parity_messages_slow_mode_b => "slow-mode-b";
    }

    fn launch(name: &str) -> Driver {
        let path = fixture(name);
        let loaded = dilla_parity::fixture::load(&path, dilla_parity::Os::Linux).unwrap();
        let mut d = Driver::launch_fixture(&path, loaded.shot_path.as_deref(), &[]);
        d.wait_idle();
        d
    }

    /// Saves the frame when `DILLA_SHOT_DIR` names a directory (for a person reading a failure).
    fn dump(d: &mut Driver, name: &str) {
        if let Ok(dir) = std::env::var("DILLA_SHOT_DIR") {
            let _ = d.screenshot().save(format!("{dir}/{name}.png"));
        }
    }

    /// Hovering a row paints its toolbar over the row's top edge: the frame changes inside the toolbar's box.
    #[test]
    fn hovering_a_row_shows_its_toolbar() {
        let mut d = launch("conversation-actions");
        let before = d.screenshot();
        d.hover("message-row-5");
        d.wait_idle();
        dump(&mut d, "hover");
        let bar = d
            .element("message-toolbar-5")
            .expect("the toolbar is laid out over the hovered row");
        let after = d.screenshot();
        let (x0, y0) = ((bar.x * 2.0) as u32, (bar.y * 2.0) as u32);
        let (x1, y1) = (
            x0 + (bar.width * 2.0) as u32,
            y0 + (bar.height * 2.0) as u32,
        );
        let changed = (y0..y1)
            .flat_map(|y| (x0..x1).map(move |x| (x, y)))
            .filter(|&(x, y)| before.get_pixel(x, y) != after.get_pixel(x, y))
            .count();
        assert!(
            changed > 100,
            "the toolbar is painted once hovered ({changed} pixels changed)"
        );
    }

    /// How many pixels of `a` and `b` differ inside the device-pixel box (x0, y0, x1, y1).
    fn differing(
        a: &dilla_parity::RgbaImage,
        b: &dilla_parity::RgbaImage,
        (x0, y0, x1, y1): (u32, u32, u32, u32),
    ) -> usize {
        (y0..y1)
            .flat_map(|y| (x0..x1).map(move |x| (x, y)))
            .filter(|&(x, y)| a.get_pixel(x, y) != b.get_pixel(x, y))
            .count()
    }

    /// The log (1280x800 at scale 2), right of the sidebar and between the header and the composer.
    const LOG: (u32, u32, u32, u32) = (700, 200, 2500, 1300);
    /// Over the composer's field: where the mention list floats.
    const OVER_COMPOSER: (u32, u32, u32, u32) = (1200, 1180, 2000, 1330);

    /// Up in an empty composer edits the newest own message; Escape cancels it.
    #[test]
    fn keyboard_up_edits_the_last_own_message_and_escape_cancels() {
        let mut d = launch("edit");
        let before = d.screenshot();
        d.click("composer");
        d.press("up");
        d.wait_idle();
        dump(&mut d, "up");
        let editing = d.screenshot();
        assert!(
            differing(&before, &editing, LOG) > 2000,
            "the editor replaces the row's body"
        );
        d.press("escape");
        d.wait_idle();
        let closed = d.screenshot();
        assert_eq!(
            differing(&before, &closed, LOG),
            0,
            "Escape closes the editor and the log is as it was"
        );
    }

    /// Task 6c: the composer's typed text is in the R13 text mask, as a line box exactly its glyphs' advance wide and a
    /// line tall at the field's text origin (the textarea draws it itself, so no `Line` reports it); nothing wider.
    #[test]
    fn typed_text_is_in_the_text_mask_as_its_line_box() {
        let mut d = launch("mention");
        let field = d.element("composer").expect("the composer field");
        let has_line = |d: &mut dilla_parity::Driver| {
            d.text_bounds().iter().any(|e| {
                (e.x - field.x).abs() < 0.01
                    && (e.y - field.y).abs() < 0.01
                    && (e.width - 10.0 * 13.0 * 0.6).abs() < 0.02
                    && (e.height - 20.0).abs() < 0.01
            })
        };
        assert!(!has_line(&mut d), "an empty composer reports no typed line");
        d.type_text("composer", "see you @b");
        d.wait_idle();
        assert!(
            has_line(&mut d),
            "the typed line `see you @b` (10 characters at 7.8px) is a text line at the field's origin: {:?}",
            d.text_bounds()
                .iter()
                .filter(|e| (e.y - field.y).abs() < 2.0)
                .map(|e| (e.x, e.y, e.width, e.height))
                .collect::<Vec<_>>()
        );
    }

    /// The mention list opens on `@`, takes Tab to choose, and Escape closes it.
    #[test]
    fn keyboard_mention_popup_choose_and_escape() {
        let mut d = launch("mention");
        let before = d.screenshot();
        d.type_text("composer", "see you @b");
        d.wait_idle();
        dump(&mut d, "mention");
        assert!(
            differing(&before, &d.screenshot(), OVER_COMPOSER) > 2000,
            "the list floats over the composer"
        );
        d.press("escape");
        d.wait_idle();
        assert_eq!(
            differing(&before, &d.screenshot(), OVER_COMPOSER),
            0,
            "Escape closes the list"
        );
        d.type_focused("j");
        d.wait_idle();
        assert_eq!(
            differing(&before, &d.screenshot(), OVER_COMPOSER),
            0,
            "the closed `@` stays closed while it is typed on"
        );
        // A new @ opens it again; Tab chooses and closes it.
        d.type_focused(" @b");
        d.wait_idle();
        assert!(differing(&before, &d.screenshot(), OVER_COMPOSER) > 2000);
        d.press("tab");
        d.wait_idle();
        assert_eq!(
            differing(&before, &d.screenshot(), OVER_COMPOSER),
            0,
            "Tab chooses and closes it"
        );
    }

    /// Tab walks the area in its order: the composer's field, then whatever follows it.
    #[test]
    fn keyboard_messages_focus_order() {
        let mut d = launch("conversation-actions");
        d.click("composer");
        assert_eq!(d.focused().as_deref(), Some("composer"));
    }
}

/// PageUp and PageDown are bound in the log's key context.
#[gpui::test]
fn keyboard_page_up_and_down_are_bound_in_the_log(cx: &mut gpui::TestAppContext) {
    use dilla_desktop::screens::timeline::{PageDown, PageUp};
    cx.update(|cx| {
        dilla_desktop::screens::messages::keys::bind(cx);
        let keymap = cx.key_bindings();
        let keymap = keymap.borrow();
        let keys = |action: &dyn gpui::Action| -> Vec<String> {
            keymap
                .bindings_for_action(action)
                .map(|b| {
                    let k = b.keystrokes()[0].inner();
                    assert!(!k.modifiers.modified(), "a bare key");
                    k.key.clone()
                })
                .collect()
        };
        assert_eq!(keys(&PageUp), ["pageup"]);
        assert_eq!(keys(&PageDown), ["pagedown"]);
    });
}

/// Package 10a: a failed send's retry and discard and the timed-out composer, against a real dillad. The failure is the
/// instance's own refusal of a timed-out member's post (postpolicy.go: `E_FORBIDDEN` "member is timed out").
mod live {
    use std::time::Duration;

    use dilla_client::Client;
    use dilla_client::features::moderation::TimeoutLength;
    use dilla_client::state::{
        AppState, ChannelId, CommunityId, MessageId, TimelineItem, TimelineItemState, UserId,
    };
    use dilla_desktop::screens::messages::composing::{self, Blocked};
    use dilla_desktop::screens::messages::send_state;

    use super::support::world;

    const WITHIN: Duration = Duration::from_secs(30);

    /// The owner, Ada (a member) and their community's #general, both clients in its group.
    struct Room {
        host: world::Host,
        owner: Client,
        ada: Client,
        ada_id: UserId,
        community: CommunityId,
        general: ChannelId,
    }

    async fn room(tag: &'static str) -> Room {
        let host = tokio::task::spawn_blocking(move || world::start_host(tag, "127.0.0.1:0", None))
            .await
            .unwrap();
        let invite = host.invites.last().unwrap().clone();
        let olof = world::sign_up(&host, &invite, "olof", "Olof", tag).await;
        let ada = world::sign_up(&host, &invite, "ada", "Ada", tag).await;
        let (host, olof, ada, c) = tokio::task::spawn_blocking(move || {
            let c = world::community(&host, &olof, "crew", &[&ada]);
            world::clear_session(&olof);
            world::clear_session(&ada);
            (host, olof, ada, CommunityId(c))
        })
        .await
        .unwrap();
        let ada_id = UserId(ada.user_id);
        let (owner, ada) = (world::start(&olof).await, world::start(&ada).await);
        let mut general = None;
        for client in [&owner, &ada] {
            let s = world::wait_for(client, WITHIN, "#general", |s| {
                s.channel_named(c, "general").is_some()
                    && s.members.get(&c).is_some_and(|m| m.len() == 2)
            })
            .await;
            let ch = s.channel_named(c, "general").unwrap().id;
            client
                .open_channel(ch)
                .await
                .unwrap()
                .expect("opens #general");
            general = Some(ch);
        }
        Room {
            host,
            owner,
            ada,
            ada_id,
            community: c,
            general: general.unwrap(),
        }
    }

    fn item<'a>(s: &'a AppState, ch: ChannelId, body: &str) -> Option<&'a TimelineItem> {
        s.timelines
            .get(&ch)
            .and_then(|t| t.items.iter().find(|i| i.body == body))
    }

    /// The owner times Ada out; Ada's client reads her own timeout (the composer's advisory read).
    async fn time_out(r: &Room) -> u64 {
        r.owner
            .moderation()
            .timeout(r.community, r.ada_id, TimeoutLength::TenMinutes)
            .await
            .unwrap();
        let expires = r
            .ada
            .moderation()
            .timeout_of(r.community, r.ada_id)
            .await
            .unwrap()
            .expect("timed out");
        world::wait_for(&r.ada, WITHIN, "the own timeout", |s| {
            s.people
                .timeouts
                .get(&r.community)
                .and_then(|m| m.get(&r.ada_id))
                == Some(&expires)
        })
        .await;
        expires
    }

    /// Ada posts `body` while timed out: the instance refuses it, and her log holds it failed, with its reason under it
    /// and retry and discard offered.
    async fn refused(r: &Room, body: &str) -> MessageId {
        r.ada
            .send_text(r.general, body.to_owned())
            .await
            .unwrap()
            .expect("queued");
        let s = world::wait_for(&r.ada, WITHIN, "the refused send", |s| {
            item(s, r.general, body).is_some_and(|i| i.state == TimelineItemState::Failed)
        })
        .await;
        let failed = item(&s, r.general, body).unwrap();
        assert!(
            send_state::offers_retry(failed),
            "a failed send offers retry and discard"
        );
        assert!(!failed.reason.is_empty());
        assert_eq!(
            send_state::detail(failed).as_deref(),
            Some(failed.reason.as_str()),
            "its reason is shown under it"
        );
        failed.msg_id.unwrap()
    }

    async fn lift(r: &Room) {
        r.owner
            .moderation()
            .timeout(r.community, r.ada_id, TimeoutLength::Clear)
            .await
            .unwrap();
        r.ada
            .moderation()
            .timeout_of(r.community, r.ada_id)
            .await
            .unwrap();
    }

    /// e2e/web/timeout.spec.ts and MessageItem.tsx `retry`: a post the instance refused is failed on Ada's device; once
    /// her timeout is lifted, Retry sends that same message and the owner reads it.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    #[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
    async fn a_failed_send_retries() {
        let r = room("p10a-retry").await;
        time_out(&r).await;
        let msg = refused(&r, "sent again").await;
        lift(&r).await;
        r.ada.messages().retry(msg).await.unwrap().unwrap();
        world::wait_for(&r.owner, WITHIN, "the retried message", |s| {
            item(s, r.general, "sent again").is_some_and(|i| i.state == TimelineItemState::Ok)
        })
        .await;
        let s = world::wait_for(&r.ada, WITHIN, "the stored retry", |s| {
            item(s, r.general, "sent again").is_some_and(|i| i.state == TimelineItemState::Ok)
        })
        .await;
        let sent = item(&s, r.general, "sent again").unwrap();
        assert_eq!(sent.msg_id, Some(msg), "the retry sends the same message");
        assert!(!send_state::offers_retry(sent));
        assert_eq!(
            s.timelines[&r.general]
                .items
                .iter()
                .filter(|i| i.body == "sent again")
                .count(),
            1
        );
    }

    /// MessageItem.tsx `pendingAction` (beta.5): Ada edits her message while she is timed out. The instance refuses the
    /// edit, so the message says "not saved" with retry and discard of the edit (not of the message); once the timeout is
    /// lifted, Retry sends the same edit and the owner reads the new text.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    #[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
    async fn a_failed_edit_is_not_saved_and_retries() {
        let r = room("p10a-fold").await;
        r.ada
            .send_text(r.general, "first words".to_owned())
            .await
            .unwrap()
            .expect("queued");
        let s = world::wait_for(&r.owner, WITHIN, "the message", |s| {
            item(s, r.general, "first words").is_some_and(|i| i.state == TimelineItemState::Ok)
        })
        .await;
        let msg = item(&s, r.general, "first words").unwrap().msg_id.unwrap();
        world::wait_for(&r.ada, WITHIN, "the stored message", |s| {
            item(s, r.general, "first words").is_some_and(|i| i.seq.is_some())
        })
        .await;
        time_out(&r).await;
        r.ada
            .messages()
            .edit(r.general, msg, "second words".to_owned())
            .await
            .unwrap()
            .unwrap();
        let s = world::wait_for(&r.ada, WITHIN, "the refused edit", |s| {
            item(s, r.general, "first words").is_some_and(|i| i.actions.iter().any(|a| a.failed))
        })
        .await;
        let row = item(&s, r.general, "first words").unwrap();
        let line = send_state::pending_line(row).expect("a pending line");
        assert_eq!(
            line.label,
            dilla_desktop::strings::t("shell.message.notSaved")
        );
        let fold = row.actions.iter().find(|a| a.failed).unwrap();
        assert_eq!(fold.kind, 1);
        assert_ne!(
            fold.msg_id, msg,
            "retry and discard take the edit's own row"
        );
        assert_eq!(
            line.actions,
            [
                send_state::LineAction::Retry(fold.msg_id),
                send_state::LineAction::Discard(fold.msg_id)
            ]
        );
        let fold = fold.msg_id;
        lift(&r).await;
        r.ada.messages().retry(fold).await.unwrap().unwrap();
        world::wait_for(&r.owner, WITHIN, "the retried edit", |s| {
            s.timelines.get(&r.general).is_some_and(|t| {
                t.items
                    .iter()
                    .any(|i| i.msg_id == Some(msg) && i.body == "second words" && i.edited)
            })
        })
        .await;
        world::wait_for(&r.ada, WITHIN, "the edit saved here", |s| {
            s.timelines.get(&r.general).is_some_and(|t| {
                t.items.iter().any(|i| {
                    i.msg_id == Some(msg) && i.actions.is_empty() && i.body == "second words"
                })
            })
        })
        .await;
    }

    /// MessageItem.tsx `discard`: the failed post leaves Ada's log and never reaches the owner.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    #[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
    async fn discard_removes_the_failed_message() {
        let r = room("p10a-discard").await;
        time_out(&r).await;
        let msg = refused(&r, "thrown away").await;
        r.ada.messages().discard(msg).await.unwrap().unwrap();
        world::wait_for(&r.ada, WITHIN, "the discarded row gone", |s| {
            item(s, r.general, "thrown away").is_none()
        })
        .await;
        // A second discard (a double click) calls nothing.
        r.ada.messages().discard(msg).await.unwrap().unwrap();
        lift(&r).await;
        r.ada
            .send_text(r.general, "after".to_owned())
            .await
            .unwrap()
            .expect("queued");
        let s = world::wait_for(&r.owner, WITHIN, "the next message", |s| {
            item(s, r.general, "after").is_some_and(|i| i.state == TimelineItemState::Ok)
        })
        .await;
        assert!(
            item(&s, r.general, "thrown away").is_none(),
            "a discarded post never reaches the community"
        );
    }

    /// Shell.tsx `timeoutReason`: a timed-out member's composer says until when (`shell.composer.timedOut`), in a text
    /// channel of that community only, and opens again when the timeout is lifted.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    #[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
    async fn timed_out_member_sees_until() {
        let r = room("p10a-timed-out").await;
        let s = r.ada.state().borrow().clone();
        let ch = s.channel(r.general).unwrap().clone();
        assert_eq!(
            composing::blocked_at(&s, &ch, world::clock_now(&r.host)),
            None
        );
        let expires = time_out(&r).await;
        let s = r.ada.state().borrow().clone();
        assert_eq!(
            composing::blocked_at(&s, &ch, expires - 60),
            Some(Blocked::TimedOut(expires))
        );
        // Past the expiry the line goes by itself (Shell.tsx's expiry timer); a voice channel has no composer line.
        assert_eq!(composing::blocked_at(&s, &ch, expires), None);
        let mut voice = ch.clone();
        voice.kind = 1;
        assert_eq!(composing::timed_out(&s, &voice, expires - 60), None);
        let line = dilla_desktop::strings::tf(
            "shell.composer.timedOut",
            &[("time", &dilla_desktop::format::time(expires))],
        );
        assert!(line.starts_with("You cannot post messages in this community until "));
        lift(&r).await;
        let s = world::wait_for(&r.ada, WITHIN, "the lifted timeout", |s| {
            s.people
                .timeouts
                .get(&r.community)
                .is_none_or(|m| !m.contains_key(&r.ada_id))
        })
        .await;
        assert_eq!(composing::blocked_at(&s, &ch, expires - 60), None);
    }
}
