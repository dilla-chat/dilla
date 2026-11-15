//! The operating-system shell around the windows (GPUI parity plan, Task 0): the tray, the badge, autostart, OS
//! notifications, `dilla://` links, the single-process lock, the quit guards, the application menu and the permission
//! prompts. beta.5 kept all of this in `packages/desktop/src/main.ts`.
//!
//! **One file per package.** [`NativeShell`] (an app global) is the registry the files meet in, so packages 4 to 7
//! each fill their own file and never edit `main.rs`, `app.rs` or `windows.rs`:
//!
//! | File | Package | Entry points called by this module (fixed signatures) |
//! |---|---|---|
//! | [`tray`] | 5 | `install(cx)`: draws the OS tray from [`NativeShell::tray_menu`], re-drawn on [`NativeShell::on_tray_changed`] |
//! | [`badge`] | 5 | `install(cx)`: a [`NativeShell::add_badge_sink`] that sets the dock, overlay or launcher count |
//! | [`autostart`] | 5 | `hidden_start(argv)`, `install(cx)` (its tray section) |
//! | [`menus`] | 5 | `prepare(app)` (`on_reopen`, before the run loop), `install(cx)` (`set_menus`, zoom) |
//! | [`notifier`] | 4 | `install(cx)`: the OS notifier; withdraws a window's notifications on [`WindowEvent::Closed`] |
//! | [`links`] | 6 | `prepare(app)` (`on_open_urls`), `parse(raw)`, `install(cx)` (its [`NativeShell::on_link`] handler) |
//! | [`single_instance`] | 6 | `claim(argv)` (before the run loop), `install(primary, cx)` (second launches) |
//! | [`quit`] | 7 | `install(cx)`: the quit dialogs, through [`NativeShell::set_quit_prompt`] |
//! | [`permissions`] | 7 | `install(cx)`: the OS permission checks and the macOS usage strings |
//!
//! Package 3 (application audio) and the release plan's updater add tray sections through
//! [`NativeShell::add_tray_section`] from their own files; Package 7's guard providers live in their screens and call
//! [`NativeShell::add_quit_guard`].
//!
//! **Start-up order** (`main.rs`): [`claim`] runs before the run loop (a second launch hands its argv over and exits),
//! [`prepare`] registers the platform callbacks on the `Application`, and [`install`] puts the shell in place inside
//! the run loop, before the first window opens. `windows.rs` reports every window it opens, moves and closes
//! ([`NativeShell::window_opened`], [`NativeShell::window_moved`], [`NativeShell::window_closed`]); `app.rs` sends the
//! [`Quit`] action to [`NativeShell::request_quit`].
//!
//! **Closures and borrows.** Every closure is kept as an `Rc` and cloned out of the global before it runs, so a
//! closure may call back into the shell (`NativeShell::...(cx)`) without a double borrow.

pub mod autostart;
pub mod badge;
pub mod links;
pub mod menus;
pub mod notifier;
pub mod permissions;
pub mod quit;
pub mod single_instance;
pub mod tray;

use std::collections::BTreeMap;
use std::collections::VecDeque;
use std::rc::Rc;

use dilla_client::seams::InstanceId;
use gpui::{App, Application, Global, SharedString, WindowId, actions};

use crate::Launch;

actions!(
    dilla_native,
    [
        /// Quit Dilla: the tray's and the application menu's "Quit Dilla". It asks the quit guards first
        /// ([`NativeShell::request_quit`]).
        Quit
    ]
);

// ---------------------------------------------------------------------------------------------------------------------
// The tray menu
// ---------------------------------------------------------------------------------------------------------------------

/// Where a tray section goes. The menu lists the slots in this order (beta.5 `refreshMenu`), and the sections of one
/// slot in the order they were added.
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub enum TraySlot {
    /// "Sharing application sound: {label} — stop" (Package 3).
    AppAudio,
    /// One submenu per ringing call, with Answer and Decline (Package 4).
    Ring,
    /// "Your instances…", then one row per open instance, then a separator (Package 5).
    Instances,
    /// "Start Dilla at login" (Package 5, `autostart`).
    Autostart,
    /// "Check for updates", "Update failed — retry", "Update using your package manager", "Restart to update" (the
    /// release plan's updater).
    Update,
    /// A separator, then "Quit Dilla" (Package 5).
    Quit,
}

/// What a tray entry runs when it is chosen. It runs on the UI thread with the app.
pub type TrayAction = Rc<dyn Fn(&mut App)>;

