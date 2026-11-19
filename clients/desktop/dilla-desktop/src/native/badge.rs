//! The unread badge (Package 5; beta.5 `app.setBadgeCount(sum of unread)`).
//!
//! - **The count.** Every client reports its total ([`attach`], from the same per-frame hook as the notifier, then on
//!   every change of its state, so a window closed to the tray keeps counting): `features::notify::total_badge`, a
//!   muted channel's mentions only. The shell sums the instances ([`NativeShell::set_badge`]).
//! - **macOS**: the dock tile's badge label (`NSDockTile.badgeLabel`), cleared at 0.
//! - **Linux**: the Unity `LauncherEntry` D-Bus signal (`count`, `count-visible`), which KDE Plasma, Dash to Dock and
//!   Ubuntu's dock read, for the desktop entry `<app id>.desktop` (Electron's `setBadgeCount` did the same).
//! - **Windows**: none. Electron's `setBadgeCount` did nothing on Windows; beta.5 showed the count in the tray's
//!   tooltip only, as here ([`super::tray`]).

use std::rc::Rc;

use dilla_client::Client;
use dilla_client::features::notify::total_badge;
use dilla_client::seams::InstanceId;
use gpui::{App, Global, Task, Window};

use super::NativeShell;
use crate::slots::SlotCx;

/// One client's watcher.
struct Watch {
    client: usize,
    instance: InstanceId,
    _task: Task<()>,
}

#[derive(Default)]
struct Watches(Vec<Watch>);

impl Global for Watches {}

/// Every frame of a window that shows a client's workspace (`screens::ring::effects::sync`): starts, once per client,
/// the watcher that reports its unread total. A window the shell does not know (a fixture's) starts nothing.
pub fn attach(ctx: &SlotCx, window: &Window, cx: &mut App) {
    let Some(client) = &ctx.client else { return };
    let id = window.window_handle().window_id();
    let Some(instance) = NativeShell::open_windows(cx)
        .into_iter()
        .find(|(w, _)| *w == id)
        .and_then(|(_, i)| i)
    else {
        return;
    };
    let key = Rc::as_ptr(client) as usize;
    let watches = cx.default_global::<Watches>();
    if watches.0.iter().any(|w| w.client == key) {
        return;
    }
    watches.0.retain(|w| w.instance != instance);
    let mut state = client.state();
    let prefs = client.prefs();
    let task = cx.spawn(async move |cx| {
        loop {
            let total = total_badge(&state.borrow_and_update(), &prefs);
            cx.update(|cx| NativeShell::set_badge(instance, total, cx));
            if state.changed().await.is_err() {
                break;
            }
        }
    });
    cx.default_global::<Watches>().0.push(Watch {
        client: key,
        instance,
        _task: task,
    });
}

/// Lets go of `client` (Forget, sign-out): its watcher stops and its instance counts nothing.
pub fn release(client: &Rc<Client>, cx: &mut App) {
    if !cx.has_global::<Watches>() {
        return;
    }
    let key = Rc::as_ptr(client) as usize;
    let watches = cx.global_mut::<Watches>();
    let gone: Vec<InstanceId> = watches
        .0
        .iter()
        .filter(|w| w.client == key)
        .map(|w| w.instance)
        .collect();
    watches.0.retain(|w| w.client != key);
    for id in gone {
        NativeShell::set_badge(id, 0, cx);
    }
}

/// The text a badge shows for `n` (none at 0).
pub fn label(n: u32) -> Option<String> {
    (n > 0).then(|| n.to_string())
}

/// Called once by [`super::install`]: the platform's badge as a sink of the total.
pub fn install(cx: &mut App) {
    let sink = os::sink();
    NativeShell::add_badge_sink(Box::new(move |n, _| sink(n)), cx);
}

#[cfg(target_os = "macos")]
mod os {
    use objc2::MainThreadMarker;
    use objc2_app_kit::NSApplication;
    use objc2_foundation::NSString;

