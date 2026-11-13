//! Task 23's parity, behaviour and keyboard tests for the search area (standard procedure S4-S6).
//!
//! Parity: `parity_search_<state>[_b]` compare the palette (Ctrl+K, then the query typed) with the web client's frame. The
//! palette is drawn offline from the fixture's state: its message hits come from the messages that state holds, matched
//! the way the core matches them.
//!
//! Behaviour and keyboard, in a real (test) window: the arrows, Enter, Escape and Tab with the focus on the textbox, and
//! what choosing a result asks the host to open. The query never leaves the device: the live search asks only the core.
//!
//! Run: `cargo test -p dilla-desktop --features automation --test search -- --include-ignored`.
#![cfg(feature = "automation")]

use std::cell::RefCell;
use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::rc::Rc;
use std::sync::Arc;

use dilla_client::state::{
    AppState, Channel, ChannelGroupState, ChannelId, Community, CommunityId, Timeline,
    TimelineItem, TimelineItemState, UserId,
};
use dilla_desktop::router::UserSection;
use dilla_desktop::screens::search::{self, Navigate, SearchEnv};
use dilla_desktop::screens::shell::Clock;
use dilla_desktop::screens::{FocusNext, FocusPrevious};
use dilla_desktop::slots::{self, SlotCx};
use dilla_desktop::strings::t;
use dilla_parity::Driver;
use dilla_parity::screen::assert_screen;
use gpui::{
    Context, Entity, FocusHandle, InteractiveElement, IntoElement, ParentElement, Render, Styled,
    TestAppContext, VisualTestContext, Window, div, px,
};

// ---- Parity (S4) ---------------------------------------------------------------------------------------------------

fn fixture_path(name: &str) -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../parity/fixtures/search")
        .join(format!("{name}.json"))
}

fn parity(name: &str) {
    let report = assert_screen(&fixture_path(name))
        .unwrap_or_else(|e| panic!("search/{name} differs from its web reference: {e}"));
    println!(
        "SCREEN search/{name}: {} text lines, chrome {:.4} %",
        report.text_lines, report.chrome_pct
    );
}

#[test]
#[cfg_attr(not(target_os = "linux"), ignore = "references are Linux only")]
#[ignore = "parity after T6c: chrome 0.2021 % (on Task 8's shell)"]
fn parity_search_palette() {
    parity("palette");
}

#[test]
#[cfg_attr(not(target_os = "linux"), ignore = "references are Linux only")]
#[ignore = "parity after T6c: 1.0264 % pixels (members pane open by default since Task 14; the reference closed it: re-shoot)"]
fn parity_search_palette_b() {
    parity("palette-b");
}

#[test]
#[cfg_attr(not(target_os = "linux"), ignore = "references are Linux only")]
#[ignore = "parity after T6c: chrome 0.2059 % (on Task 8's shell)"]
fn parity_search_results() {
    parity("results");
}

#[test]
#[cfg_attr(not(target_os = "linux"), ignore = "references are Linux only")]
#[ignore = "parity after T6c: 1.0350 % pixels (members pane open by default since Task 14; the reference closed it: re-shoot)"]
fn parity_search_results_b() {
    parity("results-b");
}

#[test]
#[cfg_attr(not(target_os = "linux"), ignore = "references are Linux only")]
#[ignore = "parity after T6c: chrome 0.2519 % (on Task 8's shell)"]
fn parity_search_scope_settings() {
    parity("scope-settings");
}

#[test]
#[cfg_attr(not(target_os = "linux"), ignore = "references are Linux only")]
#[ignore = "parity after T6c: 0.8446 % pixels (members pane open by default since Task 14; the reference closed it: re-shoot)"]
fn parity_search_scope_settings_b() {
    parity("scope-settings-b");
}

// ---- A host window for the palette --------------------------------------------------------------------------------

fn id(n: u8) -> [u8; 16] {
    [n; 16]
}

