//! `IconButton` (packages/ui/src/IconButton): a square icon control, 28, 32, 40 or 44px, with a tooltip that names it.
//! A toggle draws the accent glyph and a 2px bar under it (R27), never colour alone.

use gpui::{
    App, ClickEvent, ElementId, InteractiveElement, IntoElement, ParentElement, Pixels, RenderOnce,
    SharedString, StatefulInteractiveElement, Styled, Window, div, px,
};

use super::button::ClickHandler;
use super::tooltip::{SHOW_DELAY, tooltip};
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::{OPACITY_DISABLED, focus_ring};
use crate::tokens::{color, radius};

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum IconButtonSize {
    S28,
    #[default]
    S32,
    S40,
    S44,
}

impl IconButtonSize {
    /// The box in CSS px.
    pub fn side(self) -> Pixels {
        match self {
            Self::S28 => px(28.0),
            Self::S32 => px(32.0),
            Self::S40 => px(40.0),
            Self::S44 => px(44.0),
        }
    }

    /// The glyph (`GLYPH` in IconButton.tsx).
    pub fn glyph(self) -> Pixels {
        match self {
            Self::S28 => px(15.0),
            Self::S32 => px(18.0),
            Self::S40 | Self::S44 => px(20.0),
        }
    }
}

#[derive(IntoElement)]
pub struct IconButton {
    id: ElementId,
    icon: IconName,
    label: SharedString,
    size: IconButtonSize,
    pressed: Option<bool>,
    keys: Vec<String>,
    tooltip: bool,
    disabled: bool,
    on_click: Option<ClickHandler>,
}

impl IconButton {
    pub fn new(id: impl Into<ElementId>, icon: IconName, label: impl Into<SharedString>) -> Self {
        Self {
            id: id.into(),
            icon,
            label: label.into(),
            size: IconButtonSize::S32,
            pressed: None,
            keys: Vec::new(),
            tooltip: true,
            disabled: false,
            on_click: None,
        }
    }

    pub fn size(mut self, size: IconButtonSize) -> Self {
        self.size = size;
        self
    }

    /// Makes the button a toggle.
    pub fn pressed(mut self, pressed: bool) -> Self {
        self.pressed = Some(pressed);
        self
    }

    /// The chord shown in the tooltip.
    pub fn keys(mut self, keys: &[&str]) -> Self {
        self.keys = keys.iter().map(|k| (*k).to_owned()).collect();
        self
    }

    /// False for a control whose label is visible beside it.
    pub fn tooltip(mut self, tooltip: bool) -> Self {
        self.tooltip = tooltip;
        self
    }

    pub fn disabled(mut self, disabled: bool) -> Self {
        self.disabled = disabled;
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

impl RenderOnce for IconButton {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let pressed = self.pressed == Some(true);
        let fg = if pressed { color::ACCENT } else { color::FG_2 };
        let mut button = div()
            .id(self.id)
            .relative()
            .flex()
            .flex_none()
            .items_center()
            .justify_center()
            .size(self.size.side())
            .rounded(radius::MD)
            .text_color(fg)
            .child(Icon::new(self.icon).size(self.size.glyph()).color(fg));
        if pressed {
            button = button.child(
                div()
                    .absolute()
                    .left_0()
                    .right_0()
                    .bottom_0()
                    .h(px(2.0))
                    .bg(color::ACCENT),
            );
        }
        if self.disabled {
            return button.opacity(OPACITY_DISABLED).cursor_not_allowed();
        }
        let refs: Vec<&str> = self.keys.iter().map(String::as_str).collect();
        let mut button = button
            .cursor_pointer()
            .tab_index(0)
            .hover(|s| s.bg(color::HOVER).text_color(color::FG))
            .focus_visible(|s| s.shadow(focus_ring()));
        if self.tooltip {
            button = button
                .tooltip(tooltip(self.label, &refs))
                .tooltip_show_delay(SHOW_DELAY);
        }
        match self.on_click {
            Some(handler) => button.on_click(handler),
            None => button,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sizes_and_glyphs_match_icon_button_tsx() {
        let sizes = [
            (IconButtonSize::S28, 28.0, 15.0),
            (IconButtonSize::S32, 32.0, 18.0),
            (IconButtonSize::S40, 40.0, 20.0),
            (IconButtonSize::S44, 44.0, 20.0),
        ];
        for (size, side, glyph) in sizes {
            assert_eq!((size.side(), size.glyph()), (px(side), px(glyph)));
        }
    }
}
