//! Task 18's behaviour and keyboard tests for the ring area (standard procedure S5-S6), and the pure parts of the DM
//! call, block and AFK views.
//!
//! The banner runs in a real (test) window and is driven with keystrokes: Enter answers and Escape declines the ring with
//! focus, Enter on Decline declines, Tab walks each ring's Answer then Decline, and a ring that arrives moves focus only
//! while the window is focused (packages/web/src/screens/settings/Keyboard.tsx has no ring row: these are the brief's).
//! The banner's parity fixtures and the full-app behaviour tests wait for the shell's host (Task 8) and the voice stage
//! (Task 17): see Task 18's report.

use std::cell::RefCell;
use std::rc::Rc;
use std::sync::Arc;

use dilla_client::state::ring::{
    AfkNotice, AfkSnapshot, Away, BlockWhen, CallBlock, IncomingRing, RING, RingState,
};
use dilla_client::state::{AppState, ChannelId, CommunityId, UserId};
use dilla_desktop::screens::ring::{
    RingEnv, afk_explainer, afk_moved, afk_title, block_notice, dm_call_start, effects, join_held,
    provide, ring_banner, ring_key, ring_text,
};
use dilla_desktop::screens::{FocusNext, FocusPrevious};
use dilla_desktop::slots::SlotCx;
use dilla_desktop::strings::{t, tf};
use dilla_desktop_ui::components::ring::{OnRing, RingBanner, RingFocus, RingRow};
use gpui::{
    Context, FocusHandle, InteractiveElement, IntoElement, ParentElement, Render, SharedString,
    Styled, TestAppContext, VisualTestContext, Window, div,
};

const DM: ChannelId = ChannelId([0xd4; 16]);
const DM2: ChannelId = ChannelId([0xd5; 16]);
const BOB: UserId = UserId([0xb2; 16]);

fn ring(channel: ChannelId, name: &str, busy: bool) -> IncomingRing {
    IncomingRing {
        call_id: channel.0,
        channel,
        caller: BOB,
        caller_name: name.into(),
        ring_ts: 1_700_000_030,
        expires_at: tokio::time::Instant::now() + RING,
        busy,
        quiet: false,
    }
}

fn install(cx: &mut TestAppContext) {
    cx.update(dilla_desktop::install);
}

fn focused(cx: &mut VisualTestContext, handle: &FocusHandle) -> bool {
    cx.update(|window, _| handle.is_focused(window))
}

/// A window holding the banner component with recording callbacks.
struct BannerHost {
    rows: Vec<RingRow>,
    answered: Rc<RefCell<Vec<SharedString>>>,
    declined: Rc<RefCell<Vec<SharedString>>>,
}

impl Render for BannerHost {
    fn render(&mut self, _window: &mut Window, _cx: &mut Context<Self>) -> impl IntoElement {
        let a = self.answered.clone();
        let d = self.declined.clone();
        let on_answer: OnRing = Rc::new(move |k, _, _| a.borrow_mut().push(k.clone()));
        let on_decline: OnRing = Rc::new(move |k, _, _| d.borrow_mut().push(k.clone()));
        div()
            .id("host")
            .size_full()
            .on_action(|_: &FocusNext, window, cx| window.focus_next(cx))
            .on_action(|_: &FocusPrevious, window, cx| window.focus_prev(cx))
            .child(RingBanner::new(
                "banner",
                self.rows.clone(),
                t("call.ring.answer"),
                t("call.ring.decline"),
                on_answer,
                on_decline,
            ))
    }
}

fn banner(cx: &mut TestAppContext, n: usize) -> (gpui::Entity<BannerHost>, &mut VisualTestContext) {
    install(cx);
    cx.add_window_view(|_, cx| BannerHost {
        rows: (0..n)
            .map(|i| RingRow {
                key: format!("ring{i}").into(),
                name: "Ada".into(),
                text: tf("call.ring.incoming", &[("name", "Ada")]).into(),
                busy: false,
                focus: RingFocus::new(cx),
            })
            .collect(),
        answered: Rc::default(),
        declined: Rc::default(),
    })
}