fn item(body: &str, seq: u64) -> TimelineItem {
    TimelineItem {
        key: format!("s{seq}"),
        state: TimelineItemState::Ok,
        reason: String::new(),
        sender_user: Some(UserId(id(9))),
        sender_device: None,
        own: false,
        bot: false,
        ts: 1_794_481_730,
        body: body.into(),
        msg_id: None,
        seq: Some(seq),
        edited: false,
        reply: None,
        reactions: Vec::new(),
        pinned: false,
        attachments: Vec::new(),
        mention: false,
    }
}

fn channel(n: u8, name: &str, kind: u8) -> Channel {
    Channel {
        id: ChannelId(id(n)),
        community_id: CommunityId(id(1)),
        kind,
        mode: 0,
        name: name.into(),
        topic: String::new(),
        parent_id: None,
        position: u32::from(n),
        slowmode: 0,
        text_group_id: None,
        group: ChannelGroupState::Active,
    }
}

/// A server with two text channels and a voice one, and a few messages.
fn state() -> Arc<AppState> {
    let timeline = |n: u8, bodies: &[&str]| {
        (
            ChannelId(id(n)),
            Timeline {
                channel_id: ChannelId(id(n)),
                group: ChannelGroupState::Active,
                items: bodies
                    .iter()
                    .enumerate()
                    .map(|(i, b)| item(b, i as u64 + 1))
                    .collect(),
                has_earlier: false,
            },
        )
    };
    Arc::new(AppState {
        communities: vec![Community {
            id: CommunityId(id(1)),
            name: "Midgard Crew".into(),
            owner: UserId(id(9)),
            policy_version: 0,
        }],
        channels: vec![
            channel(2, "general", 0),
            channel(3, "loot", 0),
            channel(4, "harbour", 1),
        ],
        timelines: HashMap::from([
            timeline(
                2,
                &[
                    "bring a rope this time",
                    "two ropes then",
                    "the café compass",
                ],
            ),
            timeline(3, &["the rope is in the loot pile"]),
        ]),
        ..AppState::default()
    })
}

/// A window that draws the palette over a control, the way the shell does, and records what was opened.
struct Host {
    env: SearchEnv,
    opener: FocusHandle,
}

type Opened = Rc<RefCell<Vec<Navigate>>>;

impl Host {
    fn new(state: Arc<AppState>, opened: Opened, answer: bool, cx: &mut Context<Self>) -> Self {
        let ctx = SlotCx {
            state,
            client: None,
            community: Some(CommunityId(id(1))),
            channel: Some(ChannelId(id(2))),
            clock: Clock::Fixed {
                now: 1_794_481_738,
                utc: true,
            },
        };
        Self {
            env: SearchEnv {
                ctx,
                navigate: Rc::new(move |target, _, _| {
                    opened.borrow_mut().push(target);
                    answer
                }),
            },
            opener: cx.focus_handle().tab_stop(true),
        }
    }
}

impl Render for Host {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        search::provide(self.env.clone(), cx);
        let overlays = slots::overlays(&slots::SlotCx::default(), window, cx);
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
            .children(overlays)
    }
}

fn host(cx: &mut TestAppContext, answer: bool) -> (Entity<Host>, &mut VisualTestContext, Opened) {
    cx.update(|cx| {
        dilla_desktop::install(cx);
    });
    let opened: Opened = Rc::default();
    let state = state();
    let seen = opened.clone();
    let (host, vcx) = cx.add_window_view(|_, cx| Host::new(state, seen, answer, cx));
    (host, vcx, opened)
}

fn focused(cx: &mut VisualTestContext, handle: &FocusHandle) -> bool {
    cx.update(|window, _| handle.is_focused(window))
}

/// The name of the palette's element that holds the focus.
fn focus_name(cx: &mut VisualTestContext) -> Option<&'static str> {
    cx.update(|window, cx| {
        let held = window.focused(cx)?;
        let shown = search::inspect(window, cx)?;
        [
            ("search-input", shown.input),
            ("search-close", shown.close),
            ("search-results", shown.results),
        ]
        .into_iter()
        .find(|(_, h)| *h == held)
        .map(|(n, _)| n)
    })
}

fn open(cx: &mut VisualTestContext) {
    cx.update(|_, cx| search::request_open(cx));
    cx.run_until_parked();
}

