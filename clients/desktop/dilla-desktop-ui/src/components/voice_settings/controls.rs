//! The Voice section's own controls: Let's check (the mic test button) and the push-to-talk key box with the button that
//! captures it (`d-voice-settings__check`, `__kbd`, `__capture`).

use std::rc::Rc;

use gpui::{
    App, ClickEvent, ElementId, FocusHandle, InteractiveElement, IntoElement, KeyDownEvent,
    ParentElement, RenderOnce, SharedString, StatefulInteractiveElement, Styled, Window, div, px,
};

use gpui::prelude::FluentBuilder as _;

use crate::a11y::{Accessible, Role};
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::{OPACITY_MUTED, focus_ring};
use crate::text::{line, line_height};
use crate::tokens::{color, font, radius, text};

type Leave = Rc<dyn Fn(&mut Window, &mut App) + 'static>;
type Click = Rc<dyn Fn(&ClickEvent, &mut Window, &mut App) + 'static>;

/// Let's check / Stop testing: 32px (plus its border) high, radius 7, 12/700; `--accent` filled, or the danger outline
/// while the test runs. Disabled while a call holds the microphone.
#[derive(IntoElement)]
pub struct CheckButton {
    id: ElementId,
    label: SharedString,
    testing: bool,
    disabled: bool,
    focus: Option<FocusHandle>,
    on_click: Option<Click>,
}

impl CheckButton {
    pub fn new(id: impl Into<ElementId>, label: impl Into<SharedString>, testing: bool) -> Self {
        Self {
            id: id.into(),
            label: label.into(),
            testing,
            disabled: false,
            focus: None,
            on_click: None,
        }
    }

    /// Tracks `handle` as the button's focus.
    pub fn focus_handle(mut self, handle: &FocusHandle) -> Self {
        self.focus = Some(handle.clone());
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
        self.on_click = Some(Rc::new(handler));
        self
    }
}

impl RenderOnce for CheckButton {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let (fg, bg, edge) = if self.testing {
            (
                color::DANGER,
                gpui::transparent_black(),
                color::DANGER_BORDER,
            )
        } else {
            (color::ACCENT_INK, color::ACCENT, color::ACCENT)
        };
        let icon = if self.testing {
            IconName::Square
        } else {
            IconName::Mic
        };
        let mut button = div()
            .id(self.id)
            .a11y(Role::Button, self.label.clone())
            .toggled(self.testing)
            .when_some(self.focus.as_ref(), |d, h| {
                d.track_focus(&h.clone().tab_stop(true))
            })
            .when(self.focus.is_none(), |d| d.tab_index(0))
            .flex()
            .flex_none()
            .items_center()
            .gap(px(8.0))
            .h(px(34.0))
            .px(px(14.0))
            .border_1()
            .border_color(edge)
            .rounded(radius::SEG)
            .bg(bg)
            .focus_visible(|s| s.shadow(focus_ring()))
            .child(Icon::new(icon).size(px(14.0)).color(fg))
            .child(
                line(self.label, text::UI, line_height(text::UI, 1.5), fg)
                    .weight(font::WEIGHT_DISPLAY),
            );
        button = if self.disabled {
            button.cursor_not_allowed().opacity(OPACITY_MUTED)
        } else {
            let handler = self.on_click;
            button.cursor_pointer().on_click(move |event, window, cx| {
                if let Some(handler) = &handler {
                    handler(event, window, cx);
                }
            })
        };
        button
    }
}

/// What a key does while the capture button listens.
pub type OnCaptureKey = Rc<dyn Fn(&KeyDownEvent, &mut Window, &mut App) + 'static>;

/// The push-to-talk key: a 38px box (36px of content in a 1px border) showing the key, or "No key set", or "Press a key..."
/// with the accent border and a 3px ring while it listens, and the 36px button beside it. The button takes the focus when
/// the capture starts and reads Cancel; every key but Tab goes to `on_key` while it listens, and leaving it cancels.
#[derive(IntoElement)]
pub struct KeyCapture {
    id: ElementId,
    key_text: SharedString,
    button_label: SharedString,
    empty: bool,
    listening: bool,
    focus: Option<FocusHandle>,
    on_click: Option<Click>,
    on_key: Option<OnCaptureKey>,
    on_blur: Option<Leave>,
}

