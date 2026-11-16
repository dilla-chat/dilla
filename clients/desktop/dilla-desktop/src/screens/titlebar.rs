//! The top bar (packages/ui TopBar, Signal §5.1) and the window's own controls (packages/web DesktopTitlebar): the brand,
//! the search field, the bell and settings buttons, and, in the shipping app, the menu and the minimise, maximise and close
//! buttons. The bar is also the window's drag region (a press moves the window, a double click maximises it). A fixture
//! draws the bar without the window controls, as the web client does in a browser.

use gpui::{
    App, ClickEvent, FocusHandle, InteractiveElement, IntoElement, MouseButton, MouseDownEvent,
    ParentElement, Pixels, StatefulInteractiveElement, Styled, Window, canvas, div,
};

use dilla_desktop_ui::a11y::{Accessible, Role};
use dilla_desktop_ui::components::tooltip::{SHOW_DELAY, tooltip};
use dilla_desktop_ui::metrics::focus_ring;
use dilla_desktop_ui::text::{line, line_height};
use dilla_desktop_ui::tokens::{color, font, layout, radius, text};
use dilla_desktop_ui::{Icon, IconName};

use crate::screens::{LABEL_TRACKING_WIDE, centred};
use crate::strings::t;

/// TopBar.css.
mod css {
    pub const LEFT: f32 = 14.0;
    pub const RIGHT: f32 = 12.0;
    pub const GAP: f32 = 12.0;
    pub const BRAND_GAP: f32 = 8.0;
    pub const BRAND_MARK: f32 = 18.0;
    pub const BRAND_CURSOR_W: f32 = 6.0;
    pub const BRAND_CURSOR_H: f32 = 12.0;
    pub const SEARCH_SLOT: f32 = 420.0;
    pub const SEARCH_W: f32 = 360.0;
    pub const SEARCH_H: f32 = 28.0;
    pub const SEARCH_LEFT: f32 = 10.0;
    pub const SEARCH_RIGHT: f32 = 8.0;
    pub const SEARCH_GLYPH: f32 = 13.0;
    pub const KEYCAP_H: f32 = 18.0;
    pub const KEYCAP_PAD: f32 = 5.0;
    pub const END_GAP: f32 = 8.0;
    pub const BUTTON: f32 = 28.0;
    pub const BUTTON_GLYPH: f32 = 15.0;
    /// `.d-topbar__voice`: 28px, 10px in.
    pub const VOICE_H: f32 = 28.0;
    pub const VOICE_PAD_X: f32 = 10.0;
    /// DesktopTitlebar.css: the window buttons are 28px with a 14px glyph; the menu button the same.
    pub const WINDOW_GLYPH: f32 = 14.0;
    /// DesktopTitlebar.css `html[data-desktop-platform="darwin"] .d-topbar { padding-left: 112px }`: the macOS traffic
    /// lights (at 12, 16; `crate::MAC_WINDOW_BUTTONS`) and beta.5's menu button (`left: 78px`, 32px wide) sit left of
    /// the brand.
    pub const DARWIN_LEFT: f32 = 112.0;
    /// `.dw-desktop-card-close` in the compact (starting and authentication) windows: 22px, 6px in from the top right,
    /// its glyph 14px.
    pub const CARD_CLOSE: f32 = 22.0;
    pub const CARD_CLOSE_INSET: f32 = 6.0;
    /// `.dw-desktop-card-drag`: the compact windows' drag strip, `inset: 0 34px auto 0; height: 26px`.
    pub const CARD_DRAG_H: f32 = 26.0;
    pub const CARD_DRAG_RIGHT: f32 = 34.0;
}

/// The bar's left padding: beta.5's 112px on macOS, where the traffic lights sit in the bar, else TopBar.css's 14px. A
/// fixture (no window controls) draws the web's bar on every platform.
pub fn left_inset(mac: bool, controls: bool) -> f32 {
    if mac && controls {
        css::DARWIN_LEFT
    } else {
        css::LEFT
    }
}

