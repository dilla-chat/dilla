//! The reply bar over the composer's box (ReplyChip.css, §5.5): "↳ Replying to **Ada** · excerpt" and ×. It sits on the
//! box: margin −1px, no bottom edge, 8px top corners.

use std::rc::Rc;

use gpui::{
    AnyElement, App, ClickEvent, ElementId, InteractiveElement, IntoElement, ParentElement,
    RenderOnce, SharedString, StatefulInteractiveElement, Styled, Window, div, px,
};

use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::text::{line, line_height};
use crate::tokens::{color, font, radius, text};

/// `padding: 0.375rem 0.625rem`, `gap: 0.5rem`, the glyphs 12px and 13px.
const PAD_Y: f32 = 6.0;
const PAD_X: f32 = 10.0;
const GAP: f32 = 8.0;
const GLYPH: f32 = 12.0;
const CANCEL: f32 = 13.0;

/// The bar's height in the composer's flow: the text's line and the two paddings (the 1px edges overlap the box).
pub fn height() -> f32 {
    line_height(text::BTN, 1.5) + 2.0 * PAD_Y
}

type Cancel = Rc<dyn Fn(&ClickEvent, &mut Window, &mut App)>;

#[derive(IntoElement)]
pub struct ReplyBar {
    id: ElementId,
    before: SharedString,
    name: SharedString,
    after: SharedString,
    excerpt: SharedString,
    cancel_id: ElementId,
    on_cancel: Option<Cancel>,
    wrap_cancel: Option<fn(AnyElement) -> AnyElement>,
}

impl ReplyBar {
    /// `label` is the whole "Replying to Ada" and `name` the person inside it, set in bold.
    pub fn new(
        id: impl Into<ElementId>,
        cancel_id: impl Into<ElementId>,
        label: &str,
        name: &str,
        excerpt: impl Into<SharedString>,
    ) -> Self {
        let (before, name, after) = match label.rfind(name).filter(|_| !name.is_empty()) {
            Some(at) => (
                label[..at].to_owned(),
                name.to_owned(),
                label[at + name.len()..].to_owned(),
            ),
            None => (label.to_owned(), String::new(), String::new()),
        };
        Self {
            id: id.into(),
            before: before.into(),
            name: name.into(),
            after: after.into(),
            excerpt: excerpt.into(),
            cancel_id: cancel_id.into(),
            on_cancel: None,
            wrap_cancel: None,
        }
    }

    /// Wraps the × button (the screen's automation probe).
    pub fn wrap_cancel(mut self, wrap: fn(AnyElement) -> AnyElement) -> Self {
        self.wrap_cancel = Some(wrap);
        self
    }

    pub fn on_cancel(mut self, f: impl Fn(&ClickEvent, &mut Window, &mut App) + 'static) -> Self {
        self.on_cancel = Some(Rc::new(f));
        self
    }
}

impl RenderOnce for ReplyBar {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let size = text::BTN;
        let lh = line_height(size, 1.5);
        let mut cancel = div()
            .id(self.cancel_id)
            .flex()
            .flex_none()
            .cursor_pointer()
            .child(Icon::new(IconName::X).size(px(CANCEL)).color(color::FG_2));
        if let Some(f) = self.on_cancel {
            cancel = cancel.on_click(move |e, window, cx| f(e, window, cx));
        }
        let mut label = div().flex().items_center().min_w_0().overflow_hidden();
        label = label.child(line(self.before, size, lh, color::FG_2).clip());
        if !self.name.is_empty() {
            label = label.child(
                line(self.name, size, lh, color::FG)
                    .weight(font::WEIGHT_DISPLAY)
                    .clip(),
            );
        }
        if !self.after.is_empty() {
            label = label.child(line(self.after, size, lh, color::FG_2).clip());
        }
        if !self.excerpt.is_empty() {
            label =
                label.child(line(format!(" · {}", self.excerpt), size, lh, color::FG_3).ellipsis());
        }
        div()
            .id(self.id)
            .flex()
            .items_center()
            .gap(px(GAP))
            .min_w_0()
            .px(px(PAD_X))
            .py(px(PAD_Y))
            .mb(px(-1.0))
            .bg(color::SURFACE_2)
            .border_1()
            .border_b_0()
            .border_color(color::HAIRLINE_2)
            .rounded_t(radius::LG)
            .child(
                Icon::new(IconName::Reply)
                    .size(px(GLYPH))
                    .color(color::FG_2),
            )
            .child(div().flex_1().min_w_0().overflow_hidden().child(label))
            .child(match self.wrap_cancel {
                Some(wrap) => wrap(cancel.into_any_element()),
                None => cancel.into_any_element(),
            })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_bar_is_a_line_between_two_paddings() {
        assert_eq!(height(), 17.25 + 12.0);
    }

    #[test]
    fn the_name_is_cut_out_of_the_label() {
        let bar = ReplyBar::new("b", "c", "Replying to Ada", "Ada", "hi");
        assert_eq!(
            (bar.before.as_ref(), bar.name.as_ref(), bar.after.as_ref()),
            ("Replying to ", "Ada", "")
        );
        let bare = ReplyBar::new("b", "c", "Replying", "", "");
        assert_eq!((bare.before.as_ref(), bare.name.as_ref()), ("Replying", ""));
    }
}