/// One entry of the tray menu (and of the macOS application menu's Dilla menu, which shows the same entries). The
/// shell adds no separators of its own: a section that wants one adds [`TrayItem::Separator`].
#[derive(Clone)]
pub enum TrayItem {
    /// A plain row. A disabled row shows its label only (beta.5's update message row).
    Action {
        label: SharedString,
        enabled: bool,
        run: TrayAction,
    },
    /// A row with a check mark ("Start Dilla at login"). `run` toggles it.
    Check {
        label: SharedString,
        checked: bool,
        run: TrayAction,
    },
    /// A row that opens more rows (a ring's Answer and Decline).
    Submenu {
        label: SharedString,
        items: Vec<TrayItem>,
    },
    Separator,
}

impl TrayItem {
    /// An enabled [`TrayItem::Action`].
    pub fn action(label: impl Into<SharedString>, run: impl Fn(&mut App) + 'static) -> Self {
        Self::Action {
            label: label.into(),
            enabled: true,
            run: Rc::new(run),
        }
    }

    /// The row's label (`None` for a separator).
    pub fn label(&self) -> Option<&SharedString> {
        match self {
            Self::Action { label, .. }
            | Self::Check { label, .. }
            | Self::Submenu { label, .. } => Some(label),
            Self::Separator => None,
        }
    }
}

impl std::fmt::Debug for TrayItem {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Action { label, enabled, .. } => f
                .debug_struct("Action")
                .field("label", label)
                .field("enabled", enabled)
                .finish(),
            Self::Check { label, checked, .. } => f
                .debug_struct("Check")
                .field("label", label)
                .field("checked", checked)
                .finish(),
            Self::Submenu { label, items } => f
                .debug_struct("Submenu")
                .field("label", label)
                .field("items", items)
                .finish(),
            Self::Separator => f.write_str("Separator"),
        }
    }
}

/// What a tray section shows now, asked every time the menu is built.
pub type TrayRows = dyn Fn(&App) -> Vec<TrayItem>;

/// What re-draws the OS tray ([`NativeShell::on_tray_changed`]).
pub type TrayRedraw = dyn Fn(&mut App);

/// A badge sink: hears the unread total across instances ([`NativeShell::add_badge_sink`]).
pub type BadgeSink = dyn Fn(u32, &mut App);

/// The link handler ([`NativeShell::on_link`]).
pub type LinkHandler = dyn Fn(Link, &mut App);

/// A window-event listener ([`NativeShell::on_window_event`]).
pub type WindowListener = dyn Fn(&WindowEvent, &mut App);

/// A tray section: its slot and the rows it shows now. `rows` is asked every time the menu is built, so it reads the
/// current state; a section whose state changed calls [`NativeShell::refresh_tray`].
pub struct TraySection {
    pub slot: TraySlot,
    pub rows: Box<TrayRows>,
}

impl TraySection {
    pub fn new(slot: TraySlot, rows: impl Fn(&App) -> Vec<TrayItem> + 'static) -> Self {
        Self {
            slot,
            rows: Box::new(rows),
        }
    }
}

// ---------------------------------------------------------------------------------------------------------------------
// Quit guards
// ---------------------------------------------------------------------------------------------------------------------

/// What one guard provider (an instance's call, its drafts) says when the app is asked to quit (beta.5 `setDesktopGuard`
/// and `quit-ack`).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct GuardState {
    /// The instance is in a call.
    pub active_call: bool,
    /// The instance has text it has not sent (a draft or the composer's pending input).
    pub unsaved_draft: bool,
    /// The provider could tell. `false` is beta.5's window that did not answer within 3 s: it counts as in use, and it
    /// asks "Dilla is not responding" first.
    pub answered: bool,
}

impl GuardState {
    /// Nothing to lose: answered, no call, no draft.
    pub const IDLE: Self = Self {
        active_call: false,
        unsaved_draft: false,
        answered: true,
    };
    /// A provider that could not tell.
    pub const UNANSWERED: Self = Self {
        active_call: false,
        unsaved_draft: false,
        answered: false,
    };

    /// Quitting now would lose something (or might: an unanswered provider counts as in use).
    pub fn in_use(self) -> bool {
        self.active_call || self.unsaved_draft || !self.answered
    }
}

/// A guard provider: asked once per quit request, on the UI thread. It must answer at once (read state, never wait).
pub type QuitGuard = Box<GuardCheck>;

/// The function behind a [`QuitGuard`].
pub type GuardCheck = dyn Fn(&App) -> GuardState;

/// The handle of a registered guard, for [`NativeShell::remove_quit_guard`].
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub struct QuitGuardId(u64);

/// Why the app is asked to quit.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum QuitReason {
    /// "Quit Dilla" (tray, menu, the [`Quit`] action).
    User,
    /// "Restart to update" (the release plan's Task 14): the guards are collected again before the restart.
    UpdateRestart,
}

