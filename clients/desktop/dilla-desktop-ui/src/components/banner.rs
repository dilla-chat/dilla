//! `Banner` (packages/ui/src/Banner): a one-line notice. Tone is never colour alone: a glyph carries it too.

use gpui::{
    App, ElementId, Hsla, InteractiveElement, IntoElement, ParentElement, RenderOnce, SharedString,
    Styled, Window, div, px,
};

use super::button::{Button, ClickHandler};
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::Density;
use crate::text::{line, line_height};
use crate::tokens::{color, text};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum BannerTone {
    Info,
    Warn,
    Danger,
}

impl BannerTone {
    /// The accent of the left edge and the glyph (`Banner.css`).
    pub fn accent(self) -> Hsla {
        match self {
            Self::Info => color::FG_3,
            Self::Warn => color::WARN,
            Self::Danger => color::DANGER,
        }
    }

    /// The glyph: a chevron for info, the alert triangle for warn, a cross for danger.
    pub fn glyph(self) -> Option<IconName> {
        match self {
            Self::Info => None,
            Self::Warn => Some(IconName::TriangleAlert),
            Self::Danger => Some(IconName::X),
        }
    }

    /// `role="status"` for info, `role="alert"` for warn and danger.
    pub fn is_alert(self) -> bool {
        self != Self::Info
    }
}

#[derive(IntoElement)]
pub struct Banner {
    id: ElementId,
    tone: BannerTone,
    text: SharedString,
    action: Option<(SharedString, ClickHandler)>,
    /// The density to step by; the window's ([`Density::of`]) when none is given.
    density: Option<Density>,
}

impl Banner {
    pub fn new(id: impl Into<ElementId>, tone: BannerTone, text: impl Into<SharedString>) -> Self {
        Self {
            id: id.into(),
            tone,
            text: text.into(),
            action: None,
            density: None,
        }
    }

    pub fn action(
        mut self,
        label: impl Into<SharedString>,
        on_click: impl Fn(&gpui::ClickEvent, &mut Window, &mut App) + 'static,
    ) -> Self {
        self.action = Some((label.into(), Box::new(on_click)));
        self
    }

    pub fn density(mut self, density: Density) -> Self {
        self.density = Some(density);
        self
    }
}

impl RenderOnce for Banner {
    fn render(self, window: &mut Window, cx: &mut App) -> impl IntoElement {
        let density = self.density.unwrap_or_else(|| Density::of(window, cx));
        // Banner.css sets no line height: the text takes the root's `--line-height`.
        let lh = line_height(text::SM, density.line_height());
        let accent = self.tone.accent();
        let glyph = match self.tone.glyph() {
            Some(icon) => div().child(Icon::new(icon).size(px(14.0)).color(accent)),
            None => div().child(line("›", text::SM, lh, accent)),
        };
        let id = self.id.clone();
        let mut root = div()
            .id(self.id)
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
            .child(glyph.flex_none())
            .child(
                div()
                    .flex_1()
                    .min_w_0()
                    .child(line(self.text, text::SM, lh, color::FG)),
            );
        // The 3px left edge takes the tone while the bottom edge stays a hairline: GPUI has one border colour per box, so the
        // left edge is a bar over the padding that makes room for it.
        root = root.relative().child(
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
                    .child(Button::new((id, "action"), label).on_click(handler)),
            );
        }
        root
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn tone_is_never_colour_alone() {
        assert_eq!(BannerTone::Info.glyph(), None);
        assert_eq!(BannerTone::Warn.glyph(), Some(IconName::TriangleAlert));
        assert_eq!(BannerTone::Danger.glyph(), Some(IconName::X));
        assert!(!BannerTone::Info.is_alert() && BannerTone::Danger.is_alert());
        assert_eq!(BannerTone::Warn.accent(), color::WARN);
    }
}
