//! The application menu bar and the window menu (Package 5; beta.5 `Menu.setApplicationMenu`): Dilla (the tray's
//! entries), Edit, View (reset zoom, zoom in, zoom out, toggle fullscreen), Window; and macOS reopen (`activate`).
//!
//! To do (Package 5): [`prepare`] registers `Application::on_reopen` (it calls
//! [`super::NativeShell::show_app`]); [`install`] calls `App::set_menus` and binds the zoom actions.

use gpui::{App, Application};

/// Called once by [`super::prepare`], before the run loop. Task 0: nothing yet.
pub fn prepare(_app: &Application) {}

/// Called once by [`super::install`]. Task 0: nothing yet.
pub fn install(_cx: &mut App) {}
