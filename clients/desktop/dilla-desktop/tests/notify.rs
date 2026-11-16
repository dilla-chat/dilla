//! Task 22's parity, behaviour and keyboard tests for the notify area (standard procedure S4-S6).
//!
//! - `parity_notify_*`: each fixture of `parity/fixtures/notify/` against its web reference (`assert_screen`, Linux only).
//!   The two `settings` fixtures are drawn in the settings host.
//! - `keyboard_notify_focus_order`: Tab goes through the section's controls in the web's order, and each control reports
//!   its change.
//! - The behaviours of `e2e/web/badges.spec.ts` that this area's client answers are in `dilla-client/tests/notify.rs`.

use std::cell::RefCell;
use std::path::{Path, PathBuf};
use std::rc::Rc;

use dilla_client::features::notify::NotifyMode;
use dilla_client::state::{AppState, Community, CommunityId, UserId};
use dilla_desktop::screens::notify::{Change, Sink, model, view};
use dilla_desktop::screens::{FocusNext, FocusPrevious};
use dilla_parity::screen::assert_screen;
use gpui::{
    Context, FocusHandle, InteractiveElement, IntoElement, ParentElement, Render, Styled,
    TestAppContext, Window, div,
};

fn fixture(name: &str) -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../parity/fixtures/notify")
        .join(format!("{name}.json"))
}

fn parity(name: &str) {
    let report = assert_screen(&fixture(name))
        .unwrap_or_else(|e| panic!("notify/{name} differs from its web reference: {e}"));
    println!(
        "SCREEN notify/{name}: {} text lines, chrome {:.4} %",
        report.text_lines, report.chrome_pct
    );
}

#[test]
#[cfg_attr(
    not(any(target_os = "linux", target_os = "macos")),
    ignore = "references are Linux and macOS only"
)]
fn parity_notify_settings() {
    parity("settings");
}

#[test]
#[cfg_attr(
    not(any(target_os = "linux", target_os = "macos")),
    ignore = "references are Linux and macOS only"
)]
fn parity_notify_settings_b() {
    parity("settings-b");
}

#[test]
#[cfg_attr(
    not(any(target_os = "linux", target_os = "macos")),
    ignore = "references are Linux and macOS only"
)]
fn parity_notify_unread_badge() {
    parity("unread-badge");
}

#[test]
#[cfg_attr(
    not(any(target_os = "linux", target_os = "macos")),
    ignore = "references are Linux and macOS only"
)]
fn parity_notify_unread_badge_b() {
    parity("unread-badge-b");
}

#[test]
#[cfg_attr(
    not(any(target_os = "linux", target_os = "macos")),
    ignore = "references are Linux and macOS only"
)]
fn parity_notify_dm_badge() {
    parity("dm-badge");
}

#[test]
#[cfg_attr(
    not(any(target_os = "linux", target_os = "macos")),
    ignore = "references are Linux and macOS only"
)]
fn parity_notify_dm_badge_b() {
    parity("dm-badge-b");
}

// ---- keyboard ---------------------------------------------------------------------------------------------------------

const SERVER: CommunityId = CommunityId([1; 16]);

struct Host {
    root: FocusHandle,
    changes: Rc<RefCell<Vec<Change>>>,
    state: AppState,
}

impl Render for Host {
    fn render(&mut self, _window: &mut Window, _cx: &mut Context<Self>) -> impl IntoElement {
        let changes = self.changes.clone();
        let sink: Sink = Rc::new(move |change, _| changes.borrow_mut().push(change));
        let empty = std::collections::HashMap::<String, String>::new();
        let section = view(&model(&self.state, None, &empty), sink);
        div()
            .id("host")
            .track_focus(&self.root)
            .size_full()
            .on_action(|_: &FocusNext, window, cx| window.focus_next(cx))
            .on_action(|_: &FocusPrevious, window, cx| window.focus_prev(cx))
            .child(section)
    }
}

fn state() -> AppState {
    AppState {
        communities: vec![Community {
            id: SERVER,
            name: "Midgard Crew".into(),
            owner: UserId([9; 16]),
            policy_version: 0,
        }],
        ..AppState::default()
    }
}

/// Presses a key as a keyboard does: down, then up (GPUI turns Space down-and-up on a focused switch into a click).
fn press(cx: &mut gpui::VisualTestContext, key: &'static str) {
    cx.simulate_keystrokes(key);
    cx.simulate_event(gpui::KeyUpEvent {
        keystroke: gpui::Keystroke::parse(key).expect("a key"),
    });
    cx.run_until_parked();
}