/// One quit request with every guard's answer, in the order the guards were added. The quit prompt gets it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct QuitRequest {
    pub reason: QuitReason,
    pub guards: Vec<(InstanceId, GuardState)>,
}

impl QuitRequest {
    /// Some provider did not answer (beta.5 `unknownGuard`: "Dilla is not responding" comes first).
    pub fn unanswered(&self) -> bool {
        self.guards.iter().any(|(_, g)| !g.answered)
    }

    /// Some instance is in a call or has unsent text (beta.5 `inUse`).
    pub fn in_use(&self) -> bool {
        self.guards
            .iter()
            .any(|(_, g)| g.active_call || g.unsaved_draft)
    }

    /// Quitting needs no question: every provider answered and nothing is in use.
    pub fn is_clear(&self) -> bool {
        self.guards.iter().all(|(_, g)| !g.in_use())
    }
}

/// How the quit prompt ended.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum QuitAnswer {
    Quit,
    Stay,
}

/// The quit dialogs (Package 7): shown for a request that is not clear. It must end, now or later, with
/// [`NativeShell::finish_quit`]; until then another quit request is ignored.
pub type QuitPrompt = Box<PromptFn>;

/// The function behind a [`QuitPrompt`].
pub type PromptFn = dyn Fn(QuitRequest, &mut App);

// ---------------------------------------------------------------------------------------------------------------------
// Links and windows
// ---------------------------------------------------------------------------------------------------------------------

/// A `dilla://` link, parsed ([`links::parse`]; beta.5 `policy.ts` `parseInvite`). Nothing else ever reaches a link
/// handler: links never carry credentials.
#[derive(Clone, Debug, PartialEq, Eq, Hash)]
pub enum Link {
    /// `dilla://invite/<host[:port]>/<code>`: join instance `host` with invite `code`. The instance may be unknown; the
    /// handler confirms it before adding it.
    Invite { host: String, code: String },
}

/// How many links wait for a handler at most; the oldest goes first.
pub const MAX_QUEUED_LINKS: usize = 16;

/// A window the shell knows about, as `windows.rs` reports it.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum WindowEvent {
    /// A window opened: an instance's (`Some`) or the manager (`None`).
    Opened {
        window: WindowId,
        instance: Option<InstanceId>,
    },
    /// An instance's start window handed over to its workspace window (the window shapes): the same instance, a new
    /// window. Nothing of the instance closed.
    Moved {
        from: WindowId,
        to: WindowId,
        instance: InstanceId,
    },
    /// A window the shell knew closed.
    Closed {
        window: WindowId,
        instance: Option<InstanceId>,
    },
}

/// What `main.rs` hands the shell so it can open windows itself (a tray click, a second launch, macOS reopen).
#[derive(Clone)]
pub struct Host {
    pub launch: Launch,
    pub allow_loopback: bool,
}

/// What [`claim`] found on the command line, for [`install`].
pub struct Startup {
    /// `--hidden` (autostart): start in the tray with no window ([`autostart::hidden_start`]).
    pub hidden: bool,
    /// The `dilla://` links on the command line, parsed; they are queued for the link handler.
    pub links: Vec<Link>,
    primary: single_instance::Primary,
}

/// What [`claim`] answers.
pub enum Claimed {
    /// This process is the app: go on and run.
    Primary(Startup),
    /// Another process is the app and took this launch's arguments: exit now, with success.
    HandedOver,
}

// ---------------------------------------------------------------------------------------------------------------------
// The registry
// ---------------------------------------------------------------------------------------------------------------------

struct GuardEntry {
    id: QuitGuardId,
    instance: InstanceId,
    check: Rc<GuardCheck>,
}

/// The registry of the OS shell, one per app. Every function takes the app; the functions that change it create it
/// when it is missing, so the screens and the tests may call them without [`install`].
#[derive(Default)]
pub struct NativeShell {
    host: Option<Host>,
    tray_sections: Vec<Rc<TraySection>>,
    tray_changed: Option<Rc<TrayRedraw>>,
    /// Unread per instance, by its number.
    badges: BTreeMap<u8, u32>,
    badge_sinks: Vec<Rc<BadgeSink>>,
    quit_guards: Vec<GuardEntry>,
    next_guard: u64,
    quit_prompt: Option<Rc<PromptFn>>,
    asking_quit: bool,
    quitting: bool,
    link_handler: Option<Rc<LinkHandler>>,
    queued_links: VecDeque<Link>,
    windows: Vec<(WindowId, Option<InstanceId>)>,
    window_listeners: Vec<Rc<WindowListener>>,
    background: bool,
}

impl Global for NativeShell {}

impl NativeShell {
    fn of(cx: &mut App) -> &mut Self {
        cx.default_global::<Self>()
    }

    // --- tray ---------------------------------------------------------------------------------------------------------

