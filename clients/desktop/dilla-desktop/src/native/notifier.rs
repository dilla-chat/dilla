//! Operating-system notifications (Package 4; beta.5 `main.ts:484-512`, `notify`, `notify-ring`,
//! `dismiss-notification`), on GPUI's own notification support (`App::show_system_notification`: XDG notifications
//! through `notify-rust` on Linux, `UNUserNotificationCenter` on macOS, toasts on Windows).
//!
//! - **Messages and mentions.** [`attach`] starts the client's notify engine (`Notify::start`) with an [`OsNotifier`]
//!   the first time a window draws the client's workspace; the engine keeps running while the client does (a closed
//!   window keeps its client in the background) and stops when the client is released ([`release`]). The engine
//!   decides as before ([`dilla_client::features::notify::should_notify`]: the device switch, the permission, mutes,
//!   the channel on screen), so what reaches the OS is what the in-app notifier recorded.
//! - **Rings.** `screens::ring::effects` raises a ring's notification while no window of its instance is focused,
//!   with no window at all too (closed to the tray) ([`show_ring`]): silent (the app plays the ring), with Answer and
//!   Decline, owned by the instance. A ring that ends, or a window of the instance focused again, withdraws it; closing
//!   the window does not. Answer and Decline act only on the ring that is still current (the notification is live and
//!   the ring is in the client's state now), as beta.5's `current()`; Answer with no window opens the instance's
//!   window, which answers. The tray lists each live ring with the same two rows ([`super::TraySlot::Ring`]).
//! - **Click.** The body brings the instance's window forward and opens the conversation (beta.5
//!   `notification-click`); with no window (closed to the tray) the instance's window opens.
//! - **Lifecycle.** Notifications are keyed per instance (`dilla:<instance>:<tag>`, beta.5 `noticeKey`): a later one
//!   with the same tag replaces it. At most [`MAX_LIVE`] are live; beta.5 refused the 65th, but GPUI reports no close
//!   of a notification, so here the oldest is withdrawn to make room (otherwise 64 shown notifications would silence
//!   the app for good). Closing a window withdraws its instance's message notifications
//!   ([`super::WindowEvent::Closed`]); its rings' notifications stay while they ring.
//! - **Identity.** [`install`] sets the app identity (`crate::window_app_id()`): the Windows AppUserModelID, which needs the
//!   installer's Start-menu shortcut to show toasts (release plan Task 13), and the Linux notification's app name;
//!   macOS shows notifications only from the app bundle.
//!
//! E2EE boundary: a notification carries the title and body the engine built (the sender, the channel and up to 200
//! characters of a message the core already decrypted), as beta.5 did; nothing else leaves the process.

use std::rc::Rc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};

use dilla_client::Client;
use dilla_client::features::notify::{
    Notification, NotificationKind, Notifier, NotifyEnv, NotifyTask, Presence,
};
use dilla_client::seams::InstanceId;
use dilla_client::state::ChannelId;
use dilla_client::state::profile::PresenceChoice;
use gpui::{
    App, Global, SharedString, SystemNotification, SystemNotificationAction,
    SystemNotificationResponse, Task, Window, WindowId,
};
use tokio::sync::mpsc;

use super::{NativeShell, TrayItem, TraySection, TraySlot, WindowEvent};
use crate::slots::SlotCx;

/// The most notifications live at once, across instances (beta.5's `notices.size >= 64`).
pub const MAX_LIVE: usize = 64;
/// A notification's title, in characters (beta.5's contract `notify.title`).
pub const TITLE_MAX: usize = 256;
/// A notification's body, in characters (beta.5's contract `notify.body`).
pub const BODY_MAX: usize = 4096;
/// The ring's action ids, as the response carries them back.
pub const ANSWER: &str = "answer";
pub const DECLINE: &str = "decline";
/// The ring's buttons (beta.5 `main.ts` literals: the OS notification's chrome is not in the web strings).
pub const ANSWER_LABEL: &str = "Answer";
pub const DECLINE_LABEL: &str = "Decline";

/// Who a notification belongs to: an instance (its engine, a registered window) or, for a window the shell does not
/// know (a fixture's), the window itself.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum Owner {
    Instance(InstanceId),
    Window(WindowId),
}

