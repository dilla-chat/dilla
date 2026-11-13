//! The notify area's key bindings (Task 22): `crate::keys::bind_all` calls [`bind`] once per app.
//!
//! None of its own: `Keyboard.tsx` lists no binding for notifications. The section is reached with Tab and the controls
//! are Task 6's (the segmented control's arrows, the switch's Space), proved by `keyboard_notify_focus_order`.

use gpui::App;

/// Registers this area's bindings: there are none.
pub fn bind(_cx: &mut App) {}
