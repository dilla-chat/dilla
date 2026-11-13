//! The separator between two panes of the desktop shell (packages/ui `AppShell/Splitter.tsx`): a 24px hit area centred on
//! the 1px line the neighbouring pane already draws. A press starts a drag the shell follows (the shell sees the pointer
//! over the whole window); keys move it by 1rem (4rem with Shift), Home and End go to the limits, Enter and a double
//! click reset to the default.

use std::rc::Rc;

use gpui::{
    App, ClickEvent, ElementId, FocusHandle, InteractiveElement, IntoElement, Keystroke,
    MouseButton, MouseDownEvent, ParentElement, Pixels, RenderOnce, StatefulInteractiveElement,
    Styled, Window, div,
};

use super::panes::{Pane, REM};
use crate::tokens::color;

/// What a key asks of a splitter.
#[derive(Clone, Copy, Debug, PartialEq)]
pub enum SplitterKey {
    /// Move to this width (the shell clamps it).
    To(f32),
    /// Back to the default width.
    Reset,
}

/// `Splitter.tsx` `onKeyDown`: the arrows move by 1rem (4rem with Shift; the aside's splitter is on its left edge, so
/// Right narrows it), Home and End go to `min` and `max`, Enter resets. Alt, Ctrl and the platform key are not the
/// splitter's.
pub fn splitter_key(
    key: &Keystroke,
    pane: Pane,
    now: f32,
    min: f32,
    max: f32,
) -> Option<SplitterKey> {
    let m = &key.modifiers;
    if m.alt || m.control || m.platform {
        return None;
    }
    let sign = if pane == Pane::Sidebar { 1.0 } else { -1.0 };
    let step = if m.shift { 4.0 } else { 1.0 } * REM;
    match key.key.as_str() {
        "right" => Some(SplitterKey::To(now + sign * step)),
        "left" => Some(SplitterKey::To(now - sign * step)),
        "home" => Some(SplitterKey::To(min)),
        "end" => Some(SplitterKey::To(max)),
        "enter" => Some(SplitterKey::Reset),
        _ => None,
    }
}

type Press = Rc<dyn Fn(Pixels, &mut Window, &mut App)>;
type Set = Rc<dyn Fn(SplitterKey, &mut Window, &mut App)>;

#[derive(IntoElement)]
pub struct Splitter {
    id: ElementId,
    pane: Pane,
    focus: FocusHandle,
    now: f32,
    min: f32,
    max: f32,
    left: f32,
    probe: Option<gpui::AnyElement>,
    on_press: Option<Press>,
    on_set: Option<Set>,
}

impl Splitter {
    pub fn new(id: impl Into<ElementId>, pane: Pane, focus: &FocusHandle) -> Self {
        Self {
            id: id.into(),
            pane,
            focus: focus.clone(),
            now: 0.0,
            min: 0.0,
            max: 0.0,
            left: 0.0,
            probe: None,
            on_press: None,
            on_set: None,
        }
    }

    /// The width now and its limits, for the keys.
    pub fn range(mut self, now: f32, min: f32, max: f32) -> Self {
        (self.now, self.min, self.max) = (now, min, max);
        self
    }

    /// The hit area's left edge, from its container's.
    pub fn at(mut self, left: f32) -> Self {
        self.left = left;
        self
    }

    /// An invisible child the test-only automation interface looks the splitter up by.
    pub fn probe(mut self, probe: impl IntoElement) -> Self {
        self.probe = Some(probe.into_any_element());
        self
    }

    /// The pointer went down at window x.
    pub fn on_press(mut self, f: impl Fn(Pixels, &mut Window, &mut App) + 'static) -> Self {
        self.on_press = Some(Rc::new(f));
        self
    }

    pub fn on_set(mut self, f: impl Fn(SplitterKey, &mut Window, &mut App) + 'static) -> Self {
        self.on_set = Some(Rc::new(f));
        self
    }
}

/// The hit area's width.
pub const HIT: f32 = 24.0;

impl RenderOnce for Splitter {
    fn render(self, window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let focused = self.focus.is_focused(window);
        let (pane, now, min, max) = (self.pane, self.now, self.min, self.max);
        let on_set = self.on_set.clone();
        let reset = self.on_set.clone();
        let press = self.on_press.clone();
        let focus = self.focus.clone();
        // The line is 1px; focused it is the accent, as the web's `:focus-visible` ring on the separator.
        let line = div()
            .absolute()
            .left(Pixels::from(HIT / 2.0 - 1.0))
            .top_0()
            .h_full()
            .w(Pixels::from(1.0f32))
            .bg(if focused {
                color::ACCENT
            } else {
                color::HAIRLINE
            });
        div()
            .id(self.id)
            .track_focus(&self.focus)
            .absolute()
            .left(Pixels::from(self.left))
            .top_0()
            .h_full()
            .w(Pixels::from(HIT))
            .cursor_col_resize()
            .hover(|s| s.bg(color::ACCENT_SPLIT))
            .on_mouse_down(MouseButton::Left, move |e: &MouseDownEvent, window, cx| {
                window.focus(&focus, cx);
                if let Some(press) = &press {
                    press(e.position.x, window, cx);
                }
            })
            .on_click(move |e: &ClickEvent, window, cx| {
                if e.click_count() == 2
                    && let Some(set) = &reset
                {
                    set(SplitterKey::Reset, window, cx);
                }
            })
            .on_key_down(move |e, window, cx| {
                if let Some(key) = splitter_key(&e.keystroke, pane, now, min, max)
                    && let Some(set) = &on_set
                {
                    cx.stop_propagation();
                    set(key, window, cx);
                }
            })
            .child(line)
            .children(self.probe)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn key(s: &'static str) -> Keystroke {
        Keystroke::parse(s).unwrap()
    }

    #[test]
    fn arrows_move_by_one_rem_and_shift_by_four() {
        let k = |s: &'static str, pane| splitter_key(&key(s), pane, 244.0, 192.0, 384.0);
        assert_eq!(k("right", Pane::Sidebar), Some(SplitterKey::To(260.0)));
        assert_eq!(k("left", Pane::Sidebar), Some(SplitterKey::To(228.0)));
        assert_eq!(
            k("shift-right", Pane::Sidebar),
            Some(SplitterKey::To(308.0))
        );
        // The aside's splitter is on its left edge: Right narrows.
        assert_eq!(k("right", Pane::Aside), Some(SplitterKey::To(228.0)));
        assert_eq!(k("left", Pane::Aside), Some(SplitterKey::To(260.0)));
    }

    #[test]
    fn home_end_enter_and_foreign_chords() {
        let k = |s: &'static str| splitter_key(&key(s), Pane::Sidebar, 244.0, 192.0, 384.0);
        assert_eq!(k("home"), Some(SplitterKey::To(192.0)));
        assert_eq!(k("end"), Some(SplitterKey::To(384.0)));
        assert_eq!(k("enter"), Some(SplitterKey::Reset));
        assert_eq!(k("ctrl-right"), None);
        assert_eq!(k("alt-right"), None);
        assert_eq!(k("a"), None);
    }
}
