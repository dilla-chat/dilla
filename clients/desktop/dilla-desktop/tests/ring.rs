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

use dilla_client::seams::InstanceId;
use dilla_client::state::ring::{
    AfkNotice, AfkSnapshot, Away, BlockWhen, CallBlock, IncomingRing, RING, RingState,
};
use dilla_client::state::{AppState, ChannelId, CommunityId, UserId};
use dilla_desktop::native::{notifier, tray};
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

// ---- integration 3, step 0 item 2: the voice stage and the DM header read the ring area's state ----

const SERVER: CommunityId = CommunityId([0x51; 16]);
const GENERAL: ChannelId = ChannelId([0x61; 16]);
const LOUNGE: ChannelId = ChannelId([0x62; 16]);
const IDLE: ChannelId = ChannelId([0x63; 16]);

fn chan(id: ChannelId, kind: u8, name: &str, position: u32) -> dilla_client::state::Channel {
    dilla_client::state::Channel {
        id,
        community_id: SERVER,
        kind,
        mode: 0,
        name: name.into(),
        topic: String::new(),
        parent_id: None,
        position,
        slowmode: 0,
        text_group_id: None,
        group: dilla_client::state::ChannelGroupState::Active,
    }
}

/// A server with #general, the voice channel Lounge and the AFK channel Idle, and a DM with Bob.
fn server() -> AppState {
    let mut s = AppState::default();
    s.communities.push(dilla_client::state::Community {
        id: SERVER,
        name: "Midgard".into(),
        owner: UserId([9; 16]),
        policy_version: 0,
    });
    s.channels = vec![
        chan(GENERAL, 0, "general", 0),
        chan(LOUNGE, 1, "Lounge", 1),
        chan(IDLE, 1, "Idle", 2),
    ];
    s.dms.list = Some(vec![dilla_client::state::dms::DmSummary {
        id: DM,
        kind: dilla_client::state::dms::DM_KIND_PAIR,
        members: vec![BOB],
        name: "Bob".into(),
        group: dilla_client::state::ChannelGroupState::Active,
    }]);
    s.ring.afk.insert(
        SERVER,
        AfkSnapshot {
            channel: Some(IDLE),
            timeout_seconds: 300,
            ..AfkSnapshot::default()
        },
    );
    s
}

/// The shell on `state`, showing `route`, drawn.
fn shell_on(
    state: AppState,
    route: dilla_desktop::router::Route,
    cx: &mut TestAppContext,
) -> (
    gpui::Entity<dilla_desktop::screens::shell::ShellView>,
    &mut VisualTestContext,
) {
    install(cx);
    let (shell, vcx) = cx.add_window_view(move |window, cx| {
        dilla_desktop::screens::shell::ShellView::new(
            Arc::new(state),
            None,
            None,
            dilla_desktop::screens::shell::Clock::Fixed { now: 0, utc: true },
            window,
            cx,
        )
    });
    shell.update(vcx, |s, cx| s.navigate(route, cx));
    vcx.run_until_parked();
    vcx.update(|window, _| window.refresh());
    vcx.run_until_parked();
    (shell, vcx)
}

fn lounge() -> dilla_desktop::router::Route {
    dilla_desktop::router::Route::Channel {
        community: SERVER,
        channel: Some(LOUNGE),
    }
}

/// VoiceView.tsx `Stage`: a block that holds the join says so on the stage, and the stage offers no join; without one
/// the join is there and no notice.
#[gpui::test]
fn the_stage_says_a_held_block_and_offers_no_join(cx: &mut TestAppContext) {
    let mut s = server();
    s.ring.blocked = Some(CallBlock {
        channel: LOUNGE,
        code: "E_DEVICE_UNLISTED".into(),
        changed_user: None,
        when: BlockWhen::BeforeJoin,
    });
    let (_, vcx) = shell_on(s, lounge(), cx);
    assert!(vcx.debug_bounds("voice-stage-root").is_some(), "the stage");
    assert!(
        vcx.debug_bounds("voice-blocked").is_some(),
        "the block's notice"
    );
    assert!(
        vcx.debug_bounds("voice-join").is_none(),
        "no join while held"
    );
}

