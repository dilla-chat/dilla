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
        // t28-bot-review I1: only while the composer the list and chips belong to holds the focus. Keys typed in a
        // settings field, a search, a dialog, or with nothing focused (push to talk) are never theirs.
        if !takes_keys(window, cx) {
            return;
        }
        if chips_key(window.window_handle().window_id(), stroke, cx) {
            cx.stop_propagation();
            window.refresh();
            return;
        }
        if stroke.modifiers.control || stroke.modifiers.alt || stroke.modifiers.platform {
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

/// Whether `window`'s list and chips take its keys: the composer that reported the draft holds the focus.
pub fn takes_keys(window: &gpui::Window, cx: &App) -> bool {
    crate::per_window::get::<PickerState>(window.window_handle().window_id(), cx)
        .and_then(|s| s.composer.as_ref()?.upgrade())
        .is_some_and(|focus| focus.is_focused(window))
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
    let m = stroke.modifiers;
    // A shortcut (Cmd, or Ctrl without Alt) is the app's; AltGr (Ctrl+Alt on Linux and Windows) and Option (macOS)
    // type characters (t28-bot-review I2).
    let shortcut = m.platform || (m.control && !m.alt);
    match stroke.key.as_str() {
        // Shift+Enter would be a newline in the hidden field, which drops the chips: swallowed.
        "enter" => return m.shift && !shortcut,
        "escape" if !shortcut => {
            s.chips = None;
            return true;
        }
        "tab" if !shortcut => chips.step(m.shift),
        "backspace" if !shortcut => {
            chips.active_value().pop();
        }
        _ if shortcut => return false,
        _ => match stroke.key_char.as_deref() {
            Some(text) if !text.is_empty() && !text.chars().any(char::is_control) => {
                chips.active_value().push_str(text);
            }
            _ => return false,
        },
    }
    true
}

#[cfg(test)]
mod tests {
    use super::*;
    use gpui::{Keystroke, Modifiers};

    fn stroke(key: &str, key_char: Option<&str>, modifiers: Modifiers) -> Keystroke {
        Keystroke {
            modifiers,
            key: key.into(),
            key_char: key_char.map(Into::into),
        }
    }

    fn chipped(window: gpui::WindowId, cx: &mut App) {
        super::super::draft_changed(window, "/remind", cx);
        let s = super::super::picker_state_mut(window, cx);
        let mut chips = super::super::chips::Chips::new("remind".into());
        chips.fields = 2;
        s.chips = Some(chips);
    }

    /// t28-bot-review I2: AltGr (Ctrl+Alt on Linux and Windows) and Option (macOS) characters are typed into the
    /// active chip; Shift+Enter is swallowed; a shortcut (Cmd, or Ctrl without Alt) and Enter are left to the app.
    #[gpui::test]
    fn altgr_and_option_characters_land_in_the_active_chip(cx: &mut gpui::TestAppContext) {
        let w = cx
            .add_empty_window()
            .update(|w, _| w.window_handle().window_id());
        cx.update(|cx| {
            chipped(w, cx);
            let altgr = Modifiers {
                control: true,
                alt: true,
                ..Default::default()
            };
            let option = Modifiers {
                alt: true,
                ..Default::default()
            };
            assert!(chips_key(w, &stroke("2", Some("@"), altgr), cx));
            assert!(chips_key(w, &stroke("8", Some("["), option), cx));
            assert!(chips_key(w, &stroke("enter", None, Modifiers::shift()), cx));
            assert!(!chips_key(w, &stroke("v", None, Modifiers::control()), cx));
            assert!(!chips_key(
                w,
                &stroke("v", Some("v"), Modifiers::command()),
                cx
            ));
            assert!(!chips_key(w, &stroke("enter", None, Modifiers::none()), cx));
            let s = super::super::picker_state(w, cx);
            assert_eq!(s.chips.expect("chips kept").values, ["@["]);
        });
    }

    /// t28-bot-review I1: with the focus anywhere but the composer that holds the chips (a settings field, a dialog,
    /// the member list for push to talk), keys are not the chips'.
    #[gpui::test]
    fn chips_take_keys_only_while_their_composer_holds_the_focus(cx: &mut gpui::TestAppContext) {
        let window = cx.add_empty_window();
        window.update(|window, cx| {
            let w = window.window_handle().window_id();
            let composer = cx.focus_handle();
            let other = cx.focus_handle();
            window.focus(&composer, cx);
            super::super::set_composer(w, &composer, cx);
            chipped(w, cx);
            assert!(takes_keys(window, cx), "the composer holds the focus");
            window.focus(&other, cx);
            assert!(!takes_keys(window, cx), "a settings field holds the focus");
            window.blur(cx);
            assert!(
                !takes_keys(window, cx),
                "nothing holds the focus (push to talk)"
            );
            window.focus(&composer, cx);
            assert!(takes_keys(window, cx));
        });
    }
}
