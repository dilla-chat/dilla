//! The title bar's menu as the OS's own popup (beta.5 `showWindowMenu`: `Menu.getApplicationMenu()?.popup(...)`): the
//! application menu ([`super::menus::menus`]) with its submenus, disabled rows and, on macOS, the system's Services
//! menu, popped up at a point of the window. macOS draws it with NSMenu (`popUpMenuPositioningItem:atLocation:inView:`)
//! and Windows with `TrackPopupMenu`, both through `muda` (already here for the tray, through `tray-icon`); GPUI has no
//! native popup menu of its own. Linux has no native menu without GTK, so the title bar draws its own list there
//! ([`crate::screens::titlebar::window_menu`]).
//!
//! Both popups run a modal loop of the OS until a row is chosen or the menu closes, and the windows keep drawing
//! meanwhile, so the popup is shown from a foreground task, outside any borrow of the app. The chosen row's action is
//! then dispatched in the window, to the element that had the focus when the menu opened (Edit's rows act on the
//! focused text field).

use gpui::{Action, App, Menu, MenuItem, Window};

/// The id of the popup's row `index`: the tray's menu event handler sends ids with this prefix here
/// ([`route`]).
pub const ID_PREFIX: &str = "dilla-popup-";

/// The row id of the action at `index`.
pub fn row_id(index: usize) -> String {
    format!("{ID_PREFIX}{index}")
}

/// The action index of a row id, if it is one of the popup's.
pub fn index_of(id: &str) -> Option<usize> {
    id.strip_prefix(ID_PREFIX)?.parse().ok()
}

/// One row of the popup, as the OS menu draws it.
#[derive(Debug, PartialEq)]
pub enum Row {
    /// An action: its index in the actions list, its label, enabled, checked.
    Action {
        index: usize,
        label: String,
        enabled: bool,
        checked: bool,
    },
    Separator,
    Submenu {
        label: String,
        rows: Vec<Row>,
    },
    /// The system's Services menu (macOS).
    Services {
        label: String,
    },
}

/// The popup's rows: one submenu per menu of the menu bar (beta.5 popped the whole application menu up), and the
/// actions by index.
pub fn rows(menus: Vec<Menu>) -> (Vec<Row>, Vec<Box<dyn Action>>) {
    fn walk(items: Vec<MenuItem>, actions: &mut Vec<Box<dyn Action>>) -> Vec<Row> {
        items
            .into_iter()
            .map(|item| match item {
                MenuItem::Separator => Row::Separator,
                MenuItem::Submenu(menu) => Row::Submenu {
                    label: menu.name.to_string(),
                    rows: walk(menu.items, actions),
                },
                MenuItem::SystemMenu(os) => Row::Services {
                    label: os.name.to_string(),
                },
                MenuItem::Action {
                    name,
                    action,
                    checked,
                    disabled,
                    ..
                } => {
                    actions.push(action);
                    Row::Action {
                        index: actions.len() - 1,
                        label: name.to_string(),
                        enabled: !disabled,
                        checked,
                    }
                }
            })
            .collect()
    }
    let mut actions = Vec::new();
    let rows = menus
        .into_iter()
        .map(|menu| Row::Submenu {
            label: menu.name.to_string(),
            rows: walk(menu.items, &mut actions),
        })
        .collect();
    (rows, actions)
}

/// Pops the application menu up at `at` (logical px from the window's top left) over `window`. Returns false where
/// there is no native popup (Linux): the caller draws its own.
pub fn show(window: &mut Window, at: (f32, f32), cx: &mut App) -> bool {
    os::show(window, at, cx)
}

/// The tray's menu event handler hands over the events of the popup's rows (muda has one handler per process).
pub fn route(id: &str) -> bool {
    os::route(id)
}

#[cfg(any(target_os = "macos", target_os = "windows"))]
mod os {
    use std::sync::Mutex;

    use gpui::{App, AppContext as _, Window};
    use raw_window_handle::{HasWindowHandle, RawWindowHandle};
    use tray_icon::menu::dpi::{LogicalPosition, Position};
    use tray_icon::menu::{
        CheckMenuItem, ContextMenu, IsMenuItem, Menu, MenuEvent, MenuItem, PredefinedMenuItem,
        Submenu,
    };

    use super::{Row, index_of, row_id};

    /// The row chosen in the popup now open, set by [`route`] while the OS's modal loop runs.
    static CHOSEN: Mutex<Option<usize>> = Mutex::new(None);

    pub fn route(id: &str) -> bool {
        match index_of(id) {
            Some(index) => {
                *CHOSEN
                    .lock()
                    .unwrap_or_else(std::sync::PoisonError::into_inner) = Some(index);
                true
            }
            None => false,
        }
    }

    fn build(rows: &[Row]) -> Vec<Box<dyn IsMenuItem>> {
        let mut out: Vec<Box<dyn IsMenuItem>> = Vec::new();
        for row in rows {
            match row {
                Row::Action {
                    index,
                    label,
                    enabled,
                    checked: true,
                } => out.push(Box::new(CheckMenuItem::with_id(
                    row_id(*index),
                    label,
                    *enabled,
                    true,
                    None,
                ))),
                Row::Action {
                    index,
                    label,
                    enabled,
                    ..
                } => out.push(Box::new(MenuItem::with_id(
                    row_id(*index),
                    label,
                    *enabled,
                    None,
                ))),
                Row::Separator => out.push(Box::new(PredefinedMenuItem::separator())),
                Row::Submenu { label, rows } => {
                    let children = build(rows);
                    let refs: Vec<&dyn IsMenuItem> = children.iter().map(|c| c.as_ref()).collect();
                    match Submenu::with_items(label, true, &refs) {
                        Ok(sub) => out.push(Box::new(sub)),
                        Err(e) => log::warn!("dilla-desktop: a menu popup submenu failed: {e}"),
                    }
                }
                Row::Services { label } => {
                    out.push(Box::new(PredefinedMenuItem::services(Some(label.as_str()))))
                }
            }
        }
        out
    }

