//! Task 13's parity, behaviour and keyboard tests for the dms area (standard procedure S4-S6).
//!
//! The views run in a real (test) window over a trust slice as the client publishes it, and are driven with keystrokes.
//! The trust behaviours against a real dillad (trust-polish.spec.ts) are the client's `tests/dms.rs`; here: what the
//! Privacy section and the compare dialog do with the slice, the focus rules, and the DM picker's keys.

use std::sync::Arc;

use dilla_client::state::trust::{TrustState, TrustSummary};
use dilla_client::state::{AppState, Member, UserId};
use dilla_desktop::screens::dms::{compare, conversation, picker, privacy};
use dilla_desktop::screens::shell::Clock;
use dilla_desktop::screens::{FocusNext, FocusPrevious};
use dilla_desktop::slots::SlotCx;
use gpui::{
    Context, FocusHandle, InteractiveElement, IntoElement, ParentElement, Render, Styled,
    TestAppContext, VisualTestContext, Window, div, px,
};

fn contact(n: u8, name: &str) -> TrustSummary {
    TrustSummary {
        user_id: UserId([n; 16]),
        name: name.into(),
        shared: true,
        umk: format!("{n:02x}").repeat(32),
        first_seen: 1,
        verified: false,
        changed_umk: None,
        unlisted: false,
        removed_in: Vec::new(),
        blocked: None,
    }
}

fn state_with(contacts: Vec<TrustSummary>) -> AppState {
    AppState {
        trust: TrustState {
            loaded: true,
            contacts,
            ..TrustState::default()
        },
        ..AppState::default()
    }
}

/// A window with an opener, the Privacy section and the two overlays, as the shell draws them.
struct Host {
    state: Arc<AppState>,
    opener: FocusHandle,
}

impl Render for Host {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let ctx = SlotCx {
            state: self.state.clone(),
            client: None,
            community: None,
            channel: None,
            clock: Clock::Local,
        };
        div()
            .id("host")
            .relative()
            .size_full()
            .on_action(|_: &FocusNext, window, cx| window.focus_next(cx))
            .on_action(|_: &FocusPrevious, window, cx| window.focus_prev(cx))
            .child(
                div()
                    .id("opener")
                    .track_focus(&self.opener)
                    .tab_stop(true)
                    .w(px(80.0))
                    .h(px(24.0)),
            )
            .child(privacy::section(&ctx, window, cx))
            .children(compare::compare_overlay(&self.state, window, cx))
            .children(picker::picker_overlay(&self.state, window, cx))
    }
}

fn install(cx: &mut TestAppContext) {
    cx.update(dilla_desktop::install);
}

fn host(cx: &mut TestAppContext, state: AppState) -> (gpui::Entity<Host>, &mut VisualTestContext) {
    install(cx);
    cx.add_window_view(|_, cx| Host {
        state: Arc::new(state),
        opener: cx.focus_handle().tab_stop(true),
    })
}

fn focused(cx: &mut VisualTestContext, handle: &FocusHandle) -> bool {
    cx.update(|window, _| handle.is_focused(window))
}

/// Presses a key as a keyboard does: down, then up (Enter on a focused button is a click).
fn press(cx: &mut VisualTestContext, key: &'static str) {
    cx.simulate_keystrokes(key);
    cx.simulate_event(gpui::KeyUpEvent {
        keystroke: gpui::Keystroke::parse(key).expect("a key"),
    });
    cx.run_until_parked();
}

/// S6: Tab goes from the opener to each contact's Compare, in the list's order, and back.
#[gpui::test]
fn keyboard_dms_focus_order(cx: &mut TestAppContext) {
    let mut verified = contact(2, "Bo");
    verified.verified = true;
    let (host, cx) = host(cx, state_with(vec![contact(1, "Ada"), verified]));
    cx.run_until_parked();
    let opener = host.read_with(cx, |h, _| h.opener.clone());
    let (first, second) = cx.update(|_, cx| {
        (
            privacy::compare_handle(0, cx),
            privacy::compare_handle(1, cx),
        )
    });
    cx.update(|window, cx| opener.focus(window, cx));
    cx.simulate_keystrokes("tab");
    assert!(focused(cx, &first), "Tab reaches Compare with Ada first");
    cx.simulate_keystrokes("tab");
    assert!(focused(cx, &second), "then Compare with Bo");
    cx.simulate_keystrokes("shift-tab");
    assert!(focused(cx, &first), "Shift+Tab goes back");
}