/// What the palette lists, row by row: `(label, sub-line)`.
fn rows(cx: &mut VisualTestContext) -> Vec<(String, Option<String>)> {
    cx.update(|window, cx| search::inspect(window, cx).map_or_else(Vec::new, |s| s.rows))
}

fn is_open(cx: &mut VisualTestContext) -> bool {
    cx.update(|window, cx| search::inspect(window, cx).is_some_and(|s| s.open))
}

fn active(cx: &mut VisualTestContext) -> Option<String> {
    cx.update(|window, cx| search::inspect(window, cx).and_then(|s| s.active))
}

fn status(cx: &mut VisualTestContext) -> Option<String> {
    cx.update(|window, cx| search::inspect(window, cx).map(|s| s.status))
}

// ---- Behaviour (S5) ------------------------------------------------------------------------------------------------

/// e2e/web/search-settings.spec.ts `the search palette finds a setting and opens its section`: "microphone test" lists
/// "You / Voice", and choosing it asks the host to open the Voice section.
#[gpui::test]
fn palette_finds_setting_opens_section(cx: &mut TestAppContext) {
    let (_host, cx, opened) = host(cx, true);
    open(cx);
    cx.simulate_input("microphone test");
    cx.run_until_parked();
    let listed = rows(cx);
    let voice = listed
        .iter()
        .position(|(_, sub)| {
            sub.as_deref()
                == Some(&format!(
                    "{} / {}",
                    t("workspace.group.you"),
                    t("workspace.voice")
                ))
        })
        .expect("a You / Voice result");
    // Choose it with the arrows and Enter, as a person does.
    for _ in 0..voice {
        cx.simulate_keystrokes("down");
    }
    cx.simulate_keystrokes("enter");
    cx.run_until_parked();
    let seen = opened.borrow();
    assert!(
        matches!(seen.as_slice(), [Navigate::Settings(UserSection::Voice)]),
        "{seen:?}"
    );
    assert!(!is_open(cx), "choosing a result closes the palette");
}

/// e2e/web/search-settings.spec.ts `native edit is searched by prefix`: a prefix of a word, without its accent, finds the
/// message (the offline match is the core's rule; the real store's is `dilla-client/tests/search.rs`).
#[gpui::test]
fn edit_found_by_prefix_offline_match(cx: &mut TestAppContext) {
    let (_host, cx, _) = host(cx, true);
    open(cx);
    cx.simulate_input("cafe");
    cx.run_until_parked();
    let listed = rows(cx);
    assert!(
        listed.iter().any(|(label, _)| label == "the café compass"),
        "{listed:?}"
    );
    assert!(
        listed
            .iter()
            .filter(|(_, sub)| sub.as_deref().is_some_and(|s| s.starts_with("#general")))
            .count()
            >= 1
    );
}

/// e2e/web/search-settings.spec.ts `scopes and keyboard selection stay on the textbox until a target is chosen`: there
/// is no scope control, the arrows move the active result while the textbox keeps the focus, and Enter opens the active
/// result.
#[gpui::test]
fn scopes_and_selection_stay_on_textbox(cx: &mut TestAppContext) {
    let (_host, cx, opened) = host(cx, true);
    open(cx);
    assert_eq!(
        focus_name(cx),
        Some("search-input"),
        "opening focuses the textbox"
    );
    cx.simulate_input("rope");
    cx.run_until_parked();
    let listed = rows(cx);
    // The open conversation's hits first (#general), then the rest.
    assert_eq!(
        listed[0].1.as_deref().map(|s| s.starts_with("#general")),
        Some(true),
        "{listed:?}"
    );
    assert!(
        listed
            .iter()
            .any(|(_, s)| s.as_deref().is_some_and(|s| s.starts_with("#loot")))
    );
    let first = active(cx);
    cx.simulate_keystrokes("down");
    assert_eq!(
        focus_name(cx),
        Some("search-input"),
        "an arrow never moves the focus"
    );
    let second = active(cx);
    assert_ne!(first, second, "the arrow moved the active result");
    // The textbox still takes typing: the query grows.
    cx.simulate_input("s");
    cx.run_until_parked();
    assert!(rows(cx).iter().any(|(l, _)| l == "two ropes then"));
    cx.simulate_keystrokes("enter");
    cx.run_until_parked();
    assert!(matches!(opened.borrow().as_slice(), [Navigate::Message(_)]));
    assert!(!is_open(cx));
}

