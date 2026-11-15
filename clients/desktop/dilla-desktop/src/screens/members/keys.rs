//! The members area's key bindings (Task 14): `crate::keys::bind_all` calls [`bind`] once per app.
//!
//! Escape closes the profile card (and the person menu) while the focus is in the members pane. Shift+F10 and the Menu
//! key open the person menu through `ActionMenu`'s own context-menu routing (`opens_context_menu`).

use gpui::{App, KeyBinding, actions};

actions!(dilla_members, [CloseCard]);

/// Registers this area's bindings.
pub fn bind(cx: &mut App) {
    cx.bind_keys([KeyBinding::new("escape", CloseCard, Some("MembersPane"))]);
}