/// The Compare dialog opens from Compare, traps focus, and Escape closes it and returns focus to that Compare.
#[gpui::test]
fn compare_dialog_traps_focus_and_escape_returns_it(cx: &mut TestAppContext) {
    let (host, cx) = host(cx, state_with(vec![contact(1, "Ada")]));
    cx.run_until_parked();
    let (opener, compare_button) = (
        host.read_with(cx, |h, _| h.opener.clone()),
        cx.update(|_, cx| privacy::compare_handle(0, cx)),
    );
    cx.update(|window, cx| compare_button.focus(window, cx));
    press(cx, "enter");
    let open = cx.update(|_, cx| cx.global::<compare::CompareModel>().open);
    assert!(open, "Enter on Compare opens the dialog");
    assert!(
        !focused(cx, &compare_button),
        "opening moves focus into the dialog"
    );
    for _ in 0..8 {
        cx.simulate_keystrokes("tab");
        assert!(
            !focused(cx, &compare_button) && !focused(cx, &opener),
            "Tab stays inside the dialog"
        );
        assert!(cx.update(|w, cx| w.focused(cx).is_some()));
    }
    for _ in 0..3 {
        cx.simulate_keystrokes("shift-tab");
        assert!(!focused(cx, &compare_button) && !focused(cx, &opener));
    }
    cx.simulate_keystrokes("escape");
    cx.run_until_parked();
    assert!(!cx.update(|_, cx| cx.global::<compare::CompareModel>().open));
    assert!(
        focused(cx, &compare_button),
        "Escape returns focus to the Compare that opened it"
    );
}

/// trust-polish.spec.ts "Copy alone does not approve": Copy leaves "All 12 blocks match" unavailable; only the person's
/// confirmation of exactly this comparison makes it available, and a comparison key that moves takes it back.
#[gpui::test]
fn copy_alone_does_not_approve(cx: &mut TestAppContext) {
    let mut changed = contact(1, "Ada");
    changed.changed_umk = Some("cc".repeat(32));
    changed.blocked = Some(dilla_client::state::trust::TrustBlock::UmkChanged);
    let state = state_with(vec![changed.clone()]);
    let (host, cx) = host(cx, state.clone());
    let groups: Vec<String> = (0..12).map(|i| format!("{:05}", i * 7)).collect();
    cx.update(|_, cx| {
        compare::open(&changed, None, cx);
        cx.global_mut::<compare::CompareModel>().groups = groups.clone();
    });
    cx.run_until_parked();
    let shown = cx.update(|_, cx| cx.global::<compare::CompareModel>().approved_umk.clone());
    assert_eq!(
        shown.as_deref(),
        Some("cc".repeat(32).as_str()),
        "the dialog compares the pending key"
    );
    cx.update(|_, cx| compare::copy(cx));
    assert_eq!(
        cx.update(|_, cx| cx.read_from_clipboard().and_then(|c| c.text())),
        Some(groups.join(" ")),
        "Copy puts the twelve groups on the clipboard"
    );
    assert!(
        !cx.update(|_, cx| compare::permitted(&state, cx)),
        "Copy alone approves nothing"
    );
    cx.update(|_, cx| compare::toggle_confirm(cx));
    assert!(
        cx.update(|_, cx| compare::permitted(&state, cx)),
        "the exact comparison, confirmed, may approve"
    );
    // The key moved while the dialog was open: the confirmation was for another key.
    let mut moved = changed.clone();
    moved.changed_umk = Some("dd".repeat(32));
    let moved_state = state_with(vec![moved]);
    assert!(!cx.update(|_, cx| compare::permitted(&moved_state, cx)));
    host.update(cx, |h, cx| {
        h.state = Arc::new(moved_state.clone());
        cx.notify();
    });
    cx.run_until_parked();
    // Another number for the same key is another comparison: the confirmation does not carry over.
    cx.update(|_, cx| {
        cx.global_mut::<compare::CompareModel>().groups[0] = "99999".into();
    });
    assert!(!cx.update(|_, cx| compare::permitted(&state, cx)));
    // An unlisted contact is never verifiable from the dialog.
    let mut unlisted = changed;
    unlisted.unlisted = true;
    cx.update(|_, cx| compare::toggle_confirm(cx));
    assert!(!cx.update(|_, cx| compare::permitted(&state_with(vec![unlisted]), cx)));
}

