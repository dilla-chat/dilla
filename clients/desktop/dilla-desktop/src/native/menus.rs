//! The application menu bar and the window menu (Package 5; beta.5 `Menu.setApplicationMenu`): Dilla (the tray's
//! entries), Edit, View (toggle full screen), Window; and macOS reopen (`activate`).
//!
//! - **Dilla** shows the tray's rows as they are now (the same snapshot, [`super::tray::snapshot`]), so a row added by
//!   another package (a ring, the updater, application audio) is in both. beta.5's menu was exactly this
//!   (main.ts:362, `{ label: 'Dilla', submenu: template }`, no `appMenu` role). On macOS the menu also carries the
//!   system's app-menu rows ([`mac_app_items`]): About Dilla, Settings… ⌘, Services, Hide Dilla ⌘H, Hide Others ⌥⌘H,
//!   Show All, and Quit Dilla ⌘Q, which asks the quit guards ([`super::Quit`]) as the tray's row does.
//! - **Edit** is the system's (undo, redo, cut, copy, paste, select all on the focused text field).
//! - **View** has Toggle Full Screen (F11; Control-⌘-F on macOS). beta.5's Reset Zoom, Zoom In and Zoom Out are not
//!   offered: the native client draws at fixed sizes (pixel parity), and Jonas decided to skip zoom (2026-11-15).
//! - **Window**: Minimize (⌘M on macOS), Zoom and Close Window, on the window in front; on macOS also Bring All to Front
//!   (Electron's `windowMenu` role).
//! - **Reopen** (a click on the dock icon with no window): the first window, else the manager
//!   ([`NativeShell::show_app`]).
//!
//! GPUI draws a menu bar on macOS only; on Linux and Windows the same rows are in the tray.

use gpui::{
    Action, App, Application, KeyBinding, Menu, MenuItem, OsAction, SharedString, SystemMenuType,
    actions,
};

use super::{NativeShell, TrayItem};

actions!(
    dilla_native,
    [
        /// View → Toggle Full Screen.
        ToggleFullScreen,
        /// Window → Minimize.
        Minimize,
        /// Window → Zoom.
        ZoomWindow,
        /// Window → Close Window.
        CloseWindow,
        /// Window → Bring All to Front (macOS).
        BringAllToFront,
        /// Dilla → About Dilla (macOS: the system's About panel).
        About,
        /// Dilla → Hide Dilla (macOS).
        Hide,
        /// Dilla → Hide Others (macOS).
        HideOthers,
        /// Dilla → Show All (macOS).
        ShowAll
    ]
);

/// A Dilla-menu row: the tray row at `path` of the snapshot `generation`.
#[derive(Clone, Debug, PartialEq, Eq, Action)]
#[action(namespace = dilla_native, no_json)]
pub struct TrayRow {
    pub generation: u64,
    pub path: Vec<usize>,
}

/// The Dilla menu's name.
pub const DILLA: &str = "Dilla";

/// Called once by [`super::prepare`], before the run loop: macOS reopen brings the app forward.
pub fn prepare(app: &Application) {
    app.on_reopen(NativeShell::show_app);
}

fn on_window(cx: &mut App, f: impl FnOnce(&mut gpui::Window) + 'static) {
    if let Some(window) = cx.active_window() {
        let _ = window.update(cx, |_, window, _| f(window));
    }
}

/// Called once by [`super::install`]: the actions, their keys and the menu bar.
pub fn install(cx: &mut App) {
    cx.on_action(|row: &TrayRow, cx| super::tray::choose(row.generation, row.path.clone(), cx));
    cx.on_action(|_: &ToggleFullScreen, cx| on_window(cx, |w| w.toggle_fullscreen()));
    cx.on_action(|_: &Minimize, cx| on_window(cx, |w| w.minimize_window()));
    cx.on_action(|_: &ZoomWindow, cx| on_window(cx, |w| w.zoom_window()));
    cx.on_action(|_: &CloseWindow, cx| on_window(cx, |w| w.remove_window()));
    // Quit with no window in front (the app in the tray, the menu bar still up); a window's root takes it first.
    cx.on_action(|_: &super::Quit, cx| NativeShell::request_quit(cx));
    cx.on_action(|_: &Hide, cx| cx.hide());
    cx.on_action(|_: &HideOthers, cx| cx.hide_other_apps());
    cx.on_action(|_: &ShowAll, cx| cx.unhide_other_apps());
    cx.on_action(|_: &About, _| mac::about());
    cx.on_action(|_: &BringAllToFront, _| mac::bring_all_to_front());
    cx.bind_keys(key_bindings(cfg!(target_os = "macos")));
    refresh(cx);
}

