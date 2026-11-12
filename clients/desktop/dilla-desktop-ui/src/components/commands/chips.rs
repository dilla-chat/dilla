//! The chips that replace the composer's field while a command is picked (packages/ui CommandPicker `CommandChips`,
//! `CommandField`, `CommandBar`): the accent `/name` token, one field per argument (its label tile accent while active,
//! danger while invalid, `?` after an optional label), and the 11px line above them.

use gpui::{
    AnyElement, App, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce,
    SharedString, Styled, Window, div, px,
};

use super::halo;
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::text::{MONO_ADVANCE, line, line_height};
use crate::tokens::{color, font, radius, text};

/// A chip's height: 26px (`height: 1.625rem`) inside its 1px border.
pub const CHIP: f32 = 26.0;

/// `CommandChips`: the `/name` token, then the fields.
#[derive(IntoElement)]
pub struct CommandChips {
    id: ElementId,
    icon: IconName,
    name: SharedString,
    fields: Vec<AnyElement>,
}

impl CommandChips {
    pub fn new(id: impl Into<ElementId>, icon: IconName, name: impl Into<SharedString>) -> Self {
        Self {
            id: id.into(),
            icon,
            name: name.into(),
            fields: Vec::new(),
        }
    }

    pub fn field(mut self, field: impl IntoElement) -> Self {
        self.fields.push(field.into_any_element());
        self
    }
}

impl RenderOnce for CommandChips {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let lh = line_height(text::UI, 1.5);
        div()
            .id(self.id)
            .flex()
            .flex_1()
            .flex_wrap()
            .items_center()
            .gap(px(6.0))
            .min_w_0()
            .py(px(2.0))
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap(px(6.0))
                    .h(px(CHIP))
                    .px(px(8.0))
                    .rounded(radius::ROW)
                    .bg(color::ACCENT_TAG)
                    .child(Icon::new(self.icon).size(px(12.0)).color(color::ACCENT_2))
                    .child(line("/", text::UI, lh, color::ACCENT_2).weight(font::WEIGHT_DISPLAY))
                    .child(
                        line(self.name, text::UI, lh, color::ACCENT_2).weight(font::WEIGHT_DISPLAY),
                    ),
            )
            .children(self.fields)
    }
}

/// `CommandField`: one argument chip.
#[derive(IntoElement)]
pub struct CommandField {
    id: ElementId,
    label: SharedString,
    optional: bool,
    value: SharedString,
    placeholder: SharedString,
    active: bool,
    invalid: bool,
    ghost: bool,
    input: Option<AnyElement>,
}

impl CommandField {
    pub fn new(
        id: impl Into<ElementId>,
        label: impl Into<SharedString>,
        value: impl Into<SharedString>,
        placeholder: impl Into<SharedString>,
    ) -> Self {
        Self {
            id: id.into(),
            label: label.into(),
            optional: false,
            value: value.into(),
            placeholder: placeholder.into(),
            active: false,
            invalid: false,
            ghost: false,
            input: None,
        }
    }

    pub fn optional(mut self, optional: bool) -> Self {
        self.optional = optional;
        self
    }

    pub fn active(mut self, active: bool) -> Self {
        self.active = active;
        self
    }

    pub fn invalid(mut self, invalid: bool) -> Self {
        self.invalid = invalid;
        self
    }

    /// A command without arguments: one label-less chip that says "press ↵ to run".
    pub fn ghost(mut self, ghost: bool) -> Self {
        self.ghost = ghost;
        self
    }

    /// The editable input drawn in place of the value (the screen owns its state).
    pub fn input(mut self, input: impl IntoElement) -> Self {
        self.input = Some(input.into_any_element());
        self
    }

    /// The input's width: `calc(max(len, placeholder, 4)ch + 1rem)`.
    pub fn input_width(value: &str, placeholder: &str) -> f32 {
        let longest = value
            .split('\n')
            .map(|l| l.chars().count())
            .max()
            .unwrap_or(0);
        let chars = longest.max(placeholder.chars().count()).max(4);
        chars as f32 * f32::from(text::INPUT) * MONO_ADVANCE + 16.0
    }
}

