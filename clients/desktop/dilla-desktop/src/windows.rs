//! One window and one `Client` per instance in one process (Task 27), and the manager window that lists them.
//!
//! The [`Registry`] (an app global) holds, per instance with a window or a running client, the hold on it
//! ([`InstanceGuard`]: a second window or another process cannot open it, and a removal refuses it), its window and
//! its client. Closing a window drops only the window: the client, and the hold, stay until the instance is removed
//! or the app quits, as the Electron shell keeps a closed window's client alive for background work
//! (the former Electron shell, tag desktop-v0.1.0-beta.5, `packages/desktop/test/electron/shell.spec.ts`, "closing a window keeps the client alive"). Opening the instance
//! again shows a new window on the running client.

use std::any::Any;
use std::collections::HashMap;
use std::rc::{Rc, Weak};

use dilla_client::Client;
use dilla_client::instances::{InstanceError, InstanceGuard, Instances};
use dilla_client::seams::{InstanceConfig, InstanceId};
use dilla_client::store::StoreError;
use gpui::{
    App, AppContext as _, Bounds, Global, TitlebarOptions, WindowBounds, WindowHandle, WindowId,
    WindowOptions, px, size,
};

use crate::screens::instances::copy;
use crate::screens::instances::model::Manager;
use crate::screens::instances::{ManagerEvent, ManagerView};
use crate::{AppView, Launch};

/// The manager window's size, logical px (`showManager`: 720x760).
pub const MANAGER_SIZE: (f32, f32) = (720.0, 760.0);
/// The manager window's smallest width (`minWidth: 480`).
pub const MANAGER_MIN_WIDTH: f32 = 480.0;

/// Why an instance's window did not open.
#[derive(Debug)]
pub enum OpenError {
    /// It is open in another process, or its list or lock could not be read.
    Instance(InstanceError),
    /// The platform refused the window.
    Window(String),
}

impl std::fmt::Display for OpenError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Instance(e) => e.fmt(f),
            Self::Window(e) => f.write_str(e),
        }
    }
}

impl std::error::Error for OpenError {}

struct Live {
    /// The instance's hold, kept while its window or its client exists.
    guard: InstanceGuard,
    window: Option<WindowHandle<AppView>>,
    client: Option<Rc<Client>>,
    /// Set while a window's `Client::start` runs; a window opened meanwhile waits for it instead of starting a second
    /// client. Only a `Weak`: the token is owned by the window's start task, so it dies when the start ends or the
    /// window closes mid-start (this never keeps a start alive by itself).
    starting: Option<Weak<()>>,
}

/// A released instance whose client has not stopped yet: its hold stays until `alive` is gone, so a removal or another
/// window never meets the old client's open store.
struct Retired {
    id: InstanceId,
    _guard: InstanceGuard,
    alive: Weak<dyn Any>,
}

/// The windows and clients of this process.
#[derive(Default)]
pub struct Registry {
    manager: Option<WindowHandle<ManagerView>>,
    live: HashMap<u8, Live>,
    retired: Vec<Retired>,
    closing_hook: bool,
}

impl Global for Registry {}

/// What a closed window was.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Closed {
    Manager,
    Instance(InstanceId),
    Other,
}

impl Registry {
    fn of(cx: &mut App) -> &mut Self {
        if !cx.has_global::<Self>() {
            cx.set_global(Self::default());
        }
        cx.global_mut::<Self>()
    }

    /// Forgets the window `id` and says what it was. Its client stays.
    fn closed(&mut self, id: WindowId) -> Closed {
        if self.manager.is_some_and(|m| m.window_id() == id) {
            self.manager = None;
            return Closed::Manager;
        }
        for (n, live) in &mut self.live {
            if live.window.is_some_and(|w| w.window_id() == id) {
                live.window = None;
                return Closed::Instance(InstanceId(*n));
            }
        }
        Closed::Other
    }
}

/// The instances with a running client in this process.
pub fn running_instances(cx: &App) -> Vec<InstanceId> {
    cx.try_global::<Registry>().map_or_else(Vec::new, |r| {
        let mut ids: Vec<u8> = r
            .live
            .iter()
            .filter(|(_, l)| l.client.is_some())
            .map(|(n, _)| *n)
            .collect();
        ids.sort_unstable();
        ids.into_iter().map(InstanceId).collect()
    })
}

