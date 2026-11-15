//! The tray icon and its menu (Package 5; beta.5 `main.ts` `refreshMenu`, `tray.on('click')`).
//!
//! To do (Package 5): put up the OS tray (`ksni` on Linux, `tray-icon` + `muda` on macOS and Windows), drawn from
//! [`NativeShell::tray_menu`] and again whenever the [`NativeShell::on_tray_changed`] callback fires; a click calls
//! [`NativeShell::show_app`]; the tooltip "Dilla — {n} unread" comes from a [`NativeShell::add_badge_sink`]; add the
//! [`TraySlot::Instances`] and [`TraySlot::Quit`] sections ("Quit Dilla" dispatches [`super::Quit`]); call
//! [`NativeShell::set_background`] once the tray is up, so closing the last window keeps the app running.
//!
//! [`NativeShell::tray_menu`]: super::NativeShell::tray_menu
//! [`NativeShell::on_tray_changed`]: super::NativeShell::on_tray_changed
//! [`NativeShell::show_app`]: super::NativeShell::show_app
//! [`NativeShell::add_badge_sink`]: super::NativeShell::add_badge_sink
//! [`NativeShell::set_background`]: super::NativeShell::set_background
//! [`TraySlot::Instances`]: super::TraySlot::Instances
//! [`TraySlot::Quit`]: super::TraySlot::Quit

use gpui::App;

/// Called once by [`super::install`]. Task 0: nothing yet.
pub fn install(_cx: &mut App) {}