#[gpui::test]
fn the_stage_without_a_block_offers_the_join(cx: &mut TestAppContext) {
    let (_, vcx) = shell_on(server(), lounge(), cx);
    assert!(vcx.debug_bounds("voice-join").is_some());
    assert!(vcx.debug_bounds("voice-blocked").is_none());
    assert!(
        vcx.debug_bounds("voice-afk-page").is_none(),
        "Lounge is no AFK channel"
    );
}

/// A changed contact's block offers the comparison.
#[gpui::test]
fn a_named_block_on_the_stage_offers_the_comparison(cx: &mut TestAppContext) {
    let mut s = server();
    s.ring.blocked = Some(CallBlock {
        channel: LOUNGE,
        code: "E_UMK_CHANGED".into(),
        changed_user: Some(BOB),
        when: BlockWhen::BeforeJoin,
    });
    s.trust
        .contacts
        .push(dilla_client::state::trust::TrustSummary {
            user_id: BOB,
            name: "Bob".into(),
            shared: true,
            umk: "a".repeat(64),
            first_seen: 1,
            verified: false,
            changed_umk: Some("b".repeat(64)),
            unlisted: false,
            removed_in: Vec::new(),
            blocked: None,
        });
    let (_, vcx) = shell_on(s, lounge(), cx);
    assert!(vcx.debug_bounds("voice-blocked").is_some());
    assert!(vcx.debug_bounds("voice-notice-compare").is_some());
}

/// VoiceView.tsx `VoiceView`: the server's AFK channel shows the AFK page in place of the stage, and Come back opens the
/// text channel last opened there.
#[gpui::test]
fn the_afk_channel_shows_the_afk_page_and_come_back_returns(cx: &mut TestAppContext) {
    let (shell, vcx) = shell_on(
        server(),
        dilla_desktop::router::Route::Channel {
            community: SERVER,
            channel: Some(GENERAL),
        },
        cx,
    );
    shell.update(vcx, |s, cx| {
        s.navigate(
            dilla_desktop::router::Route::Channel {
                community: SERVER,
                channel: Some(IDLE),
            },
            cx,
        )
    });
    vcx.run_until_parked();
    vcx.update(|window, _| window.refresh());
    vcx.run_until_parked();
    assert!(vcx.debug_bounds("voice-afk-page").is_some(), "the AFK page");
    assert!(vcx.debug_bounds("voice-join").is_none(), "no call controls");
    shell.update(vcx, |s, cx| s.leave_afk(cx));
    assert_eq!(
        shell.read_with(vcx, |s, _| s.route().clone()),
        dilla_desktop::router::Route::Channel {
            community: SERVER,
            channel: Some(GENERAL),
        }
    );
}

/// R-AFK4: the move's line shows on a voice channel of that server.
#[gpui::test]
fn an_afk_move_is_said_on_the_stage(cx: &mut TestAppContext) {
    let mut s = server();
    s.ring.afk_notice = Some(AfkNotice {
        community: SERVER,
        channel: Some(IDLE),
        reason: 1,
        seq: 1,
    });
    let (_, vcx) = shell_on(s, lounge(), cx);
    assert!(vcx.debug_bounds("voice-afk-moved").is_some());
}

/// The DM row's menu: a right-click on the list's row of `DM` (Home lists the DMs), as ChannelList.tsx's `onContextMenu`.
fn open_dm_row_menu(vcx: &mut VisualTestContext) {
    let row = vcx
        .debug_bounds("dm-row-0")
        .expect("the DM list's row of the DM");
    vcx.simulate_event(gpui::MouseDownEvent {
        button: gpui::MouseButton::Right,
        position: row.center(),
        modifiers: gpui::Modifiers::none(),
        click_count: 1,
        first_mouse: false,
    });
    vcx.run_until_parked();
    vcx.update(|window, _| window.refresh());
    vcx.run_until_parked();
}

/// Ruling Z (Shell.tsx: a DM's call is in the DM row's context menu, VoiceView.tsx `dmCallStart`): the DM row's menu
/// starts the call until a block holds it, and the DM header has no call button (a DM header is name and topic only).
#[gpui::test]
fn the_dm_rows_menu_has_its_call_until_a_block_holds_it(cx: &mut TestAppContext) {
    let (_, vcx) = shell_on(server(), dilla_desktop::router::Route::Dm(DM), cx);
    assert!(
        vcx.debug_bounds("header-dm-call").is_none(),
        "no header button"
    );
    open_dm_row_menu(vcx);
    assert!(vcx.debug_bounds("menu-item-call").is_some(), "Start call");
    assert!(vcx.debug_bounds("menu-item-pins").is_some(), "the pins");
    assert!(vcx.debug_bounds("voice-blocked").is_none());
}

