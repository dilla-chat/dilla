//! Behaviour of the shared components (Task 6): the dialog's focus trap and return, the action menu's keys, the toast's
//! timeout, the segmented group's arrows. Each runs the component in a real (test) window and drives it with keystrokes.

use std::time::Duration;

use dilla_desktop::screens::{FocusNext, FocusPrevious};
use dilla_desktop::strings::t;
use dilla_desktop_ui::components::action_menu::{ActionMenu, MenuItem};
use dilla_desktop_ui::components::dialog::Dialog;
use dilla_desktop_ui::components::segmented::Segmented;
use dilla_desktop_ui::components::toast::{TOAST_MS, ToastHost, current, show_toast};
use dilla_desktop_ui::components::{Button, ButtonVariant};
use gpui::{
    Context, Entity, FocusHandle, InteractiveElement, IntoElement, ParentElement, Render, Styled,
    TestAppContext, VisualTestContext, Window, div, px,
};

/// Presses a key as a keyboard does: down, then up (GPUI turns Enter or Space down-and-up on a focused element into a click).
fn press(cx: &mut VisualTestContext, key: &'static str) {
    cx.simulate_keystrokes(key);
    cx.simulate_event(gpui::KeyUpEvent {
        keystroke: gpui::Keystroke::parse(key).expect("a key"),
    });
    cx.run_until_parked();
}

fn focused(cx: &mut VisualTestContext, handle: &FocusHandle) -> bool {
    cx.update(|window, _| handle.is_focused(window))
}

/// A window with an opener, a second control outside, and a dialog that opens over them.
struct DialogHost {
    open: bool,
    opener: FocusHandle,
    outside: FocusHandle,
    close: FocusHandle,
    save: FocusHandle,
    saved: usize,
}

impl Render for DialogHost {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let this = cx.entity();
        let (close, save) = (this.clone(), this);
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
            .child(
                div()
                    .id("outside")
                    .track_focus(&self.outside)
                    .tab_stop(true)
                    .w(px(80.0))
                    .h(px(24.0)),
            )
            .child(
                Dialog::new("dlg", "Rename", t("dialog.close"))
                    .open(self.open)
                    .close_handle(&self.close)
                    .body(div().child("body"))
                    .footer(
                        Button::new("dlg-save", "Save")
                            .focus_handle(&self.save)
                            .variant(ButtonVariant::Accent)
                            .on_click(move |_, _, cx| save.update(cx, |t, _| t.saved += 1)),
                    )
                    .on_close(move |_, cx| {
                        close.update(cx, |t, cx| {
                            t.open = false;
                            cx.notify();
                        });
                    }),
            )
    }
}

fn install(cx: &mut TestAppContext) {
    cx.update(|cx| {
        dilla_desktop::install(cx);
    });
}

#[gpui::test]
fn dialog_traps_focus_and_escape_returns_it(cx: &mut TestAppContext) {
    install(cx);
    let (host, cx) = cx.add_window_view(|_, cx| DialogHost {
        open: false,
        opener: cx.focus_handle().tab_stop(true),
        outside: cx.focus_handle().tab_stop(true),
        close: cx.focus_handle().tab_stop(true),
        save: cx.focus_handle().tab_stop(true),
        saved: 0,
    });
    let (opener, outside, close, save) = host.read_with(cx, |h, _| {
        (
            h.opener.clone(),
            h.outside.clone(),
            h.close.clone(),
            h.save.clone(),
        )
    });
    cx.update(|window, cx| opener.focus(window, cx));
    assert!(
        focused(cx, &opener),
        "the opener has focus before the dialog opens"
    );

    host.update(cx, |h, cx| {
        h.open = true;
        cx.notify();
    });
    cx.run_until_parked();
    let focus_is_some = |cx: &mut VisualTestContext| cx.update(|w, cx| w.focused(cx).is_some());
    assert!(focus_is_some(cx), "opening puts focus somewhere");
    assert!(
        !focused(cx, &opener) && !focused(cx, &outside),
        "opening moves focus into the dialog"
    );

    // The real cycle (the panel itself is never a place to rest): Close, Save, Close, ... and back with Shift+Tab,
    // wrapping to the last control (Save) from the first.
    let step = |cx: &mut VisualTestContext, key: &'static str, want: &FocusHandle, name: &str| {
        cx.simulate_keystrokes(key);
        assert!(focus_is_some(cx), "{key} left focus nowhere");
        assert!(focused(cx, want), "{key} should land on {name}");
        assert!(
            !focused(cx, &opener) && !focused(cx, &outside),
            "{key} left the dialog"
        );
    };
    step(cx, "tab", &close, "Close");
    step(cx, "tab", &save, "Save");
    step(cx, "tab", &close, "Close (wrapped)");
    step(cx, "shift-tab", &save, "Save (wrapped back)");
    step(cx, "shift-tab", &close, "Close");
    step(cx, "shift-tab", &save, "Save (wrapped back)");

    // Escape closes it and focus goes back to what had it.
    cx.simulate_keystrokes("escape");
    cx.run_until_parked();
    assert!(
        !host.read_with(cx, |h, _| h.open),
        "Escape closed the dialog"
    );
    assert!(focused(cx, &opener), "Escape returned focus to the opener");
}