/// A result that is no longer there leaves the palette open and says so (`search.unavailable`).
#[gpui::test]
fn a_result_that_is_gone_keeps_the_palette_open(cx: &mut TestAppContext) {
    let (_host, cx, opened) = host(cx, false);
    open(cx);
    cx.simulate_input("rope");
    cx.run_until_parked();
    cx.simulate_keystrokes("enter");
    cx.run_until_parked();
    assert_eq!(opened.borrow().len(), 1);
    assert!(is_open(cx), "the palette stays open");
    assert_eq!(status(cx).as_deref(), Some(t("search.unavailable")));
    assert_eq!(focus_name(cx), Some("search-input"));
}

/// Nothing found says so, with the query in it; nothing typed says nothing.
#[gpui::test]
fn nothing_found_says_so(cx: &mut TestAppContext) {
    let (_host, cx, _) = host(cx, true);
    open(cx);
    assert_eq!(status(cx).as_deref(), Some(""));
    cx.simulate_input("lantern");
    cx.run_until_parked();
    assert!(rows(cx).is_empty());
    assert_eq!(
        status(cx).as_deref(),
        Some("Nothing in this browser matches “lantern”.")
    );
}

// ---- Keyboard (S6) -------------------------------------------------------------------------------------------------

/// Ctrl+K opens the palette with the textbox focused, and Ctrl+/ does too; a second press while it is open does nothing.
#[gpui::test]
fn keyboard_search_ctrl_k_opens(cx: &mut TestAppContext) {
    let (host, cx, _) = host(cx, true);
    let opener = host.read_with(cx, |h, _| h.opener.clone());
    cx.update(|window, cx| opener.focus(window, cx));
    cx.simulate_keystrokes("ctrl-k");
    cx.run_until_parked();
    assert!(is_open(cx), "Ctrl+K opens the palette");
    assert_eq!(focus_name(cx), Some("search-input"));
    cx.simulate_keystrokes("escape");
    cx.run_until_parked();
    assert!(!is_open(cx));
    cx.simulate_keystrokes("ctrl-/");
    cx.run_until_parked();
    assert!(is_open(cx), "Ctrl+/ opens it too");
}

/// Escape closes the palette from anywhere in it and gives the focus back to where search was opened.
#[gpui::test]
fn keyboard_search_escape_closes_and_returns_focus(cx: &mut TestAppContext) {
    let (host, cx, _) = host(cx, true);
    let opener = host.read_with(cx, |h, _| h.opener.clone());
    cx.update(|window, cx| opener.focus(window, cx));
    open(cx);
    // From the esc key cap too, not only from the textbox.
    cx.simulate_keystrokes("tab");
    assert_eq!(focus_name(cx), Some("search-close"));
    cx.simulate_keystrokes("escape");
    cx.run_until_parked();
    assert!(!is_open(cx));
    assert!(
        focused(cx, &opener),
        "focus returned to where search was opened"
    );
}

/// Arrows move the active result with wrap-around, Home and End are the textbox's own.
#[gpui::test]
fn keyboard_search_arrows_move_and_wrap(cx: &mut TestAppContext) {
    let (_host, cx, _) = host(cx, true);
    open(cx);
    let listed = rows(cx).len();
    assert!(
        listed >= 3,
        "channels and actions are listed with nothing typed"
    );
    let first = active(cx);
    cx.simulate_keystrokes("up");
    let last = active(cx);
    assert_ne!(first, last, "up from the first wraps to the last");
    cx.simulate_keystrokes("down");
    assert_eq!(active(cx), first, "down from the last wraps to the first");
    assert_eq!(focus_name(cx), Some("search-input"));
}