impl RenderOnce for CommandField {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let (edge, label_fg, label_bg) = match (self.active, self.invalid) {
            (_, true) => (color::DANGER, color::ACCENT_INK, color::DANGER),
            (true, false) => (color::ACCENT, color::ACCENT_INK, color::ACCENT),
            (false, false) => (color::CONTROL_EDGE_FIELD, color::FG_2, color::HOVER),
        };
        let lh = line_height(text::META, 1.5);
        let show_label = !self.ghost && !self.label.is_empty();
        let width = Self::input_width(&self.value, &self.placeholder);
        let value = self.input.unwrap_or_else(|| {
            let (shown, fg) = if self.value.is_empty() {
                (self.placeholder.clone(), color::FG_FAINT)
            } else {
                (self.value.clone(), color::FG)
            };
            line(shown, text::INPUT, CHIP, fg).into_any_element()
        });
        let mut chip = div()
            .id(self.id)
            .flex()
            .items_stretch()
            .h(px(CHIP))
            .overflow_hidden()
            .border_1()
            .border_color(edge)
            .rounded(radius::ROW)
            .bg(color::SURFACE_2)
            .children(show_label.then(|| {
                div()
                    .flex()
                    .items_center()
                    .px(px(6.0))
                    .bg(label_bg)
                    .child(line(
                        format!("{}{}", self.label, if self.optional { "?" } else { "" }),
                        text::META,
                        lh,
                        label_fg,
                    ))
            }))
            .child(
                div()
                    .flex()
                    .items_center()
                    .w(px(width))
                    .min_w_0()
                    .px(px(7.0))
                    .child(value),
            );
        if self.active {
            chip = chip.shadow(halo(if self.invalid {
                color::DANGER_RING
            } else {
                color::ACCENT_SOFT
            }));
        }
        chip
    }
}

/// `CommandBar`'s tone.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum BarTone {
    Info,
    Danger,
}

/// `CommandBar`: the 11px line above the chips (the description, a preview, or an error) and the keyboard hint.
#[derive(IntoElement)]
pub struct CommandBar {
    id: ElementId,
    icon: IconName,
    text: SharedString,
    tone: BarTone,
    hint: Option<SharedString>,
}

impl CommandBar {
    pub fn new(
        id: impl Into<ElementId>,
        icon: IconName,
        text: impl Into<SharedString>,
        tone: BarTone,
    ) -> Self {
        Self {
            id: id.into(),
            icon,
            text: text.into(),
            tone,
            hint: None,
        }
    }

    pub fn hint(mut self, hint: impl Into<SharedString>) -> Self {
        self.hint = Some(hint.into());
        self
    }

    /// Its height in flow: the 11px line (1.5) and its 6px bottom margin.
    pub fn height() -> f32 {
        line_height(text::XS, 1.5) + 6.0
    }
}

impl RenderOnce for CommandBar {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let (fg, icon) = match self.tone {
            BarTone::Info => (color::FG_2, color::ACCENT),
            BarTone::Danger => (color::DANGER, color::DANGER),
        };
        let lh = line_height(text::XS, 1.5);
        div()
            .id(self.id)
            .flex()
            .items_center()
            .gap(px(8.0))
            .min_w_0()
            .mb(px(6.0))
            .px(px(4.0))
            .child(Icon::new(self.icon).size(px(12.0)).color(icon))
            .child(
                div()
                    .flex_1()
                    .min_w_0()
                    .child(line(self.text, text::XS, lh, fg).ellipsis()),
            )
            .children(self.hint.map(|h| line(h, text::XS, lh, color::FG_3)))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn an_input_is_as_wide_as_its_longest_text_and_at_least_four_characters() {
        let ch = f32::from(text::INPUT) * MONO_ADVANCE;
        assert_eq!(CommandField::input_width("", ""), 4.0 * ch + 16.0);
        assert_eq!(CommandField::input_width("in 1 hour", ""), 9.0 * ch + 16.0);
        assert_eq!(
            CommandField::input_width("", "bring two ropes"),
            15.0 * ch + 16.0
        );
        assert_eq!(CommandField::input_width("ab\nabcdef", ""), 6.0 * ch + 16.0);
    }
}
