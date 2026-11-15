//! The reply line of a sent reply (MessageRow.css `.d-message-row__reply`, §6.5): a 2px connector curve out of the avatar
//! column, the 14px mini avatar, the original's author in bold and one line of its text. A reply to a deleted or missing
//! message shows its state in place of the author and text.

use std::rc::Rc;

use gpui::{
    App, ClickEvent, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce,
    SharedString, StatefulInteractiveElement, Styled, Window, div, px,
};

use crate::components::{Avatar, AvatarSize};
use crate::text::{line, line_height};
use crate::tokens::{color, font, text};

/// What the reply points at.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ReplyState {
    /// The original is held: author and excerpt.
    Ok,
    /// Not held on this device.
    Missing,
    /// Deleted.
    Deleted,
}

/// `.d-message-row__reply`: gap 0.375rem, margin 0 0 0.125rem.
const GAP: f32 = 6.0;
const MARGIN_BOTTOM: f32 = 2.0;
/// The 2px connector: from 26px left of the content, 9px down; 20x11, its top-left corner rounded 6px.
const CURVE_LEFT: f32 = -26.0;
const CURVE_TOP: f32 = 9.0;
const CURVE_W: f32 = 20.0;
const CURVE_H: f32 = 11.0;
const CURVE_BORDER: f32 = 2.0;
const CURVE_RADIUS: f32 = 6.0;

/// The line's own height: the text's line (`--text-btn` at the row's 1.5) or the 14px avatar, whichever is taller.
pub fn line_box() -> f32 {
    line_height(text::BTN, 1.5).max(f32::from(AvatarSize::Mini.metrics().0))
}

/// What the reply line takes in a row: its box and its bottom margin. The row's avatar and gutter move down by the same
/// amount the CSS gives them (`data-reply`: 20px and 38px, the line, its margin and the grid's 1px gap).
pub fn height() -> f32 {
    line_box() + MARGIN_BOTTOM
}

type Jump = Rc<dyn Fn(&ClickEvent, &mut Window, &mut App)>;

#[derive(IntoElement)]
pub struct ReplyLine {
    id: ElementId,
    author: SharedString,
    text: SharedString,
    state: ReplyState,
    on_jump: Option<Jump>,
}

impl ReplyLine {
    /// `text` is the original's excerpt for [`ReplyState::Ok`], else the state's sentence.
    pub fn new(
        id: impl Into<ElementId>,
        author: impl Into<SharedString>,
        text: impl Into<SharedString>,
        state: ReplyState,
    ) -> Self {
        Self {
            id: id.into(),
            author: author.into(),
            text: text.into(),
            state,
            on_jump: None,
        }
    }

    /// The line is a button that jumps to the original (a held or deleted original only).
    pub fn on_jump(mut self, f: impl Fn(&ClickEvent, &mut Window, &mut App) + 'static) -> Self {
        self.on_jump = Some(Rc::new(f));
        self
    }
}

impl RenderOnce for ReplyLine {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let size = text::BTN;
        let lh = line_height(size, 1.5);
        let curve = div()
            .absolute()
            .left(px(CURVE_LEFT))
            .top(px(CURVE_TOP))
            .w(px(CURVE_W))
            .h(px(CURVE_H))
            .border_l(px(CURVE_BORDER))
            .border_t(px(CURVE_BORDER))
            .border_color(color::LINE_STRONG)
            .rounded_tl(px(CURVE_RADIUS));
        let body = match self.state {
            ReplyState::Ok => div()
                .flex()
                .items_center()
                .gap(px(GAP))
                .min_w_0()
                .child(Avatar::new("reply-avatar", self.author.clone()).size(AvatarSize::Mini))
                .child(
                    line(self.author.clone(), size, lh, color::FG_STRONG)
                        .weight(font::WEIGHT_DISPLAY),
                )
                .child(line(self.text.clone(), size, lh, color::FG_2).ellipsis()),
            ReplyState::Missing | ReplyState::Deleted => div()
                .flex()
                .items_center()
                .gap(px(GAP))
                .min_w_0()
                .child(line(self.text.clone(), size, lh, color::FG_3).ellipsis()),
        };
        let can_jump = self.on_jump.is_some() && self.state != ReplyState::Missing;
        let mut root = div()
            .id(self.id)
            .relative()
            .flex()
            .items_center()
            .min_w_0()
            .h(px(line_box()))
            .mb(px(MARGIN_BOTTOM))
            .child(curve)
            .child(body);
        if can_jump && let Some(jump) = self.on_jump {
            root = root
                .cursor_pointer()
                .on_click(move |e, window, cx| jump(e, window, cx));
        }
        root
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_line_is_the_buttons_text_line_and_a_margin() {
        // 11.5px at 1.5 is 17.25 (layout units), taller than the 14px avatar; the margin adds 2.
        assert_eq!(line_box(), 17.25);
        assert_eq!(height(), 19.25);
    }
}
