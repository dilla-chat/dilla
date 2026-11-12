//! `ReactionBar` (packages/ui/src/ReactionBar): a message's reactions, one pressed-state chip per emoji, then the add
//! button. One roving tab stop across the chips and the add button (arrows wrap, Home and End jump); a chip toggles on a
//! click, Enter or Space. A pressed chip is the accent chip (README 6.5). The add button (a dashed 26 x 22 smile) shows
//! while the bar is under the pointer or holds focus, or when the host says so (the message row is hovered).
//!
//! The geometry is the CSS's (ReactionBar.css): 22px chips with a radius of `--r-row`, 7px sides, a 6px gap between the
//! emoji and the count, 4px between chips and between wrapped lines. [`wrap_lines`] and [`bar_height`] are the same
//! arithmetic as pure functions, so a host can lay out the bar's height before it is drawn.

use std::rc::Rc;

use gpui::{
    AnyElement, App, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce, SharedString,
    StatefulInteractiveElement, Styled, Window, div, px,
};

use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::focus_ring;
use crate::roving::{Roving, focus_handles};
use crate::text::{MONO_ADVANCE, line, line_height};
use crate::tokens::{color, font, radius, text};

/// A chip's height (`1.375rem`).
pub const CHIP_HEIGHT: f32 = 22.0;
/// A chip's side padding (`0.4375rem`) and its border.
pub const CHIP_PAD_X: f32 = 7.0;
pub const CHIP_BORDER: f32 = 1.0;
/// The gap between a chip's emoji and its count (`0.375rem`).
pub const CHIP_INNER_GAP: f32 = 6.0;
/// The gap between chips, and between wrapped lines (`0.25rem`), and the bar's margin above.
pub const BAR_GAP: f32 = 4.0;
pub const BAR_MARGIN_TOP: f32 = 4.0;
/// The add button's width (`1.625rem`).
pub const ADD_WIDTH: f32 = 26.0;

/// One reaction chip.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Chip {
    /// The emoji as the person sees it (`reactionDisplay`).
    pub display: SharedString,
    pub count: u32,
    /// This account reacted with it: the pressed accent chip.
    pub mine: bool,
    /// The accessible name, `{name}, {count}`.
    pub name: SharedString,
    /// What a toggle sends (the raw emoji).
    pub emoji: SharedString,
}

/// A chip's width from the width of its emoji glyph and the digits of its count (`tabular-nums` mono).
pub fn chip_width(emoji_width: f32, count: u32) -> f32 {
    let digits = count.to_string().len() as f32;
    2.0 * CHIP_BORDER
        + 2.0 * CHIP_PAD_X
        + emoji_width
        + CHIP_INNER_GAP
        + digits * f32::from(text::META) * MONO_ADVANCE
}

/// How many lines `flex-wrap: wrap` makes of items of `widths` (in order) in `available` px, with [`BAR_GAP`] between
/// them. An item wider than a line sits alone on one; no items make no lines.
pub fn wrap_lines(widths: &[f32], available: f32) -> usize {
    let mut lines = 0;
    let mut used = 0.0_f32;
    for &w in widths {
        if lines == 0 {
            lines = 1;
            used = w;
        } else if used + BAR_GAP + w <= available + 1e-3 {
            used += BAR_GAP + w;
        } else {
            lines += 1;
            used = w;
        }
    }
    lines
}

/// The bar's height with `lines` lines: the margin above, the chips' lines and the gaps between lines.
pub fn bar_height(lines: usize) -> f32 {
    if lines == 0 {
        return 0.0;
    }
    BAR_MARGIN_TOP + lines as f32 * CHIP_HEIGHT + (lines - 1) as f32 * BAR_GAP
}

/// Which element of the bar a decorator wraps.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Slot {
    Chip(usize),
    Add,
}

/// Wraps an element of the bar, given the focus handle it holds (the host's automation ids).
pub type Decorate = Rc<dyn Fn(Slot, &gpui::FocusHandle, AnyElement) -> AnyElement>;
type OnToggle = Rc<dyn Fn(&str, &mut Window, &mut App)>;
type OnAdd = Rc<dyn Fn(&mut Window, &mut App)>;

