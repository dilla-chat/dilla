//! The threads area's key bindings (Task 11): `crate::keys::bind_all` calls [`bind`] once per app.
//!
//! Escape inside the pane closes the thread and nothing more (ThreadPane.tsx `onKeyDown`); focus goes back to the thread's
//! chip. The toolbar's reply action and the thread pane's composer need no binding of their own: Enter sends (the field's
//! `submit_on_enter`), and the web's reply-bar Escape is the composer's (Task 9).

use gpui::{App, KeyBinding, actions};

actions!(threads, [ClosePane]);

/// The key context of the pane's root.
pub const CONTEXT: &str = "ThreadPane";

/// Registers this area's bindings.
pub fn bind(cx: &mut App) {
    cx.bind_keys([KeyBinding::new("escape", ClosePane, Some(CONTEXT))]);
}