#[gpui::test]
fn dialog_leaves_focus_alone_when_a_flow_moved_it_elsewhere(cx: &mut TestAppContext) {
    install(cx);
    let (host, cx) = cx.add_window_view(|_, cx| DialogHost {
        open: false,
        opener: cx.focus_handle().tab_stop(true),
        outside: cx.focus_handle().tab_stop(true),
        close: cx.focus_handle().tab_stop(true),
        save: cx.focus_handle().tab_stop(true),
        saved: 0,
    });
    let (opener, outside) = host.read_with(cx, |h, _| (h.opener.clone(), h.outside.clone()));
    cx.update(|window, cx| opener.focus(window, cx));
    host.update(cx, |h, cx| {
        h.open = true;
        cx.notify();
    });
    cx.run_until_parked();
    // The host closes the dialog and puts focus on another control in the same update.
    host.update(cx, |h, cx| {
        h.open = false;
        cx.notify();
    });
    cx.update(|window, cx| outside.focus(window, cx));
    cx.run_until_parked();
    assert!(focused(cx, &outside), "a flow that moved focus keeps it");
}

struct MenuHost {
    open: bool,
    chosen: Vec<String>,
    root: FocusHandle,
}

impl Render for MenuHost {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let (open, select) = (cx.entity(), cx.entity());
        div()
            .id("menu-host")
            .track_focus(&self.root)
            .relative()
            .size_full()
            .on_action(|_: &FocusNext, window, cx| window.focus_next(cx))
            .on_action(|_: &FocusPrevious, window, cx| window.focus_prev(cx))
            .child(
                ActionMenu::new(
                    "actions",
                    "actions for Astrid",
                    vec![
                        MenuItem::new("copy", "Copy link"),
                        MenuItem::new("mute", "Mute").check(false),
                        MenuItem::new("kick", "Kick").danger().sep(),
                    ],
                )
                .open(self.open)
                .on_open_change(move |is_open, _, cx| {
                    open.update(cx, |t, cx| {
                        t.open = is_open;
                        cx.notify();
                    });
                })
                .on_select(move |id, _, cx| {
                    select.update(cx, |t, _| t.chosen.push(id.to_owned()));
                }),
            )
    }
}