/// Tab goes server mode (the checked option holds the tab stop), @everyone, desktop notifications, sounds; the arrows move
/// the mode, Space flips a switch, and each reports exactly the change the web's control writes.
#[gpui::test]
fn keyboard_notify_focus_order(cx: &mut TestAppContext) {
    cx.update(dilla_desktop::install);
    let changes = Rc::new(RefCell::new(Vec::new()));
    let (_host, cx) = cx.add_window_view({
        let changes = changes.clone();
        move |_, cx| Host {
            root: cx.focus_handle(),
            changes,
            state: state(),
        }
    });
    let root = _host.read_with(cx, |h, _| h.root.clone());
    cx.update(|window, cx| root.focus(window, cx));

    cx.simulate_keystrokes("tab");
    cx.simulate_keystrokes("right");
    cx.run_until_parked();
    assert_eq!(
        changes.borrow().as_slice(),
        [Change::ServerMode(SERVER, NotifyMode::Nothing)],
        "the mode starts on Mentions; Right moves to Nothing"
    );
    changes.borrow_mut().clear();

    for (stop, want) in [
        ("@everyone", Change::EveryoneNotifies(SERVER, false)),
        ("desktop notifications", Change::Desktop(false)),
        ("sounds", Change::Sounds(false)),
    ] {
        cx.simulate_keystrokes("tab");
        press(cx, "space");
        assert_eq!(
            changes.borrow().as_slice(),
            [want],
            "the {stop} switch holds the next tab stop"
        );
        changes.borrow_mut().clear();
    }
    cx.simulate_keystrokes("shift-tab shift-tab");
    press(cx, "space");
    assert_eq!(
        changes.borrow().as_slice(),
        [Change::EveryoneNotifies(SERVER, false)],
        "Shift+Tab goes back the same way"
    );
}

// ---- OS notifications (GPUI parity Package 4: beta.5 `main.ts` `notify`, `dismiss-notification`) ----

mod os {
    use std::sync::Arc;

    use dilla_client::features::notify::{Notification, NotificationKind, Notifier};
    use dilla_client::seams::InstanceId;
    use dilla_client::state::{
        AppState, Channel, ChannelGroupState, ChannelId, Community, CommunityId, UserId,
    };
    use dilla_desktop::native::NativeShell;
    use dilla_desktop::native::notifier::{self, MAX_LIVE, Owner};
    use dilla_desktop::router::Route;
    use dilla_desktop::screens::shell::{Clock, ShellView};
    use gpui::{SystemNotificationResponse, TestAppContext};

    const SERVER: CommunityId = CommunityId([0x51; 16]);
    const GENERAL: ChannelId = ChannelId([0x61; 16]);
    const RANDOM: ChannelId = ChannelId([0x62; 16]);

    fn note(tag: &str, channel: Option<ChannelId>) -> Notification {
        let mut n = Notification::new(NotificationKind::Mention, "#general · Midgard", "hi");
        n.tag = tag.into();
        n.channel = channel;
        n
    }

    fn server() -> AppState {
        let mut s = AppState::default();
        s.communities.push(Community {
            id: SERVER,
            name: "Midgard".into(),
            owner: UserId([9; 16]),
            policy_version: 0,
        });
        s.channels = [(GENERAL, "general"), (RANDOM, "random")]
            .into_iter()
            .zip(0u32..)
            .map(|((id, name), position)| Channel {
                id,
                community_id: SERVER,
                kind: 0,
                mode: 0,
                name: name.into(),
                topic: String::new(),
                parent_id: None,
                position,
                slowmode: 0,
                text_group_id: None,
                group: ChannelGroupState::Active,
            })
            .collect();
        s
    }

    fn installed(cx: &mut TestAppContext) {
        cx.update(|cx| {
            dilla_desktop::install(cx);
            notifier::install(cx);
        });
    }

    /// `00-mac-notifications.spec.ts` (its platform part): what the engine delivers on its own thread reaches the
    /// platform's notification centre through the UI thread, under the app's identity.
    #[gpui::test]
    fn os_notifier_shows_through_the_platform(cx: &mut TestAppContext) {
        installed(cx);
        let n = cx.update(|cx| notifier::os_notifier(InstanceId(1), 1, cx));
        std::thread::spawn(move || n.deliver(&note("dilla:c1", Some(GENERAL))))
            .join()
            .unwrap();
        cx.run_until_parked();
        let shown = cx.shown_system_notifications();
        assert_eq!(shown.len(), 1);
        assert_eq!(shown[0].tag.as_ref(), "dilla:1:dilla:c1");
        assert_eq!(shown[0].title.as_ref(), "#general · Midgard");
        assert!(shown[0].actions.is_empty(), "a message has no buttons");
        assert_eq!(
            cx.app_identity().map(|(id, _)| id),
            Some(dilla_desktop::APP_ID.into())
        );
    }

