//! Behaviour of the shared components (Task 6): the dialog's focus trap and return, the action menu's keys, the toast's
//! timeout, the segmented group's arrows. Each runs the component in a real (test) window and drives it with keystrokes.

use std::time::Duration;

use dilla_desktop::screens::{FocusNext, FocusPrevious};
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
                Dialog::new("dlg", "Rename")
                    .open(self.open)
                    .body(div().child("body"))
                    .footer(
                        Button::new("dlg-save", "Save")
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
        saved: 0,
    });
    let (opener, outside) = host.read_with(cx, |h, _| (h.opener.clone(), h.outside.clone()));
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
    assert!(
        !focused(cx, &opener) && !focused(cx, &outside),
        "opening moves focus into the dialog"
    );

    // Tab and Shift+Tab cycle inside the dialog: focus never reaches the controls behind it.
    for key in [
        "tab",
        "tab",
        "tab",
        "tab",
        "shift-tab",
        "shift-tab",
        "shift-tab",
        "shift-tab",
    ] {
        cx.simulate_keystrokes(key);
        assert!(
            !focused(cx, &opener) && !focused(cx, &outside),
            "{key} left the dialog (opener={}, outside={})",
            focused(cx, &opener),
            focused(cx, &outside)
        );
    }

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
    cx.simulate_keystrokes("tab tab shift-tab escape");
    cx.run_until_parked();
}