#[gpui::test]
fn a_held_dm_block_takes_the_call_from_the_row_menu_and_says_why(cx: &mut TestAppContext) {
    let mut s = server();
    s.ring.blocked = Some(CallBlock {
        channel: DM,
        code: "E_DEVICE_UNLISTED".into(),
        changed_user: None,
        when: BlockWhen::BeforeJoin,
    });
    let (_, vcx) = shell_on(s, dilla_desktop::router::Route::Dm(DM), cx);
    assert!(vcx.debug_bounds("voice-blocked").is_some());
    open_dm_row_menu(vcx);
    assert!(vcx.debug_bounds("menu-item-call").is_none());
    assert!(vcx.debug_bounds("menu-item-pins").is_some());
}

/// Shell.tsx's DM row menu: Start call on a DM that is not the open one opens it too (with no client the join itself
/// does nothing here).
#[gpui::test]
fn the_row_menus_call_opens_its_dm(cx: &mut TestAppContext) {
    let (shell, vcx) = shell_on(server(), dilla_desktop::router::Route::Dms, cx);
    open_dm_row_menu(vcx);
    let call = vcx.debug_bounds("menu-item-call").expect("Start call");
    vcx.simulate_click(call.center(), gpui::Modifiers::none());
    vcx.run_until_parked();
    assert_eq!(
        shell.read_with(vcx, |s, _| s.route().clone()),
        dilla_desktop::router::Route::Dm(DM)
    );
}

/// Ring.tsx `onAnswer` (Shell.tsx: "opens the DM and joins its call"): answering a ring opens its DM.
#[gpui::test]
fn answering_a_ring_opens_its_dm(cx: &mut TestAppContext) {
    let (shell, vcx) = shell_on(
        server(),
        dilla_desktop::router::Route::Channel {
            community: SERVER,
            channel: Some(GENERAL),
        },
        cx,
    );
    vcx.update(|window, _| window.activate_window());
    vcx.run_until_parked();
    // The ring arrives while the window is focused: it takes the focus, and Enter answers it.
    let mut s = server();
    s.ring.rings.push(ring(DM, "Bob", false));
    shell.update(vcx, |shell, cx| shell.set_state(Arc::new(s), cx));
    vcx.run_until_parked();
    vcx.update(|window, _| window.refresh());
    vcx.run_until_parked();
    vcx.simulate_keystrokes("enter");
    vcx.run_until_parked();
    assert_eq!(
        shell.read_with(vcx, |s, _| s.route().clone()),
        dilla_desktop::router::Route::Dm(DM)
    );
}

/// Review M2, now per instance (GPUI gaps Task 3): one ring sound for the app. It starts with the first instance that
/// rings audibly and stops with the last; one instance's ring ending (or its client released) never silences another's.
#[gpui::test]
fn the_ring_sound_follows_the_instances_that_ring(cx: &mut TestAppContext) {
    install(cx);
    let a = instance(cx, 1);
    let b = instance(cx, 2);
    for tx in [&a, &b] {
        tx.send_replace(with_rings(vec![ring(DM, "Bob", false)]));
    }
    cx.run_until_parked();
    assert!(cx.update(effects::sound_on));
    a.send_replace(with_rings(Vec::new()));
    cx.run_until_parked();
    assert!(
        cx.update(effects::sound_on),
        "the other instance still rings"
    );
    cx.update(|cx| effects::unwatch_client(client_key(2), cx));
    assert!(
        !cx.update(effects::sound_on),
        "the last ringing instance went: silent"
    );
    // A busy ring (this device is in a call) rings without a sound.
    a.send_replace(with_rings(vec![ring(DM, "Bob", true)]));
    cx.run_until_parked();
    assert!(!cx.update(effects::sound_on));
}

