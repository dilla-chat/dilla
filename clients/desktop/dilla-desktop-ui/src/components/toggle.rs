//! `Toggle` (packages/ui/src/Toggle): a switch for one setting. The row around it shows the visible label; with
//! `show_label` the switch shows its own label before the knob. A disabled switch stays focusable and ignores activation.
//! Space and Enter activate it, as the native button does on the web.

use gpui::{
    App, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce, SharedString,
    StatefulInteractiveElement, Styled, Window, div, px,
};

use crate::metrics::{OPACITY_MUTED, focus_ring, shadow_knob};
use crate::text::{line, line_height};
use crate::tokens::{color, radius, text};

pub(crate) type ChangeHandler = Box<dyn Fn(bool, &mut Window, &mut App) + 'static>;

/// The track: 36 x 20 with a 1px edge; the knob 16px, 1px in from the inner edge, 16px further right when on.
pub const TRACK_W: f32 = 36.0;
pub const TRACK_H: f32 = 20.0;
pub const KNOB: f32 = 16.0;

/// The knob's left offset from the track's inner edge (`translateX(1rem)` when on).
pub fn knob_left(on: bool) -> f32 {
    if on { 1.0 + 16.0 } else { 1.0 }
}

#[derive(IntoElement)]
pub struct Toggle {
    id: ElementId,
    label: SharedString,
    checked: bool,
    hint: Option<SharedString>,
    disabled: bool,
    show_label: bool,
    fit: Fit,
    on_change: Option<ChangeHandler>,
}

/// How the switch's 44 x 24 target sits in its host.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum Fit {
    /// The target as drawn: at least 44 x 24, 4px each side of the track.
    #[default]
    Target,
    /// In a settings row (`.d-settings-row__control .d-toggle__switch { margin: -2px -4px }`): the target reaches into
    /// the row's padding, so the row lays out the 36 x 20 track.
    Row,
    /// In a Voice switch row (`.d-voice-settings__switch-row .d-toggle__switch`): no side padding and no minimum width,
    /// 24px high, so the track is 36 wide.
    Tight,
}

impl Toggle {
    pub fn new(id: impl Into<ElementId>, label: impl Into<SharedString>, checked: bool) -> Self {
        Self {
            id: id.into(),
            label: label.into(),
            checked,
            hint: None,
            disabled: false,
            show_label: false,
            fit: Fit::Target,
            on_change: None,
        }
    }

    pub fn hint(mut self, hint: impl Into<SharedString>) -> Self {
        self.hint = Some(hint.into());
        self
    }

    pub fn disabled(mut self, disabled: bool) -> Self {
        self.disabled = disabled;
        self
    }

    /// Shows the label before the knob (named by that visible text).
    pub fn show_label(mut self, show: bool) -> Self {
        self.show_label = show;
        self
    }

    /// How the switch sits in its host ([`Fit`]).
    pub fn fit(mut self, fit: Fit) -> Self {
        self.fit = fit;
        self
    }

    pub fn on_change(mut self, handler: impl Fn(bool, &mut Window, &mut App) + 'static) -> Self {
        self.on_change = Some(Box::new(handler));
        self
    }
}

impl RenderOnce for Toggle {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let on = self.checked;
        let track = div()
            .relative()
            .flex_none()
            .w(px(TRACK_W))
            .h(px(TRACK_H))
            .rounded(radius::PILL)
            .border_1()
            .border_color(if on {
                color::ACCENT
            } else {
                color::CONTROL_EDGE
            })
            .bg(if on { color::ACCENT } else { color::TRACK_OFF })
            .child(
                div()
                    .absolute()
                    // `top: 1px; left: 1px` in the track's padding box (inside its 1px edge), then 1rem across when on.
                    .top(px(1.0))
                    .left(px(knob_left(on)))
                    .size(px(KNOB))
                    .rounded(radius::PILL)
                    .bg(color::KNOB)
                    .shadow(shadow_knob()),
            );
        let disabled = self.disabled;
        let on_change = self.on_change;
        let mut switch = div()
            .id(self.id.clone())
            .flex()
            .items_center()
            .justify_center()
            .min_h(px(24.0))
            .rounded(radius::MD)
            .tab_index(0)
            .focus_visible(|s| s.shadow(focus_ring()))
            .child(track);
        switch = match self.fit {
            Fit::Target => switch.min_w(px(44.0)).px(px(4.0)),
            Fit::Row => switch.min_w(px(44.0)).px(px(4.0)).mx(px(-4.0)).my(px(-2.0)),
            Fit::Tight => switch,
        };
        switch = if disabled {
            switch.cursor_not_allowed().opacity(OPACITY_MUTED)
        } else {
            switch.cursor_pointer()
        };
        if let Some(handler) = on_change {
            let handler = std::rc::Rc::new(handler);
            let on_click = handler.clone();
            switch = switch.on_click(move |_, window, cx| {
                if !disabled {
                    on_click(!on, window, cx);
                }
            });
        }
        let mut root = div().flex().items_center().gap(px(8.0)).min_w_0();
        if self.show_label {
            root = root.child(line(
                self.label,
                text::SM,
                line_height(text::SM, 1.5),
                color::FG_2,
            ));
        }
        root = root.child(switch);
        if let Some(hint) = self.hint {
            root = root.child(line(
                hint,
                text::SM,
                line_height(text::SM, 1.5),
                color::FG_3,
            ));
        }
        root
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_knob_travels_one_rem() {
        assert_eq!(knob_left(true) - knob_left(false), 16.0);
        assert_eq!((TRACK_W, TRACK_H, KNOB), (36.0, 20.0, 16.0));
    }
}