/// The DM picker: Up and Down move (wrapping), typing jumps to the next name with that prefix, Escape closes it.
#[gpui::test]
fn dm_picker_type_ahead_arrows_and_enter(cx: &mut TestAppContext) {
    let community = dilla_client::state::CommunityId([7; 16]);
    let me = UserId([9; 16]);
    let mut state = state_with(Vec::new());
    state.account.user_id = Some(me);
    state.communities = vec![dilla_client::state::Community {
        id: community,
        name: "Midgard".into(),
        owner: me,
        policy_version: 0,
    }];
    let member = |n: u8, name: &str| Member {
        user_id: UserId([n; 16]),
        username: name.to_lowercase(),
        display: name.into(),
        kind: 0,
        role_ids: Vec::new(),
        status_msg: String::new(),
        bio: String::new(),
    };
    state.members.insert(
        community,
        vec![
            member(9, "Me"),
            member(1, "Ada"),
            member(2, "Bo"),
            member(3, "Bea"),
            member(4, "Cy"),
        ],
    );
    let (host, cx) = host(cx, state);
    let opener = host.read_with(cx, |h, _| h.opener.clone());
    cx.update(|window, cx| opener.focus(window, cx));
    cx.update(|_, cx| picker::open(Some(community), None, cx));
    cx.run_until_parked();
    let selected =
        |cx: &mut VisualTestContext| cx.update(|_, cx| cx.global::<picker::PickerModel>().selected);
    assert_eq!(
        selected(cx),
        Some(0),
        "the first member (this account is left out)"
    );
    cx.simulate_keystrokes("down");
    assert_eq!(selected(cx), Some(1));
    cx.simulate_keystrokes("up up");
    assert_eq!(selected(cx), Some(3), "Up wraps to the last member");
    cx.simulate_keystrokes("b");
    assert_eq!(selected(cx), Some(1), "`b` goes to the next B: Bo");
    cx.simulate_keystrokes("e");
    assert_eq!(selected(cx), Some(2), "`be` is Bea");
    // Enter without a client opens nothing and keeps the dialog open.
    cx.simulate_keystrokes("enter");
    assert!(cx.update(|_, cx| cx.global::<picker::PickerModel>().open));
    cx.simulate_keystrokes("escape");
    cx.run_until_parked();
    assert!(!cx.update(|_, cx| cx.global::<picker::PickerModel>().open));
    assert!(focused(cx, &opener), "Escape returns focus to the opener");
}

/// S3: the dms fixtures (A at 1280x800, B at 1440x900 with long names, a group DM and a second server) are valid
/// fixture files whose routes name the DMs of their worlds.
#[test]
fn dms_fixtures_are_valid() {
    let dir = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("../parity/fixtures/dms");
    for name in ["home", "home-b", "conversation", "conversation-b"] {
        let text = std::fs::read_to_string(dir.join(format!("{name}.json"))).unwrap();
        let f = dilla_client::testing::spec::ScreenFixture::parse(&text)
            .unwrap_or_else(|e| panic!("{name}: {e}"));
        assert_eq!(f.name, format!("dms/{name}"));
        f.validate().unwrap_or_else(|e| panic!("{name}: {e}"));
        let route = f.resolved_route().unwrap();
        assert!(route.starts_with("/dm"), "{name}: {route}");
        assert!(!f.world.dms.is_empty());
    }
}

/// The conversation's trust surfaces: the composer is paused with the changed-key copy while the core's gate holds,
/// and the removed note shows once a validated Remove restored sending.
#[gpui::test]
fn conversation_block_and_removed_note(cx: &mut TestAppContext) {
    let _ = cx;
    let channel = dilla_client::state::ChannelId([5; 16]);
    let mut changed = contact(1, "Ada");
    changed.changed_umk = Some("cc".repeat(32));
    let mut state = state_with(vec![changed.clone()]);
    state.dms.list = Some(vec![dilla_client::state::dms::DmSummary {
        id: channel,
        kind: dilla_client::state::dms::DM_KIND_PAIR,
        members: vec![UserId([1; 16]), UserId([9; 16])],
        name: "Ada".into(),
        group: dilla_client::state::ChannelGroupState::Active,
    }]);
    state.trust.send_blocks.insert(
        channel,
        dilla_client::state::trust::SendBlock {
            code: dilla_client::state::trust::TrustBlock::UmkChanged,
            own: false,
        },
    );
    assert_eq!(
        conversation::composer_block(&state, channel),
        Some(dilla_desktop::strings::t("privacy.changed"))
    );
    state.trust.send_blocks.clear();
    state.trust.contacts[0].removed_in.push(channel);
    assert_eq!(conversation::composer_block(&state, channel), None);
    assert!(state.trust.removed_here(channel));
}