#[derive(IntoElement)]
pub struct ReactionBar {
    id: ElementId,
    label: SharedString,
    add_label: SharedString,
    chips: Vec<Chip>,
    tabbable: bool,
    revealed: bool,
    on_toggle: Option<OnToggle>,
    on_add: Option<OnAdd>,
    decorate: Option<Decorate>,
}

impl ReactionBar {
    pub fn new(
        id: impl Into<ElementId>,
        label: impl Into<SharedString>,
        add_label: impl Into<SharedString>,
        chips: Vec<Chip>,
    ) -> Self {
        Self {
            id: id.into(),
            label: label.into(),
            add_label: add_label.into(),
            chips,
            tabbable: true,
            revealed: false,
            on_toggle: None,
            on_add: None,
            decorate: None,
        }
    }

    /// False: no element of the bar is a tab stop (its row is not the active one).
    pub fn tabbable(mut self, tabbable: bool) -> Self {
        self.tabbable = tabbable;
        self
    }

    /// True: the add button shows although the bar is neither hovered nor focused (the row is).
    pub fn revealed(mut self, revealed: bool) -> Self {
        self.revealed = revealed;
        self
    }

    pub fn on_toggle(mut self, f: impl Fn(&str, &mut Window, &mut App) + 'static) -> Self {
        self.on_toggle = Some(Rc::new(f));
        self
    }

    pub fn on_add(mut self, f: impl Fn(&mut Window, &mut App) + 'static) -> Self {
        self.on_add = Some(Rc::new(f));
        self
    }

    pub fn decorate(mut self, f: Decorate) -> Self {
        self.decorate = Some(f);
        self
    }

    /// The label of the group.
    pub fn label(&self) -> &SharedString {
        &self.label
    }
}

fn chip_view(
    id: ElementId,
    chip: &Chip,
    handle: &gpui::FocusHandle,
    tab: bool,
    on_toggle: Option<OnToggle>,
) -> impl IntoElement {
    let (bg, edge, count) = if chip.mine {
        (color::ACCENT_CHIP, color::ACCENT, color::ACCENT_2)
    } else {
        (color::SURFACE_2, color::HAIRLINE_2, color::FG_2)
    };
    let emoji = chip.emoji.clone();
    div()
        .id(id)
        .track_focus(handle)
        .tab_stop(tab)
        .flex()
        .flex_none()
        .items_center()
        .gap(px(CHIP_INNER_GAP))
        .h(px(CHIP_HEIGHT))
        .px(px(CHIP_PAD_X))
        .border_1()
        .border_color(edge)
        .bg(bg)
        .rounded(radius::ROW)
        .cursor_pointer()
        .focus_visible(|s| s.shadow(focus_ring()))
        .child(line(
            chip.display.clone(),
            text::BTN,
            line_height(text::BTN, 1.0),
            color::FG,
        ))
        .child(
            line(
                chip.count.to_string(),
                text::META,
                line_height(text::META, 1.0),
                count,
            )
            .weight(font::WEIGHT_LABEL),
        )
        .on_click(move |_, window, cx| {
            if let Some(f) = &on_toggle {
                f(&emoji, window, cx);
            }
        })
}

