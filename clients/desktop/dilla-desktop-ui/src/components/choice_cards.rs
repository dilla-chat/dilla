//! `ChoiceCards` (packages/ui/src/ChoiceCards): the second-factor choice. A radio group of cards: a 32px icon tile, the
//! title over the hint (and the reason a card is off), and a 14px dot at the right. One tab stop, on the picked enabled
//! card (the first enabled one when none is); the arrows move and pick among the enabled cards, wrapping.

use std::rc::Rc;

use gpui::{
    App, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce, SharedString,
    StatefulInteractiveElement, Styled, Window, div, px,
};

use crate::a11y::{Accessible, Role};
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::focus_ring;
use crate::text::{line, line_height};
use crate::tokens::{color, font, radius, text};

#[derive(Clone, Debug, PartialEq)]
pub struct ChoiceCardOption {
    pub id: SharedString,
    pub title: SharedString,
    pub hint: SharedString,
    pub disabled: bool,
    /// Why the card is off; shown only while it is.
    pub reason: Option<SharedString>,
    pub icon: Option<IconName>,
}

impl ChoiceCardOption {
    pub fn new(
        id: impl Into<SharedString>,
        title: impl Into<SharedString>,
        hint: impl Into<SharedString>,
    ) -> Self {
        Self {
            id: id.into(),
            title: title.into(),
            hint: hint.into(),
            disabled: false,
            reason: None,
            icon: None,
        }
    }

    pub fn icon(mut self, icon: IconName) -> Self {
        self.icon = Some(icon);
        self
    }

    pub fn disabled(mut self, reason: impl Into<SharedString>) -> Self {
        self.disabled = true;
        self.reason = Some(reason.into());
        self
    }
}

/// The index (among all options) holding the tab stop: the picked enabled one, else the first enabled one.
pub fn tab_stop(options: &[ChoiceCardOption], value: &str) -> Option<usize> {
    options
        .iter()
        .position(|o| !o.disabled && o.id == value)
        .or_else(|| options.iter().position(|o| !o.disabled))
}

/// The option an arrow moves to from `from`: the next enabled one in `step` (1 or -1) direction, wrapping.
pub fn arrow_target(options: &[ChoiceCardOption], from: usize, step: isize) -> Option<usize> {
    let enabled: Vec<usize> = (0..options.len())
        .filter(|i| !options[*i].disabled)
        .collect();
    let at = enabled.iter().position(|i| *i == from)?;
    let len = enabled.len() as isize;
    Some(enabled[(at as isize + step).rem_euclid(len) as usize])
}

type PickHandler = Rc<dyn Fn(&str, &mut Window, &mut App) + 'static>;

#[derive(IntoElement)]
pub struct ChoiceCards {
    id: ElementId,
    options: Vec<ChoiceCardOption>,
    value: SharedString,
    on_change: Option<PickHandler>,
}

impl ChoiceCards {
    pub fn new(
        id: impl Into<ElementId>,
        options: Vec<ChoiceCardOption>,
        value: impl Into<SharedString>,
    ) -> Self {
        Self {
            id: id.into(),
            options,
            value: value.into(),
            on_change: None,
        }
    }

    pub fn on_change(mut self, handler: impl Fn(&str, &mut Window, &mut App) + 'static) -> Self {
        self.on_change = Some(Rc::new(handler));
        self
    }
}

/// The 14px dot (`box-sizing: content-box`, a 1px edge: 16 in all): empty, or the accent edge round a `--bg-2` ring and
/// an 8px accent centre.
fn dot(checked: bool) -> impl IntoElement {
    let ring = div()
        .flex()
        .flex_none()
        .items_center()
        .justify_center()
        .size(px(16.0))
        .rounded(radius::PILL)
        .border_1();
    if checked {
        ring.border_color(color::ACCENT)
            .bg(color::BG_2)
            .child(div().size(px(8.0)).rounded(radius::PILL).bg(color::ACCENT))
    } else {
        ring.border_color(color::CONTROL_EDGE_KEY)
    }
}

