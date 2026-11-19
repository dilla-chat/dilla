//! The tray icon and its menu (Package 5; beta.5 `main.ts` `refreshMenu`, `tray.on('click')`).
//!
//! - The menu is [`NativeShell::tray_menu`], drawn again whenever the [`NativeShell::on_tray_changed`] callback fires.
//!   This file adds its two sections: [`TraySlot::Instances`] ("Your instances…", then one row per open instance, then
//!   a separator) and [`TraySlot::Quit`] (a separator, then "Quit Dilla", which asks the quit guards).
//! - A click on the icon brings the first window forward, else opens the manager ([`NativeShell::show_app`]).
//! - The tooltip is "Dilla", "Dilla — {n} unread" with unread messages (a [`NativeShell::add_badge_sink`]), or the
//!   update-failed text the updater sets ([`set_update_failed`]).
//! - Once the tray is up, closing the last window keeps the app running ([`NativeShell::set_background`]); with no tray
//!   (GNOME without the AppIndicator extension, as beta.5) the last window's close quits as before.
//!
//! **Backends.** Linux: `ksni`, a StatusNotifierItem over D-Bus (pure Rust on `zbus`, already in the graph through
//! `ashpd`; no GTK and no libappindicator, so nothing new to install). macOS and Windows: `tray-icon` (Tauri's
//! NSStatusItem and `Shell_NotifyIcon` wrapper, with `muda` menus). Both run beside GPUI's event loop without owning
//! it: `ksni` on its own D-Bus thread, `tray-icon` on the main thread inside the loop GPUI already runs (AppKit's run
//! loop, the Win32 message loop), with its events handed over through a channel. Either way the menu the OS shows is a
//! snapshot ([`TrayModel`]); a chosen row comes back as its path and the generation of the snapshot, so a row chosen
//! from a menu that changed meanwhile does nothing.
//!
//! The menu's copy is beta.5's main-process copy (`main.ts` literals; the web strings have none of it).

use std::cell::RefCell;
use std::rc::Rc;

use dilla_client::seams::InstanceId;
use gpui::{App, Global, Task};
use tokio::sync::mpsc;

use super::{NativeShell, TrayItem, TraySection, TraySlot};

/// "Your instances…" (beta.5 `refreshMenu`).
pub const YOUR_INSTANCES: &str = "Your instances…";
/// "Quit Dilla".
pub const QUIT_DILLA: &str = "Quit Dilla";
/// The tooltip with nothing unread.
pub const TOOLTIP: &str = "Dilla";
/// The tooltip while an update failed (beta.5 `updater.state.phase === 'error'`).
pub const TOOLTIP_UPDATE_FAILED: &str = "Dilla — update failed; open the tray to retry";

/// The tray icon (beta.5 `assets/tray.png`, 32x32 RGBA).
pub const ICON_PNG: &[u8] = include_bytes!("../../../assets/brand/tray.png");

/// The tooltip for `unread` messages (beta.5: `Dilla — {n} unread`).
pub fn tooltip(unread: u32, update_failed: bool) -> String {
    if update_failed {
        TOOLTIP_UPDATE_FAILED.to_owned()
    } else if unread > 0 {
        format!("Dilla — {unread} unread")
    } else {
        TOOLTIP.to_owned()
    }
}

// ---------------------------------------------------------------------------------------------------------------------
// The model the OS shows
// ---------------------------------------------------------------------------------------------------------------------

/// One row of the OS menu, as drawn (no closures: the backends may run on another thread).
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Row {
    Action { label: String, enabled: bool },
    Check { label: String, checked: bool },
    Submenu { label: String, rows: Vec<Row> },
    Separator,
}

/// What the OS tray shows now.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct TrayModel {
    pub tooltip: String,
    pub rows: Vec<Row>,
    /// Counts the snapshots: a choice from an older one is ignored.
    pub generation: u64,
}

/// What the OS tray reports.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum TrayEvent {
    /// The icon was clicked (activated).
    Click,
    /// The row at `path` (indices from the top, into submenus) of snapshot `generation` was chosen.
    Choose { generation: u64, path: Vec<usize> },
}

