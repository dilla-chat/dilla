//! The search palette's parts (`packages/ui/src/SearchPalette/SearchPalette.{tsx,css}`, Signal §5.8): the 50px input row's
//! pieces, the group heading, the result row (a 24px glyph tile or avatar, the label over its sub-line, the selected row on
//! `--surface-hi` with a 2px accent bar and ↵), the status line and the footer. The screen (`dilla_desktop::screens::search`)
//! owns the input, the focus and the list, and composes these in the palette's panel; this file is layout only and
//! draws no English (every word comes in as an argument).

use std::rc::Rc;

use gpui::{
    AnyElement, App, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce,
    SharedString, StatefulInteractiveElement, Styled, Window, div, px,
};

use crate::components::avatar::{Avatar, AvatarShape, AvatarSize};
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::{LABEL_TRACKING_WIDE, focus_ring, shadow_3};
use crate::text::{line, line_height};
use crate::tokens::{color, font, radius, text};

/// `.d-search-palette-dialog`: `inset: 14vh 0 auto 0`.
pub const TOP_VH: f32 = 0.14;
/// `width: min(600px, 92%)`.
pub const WIDTH: f32 = 600.0;
/// The 92 % that caps the width in a narrow window.
pub const WIDTH_SHARE: f32 = 0.92;
/// `.d-search-palette-dialog > .d-search-palette { max-height: calc(100dvh - 14vh - 16px) }`: the 16px.
pub const BOTTOM_GAP: f32 = 16.0;
/// `.d-search-palette__results { max-height: 392px }`.
pub const RESULTS_MAX: f32 = 392.0;
/// `.d-search-palette__results { padding: 6px }`.
pub const RESULTS_PAD: f32 = 6.0;
/// `.d-search-palette__field { height: 50px }` (content box: the hairline under it is outside).
pub const FIELD_H: f32 = 50.0;
/// The textbox's height inside the field.
pub const INPUT_H: f32 = 40.0;
/// A result row's content height (`min-height: 38px`, content box: 4px padding above and below).
pub const ROW_CONTENT_H: f32 = 38.0;
/// A result row's padding above and below.
pub const ROW_PAD_Y: f32 = 4.0;
/// A result row's padding to its sides.
pub const ROW_PAD_X: f32 = 10.0;
/// The glyph tile and the avatar.
pub const TILE: f32 = 24.0;
/// A row's text block: `line-height: 1.35`.
pub const TEXT_LEADING: f32 = 1.35;

/// The palette's width in a window `viewport_w` wide.
pub fn panel_width(viewport_w: f32) -> f32 {
    WIDTH.min(viewport_w * WIDTH_SHARE)
}

/// The palette's offset from the top of a window `viewport_h` high.
pub fn top_offset(viewport_h: f32) -> f32 {
    viewport_h * TOP_VH
}

/// The panel's tallest: what is left of the window under its offset, less the 16px.
pub fn panel_max_height(viewport_h: f32) -> f32 {
    viewport_h - top_offset(viewport_h) - BOTTOM_GAP
}

/// What a result row shows at its left.
#[derive(Clone, Debug)]
pub enum Glyph {
    /// A 24px tile with a 13px icon (a channel, a message, a setting, an action).
    Icon(IconName),
    /// A 24px avatar (a person).
    Avatar { name: SharedString, bot: bool },
}

/// The item after (`step` 1) or before (`step` -1) the active one in `ids`, wrapping at the ends: the palette's arrows
/// (`handleInputKey`). With no active item (`None`, or one that left the list) the first is active, so a step goes from it.
pub fn step_active(ids: &[SharedString], active: Option<&str>, step: i32) -> Option<usize> {
    if ids.is_empty() {
        return None;
    }
    let len = ids.len() as i32;
    let at = active
        .and_then(|a| ids.iter().position(|id| id.as_ref() == a))
        .unwrap_or(0) as i32;
    Some((at + step).rem_euclid(len) as usize)
}

/// The scrim over the window with the palette's column in it (`.d-search-palette-dialog` and its `::backdrop`): `14vh` from
/// the top, the panel centred. The caller adds the panel and the handlers, and draws it as an overlay.
pub fn scrim(id: impl Into<ElementId>, viewport_h: f32) -> gpui::Stateful<gpui::Div> {
    div()
        .id(id)
        .absolute()
        .inset_0()
        .flex()
        .flex_col()
        .items_center()
        .pt(px(top_offset(viewport_h)))
        .bg(color::SCRIM)
        .occlude()
}

/// The panel (`.d-search-palette`): `--surface` in the `--line-strong` edge, 12px corners, the `--shadow-3`, at most 600px
/// wide and as high as the window leaves it. The caller adds the focus, the key context, the handlers and the three parts.
pub fn panel(
    id: impl Into<ElementId>,
    viewport_w: f32,
    viewport_h: f32,
) -> gpui::Stateful<gpui::Div> {
    div()
        .id(id)
        .flex()
        .flex_col()
        .flex_none()
        .w(px(panel_width(viewport_w)))
        .max_h(px(panel_max_height(viewport_h)))
        .bg(color::SURFACE)
        .border_1()
        .border_color(color::LINE_STRONG)
        .rounded(radius::XXL)
        .shadow(shadow_3())
        .overflow_hidden()
}