/// The client already running for `id`: a window opened again on an instance shows it, never starts a second one.
pub fn running_client(cx: &App, id: InstanceId) -> Option<Rc<Client>> {
    cx.try_global::<Registry>()
        .and_then(|r| r.live.get(&id.0))
        .and_then(|l| l.client.clone())
}

/// Re-review C: the instances whose store a Sign out still has to wipe (a holder kept the old client), and how often
/// each instance was started, so a wipe that waited never deletes a session started after it. Pending wipes are
/// counted (final review M6): two wipes of one instance keep it pending until both ended.
#[derive(Default)]
struct Wipes {
    pending: HashMap<InstanceId, u32>,
    starts: HashMap<InstanceId, u64>,
}

impl Global for Wipes {}

/// A wipe of `id` (a Sign out's or a Forget's) waits for the old client: a start of `id` is refused until
/// [`wipe_end`]. Answers the instance's start count now, for [`wipe_may_delete`].
pub fn wipe_begin(cx: &mut App, id: InstanceId) -> u64 {
    let w = cx.default_global::<Wipes>();
    *w.pending.entry(id).or_insert(0) += 1;
    w.starts.get(&id).copied().unwrap_or(0)
}

/// One pending wipe of `id` ended (done, or skipped).
pub fn wipe_end(cx: &mut App, id: InstanceId) {
    let w = cx.default_global::<Wipes>();
    if let Some(n) = w.pending.get_mut(&id) {
        *n = n.saturating_sub(1);
        if *n == 0 {
            w.pending.remove(&id);
        }
    }
}

/// A wipe of `id` is still pending: a start of it waits.
pub fn wipe_pending(cx: &App, id: InstanceId) -> bool {
    cx.try_global::<Wipes>()
        .is_some_and(|w| w.pending.contains_key(&id))
}

/// A client of `id` is starting (any window): a wipe that began before it may no longer delete anything.
pub fn note_start(cx: &mut App, id: InstanceId) {
    *cx.default_global::<Wipes>().starts.entry(id).or_insert(0) += 1;
}

/// Whether the wipe of `id` that began at start count `generation` may still delete the store: no client of `id` runs
/// or starts in this process and none started since (defence in depth behind the refused sign-in).
pub fn wipe_may_delete(cx: &App, id: InstanceId, generation: u64) -> bool {
    let starts = cx
        .try_global::<Wipes>()
        .and_then(|w| w.starts.get(&id).copied())
        .unwrap_or(0);
    running_client(cx, id).is_none() && !is_starting(cx, id) && starts == generation
}

/// Hands the registry the client a window started for `id`, so closing the window does not stop it.
pub fn keep_client(cx: &mut App, id: InstanceId, client: Rc<Client>) {
    if let Some(live) = Registry::of(cx).live.get_mut(&id.0) {
        live.client = Some(client);
        live.starting = None;
    }
}

/// Lets go of the client the registry keeps for `id` (Task 26's Forget closes its store): the window and the hold stay.
pub fn drop_client(cx: &mut App, id: InstanceId) {
    if let Some(live) = Registry::of(cx).live.get_mut(&id.0) {
        live.client = None;
    }
}

/// The instance's window is now `window`: the compact start window handed its client to a workspace window (Task 8's
/// window shapes) and closes, so the registry follows the new one.
pub fn moved_window(cx: &mut App, id: InstanceId, window: WindowHandle<AppView>) {
    if let Some(live) = Registry::of(cx).live.get_mut(&id.0) {
        live.window = Some(window);
    }
}

/// What [`mark_starting`] found.
pub enum Starting {
    /// This window may start the client. The token (when the instance is registered) is kept alive until the start ends.
    Go(Option<Rc<()>>),
    /// Another window's start of the instance is running.
    Busy,
}

/// Marks a `Client::start` for `id` as running.
pub fn mark_starting(cx: &mut App, id: InstanceId) -> Starting {
    let Some(live) = Registry::of(cx).live.get_mut(&id.0) else {
        return Starting::Go(None);
    };
    if live.starting.as_ref().is_some_and(|w| w.strong_count() > 0) {
        return Starting::Busy;
    }
    let token = Rc::new(());
    live.starting = Some(Rc::downgrade(&token));
    Starting::Go(Some(token))
}

/// Whether a window's `Client::start` for `id` is running.
pub fn is_starting(cx: &App, id: InstanceId) -> bool {
    cx.try_global::<Registry>()
        .and_then(|r| r.live.get(&id.0))
        .is_some_and(|l| l.starting.as_ref().is_some_and(|w| w.strong_count() > 0))
}

