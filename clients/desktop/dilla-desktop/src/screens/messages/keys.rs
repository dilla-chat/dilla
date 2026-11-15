//! The messages area's key bindings (Task 9): `crate::keys::bind_all` calls [`bind`] once per app.
//!
//! PageUp and PageDown scroll the log while it holds the focus. Enter, Shift+Enter, Up and Escape in the composer and the
//! editor, and the mention list's keys, are the fields' own (`screens/composer.rs`, `messages/editor.rs`).

use gpui::{App, KeyBinding};

use crate::screens::timeline::{PageDown, PageUp};

/// Registers this area's bindings.
pub fn bind(cx: &mut App) {
    cx.bind_keys([
        KeyBinding::new("pageup", PageUp, Some("MessageLog")),
        KeyBinding::new("pagedown", PageDown, Some("MessageLog")),
    ]);
}