/// Tab goes textbox, esc key cap, results, and wraps; Shift+Tab goes back. (The palette has no scope control: the web's
/// does not either, see `scopes_and_selection_stay_on_textbox`.)
#[gpui::test]
fn keyboard_search_focus_order(cx: &mut TestAppContext) {
    let (_host, cx, _) = host(cx, true);
    open(cx);
    assert_eq!(focus_name(cx), Some("search-input"));
    let mut order = Vec::new();
    for _ in 0..4 {
        cx.simulate_keystrokes("tab");
        order.push(focus_name(cx));
    }
    assert_eq!(
        order,
        [
            Some("search-close"),
            Some("search-results"),
            Some("search-input"),
            Some("search-close")
        ]
    );
    cx.simulate_keystrokes("shift-tab");
    assert_eq!(focus_name(cx), Some("search-input"));
    cx.simulate_keystrokes("shift-tab");
    assert_eq!(
        focus_name(cx),
        Some("search-results"),
        "Shift+Tab wraps to the last stop: {:?}",
        cx.update(|w, cx| w.focused(cx).map(|h| format!("{h:?}")))
    );
}

/// Enter opens the active result; with nothing typed the first listed (a channel) opens.
#[gpui::test]
fn keyboard_search_enter_opens(cx: &mut TestAppContext) {
    let (_host, cx, opened) = host(cx, true);
    open(cx);
    cx.simulate_keystrokes("enter");
    cx.run_until_parked();
    assert!(matches!(
        opened.borrow().as_slice(),
        [Navigate::Channel { channel, .. }] if *channel == ChannelId(id(2))
    ));
    assert!(!is_open(cx));
}

// ---- The offline app (S4, S6 through the driver) --------------------------------------------------------------------

fn app(name: &str) -> Driver {
    let mut app = Driver::launch_fixture(&fixture_path(name), None, &[]);
    app.wait_idle();
    app
}

fn same_box(a: &dilla_parity::ElementInfo, b: &dilla_parity::ElementInfo) -> bool {
    (a.x - b.x).abs() < 0.5
        && (a.y - b.y).abs() < 0.5
        && (a.width - b.width).abs() < 0.5
        && (a.height - b.height).abs() < 0.5
}

/// The same keys in the real (headless) app, drawn from a fixture, with nothing focused when Ctrl+K is pressed: the palette
/// opens with the textbox focused; the arrows move the active row while the textbox keeps the focus; Tab walks the stops
/// and wraps; Escape closes it.
#[test]
#[cfg_attr(
    not(target_os = "linux"),
    ignore = "the references and the driver run on Linux"
)]
fn keyboard_search_in_the_app() {
    let mut app = app("results");
    assert!(app.element("search-input").is_none(), "closed at first");
    app.press("ctrl-k");
    app.wait_idle();
    assert_eq!(app.focused().as_deref(), Some("search-input"));
    // "bridge": the results fixture's own query (its three messages hold it); "rope" is in no message of it.
    app.type_focused("bridge");
    app.wait_idle();
    let first = app.element("search-option-0").expect("a first result");
    assert!(
        app.element("search-active")
            .is_some_and(|a| same_box(&a, &first)),
        "the first result is the active one"
    );
    app.press("down");
    app.wait_idle();
    assert_eq!(
        app.focused().as_deref(),
        Some("search-input"),
        "an arrow never moves the focus"
    );
    let second = app.element("search-option-1").expect("a second result");
    assert!(
        app.element("search-active")
            .is_some_and(|a| same_box(&a, &second)),
        "the arrow moved the active row"
    );
    // Tab: esc key cap, results, textbox again.
    for want in ["search-close", "search-results", "search-input"] {
        app.press("tab");
        assert_eq!(app.focused().as_deref(), Some(want));
    }
    app.press("escape");
    app.wait_idle();
    assert!(
        app.element("search-input").is_none(),
        "Escape closed the palette"
    );
}

/// Enter opens the first result (the first channel) and closes the palette.
#[test]
#[cfg_attr(
    not(target_os = "linux"),
    ignore = "the references and the driver run on Linux"
)]
fn keyboard_search_enter_opens_a_channel_in_the_app() {
    let mut app = app("palette");
    app.press("ctrl-k");
    app.wait_idle();
    assert!(app.element("search-input").is_some(), "open");
    app.press("enter");
    app.wait_idle();
    assert!(
        app.element("search-input").is_none(),
        "choosing a result closes the palette"
    );
}