/// The application menu's keys: full screen everywhere (Control-⌘-F on macOS, F11 elsewhere); on macOS also Quit ⌘Q,
/// Hide ⌘H, Hide Others ⌥⌘H and Minimize ⌘M, as an AppKit app has them. Settings ⌘, is the shell's own binding
/// (`screens::shell::keys`).
pub fn key_bindings(mac: bool) -> Vec<KeyBinding> {
    if mac {
        vec![
            KeyBinding::new("ctrl-cmd-f", ToggleFullScreen, None),
            KeyBinding::new("cmd-q", super::Quit, None),
            KeyBinding::new("cmd-h", Hide, None),
            KeyBinding::new("alt-cmd-h", HideOthers, None),
            KeyBinding::new("cmd-m", Minimize, None),
        ]
    } else {
        vec![KeyBinding::new("f11", ToggleFullScreen, None)]
    }
}

/// The system's app-menu rows on macOS, round the tray's: About Dilla and Settings… first; then the tray's rows;
/// Services, the Hide rows and Quit Dilla last (`cmd-q`, the quit guards). The tray's own "Quit Dilla" row (its last,
/// after a separator) gives way to this one, so the menu has one Quit.
pub fn mac_app_items(tray_rows: Vec<MenuItem>) -> Vec<MenuItem> {
    let mut rows = tray_rows;
    if matches!(rows.last(), Some(MenuItem::Action { name, .. }) if name.as_ref() == super::tray::QUIT_DILLA)
    {
        rows.pop();
    }
    while matches!(rows.last(), Some(MenuItem::Separator)) {
        rows.pop();
    }
    let mut items = vec![
        MenuItem::action("About Dilla", About),
        MenuItem::separator(),
        MenuItem::action("Settings…", crate::screens::shell::keys::OpenSettings),
        MenuItem::separator(),
    ];
    items.extend(rows);
    items.extend([
        MenuItem::separator(),
        MenuItem::os_submenu("Services", SystemMenuType::Services),
        MenuItem::separator(),
        MenuItem::action("Hide Dilla", Hide),
        MenuItem::action("Hide Others", HideOthers),
        MenuItem::action("Show All", ShowAll),
        MenuItem::separator(),
        MenuItem::action(super::tray::QUIT_DILLA, super::Quit),
    ]);
    items
}

/// AppKit's own About panel and window ordering (macOS); nothing elsewhere, where the menu bar is not drawn.
mod mac {
    #[cfg(target_os = "macos")]
    pub fn about() {
        if let Some(mtm) = objc2::MainThreadMarker::new() {
            objc2_app_kit::NSApplication::sharedApplication(mtm).orderFrontStandardAboutPanel(None);
        }
    }

    #[cfg(target_os = "macos")]
    pub fn bring_all_to_front() {
        if let Some(mtm) = objc2::MainThreadMarker::new() {
            objc2_app_kit::NSApplication::sharedApplication(mtm).arrangeInFront(None);
        }
    }

    #[cfg(not(target_os = "macos"))]
    pub fn about() {}

    #[cfg(not(target_os = "macos"))]
    pub fn bring_all_to_front() {}
}

fn dilla_items(generation: u64, items: &[TrayItem], prefix: &[usize]) -> Vec<MenuItem> {
    items
        .iter()
        .enumerate()
        .map(|(i, item)| {
            let mut path = prefix.to_vec();
            path.push(i);
            match item {
                TrayItem::Action { label, enabled, .. } => {
                    MenuItem::action(label.clone(), TrayRow { generation, path }).disabled(!enabled)
                }
                TrayItem::Check { label, checked, .. } => {
                    MenuItem::action(label.clone(), TrayRow { generation, path }).checked(*checked)
                }
                TrayItem::Submenu { label, items } => MenuItem::submenu(
                    Menu::new(label.clone()).items(dilla_items(generation, items, &path)),
                ),
                TrayItem::Separator => MenuItem::separator(),
            }
        })
        .collect()
}

