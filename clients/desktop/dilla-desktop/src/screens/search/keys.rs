//! The search area's key bindings (Task 23): `crate::keys::bind_all` calls [`bind`] once per app.
//!
//! Ctrl+K and Cmd+K open the palette, and so does Ctrl+/ (the web's alternate shortcut, Settings → Keyboard
//! `keyboard.searchAll` and `keyboard.searchAlt`). An open palette ignores a second request. The web also refuses the key
//! under another overlay (a dialog, the settings frame); the shell knows its overlays, so that guard is the shell's to add
//! (Task 28). The arrows, Enter and Escape inside the palette are handled where the palette is drawn (`super`), because
//! they act on the textbox that keeps the focus.

use gpui::{App, KeyBinding, actions};

actions!(dilla_search, [OpenSearch]);

/// Registers this area's bindings.
pub fn bind(cx: &mut App) {
    cx.bind_keys([
        KeyBinding::new("ctrl-k", OpenSearch, None),
        KeyBinding::new("cmd-k", OpenSearch, None),
        KeyBinding::new("ctrl-/", OpenSearch, None),
    ]);
    cx.on_action(|_: &OpenSearch, cx| super::request_open(cx));
}
