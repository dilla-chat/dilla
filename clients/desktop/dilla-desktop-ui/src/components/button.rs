//! `Button` (packages/ui/src/Button): 32px (22px small) tall, 1px control edge, radius `md`, four variants, a toggle mark.

use gpui::{
    App, ClickEvent, ElementId, Hsla, InteractiveElement, IntoElement, ParentElement, RenderOnce,
    SharedString, StatefulInteractiveElement, Styled, Window, div, px,
};

use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::text::{line, line_height};
use crate::tokens::{color, font, radius, text};

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum ButtonVariant {
    #[default]
    Default,
    Accent,
    Danger,
    Ghost,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum ButtonSize {
    Sm,
    #[default]
    Md,
    /// In a bar (ChannelHeader.css `.d-channel-header__actions .d-btn`): 28px, 8px across, a 6px gap, `--text-xs`, no
    /// border.
    Bar,
}

/// The colours of one state, resolved from the variant.
#[derive(Clone, Copy, Debug, PartialEq)]
pub(crate) struct Palette {
    pub fg: Hsla,
    pub bg: Hsla,
    pub border: Hsla,
    pub hover_fg: Hsla,
    pub hover_bg: Hsla,
}

/// `Button.css`: default is transparent with `--control-edge`; accent fills with `--accent` (hover `--accent-2`); danger is
/// `--danger` on `--danger-border` (hover `--danger-soft`); ghost is `--fg-2` with no edge (hover `--fg` on `--hover`).
/// A pressed toggle is the accent fill. A disabled accent button is an outline in `--fg-3`.
pub(crate) fn palette(variant: ButtonVariant, pressed: bool, disabled: bool) -> Palette {
    let clear = gpui::transparent_black();
    if pressed {
        return Palette {
            fg: color::ACCENT_INK,
            bg: color::ACCENT,
            border: color::ACCENT,
            hover_fg: color::ACCENT_INK,
            hover_bg: color::ACCENT,
        };
    }
    match variant {
        ButtonVariant::Default => Palette {
            fg: color::FG,
            bg: clear,
            border: color::CONTROL_EDGE,
            hover_fg: color::FG,
            hover_bg: color::HOVER,
        },
        ButtonVariant::Accent if disabled => Palette {
            fg: color::FG_3,
            bg: clear,
            border: color::HAIRLINE,
            hover_fg: color::FG_3,
            hover_bg: clear,
        },
        ButtonVariant::Accent => Palette {
            fg: color::ACCENT_INK,
            bg: color::ACCENT,
            border: color::ACCENT,
            hover_fg: color::ACCENT_INK,
            hover_bg: color::ACCENT_2,
        },
        ButtonVariant::Danger => Palette {
            fg: color::DANGER,
            bg: clear,
            border: color::DANGER_BORDER,
            hover_fg: color::DANGER,
            hover_bg: color::DANGER_SOFT,
        },
        ButtonVariant::Ghost => Palette {
            fg: color::FG_2,
            bg: clear,
            border: clear,
            hover_fg: color::FG,
            hover_bg: color::HOVER,
        },
    }
}

pub(crate) type ClickHandler = Box<dyn Fn(&ClickEvent, &mut Window, &mut App) + 'static>;

#[derive(IntoElement)]
pub struct Button {
    id: ElementId,
    label: SharedString,
    icon: Option<IconName>,
    variant: ButtonVariant,
    size: ButtonSize,
    key_hint: Option<SharedString>,
    pressed: Option<bool>,
    disabled: bool,
    on_click: Option<ClickHandler>,
}

impl Button {
    pub fn new(id: impl Into<ElementId>, label: impl Into<SharedString>) -> Self {
        Self {
            id: id.into(),
            label: label.into(),
            icon: None,
            variant: ButtonVariant::Default,
            size: ButtonSize::Md,
            key_hint: None,
            pressed: None,
            disabled: false,
            on_click: None,
        }
    }

    pub fn variant(mut self, variant: ButtonVariant) -> Self {
        self.variant = variant;
        self
    }

    /// An icon before the label (14px in a bar, as the header's buttons draw theirs).
    pub fn icon(mut self, icon: IconName) -> Self {
        self.icon = Some(icon);
        self
    }

    pub fn size(mut self, size: ButtonSize) -> Self {
        self.size = size;
        self
    }

    /// A keybind shown after the label, such as `M`.
    pub fn key_hint(mut self, hint: impl Into<SharedString>) -> Self {
        self.key_hint = Some(hint.into());
        self
    }

    /// Makes the button a toggle: pressed draws the accent fill and a `[x]` mark, not pressed `[ ]`.
    pub fn pressed(mut self, pressed: bool) -> Self {
        self.pressed = Some(pressed);
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

impl RenderOnce for Button {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let p = palette(self.variant, self.pressed == Some(true), self.disabled);
        let (height, pad, gap, size) = match self.size {
            ButtonSize::Sm => (22.0, 7.0, 8.0, text::XS),
            ButtonSize::Md => (32.0, 12.0, 8.0, text::BTN),
            ButtonSize::Bar => (28.0, 8.0, 6.0, text::XS),
        };
        let mut base = div()
            .id(self.id)
            .flex()
            .flex_none()
            .items_center()
            .gap(px(gap))
            .h(px(height))
            .px(px(pad))
            .font_family(font::MONO)
            .text_size(size)
            .font_weight(font::WEIGHT_BTN)
            .text_color(p.fg)
            .bg(p.bg)
            .rounded(radius::MD);
        if self.size != ButtonSize::Bar {
            base = base.border_1().border_color(p.border);
        }
        if let Some(icon) = self.icon {
            base = base.child(Icon::new(icon).size(px(14.0)).color(p.fg));
        }
        if self.pressed.is_some() {
            base = base.child(div().child(if self.pressed == Some(true) {
                "[x]"
            } else {
                "[ ]"
            }));
        }
        base = base
            .child(line(self.label, size, line_height(size, 1.5), p.fg).weight(font::WEIGHT_BTN));
        if let Some(hint) = self.key_hint {
            base = base.child(
                div()
                    .px(px(4.0))
                    .text_size(text::MICRO)
                    .border_1()
                    .border_b_2()
                    .border_color(p.fg)
                    .rounded(radius::SM)
                    .child(hint),
            );
        }
        if self.disabled {
            return base.opacity(0.4).cursor_not_allowed();
        }
        base = base
            .cursor_pointer()
            .hover(move |s| s.bg(p.hover_bg).text_color(p.hover_fg));
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
    fn variants_draw_the_css_colours() {
        let accent = palette(ButtonVariant::Accent, false, false);
        assert_eq!(
            (accent.bg, accent.fg, accent.hover_bg),
            (color::ACCENT, color::ACCENT_INK, color::ACCENT_2)
        );
        let danger = palette(ButtonVariant::Danger, false, false);
        assert_eq!(
            (danger.fg, danger.border, danger.hover_bg),
            (color::DANGER, color::DANGER_BORDER, color::DANGER_SOFT)
        );
        let ghost = palette(ButtonVariant::Ghost, false, false);
        assert_eq!((ghost.fg, ghost.hover_fg), (color::FG_2, color::FG));
        assert_eq!(
            palette(ButtonVariant::Default, false, false).border,
            color::CONTROL_EDGE
        );
    }

    #[test]
    fn pressed_is_the_accent_fill_and_a_disabled_accent_is_an_outline() {
        assert_eq!(palette(ButtonVariant::Ghost, true, false).bg, color::ACCENT);
        let off = palette(ButtonVariant::Accent, false, true);
        assert_eq!((off.fg, off.border), (color::FG_3, color::HAIRLINE));
        assert_eq!(off.bg.a, 0.0);
    }
}