/// Ends a failed start for `id`.
pub fn finish_starting(cx: &mut App, id: InstanceId) {
    if let Some(live) = Registry::of(cx).live.get_mut(&id.0) {
        live.starting = None;
    }
}

/// Whether this process holds instance `id` (a window or client of it exists, or its client is still stopping).
pub fn held_here(cx: &App, id: InstanceId) -> bool {
    cx.try_global::<Registry>()
        .is_some_and(|r| r.live.contains_key(&id.0) || r.retired.iter().any(|t| t.id == id))
}

/// Whether instance `id` has let go of everything: no window or client, and a released client has really stopped.
/// The hold of a released instance is dropped here, once its client is gone.
pub fn stopped(cx: &mut App, id: InstanceId) -> bool {
    let registry = Registry::of(cx);
    registry.retired.retain(|t| t.alive.upgrade().is_some());
    !registry.live.contains_key(&id.0) && !registry.retired.iter().any(|t| t.id == id)
}

/// Keeps `guard` (the hold of `id`) until `alive` is dropped: [`release_instance`] hands over the client it stops, and
/// tests hand over a stand-in.
pub fn retire_hold(cx: &mut App, id: InstanceId, guard: InstanceGuard, alive: Weak<dyn Any>) {
    Registry::of(cx).retired.push(Retired {
        id,
        _guard: guard,
        alive,
    });
    // Let go of the hold as soon as the client has stopped, whether or not a removal is waiting for it.
    cx.spawn(async move |cx| {
        loop {
            cx.background_executor().timer(RETIRE_POLL).await;
            let pending = cx.update(|cx| {
                let registry = Registry::of(cx);
                registry.retired.retain(|t| t.alive.upgrade().is_some());
                registry.retired.iter().any(|t| t.id == id)
            });
            if !pending {
                break;
            }
        }
    })
    .detach();
}

/// How often a released instance's hold is checked for release.
const RETIRE_POLL: std::time::Duration = std::time::Duration::from_millis(250);

/// Called when a window has closed and none is left: opens the manager when a client still runs (so the app does not
/// vanish with work in the background and nothing to bring it back), else quits. Wire it from `main`'s
/// `on_window_closed`.
pub fn after_window_closed(cx: &mut App, launch: &Launch, allow_loopback: bool) {
    if !cx.windows().is_empty() {
        return;
    }
    if running_instances(cx).is_empty() || open_manager(cx, launch, allow_loopback).is_err() {
        cx.quit();
    }
}

/// Records which window closed and says what it was.
fn note_closed(cx: &mut App, id: WindowId) -> Closed {
    Registry::of(cx).closed(id)
}

/// Opens the manager window, or brings the open one forward. Its "Open" requests open instance windows.
pub fn open_manager(
    cx: &mut App,
    launch: &Launch,
    allow_loopback: bool,
) -> Result<WindowHandle<ManagerView>, OpenError> {
    crate::install(cx);
    install_close_hook(cx, launch.clone(), allow_loopback);
    if let Some(open) = Registry::of(cx).manager {
        if open
            .update(cx, |_, window, _| window.activate_window())
            .is_ok()
        {
            return Ok(open);
        }
        Registry::of(cx).manager = None;
    }
    let model = match &launch.root {
        Some(root) => Manager::load(root, launch.keychain.clone(), allow_loopback),
        None => Err(StoreError::NoDataDir),
    };
    let bounds = Bounds::centered(None, size(px(MANAGER_SIZE.0), px(MANAGER_SIZE.1)), cx);
    let launch = launch.clone();
    let handle = cx
        .open_window(manager_options(bounds), move |window, cx| {
            window.set_window_title(copy::WINDOW_TITLE);
            let host = crate::screens::instances::view::Host {
                release: Rc::new(release_instance_in),
                held_here: Rc::new(|id, cx| held_here(cx, id)),
                stopped: Rc::new(|id, cx| stopped(cx, id)),
            };
            let view = cx.new(|cx| ManagerView::new(model, host, window, cx));
            let weak = view.downgrade();
            cx.subscribe(&view, move |_, event: &ManagerEvent, cx| {
                let ManagerEvent::Open { instance, invite } = event;
                if let Err(e) = open_instance(
                    cx,
                    &launch,
                    instance.clone(),
                    invite.clone(),
                    allow_loopback,
                ) {
                    let message = match &e {
                        OpenError::Instance(InstanceError::InUse) => {
                            crate::screens::instances::copy::ERROR_IN_USE.to_owned()
                        }
                        _ => crate::screens::instances::copy::ERROR_STORE.to_owned(),
                    };
                    weak.update(cx, |view, cx| view.show_error(message, cx))
                        .ok();
                }
            })
            .detach();
            view
        })
        .map_err(|e| OpenError::Window(e.to_string()))?;
    Registry::of(cx).manager = Some(handle);
    Ok(handle)
}