/// Jonas's decision for Task 18: a ring that arrives takes the keyboard focus (Enter answers, Escape declines), and the
/// focus goes back to where it was once the ring ends or is dismissed.
#[gpui::test]
fn the_ring_takes_the_focus_and_gives_it_back(cx: &mut TestAppContext) {
    let (shell, vcx) = shell_on(
        server(),
        dilla_desktop::router::Route::Channel {
            community: SERVER,
            channel: Some(GENERAL),
        },
        cx,
    );
    vcx.update(|window, _| window.activate_window());
    // The person is somewhere in the shell: its own focus.
    let before = shell.read_with(vcx, gpui::Focusable::focus_handle);
    vcx.update(|window, cx| before.focus(window, cx));
    vcx.run_until_parked();
    let mut ringing = server();
    ringing.ring.rings.push(ring(DM, "Bob", false));
    shell.update(vcx, |s, cx| s.set_state(Arc::new(ringing), cx));
    vcx.run_until_parked();
    vcx.update(|window, _| window.refresh());
    vcx.run_until_parked();
    assert!(
        !vcx.update(|window, _| before.is_focused(window)),
        "the ring took the focus"
    );
    // The ring ends (the caller gave up): the focus is back where it was.
    shell.update(vcx, |s, cx| s.set_state(Arc::new(server()), cx));
    vcx.run_until_parked();
    vcx.update(|window, _| window.refresh());
    vcx.run_until_parked();
    assert!(
        vcx.update(|window, _| before.is_focused(window)),
        "the focus came back"
    );
}

// ---- the OS ring notification (GPUI parity Package 4: beta.5 `notify-ring`; GPUI gaps Task 3: with no window) ----

/// The test's stand-in for a running client's key (`Rc::as_ptr`).
fn client_key(id: u8) -> usize {
    1000 + usize::from(id)
}

/// A running instance whose client state the test sets: the app-level ring observer watches it, window or not
/// (`windows::keep_client` starts the same observer on a real client's state).
fn instance(cx: &mut TestAppContext, id: u8) -> tokio::sync::watch::Sender<Arc<AppState>> {
    let (tx, rx) = tokio::sync::watch::channel(Arc::new(AppState::default()));
    cx.update(|cx| effects::watch_instance(InstanceId(id), client_key(id), rx, cx));
    cx.run_until_parked();
    tx
}

fn os_installed(cx: &mut TestAppContext) {
    install(cx);
    cx.update(notifier::install);
}

fn answer(tag: SharedString) -> gpui::SystemNotificationResponse {
    gpui::SystemNotificationResponse {
        tag,
        action_id: Some("answer".into()),
    }
}

/// Electron parity: the app closed to the tray (no window at all) still rings. The ring sound starts and one OS
/// notification is raised with Answer and Decline (not again on a later state); the tray lists it with the same rows.
#[gpui::test]
fn a_ring_with_no_window_open_sounds_and_notifies(cx: &mut TestAppContext) {
    os_installed(cx);
    let tx = instance(cx, 1);
    assert!(cx.windows().is_empty(), "no window is open");
    tx.send_replace(with_rings(vec![ring(DM, "Ada", false)]));
    cx.run_until_parked();
    // A later state with the same ring raises nothing more.
    tx.send_replace(with_rings(vec![ring(DM, "Ada", false)]));
    cx.run_until_parked();
    assert!(cx.update(effects::sound_on), "the ring sound plays");
    let shown = cx.shown_system_notifications();
    assert_eq!(shown.len(), 1, "one notification, not one per state");
    assert_eq!(shown[0].title.as_ref(), "Ada is calling");
    assert!(
        shown[0].tag.starts_with("dilla:1:"),
        "the instance owns it: {}",
        shown[0].tag
    );
    let actions: Vec<(&str, &str)> = shown[0]
        .actions
        .iter()
        .map(|a| (a.id.as_ref(), a.label.as_ref()))
        .collect();
    assert_eq!(actions, [("answer", "Answer"), ("decline", "Decline")]);
    let tray = cx.update(|cx| dilla_desktop::native::NativeShell::tray_menu(cx));
    assert_eq!(tray.len(), 1);
    assert_eq!(
        tray[0].label().map(|l| l.to_string()).as_deref(),
        Some("Ada is calling")
    );
}