#[gpui::test]
fn action_menu_opens_on_shift_f10_and_menu_key(cx: &mut TestAppContext) {
    install(cx);
    let (host, cx) = cx.add_window_view(|_, cx| MenuHost {
        open: false,
        chosen: Vec::new(),
        root: cx.focus_handle(),
    });
    // The trigger is the window's only tab stop: Tab from the (focused) window root reaches it.
    let root = host.read_with(cx, |h, _| h.root.clone());
    cx.update(|window, cx| root.focus(window, cx));
    cx.simulate_keystrokes("tab");
    let open =
        |host: &Entity<MenuHost>, cx: &mut VisualTestContext| host.read_with(cx, |h, _| h.open);

    cx.simulate_keystrokes("shift-f10");
    cx.run_until_parked();
    assert!(open(&host, cx), "Shift+F10 opens the menu");
    cx.simulate_keystrokes("escape");
    cx.run_until_parked();
    assert!(!open(&host, cx), "Escape closes it");

    // Escape put focus back on the trigger, so the Menu key opens it again.
    cx.simulate_keystrokes("menu");
    cx.run_until_parked();
    assert!(open(&host, cx), "the Menu key opens the menu");

    // Inside: the arrows move (wrapping), Enter chooses and closes; a check item keeps the menu open.
    press(cx, "down");
    press(cx, "enter");
    assert_eq!(host.read_with(cx, |h, _| h.chosen.clone()), ["mute"]);
    assert!(open(&host, cx), "a check item keeps the menu open");
    press(cx, "down");
    press(cx, "enter");
    assert_eq!(
        host.read_with(cx, |h, _| h.chosen.clone()),
        ["mute", "kick"]
    );
    assert!(!open(&host, cx), "choosing an action closes the menu");
}

#[gpui::test]
fn tab_in_an_open_menu_closes_it_and_moves_on(cx: &mut TestAppContext) {
    install(cx);
    let (host, cx) = cx.add_window_view(|_, cx| MenuHost {
        open: false,
        chosen: Vec::new(),
        root: cx.focus_handle(),
    });
    let root = host.read_with(cx, |h, _| h.root.clone());
    cx.update(|window, cx| root.focus(window, cx));
    cx.simulate_keystrokes("tab");
    cx.simulate_keystrokes("down");
    cx.run_until_parked();
    assert!(host.read_with(cx, |h, _| h.open), "Down opens the menu");
    cx.simulate_keystrokes("tab");
    cx.run_until_parked();
    assert!(!host.read_with(cx, |h, _| h.open), "Tab closed the menu");
    assert!(
        cx.update(|w, cx| w.focused(cx).is_some()),
        "focus moved on, not nowhere"
    );
}

struct ToastRoot;

impl Render for ToastRoot {
    fn render(&mut self, _window: &mut Window, _cx: &mut Context<Self>) -> impl IntoElement {
        div().relative().size_full().child(ToastHost::new("toasts"))
    }
}

#[gpui::test]
fn toast_auto_dismisses_after_ui_timeout(cx: &mut TestAppContext) {
    install(cx);
    let (_root, cx) = cx.add_window_view(|_, _| ToastRoot);
    assert_eq!(cx.update(|_, cx| current(cx)), None);

    cx.update(|_, cx| show_toast("Copied", cx));
    assert_eq!(cx.update(|_, cx| current(cx)).as_deref(), Some("Copied"));
    cx.executor()
        .advance_clock(Duration::from_millis(TOAST_MS - 1));
    cx.run_until_parked();
    assert_eq!(
        cx.update(|_, cx| current(cx)).as_deref(),
        Some("Copied"),
        "still showing just before the timeout"
    );
    cx.executor().advance_clock(Duration::from_millis(2));
    cx.run_until_parked();
    assert_eq!(
        cx.update(|_, cx| current(cx)),
        None,
        "gone after the timeout"
    );

    // A second toast replaces the first and restarts the clock: the first one's timer must not end it early.
    cx.update(|_, cx| show_toast("One", cx));
    cx.executor().advance_clock(Duration::from_millis(1000));
    cx.update(|_, cx| show_toast("Two", cx));
    cx.executor()
        .advance_clock(Duration::from_millis(TOAST_MS - 1000 + 100));
    cx.run_until_parked();
    assert_eq!(cx.update(|_, cx| current(cx)).as_deref(), Some("Two"));
    cx.executor().advance_clock(Duration::from_millis(1000));
    cx.run_until_parked();
    assert_eq!(cx.update(|_, cx| current(cx)), None);
}

struct SegmentedHost {
    value: String,
    root: FocusHandle,
}

impl Render for SegmentedHost {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let this = cx.entity();
        div()
            .id("seg-host")
            .track_focus(&self.root)
            .size_full()
            .on_action(|_: &FocusNext, window, cx| window.focus_next(cx))
            .child(
                Segmented::new(
                    "density",
                    &[
                        ("compact", "Compact"),
                        ("regular", "Regular"),
                        ("cozy", "Cozy"),
                    ],
                    self.value.clone(),
                )
                .on_change(move |value, _, cx| {
                    this.update(cx, |t, cx| {
                        t.value = value.to_owned();
                        cx.notify();
                    });
                }),
            )
    }
}

