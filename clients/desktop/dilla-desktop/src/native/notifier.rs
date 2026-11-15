//! Operating-system notifications (Package 4; beta.5 `main.ts:484-512`, `notify`, `notify-ring`,
//! `dismiss-notification`).
//!
//! To do (Package 4): an `OsNotifier` implementing `dilla_client::features::notify::Notifier` on
//! `App::show_system_notification`; a click focuses the window and opens the conversation; at most 64 live, keyed per
//! instance; [`install`] adds a [`super::NativeShell::on_window_event`] listener that withdraws a closed window's
//! notifications ([`super::WindowEvent::Closed`]).

use gpui::App;

/// Called once by [`super::install`]. Task 0: nothing yet.
pub fn install(_cx: &mut App) {}