/// The manager window's options at `bounds`: no title through the options (the window sets it once, as the main
/// window does), the preview's app id, and a smallest width.
pub fn manager_options(bounds: Bounds<gpui::Pixels>) -> WindowOptions {
    WindowOptions {
        window_bounds: Some(WindowBounds::Windowed(bounds)),
        window_min_size: Some(size(px(MANAGER_MIN_WIDTH), px(0.0))),
        titlebar: Some(TitlebarOptions {
            title: None,
            ..Default::default()
        }),
        app_id: Some(crate::APP_ID.to_owned()),
        ..Default::default()
    }
}

/// Opens the window of instance `cfg`, on its running client if it has one, or brings its window forward. Another
/// window of the same instance, here or in another process, is refused ([`InstanceError::InUse`]): the store lock of
/// the instance is held while its window is open.
pub fn open_instance(
    cx: &mut App,
    launch: &Launch,
    cfg: InstanceConfig,
    invite: Option<String>,
    allow_loopback: bool,
) -> Result<(), OpenError> {
    install_close_hook(cx, launch.clone(), allow_loopback);
    if let Some(live) = Registry::of(cx).live.get(&cfg.id.0) {
        if let Some(window) = live.window
            && window
                .update(cx, |_, window, _| window.activate_window())
                .is_ok()
        {
            return Ok(());
        }
    } else {
        let root = launch
            .root
            .as_deref()
            .ok_or(OpenError::Instance(InstanceError::Store(
                StoreError::NoDataDir,
            )))?;
        let guard = Instances::load(root)
            .map_err(InstanceError::from)
            .and_then(|instances| instances.hold(cfg.id))
            .map_err(OpenError::Instance)?;
        Registry::of(cx).live.insert(
            cfg.id.0,
            Live {
                guard,
                window: None,
                client: None,
                starting: None,
            },
        );
    }
    let mut for_instance = launch.clone();
    for_instance.instance = Some(cfg.id);
    for_instance.resume = true;
    for_instance.invite = invite;
    match crate::open_main_window_with(cx, for_instance) {
        Ok(window) => {
            if let Some(live) = Registry::of(cx).live.get_mut(&cfg.id.0) {
                live.window = Some(window);
            }
            Ok(())
        }
        Err(e) => {
            // No window, no client: let go of the hold again.
            let registry = Registry::of(cx);
            if registry
                .live
                .get(&cfg.id.0)
                .is_some_and(|l| l.client.is_none())
            {
                registry.live.remove(&cfg.id.0);
            }
            Err(OpenError::Window(e.to_string()))
        }
    }
}

/// Closes instance `id`'s window and drops its client. Its hold stays (retired) until the client has really stopped:
/// [`stopped`] says when. What removing an instance needs first.
pub fn release_instance(cx: &mut App, id: InstanceId) {
    let Some(live) = Registry::of(cx).live.remove(&id.0) else {
        return;
    };
    let Live {
        guard,
        window,
        client,
        starting,
    } = live;
    let alive: Weak<dyn Any> = match (&client, starting) {
        (Some(c), _) => {
            let any: Rc<dyn Any> = c.clone();
            Rc::downgrade(&any)
        }
        (None, Some(token)) => token,
        (None, None) => Weak::<()>::new(),
    };
    retire_hold(cx, id, guard, alive);
    let close = move |cx: &mut App| {
        if let Some(window) = window {
            window
                .update(cx, |_, window, _| window.remove_window())
                .ok();
        }
    };
    let Some(client) = client else {
        close(cx);
        return;
    };
    // Re-review N1: a call this device is in is left first, as Forget and Sign out do (its self-Remove goes to the
    // instance, so the others see the device leave at once), for at most LEAVE_WAIT. Then final review C1: the full
    // release path. The client stops syncing (its key is already erased), every app-wide area lets go of it and clears
    // what it decrypted, and the window goes, so its last handle goes with the window and the removal can delete its
    // data.
    let mut leave = client.voice().leave();
    cx.spawn(async move |cx| {
        let polls = LEAVE_WAIT.as_millis() / LEAVE_POLL.as_millis();
        for _ in 0..polls {
            match leave.try_recv() {
                Err(tokio::sync::oneshot::error::TryRecvError::Empty) => {
                    cx.background_executor().timer(LEAVE_POLL).await;
                }
                _ => break,
            }
        }
        cx.update(|cx| {
            client.halt();
            crate::release::release_client(&client, cx);
            close(cx);
        });
        // The client stops when the last reference goes: this one (here), then the shell view's with its window.
        drop(client);
    })
    .detach();
}