#[gpui::test]
fn segmented_arrows_select_and_wrap(cx: &mut TestAppContext) {
    install(cx);
    let (host, cx) = cx.add_window_view(|_, cx| SegmentedHost {
        value: "regular".into(),
        root: cx.focus_handle(),
    });
    let root = host.read_with(cx, |h, _| h.root.clone());
    cx.update(|window, cx| root.focus(window, cx));
    let value = |host: &Entity<SegmentedHost>, cx: &mut VisualTestContext| {
        host.read_with(cx, |h, _| h.value.clone())
    };
    cx.simulate_keystrokes("tab");
    cx.simulate_keystrokes("right");
    cx.run_until_parked();
    assert_eq!(value(&host, cx), "cozy");
    cx.simulate_keystrokes("right");
    cx.run_until_parked();
    assert_eq!(value(&host, cx), "compact", "the arrows wrap");
    cx.simulate_keystrokes("end");
    cx.run_until_parked();
    assert_eq!(value(&host, cx), "cozy");
    cx.simulate_keystrokes("home");
    cx.run_until_parked();
    assert_eq!(value(&host, cx), "compact");
}

/// A click on an option chooses it, as the web's radio does (Segmented.tsx `onClick`): the pointer arrives, rests past the
/// tooltip delay, and presses, as a person's does. The options once had only the keyboard, so a click on the invite's
/// "Expires after" or "Uses" did nothing (Mac report, 2026-11-16).
#[gpui::test]
fn segmented_a_click_chooses_the_option(cx: &mut TestAppContext) {
    install(cx);
    let (host, cx) = cx.add_window_view(|_, cx| SegmentedHost {
        value: "regular".into(),
        root: cx.focus_handle(),
    });
    cx.run_until_parked();
    let value = |host: &Entity<SegmentedHost>, cx: &mut VisualTestContext| {
        host.read_with(cx, |h, _| h.value.clone())
    };
    for (selector, want) in [
        ("segmented-density-cozy", "cozy"),
        ("segmented-density-compact", "compact"),
        // The checked option again: no change.
        ("segmented-density-compact-checked", "compact"),
    ] {
        let at = cx
            .debug_bounds(selector)
            .unwrap_or_else(|| panic!("{selector} drew"))
            .center();
        person_click(cx, at);
        assert_eq!(value(&host, cx), want, "a click on {selector}");
    }
}

/// A person's click: the pointer moves there, rests (hover styles and tooltip timers run), then presses and releases.
fn person_click(cx: &mut VisualTestContext, at: gpui::Point<gpui::Pixels>) {
    cx.simulate_mouse_move(at, None, gpui::Modifiers::none());
    cx.executor().advance_clock(Duration::from_millis(600));
    cx.run_until_parked();
    cx.simulate_click(at, gpui::Modifiers::none());
    cx.run_until_parked();
}

// The gallery draws every component in each of its states; this draws it once with every overlay open, so a component that
// panics on layout or paint fails here and not in front of a person.
#[allow(dead_code)]
#[path = "../examples/gallery.rs"]
mod gallery;

#[gpui::test]
fn every_component_renders_in_every_state(cx: &mut TestAppContext) {
    install(cx);
    let (page, cx) = cx.add_window_view(gallery::Gallery::new);
    cx.run_until_parked();
    page.update(cx, |g, cx| {
        g.open_all();
        cx.notify();
    });
    cx.update(|_, cx| show_toast("Link copied", cx));
    cx.run_until_parked();
    // Tab around inside the open overlays and press Escape: the settings frame (the topmost) closes first.
    cx.simulate_keystrokes("tab tab shift-tab");
    assert!(page.read_with(cx, |g, _| g.settings_open()));
    cx.simulate_keystrokes("escape");
    cx.run_until_parked();
    assert!(
        !page.read_with(cx, |g, _| g.settings_open()),
        "Escape closed the settings frame"
    );
}