fn focus_of(host: &gpui::Entity<BannerHost>, cx: &mut VisualTestContext, i: usize) -> RingFocus {
    host.read_with(cx, |h, _| h.rows[i].focus.clone())
}

#[gpui::test]
fn keyboard_ring_focus_order(cx: &mut TestAppContext) {
    let (host, cx) = banner(cx, 2);
    let (f0, f1) = (focus_of(&host, cx, 0), focus_of(&host, cx, 1));
    cx.update(|window, cx| window.focus(&f0.row, cx));
    for (want, name) in [
        (&f0.answer, "ring 1 answer"),
        (&f0.decline, "ring 1 decline"),
        (&f1.answer, "ring 2 answer"),
        (&f1.decline, "ring 2 decline"),
    ] {
        cx.simulate_keystrokes("tab");
        assert!(focused(cx, want), "Tab should land on {name}");
    }
    cx.simulate_keystrokes("shift-tab");
    assert!(
        focused(cx, &f1.answer),
        "Shift+Tab goes back to ring 2 answer"
    );
}

#[gpui::test]
fn enter_answers_the_ring_with_focus(cx: &mut TestAppContext) {
    let (host, cx) = banner(cx, 2);
    let f1 = focus_of(&host, cx, 1);
    cx.update(|window, cx| window.focus(&f1.row, cx));
    cx.simulate_keystrokes("enter");
    let (a, d) = host.read_with(cx, |h, _| {
        (h.answered.borrow().clone(), h.declined.borrow().clone())
    });
    assert_eq!(
        a,
        vec![SharedString::from("ring1")],
        "Enter answered the focused ring once"
    );
    assert!(d.is_empty());
    // On the Answer button too, once (the binding, not a second click).
    cx.update(|window, cx| window.focus(&f1.answer, cx));
    cx.simulate_keystrokes("enter");
    assert_eq!(host.read_with(cx, |h, _| h.answered.borrow().len()), 2);
}

#[gpui::test]
fn escape_declines_the_ring_with_focus(cx: &mut TestAppContext) {
    let (host, cx) = banner(cx, 2);
    let (f0, f1) = (focus_of(&host, cx, 0), focus_of(&host, cx, 1));
    cx.update(|window, cx| window.focus(&f0.row, cx));
    cx.simulate_keystrokes("escape");
    // From a ring's Answer button, Escape declines that ring.
    cx.update(|window, cx| window.focus(&f1.answer, cx));
    cx.simulate_keystrokes("escape");
    let (a, d) = host.read_with(cx, |h, _| {
        (h.answered.borrow().clone(), h.declined.borrow().clone())
    });
    assert!(a.is_empty(), "Escape never answers");
    assert_eq!(
        d,
        vec![SharedString::from("ring0"), SharedString::from("ring1")]
    );
}

#[gpui::test]
fn enter_on_decline_declines(cx: &mut TestAppContext) {
    let (host, cx) = banner(cx, 1);
    let f0 = focus_of(&host, cx, 0);
    cx.update(|window, cx| window.focus(&f0.decline, cx));
    cx.simulate_keystrokes("enter");
    let (a, d) = host.read_with(cx, |h, _| {
        (h.answered.borrow().clone(), h.declined.borrow().clone())
    });
    assert!(a.is_empty(), "Enter on Decline never answers");
    assert_eq!(d, vec![SharedString::from("ring0")]);
}

/// A window whose render draws the area's real hook (`slots::overlays` guest) over a state the test sets.
struct HookHost {
    state: Arc<AppState>,
    other: FocusHandle,
}

impl Render for HookHost {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        provide(
            RingEnv {
                ctx: SlotCx {
                    state: self.state.clone(),
                    ..SlotCx::default()
                },
            },
            window,
            cx,
        );
        div()
            .id("hook-host")
            .size_full()
            .child(div().id("other").track_focus(&self.other).tab_stop(true))
            .children(ring_banner(window, cx))
    }
}

fn with_rings(rings: Vec<IncomingRing>) -> Arc<AppState> {
    Arc::new(AppState {
        ring: RingState {
            rings,
            ..RingState::default()
        },
        ..AppState::default()
    })
}