/// The ring ends (the caller gave up, another device answered): its notification is withdrawn and the sound stops.
#[gpui::test]
fn the_ring_notification_is_withdrawn_when_the_ring_ends(cx: &mut TestAppContext) {
    os_installed(cx);
    let tx = instance(cx, 1);
    tx.send_replace(with_rings(vec![ring(DM, "Ada", false)]));
    cx.run_until_parked();
    let live = cx.update(|cx| notifier::live_tags(cx));
    assert_eq!(live.len(), 1);
    tx.send_replace(with_rings(Vec::new()));
    cx.run_until_parked();
    assert!(cx.update(|cx| notifier::live_tags(cx)).is_empty());
    assert_eq!(cx.dismissed_system_notifications(), [live[0].clone()]);
    assert!(!cx.update(effects::sound_on), "the sound stopped");
}

/// `ring.spec.ts` "answer: bob rings (banner, sound, one notification while hidden)…", its OS part, as before: while
/// a window of the instance is focused its ring raises no notification (the banner shows it); unfocused, one is raised;
/// focused again, it is withdrawn.
#[gpui::test]
fn a_focused_window_raises_no_ring_notification(cx: &mut TestAppContext) {
    os_installed(cx);
    let tx = instance(cx, 1);
    let ringing = with_rings(vec![ring(DM, "Ada", false)]);
    let (host, vcx) = cx.add_window_view(|_, cx| HookHost {
        state: with_rings(Vec::new()),
        other: cx.focus_handle(),
    });
    vcx.update(|window, cx| {
        let id = window.window_handle().window_id();
        dilla_desktop::native::NativeShell::window_opened(id, Some(InstanceId(1)), cx);
        window.activate_window();
    });
    vcx.run_until_parked();
    host.update(vcx, |h, cx| {
        h.state = ringing.clone();
        cx.notify();
    });
    tx.send_replace(ringing);
    vcx.run_until_parked();
    assert!(
        vcx.shown_system_notifications().is_empty(),
        "a focused window shows the banner, no notification"
    );
    assert!(vcx.update(|_, cx| effects::sound_on(cx)), "it still rings");
    vcx.deactivate_window();
    vcx.run_until_parked();
    assert_eq!(vcx.shown_system_notifications().len(), 1, "unfocused: one");
    vcx.update(|window, _| window.activate_window());
    vcx.run_until_parked();
    assert!(vcx.update(|_, cx| notifier::live_tags(cx).is_empty()));
}

/// Closing the instance's focused window while it rings: the ring now notifies (no window of it is focused any more),
/// and closing the window never withdraws a ring's notification.
#[gpui::test]
fn closing_the_ringing_window_raises_the_ring_notification(cx: &mut TestAppContext) {
    os_installed(cx);
    let tx = instance(cx, 1);
    let vcx = cx.add_empty_window();
    let w = vcx.update(|window, cx| {
        let id = window.window_handle().window_id();
        dilla_desktop::native::NativeShell::window_opened(id, Some(InstanceId(1)), cx);
        window.activate_window();
        id
    });
    vcx.run_until_parked();
    // The window is focused when the ring comes: nothing is raised (the observer reads the window's focus itself).
    tx.send_replace(with_rings(vec![ring(DM, "Ada", false)]));
    vcx.run_until_parked();
    assert!(vcx.shown_system_notifications().is_empty());
    vcx.update(|_, cx| dilla_desktop::native::NativeShell::window_closed(w, cx));
    vcx.run_until_parked();
    assert_eq!(vcx.shown_system_notifications().len(), 1);
    assert_eq!(vcx.update(|_, cx| notifier::live_tags(cx)).len(), 1);
}