fn rows_of(items: &[TrayItem]) -> Vec<Row> {
    items
        .iter()
        .map(|item| match item {
            TrayItem::Action { label, enabled, .. } => Row::Action {
                label: label.to_string(),
                enabled: *enabled,
            },
            TrayItem::Check { label, checked, .. } => Row::Check {
                label: label.to_string(),
                checked: *checked,
            },
            TrayItem::Submenu { label, items } => Row::Submenu {
                label: label.to_string(),
                rows: rows_of(items),
            },
            TrayItem::Separator => Row::Separator,
        })
        .collect()
}

/// The item at `path` in `items`.
fn item_at<'a>(items: &'a [TrayItem], path: &[usize]) -> Option<&'a TrayItem> {
    let (first, rest) = path.split_first()?;
    let item = items.get(*first)?;
    match (rest.is_empty(), item) {
        (true, _) => Some(item),
        (false, TrayItem::Submenu { items, .. }) => item_at(items, rest),
        (false, _) => None,
    }
}

/// Where the snapshots go: the OS tray, or a test's recorder.
pub trait Backend {
    fn show(&mut self, model: &TrayModel);
}

#[derive(Default)]
struct Tray {
    backend: Option<Box<dyn Backend>>,
    /// The items of the last snapshot, for the choice that comes back.
    items: Vec<TrayItem>,
    generation: u64,
    unread: u32,
    update_failed: bool,
    _pump: Option<Task<()>>,
}

impl Global for Tray {}

fn tray(cx: &mut App) -> &mut Tray {
    cx.default_global::<Tray>()
}

/// The snapshot the tray shows now (tests read it; the backends get it on every change).
pub fn model(cx: &App) -> TrayModel {
    let (unread, failed, generation) = cx
        .try_global::<Tray>()
        .map_or((0, false, 0), |t| (t.unread, t.update_failed, t.generation));
    TrayModel {
        tooltip: tooltip(unread, failed),
        rows: rows_of(&NativeShell::tray_menu(cx)),
        generation,
    }
}

/// Draws the tray again from the shell's sections.
fn redraw(cx: &mut App) {
    let items = NativeShell::tray_menu(cx);
    let t = tray(cx);
    t.generation += 1;
    t.items = items;
    let model = TrayModel {
        tooltip: tooltip(t.unread, t.update_failed),
        rows: rows_of(&t.items),
        generation: t.generation,
    };
    if let Some(backend) = t.backend.as_mut() {
        backend.show(&model);
    }
    super::menus::refresh(cx);
}

/// The updater's failed state for the tooltip (the release plan's updater; beta.5 `updater.state.phase === 'error'`).
pub fn set_update_failed(failed: bool, cx: &mut App) {
    tray(cx).update_failed = failed;
    redraw(cx);
}

/// What the OS tray reported, on the UI thread.
pub fn handle(event: TrayEvent, cx: &mut App) {
    match event {
        TrayEvent::Click => NativeShell::show_app(cx),
        TrayEvent::Choose { generation, path } => {
            let Some(t) = cx.try_global::<Tray>() else {
                return;
            };
            if t.generation != generation {
                return;
            }
            let run = match item_at(&t.items, &path) {
                Some(TrayItem::Action {
                    run, enabled: true, ..
                })
                | Some(TrayItem::Check { run, .. }) => run.clone(),
                _ => return,
            };
            run(cx);
        }
    }
}

/// The current snapshot's generation and the item at `path` in it (the application menu runs the same rows).
pub(crate) fn choose(generation: u64, path: Vec<usize>, cx: &mut App) {
    handle(TrayEvent::Choose { generation, path }, cx);
}

/// The items of the snapshot the OS shows now, with its generation (the application menu draws the same rows).
pub(crate) fn snapshot(cx: &App) -> (u64, Vec<TrayItem>) {
    cx.try_global::<Tray>()
        .map_or((0, Vec::new()), |t| (t.generation, t.items.clone()))
}

// ---------------------------------------------------------------------------------------------------------------------
// The sections and installation
// ---------------------------------------------------------------------------------------------------------------------

/// An instance's row: its host, and its path unless `/` (beta.5 `new URL(instance).host + pathname`).
pub fn instance_label(server: &url::Url) -> String {
    let host = match (server.host_str(), server.port()) {
        (Some(h), Some(p)) => format!("{h}:{p}"),
        (Some(h), None) => h.to_owned(),
        (None, _) => server.as_str().to_owned(),
    };
    match server.path() {
        "/" | "" => host,
        path => format!("{host}{path}"),
    }
}