impl RenderOnce for ChoiceCards {
    fn render(self, window: &mut Window, cx: &mut App) -> impl IntoElement {
        let count = self.options.len();
        let handles = crate::roving::focus_handles((self.id.clone(), "focus"), count, window, cx);
        let stop = tab_stop(&self.options, &self.value);
        let (options, on_change) = (self.options.clone(), self.on_change.clone());
        let group_handles = handles.clone();
        let mut group = div()
            .id(self.id.clone())
            .flex()
            .flex_col()
            .gap(px(8.0))
            .min_w_0()
            .on_key_down(move |event, window, cx| {
                let m = &event.keystroke.modifiers;
                if m.alt || m.control || m.platform || m.shift {
                    return;
                }
                let step = match event.keystroke.key.as_str() {
                    "down" | "right" => 1,
                    "up" | "left" => -1,
                    _ => return,
                };
                let Some(from) = group_handles.iter().position(|h| h.is_focused(window)) else {
                    return;
                };
                let Some(next) = arrow_target(&options, from, step) else {
                    return;
                };
                cx.stop_propagation();
                group_handles[next].focus(window, cx);
                if next != from
                    && let Some(on_change) = &on_change
                {
                    on_change(&options[next].id, window, cx);
                }
            });
        let on_change = self.on_change;
        for (i, o) in self.options.iter().enumerate() {
            let checked = o.id == self.value;
            let mut tile = None;
            if let Some(icon) = o.icon {
                tile = Some(
                    div()
                        .flex()
                        .flex_none()
                        .items_center()
                        .justify_center()
                        .size(px(32.0))
                        .rounded(radius::LG)
                        .bg(color::CHOICE_TILE)
                        .child(Icon::new(icon).size(px(16.0)).color(color::ACCENT)),
                );
            }
            let (title_fg, hint_fg) = if o.disabled {
                (color::FG_3, color::FG_3)
            } else {
                (color::FG, color::FG_2)
            };
            let mut body = div()
                .flex()
                .flex_col()
                .flex_1()
                .min_w_0()
                .child(
                    line(
                        o.title.clone(),
                        text::SM,
                        line_height(text::SM, 1.35),
                        title_fg,
                    )
                    .weight(font::WEIGHT_LABEL),
                )
                .child(line(
                    o.hint.clone(),
                    text::XS,
                    line_height(text::XS, 1.35),
                    hint_fg,
                ));
            if let (true, Some(reason)) = (o.disabled, &o.reason) {
                body = body.child(line(
                    reason.clone(),
                    text::XS,
                    line_height(text::XS, 1.35),
                    color::FG,
                ));
            }
            let mut card = div()
                .id((self.id.clone(), format!("card-{}", o.id)))
                .a11y(Role::RadioButton, o.title.clone())
                .aria_description(o.hint.clone())
                .toggled(checked)
                // The handle's own flag decides the tab stop of a tracked element.
                .track_focus(&handles[i].clone().tab_stop(stop == Some(i)))
                .flex()
                .items_center()
                .gap(px(12.0))
                .p(px(12.0))
                .min_h(px(48.0))
                .rounded(radius::MENU)
                .border_1()
                .border_color(if checked {
                    color::ACCENT
                } else {
                    color::CONTROL_EDGE_FIELD
                })
                .focus_visible(|s| s.shadow(focus_ring()));
            if checked {
                card = card.bg(color::ACCENT_PICK);
            }
            if let Some(tile) = tile {
                card = card.child(tile);
            }
            card = card.child(body).child(dot(checked));
            card = if o.disabled {
                card.cursor_not_allowed()
            } else {
                card.cursor_pointer()
            };
            if !o.disabled
                && let Some(handler) = on_change.clone()
            {
                let id = o.id.clone();
                let pick = {
                    let handler = handler.clone();
                    let id = id.clone();
                    move |window: &mut Window, cx: &mut App| handler(&id, window, cx)
                };
                card = card.on_click(move |_, window, cx| pick(window, cx));
            }
            group = group.child(card);
        }
        group
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn options() -> Vec<ChoiceCardOption> {
        vec![
            ChoiceCardOption::new("totp", "Authenticator app", "Six digits"),
            ChoiceCardOption::new("passkey", "Passkey", "Touch")
                .disabled("This browser has no passkeys"),
            ChoiceCardOption::new("key", "Security key", "Touch"),
        ]
    }

    #[test]
    fn the_tab_stop_is_the_picked_enabled_card_else_the_first_enabled() {
        let o = options();
        assert_eq!(tab_stop(&o, "key"), Some(2));
        assert_eq!(
            tab_stop(&o, "passkey"),
            Some(0),
            "a disabled pick holds no stop"
        );
        assert_eq!(tab_stop(&o, "none"), Some(0));
    }

    #[test]
    fn arrows_skip_disabled_cards_and_wrap() {
        let o = options();
        assert_eq!(arrow_target(&o, 0, 1), Some(2));
        assert_eq!(arrow_target(&o, 2, 1), Some(0));
        assert_eq!(arrow_target(&o, 0, -1), Some(2));
        assert_eq!(
            arrow_target(&o, 1, 1),
            None,
            "focus is never on a disabled card"
        );
    }
}
