//! The reactions area's key bindings (Task 10): `crate::keys::bind_all` calls [`bind`] once per app.
//!
//! None are global. The web's `Keyboard.tsx` lists no shortcut for reactions: the emoji grid's arrows, Home, End, Enter,
//! Space and Escape, and the bar's arrows, are handled on the elements themselves (`EmojiGrid.tsx`, `ReactionBar.tsx`), and
//! so they are here (`dilla-desktop-ui`'s `components::reactions`), where a key reaches them only while one of their
//! elements holds the focus.

use gpui::App;

/// Registers this area's bindings: none.
pub fn bind(_cx: &mut App) {}