#[gpui::test]
fn a_ring_moves_focus_only_while_the_window_is_focused(cx: &mut TestAppContext) {
    install(cx);
    let (host, cx) = cx.add_window_view(|_, cx| HookHost {
        state: with_rings(Vec::new()),
        other: cx.focus_handle(),
    });
    let other = host.read_with(cx, |h, _| h.other.clone());
    cx.update(|window, cx| window.focus(&other, cx));
    // The window is not focused: a ring arrives, and focus stays where it was.
    cx.deactivate_window();
    host.update(cx, |h, cx| {
        h.state = with_rings(vec![ring(DM, "Ada", false)]);
        cx.notify();
    });
    cx.run_until_parked();
    assert!(
        focused(cx, &other),
        "a ring never takes focus from an unfocused window"
    );
    // Focused again, a second ring arrives: it takes focus, and Escape declines it (no client in a fixture: nothing is
    // sent, and nothing panics).
    cx.update(|window, _| window.activate_window());
    cx.run_until_parked();
    host.update(cx, |h, cx| {
        h.state = with_rings(vec![ring(DM, "Ada", false), ring(DM2, "Björn", false)]);
        cx.notify();
    });
    cx.run_until_parked();
    assert!(!focused(cx, &other), "the arriving ring took focus");
    cx.simulate_keystrokes("escape");
    // The ring that was already shown does not take focus again on a later frame.
    cx.update(|window, cx| window.focus(&other, cx));
    host.update(cx, |h, cx| {
        h.state = with_rings(vec![ring(DM, "Ada", false), ring(DM2, "Björn", false)]);
        cx.notify();
    });
    cx.run_until_parked();
    assert!(focused(cx, &other), "a ring moves focus once, on arrival");
}

#[gpui::test]
fn the_banner_draws_nothing_without_a_ring_and_every_ring_state_with_one(cx: &mut TestAppContext) {
    install(cx);
    let (host, cx) = cx.add_window_view(|_, cx| HookHost {
        state: with_rings(Vec::new()),
        other: cx.focus_handle(),
    });
    let drawn =
        |cx: &mut VisualTestContext| cx.update(|window, cx| ring_banner(window, cx).is_some());
    assert!(!drawn(cx), "no ring, no banner");
    for rings in [
        vec![ring(DM, "Ada", false)],
        vec![ring(DM, "Ada", true)],
        vec![
            ring(DM, "Ada", false),
            ring(DM2, "a-very-long-name-for-a-caller-that-wraps", true),
        ],
    ] {
        host.update(cx, |h, cx| {
            h.state = with_rings(rings);
            cx.notify();
        });
        cx.run_until_parked();
        assert!(drawn(cx));
    }
}

// ---- the pure parts ----

#[test]
fn the_ring_says_incoming_or_busy_naming_where() {
    let r = ring(DM, "Ada", false);
    assert_eq!(ring_text(&r, None), "Ada is calling");
    assert_eq!(
        ring_text(&r, Some("general")),
        "Ada is calling",
        "a ring that is not busy"
    );
    let b = ring(DM, "Ada", true);
    assert_eq!(
        ring_text(&b, Some("general")),
        "Ada is calling. Answering leaves general."
    );
    assert_eq!(
        ring_key(&b),
        SharedString::from(format!("{}:1700000030", "d4".repeat(16)))
    );
}

#[test]
fn only_an_incoming_unmuted_ring_rings_audibly() {
    let mut r = ring(DM, "Ada", false);
    assert!(effects::audible(&r));
    r.quiet = true;
    assert!(!effects::audible(&r));
    assert!(!effects::audible(&ring(DM, "Ada", true)));
}

fn blocked(block: CallBlock) -> AppState {
    AppState {
        ring: RingState {
            blocked: Some(block),
            ..RingState::default()
        },
        ..AppState::default()
    }
}