    pub fn sink() -> impl Fn(u32) {
        |n| {
            // The sinks run on the UI thread, which is AppKit's main thread.
            let Some(mtm) = MainThreadMarker::new() else {
                return;
            };
            let tile = NSApplication::sharedApplication(mtm).dockTile();
            let text = super::label(n).map(|s| NSString::from_str(&s));
            tile.setBadgeLabel(text.as_deref());
        }
    }
}

#[cfg(target_os = "linux")]
mod os {
    use std::collections::HashMap;

    use zbus::zvariant::Value;

    /// The LauncherEntry object path (any path works; the desktop entry is named in the signal).
    const PATH: &str = "/dev/dilla/desktop/LauncherEntry";

    /// `application://<app id>.desktop`.
    pub fn app_uri() -> String {
        format!("application://{}.desktop", crate::LINUX_APP_ID)
    }

    /// The signal's properties for `n`.
    pub fn properties(n: u32) -> HashMap<&'static str, Value<'static>> {
        HashMap::from([
            ("count", Value::from(i64::from(n))),
            ("count-visible", Value::from(n > 0)),
        ])
    }

    pub fn sink() -> impl Fn(u32) {
        // One thread owns the session bus connection; the newest count wins.
        let (tx, rx) = std::sync::mpsc::channel::<u32>();
        let spawned = std::thread::Builder::new()
            .name("dilla-badge".into())
            .spawn(move || {
                let Ok(conn) = zbus::blocking::Connection::session() else {
                    log::info!("dilla-desktop: no session bus; no launcher badge");
                    return;
                };
                while let Ok(mut n) = rx.recv() {
                    while let Ok(newer) = rx.try_recv() {
                        n = newer;
                    }
                    let sent = conn.emit_signal(
                        None::<()>,
                        PATH,
                        "com.canonical.Unity.LauncherEntry",
                        "Update",
                        &(app_uri(), properties(n)),
                    );
                    if let Err(e) = sent {
                        log::debug!("dilla-desktop: the launcher badge was not sent: {e}");
                    }
                }
            });
        if let Err(e) = spawned {
            log::warn!("dilla-desktop: no launcher badge thread: {e}");
        }
        move |n| {
            let _ = tx.send(n);
        }
    }
}

#[cfg(not(any(target_os = "macos", target_os = "linux")))]
mod os {
    /// Windows (and anything else): the tray's tooltip says the count.
    pub fn sink() -> impl Fn(u32) {
        |_| {}
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The badge is the sum of every instance's unread, and a released instance counts nothing.
    #[gpui::test]
    fn sum_of_unread_across_instances(cx: &mut gpui::TestAppContext) {
        let heard = Rc::new(std::cell::RefCell::new(Vec::new()));
        let seen = heard.clone();
        cx.update(|cx| {
            NativeShell::add_badge_sink(Box::new(move |n, _| seen.borrow_mut().push(n)), cx);
            NativeShell::set_badge(InstanceId(0), 3, cx);
            NativeShell::set_badge(InstanceId(5), 4, cx);
            NativeShell::set_badge(InstanceId(0), 1, cx);
            assert_eq!(NativeShell::badge(cx), 5);
        });
        assert_eq!(*heard.borrow(), [0, 3, 7, 5]);
        assert_eq!(label(0), None);
        assert_eq!(label(12).as_deref(), Some("12"));
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn the_launcher_entry_names_the_desktop_entry_and_hides_zero() {
        assert_eq!(
            os::app_uri(),
            format!("application://{}.desktop", crate::LINUX_APP_ID)
        );
        let p = os::properties(0);
        assert_eq!(p["count-visible"], zbus::zvariant::Value::from(false));
        let p = os::properties(9);
        assert_eq!(p["count"], zbus::zvariant::Value::from(9i64));
        assert_eq!(p["count-visible"], zbus::zvariant::Value::from(true));
    }
}