    /// Adds a tray section, and re-draws the tray.
    pub fn add_tray_section(section: TraySection, cx: &mut App) {
        Self::of(cx).tray_sections.push(Rc::new(section));
        Self::refresh_tray(cx);
    }

    /// The tray menu now: every section's rows, by slot, then in the order the sections were added.
    pub fn tray_menu(cx: &App) -> Vec<TrayItem> {
        let Some(shell) = cx.try_global::<Self>() else {
            return Vec::new();
        };
        let mut sections: Vec<&Rc<TraySection>> = shell.tray_sections.iter().collect();
        // A stable sort keeps the order of addition inside a slot.
        sections.sort_by_key(|s| s.slot);
        sections.into_iter().flat_map(|s| (s.rows)(cx)).collect()
    }

    /// Sets what re-draws the OS tray (Package 5's tray; one per app, the last one set wins).
    pub fn on_tray_changed(redraw: Box<TrayRedraw>, cx: &mut App) {
        Self::of(cx).tray_changed = Some(Rc::from(redraw));
    }

    /// The tray's rows may have changed: re-draw it.
    pub fn refresh_tray(cx: &mut App) {
        if let Some(redraw) = cx.try_global::<Self>().and_then(|s| s.tray_changed.clone()) {
            redraw(cx);
        }
    }

    /// Whether closing the last window keeps the app running in the background (Package 5 sets it once its tray is
    /// up). Off, the last window's close falls back to the manager or quits, as before the tray.
    pub fn set_background(on: bool, cx: &mut App) {
        Self::of(cx).background = on;
    }

    /// See [`NativeShell::set_background`].
    pub fn stays_in_background(cx: &App) -> bool {
        cx.try_global::<Self>().is_some_and(|s| s.background)
    }

    // --- badge --------------------------------------------------------------------------------------------------------

    /// Instance `instance` has `n` unread. The sinks hear the new total across instances when it changed.
    pub fn set_badge(instance: InstanceId, n: u32, cx: &mut App) {
        let shell = Self::of(cx);
        let before = shell.badge_total();
        if n == 0 {
            shell.badges.remove(&instance.0);
        } else {
            shell.badges.insert(instance.0, n);
        }
        let after = shell.badge_total();
        if after != before {
            Self::tell_badge(after, cx);
        }
    }

    /// The unread count of every instance together (beta.5 `app.setBadgeCount`, the tooltip "Dilla — {n} unread").
    pub fn badge(cx: &App) -> u32 {
        cx.try_global::<Self>().map_or(0, Self::badge_total)
    }

    fn badge_total(&self) -> u32 {
        self.badges.values().fold(0u32, |a, b| a.saturating_add(*b))
    }

    /// Adds a sink that shows the total (the dock or launcher badge, the tray tooltip). It hears the current total at
    /// once, then every change.
    pub fn add_badge_sink(sink: Box<BadgeSink>, cx: &mut App) {
        let sink: Rc<BadgeSink> = Rc::from(sink);
        Self::of(cx).badge_sinks.push(sink.clone());
        let total = Self::badge(cx);
        sink(total, cx);
    }

    fn tell_badge(total: u32, cx: &mut App) {
        let sinks = Self::of(cx).badge_sinks.clone();
        for sink in sinks {
            sink(total, cx);
        }
    }

    // --- quit ---------------------------------------------------------------------------------------------------------

    /// Adds a guard provider for `instance`. Every quit request asks every guard, in the order they were added.
    pub fn add_quit_guard(instance: InstanceId, guard: QuitGuard, cx: &mut App) -> QuitGuardId {
        let shell = Self::of(cx);
        shell.next_guard += 1;
        let id = QuitGuardId(shell.next_guard);
        shell.quit_guards.push(GuardEntry {
            id,
            instance,
            check: Rc::from(guard),
        });
        id
    }

    /// Removes a guard provider (its instance went away). An unknown id is ignored.
    pub fn remove_quit_guard(id: QuitGuardId, cx: &mut App) {
        Self::of(cx).quit_guards.retain(|g| g.id != id);
    }

    /// Sets the quit dialogs (Package 7; one per app, the last one set wins).
    pub fn set_quit_prompt(prompt: QuitPrompt, cx: &mut App) {
        Self::of(cx).quit_prompt = Some(Rc::from(prompt));
    }

    /// Asks every guard now, in the order they were added.
    pub fn collect_guards(reason: QuitReason, cx: &App) -> QuitRequest {
        let guards = cx.try_global::<Self>().map_or_else(Vec::new, |s| {
            s.quit_guards
                .iter()
                .map(|g| (g.instance, (g.check)(cx)))
                .collect()
        });
        QuitRequest { reason, guards }
    }