/// The window buttons' names (packages/web DesktopTitlebar.tsx holds them as literals; `strings.rs` has no key).
const MINIMIZE: &str = "Minimize window";
const MAXIMIZE: &str = "Maximize or restore window";
const CLOSE: &str = "Close window";
/// The compact windows' close button (DesktopTitlebar.tsx `dw-desktop-card-close`).
const CLOSE_LOGIN: &str = "Close login window";

/// The bar is the window's drag region except where the web excludes it (`button, input, a, .d-topbar__search`): an
/// interactive child takes its own press, so a click on it is a click and not the start of a window move.
fn interactive<E: InteractiveElement>(element: E) -> E {
    element.on_mouse_down(MouseButton::Left, |_: &MouseDownEvent, window, cx| {
        window.prevent_default();
        cx.stop_propagation();
    })
}

pub struct TitlebarModel {
    /// Draws the window's own buttons (and makes the bar a drag region): the shipping window, not a fixture.
    pub controls: bool,
    /// The ⌘K / Ctrl K hint in the search field.
    pub search_hint: &'static str,
    /// The due reminders: the bell draws a count badge above 0.
    pub reminders_due: usize,
    /// The reminders popover is open: the bell reads as pressed.
    pub reminders_open: bool,
    /// The bell's focus (the popover's Escape gives it back).
    pub bell_focus: Option<FocusHandle>,
    /// The voice chip: where this device's call is, while it is joining or in one.
    pub voice: Option<String>,
}

#[derive(Clone)]
pub struct TitlebarHandlers {
    /// The voice chip was pressed: the call's channel or DM opens.
    pub on_voice: super::shell::Action,
    /// The settings button was pressed.
    pub on_settings: super::shell::Action,
    /// The search field was pressed: the search palette opens (Task 23).
    pub on_search: super::shell::Action,
    /// The bell was pressed: the reminders popover opens or closes.
    pub on_reminders: super::shell::Action,
}

/// TopBar.css `.d-topbar__badge`: 14px high, at least as wide, 2px in from the bell's top right, a 2px ring of the bar.
mod badge {
    pub const SIZE: f32 = 14.0;
    pub const INSET: f32 = 2.0;
    pub const PAD: f32 = 3.0;
    pub const RING: f32 = 2.0;
}

/// The bell (`.d-topbar__bell`): its button, its due count and, while open, the reminders popover hanging under it.
fn bell(
    model: &TitlebarModel,
    on_reminders: super::shell::Action,
    popover: Option<gpui::AnyElement>,
) -> impl IntoElement + use<> {
    let open = model.reminders_open;
    let mut button = interactive(div().id("topbar-bell"))
        .a11y(Role::Button, t("reminder.inbox"))
        .aria_expanded(open)
        .node_props(|n| n.set_has_popup(gpui::accesskit::HasPopup::Dialog))
        .tab_index(0)
        .cursor_pointer()
        .rounded(radius::MD)
        .focus_visible(|s| s.shadow(focus_ring()))
        .tooltip(tooltip(t("reminder.inbox"), &[]))
        .tooltip_show_delay(SHOW_DELAY)
        .on_click(move |_: &ClickEvent, window, cx| on_reminders(window, cx))
        .child(centred(
            Pixels::from(css::BUTTON),
            Icon::new(IconName::Bell)
                .size(Pixels::from(css::BUTTON_GLYPH))
                .color(if open { color::ACCENT } else { color::FG_2 }),
        ));
    if open {
        button = button.bg(color::ACCENT_SOFT);
    }
    if let Some(focus) = &model.bell_focus {
        button = button.track_focus(focus);
    }
    let due = model.reminders_due;
    let badge = (due > 0).then(|| {
        let count = if due > 99 {
            "99+".to_owned()
        } else {
            due.to_string()
        };
        div()
            .absolute()
            .top(Pixels::from(badge::INSET))
            .right(Pixels::from(badge::INSET))
            .min_w(Pixels::from(badge::SIZE))
            .h(Pixels::from(badge::SIZE))
            .px(Pixels::from(badge::PAD))
            .flex()
            .items_center()
            .justify_center()
            .rounded(radius::PILL)
            .bg(color::MENTION)
            .shadow(dilla_desktop_ui::metrics::spread_ring(
                color::BG_2,
                badge::RING,
            ))
            .child(
                line(
                    count,
                    text::BADGE,
                    f32::from(text::BADGE),
                    color::MENTION_INK,
                )
                .weight(font::WEIGHT_DISPLAY),
            )
    });
    // The popover's press-outside check leaves a press on the bell to the bell's own toggle.
    let bounds = canvas(
        |bounds, window, cx| {
            crate::screens::commands::reminders::note_bell(
                window.window_handle().window_id(),
                bounds,
                cx,
            )
        },
        |_, (), _, _| {},
    )
    .absolute()
    .size_full();
    let wrap = div()
        .relative()
        .flex_none()
        .flex()
        .child(button)
        .child(bounds)
        .children(badge)
        .children(popover);
    #[cfg(feature = "automation")]
    let wrap = wrap
        .child(crate::automation::bounds_probe("topbar-bell"))
        .children(
            model
                .bell_focus
                .as_ref()
                .map(|f| crate::automation::focus_probe("topbar-bell", f)),
        );
    wrap
}

