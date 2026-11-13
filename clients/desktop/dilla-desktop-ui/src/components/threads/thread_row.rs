//! A message inside the thread pane (ThreadPane.css, Ruling AL, the prototype's thread messages): no time gutter, a 26px
//! avatar and 10px beside it the name (12px 700, always `--fg`) and its time (10px `--fg-faint`) over the 12.5px
//! `--fg-body` text. A follow-up by the same person within the grouping window has no avatar and no head.

use gpui::{
    ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce, SharedString, Styled,
    div, px,
};

use crate::components::avatar::{colour_of, initials_of};
use crate::text::{Face, chromium_baseline, line, line_height, paragraph};
use crate::tokens::{color, font, radius, text};

/// The avatar's box and the gap beside it (`grid-template-columns: 1.625rem minmax(0, 1fr)`, `column-gap: 10px`).
pub const AVATAR: f32 = 26.0;
pub const COLUMN_GAP: f32 = 10.0;
/// The log's padding (`.d-thread-pane .d-message-log { padding: 12px 14px 6px }`), and the root's 14px.
pub const SIDE: f32 = 14.0;
/// Between two messages by different people (`.d-message-row + .d-message-row:not([data-grouped]) { margin-top: 12px }`).
pub const ROW_GAP: f32 = 12.0;
/// A follow-up's `padding: 2px 0 0`.
pub const FOLLOW_PAD: f32 = 2.0;
/// The head's gap between the name and the time (`.d-message-row__head { gap: 0.5rem }`), and the main grid's 1px gap.
const HEAD_GAP: f32 = 8.0;
const MAIN_GAP: f32 = 1.0;

/// The text columns' width in a pane `pane_width` wide.
pub fn body_width(pane_width: f32) -> f32 {
    pane_width - 2.0 * SIDE - AVATAR - COLUMN_GAP
}

fn name_line() -> f32 {
    line_height(text::UI, 1.5)
}
fn time_line() -> f32 {
    line_height(text::MICRO, 1.5)
}
fn body_line() -> f32 {
    line_height(text::SM, 1.5)
}

/// The head's box and where the name and the time sit in it: a flex row aligned on the baseline, so the box is as high as
/// the taller ascent plus the deeper descent.
fn head() -> (f32, f32, f32) {
    let (name_h, time_h) = (name_line(), time_line());
    let name_base = chromium_baseline(Face::Mono, f32::from(text::UI), name_h);
    let time_base = chromium_baseline(Face::Mono, f32::from(text::MICRO), time_h);
    let above = name_base.max(time_base);
    let below = (name_h - name_base).max(time_h - time_base);
    (above + below, above - name_base, above - time_base)
}

/// A row's height with `lines` body lines (without the gap above it).
pub fn row_height(grouped: bool, lines: usize) -> f32 {
    let body = lines as f32 * body_line();
    if grouped {
        FOLLOW_PAD + body
    } else {
        head().0 + MAIN_GAP + body
    }
}

/// How a row is drawn.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum ThreadRowKind {
    #[default]
    Ok,
    /// Queued or in flight: `--fg-2`.
    Pending,
    /// The send failed: `--danger`.
    Failed,
    /// A note in the body's place (deleted, cannot be read): italic-toned `--fg-3`.
    Note,
}

#[derive(IntoElement)]
pub struct ThreadRow {
    id: ElementId,
    author: SharedString,
    time: SharedString,
    body: SharedString,
    grouped: bool,
    spaced: bool,
    kind: ThreadRowKind,
    bot: bool,
    width: f32,
}

impl ThreadRow {
    pub fn new(
        id: impl Into<ElementId>,
        author: impl Into<SharedString>,
        time: impl Into<SharedString>,
        body: impl Into<SharedString>,
        pane_width: f32,
    ) -> Self {
        Self {
            id: id.into(),
            author: author.into(),
            time: time.into(),
            body: body.into(),
            grouped: false,
            spaced: false,
            kind: ThreadRowKind::Ok,
            bot: false,
            width: body_width(pane_width),
        }
    }