/// How long a removed instance's call leave may take before its client is stopped anyway, and how often it is asked.
const LEAVE_WAIT: std::time::Duration = std::time::Duration::from_secs(10);
const LEAVE_POLL: std::time::Duration = std::time::Duration::from_millis(50);

/// [`release_instance`] with the argument order of the manager's host closures.
pub fn release_instance_in(id: InstanceId, cx: &mut App) {
    release_instance(cx, id);
}

/// Forgets every window and client, for a clean quit.
pub fn release_all(cx: &mut App) {
    let ids: Vec<u8> = Registry::of(cx).live.keys().copied().collect();
    for id in ids {
        release_instance(cx, InstanceId(id));
    }
}

/// What the areas keep per window, let go the moment `id` closes (the close hook's fan-out): its voice area leaves the
/// window's call at once (media cut, Idle published, the Remove sent) and drops its media session; its threads area
/// drops the pane and the view it held; the create flows, the factor area (step-ups, the Sign-in section) and the
/// attachments area (the composer target, the lightbox, the GIF picker and notice) drop the window's client.
pub fn on_closed(cx: &mut App, id: WindowId) {
    crate::screens::voice::window_closed(id, cx);
    crate::screens::threads::window_closed(id, cx);
    crate::screens::ring::window_closed(id, cx);
    crate::screens::create::window_closed(id, cx);
    crate::screens::factor::window_closed(id, cx);
    crate::screens::attachments::ui::window_closed(id, cx);
    crate::per_window::window_closed(id, cx);
}

fn install_close_hook(cx: &mut App, launch: Launch, allow_loopback: bool) {
    let registry = Registry::of(cx);
    if registry.closing_hook {
        return;
    }
    registry.closing_hook = true;
    cx.on_window_closed(move |cx, id| {
        on_closed(cx, id);
        let closed = note_closed(cx, id);
        if cx.windows().is_empty() {
            // The last window of a manager closes the app; the last window of an instance falls back to the manager
            // when its client keeps running.
            match closed {
                Closed::Instance(_) => after_window_closed(cx, &launch, allow_loopback),
                _ => {
                    release_all(cx);
                    cx.quit();
                }
            }
        }
    })
    .detach();
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Re-review C: a deferred wipe of an instance deletes nothing once the instance started again (a new session on
    /// the same data dir); while it is pending, a sign-in for that instance is refused.
    #[gpui::test]
    fn a_deferred_wipe_never_deletes_a_new_session(cx: &mut gpui::TestAppContext) {
        let id = InstanceId(3);
        cx.update(|cx| {
            let generation = wipe_begin(cx, id);
            assert!(wipe_pending(cx, id), "pending: the sign-in is refused");
            assert!(
                wipe_may_delete(cx, id, generation),
                "nothing started meanwhile"
            );
            note_start(cx, id);
            assert!(
                !wipe_may_delete(cx, id, generation),
                "the instance started again: the old wipe deletes nothing"
            );
            wipe_end(cx, id);
            assert!(!wipe_pending(cx, id));
        });
    }

    /// Final review M6: two wipes of one instance pending at once keep it pending until both ended.
    #[gpui::test]
    fn two_pending_wipes_keep_the_instance_pending_until_both_end(cx: &mut gpui::TestAppContext) {
        let id = InstanceId(4);
        cx.update(|cx| {
            wipe_begin(cx, id);
            wipe_begin(cx, id);
            wipe_end(cx, id);
            assert!(wipe_pending(cx, id), "one wipe still waits");
            wipe_end(cx, id);
            assert!(!wipe_pending(cx, id));
            wipe_end(cx, id);
            assert!(!wipe_pending(cx, id), "an extra end never goes below none");
        });
    }
}
