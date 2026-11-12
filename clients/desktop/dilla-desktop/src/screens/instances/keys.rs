//! The instances area's key bindings (Task 27): `crate::keys::bind_all` calls [`bind`] once per app.
//!
//! The manager needs none of its own. Tab and Shift+Tab are the shared focus keys (`screens::bind_keys`), the
//! confirmation dialog's keys are the dialog component's, and the list's Up, Down, Home, End, Enter and Delete are
//! read from the focused row's key-down (`view::ManagerView::row_key`), because they apply to the row that has focus.

use gpui::App;

/// Registers this area's bindings: none.
pub fn bind(_cx: &mut App) {}