impl RenderOnce for ReactionBar {
    fn render(self, window: &mut Window, cx: &mut App) -> impl IntoElement {
        let n = self.chips.len();
        let handles = focus_handles((self.id.clone(), "stops"), n + 1, window, cx);
        // The one tab stop follows focus (`stop` in ReactionBar.tsx).
        let memory = window.use_keyed_state((self.id.clone(), "stop"), cx, |_, _| 0usize);
        let focused = handles.iter().position(|h| h.is_focused(window));
        if let Some(i) = focused {
            if *memory.read(cx) != i {
                memory.update(cx, |m, _| *m = i);
            }
        }
        let stop = Roving::stop(focused, Some(*memory.read(cx)), n + 1);
        let tab = |i: usize| self.tabbable && i == stop;
        let group: SharedString = format!("{:?}-bar", self.id).into();

        let mut row = div()
            .id((self.id.clone(), "bar"))
            .group(group.clone())
            .flex()
            .flex_wrap()
            .gap(px(BAR_GAP))
            .mt(px(BAR_MARGIN_TOP));
        for (i, chip) in self.chips.iter().enumerate() {
            let view = chip_view(
                (self.id.clone(), format!("chip-{i}")).into(),
                chip,
                &handles[i],
                tab(i),
                self.on_toggle.clone(),
            )
            .into_any_element();
            row = row.child(match &self.decorate {
                Some(d) => d(Slot::Chip(i), &handles[i], view),
                None => view,
            });
        }
        let shown = self.revealed || focused.is_some();
        let on_add = self.on_add.clone();
        let add = div()
            .id((self.id.clone(), "add"))
            .track_focus(&handles[n])
            .tab_stop(tab(n))
            .flex()
            .flex_none()
            .items_center()
            .justify_center()
            .w(px(ADD_WIDTH))
            .h(px(CHIP_HEIGHT))
            .border_1()
            .border_dashed()
            .border_color(color::LINE_STRONG)
            .rounded(radius::ROW)
            .cursor_pointer()
            .opacity(if shown { 1.0 } else { 0.0 })
            .group_hover(group, |s| s.opacity(1.0))
            .hover(|s| s.border_color(color::ACCENT))
            .focus_visible(|s| s.shadow(focus_ring()))
            .tooltip(super::super::tooltip::tooltip(self.add_label.clone(), &[]))
            .tooltip_show_delay(super::super::tooltip::SHOW_DELAY)
            .child(Icon::new(IconName::SmilePlus).size(px(12.0)).color(color::FG_3))
            .on_click(move |_, window, cx| {
                if let Some(f) = &on_add {
                    f(window, cx);
                }
            })
            .into_any_element();
        row = row.child(match &self.decorate {
            Some(d) => d(Slot::Add, &handles[n], add),
            None => add,
        });
        let key_handles = handles;
        row.on_key_down(move |event, window, cx| {
            let from = key_handles.iter().position(|h| h.is_focused(window));
            let Some(from) = from else { return };
            if let Some(next) =
                (Roving::Radio { both: false }).next(&event.keystroke, from, key_handles.len())
            {
                cx.stop_propagation();
                key_handles[next].focus(window, cx);
            }
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_chip_is_its_content_plus_the_css_padding() {
        // 1px borders, 7px sides, the emoji, 6px, one digit of 10.5px mono.
        let w = chip_width(14.0, 3);
        assert!((w - (2.0 + 14.0 + 14.0 + 6.0 + 6.3)).abs() < 1e-4, "{w}");
        // Two digits take one more advance.
        assert!((chip_width(14.0, 12) - w - 6.3).abs() < 1e-4);
    }

    #[test]
    fn chips_wrap_like_a_flex_row() {
        assert_eq!(wrap_lines(&[], 100.0), 0);
        // 30 + 4 + 30 + 4 + 30 = 98 fits in 100; a fourth does not.
        assert_eq!(wrap_lines(&[30.0, 30.0, 30.0], 100.0), 1);
        assert_eq!(wrap_lines(&[30.0, 30.0, 30.0, 30.0], 100.0), 2);
        // An item wider than the line sits alone.
        assert_eq!(wrap_lines(&[200.0, 10.0], 100.0), 2);
        // The first item never starts a line of its own just because it is wide.
        assert_eq!(wrap_lines(&[200.0], 100.0), 1);
    }

    #[test]
    fn the_bar_is_its_margin_its_lines_and_their_gaps() {
        assert_eq!(bar_height(0), 0.0);
        assert_eq!(bar_height(1), 4.0 + 22.0);
        assert_eq!(bar_height(2), 4.0 + 44.0 + 4.0);
        assert_eq!(bar_height(3), 4.0 + 66.0 + 8.0);
    }
}