impl Owner {
    fn prefix(self) -> String {
        match self {
            Self::Instance(id) => format!("dilla:{}:", id.0),
            Self::Window(w) => format!("dilla:w{}:", w.as_u64()),
        }
    }

    /// The owner of what `window` shows: its instance when the shell knows it.
    pub fn of_window(window: WindowId, cx: &App) -> Self {
        NativeShell::open_windows(cx)
            .into_iter()
            .find(|(w, _)| *w == window)
            .and_then(|(_, i)| i)
            .map_or(Self::Window(window), Self::Instance)
    }
}

/// A ring notification's part: the ring's key in its instance (`screens::ring::ring_key`) and its title (the tray's
/// submenu label).
#[derive(Clone, Debug)]
struct RingPart {
    key: SharedString,
    title: SharedString,
}

/// One live notification.
#[derive(Clone, Debug)]
struct Live {
    /// The OS tag: `dilla:<owner>:<tag>`.
    tag: SharedString,
    owner: Owner,
    channel: Option<ChannelId>,
    ring: Option<RingPart>,
}

/// What the engine's thread hands the UI thread.
/// Each carries the id of the engine that sent it: the pump drops it once that engine is gone (a sign-out's queue never
/// shows the signed-out account's notifications).
enum Command {
    Show(u64, InstanceId, Notification),
    Withdraw(u64, InstanceId, String),
}

/// The notify engine of one client.
struct Engine {
    /// The engine's id, carried by its commands.
    id: u64,
    client: usize,
    instance: InstanceId,
    env: Arc<Env>,
    /// Set when the engine goes ([`release`], a new client of its instance): its notifier sends nothing more.
    stopped: Arc<AtomicBool>,
    /// The client's notify task (`None` for a notifier made on its own, [`os_notifier`]).
    _task: Option<NotifyTask>,
}

impl Engine {
    fn stop(&self) {
        self.stopped.store(true, Ordering::SeqCst);
    }
}

/// The registry of live notifications and running engines, one per app.
#[derive(Default)]
struct Notices {
    live: Vec<Live>,
    tx: Option<mpsc::UnboundedSender<Command>>,
    engines: Vec<Engine>,
    next_engine: u64,
    untagged: u64,
    _pump: Option<Task<()>>,
}

impl Global for Notices {}

fn notices(cx: &mut App) -> &mut Notices {
    cx.default_global::<Notices>()
}

/// The [`Notifier`] the engine of instance `instance` delivers to: it runs on the client's thread, so it hands each
/// notification to the UI thread, which shows it.
#[derive(Clone)]
pub struct OsNotifier {
    engine: u64,
    instance: InstanceId,
    stopped: Arc<AtomicBool>,
    tx: mpsc::UnboundedSender<Command>,
}

impl Notifier for OsNotifier {
    fn deliver(&self, notification: &Notification) {
        if self.stopped.load(Ordering::SeqCst) {
            return;
        }
        let _ = self.tx.send(Command::Show(
            self.engine,
            self.instance,
            notification.clone(),
        ));
    }

    fn withdraw(&self, tag: &str) {
        if self.stopped.load(Ordering::SeqCst) {
            return;
        }
        let _ = self.tx.send(Command::Withdraw(
            self.engine,
            self.instance,
            tag.to_owned(),
        ));
    }
}

/// A notifier for instance `instance`'s engine on client `client` (`Rc::as_ptr` of it), registered as that engine: what
/// [`attach`] hands `Notify::start`. It sends nothing once the engine goes ([`release_client_key`]).
pub fn os_notifier(instance: InstanceId, client: usize, cx: &mut App) -> OsNotifier {
    let tx = sender(cx);
    let n = notices(cx);
    n.next_engine += 1;
    let engine = n.next_engine;
    let stopped = Arc::new(AtomicBool::new(false));
    n.engines.push(Engine {
        id: engine,
        client,
        instance,
        env: Arc::new(Env::default()),
        stopped: stopped.clone(),
        _task: None,
    });
    OsNotifier {
        engine,
        instance,
        stopped,
        tx,
    }
}