/// The instances the tray lists: those with a window or a running client, in order, with their labels.
fn open_instances(cx: &App) -> Vec<(InstanceId, String)> {
    let mut ids: Vec<InstanceId> = NativeShell::open_windows(cx)
        .into_iter()
        .filter_map(|(_, i)| i)
        .chain(crate::windows::running_instances(cx))
        .collect();
    ids.sort_by_key(|i| i.0);
    ids.dedup();
    if ids.is_empty() {
        return Vec::new();
    }
    let list = NativeShell::host(cx)
        .and_then(|h| h.launch.root)
        .and_then(|root| dilla_client::instances::Instances::load(&root).ok());
    ids.into_iter()
        .map(|id| {
            let label = list.as_ref().and_then(|l| l.get(id)).map_or_else(
                || format!("Instance {}", id.0),
                |c| instance_label(&c.server),
            );
            (id, label)
        })
        .collect()
}

fn instances_rows(cx: &App) -> Vec<TrayItem> {
    let mut rows = vec![TrayItem::action(YOUR_INSTANCES, show_manager)];
    rows.extend(
        open_instances(cx)
            .into_iter()
            .map(|(id, label)| TrayItem::action(label, move |cx| show_instance(id, cx))),
    );
    rows.push(TrayItem::Separator);
    rows
}

fn quit_rows(_: &App) -> Vec<TrayItem> {
    vec![
        TrayItem::Separator,
        TrayItem::action(QUIT_DILLA, NativeShell::request_quit),
    ]
}

/// "Your instances…": the manager window (opened, or brought forward).
fn show_manager(cx: &mut App) {
    if NativeShell::is_quitting(cx) || NativeShell::is_asking_quit(cx) {
        return;
    }
    let manager = NativeShell::open_windows(cx)
        .into_iter()
        .find(|(_, i)| i.is_none())
        .and_then(|(w, _)| cx.windows().into_iter().find(|h| h.window_id() == w));
    if let Some(handle) = manager
        && handle
            .update(cx, |_, window, _| window.activate_window())
            .is_ok()
    {
        cx.activate(true);
        return;
    }
    if let Some(host) = NativeShell::host(cx)
        && let Err(e) = crate::windows::open_manager(cx, &host.launch, host.allow_loopback)
    {
        log::warn!("dilla-desktop: the manager did not open: {e}");
    }
}

/// The tray's own sections and the tooltip's sink, with no OS tray behind them (tests; [`install`] adds the tray).
pub fn install_sections(cx: &mut App) {
    NativeShell::add_tray_section(TraySection::new(TraySlot::Instances, instances_rows), cx);
    NativeShell::add_tray_section(TraySection::new(TraySlot::Quit, quit_rows), cx);
    NativeShell::add_badge_sink(
        Box::new(|n, cx| {
            if tray(cx).unread != n {
                tray(cx).unread = n;
                redraw(cx);
            }
        }),
        cx,
    );
    NativeShell::on_tray_changed(Box::new(redraw), cx);
    redraw(cx);
}

/// Puts `backend` behind the tray (a test's recorder) and draws it.
pub fn set_backend(backend: Box<dyn Backend>, cx: &mut App) {
    tray(cx).backend = Some(backend);
    redraw(cx);
}

/// Called once by [`super::install`]: the sections, then the OS tray. With the tray up, the app stays in the
/// background when its last window closes.
pub fn install(cx: &mut App) {
    install_sections(cx);
    let (tx, mut rx) = mpsc::unbounded_channel::<TrayEvent>();
    match os::start(tx) {
        Ok(backend) => {
            let pump = cx.spawn(async move |cx| {
                while let Some(event) = rx.recv().await {
                    cx.update(|cx| handle(event, cx));
                }
            });
            tray(cx)._pump = Some(pump);
            set_backend(backend, cx);
            NativeShell::set_background(true, cx);
        }
        Err(e) => log::warn!("dilla-desktop: no tray ({e}); closing the last window quits"),
    }
}

