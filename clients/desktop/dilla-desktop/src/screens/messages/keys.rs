//! The messages area's key bindings (Task 9): `crate::keys::bind_all` calls [`bind`] once per app.
//!
//! PageUp and PageDown scroll the log while it holds the focus, and Ctrl+Home takes it to its top. Enter, Shift+Enter, Up
//! and Escape in the composer and the editor, and the mention list's keys, are the fields' own (`screens/composer.rs`,
//! `messages/editor.rs`).

use gpui::{App, KeyBinding};

use crate::screens::timeline::{LogTop, PageDown, PageUp};

/// Registers this area's bindings.
pub fn bind(cx: &mut App) {
    cx.bind_keys([
        KeyBinding::new("pageup", PageUp, Some("MessageLog")),
        KeyBinding::new("pagedown", PageDown, Some("MessageLog")),
        // The web's log is a focused `tabIndex=0` scroller (MessageLog.tsx:131) whose key handler leaves Ctrl+Home alone
        // (:118-124, and Conversation.tsx `rowKey` returns on Ctrl), so Chromium scrolls it to its top; GPUI has no native
        // scrolling by key. Plain Home is not native there: `rowKey` (Conversation.tsx:348-354) moves to the first row,
        // which the rows' own handler does here.
        KeyBinding::new("ctrl-home", LogTop, Some("MessageLog")),
    ]);
}