fn engine_alive(engine: u64, cx: &App) -> bool {
    cx.try_global::<Notices>()
        .is_some_and(|n| n.engines.iter().any(|e| e.id == engine))
}

/// What the engine reads of the window (`NotifyEnv`): written on every frame of the window by [`attach`]: whether it is
/// focused, the channel it shows, and the account's presence choice (the user panel's status menu).
struct Env(Mutex<(bool, Option<ChannelId>, Presence)>);

impl Default for Env {
    fn default() -> Self {
        Self(Mutex::new((false, None, Presence::Online)))
    }
}

impl Env {
    fn set(&self, visible: bool, on_screen: Option<ChannelId>, presence: Presence) {
        *self
            .0
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner) = (visible, on_screen, presence);
    }

    fn get(&self) -> (bool, Option<ChannelId>, Presence) {
        *self
            .0
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }
}

/// `ctx.presenceChoice` of the notify decision. Not read yet, the choice counts as online (the app notified before the
/// choice was read at all; the web's worker holds every notice until its first read).
pub(crate) fn presence_of(choice: Option<PresenceChoice>) -> Presence {
    match choice {
        None | Some(PresenceChoice::Online) => Presence::Online,
        Some(PresenceChoice::Idle) => Presence::Idle,
        Some(PresenceChoice::Dnd) => Presence::Dnd,
        Some(PresenceChoice::Invisible) => Presence::Invisible,
    }
}

impl NotifyEnv for Env {
    fn visible(&self) -> bool {
        self.get().0
    }

    fn on_screen(&self) -> Option<ChannelId> {
        self.get().1
    }

    fn presence(&self) -> Presence {
        self.get().2
    }
}

// ---------------------------------------------------------------------------------------------------------------------
// Installation
// ---------------------------------------------------------------------------------------------------------------------

/// Called once by [`super::install`]: the app identity, the response handler, the pump from the engines' threads, the
/// window-close withdrawal and the tray's ring rows.
pub fn install(cx: &mut App) {
    cx.set_app_identity(crate::window_app_id(), crate::PRODUCT_NAME);
    cx.on_system_notification_response(respond);
    sender(cx);
    NativeShell::on_window_event(
        Box::new(|event, cx| {
            if let WindowEvent::Closed { window, instance } = *event {
                withdraw_owner(Owner::Window(window), cx);
                if let Some(id) = instance {
                    // Its messages go; its rings ring on without the window (GPUI gaps Task 3), and with no focused
                    // window left they notify now.
                    let owner = Owner::Instance(id);
                    withdraw_tags(|l| l.owner == owner && l.ring.is_none(), cx);
                    if let Some(e) = notices(cx).engines.iter().find(|e| e.instance == id) {
                        let presence = e.env.get().2;
                        e.env.set(false, None, presence);
                    }
                    crate::screens::ring::effects::resync(id, cx);
                }
            }
        }),
        cx,
    );
    NativeShell::add_tray_section(TraySection::new(TraySlot::Ring, tray_rows), cx);
}

/// The sender the engines deliver through; the first call starts the pump on the UI thread.
fn sender(cx: &mut App) -> mpsc::UnboundedSender<Command> {
    if let Some(tx) = notices(cx).tx.clone() {
        return tx;
    }
    let (tx, mut rx) = mpsc::unbounded_channel::<Command>();
    let pump = cx.spawn(async move |cx| {
        while let Some(command) = rx.recv().await {
            cx.update(|cx| match command {
                // A command of an engine that is gone (released, replaced) is dropped.
                Command::Show(engine, id, n) => {
                    if engine_alive(engine, cx) {
                        show(Owner::Instance(id), &n, cx);
                    }
                }
                Command::Withdraw(engine, id, tag) => {
                    if engine_alive(engine, cx) {
                        withdraw(Owner::Instance(id), &tag, cx);
                    }
                }
            });
        }
    });
    let n = notices(cx);
    n.tx = Some(tx.clone());
    n._pump = Some(pump);
    tx
}

// ---------------------------------------------------------------------------------------------------------------------
// The engines
// ---------------------------------------------------------------------------------------------------------------------