/// Answer on the notification of a ring that rang with no window: the tray's `show_instance` opens the instance's window
/// (here through the test's opener: a test has no host), and once it draws, the answer it waited for opens the ring's
/// DM. Joining the call is `answer`'s next step, through `Voice::join_dm`, which needs a client: a fixture's shell has
/// none, so the DM is what this checks.
#[gpui::test]
fn answering_from_the_notification_opens_the_window_on_the_rings_dm(cx: &mut TestAppContext) {
    os_installed(cx);
    let tx = instance(cx, 1);
    let mut ringing = server();
    ringing.ring.rings.push(ring(DM, "Bob", false));
    tx.send_replace(Arc::new(ringing.clone()));
    cx.run_until_parked();
    let live = cx.update(|cx| notifier::live_tags(cx));
    assert_eq!(live.len(), 1, "the ring raised its notification");
    // The instance's window opens on the client's state (where it was last: its start route).
    use gpui::AppContext as _;
    let opened: Rc<RefCell<Option<gpui::WindowHandle<dilla_desktop::screens::shell::ShellView>>>> =
        Rc::default();
    let into = opened.clone();
    cx.update(|cx| {
        cx.set_global(tray::OpenInstanceHook(Rc::new(move |id, cx| {
            let state = Arc::new(ringing.clone());
            let handle = cx
                .open_window(gpui::WindowOptions::default(), move |window, cx| {
                    let wid = window.window_handle().window_id();
                    dilla_desktop::native::NativeShell::window_opened(wid, Some(id), cx);
                    cx.new(|cx| {
                        dilla_desktop::screens::shell::ShellView::new(
                            state,
                            None,
                            None,
                            dilla_desktop::screens::shell::Clock::Fixed { now: 0, utc: true },
                            window,
                            cx,
                        )
                    })
                })
                .expect("the window opens");
            *into.borrow_mut() = Some(handle);
            true
        })));
    });
    // The window may draw (and answer) while it opens, inside the response: the answer waits from before it opens.
    cx.update(|cx| notifier::respond(answer(live[0].clone()), cx));
    assert!(cx.update(|cx| notifier::live_tags(cx)).is_empty());
    let handle = opened.borrow().expect("show_instance opened the window");
    let vcx = &mut VisualTestContext::from_window(handle.into(), cx);
    vcx.run_until_parked();
    vcx.update(|window, _| window.refresh());
    vcx.run_until_parked();
    assert_eq!(
        handle
            .read_with(vcx, |s, _| s.route().clone())
            .expect("the window is open"),
        dilla_desktop::router::Route::Dm(DM),
        "the window answered: it shows the ring's DM"
    );
    assert!(vcx.update(|window, _| window.is_window_active()));
    assert!(!vcx.update(|_, cx| effects::answer_pending(InstanceId(1), cx)));
}

/// Review of Task 3, minor 2: Answer with no window, and the window does not open (here: no host to open it, as while
/// the app quits or when the instance left the list). The answer does not wait on: a window opened later for another
/// reason never answers a call by itself.
#[gpui::test]
fn a_failed_window_open_clears_the_pending_answer(cx: &mut TestAppContext) {
    os_installed(cx);
    let tx = instance(cx, 1);
    tx.send_replace(with_rings(vec![ring(DM, "Bob", false)]));
    cx.run_until_parked();
    let live = cx.update(|cx| notifier::live_tags(cx));
    assert_eq!(live.len(), 1, "the ring raised its notification");
    assert!(cx.windows().is_empty());
    cx.update(|cx| notifier::respond(answer(live[0].clone()), cx));
    cx.run_until_parked();
    assert!(cx.windows().is_empty(), "no window opened");
    assert!(
        !cx.update(|cx| effects::answer_pending(InstanceId(1), cx)),
        "no answer waits for a window that never opened"
    );
    assert!(
        !cx.update(|cx| tray::show_instance(InstanceId(1), cx)),
        "show_instance says it opened nothing"
    );
}

/// Review of Task 3, minor 1: a client whose state sender drops (it stopped without `unwatch_client`) leaves no observer
/// entry behind. A later client at the same address (`Rc::as_ptr` is reused) is observed: its ring sounds.
#[gpui::test]
fn a_dropped_state_sender_removes_its_watch(cx: &mut TestAppContext) {
    os_installed(cx);
    let tx = instance(cx, 1);
    tx.send_replace(with_rings(vec![ring(DM, "Bob", false)]));
    cx.run_until_parked();
    assert!(cx.update(effects::sound_on));
    drop(tx);
    cx.run_until_parked();
    assert!(!cx.update(effects::sound_on), "the gone client's ring went");
    // The same key again: a new client allocated where the old one was.
    let again = instance(cx, 1);
    again.send_replace(with_rings(vec![ring(DM2, "Ada", false)]));
    cx.run_until_parked();
    assert!(
        cx.update(effects::sound_on),
        "the new client at the old address is observed"
    );
}

