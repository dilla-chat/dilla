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

use dilla_client::seams::InstanceId;
use gpui::App;

use super::NativeShell;

/// Called once by [`super::install`]. Task 0: nothing yet.
pub fn install(_cx: &mut App) {}

/// Brings instance `id`'s window forward, or opens it when it has none (closed to the tray). Nothing while the app
/// quits.
pub fn show_instance(id: InstanceId, cx: &mut App) {
    if NativeShell::is_quitting(cx) || NativeShell::is_asking_quit(cx) {
        return;
    }
    let window = NativeShell::open_windows(cx)
        .into_iter()
        .find(|(_, i)| *i == Some(id))
        .map(|(w, _)| w);
    if let Some(window) = window
        && let Some(handle) = cx.windows().into_iter().find(|w| w.window_id() == window)
        && handle
            .update(cx, |_, window, _| window.activate_window())
            .is_ok()
    {
        cx.activate(true);
        return;
    }
    let Some(host) = NativeShell::host(cx) else {
        return;
    };
    let Some(cfg) = host
        .launch
        .root
        .as_deref()
        .and_then(|root| dilla_client::instances::Instances::load(root).ok())
        .and_then(|list| list.get(id).cloned())
    else {
        return;
    };
    if let Err(e) = crate::windows::open_instance(cx, &host.launch, cfg, None, host.allow_loopback)
    {
        log::warn!("dilla-desktop: instance {} did not open: {e}", id.0);
    }
}