/// The input row (`.d-search-palette__field`): 50px, 0 14px, a 10px gap, a hairline under it (outside the 50px).
pub fn field() -> gpui::Div {
    div()
        .flex_none()
        .flex()
        .items_center()
        .gap(px(10.0))
        .h(px(FIELD_H + 1.0))
        .px(px(14.0))
        .border_b_1()
        .border_color(color::HAIRLINE)
}

/// The accent search glyph at the row's left.
pub fn field_glyph() -> impl IntoElement {
    div().flex_none().child(
        Icon::new(IconName::Search)
            .size(px(16.0))
            .color(color::ACCENT),
    )
}

/// The textbox's box (`.d-search-palette input`: 40px high, the 14px text): the caller puts the input in it.
pub fn input_box() -> gpui::Div {
    div()
        .relative()
        .flex_1()
        .min_w_0()
        .h(px(INPUT_H))
        .text_size(text::MD)
        .font_family(font::MONO)
        .text_color(color::FG)
}

/// The hint over an empty textbox.
pub fn placeholder(label: impl Into<SharedString>) -> impl IntoElement {
    div()
        .absolute()
        .left_0()
        .top_0()
        .h(px(INPUT_H))
        .flex()
        .items_center()
        .child(line(
            label,
            text::MD,
            line_height(text::MD, 1.5),
            color::FG_3,
        ))
}

/// The esc key cap (`.d-search-palette__esc`): 0 6px, 18px lines in a 1px `--line-strong` edge. The caller adds the focus
/// and the handlers.
pub fn esc_key(
    id: impl Into<ElementId>,
    label: impl Into<SharedString>,
) -> gpui::Stateful<gpui::Div> {
    div()
        .id(id)
        .flex_none()
        .h(px(20.0))
        .px(px(6.0))
        .border_1()
        .border_color(color::LINE_STRONG)
        .rounded(radius::SM)
        .flex()
        .items_center()
        .cursor_pointer()
        .focus_visible(|s| s.shadow(focus_ring()))
        .child(line(label, text::MICRO, 18.0, color::FG_2))
}

/// The results region (`.d-search-palette__results`): at most 392px with 6px in, scrolling. The caller adds the scroll
/// handle and the focus.
pub fn results(id: impl Into<ElementId>) -> gpui::Stateful<gpui::Div> {
    div()
        .id(id)
        .flex()
        .flex_col()
        .flex_shrink(1.0)
        .min_h_0()
        .max_h(px(RESULTS_MAX))
        .overflow_y_scroll()
        .p(px(RESULTS_PAD))
        .focus_visible(|s| s.shadow(focus_ring()))
}

/// The group heading (`.d-search-palette__group`): 10px uppercase at `--label-tracking-wide`, `--fg-3`, 8px 10px 4px.
pub fn heading(label: impl Into<SharedString>) -> impl IntoElement {
    let label: SharedString = label.into();
    div()
        .flex_none()
        .px(px(ROW_PAD_X))
        .pt(px(8.0))
        .pb(px(4.0))
        .child(
            line(
                label.to_uppercase(),
                text::MICRO,
                line_height(text::MICRO, 1.5),
                color::FG_3,
            )
            .weight(font::WEIGHT_LABEL)
            .tracking(LABEL_TRACKING_WIDE),
        )
}

type OnRow = Rc<dyn Fn(&mut Window, &mut App)>;

/// A result: the glyph, the label over its sub-line and, when selected, the ↵.
#[derive(IntoElement)]
pub struct ResultRow {
    id: ElementId,
    glyph: Glyph,
    label: SharedString,
    sub: Option<SharedString>,
    selected: bool,
    on_click: Option<OnRow>,
    on_hover: Option<OnRow>,
}

impl ResultRow {
    pub fn new(id: impl Into<ElementId>, glyph: Glyph, label: impl Into<SharedString>) -> Self {
        Self {
            id: id.into(),
            glyph,
            label: label.into(),
            sub: None,
            selected: false,
            on_click: None,
            on_hover: None,
        }
    }

    pub fn subline(mut self, sub: Option<SharedString>) -> Self {
        self.sub = sub.filter(|s| !s.is_empty());
        self
    }

    pub fn selected(mut self, selected: bool) -> Self {
        self.selected = selected;
        self
    }

    pub fn on_click(mut self, f: impl Fn(&mut Window, &mut App) + 'static) -> Self {
        self.on_click = Some(Rc::new(f));
        self
    }

    /// The pointer entered the row (the web's `onMouseEnter`).
    pub fn on_hover(mut self, f: impl Fn(&mut Window, &mut App) + 'static) -> Self {
        self.on_hover = Some(Rc::new(f));
        self
    }
}

