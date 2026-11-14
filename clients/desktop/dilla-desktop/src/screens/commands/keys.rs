//! The bot area's keys (Task 20): `crate::keys::bind_all` calls [`bind`] once per app.
//!
//! The composer's field keeps the focus while the `/` list is open (the web's `aria-activedescendant` list), so the
//! list's keys are taken before the field sees them: an interceptor answers ↑↓ (move, wrapping), Tab and Enter (pick
//! the highlighted command) and Escape (close the list, keep the draft) while the last frame drew the list, and lets
//! every other key through. `/` itself opens the list by being typed: the composer reports the draft
//! ([`super::draft_changed`]).

use gpui::{App, Global};

use super::PickerState;
use super::picker::{self, Effect, Key};

/// App-wide by design (final review sweep): the key interceptor is installed once per app; what it acts on is the window that took the
/// key (`PickerState` is per window).
struct Bound;
impl Global for Bound {}

/// Installs the picker's key interceptor (once per app).
pub fn bind(cx: &mut App) {
    if cx.has_global::<Bound>() {
        return;
    }
    cx.set_global(Bound);
    cx.intercept_keystrokes(|event, window, cx| {
        let stroke = &event.keystroke;
        if stroke.modifiers.control || stroke.modifiers.alt || stroke.modifiers.platform {
            return;
        }
        if chips_key(window.window_handle().window_id(), stroke, cx) {
            cx.stop_propagation();
            window.refresh();
            return;
        }
        let key = match (stroke.key.as_str(), stroke.modifiers.shift) {
            ("up", false) => Key::Up,
            ("down", false) => Key::Down,
            ("tab", false) => Key::Tab,
            ("enter", false) => Key::Enter,
            ("escape", false) => Key::Escape,
            _ => return,
        };
        if apply(window.window_handle().window_id(), key, cx) {
            cx.stop_propagation();
            window.refresh();
        }
    })
    .detach();
}

/// Applies `key` to the open list; `false` when the list is closed or the key is not the list's.
pub fn apply(window: gpui::WindowId, key: Key, cx: &mut App) -> bool {
    let Some(state) = crate::per_window::get::<PickerState>(window, cx) else {
        return false;
    };
    let Some(rows) = state.rows.as_ref() else {
        return false;
    };
    match picker::key(rows.len(), state.selected, key) {
        Effect::Pass => false,
        Effect::Select(i) => {
            super::picker_state_mut(window, cx).selected = i;
            true
        }
        Effect::Pick(i) => {
            pick(window, i, cx);
            true
        }
        Effect::Close { .. } => {
            let s = super::picker_state_mut(window, cx);
            s.closed_for = Some(s.draft.clone());
            s.rows = None;
            true
        }
    }
}

/// Picks row `i` of the open list: it becomes the command the chips are for, and the list closes.
pub fn pick(window: gpui::WindowId, i: usize, cx: &mut App) {
    let s = super::picker_state_mut(window, cx);
    let Some(id) = s.rows.as_ref().and_then(|r| r.get(i)).cloned() else {
        return;
    };
    s.selected = i;
    s.picked = Some(id.clone());
    s.closed_for = Some(s.draft.clone());
    s.rows = None;
    // The composer's field becomes the command's chips (CommandPicker.tsx `enterCommand`).
    s.chips = Some(super::chips::Chips::new(id));
}

/// A key while `window`'s composer shows chips: typing fills the active field, Backspace deletes its last character,
/// Tab and Shift+Tab move between fields, Escape leaves the chips (the typed draft stays, its list closed). Enter is the
/// composer's: its send runs the chips' typed form (`super::chips::draft`). `false` with no chips, or for a key they
/// leave alone.
pub fn chips_key(window: gpui::WindowId, stroke: &gpui::Keystroke, cx: &mut App) -> bool {
    let Some(state) = crate::per_window::get::<PickerState>(window, cx) else {
        return false;
    };
    if state.chips.is_none() {
        return false;
    }
    let s = super::picker_state_mut(window, cx);
    let Some(chips) = s.chips.as_mut() else {
        return false;
    };
    // The active chip's suggestions take ↑↓, and Tab or Enter while one can go in the field.
    if chips.suggestion_key(&stroke.key, stroke.modifiers.shift) {
        return true;
    }
    match stroke.key.as_str() {
        "enter" => return false,
        "escape" => {
            s.chips = None;
            return true;
        }
        "tab" => chips.step(stroke.modifiers.shift),
        "backspace" => {
            chips.active_value().pop();
        }
        _ => match stroke.key_char.as_deref() {
            Some(text) if !text.is_empty() && !text.chars().any(char::is_control) => {
                chips.active_value().push_str(text);
            }
            _ => return false,
        },
    }
    true
}
