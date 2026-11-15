//! The ring area's key bindings (Task 18): `crate::keys::bind_all` calls [`bind`] once per app.
//!
//! While a ring shows and holds focus (its banner row, or one of its buttons): Enter answers and Escape declines. Enter on
//! the Decline button declines (its own, deeper context wins).

use dilla_desktop_ui::components::ring::{Answer, CONTEXT, DECLINE_CONTEXT, Decline};
use gpui::{App, KeyBinding};

/// Registers this area's bindings.
pub fn bind(cx: &mut App) {
    cx.bind_keys([
        KeyBinding::new("enter", Answer, Some(CONTEXT)),
        KeyBinding::new("escape", Decline, Some(CONTEXT)),
        KeyBinding::new("enter", Decline, Some(DECLINE_CONTEXT)),
    ]);
}