/// Every frame of a window that shows a client's workspace (`screens::ring::effects::sync`): starts the client's notify
/// engine once, and tells it whether the window is focused and which channel it shows. A window the shell does not
/// know (a fixture's) starts nothing.
pub fn attach(ctx: &SlotCx, window: &Window, cx: &mut App) {
    let Some(client) = &ctx.client else { return };
    let id = window.window_handle().window_id();
    let Owner::Instance(instance) = Owner::of_window(id, cx) else {
        return;
    };
    let key = Rc::as_ptr(client) as usize;
    let visible = window.is_window_active();
    let presence = presence_of(ctx.state.profile.presence_choice);
    if let Some(e) = notices(cx).engines.iter().find(|e| e.client == key) {
        e.env.set(visible, ctx.channel, presence);
        return;
    }
    // A new client of this instance (a sign-in again): the old engine goes.
    notices(cx).engines.retain(|e| {
        let keep = e.instance != instance;
        if !keep {
            e.stop();
        }
        keep
    });
    let notifier = os_notifier(instance, key, cx);
    let engine = notifier.engine;
    let Some(env) = notices(cx)
        .engines
        .iter()
        .find(|e| e.id == engine)
        .map(|e| e.env.clone())
    else {
        return;
    };
    env.set(visible, ctx.channel, presence);
    let task = client.notify().start(
        client,
        Arc::new(notifier),
        env,
        crate::screens::notify::text(),
    );
    if let Some(e) = notices(cx).engines.iter_mut().find(|e| e.id == engine) {
        e._task = Some(task);
    }
}

/// Lets go of `client` (Forget, sign-out: `crate::release::release_client` through the ring area): its engine stops
/// and its instance's notifications are withdrawn.
pub fn release(client: &Rc<Client>, cx: &mut App) {
    release_client_key(Rc::as_ptr(client) as usize, cx);
}

/// [`release`] by the client's key (`Rc::as_ptr`): its engines stop first (their notifiers send nothing more, and
/// what they queued is dropped), then its instance's notifications are withdrawn.
pub fn release_client_key(key: usize, cx: &mut App) {
    if !cx.has_global::<Notices>() {
        return;
    }
    let n = notices(cx);
    let gone: Vec<InstanceId> = n
        .engines
        .iter()
        .filter(|e| e.client == key)
        .map(|e| {
            e.stop();
            e.instance
        })
        .collect();
    n.engines.retain(|e| e.client != key);
    for id in gone {
        withdraw_owner(Owner::Instance(id), cx);
    }
}

/// Whether an engine runs for `instance` (tests).
pub fn engine_runs(instance: InstanceId, cx: &App) -> bool {
    cx.try_global::<Notices>()
        .is_some_and(|n| n.engines.iter().any(|e| e.instance == instance))
}

// ---------------------------------------------------------------------------------------------------------------------
// Showing and withdrawing
// ---------------------------------------------------------------------------------------------------------------------

fn clip(s: &str, max: usize) -> SharedString {
    s.chars().take(max).collect::<String>().into()
}

/// XDG notification servers render a subset of HTML in the body (and some in the summary): text that came from a
/// message is escaped so it shows as written and never as markup or a link.
pub fn escape_markup(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for ch in s.chars() {
        match ch {
            '&' => out.push_str("&amp;"),
            '<' => out.push_str("&lt;"),
            '>' => out.push_str("&gt;"),
            '"' => out.push_str("&quot;"),
            '\'' => out.push_str("&#39;"),
            c => out.push(c),
        }
    }
    out
}

/// The title or body as the platform gets it: clipped to its cap on the original text, then escaped on Linux.
fn os_text(s: &str, max: usize) -> SharedString {
    let clipped = clip(s, max);
    if cfg!(target_os = "linux") {
        escape_markup(&clipped).into()
    } else {
        clipped
    }
}

fn os_tag(owner: Owner, tag: &str, cx: &mut App) -> SharedString {
    let tag = if tag.is_empty() {
        let n = notices(cx);
        n.untagged += 1;
        format!("untagged-{}", n.untagged)
    } else {
        tag.to_owned()
    };
    format!("{}{tag}", owner.prefix()).into()
}

/// Shows `notification` for `owner` (a later one with its tag replaces it). Answers the OS tag.
pub fn show(owner: Owner, notification: &Notification, cx: &mut App) -> SharedString {
    post(owner, notification, None, cx)
}