impl RenderOnce for ResultRow {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let glyph: AnyElement = match self.glyph {
            Glyph::Icon(name) => div()
                .flex_none()
                .size(px(TILE))
                .flex()
                .items_center()
                .justify_center()
                .rounded(radius::MD)
                .bg(color::HOVER)
                .child(Icon::new(name).size(px(13.0)).color(color::FG_2))
                .into_any_element(),
            Glyph::Avatar { name, bot } => div()
                .flex_none()
                .child(
                    Avatar::new((self.id.clone(), "avatar"), name)
                        .size(AvatarSize::Xs)
                        .shape(if bot {
                            AvatarShape::Bot
                        } else {
                            AvatarShape::Circle
                        }),
                )
                .into_any_element(),
        };
        let mut texts = div().flex().flex_col().flex_1().min_w_0().child(
            line(
                self.label,
                text::SM,
                line_height(text::SM, TEXT_LEADING),
                color::FG,
            )
            .ellipsis(),
        );
        if let Some(sub) = self.sub {
            texts = texts.child(
                line(
                    sub,
                    text::META,
                    line_height(text::META, TEXT_LEADING),
                    if self.selected {
                        color::FG_2
                    } else {
                        color::FG_3
                    },
                )
                .ellipsis(),
            );
        }
        let mut row = div()
            .id(self.id)
            .relative()
            .flex()
            .flex_none()
            .items_center()
            .gap(px(10.0))
            .min_h(px(ROW_CONTENT_H + 2.0 * ROW_PAD_Y))
            .px(px(ROW_PAD_X))
            .py(px(ROW_PAD_Y))
            .rounded(radius::SEG)
            .overflow_hidden()
            .cursor_pointer()
            .child(glyph)
            .child(texts);
        if self.selected {
            row = row
                .bg(color::SURFACE_HI)
                // `box-shadow: inset 2px 0 0 var(--accent)`: a 2px edge that follows the row's rounded corners.
                .child(
                    div()
                        .absolute()
                        .inset_0()
                        .rounded(radius::SEG)
                        .border_l(px(2.0))
                        .border_color(color::ACCENT),
                )
                .child(line(
                    "↵",
                    text::MICRO,
                    line_height(text::MICRO, 1.5),
                    color::ACCENT,
                ));
        }
        if let Some(f) = self.on_click {
            row = row.on_click(move |_, window, cx| f(window, cx));
        }
        if let Some(f) = self.on_hover {
            row = row.on_hover(move |entered, window, cx| {
                if *entered {
                    f(window, cx);
                }
            });
        }
        row
    }
}

/// The status line (`.d-search-palette__status`): 28px in, centred, 12px `--fg-3`; nothing at all while it is empty.
pub fn status(text_: impl Into<SharedString>) -> Option<impl IntoElement> {
    let text_: SharedString = text_.into();
    if text_.is_empty() {
        return None;
    }
    Some(
        div()
            .flex_none()
            .p(px(28.0))
            .flex()
            .justify_center()
            .child(line(
                text_,
                text::UI,
                line_height(text::UI, 1.5),
                color::FG_3,
            )),
    )
}

/// The footer (`.d-search-palette__foot`): the key words, then the note at the right, over a hairline.
pub fn foot(move_hint: &str, open_hint: &str, note: impl Into<SharedString>) -> impl IntoElement {
    let meta = |s: String| line(s, text::META, line_height(text::META, 1.5), color::FG_3);
    div()
        .flex_none()
        .flex()
        .items_center()
        .gap(px(14.0))
        .px(px(14.0))
        .py(px(8.0))
        .border_t_1()
        .border_color(color::HAIRLINE)
        .child(
            div()
                .flex()
                .flex_none()
                .gap(px(14.0))
                .child(meta(format!("↑↓ {move_hint}")))
                .child(meta(format!("↵ {open_hint}"))),
        )
        .child(div().flex_1())
        .child(
            div().min_w_0().flex().child(
                line(note, text::META, line_height(text::META, 1.5), color::FG_3).ellipsis(),
            ),
        )
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ids(n: usize) -> Vec<SharedString> {
        (0..n)
            .map(|i| SharedString::from(format!("r{i}")))
            .collect()
    }

    #[test]
    fn the_panel_is_600_wide_at_14vh_and_92_percent_in_a_narrow_window() {
        assert_eq!(panel_width(1280.0), 600.0);
        assert_eq!(panel_width(500.0), 460.0);
        assert!((top_offset(800.0) - 112.0).abs() < 1e-3);
        assert!((panel_max_height(800.0) - 672.0).abs() < 1e-3);
    }

    /// SearchPalette.test.tsx: arrows wrap and move from the active item; none active starts at the first.
    #[test]
    fn arrows_wrap_across_the_list_and_start_at_the_first() {
        let list = ids(3);
        assert_eq!(step_active(&list, None, 1), Some(1));
        assert_eq!(step_active(&list, Some("r2"), 1), Some(0));
        assert_eq!(step_active(&list, Some("r0"), -1), Some(2));
        // An active item that left the list is the first again (results arrived later).
        assert_eq!(step_active(&list, Some("gone"), 1), Some(1));
        assert_eq!(step_active(&[], None, 1), None);
    }
}
