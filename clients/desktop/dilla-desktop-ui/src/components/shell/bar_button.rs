//! The channel header's action button (ChannelHeader.css `.d-channel-header__actions .d-btn`): 28px high, an icon and a
//! label in `--fg-2` at 11px with gap 6px and padding 0 8px; hovered it brightens to `--fg` (no fill); while its aside is
//! open (pressed) it is `--accent` on `--accent-soft`, with no `[x]` mark. The shared `Button` draws a mark and a solid
//! accent fill for a pressed toggle, so the header has its own.

use gpui::{
    App, ClickEvent, ElementId, InteractiveElement, IntoElement, ParentElement, Pixels, RenderOnce,
    SharedString, StatefulInteractiveElement, Styled, Window, div,
};

use crate::components::button::ClickHandler;
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::focus_ring;
use crate::text::{line, line_height};
use crate::tokens::{color, font, radius, text};

const HEIGHT: f32 = 28.0;
const PAD: f32 = 8.0;
const GAP: f32 = 6.0;
const GLYPH: f32 = 14.0;

#[derive(IntoElement)]
pub struct BarButton {
    id: ElementId,
    label: SharedString,
    icon: IconName,
    pressed: bool,
    on_click: Option<ClickHandler>,
}

impl BarButton {
    pub fn new(id: impl Into<ElementId>, icon: IconName, label: impl Into<SharedString>) -> Self {
        Self {
            id: id.into(),
            label: label.into(),
            icon,
            pressed: false,
            on_click: None,
        }
    }

    pub fn pressed(mut self, pressed: bool) -> Self {
        self.pressed = pressed;
        self
    }

    pub fn on_click(
        mut self,
        handler: impl Fn(&ClickEvent, &mut Window, &mut App) + 'static,
    ) -> Self {
        self.on_click = Some(Box::new(handler));
        self
    }
}

/// The colours of the button: `(text, fill)`.
pub fn colours(pressed: bool) -> (gpui::Hsla, gpui::Hsla) {
    if pressed {
        (color::ACCENT, color::ACCENT_SOFT)
    } else {
        (color::FG_2, gpui::transparent_black())
    }
}

impl RenderOnce for BarButton {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let (fg, bg) = colours(self.pressed);
        let hover_fg = if self.pressed {
            color::ACCENT
        } else {
            color::FG
        };
        let base = div()
            .id(self.id)
            .flex()
            .flex_none()
            .items_center()
            .gap(Pixels::from(GAP))
            .h(Pixels::from(HEIGHT))
            .px(Pixels::from(PAD))
            .font_family(font::MONO)
            .text_color(fg)
            .bg(bg)
            .rounded(radius::MD)
            .cursor_pointer()
            .tab_index(0)
            .hover(move |s| s.text_color(hover_fg))
            .focus_visible(|s| s.shadow(focus_ring()))
            .child(Icon::new(self.icon).size(Pixels::from(GLYPH)).color(fg))
            .child(
                line(self.label, text::XS, line_height(text::XS, 1.5), fg).weight(font::WEIGHT_BTN),
            );
        match self.on_click {
            Some(handler) => base.on_click(handler),
            None => base,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pressed_is_accent_on_accent_soft_and_rest_has_no_fill() {
        assert_eq!(colours(true), (color::ACCENT, color::ACCENT_SOFT));
        let (fg, bg) = colours(false);
        assert_eq!(fg, color::FG_2);
        assert_eq!(bg.a, 0.0);
    }
}
