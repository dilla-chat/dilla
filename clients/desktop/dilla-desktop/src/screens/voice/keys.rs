//! The voice area's key bindings (Task 17; packages/web/src/screens/voice/shortcuts.ts): Ctrl+Shift+M mutes,
//! Ctrl+Shift+D deafens, in a call or out of one (out of one the next join starts that way). Push to talk is the
//! dock's key listener (`super::dock`); the arrows and Escape are the stage's own (`super::stage`).
//! `crate::keys::bind_all` calls [`bind`] once per app.

use gpui::{App, KeyBinding, actions};

actions!(dilla_voice, [ToggleMute, ToggleDeafen]);

/// Registers this area's bindings and their app-wide handlers.
pub fn bind(cx: &mut App) {
    cx.bind_keys([
        KeyBinding::new("ctrl-shift-m", ToggleMute, None),
        KeyBinding::new("ctrl-shift-d", ToggleDeafen, None),
    ]);
    cx.on_action(|_: &ToggleMute, cx| super::toggle_mute(cx));
    cx.on_action(|_: &ToggleDeafen, cx| super::toggle_deafen(cx));
}

/// The key of a push-to-talk chord as gpui names it (`voice.pttChord` holds the web's `KeyboardEvent.code`-like name:
/// `Insert`, `KeyV`, `Digit1`, `Space`, `F13`).
pub fn ptt_key(chord: &str) -> String {
    let last = chord.rsplit('+').next().unwrap_or(chord);
    let key = last
        .strip_prefix("Key")
        .or_else(|| last.strip_prefix("Digit"))
        .unwrap_or(last);
    key.to_ascii_lowercase()
}

#[cfg(test)]
mod tests {
    use super::ptt_key;

    #[test]
    fn a_chord_names_its_key_as_gpui_does() {
        assert_eq!(ptt_key("Insert"), "insert");
        assert_eq!(ptt_key("KeyV"), "v");
        assert_eq!(ptt_key("Ctrl+Digit1"), "1");
        assert_eq!(ptt_key("Space"), "space");
    }
}