    pub fn grouped(mut self, grouped: bool) -> Self {
        self.grouped = grouped;
        self
    }

    /// The 12px of space above a row that follows another one.
    pub fn spaced(mut self, spaced: bool) -> Self {
        self.spaced = spaced;
        self
    }

    pub fn kind(mut self, kind: ThreadRowKind) -> Self {
        self.kind = kind;
        self
    }

    /// A bot keeps its tone (`--accent-2`) in a thread.
    pub fn bot(mut self, bot: bool) -> Self {
        self.bot = bot;
        self
    }

    /// The lines the body wraps to.
    pub fn lines(body: &str, pane_width: f32) -> usize {
        paragraph(
            body,
            text::SM,
            body_line(),
            color::FG_BODY,
            body_width(pane_width),
        )
        .len()
    }
}

impl RenderOnce for ThreadRow {
    fn render(self, _window: &mut gpui::Window, _cx: &mut gpui::App) -> impl IntoElement {
        let tone = match self.kind {
            ThreadRowKind::Ok => color::FG_BODY,
            ThreadRowKind::Pending => color::FG_2,
            ThreadRowKind::Failed => color::DANGER,
            ThreadRowKind::Note => color::FG_3,
        };
        let lines = paragraph(&self.body, text::SM, body_line(), tone, self.width);
        let body = div().flex().flex_col().children(lines);
        let mut root = div().id(self.id).flex().gap(px(COLUMN_GAP)).min_w_0();
        if self.spaced && !self.grouped {
            root = root.mt(px(ROW_GAP));
        }
        if self.grouped {
            return root
                .pt(px(FOLLOW_PAD))
                .child(div().w(px(AVATAR)).flex_none())
                .child(div().flex_1().min_w_0().child(body));
        }
        let (palette_h, palette_s, palette_l) = colour_of(&self.author);
        let disc = if self.bot {
            color::BRAND
        } else {
            gpui::hsla(
                f32::from(palette_h) / 360.0,
                f32::from(palette_s) / 100.0,
                f32::from(palette_l) / 100.0,
                1.0,
            )
        };
        let avatar = div()
            .flex()
            .flex_none()
            .items_center()
            .justify_center()
            .size(px(AVATAR))
            .rounded(radius::AVATAR)
            .bg(disc)
            .child(
                line(
                    initials_of(&self.author),
                    text::CHIP,
                    line_height(text::CHIP, 1.5),
                    color::BRAND_INK,
                )
                .weight(font::WEIGHT_DISPLAY),
            );
        let (head_h, name_dy, time_dy) = head();
        let name_colour = if self.bot { color::ACCENT_2 } else { color::FG };
        let head = div()
            .relative()
            .flex()
            .h(px(head_h))
            .gap(px(HEAD_GAP))
            .child(div().pt(px(name_dy)).child(
                line(self.author, text::UI, name_line(), name_colour).weight(font::WEIGHT_DISPLAY),
            ))
            .child(div().pt(px(time_dy)).child(line(
                self.time,
                text::MICRO,
                time_line(),
                color::FG_FAINT,
            )));
        root.child(avatar).child(
            div()
                .flex_1()
                .min_w_0()
                .flex()
                .flex_col()
                .gap(px(MAIN_GAP))
                .child(head)
                .child(body),
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_body_column_is_the_pane_less_padding_avatar_and_gap() {
        assert_eq!(body_width(340.0), 340.0 - 28.0 - 26.0 - 10.0);
    }

    #[test]
    fn a_head_is_as_tall_as_the_name_and_the_time_aligned_on_their_baseline() {
        let (h, name_dy, time_dy) = head();
        assert!(h >= name_line().max(time_line()) - 1.0, "{h}");
        assert!(name_dy >= 0.0 && time_dy >= 0.0);
        assert!(
            name_dy == 0.0 || time_dy == 0.0,
            "one of them sits on the top"
        );
    }

    #[test]
    fn a_followup_has_no_head() {
        assert_eq!(row_height(true, 2), FOLLOW_PAD + 2.0 * 18.75);
        assert!(row_height(false, 2) > row_height(true, 2));
    }
}