/// Shows a ring's notification, raised by `instance` for its ring `key`: silent, with Answer and Decline.
pub fn show_ring(
    instance: InstanceId,
    key: SharedString,
    notification: &Notification,
    cx: &mut App,
) -> SharedString {
    let owner = Owner::Instance(instance);
    let ring = RingPart {
        key,
        title: clip(&notification.title, TITLE_MAX),
    };
    post(owner, notification, Some(ring), cx)
}

fn post(
    owner: Owner,
    notification: &Notification,
    ring: Option<RingPart>,
    cx: &mut App,
) -> SharedString {
    let tag = os_tag(owner, &notification.tag, cx);
    let is_ring = ring.is_some() || notification.kind == NotificationKind::Ring;
    let n = notices(cx);
    n.live.retain(|l| l.tag != tag);
    // At most MAX_LIVE: the oldest goes (a ring last of all).
    let mut evicted = Vec::new();
    while n.live.len() >= MAX_LIVE {
        let at = n.live.iter().position(|l| l.ring.is_none()).unwrap_or(0);
        evicted.push(n.live.remove(at).tag);
    }
    n.live.push(Live {
        tag: tag.clone(),
        owner,
        channel: notification.channel,
        ring,
    });
    for old in evicted {
        cx.dismiss_system_notification(&old);
    }
    let actions = if is_ring {
        vec![
            SystemNotificationAction {
                id: ANSWER.into(),
                label: ANSWER_LABEL.into(),
            },
            SystemNotificationAction {
                id: DECLINE.into(),
                label: DECLINE_LABEL.into(),
            },
        ]
    } else {
        Vec::new()
    };
    cx.show_system_notification(SystemNotification {
        tag: tag.clone(),
        title: os_text(&notification.title, TITLE_MAX),
        body: os_text(&notification.body, BODY_MAX),
        actions,
    });
    if is_ring {
        NativeShell::refresh_tray(cx);
    }
    tag
}

/// Withdraws `owner`'s notification `tag` (the engine's tag, without the owner's prefix).
pub fn withdraw(owner: Owner, tag: &str, cx: &mut App) {
    let full = format!("{}{tag}", owner.prefix());
    withdraw_tags(|l| l.tag.as_ref() == full, cx);
}

/// Withdraws the ring notification of `instance`'s ring `key` (the ring ended, or a window of it is focused again).
pub fn withdraw_ring(instance: InstanceId, key: &SharedString, cx: &mut App) {
    let owner = Owner::Instance(instance);
    withdraw_tags(
        |l| l.owner == owner && l.ring.as_ref().is_some_and(|r| r.key == *key),
        cx,
    );
}

/// Withdraws every notification of `owner` (its window closed, its client was released).
pub fn withdraw_owner(owner: Owner, cx: &mut App) {
    withdraw_tags(|l| l.owner == owner, cx);
}

fn withdraw_tags(which: impl Fn(&Live) -> bool, cx: &mut App) {
    if !cx.has_global::<Notices>() {
        return;
    }
    let n = notices(cx);
    let (gone, kept): (Vec<Live>, Vec<Live>) = std::mem::take(&mut n.live)
        .into_iter()
        .partition(|l| which(l));
    n.live = kept;
    let rings = gone.iter().any(|l| l.ring.is_some());
    for l in gone {
        cx.dismiss_system_notification(&l.tag);
    }
    if rings {
        NativeShell::refresh_tray(cx);
    }
}

/// The OS tags live now, oldest first (tests, the tray).
pub fn live_tags(cx: &App) -> Vec<SharedString> {
    cx.try_global::<Notices>()
        .map_or_else(Vec::new, |n| n.live.iter().map(|l| l.tag.clone()).collect())
}

// ---------------------------------------------------------------------------------------------------------------------
// Responses
// ---------------------------------------------------------------------------------------------------------------------