/// beta.5 `current()`: Answer from the notification acts only on the ring that is still current. A ring that ended
/// withdrew its notification, and a late Answer for it does nothing; Answer on a live ring opens its DM.
#[gpui::test]
fn an_answer_from_the_notification_acts_only_on_the_current_ring(cx: &mut TestAppContext) {
    let general = dilla_desktop::router::Route::Channel {
        community: SERVER,
        channel: Some(GENERAL),
    };
    let (shell, vcx) = shell_on(server(), general.clone(), cx);
    vcx.update(|window, cx| {
        notifier::install(cx);
        let id = window.window_handle().window_id();
        dilla_desktop::native::NativeShell::window_opened(id, Some(InstanceId(1)), cx);
    });
    let (tx, rx) = tokio::sync::watch::channel(Arc::new(server()));
    vcx.update(|_, cx| effects::watch_instance(InstanceId(1), client_key(1), rx, cx));
    vcx.deactivate_window();
    vcx.run_until_parked();
    let ringing = |vcx: &mut VisualTestContext| {
        let mut s = server();
        s.ring.rings.push(ring(DM, "Bob", false));
        let s = Arc::new(s);
        shell.update(vcx, |shell, cx| shell.set_state(s.clone(), cx));
        tx.send_replace(s);
        vcx.run_until_parked();
        vcx.update(|window, _| window.refresh());
        vcx.run_until_parked();
        let live = vcx.update(|_, cx| notifier::live_tags(cx));
        assert_eq!(live.len(), 1, "the ring raised its notification");
        live[0].clone()
    };
    let stale = ringing(vcx);
    // The ring ends: its notification goes, and a late Answer does nothing.
    shell.update(vcx, |shell, cx| shell.set_state(Arc::new(server()), cx));
    tx.send_replace(Arc::new(server()));
    vcx.run_until_parked();
    vcx.update(|window, _| window.refresh());
    vcx.run_until_parked();
    assert!(vcx.update(|_, cx| notifier::live_tags(cx).is_empty()));
    vcx.cx
        .update(|cx| notifier::respond(answer(stale.clone()), cx));
    vcx.run_until_parked();
    assert_eq!(shell.read_with(vcx, |s, _| s.route().clone()), general);
    // A decline of a gone ring sends nothing either (no client here: nothing could be sent, and nothing panics).
    vcx.cx.update(|cx| {
        notifier::respond(
            gpui::SystemNotificationResponse {
                tag: stale,
                action_id: Some("decline".into()),
            },
            cx,
        )
    });
    // A live ring: Answer opens its DM and brings the window forward.
    let live = ringing(vcx);
    vcx.cx.update(|cx| notifier::respond(answer(live), cx));
    vcx.run_until_parked();
    assert_eq!(
        shell.read_with(vcx, |s, _| s.route().clone()),
        dilla_desktop::router::Route::Dm(DM)
    );
    assert!(vcx.update(|window, _| window.is_window_active()));
}

// ---- parity (S4, lane G): the app's frame of each fixture against the web client's (parity/references/linux/ring/) ----

fn parity_fixture(name: &str) -> std::path::PathBuf {
    std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../parity/fixtures/ring")
        .join(format!("{name}.json"))
}

macro_rules! parity {
    ($($test:ident => $name:literal $(; $why:literal)?),* $(,)?) => {$(
        #[test]
        #[cfg_attr(not(any(target_os = "linux", target_os = "macos")), ignore = "references are Linux and macOS only")]
        $(#[cfg_attr(any(target_os = "linux", target_os = "macos"), ignore = $why)])?  // elsewhere the line above already ignores it
        fn $test() {
            let report = dilla_parity::screen::assert_screen(&parity_fixture($name))
                .unwrap_or_else(|e| panic!("ring/{} differs from its web reference: {e}", $name));
            println!("SCREEN ring/{}: {} text lines, chrome {:.4} %", $name, report.text_lines, report.chrome_pct);
        }
    )*};
}

parity! {
    parity_ring_banner => "banner"; "parity after T6c: chrome 0.3675 % (R22, lane G: the web's new-device note under the empty channel, 'Messages sent before this browser joined are not shown', which the fixture's device has no counterpart of)",
    parity_ring_afk => "afk",
}
