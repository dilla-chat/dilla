//! The top bar (packages/ui TopBar, Signal §5.1) and the window's own controls (packages/web DesktopTitlebar): the brand,
//! the search field, the bell and settings buttons, and, in the shipping app, the menu and the minimise, maximise and close
//! buttons. The bar is also the window's drag region (a press moves the window, a double click maximises it). A fixture
//! draws the bar without the window controls, as the web client does in a browser.

use gpui::{
    App, ClickEvent, InteractiveElement, IntoElement, MouseButton, MouseDownEvent, ParentElement,
    Pixels, StatefulInteractiveElement, Styled, Window, div,
};

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
    /// DesktopTitlebar.css: the window buttons are 28px with a 14px glyph; the menu button the same.
    pub const WINDOW_GLYPH: f32 = 14.0;
    /// The macOS traffic lights sit in the bar's left corner: the brand moves right of them.
    pub const TRAFFIC_LIGHTS: f32 = 64.0;
}

/// The window buttons' names (packages/web DesktopTitlebar.tsx holds them as literals; `strings.rs` has no key).
const MINIMIZE: &str = "Minimize window";
const MAXIMIZE: &str = "Maximize or restore window";
const CLOSE: &str = "Close window";

pub struct TitlebarModel {
    /// Draws the window's own buttons (and makes the bar a drag region): the shipping window, not a fixture.
    pub controls: bool,
    /// The ⌘K / Ctrl K hint in the search field.
    pub search_hint: &'static str,
}

#[derive(Clone)]
pub struct TitlebarHandlers {
    /// The settings button was pressed.
    pub on_settings: super::shell::Action,
}

fn window_button(
    id: &'static str,
    icon: IconName,
    label: &'static str,
    danger_hover: bool,
    run: impl Fn(&mut Window, &mut App) + 'static,
) -> impl IntoElement {
    div()
        .id(id)
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

pub fn render(model: &TitlebarModel, handlers: &TitlebarHandlers) -> impl IntoElement + use<> {
    let brand_name_line = line_height(text::XS, 1.5);
    let brand = div()
        .flex_1()
        .min_w_0()
        .flex()
        .items_center()
        .gap(Pixels::from(css::BRAND_GAP))
        .child(
            centred(
                Pixels::from(css::BRAND_MARK),
                line("D", text::XS, line_height(text::XS, 1.0), color::BRAND_INK)
                    .weight(font::WEIGHT_DISPLAY),
            )
            .rounded(radius::SM)
            .bg(color::BRAND),
        )
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
            div()
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
    let mut end = div()
        .flex_1()
        .min_w_0()
        .flex()
        .justify_end()
        .gap(Pixels::from(css::END_GAP))
        .child(centred(
            Pixels::from(css::BUTTON),
            Icon::new(IconName::Bell)
                .size(Pixels::from(css::BUTTON_GLYPH))
                .color(color::FG_2),
        ))
        .child(
            div()
                .id("topbar-settings")
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
        .flex_none()
        .h(layout::TOPBAR)
        .bg(color::BG_2)
        .border_b_1()
        .border_color(color::HAIRLINE)
        .flex()
        .items_center()
        .pl(Pixels::from(
            if cfg!(target_os = "macos") && model.controls {
                css::TRAFFIC_LIGHTS
            } else {
                css::LEFT
            },
        ))
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