/// The user activated a notification: its body (focus and open), or a ring's Answer or Decline.
pub fn respond(response: SystemNotificationResponse, cx: &mut App) {
    if NativeShell::is_quitting(cx) {
        return;
    }
    let Some(live) = cx
        .try_global::<Notices>()
        .and_then(|n| n.live.iter().find(|l| l.tag == response.tag).cloned())
    else {
        // Withdrawn already (its ring ended, its window closed): nothing to act on.
        return;
    };
    match (response.action_id.as_deref(), &live.ring) {
        (Some(ANSWER), Some(ring)) => ring_action(&live, ring.clone(), true, cx),
        (Some(DECLINE), Some(ring)) => ring_action(&live, ring.clone(), false, cx),
        (None, _) => {
            withdraw_tags(|l| l.tag == live.tag, cx);
            open(&live, cx);
        }
        _ => {}
    }
}

fn ring_action(live: &Live, ring: RingPart, answer: bool, cx: &mut App) {
    withdraw_tags(|l| l.tag == live.tag, cx);
    if let Owner::Instance(instance) = live.owner {
        crate::screens::ring::act_on_instance(instance, &ring.key, answer, cx);
    }
}

/// A click on the body: the window of the notification's instance comes forward (opened when it was closed to the
/// tray) and shows the conversation.
fn open(live: &Live, cx: &mut App) {
    let window = match live.owner {
        Owner::Window(w) => Some(w),
        Owner::Instance(id) => NativeShell::open_windows(cx)
            .into_iter()
            .find(|(_, i)| *i == Some(id))
            .map(|(w, _)| w),
    };
    let Some(window) = window else {
        if let Owner::Instance(id) = live.owner {
            super::tray::show_instance(id, cx);
        }
        return;
    };
    let Some(handle) = cx.windows().into_iter().find(|w| w.window_id() == window) else {
        return;
    };
    let channel = live.channel;
    let _ = handle.update(cx, |_, window, _| window.activate_window());
    cx.activate(true);
    let Some(channel) = channel else { return };
    // A test's window whose root is the shell itself: the shell knows the channel.
    if let Some(shell) = handle.downcast::<crate::screens::shell::ShellView>() {
        let _ = shell.update(cx, |shell, _, cx| {
            if shell.slot_cx().state.channel(channel).is_some() {
                shell.select(channel, cx);
            } else {
                shell.navigate(crate::router::Route::Dm(channel), cx);
            }
        });
        return;
    }
    // An instance window: its client tells a server's channel from a DM, and the shell's navigator goes there.
    let community = match live.owner {
        Owner::Instance(id) => crate::windows::running_client(cx, id)
            .and_then(|c| c.state().borrow().channel(channel).map(|c| c.community_id)),
        Owner::Window(_) => None,
    };
    let to = match community {
        Some(community) => crate::screens::dms::Nav::Channel { community, channel },
        None => crate::screens::dms::Nav::Dm(channel),
    };
    let _ = handle.update(cx, |_, window, cx| {
        crate::screens::dms::navigate(to, window, cx)
    });
}

// ---------------------------------------------------------------------------------------------------------------------
// The tray's ring rows
// ---------------------------------------------------------------------------------------------------------------------

