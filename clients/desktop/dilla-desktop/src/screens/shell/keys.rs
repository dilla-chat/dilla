//! The shell's key bindings (Task 8): `crate::keys::bind_all` calls [`bind`] once per app. They apply inside the shell
//! (key context `Shell`), so a text field's own bindings, deeper in the tree, win.
//!
//! - Alt+Up and Alt+Down select the previous and next listed channel (Shell.tsx, from anywhere on a channel route).
//! - Alt+Left and Alt+Right go back and forward in the route history (the browser's history in the web client).
//! - Ctrl+, (Cmd+, on macOS) opens the user settings (the settings host, Task 1b).
//! - Ctrl+End (Cmd+End) jumps the open conversation to its newest message while it is away from it (Conversation.tsx's
//!   "jump to present"); a text field keeps the chord (it moves the caret), an overlay keeps it.
//! - Ctrl+Shift+Enter (Cmd+Shift+Enter) starts the open DM's call (Shell.tsx `dmCallKey`), as its header button does.
//!
//! The rail, the channel list and the splitters take their own keys where they are focused (roving arrows, Enter, Home
//! and End); Tab and Shift+Tab are the shared focus keys of `crate::screens::bind_keys`.

use gpui::{App, KeyBinding, actions};

actions!(
    dilla_shell,
    [
        PreviousChannel,
        NextChannel,
        HistoryBack,
        HistoryForward,
        OpenSettings,
        JumpToPresent,
        CallOpenDm
    ]
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
        // Integration 3 (the settings host): Ctrl+, (Cmd+, on macOS) opens the user settings.
        KeyBinding::new("ctrl-,", OpenSettings, Some(CONTEXT)),
        KeyBinding::new("cmd-,", OpenSettings, Some(CONTEXT)),
        // Task 28: the two rows of Settings -> Keyboard that had no binding.
        KeyBinding::new("ctrl-end", JumpToPresent, Some(CONTEXT)),
        KeyBinding::new("cmd-end", JumpToPresent, Some(CONTEXT)),
        KeyBinding::new("ctrl-shift-enter", CallOpenDm, Some(CONTEXT)),
        KeyBinding::new("cmd-shift-enter", CallOpenDm, Some(CONTEXT)),
    ]);
}