    /// beta.5 `notification-click`: the body brings the instance's window forward and opens the conversation; the
    /// notification is gone after.
    #[gpui::test]
    fn a_click_focuses_the_window_and_opens_the_conversation(cx: &mut TestAppContext) {
        installed(cx);
        let (shell, vcx) = cx.add_window_view(|window, cx| {
            ShellView::new(
                Arc::new(server()),
                None,
                None,
                Clock::Fixed { now: 0, utc: true },
                window,
                cx,
            )
        });
        shell.update(vcx, |s, cx| {
            s.navigate(
                Route::Channel {
                    community: SERVER,
                    channel: Some(GENERAL),
                },
                cx,
            )
        });
        vcx.deactivate_window();
        vcx.run_until_parked();
        let window = vcx.update(|window, _| window.window_handle().window_id());
        let tag = vcx.update(|_, cx| {
            NativeShell::window_opened(window, Some(InstanceId(4)), cx);
            notifier::show(
                Owner::Instance(InstanceId(4)),
                &note("dilla:c2", Some(RANDOM)),
                cx,
            )
        });
        vcx.cx.update(|cx| {
            notifier::respond(
                SystemNotificationResponse {
                    tag: tag.clone(),
                    action_id: None,
                },
                cx,
            )
        });
        vcx.run_until_parked();
        assert!(
            vcx.update(|window, _| window.is_window_active()),
            "the window came forward"
        );
        assert_eq!(
            shell.read_with(vcx, |s, _| s.route().clone()),
            Route::Channel {
                community: SERVER,
                channel: Some(RANDOM)
            }
        );
        assert!(vcx.update(|_, cx| notifier::live_tags(cx)).is_empty());
    }

    /// beta.5 `closeNotices`: closing an instance's window withdraws that instance's notifications, and only those.
    #[gpui::test]
    fn closing_the_window_withdraws_its_notifications(cx: &mut TestAppContext) {
        installed(cx);
        let w = cx
            .add_empty_window()
            .update(|w, _| w.window_handle().window_id());
        cx.update(|cx| {
            NativeShell::window_opened(w, Some(InstanceId(1)), cx);
            notifier::show(Owner::Instance(InstanceId(1)), &note("a", None), cx);
            notifier::show(Owner::Instance(InstanceId(2)), &note("b", None), cx);
            notifier::show(Owner::Instance(InstanceId(1)), &note("c", None), cx);
            NativeShell::window_closed(w, cx);
            assert_eq!(notifier::live_tags(cx), ["dilla:2:b"]);
        });
        assert_eq!(
            cx.dismissed_system_notifications(),
            ["dilla:1:a", "dilla:1:c"]
        );
        assert_eq!(cx.delivered_system_notifications().len(), 1);
    }

    /// beta.5's 64 live notifications: the oldest message goes to make room (GPUI reports no close, so refusing the
    /// 65th as beta.5 did would silence the app for good); a live ring outlasts the messages.
    #[gpui::test]
    fn at_most_64_live(cx: &mut TestAppContext) {
        installed(cx);
        let w = cx
            .add_empty_window()
            .update(|w, _| w.window_handle().window_id());
        cx.update(|cx| {
            let mut ring = Notification::new(
                NotificationKind::Ring,
                "Bob is calling",
                "Open dilla to answer.",
            );
            ring.tag = "dilla-ring-1".into();
            let ring_tag = notifier::show_ring(w, "k".into(), &ring, cx);
            for i in 0..MAX_LIVE + 5 {
                notifier::show(
                    Owner::Instance(InstanceId(1)),
                    &note(&format!("m{i}"), None),
                    cx,
                );
            }
            let live = notifier::live_tags(cx);
            assert_eq!(live.len(), MAX_LIVE);
            assert_eq!(live[0], ring_tag, "the ring stays");
            assert_eq!(
                live[1].as_ref(),
                "dilla:1:m6",
                "the oldest messages went first"
            );
        });
        assert_eq!(cx.dismissed_system_notifications().len(), 6);
    }
}