#[test]
fn a_block_names_the_changed_contact_and_offers_the_comparison_until_it_clears() {
    let name = |_: UserId| "peerbd2233".to_owned();
    let s = blocked(CallBlock {
        channel: DM,
        code: "E_UMK_CHANGED".into(),
        changed_user: Some(BOB),
        when: BlockWhen::BeforeJoin,
    });
    let n = block_notice(&s, DM, name).unwrap();
    assert_eq!(
        n.text,
        "peerbd2233’s identity changed. Compare safety numbers in Privacy before joining."
    );
    assert_eq!(n.compare, Some(BOB));
    assert!(join_held(&s, DM));
    assert_eq!(
        dm_call_start(&s, DM, false, false),
        None,
        "no call button while the block holds"
    );
    assert_eq!(
        block_notice(&s, DM2, name),
        None,
        "another channel's notice"
    );
    // Unnamed: the join is paused.
    let s = blocked(CallBlock {
        channel: DM,
        code: "E_DEVICE_UNLISTED".into(),
        changed_user: None,
        when: BlockWhen::BeforeJoin,
    });
    assert_eq!(
        block_notice(&s, DM, name).unwrap().text,
        t("voice.blockedJoin")
    );
    // Mid-call: the device left, the join is offered again, and nothing to compare.
    let s = blocked(CallBlock {
        channel: DM,
        code: "E_DEVICE_UNLISTED".into(),
        changed_user: None,
        when: BlockWhen::Left,
    });
    let n = block_notice(&s, DM, name).unwrap();
    assert_eq!((n.text.as_str(), n.compare), (t("voice.blockedLeft"), None));
    assert_eq!(dm_call_start(&s, DM, false, false), Some(t("call.start")));
    assert_eq!(dm_call_start(&s, DM, false, true), Some(t("call.join")));
    assert_eq!(
        dm_call_start(&s, DM, true, true),
        None,
        "in this call already"
    );
}

#[test]
fn the_afk_page_says_who_is_away_and_the_servers_timeout() {
    assert_eq!(afk_title(true, &["Ada".into()]), "You are away");
    assert_eq!(afk_title(false, &[]), "Nobody is away");
    assert_eq!(afk_title(false, &["Ada".into()]), "Ada is away");
    assert_eq!(
        afk_title(false, &["Ada".into(), "Björn".into()]),
        "Ada and Björn are away"
    );
    assert_eq!(
        afk_explainer(300),
        "After 5 minutes idle in voice, a computer is moved here. Phones are never moved."
    );
    assert_eq!(afk_explainer(60), t("afk.explain.one"));
    assert_eq!(afk_explainer(89), t("afk.explain.one"), "Math.round");
    assert_eq!(afk_explainer(0), t("afk.explain.off"));
}

#[test]
fn an_afk_move_is_said_once_with_its_reason() {
    let afk = ChannelId([7; 16]);
    let c = CommunityId([5; 16]);
    let mut s = AppState::default();
    assert_eq!(afk_moved(&s, |_| "Idle".into()), None);
    s.ring.afk.insert(
        c,
        AfkSnapshot {
            channel: Some(afk),
            seq: 1,
            pending: true,
            pending_reason: 1,
            away: vec![Away {
                user: BOB,
                since: 1,
            }],
            ..AfkSnapshot::default()
        },
    );
    s.ring.afk_notice = Some(AfkNotice {
        community: c,
        channel: Some(afk),
        reason: 1,
        seq: 1,
    });
    assert_eq!(
        afk_moved(&s, |_| "Idle".into()).as_deref(),
        Some("Moved to Idle for inactivity")
    );
    s.ring.afk_notice = Some(AfkNotice {
        community: c,
        channel: Some(afk),
        reason: 2,
        seq: 2,
    });
    assert_eq!(
        afk_moved(&s, |_| "Idle".into()).as_deref(),
        Some(t("afk.elsewhere"))
    );
    s.ring.afk_notice = Some(AfkNotice {
        community: c,
        channel: Some(afk),
        reason: 3,
        seq: 3,
    });
    assert_eq!(
        afk_moved(&s, |_| "Idle".into()).as_deref(),
        Some(t("afk.takenOver"))
    );
    assert!(s.ring.is_away(c, BOB));
}