/// A recording backend (tests): every snapshot shown, newest last.
#[derive(Clone, Default)]
pub struct Recorder(pub Rc<RefCell<Vec<TrayModel>>>);

impl Backend for Recorder {
    fn show(&mut self, model: &TrayModel) {
        self.0.borrow_mut().push(model.clone());
    }
}

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

/// The icon as straight RGBA, and its size.
fn icon_rgba() -> Option<(Vec<u8>, u32, u32)> {
    let image = image::load_from_memory_with_format(ICON_PNG, image::ImageFormat::Png).ok()?;
    let rgba = image.to_rgba8();
    let (w, h) = rgba.dimensions();
    Some((rgba.into_raw(), w, h))
}

// ---------------------------------------------------------------------------------------------------------------------
// Linux: a StatusNotifierItem (ksni)
// ---------------------------------------------------------------------------------------------------------------------

#[cfg(target_os = "linux")]
mod os {
    use ksni::blocking::TrayMethods;
    use ksni::menu::{CheckmarkItem, MenuItem, StandardItem, SubMenu};
    use tokio::sync::mpsc::UnboundedSender;

    use super::{Backend, Row, TrayEvent, TrayModel};

    struct Sni {
        model: TrayModel,
        icon: Vec<ksni::Icon>,
        tx: UnboundedSender<TrayEvent>,
    }

    fn items(rows: &[Row], prefix: &[usize], generation: u64) -> Vec<MenuItem<Sni>> {
        rows.iter()
            .enumerate()
            .map(|(i, row)| {
                let mut path = prefix.to_vec();
                path.push(i);
                let choose = move |s: &mut Sni| {
                    let _ = s.tx.send(TrayEvent::Choose {
                        generation,
                        path: path.clone(),
                    });
                };
                match row {
                    Row::Action { label, enabled } => StandardItem {
                        label: escape(label),
                        enabled: *enabled,
                        activate: Box::new(choose),
                        ..Default::default()
                    }
                    .into(),
                    Row::Check { label, checked } => CheckmarkItem {
                        label: escape(label),
                        checked: *checked,
                        activate: Box::new(choose),
                        ..Default::default()
                    }
                    .into(),
                    Row::Submenu { label, rows } => {
                        let mut here = prefix.to_vec();
                        here.push(i);
                        SubMenu {
                            label: escape(label),
                            submenu: items(rows, &here, generation),
                            ..Default::default()
                        }
                        .into()
                    }
                    Row::Separator => MenuItem::Separator,
                }
            })
            .collect()
    }

    /// DBusMenu reads `_` as the access-key marker: a literal one is doubled.
    fn escape(label: &str) -> String {
        label.replace('_', "__")
    }

    impl ksni::Tray for Sni {
        fn id(&self) -> String {
            crate::LINUX_APP_ID.to_owned()
        }

        fn title(&self) -> String {
            super::TOOLTIP.to_owned()
        }

        fn icon_pixmap(&self) -> Vec<ksni::Icon> {
            self.icon.clone()
        }

        fn tool_tip(&self) -> ksni::ToolTip {
            ksni::ToolTip {
                title: self.model.tooltip.clone(),
                ..Default::default()
            }
        }

        fn activate(&mut self, _x: i32, _y: i32) {
            let _ = self.tx.send(TrayEvent::Click);
        }

        fn menu(&self) -> Vec<MenuItem<Self>> {
            items(&self.model.rows, &[], self.model.generation)
        }
    }

    struct Handle(ksni::blocking::Handle<Sni>);

    impl Backend for Handle {
        fn show(&mut self, model: &TrayModel) {
            let model = model.clone();
            self.0.update(move |s| s.model = model);
        }
    }

    impl Drop for Handle {
        fn drop(&mut self) {
            drop(self.0.shutdown());
        }
    }

