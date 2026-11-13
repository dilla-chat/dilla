//! The shell's key bindings (Task 8): `crate::keys::bind_all` calls [`bind`] once per app. They apply inside the shell
//! (key context `Shell`), so a text field's own bindings, deeper in the tree, win.
//!
//! - Alt+Up and Alt+Down select the previous and next listed channel (Shell.tsx, from anywhere on a channel route).
//! - Alt+Left and Alt+Right go back and forward in the route history (the browser's history in the web client).
//!
//! The rail, the channel list and the splitters take their own keys where they are focused (roving arrows, Enter, Home
//! and End); Tab and Shift+Tab are the shared focus keys of `crate::screens::bind_keys`.

use gpui::{App, KeyBinding, actions};

actions!(
    dilla_shell,
    [PreviousChannel, NextChannel, HistoryBack, HistoryForward]
);

/// The key context of the shell's root.
pub const CONTEXT: &str = "Shell";

/// Registers the shell's bindings.
pub fn bind(cx: &mut App) {
    cx.bind_keys([
        KeyBinding::new("alt-up", PreviousChannel, Some(CONTEXT)),
        KeyBinding::new("alt-down", NextChannel, Some(CONTEXT)),
        KeyBinding::new("alt-left", HistoryBack, Some(CONTEXT)),
        KeyBinding::new("alt-right", HistoryForward, Some(CONTEXT)),
    ]);
}