/// Settings over a nav taller than the window; `active` is the current section.
struct NavHost {
    active: &'static str,
}

impl Render for NavHost {
    fn render(&mut self, _window: &mut Window, _cx: &mut Context<Self>) -> impl IntoElement {
        use dilla_desktop_ui::components::settings::frame::SettingsFrame;
        use dilla_desktop_ui::components::settings::nav::{NavItem, SettingsNav};
        let items: Vec<NavItem> = (0..30)
            .map(|i| NavItem::new(format!("s{i}"), format!("Section {i}")))
            .collect();
        div().relative().size_full().child(
            SettingsFrame::new("nav-host", "Close").nav(SettingsNav::new(
                "nav-host-nav",
                items,
                self.active,
            )),
        )
    }
}

/// Review I1: the nav scrolls its current item into view when it becomes current (the web's `scrollIntoView` on a route
/// change), and then leaves the column to the person: wheeling it back to the top keeps it there.
#[gpui::test]
fn the_nav_column_scrolls_to_the_current_item_once(cx: &mut TestAppContext) {
    install(cx);
    let (host, cx) = cx.add_window_view(|_, _| NavHost { active: "s0" });
    cx.simulate_resize(gpui::size(px(1280.0), px(500.0)));
    cx.run_until_parked();
    // The route changes to the last section: the column scrolls it into view.
    host.update(cx, |h, cx| {
        h.active = "s29";
        cx.notify();
    });
    cx.run_until_parked();
    let height = f32::from(cx.update(|window, _| window.viewport_size().height));
    let last = cx.debug_bounds("nav-item-s29").expect("the current item");
    assert!(
        f32::from(last.bottom()) <= height + 0.5,
        "the current item is scrolled into view: {last:?}"
    );
    let first = cx.debug_bounds("nav-item-s0").expect("the first item");
    assert!(
        f32::from(first.top()) < 0.0,
        "the first item is above the fold: {first:?}"
    );

    // Wheel the column back to its top: it stays there, frame after frame.
    cx.simulate_event(gpui::ScrollEvent {
        position: gpui::point(px(100.0), px(200.0)),
        delta: gpui::ScrollDelta::Pixels(gpui::point(px(0.0), px(5000.0))),
        ..Default::default()
    });
    cx.run_until_parked();
    for _ in 0..3 {
        cx.update(|window, _| window.refresh());
        cx.run_until_parked();
    }
    let first = cx.debug_bounds("nav-item-s0").expect("the first item");
    assert!(
        f32::from(first.top()) >= 0.0,
        "the column stays where the wheel left it: {first:?}"
    );
}

/// A dialog whose body is taller than the window allows it.
struct TallDialog;

impl Render for TallDialog {
    fn render(&mut self, _window: &mut Window, _cx: &mut Context<Self>) -> impl IntoElement {
        div().relative().size_full().child(
            Dialog::new("tall", "Tall", "Close")
                .body(div().h(px(1500.0)))
                .footer(dilla_desktop::tracked_box(
                    "tall-save",
                    Button::new("tall-save-b", "Save"),
                )),
        )
    }
}

/// Review m3: a dialog taller than its max height scrolls as one box (the web's `<dialog>` is `overflow: auto`), so its
/// footer can be reached with the wheel instead of being clipped away.
#[gpui::test]
fn a_dialog_taller_than_the_window_scrolls_to_its_footer(cx: &mut TestAppContext) {
    install(cx);
    let (_host, cx) = cx.add_window_view(|_, _| TallDialog);
    cx.simulate_resize(gpui::size(px(1280.0), px(600.0)));
    cx.run_until_parked();
    let height = f32::from(cx.update(|window, _| window.viewport_size().height));
    cx.simulate_event(gpui::ScrollEvent {
        position: gpui::point(px(640.0), px(300.0)),
        delta: gpui::ScrollDelta::Pixels(gpui::point(px(0.0), px(-5000.0))),
        ..Default::default()
    });
    cx.run_until_parked();
    let save = cx.debug_bounds("tall-save").expect("the footer's button");
    assert!(
        f32::from(save.bottom()) <= height && f32::from(save.top()) >= 0.0,
        "the footer scrolls into the window: {save:?}"
    );
}
