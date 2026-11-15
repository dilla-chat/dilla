//! The application menu bar and the window menu (Package 5; beta.5 `Menu.setApplicationMenu`): Dilla (the tray's
//! entries), Edit, View (toggle full screen), Window; and macOS reopen (`activate`).
//!
//! - **Dilla** shows the tray's rows as they are now (the same snapshot, [`super::tray::snapshot`]), so a row added by
//!   another package (a ring, the updater, application audio) is in both.
//! - **Edit** is the system's (undo, redo, cut, copy, paste, select all on the focused text field).
//! - **View** has Toggle Full Screen (F11; Control-⌘-F on macOS). beta.5's Reset Zoom, Zoom In and Zoom Out are not
//!   here yet: Electron's zoom scaled the page, and the native client draws at fixed sizes (pixel parity at 100 %), so
//!   a zoom is a design decision that waits for Jonas.
//! - **Window**: Minimize, Zoom and Close Window, on the window in front.
//! - **Reopen** (a click on the dock icon with no window): the first window, else the manager
//!   ([`NativeShell::show_app`]).
//!
//! GPUI draws a menu bar on macOS only; on Linux and Windows the same rows are in the tray.

use gpui::{Action, App, Application, KeyBinding, Menu, MenuItem, OsAction, SharedString, actions};

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
        CloseWindow
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
    let fullscreen = if cfg!(target_os = "macos") {
        "ctrl-cmd-f"
    } else {
        "f11"
    };
    cx.bind_keys([KeyBinding::new(fullscreen, ToggleFullScreen, None)]);
    refresh(cx);
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
    vec![
        Menu::new(DILLA).items(dilla_items(generation, &items, &[])),
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
        Menu::new("Window").items([
            MenuItem::action("Minimize", Minimize),
            MenuItem::action("Zoom", ZoomWindow),
            MenuItem::separator(),
            MenuItem::action("Close Window", CloseWindow),
        ]),
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
            let names: Vec<String> = dilla
                .items
                .iter()
                .map(|i| match i {
                    MenuItem::Action { name, .. } => name.to_string(),
                    MenuItem::Separator => "--".into(),
                    _ => "?".into(),
                })
                .collect();
            assert_eq!(names, ["Your instances…", "--", "--", "Quit Dilla"]);
        });
    }
}