fn window_button(
    id: &'static str,
    icon: IconName,
    label: &'static str,
    danger_hover: bool,
    run: impl Fn(&mut Window, &mut App) + 'static,
) -> impl IntoElement {
    interactive(div().id(id))
        .a11y(Role::Button, label)
        .tab_index(0)
        .flex_none()
        .size(Pixels::from(css::BUTTON))
        .flex()
        .items_center()
        .justify_center()
        .rounded(radius::MD)
        .cursor_pointer()
        .focus_visible(|s| s.shadow(focus_ring()))
        .hover(move |s| {
            if danger_hover {
                s.bg(color::DANGER_SOFT)
            } else {
                s.bg(color::HOVER)
            }
        })
        .tooltip(tooltip(label, &[]))
        .tooltip_show_delay(SHOW_DELAY)
        .on_click(move |_: &ClickEvent, window, cx| run(window, cx))
        .child(
            Icon::new(icon)
                .size(Pixels::from(css::WINDOW_GLYPH))
                .color(color::FG_2),
        )
}

pub fn render(
    model: &TitlebarModel,
    handlers: &TitlebarHandlers,
    reminders: Option<gpui::AnyElement>,
) -> impl IntoElement + use<> {
    let brand_name_line = line_height(text::XS, 1.5);
    let mark = centred(
        Pixels::from(css::BRAND_MARK),
        line("D", text::XS, line_height(text::XS, 1.0), color::BRAND_INK)
            .weight(font::WEIGHT_DISPLAY),
    )
    .rounded(radius::SM)
    .bg(color::BRAND);
    // The bar's leftmost drawing, which the macOS traffic-light clearance test keeps out of the lights.
    #[cfg(feature = "automation")]
    let mark = mark
        .relative()
        .child(crate::automation::bounds_probe("topbar-brand"));
    let brand = div()
        .flex_1()
        .min_w_0()
        .flex()
        .items_center()
        .gap(Pixels::from(css::BRAND_GAP))
        .child(mark)
        .child(
            line("DILLA", text::XS, brand_name_line, color::FG)
                .weight(font::WEIGHT_DISPLAY)
                .tracking(LABEL_TRACKING_WIDE),
        )
        .child(
            div()
                .w(Pixels::from(css::BRAND_CURSOR_W))
                .h(Pixels::from(css::BRAND_CURSOR_H))
                .bg(color::BRAND),
        );
    let search = div()
        .w(Pixels::from(css::SEARCH_SLOT))
        .flex_none()
        .flex()
        .justify_center()
        .child(
            interactive(div().id("topbar-search"))
                .a11y(Role::Button, t("topbar.search"))
                .aria_keyshortcuts("Control+K Meta+K")
                .tab_index(0)
                .focus_visible(|s| s.shadow(focus_ring()))
                .cursor_pointer()
                .on_click({
                    let on_search = handlers.on_search.clone();
                    move |_, window, cx| on_search(window, cx)
                })
                .w(Pixels::from(css::SEARCH_W))
                .h(Pixels::from(css::SEARCH_H))
                .bg(color::SURFACE_2)
                .border_1()
                .border_color(color::HAIRLINE_2)
                .rounded(radius::MD)
                .flex()
                .items_center()
                .pl(Pixels::from(css::SEARCH_LEFT))
                .pr(Pixels::from(css::SEARCH_RIGHT))
                .gap(Pixels::from(css::BRAND_GAP))
                .child(
                    Icon::new(IconName::Search)
                        .size(Pixels::from(css::SEARCH_GLYPH))
                        .color(color::FG_3),
                )
                .child(
                    div().flex_1().min_w_0().flex().child(
                        line(
                            t("topbar.search"),
                            text::UI,
                            line_height(text::UI, 1.5),
                            color::FG_3,
                        )
                        .ellipsis(),
                    ),
                )
                .child(
                    div()
                        .flex_none()
                        .h(Pixels::from(css::KEYCAP_H))
                        .border_1()
                        .border_color(color::CONTROL_EDGE)
                        .rounded(radius::SM)
                        .px(Pixels::from(css::KEYCAP_PAD))
                        .child(line(
                            model.search_hint,
                            text::MICRO,
                            line_height(text::MICRO, 1.6),
                            color::FG_2,
                        )),
                ),
        );
    let on_settings = handlers.on_settings.clone();
    let on_voice = handlers.on_voice.clone();
    // TopBar.tsx's voice chip, only while in a call: the room (and the link's ping once the client reads one).
    let voice = model.voice.clone().map(|room| {
        interactive(div().id("topbar-voice"))
            .a11y(Role::Button, room.clone())
            .tab_index(0)
            .cursor_pointer()
            .flex_none()
            .flex()
            .items_center()
            .h(Pixels::from(css::VOICE_H))
            .px(Pixels::from(css::VOICE_PAD_X))
            .bg(color::OK_TINT)
            .border_1()
            .border_color(color::OK_BORDER)
            .rounded(radius::MD)
            .focus_visible(|s| s.shadow(focus_ring()))
            .on_click(move |_: &ClickEvent, window, cx| on_voice(window, cx))
            .child(line(
                room,
                text::BTN,
                line_height(text::BTN, 1.5),
                color::OK_TEXT,
            ))
    });
    let mut end = div()
        .flex_1()
        .min_w_0()
        .flex()
        .items_center()
        .justify_end()
        .gap(Pixels::from(css::END_GAP))
        .children(voice)
        .child(bell(model, handlers.on_reminders.clone(), reminders))
        .child(
            interactive(div().id("topbar-settings"))
                .a11y(Role::Button, t("shell.rail.settings"))
                .tab_index(0)
                .cursor_pointer()
                .rounded(radius::MD)
                .focus_visible(|s| s.shadow(focus_ring()))
                .tooltip(tooltip(t("shell.rail.settings"), &[]))
                .tooltip_show_delay(SHOW_DELAY)
                .on_click(move |_: &ClickEvent, window, cx| on_settings(window, cx))
                .child(centred(
                    Pixels::from(css::BUTTON),
                    Icon::new(IconName::Settings)
                        .size(Pixels::from(css::BUTTON_GLYPH))
                        .color(color::FG_2),
                )),
        );
    if model.controls && !cfg!(target_os = "macos") {
        end = end
            .child(window_button(
                "window-minimize",
                IconName::Minimize,
                MINIMIZE,
                false,
                |window, _| window.minimize_window(),
            ))
            .child(window_button(
                "window-maximize",
                IconName::Maximize,
                MAXIMIZE,
                false,
                |window, _| window.zoom_window(),
            ))
            .child(window_button(
                "window-close",
                IconName::X,
                CLOSE,
                true,
                |window, _| window.remove_window(),
            ));
    }
    let bar = div()
        .id("topbar")
        .a11y_unnamed(Role::Banner)
        .flex_none()
        .h(layout::TOPBAR)
        .bg(color::BG_2)
        .border_b_1()
        .border_color(color::HAIRLINE)
        .flex()
        .items_center()
        .pl(Pixels::from(left_inset(
            cfg!(target_os = "macos"),
            model.controls,
        )))
        .pr(Pixels::from(css::RIGHT))
        .gap(Pixels::from(css::GAP))
        .child(brand)
        .child(search)
        .child(end);
    #[cfg(feature = "automation")]
    let bar = bar
        .relative()
        .child(crate::automation::bounds_probe("shell-topbar"));
    if model.controls {
        // The bar is the window's drag region: a press moves the window, a double click maximises it.
        return bar
            .on_mouse_down(MouseButton::Left, |e: &MouseDownEvent, window, _| {
                if e.click_count == 2 {
                    window.zoom_window();
                } else {
                    window.start_window_move();
                }
            })
            .into_any_element();
    }
    bar.into_any_element()
}