    pub fn start(tx: UnboundedSender<TrayEvent>) -> Result<Box<dyn Backend>, String> {
        // SNI pixmaps are ARGB32 in network byte order.
        let icon = super::icon_rgba()
            .map(|(rgba, w, h)| {
                let mut data = rgba;
                for px in data.as_chunks_mut::<4>().0 {
                    px.rotate_right(1);
                }
                vec![ksni::Icon {
                    width: w as i32,
                    height: h as i32,
                    data,
                }]
            })
            .unwrap_or_default();
        let sni = Sni {
            model: TrayModel {
                tooltip: super::TOOLTIP.to_owned(),
                rows: Vec::new(),
                generation: 0,
            },
            icon,
            tx,
        };
        // In a Flatpak sandbox the well-known name cannot be owned (ksni's documented exception).
        let sandboxed = std::path::Path::new("/.flatpak-info").exists();
        sni.disable_dbus_name(sandboxed)
            .spawn()
            .map(|h| Box::new(Handle(h)) as Box<dyn Backend>)
            .map_err(|e| e.to_string())
    }
}

// ---------------------------------------------------------------------------------------------------------------------
// macOS and Windows: tray-icon (NSStatusItem, Shell_NotifyIcon) with muda menus
// ---------------------------------------------------------------------------------------------------------------------

#[cfg(any(target_os = "macos", target_os = "windows"))]
mod os {
    use std::collections::HashMap;
    use std::sync::{Arc, Mutex};

    use tokio::sync::mpsc::UnboundedSender;
    use tray_icon::menu::{
        CheckMenuItem, IsMenuItem, Menu, MenuEvent, MenuId, MenuItem, PredefinedMenuItem, Submenu,
    };
    use tray_icon::{
        Icon, MouseButton, MouseButtonState, TrayIcon, TrayIconBuilder, TrayIconEvent,
    };

    use super::{Backend, Row, TrayEvent, TrayModel};

    /// The rows of the menu shown now, by muda id: the snapshot's generation and the row's path.
    type Paths = Arc<Mutex<HashMap<MenuId, (u64, Vec<usize>)>>>;

    struct Native {
        icon: TrayIcon,
        paths: Paths,
    }

    fn build(
        rows: &[Row],
        prefix: &[usize],
        generation: u64,
        paths: &mut HashMap<MenuId, (u64, Vec<usize>)>,
    ) -> Vec<Box<dyn IsMenuItem>> {
        let mut out: Vec<Box<dyn IsMenuItem>> = Vec::new();
        for (i, row) in rows.iter().enumerate() {
            let mut path = prefix.to_vec();
            path.push(i);
            match row {
                Row::Action { label, enabled } => {
                    let item = MenuItem::new(label, *enabled, None);
                    paths.insert(item.id().clone(), (generation, path));
                    out.push(Box::new(item));
                }
                Row::Check { label, checked } => {
                    let item = CheckMenuItem::new(label, true, *checked, None);
                    paths.insert(item.id().clone(), (generation, path));
                    out.push(Box::new(item));
                }
                Row::Submenu { label, rows } => {
                    let children = build(rows, &path, generation, paths);
                    let refs: Vec<&dyn IsMenuItem> = children.iter().map(|c| c.as_ref()).collect();
                    match Submenu::with_items(label, true, &refs) {
                        Ok(sub) => out.push(Box::new(sub)),
                        Err(e) => log::warn!("dilla-desktop: a tray submenu failed: {e}"),
                    }
                }
                Row::Separator => out.push(Box::new(PredefinedMenuItem::separator())),
            }
        }
        out
    }

    impl Backend for Native {
        fn show(&mut self, model: &TrayModel) {
            let mut paths = HashMap::new();
            let items = build(&model.rows, &[], model.generation, &mut paths);
            let refs: Vec<&dyn IsMenuItem> = items.iter().map(|c| c.as_ref()).collect();
            match Menu::with_items(&refs) {
                Ok(menu) => {
                    *self
                        .paths
                        .lock()
                        .unwrap_or_else(std::sync::PoisonError::into_inner) = paths;
                    self.icon.set_menu(Some(Box::new(menu)));
                }
                Err(e) => log::warn!("dilla-desktop: the tray menu failed: {e}"),
            }
            let _ = self.icon.set_tooltip(Some(&model.tooltip));
        }
    }