    /// "Quit Dilla": asks the guards; with nothing in use (or no guard at all) the app quits at once, otherwise the
    /// quit prompt decides. A request while one is open, or while quitting, is ignored (beta.5 `askingQuit`).
    pub fn request_quit(cx: &mut App) {
        Self::request_quit_for(QuitReason::User, cx);
    }

    /// [`NativeShell::request_quit`] for `reason`.
    pub fn request_quit_for(reason: QuitReason, cx: &mut App) {
        if Self::is_busy_quitting(cx) {
            return;
        }
        let request = Self::collect_guards(reason, cx);
        let prompt = cx.try_global::<Self>().and_then(|s| s.quit_prompt.clone());
        match prompt {
            // No dialogs installed (before Package 7): quit, as the app did before the guards.
            Some(prompt) if !request.is_clear() => {
                Self::of(cx).asking_quit = true;
                prompt(request, cx);
            }
            _ => Self::quit_now(cx),
        }
    }

    /// The quit prompt's answer: quit now, or stay (the next request asks again).
    pub fn finish_quit(answer: QuitAnswer, cx: &mut App) {
        Self::of(cx).asking_quit = false;
        if answer == QuitAnswer::Quit {
            Self::quit_now(cx);
        }
    }

    /// Quits without asking: from here no link opens and no quit is asked again; every instance is released (its call
    /// left, its client stopped) and the platform quits.
    pub fn quit_now(cx: &mut App) {
        let shell = Self::of(cx);
        shell.asking_quit = false;
        shell.quitting = true;
        shell.queued_links.clear();
        crate::windows::release_all(cx);
        cx.quit();
    }

    /// The quit prompt is open.
    pub fn is_asking_quit(cx: &App) -> bool {
        cx.try_global::<Self>().is_some_and(|s| s.asking_quit)
    }

    /// The app is quitting ([`NativeShell::quit_now`] ran).
    pub fn is_quitting(cx: &App) -> bool {
        cx.try_global::<Self>().is_some_and(|s| s.quitting)
    }

    fn is_busy_quitting(cx: &App) -> bool {
        Self::is_asking_quit(cx) || Self::is_quitting(cx)
    }

    // --- links --------------------------------------------------------------------------------------------------------

    /// Sets the link handler (Package 6; one per app, the last one set wins). Links that came before it are handed to
    /// it now, oldest first.
    pub fn on_link(handler: Box<LinkHandler>, cx: &mut App) {
        let handler: Rc<LinkHandler> = Rc::from(handler);
        let shell = Self::of(cx);
        shell.link_handler = Some(handler.clone());
        let queued: Vec<Link> = shell.queued_links.drain(..).collect();
        for link in queued {
            if Self::is_busy_quitting(cx) {
                break;
            }
            handler(link, cx);
        }
    }

    /// A link arrived (the command line, a second launch, macOS `open-url`). It goes to the handler, or waits for one
    /// (at most [`MAX_QUEUED_LINKS`]). While the app asks to quit or quits, no link opens: it is dropped and this
    /// answers `false`.
    pub fn open_link(link: Link, cx: &mut App) -> bool {
        if Self::is_busy_quitting(cx) {
            return false;
        }
        let shell = Self::of(cx);
        match shell.link_handler.clone() {
            Some(handler) => handler(link, cx),
            None => {
                if shell.queued_links.len() == MAX_QUEUED_LINKS {
                    shell.queued_links.pop_front();
                }
                shell.queued_links.push_back(link);
            }
        }
        true
    }

    // --- windows ------------------------------------------------------------------------------------------------------

    /// Adds a listener of window events (a notifier withdraws a closed window's notifications; the tray lists the open
    /// instances).
    pub fn on_window_event(listener: Box<WindowListener>, cx: &mut App) {
        Self::of(cx).window_listeners.push(Rc::from(listener));
    }

    /// The open windows, oldest first: an instance's (`Some`) or the manager (`None`).
    pub fn open_windows(cx: &App) -> Vec<(WindowId, Option<InstanceId>)> {
        cx.try_global::<Self>()
            .map_or_else(Vec::new, |s| s.windows.clone())
    }

    /// `windows.rs`: a window opened.
    pub fn window_opened(window: WindowId, instance: Option<InstanceId>, cx: &mut App) {
        let shell = Self::of(cx);
        shell.windows.retain(|(w, _)| *w != window);
        shell.windows.push((window, instance));
        Self::tell_windows(&WindowEvent::Opened { window, instance }, cx);
    }