/// The compact windows' own chrome (DesktopTitlebar.tsx in the `starting` and `authentication` window modes, every
/// platform): no top bar, a 26px drag strip along the top edge (short of the close button) and the 22px close button
/// in the top right corner. Drawn over the screen, as beta.5's fixed elements are.
pub fn compact_chrome() -> impl IntoElement {
    let drag = div()
        .id("window-drag")
        .absolute()
        .top_0()
        .left_0()
        .right(Pixels::from(css::CARD_DRAG_RIGHT))
        .h(Pixels::from(css::CARD_DRAG_H))
        // A compact window is not maximisable (beta.5 `setMaximizable(false)`): a press moves it, a double click does
        // nothing more.
        .on_mouse_down(MouseButton::Left, |_: &MouseDownEvent, window, cx| {
            cx.stop_propagation();
            window.start_window_move();
        });
    let close = interactive(div().id("window-card-close"))
        .a11y(Role::Button, CLOSE_LOGIN)
        .tab_index(0)
        .absolute()
        .top(Pixels::from(css::CARD_CLOSE_INSET))
        .right(Pixels::from(css::CARD_CLOSE_INSET))
        .size(Pixels::from(css::CARD_CLOSE))
        .flex()
        .items_center()
        .justify_center()
        .rounded(radius::SM)
        .cursor_pointer()
        .focus_visible(|s| s.shadow(focus_ring()))
        .hover(|s| s.bg(color::HOVER))
        .on_click(|_: &ClickEvent, window, _| window.remove_window())
        .child(
            Icon::new(IconName::X)
                .size(Pixels::from(css::WINDOW_GLYPH))
                .color(color::FG_2),
        );
    div()
        .absolute()
        .top_0()
        .left_0()
        .size_full()
        .child(drag)
        .child(close)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// beta.5 DesktopTitlebar.css: the macOS bar starts its brand at 112px, right of the traffic lights; elsewhere, and
    /// in a fixture without window controls, TopBar.css's 14px.
    #[test]
    fn the_bar_leaves_room_for_the_traffic_lights_on_macos() {
        assert_eq!(left_inset(true, true), 112.0);
        assert_eq!(left_inset(true, false), 14.0);
        assert_eq!(left_inset(false, true), 14.0);
        assert_eq!(left_inset(false, false), 14.0);
    }

    /// beta.5 DesktopTitlebar.css: the compact windows' drag strip (`inset: 0 34px auto 0`) stops short of the close
    /// button (22px, 6px in), so a press on the button is a click.
    #[test]
    fn the_compact_drag_strip_stops_short_of_the_close_button() {
        assert_eq!(
            (css::CARD_DRAG_RIGHT, css::CARD_DRAG_H),
            (34.0, 26.0),
            "the strip"
        );
        assert_eq!(
            (css::CARD_CLOSE, css::CARD_CLOSE_INSET),
            (22.0, 6.0),
            "the button: 28px from the right edge, inside the strip's 34px gap"
        );
    }
}