    pub fn start(tx: UnboundedSender<TrayEvent>) -> Result<Box<dyn Backend>, String> {
        let (rgba, w, h) = super::icon_rgba().ok_or("the tray icon did not decode")?;
        let icon = Icon::from_rgba(rgba, w, h).map_err(|e| e.to_string())?;
        let paths: Paths = Arc::default();
        let click = tx.clone();
        // A left click brings the app forward (beta.5 `tray.on('click')`); the menu opens on a right click.
        TrayIconEvent::set_event_handler(Some(move |event: TrayIconEvent| {
            if let TrayIconEvent::Click {
                button: MouseButton::Left,
                button_state: MouseButtonState::Up,
                ..
            } = event
            {
                let _ = click.send(TrayEvent::Click);
            }
        }));
        let chosen = paths.clone();
        MenuEvent::set_event_handler(Some(move |event: MenuEvent| {
            // muda has one handler per process: the title bar's popup menu takes its own rows.
            if crate::native::popup_menu::route(event.id().as_ref()) {
                return;
            }
            let found = chosen
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner)
                .get(event.id())
                .cloned();
            if let Some((generation, path)) = found {
                let _ = tx.send(TrayEvent::Choose { generation, path });
            }
        }));
        let icon = TrayIconBuilder::new()
            .with_icon(icon)
            .with_tooltip(super::TOOLTIP)
            .with_menu_on_left_click(false)
            .build()
            .map_err(|e| e.to_string())?;
        Ok(Box::new(Native { icon, paths }))
    }
}

#[cfg(not(any(target_os = "linux", target_os = "macos", target_os = "windows")))]
mod os {
    use tokio::sync::mpsc::UnboundedSender;

    use super::{Backend, TrayEvent};

    pub fn start(_tx: UnboundedSender<TrayEvent>) -> Result<Box<dyn Backend>, String> {
        Err("no tray on this platform".into())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_tooltip_counts_unread_and_says_a_failed_update() {
        assert_eq!(tooltip(0, false), "Dilla");
        assert_eq!(tooltip(3, false), "Dilla — 3 unread");
        assert_eq!(tooltip(3, true), TOOLTIP_UPDATE_FAILED);
    }

    #[test]
    fn an_instance_row_is_its_host_and_path() {
        let url = |s: &str| url::Url::parse(s).unwrap();
        assert_eq!(
            instance_label(&url("https://chat.example/")),
            "chat.example"
        );
        assert_eq!(
            instance_label(&url("https://chat.example:8443/team")),
            "chat.example:8443/team"
        );
    }

    fn labels(rows: &[Row]) -> Vec<String> {
        rows.iter()
            .map(|r| match r {
                Row::Action { label, .. }
                | Row::Check { label, .. }
                | Row::Submenu { label, .. } => label.clone(),
                Row::Separator => "--".into(),
            })
            .collect()
    }

    /// beta.5 `refreshMenu`: "Your instances…", one row per open instance, then "Quit Dilla" after a separator; a row
    /// brings its instance's window forward, and a choice from an older snapshot does nothing.
    #[gpui::test]
    fn menu_lists_open_instances_and_quit(cx: &mut gpui::TestAppContext) {
        let a = cx
            .add_empty_window()
            .update(|w, _| w.window_handle().window_id());
        let recorder = Recorder::default();
        cx.update(|cx| {
            install_sections(cx);
            set_backend(Box::new(recorder.clone()), cx);
            NativeShell::window_opened(a, Some(InstanceId(3)), cx);
            NativeShell::set_badge(InstanceId(3), 2, cx);
        });
        let shown = recorder.0.borrow().last().cloned().unwrap();
        assert_eq!(
            labels(&shown.rows),
            ["Your instances…", "Instance 3", "--", "--", "Quit Dilla"]
        );
        assert_eq!(shown.tooltip, "Dilla — 2 unread");
        assert_eq!(shown, cx.update(|cx| model(cx)));
        // A stale choice of "Quit Dilla" does nothing; the current one quits.
        cx.update(|cx| {
            handle(
                TrayEvent::Choose {
                    generation: shown.generation - 1,
                    path: vec![4],
                },
                cx,
            );
            assert!(!NativeShell::is_quitting(cx));
            handle(
                TrayEvent::Choose {
                    generation: shown.generation,
                    path: vec![4],
                },
                cx,
            );
            assert!(NativeShell::is_quitting(cx));
        });
    }

    #[test]
    fn the_icon_decodes() {
        let (rgba, w, h) = icon_rgba().unwrap();
        assert_eq!((w, h), (32, 32));
        assert_eq!(rgba.len(), 32 * 32 * 4);
    }
}