impl KeyCapture {
    pub fn new(
        id: impl Into<ElementId>,
        key_text: impl Into<SharedString>,
        button_label: impl Into<SharedString>,
    ) -> Self {
        Self {
            id: id.into(),
            key_text: key_text.into(),
            button_label: button_label.into(),
            empty: false,
            listening: false,
            focus: None,
            on_click: None,
            on_key: None,
            on_blur: None,
        }
    }

    /// No key is set (and none is being captured).
    pub fn empty(mut self, empty: bool) -> Self {
        self.empty = empty;
        self
    }

    /// Tracks `handle` as the capture button's focus.
    pub fn focus_handle(mut self, handle: &FocusHandle) -> Self {
        self.focus = Some(handle.clone());
        self
    }

    pub fn listening(mut self, listening: bool) -> Self {
        self.listening = listening;
        self
    }

    pub fn on_click(
        mut self,
        handler: impl Fn(&ClickEvent, &mut Window, &mut App) + 'static,
    ) -> Self {
        self.on_click = Some(Rc::new(handler));
        self
    }

    pub fn on_key(
        mut self,
        handler: impl Fn(&KeyDownEvent, &mut Window, &mut App) + 'static,
    ) -> Self {
        self.on_key = Some(Rc::new(handler));
        self
    }

    /// Tab left the button while it listened: the capture is over.
    pub fn on_blur(mut self, handler: impl Fn(&mut Window, &mut App) + 'static) -> Self {
        self.on_blur = Some(Rc::new(handler));
        self
    }
}

impl RenderOnce for KeyCapture {
    fn render(self, window: &mut Window, cx: &mut App) -> impl IntoElement {
        let own = window
            .use_keyed_state((self.id.clone(), "capture"), cx, |_, cx| {
                cx.focus_handle().tab_stop(true)
            })
            .read(cx)
            .clone();
        let focus = self.focus.clone().unwrap_or(own);
        // The capture starts: the button takes the focus, so the next key is its own.
        if self.listening && !focus.is_focused(window) {
            focus.focus(window, cx);
        }
        let spoken = format!("{}, {}", self.button_label, self.key_text);
        let (fg, weight) = if self.empty && !self.listening {
            (color::FG_2, font::WEIGHT_BODY)
        } else {
            (color::FG, font::WEIGHT_DISPLAY)
        };
        let mut key_box = div()
            .flex()
            .flex_1()
            .items_center()
            .justify_center()
            .h(px(38.0))
            .border_1()
            .rounded(radius::SEG)
            .bg(color::SURFACE_2)
            .border_color(if self.listening {
                color::ACCENT
            } else {
                color::CONTROL_EDGE_FIELD
            })
            .child(
                line(
                    self.key_text,
                    text::INPUT,
                    line_height(text::INPUT, 1.5),
                    fg,
                )
                .weight(weight),
            );
        if self.listening {
            key_box = key_box.shadow(vec![gpui::BoxShadow {
                color: color::ACCENT_RING,
                offset: gpui::point(px(0.0), px(0.0)),
                blur_radius: px(0.0),
                spread_radius: px(3.0),
                inset: false,
            }]);
        }
        let (listening, on_key, on_click, on_leave) =
            (self.listening, self.on_key, self.on_click, self.on_blur);
        let button = div()
            .id((self.id.clone(), "button"))
            // The button names the key it sets: "Change key, Ctrl+Space" (or what it says while listening).
            .a11y(Role::Button, spoken)
            .track_focus(&focus)
            .flex()
            .flex_none()
            .items_center()
            .h(px(38.0))
            .px(px(12.0))
            .border_1()
            .border_color(color::CONTROL_EDGE)
            .rounded(radius::SEG)
            .cursor_pointer()
            .hover(|s| s.border_color(color::ACCENT))
            .focus_visible(|s| s.shadow(focus_ring()))
            .on_key_down(move |event, window, cx| {
                if !listening {
                    return;
                }
                if event.keystroke.key == "tab" {
                    // Tab still leaves, which cancels the capture.
                    if let Some(handler) = &on_leave {
                        handler(window, cx);
                    }
                    return;
                }
                cx.stop_propagation();
                if let Some(handler) = &on_key {
                    handler(event, window, cx);
                }
            })
            .on_click(move |event, window, cx| {
                if let Some(handler) = &on_click {
                    handler(event, window, cx);
                }
            })
            .child(line(
                self.button_label,
                text::BTN,
                line_height(text::BTN, 1.5),
                color::FG_STRONG,
            ));
        div()
            .id(self.id)
            .flex()
            .gap(px(8.0))
            .child(key_box)
            .child(button)
    }
}