    /// `windows.rs`: instance `instance` now shows in `to` instead of `from` (`None`: it had no window the shell
    /// knew, so this is an open).
    pub fn window_moved(from: Option<WindowId>, to: WindowId, instance: InstanceId, cx: &mut App) {
        let shell = Self::of(cx);
        let known = from.and_then(|from| shell.windows.iter().position(|(w, _)| *w == from));
        match (from, known) {
            (Some(from), Some(at)) if from != to => {
                shell.windows[at] = (to, Some(instance));
                Self::tell_windows(&WindowEvent::Moved { from, to, instance }, cx);
            }
            (Some(_), Some(_)) => {}
            _ => Self::window_opened(to, Some(instance), cx),
        }
    }

    /// `windows.rs`: a window closed. A window the shell did not know (a moved-from window, a test's) tells nobody.
    pub fn window_closed(window: WindowId, cx: &mut App) {
        let shell = Self::of(cx);
        let Some(at) = shell.windows.iter().position(|(w, _)| *w == window) else {
            return;
        };
        let (_, instance) = shell.windows.remove(at);
        Self::tell_windows(&WindowEvent::Closed { window, instance }, cx);
    }

    fn tell_windows(event: &WindowEvent, cx: &mut App) {
        let listeners = Self::of(cx).window_listeners.clone();
        for listener in listeners {
            listener(event, cx);
        }
        Self::refresh_tray(cx);
    }

    /// Brings the app forward (a tray click, a second launch without a link, macOS reopen): the oldest open window,
    /// else the manager (beta.5 `focus(first window) ?? showManager()`). Nothing while the app quits.
    pub fn show_app(cx: &mut App) {
        if Self::is_busy_quitting(cx) {
            return;
        }
        for (id, _) in Self::open_windows(cx) {
            let Some(handle) = cx.windows().into_iter().find(|w| w.window_id() == id) else {
                continue;
            };
            if handle
                .update(cx, |_, window, _| window.activate_window())
                .is_ok()
            {
                cx.activate(true);
                return;
            }
        }
        if let Some(host) = Self::host(cx)
            && let Err(e) = crate::windows::open_manager(cx, &host.launch, host.allow_loopback)
        {
            log::warn!("dilla-desktop: the manager did not open: {e}");
        }
    }

    /// What `main.rs` handed [`install`] (`None` in tests that did not install the shell).
    pub fn host(cx: &App) -> Option<Host> {
        cx.try_global::<Self>().and_then(|s| s.host.clone())
    }
}

// ---------------------------------------------------------------------------------------------------------------------
// Start-up
// ---------------------------------------------------------------------------------------------------------------------

/// Before the run loop: takes the single-process lock, or hands this launch's argv (the program name first) to the
/// running app.
pub fn claim(argv: &[String]) -> Claimed {
    match single_instance::claim(argv) {
        single_instance::Claim::HandedOver => Claimed::HandedOver,
        single_instance::Claim::Primary(primary) => Claimed::Primary(Startup {
            hidden: autostart::hidden_start(argv),
            links: argv
                .iter()
                .skip(1)
                .filter_map(|a| links::parse(a))
                .collect(),
            primary,
        }),
    }
}

/// Before the run loop: the platform callbacks that only the `Application` takes (`on_open_urls`, `on_reopen`).
pub fn prepare(app: &Application) {
    links::prepare(app);
    menus::prepare(app);
}

/// Inside the run loop, before the first window: puts the shell in place, then each package's part, in a fixed order.
pub fn install(startup: Startup, host: Host, cx: &mut App) {
    let Startup {
        hidden: _,
        links,
        primary,
    } = startup;
    NativeShell::of(cx).host = Some(host);
    cx.on_action(|_: &Quit, cx| NativeShell::request_quit(cx));
    permissions::install(cx);
    notifier::install(cx);
    badge::install(cx);
    tray::install(cx);
    autostart::install(cx);
    menus::install(cx);
    quit::install(cx);
    links::install(cx);
    single_instance::install(primary, cx);
    for link in links {
        NativeShell::open_link(link, cx);
    }
}

#[cfg(test)]
mod tests {
    use std::cell::RefCell;

    use super::*;

    fn labels(items: &[TrayItem]) -> Vec<String> {
        items
            .iter()
            .map(|i| i.label().map_or("--".to_owned(), |l| l.to_string()))
            .collect()
    }