/// The menu bar now: Dilla (the tray's rows), Edit, View, Window.
pub fn menus(cx: &App) -> Vec<Menu> {
    let (generation, items) = super::tray::snapshot(cx);
    use gpui_component::input::{Copy, Cut, Paste, Redo, SelectAll, Undo};
    let mac = cfg!(target_os = "macos");
    let mut dilla = dilla_items(generation, &items, &[]);
    if mac {
        dilla = mac_app_items(dilla);
    }
    let mut window = vec![
        MenuItem::action("Minimize", Minimize),
        MenuItem::action("Zoom", ZoomWindow),
        MenuItem::separator(),
        MenuItem::action("Close Window", CloseWindow),
    ];
    if mac {
        window.extend([
            MenuItem::separator(),
            MenuItem::action("Bring All to Front", BringAllToFront),
        ]);
    }
    vec![
        Menu::new(DILLA).items(dilla),
        Menu::new("Edit").items([
            MenuItem::os_action("Undo", Undo, OsAction::Undo),
            MenuItem::os_action("Redo", Redo, OsAction::Redo),
            MenuItem::separator(),
            MenuItem::os_action("Cut", Cut, OsAction::Cut),
            MenuItem::os_action("Copy", Copy, OsAction::Copy),
            MenuItem::os_action("Paste", Paste, OsAction::Paste),
            MenuItem::os_action("Select All", SelectAll, OsAction::SelectAll),
        ]),
        Menu::new("View").items([MenuItem::action("Toggle Full Screen", ToggleFullScreen)]),
        Menu::new("Window").items(window),
    ]
}

/// The tray changed: the menu bar's Dilla menu follows (the tray calls this on every redraw).
pub fn refresh(cx: &mut App) {
    let menus = menus(cx);
    cx.set_menus(menus);
}

/// The menu bar's names, as drawn (tests).
pub fn menu_names(cx: &App) -> Vec<SharedString> {
    menus(cx).into_iter().map(|m| m.name).collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The menu bar is beta.5's: Dilla with the tray's rows, Edit, View, Window.
    #[gpui::test]
    fn the_menu_bar_is_dilla_edit_view_window(cx: &mut gpui::TestAppContext) {
        cx.update(|cx| {
            super::super::tray::install_sections(cx);
            assert_eq!(menu_names(cx), ["Dilla", "Edit", "View", "Window"]);
            let dilla = menus(cx).remove(0);
            let names = names(&dilla.items);
            if cfg!(target_os = "macos") {
                assert_eq!(names.first().map(String::as_str), Some("About Dilla"));
                assert_eq!(names.last().map(String::as_str), Some("Quit Dilla"));
            } else {
                assert_eq!(names, ["Your instances…", "--", "--", "Quit Dilla"]);
            }
        });
    }

    fn names(items: &[MenuItem]) -> Vec<String> {
        items
            .iter()
            .map(|i| match i {
                MenuItem::Action { name, .. } => name.to_string(),
                MenuItem::Separator => "--".into(),
                MenuItem::SystemMenu(m) => m.name.to_string(),
                _ => "?".into(),
            })
            .collect()
    }

    /// The macOS app menu: the system's rows round the tray's, one Quit Dilla (the guarded `Quit`, ⌘Q) and last.
    #[test]
    fn the_mac_app_menu_has_the_system_rows_and_one_quit() {
        let tray = vec![
            MenuItem::action("Your instances…", ShowAll),
            MenuItem::separator(),
            MenuItem::separator(),
            MenuItem::action(super::super::tray::QUIT_DILLA, ShowAll),
        ];
        let items = mac_app_items(tray);
        assert_eq!(
            names(&items),
            [
                "About Dilla",
                "--",
                "Settings…",
                "--",
                "Your instances…",
                "--",
                "Services",
                "--",
                "Hide Dilla",
                "Hide Others",
                "Show All",
                "--",
                "Quit Dilla"
            ]
        );
        let Some(MenuItem::Action { action, .. }) = items.last() else {
            panic!("Quit Dilla is an action");
        };
        assert!(action.partial_eq(&super::super::Quit), "the guarded quit");
    }

    /// macOS: ⌘Q is the quit action (through the quit guards), with ⌘H, ⌥⌘H and ⌘M; Linux and Windows bind only F11.
    #[test]
    fn the_mac_keymap_binds_cmd_q_to_quit() {
        let mac = key_bindings(true);
        let bound = |keys: &'static str| {
            let wanted = gpui::Keystroke::parse(keys).unwrap();
            mac.iter()
                .find(|b| matches!(b.keystrokes(), [k] if *k.inner() == wanted))
                .map(|b| b.action().boxed_clone())
        };
        assert!(bound("cmd-q").is_some_and(|a| a.partial_eq(&super::super::Quit)));
        assert!(bound("cmd-h").is_some_and(|a| a.partial_eq(&Hide)));
        assert!(bound("alt-cmd-h").is_some_and(|a| a.partial_eq(&HideOthers)));
        assert!(bound("cmd-m").is_some_and(|a| a.partial_eq(&Minimize)));
        assert!(bound("ctrl-cmd-f").is_some_and(|a| a.partial_eq(&ToggleFullScreen)));
        let other = key_bindings(false);
        assert_eq!(other.len(), 1);
        assert!(other[0].action().partial_eq(&ToggleFullScreen));
    }
}