    /// The window's native view (macOS `NSView`) or `HWND` (Windows), as a pointer-sized integer.
    fn native_handle(window: &Window) -> Option<isize> {
        let handle = HasWindowHandle::window_handle(window).ok()?;
        match handle.as_raw() {
            #[cfg(target_os = "macos")]
            RawWindowHandle::AppKit(h) => Some(h.ns_view.as_ptr() as isize),
            #[cfg(target_os = "windows")]
            RawWindowHandle::Win32(h) => Some(h.hwnd.get()),
            _ => None,
        }
    }

    // muda's popup entry points are `unsafe fn`s taking the raw NSView / HWND; the SAFETY note is at the call.
    #[allow(unsafe_code)]
    pub fn show(window: &mut Window, at: (f32, f32), cx: &mut App) -> bool {
        let Some(native) = native_handle(window) else {
            return false;
        };
        let (rows, actions) = super::rows(crate::native::menus::menus(cx));
        let focus = window.focused(cx);
        // GPUI's own handle, by its full path: with `HasWindowHandle` in scope, `window.window_handle()` resolves to
        // raw-window-handle's method (its blanket impl for references wins), which is not a GPUI window handle.
        let handle = Window::window_handle(window);
        cx.spawn(async move |cx| {
            let items = build(&rows);
            let refs: Vec<&dyn IsMenuItem> = items.iter().map(|c| c.as_ref()).collect();
            let menu = match Menu::with_items(&refs) {
                Ok(menu) => menu,
                Err(e) => {
                    log::warn!("dilla-desktop: the menu popup failed: {e}");
                    return;
                }
            };
            *CHOSEN
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner) = None;
            let position = Some(Position::Logical(LogicalPosition::new(
                f64::from(at.0),
                f64::from(at.1),
            )));
            // SAFETY: `native` is the live window's NSView or HWND, read a moment ago on this (the main) thread; the
            // window is checked below before anything else touches it, and AppKit and Win32 ignore a popup whose window
            // went away. The popup runs here, outside any borrow of the app, so the windows keep drawing during the
            // OS's modal loop.
            #[cfg(target_os = "macos")]
            unsafe {
                let _ =
                    menu.show_context_menu_for_nsview(native as *const std::ffi::c_void, position);
            }
            #[cfg(target_os = "windows")]
            unsafe {
                let _ = menu.show_context_menu_for_hwnd(native, position);
            }
            // With no tray (no event handler installed) muda queues the event on its channel instead.
            while let Ok(event) = MenuEvent::receiver().try_recv() {
                route(event.id().as_ref());
            }
            let chosen = CHOSEN
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner)
                .take();
            let Some(action) = chosen.and_then(|i| actions.get(i)) else {
                return;
            };
            let action = action.boxed_clone();
            let _ = cx.update_window(handle, move |_, window, cx| {
                if let Some(focus) = focus {
                    window.focus(&focus, cx);
                }
                window.dispatch_action(action, cx);
            });
        })
        .detach();
        true
    }
}

#[cfg(not(any(target_os = "macos", target_os = "windows")))]
mod os {
    use gpui::{App, Window};

    pub fn show(_window: &mut Window, _at: (f32, f32), _cx: &mut App) -> bool {
        false
    }

    pub fn route(_id: &str) -> bool {
        false
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    gpui::actions!(popup_test, [A, B, C]);

    /// beta.5 popped the whole application menu up: one submenu per menu, with its separators, its disabled rows, its
    /// checked rows and the OS's own menus (Services) kept; each action by index.
    #[test]
    fn the_popup_is_the_application_menu() {
        let menus = vec![
            Menu::new("Dilla").items([
                MenuItem::action("Your instances…", A),
                MenuItem::separator(),
                MenuItem::action("Status", B).disabled(true),
                MenuItem::os_submenu("Services", gpui::SystemMenuType::Services),
            ]),
            Menu::new("Window").items([MenuItem::action("Minimize", C).checked(true)]),
        ];
        let (rows, actions) = rows(menus);
        assert_eq!(
            rows,
            vec![
                Row::Submenu {
                    label: "Dilla".into(),
                    rows: vec![
                        Row::Action {
                            index: 0,
                            label: "Your instances…".into(),
                            enabled: true,
                            checked: false
                        },
                        Row::Separator,
                        Row::Action {
                            index: 1,
                            label: "Status".into(),
                            enabled: false,
                            checked: false
                        },
                        Row::Services {
                            label: "Services".into()
                        },
                    ],
                },
                Row::Submenu {
                    label: "Window".into(),
                    rows: vec![Row::Action {
                        index: 2,
                        label: "Minimize".into(),
                        enabled: true,
                        checked: true
                    }],
                },
            ]
        );
        assert!(actions[2].partial_eq(&C));
    }

    /// A row's id carries its action's index; the tray's ids are not the popup's.
    #[test]
    fn row_ids_round_trip() {
        assert_eq!(index_of(&row_id(7)), Some(7));
        assert_eq!(index_of("17"), None);
        assert_eq!(index_of("dilla-popup-x"), None);
    }
}