    fn rows(labels: &'static [&'static str]) -> impl Fn(&App) -> Vec<TrayItem> {
        move |_| {
            labels
                .iter()
                .map(|l| TrayItem::action(*l, |_| {}))
                .collect()
        }
    }

    fn invite(code: &str) -> Link {
        Link::Invite {
            host: "chat.example".into(),
            code: code.into(),
        }
    }

    /// The registry order: the tray lists the slots in beta.5's order whatever order the packages registered in, and
    /// the sections of one slot in the order they were added.
    #[gpui::test]
    fn tray_sections_follow_the_slot_order_then_the_order_added(cx: &mut gpui::TestAppContext) {
        cx.update(|cx| {
            NativeShell::add_tray_section(
                TraySection::new(TraySlot::Quit, rows(&["Quit Dilla"])),
                cx,
            );
            NativeShell::add_tray_section(
                TraySection::new(TraySlot::Instances, rows(&["Your instances…"])),
                cx,
            );
            NativeShell::add_tray_section(TraySection::new(TraySlot::Ring, rows(&["Alice"])), cx);
            NativeShell::add_tray_section(
                TraySection::new(TraySlot::Instances, rows(&["chat.example"])),
                cx,
            );
            NativeShell::add_tray_section(TraySection::new(TraySlot::AppAudio, rows(&[])), cx);
            assert_eq!(
                labels(&NativeShell::tray_menu(cx)),
                ["Alice", "Your instances…", "chat.example", "Quit Dilla"]
            );
        });
    }

    /// Adding a section and a window event re-draw the tray.
    #[gpui::test]
    fn the_tray_is_redrawn_on_change(cx: &mut gpui::TestAppContext) {
        let w = cx
            .add_empty_window()
            .update(|w, _| w.window_handle().window_id());
        let redraws = Rc::new(RefCell::new(0));
        let seen = redraws.clone();
        cx.update(|cx| {
            NativeShell::on_tray_changed(Box::new(move |_| *seen.borrow_mut() += 1), cx);
            NativeShell::add_tray_section(
                TraySection::new(TraySlot::Quit, rows(&["Quit Dilla"])),
                cx,
            );
            NativeShell::window_opened(w, None, cx);
        });
        assert_eq!(*redraws.borrow(), 2);
    }

    /// Quit with no guards quits at once, without the prompt; the global Quit action does the same.
    #[gpui::test]
    fn quit_with_no_guards_quits(cx: &mut gpui::TestAppContext) {
        let asked = Rc::new(RefCell::new(false));
        let seen = asked.clone();
        cx.update(|cx| {
            NativeShell::set_quit_prompt(Box::new(move |_, _| *seen.borrow_mut() = true), cx);
            cx.on_action(|_: &Quit, cx| NativeShell::request_quit(cx));
            cx.dispatch_action(&Quit);
            assert!(NativeShell::is_quitting(cx));
        });
        assert!(!*asked.borrow(), "nothing in use: no question");
    }

    /// Idle guards quit without the prompt too; with no prompt installed the app quits whatever the guards say.
    #[gpui::test]
    fn idle_guards_or_no_prompt_quit(cx: &mut gpui::TestAppContext) {
        cx.update(|cx| {
            NativeShell::set_quit_prompt(
                Box::new(|_, _| panic!("nothing in use: no question")),
                cx,
            );
            NativeShell::add_quit_guard(InstanceId(0), Box::new(|_| GuardState::IDLE), cx);
            NativeShell::request_quit(cx);
            assert!(NativeShell::is_quitting(cx));
        });
        cx.update(|cx| {
            cx.set_global(NativeShell::default());
            NativeShell::add_quit_guard(
                InstanceId(0),
                Box::new(|_| GuardState {
                    active_call: true,
                    ..GuardState::IDLE
                }),
                cx,
            );
            NativeShell::request_quit(cx);
            assert!(NativeShell::is_quitting(cx), "no dialogs yet: quit");
        });
    }

    /// A guard in use asks the prompt with every guard's answer in the order added; a second request while it is open
    /// is ignored; Stay keeps the app and Quit quits.
    #[gpui::test]
    fn a_guard_in_use_asks_the_prompt(cx: &mut gpui::TestAppContext) {
        let requests = Rc::new(RefCell::new(Vec::new()));
        let seen = requests.clone();
        cx.update(|cx| {
            NativeShell::set_quit_prompt(Box::new(move |r, _| seen.borrow_mut().push(r)), cx);
            NativeShell::add_quit_guard(InstanceId(2), Box::new(|_| GuardState::UNANSWERED), cx);
            let removed =
                NativeShell::add_quit_guard(InstanceId(5), Box::new(|_| GuardState::IDLE), cx);
            NativeShell::add_quit_guard(
                InstanceId(1),
                Box::new(|_| GuardState {
                    unsaved_draft: true,
                    ..GuardState::IDLE
                }),
                cx,
            );
            NativeShell::remove_quit_guard(removed, cx);
            NativeShell::request_quit(cx);
            assert!(NativeShell::is_asking_quit(cx));
            NativeShell::request_quit(cx);
            NativeShell::finish_quit(QuitAnswer::Stay, cx);
            assert!(!NativeShell::is_quitting(cx));
            assert!(!NativeShell::is_asking_quit(cx));
            NativeShell::request_quit_for(QuitReason::UpdateRestart, cx);
            NativeShell::finish_quit(QuitAnswer::Quit, cx);
            assert!(NativeShell::is_quitting(cx));
        });
        let requests = requests.borrow();
        assert_eq!(requests.len(), 2, "the request while asking was ignored");
        let first = &requests[0];
        assert_eq!(first.reason, QuitReason::User);
        assert_eq!(
            first.guards.iter().map(|(i, _)| i.0).collect::<Vec<_>>(),
            [2, 1]
        );
        assert!(first.unanswered());
        assert!(first.in_use());
        assert!(!first.is_clear());
        assert_eq!(requests[1].reason, QuitReason::UpdateRestart);
    }

    /// Links before a handler wait for it (oldest first, at most 16); none opens while the app quits.
    #[gpui::test]
    fn links_wait_for_the_handler_and_none_opens_during_quit(cx: &mut gpui::TestAppContext) {
        let opened = Rc::new(RefCell::new(Vec::new()));
        let seen = opened.clone();
        cx.update(|cx| {
            for n in 0..=MAX_QUEUED_LINKS {
                assert!(NativeShell::open_link(invite(&n.to_string()), cx));
            }
            NativeShell::on_link(Box::new(move |l, _| seen.borrow_mut().push(l)), cx);
            assert!(NativeShell::open_link(invite("late"), cx));
            NativeShell::quit_now(cx);
            assert!(!NativeShell::open_link(invite("during quit"), cx));
        });
        let opened = opened.borrow();
        assert_eq!(opened.len(), MAX_QUEUED_LINKS + 1);
        assert_eq!(opened[0], invite("1"), "the oldest was dropped");
        assert_eq!(opened[MAX_QUEUED_LINKS], invite("late"));
    }

    /// The badge is the sum across instances; a sink hears the total at once and on every change only.
    #[gpui::test]
    fn the_badge_sums_instances_and_tells_sinks_on_change(cx: &mut gpui::TestAppContext) {
        let heard = Rc::new(RefCell::new(Vec::new()));
        let seen = heard.clone();
        cx.update(|cx| {
            NativeShell::set_badge(InstanceId(0), 2, cx);
            NativeShell::add_badge_sink(Box::new(move |n, _| seen.borrow_mut().push(n)), cx);
            NativeShell::set_badge(InstanceId(3), 5, cx);
            NativeShell::set_badge(InstanceId(3), 5, cx);
            NativeShell::set_badge(InstanceId(0), 0, cx);
            assert_eq!(NativeShell::badge(cx), 5);
        });
        assert_eq!(*heard.borrow(), [2, 7, 5]);
    }

    /// Window events: an open, a start window's hand-over to its workspace window (a move, not a close), and a close
    /// with its instance; an unknown window's close tells nobody.
    #[gpui::test]
    fn window_events_follow_the_instance(cx: &mut gpui::TestAppContext) {
        let a = cx
            .add_empty_window()
            .update(|w, _| w.window_handle().window_id());
        let b = cx
            .add_empty_window()
            .update(|w, _| w.window_handle().window_id());
        let m = cx
            .add_empty_window()
            .update(|w, _| w.window_handle().window_id());
        let events = Rc::new(RefCell::new(Vec::new()));
        let seen = events.clone();
        let id = InstanceId(7);
        cx.update(|cx| {
            NativeShell::on_window_event(Box::new(move |e, _| seen.borrow_mut().push(*e)), cx);
            NativeShell::window_opened(m, None, cx);
            NativeShell::window_moved(None, a, id, cx);
            NativeShell::window_moved(Some(a), b, id, cx);
            NativeShell::window_closed(a, cx);
            assert_eq!(NativeShell::open_windows(cx), [(m, None), (b, Some(id))]);
            NativeShell::window_closed(b, cx);
        });
        assert_eq!(
            *events.borrow(),
            [
                WindowEvent::Opened {
                    window: m,
                    instance: None
                },
                WindowEvent::Opened {
                    window: a,
                    instance: Some(id)
                },
                WindowEvent::Moved {
                    from: a,
                    to: b,
                    instance: id
                },
                WindowEvent::Closed {
                    window: b,
                    instance: Some(id)
                },
            ]
        );
    }

    /// `claim` with the stubs: this process is the app, no hidden start, no link parsed yet.
    #[test]
    fn claim_is_primary_until_package_6() {
        let argv = vec!["dilla-desktop".to_owned(), "--other".to_owned()];
        match claim(&argv) {
            Claimed::Primary(startup) => {
                assert!(!startup.hidden);
                assert!(startup.links.is_empty());
            }
            Claimed::HandedOver => panic!("the stub never hands over"),
        }
    }
}