/// One submenu per live ring notification, with Answer and Decline (beta.5 `ringActions`).
fn tray_rows(cx: &App) -> Vec<TrayItem> {
    let Some(n) = cx.try_global::<Notices>() else {
        return Vec::new();
    };
    n.live
        .iter()
        .filter_map(|l| {
            let ring = l.ring.clone()?;
            let answer = SystemNotificationResponse {
                tag: l.tag.clone(),
                action_id: Some(ANSWER.into()),
            };
            let decline = SystemNotificationResponse {
                tag: l.tag.clone(),
                action_id: Some(DECLINE.into()),
            };
            Some(TrayItem::Submenu {
                label: ring.title,
                items: vec![
                    TrayItem::action(ANSWER_LABEL, move |cx| respond(answer.clone(), cx)),
                    TrayItem::action(DECLINE_LABEL, move |cx| respond(decline.clone(), cx)),
                ],
            })
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn note(tag: &str) -> Notification {
        let mut n = Notification::new(NotificationKind::Mention, "t", "b");
        n.tag = tag.into();
        n
    }

    /// Titles and bodies are clipped to beta.5's contract; a tag replaces its earlier notification.
    #[gpui::test]
    fn clipped_and_replaced_by_tag(cx: &mut gpui::TestAppContext) {
        cx.update(|cx| {
            install(cx);
            let mut long = note("dilla:c1");
            long.title = "x".repeat(TITLE_MAX + 10);
            long.body = "y".repeat(BODY_MAX + 10);
            show(Owner::Instance(InstanceId(1)), &long, cx);
            show(Owner::Instance(InstanceId(1)), &note("dilla:c1"), cx);
            show(Owner::Instance(InstanceId(2)), &note("dilla:c1"), cx);
            assert_eq!(live_tags(cx), ["dilla:1:dilla:c1", "dilla:2:dilla:c1"]);
        });
        let shown = cx.shown_system_notifications();
        assert_eq!(shown[0].title.chars().count(), TITLE_MAX);
        assert_eq!(shown[0].body.chars().count(), BODY_MAX);
        assert_eq!(cx.delivered_system_notifications().len(), 2);
    }

    /// The status menu's choice reaches the notify decision (do not disturb holds the notices); not read yet is online.
    #[test]
    fn the_presence_choice_is_the_decisions_presence() {
        assert_eq!(presence_of(None), Presence::Online);
        assert_eq!(presence_of(Some(PresenceChoice::Dnd)), Presence::Dnd);
        assert_eq!(presence_of(Some(PresenceChoice::Idle)), Presence::Idle);
        assert_eq!(
            presence_of(Some(PresenceChoice::Invisible)),
            Presence::Invisible
        );
        let env = Env::default();
        assert_eq!(env.presence(), Presence::Online);
        env.set(true, None, Presence::Dnd);
        assert_eq!(env.presence(), Presence::Dnd);
    }

    /// Security review: message text never reaches an XDG server as markup; the cap applies to the original text.
    #[test]
    fn markup_is_escaped_after_the_cap() {
        assert_eq!(
            escape_markup("<b>x</b> & <a href=\"https://e.example\">'y'</a>"),
            "&lt;b&gt;x&lt;/b&gt; &amp; &lt;a href=&quot;https://e.example&quot;&gt;&#39;y&#39;&lt;/a&gt;"
        );
        let long = "<".repeat(BODY_MAX + 5);
        let body = os_text(&long, BODY_MAX);
        if cfg!(target_os = "linux") {
            assert_eq!(body.as_ref(), "&lt;".repeat(BODY_MAX));
        } else {
            assert_eq!(body.chars().count(), BODY_MAX);
        }
    }

    #[cfg(target_os = "linux")]
    #[gpui::test]
    fn the_platform_gets_escaped_text_on_linux(cx: &mut gpui::TestAppContext) {
        cx.update(|cx| {
            install(cx);
            let mut n = note("t");
            n.title = "<i>Bob</i>".into();
            n.body = "<b>x</b> & <a href=\"https://e.example\">go</a>".into();
            show(Owner::Instance(InstanceId(1)), &n, cx);
        });
        let shown = cx.shown_system_notifications();
        assert_eq!(shown[0].title.as_ref(), "&lt;i&gt;Bob&lt;/i&gt;");
        assert_eq!(
            shown[0].body.as_ref(),
            "&lt;b&gt;x&lt;/b&gt; &amp; &lt;a href=&quot;https://e.example&quot;&gt;go&lt;/a&gt;"
        );
    }

    /// Security review: after a release, a notification its engine delivers late (or queued before) shows nothing.
    #[gpui::test]
    fn a_late_show_after_release_shows_nothing(cx: &mut gpui::TestAppContext) {
        let (queued, late) = cx.update(|cx| {
            install(cx);
            let n = os_notifier(InstanceId(1), 77, cx);
            n.deliver(&note("queued"));
            release_client_key(77, cx);
            (n.clone(), n)
        });
        drop(queued);
        late.deliver(&note("late"));
        cx.run_until_parked();
        assert!(cx.shown_system_notifications().is_empty());
        assert!(cx.update(|cx| live_tags(cx)).is_empty());
        // A notifier still registered shows.
        let n = cx.update(|cx| os_notifier(InstanceId(2), 78, cx));
        n.deliver(&note("live"));
        cx.run_until_parked();
        assert_eq!(cx.shown_system_notifications().len(), 1);
    }
}
