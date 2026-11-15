//! `TrustAlert` (packages/ui/src/TrustAlert): a contact's trust alert, a danger [`super::super::banner::Banner`] whose
//! text starts with the contact's name in bold (`<strong>{name}</strong> {copy}`). Never dismissable. The Compare action
//! stands only when there is a key to compare.
//!
//! The shared `Banner` takes one plain text, so the alert draws the banner's own box (the same tokens, the 3px danger bar,
//! the x glyph) with the bold name run in front.

use gpui::{
    App, ClickEvent, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce,
    SharedString, Styled, Window, div, px,
};

use crate::components::banner::BannerTone;
use crate::components::button::Button;
use crate::icon::Icon;
use crate::metrics::Density;
use crate::text::{line, line_height};
use crate::tokens::{color, font, text};

type ClickHandler = Box<dyn Fn(&ClickEvent, &mut Window, &mut App) + 'static>;

#[derive(IntoElement)]
pub struct TrustAlert {
    id: ElementId,
    name: SharedString,
    copy: SharedString,
    action: Option<(SharedString, ClickHandler)>,
}

impl TrustAlert {
    /// `copy` is `privacy.changed` (or `privacy.changedNoKey`, `privacy.unlisted`), already translated.
    pub fn new(
        id: impl Into<ElementId>,
        name: impl Into<SharedString>,
        copy: impl Into<SharedString>,
    ) -> Self {
        Self {
            id: id.into(),
            name: name.into(),
            copy: copy.into(),
            action: None,
        }
    }

    /// The Compare action (`privacy.compare`), only when there is a key to compare.
    pub fn compare(
        mut self,
        label: impl Into<SharedString>,
        on_click: impl Fn(&ClickEvent, &mut Window, &mut App) + 'static,
    ) -> Self {
        self.action = Some((label.into(), Box::new(on_click)));
        self
    }
}

impl RenderOnce for TrustAlert {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let tone = BannerTone::Danger;
        let accent = tone.accent();
        let density = Density::Regular;
        let lh = line_height(text::SM, 1.5);
        let glyph = tone.glyph().map(|icon| {
            div()
                .flex_none()
                .child(Icon::new(icon).size(px(14.0)).color(accent))
        });
        let id = self.id.clone();
        let mut root = div()
            .id(self.id)
            .relative()
            .flex()
            .items_center()
            .gap(px(12.0))
            .w_full()
            .pl(density.row_pad_x() + px(3.0))
            .pr(density.row_pad_x())
            .py(px(8.0))
            .bg(color::BG_2)
            .border_b_1()
            .border_color(color::HAIRLINE_2)
            .children(glyph)
            .child(
                div()
                    .flex_1()
                    .min_w_0()
                    .flex()
                    .flex_wrap()
                    .child(line(self.name, text::SM, lh, color::FG).weight(font::WEIGHT_DISPLAY))
                    .child(line(format!(" {}", self.copy), text::SM, lh, color::FG)),
            )
            .child(
                div()
                    .absolute()
                    .left_0()
                    .top_0()
                    .bottom_0()
                    .w(px(3.0))
                    .bg(accent),
            );
        if let Some((label, handler)) = self.action {
            root = root.child(
                div()
                    .flex_none()
                    .child(Button::new((id, "compare"), label).on_click(handler)),
            );
        }
        root
    }
}
