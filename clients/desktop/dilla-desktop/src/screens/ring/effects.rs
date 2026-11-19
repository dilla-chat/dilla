//! What a ring does outside the banner: its sound (Task 19's synth), its OS notification (Task 22's notifier) and the
//! hand-over of an answered call to the voice area (Task 17). Each is one function here, so the wiring of an area that
//! merges later is one line in it (listed in Task 18's report).
//!
//! Sound and notifications belong to the instance, not to a window (GPUI gaps Task 3: the Electron app rang with every
//! window closed to the tray). [`watch_instance`] observes a running client's state from the app, window or not
//! (`windows::keep_client` starts it), and [`sync_instance`] decides:
//!
//! - The ring sound plays while any ring of any instance is neither busy nor quiet (useSounds.tsx: no sound while in a
//!   call or for a muted DM): one sound for the app, started with the first such ring and stopped with the last.
//! - A ring raises one notification while no window of its instance is focused (flow 06: Ring.tsx
//!   `useRingNotifications`), closed when the ring ends or a window of the instance is focused again; a busy or quiet
//!   ring raises none. It goes to the OS (`native::notifier::show_ring`, with Answer and Decline) and to the in-app
//!   record ([`RingNotifier`]).
//! - Answer from a notification of an instance with no window opens the window ([`await_window`]); the window's first
//!   frame that shows the ring answers it.
//!
//! The banner's frame ([`sync`]) keeps what needs a window: it reports the window's focus (a focus change decides the
//! notifications again), tells the client's message notifications what the window shows (`native::notifier::attach`;
//! the engine itself starts with the client) and keeps its unread badge running (`native::badge::attach`).

use std::collections::HashSet;
use std::sync::Arc;

use dilla_client::features::notify::{
    self, Notifier, NotifyContext, Presence, RecordingNotifier, RingInput,
};
use dilla_client::seams::InstanceId;
use dilla_client::state::AppState;
use dilla_client::state::ring::IncomingRing;
use gpui::{App, Global, SharedString, Task, Window, WindowId};
use tokio::sync::watch;

use super::ring_key;
use crate::native::NativeShell;
use crate::native::notifier::Owner;
use crate::slots::SlotCx;

/// One running instance's rings, as last observed.
struct Ringing {
    instance: InstanceId,
    rings: Vec<IncomingRing>,
    /// Some ring of it rings audibly.
    playing: bool,
    /// A window of it is focused.
    active: bool,
    /// The rings whose notification was raised (or decided against: a ring that may not notify is remembered too, so a
    /// later state never raises it).
    noted: HashSet<SharedString>,
}

/// The app-level observer of one client's state.
struct Watch {
    client: usize,
    instance: InstanceId,
    _task: Task<()>,
}

#[derive(Default)]
struct Effects {
    instances: Vec<Ringing>,
    watches: Vec<Watch>,
    /// Answers waiting for their instance's window to open (Answer on a notification raised with no window).
    pending: Vec<(InstanceId, SharedString)>,
}

impl Global for Effects {}

/// A ring rings audibly: not while this device is in a call, not for a muted DM.
pub fn audible(r: &IncomingRing) -> bool {
    !r.busy && !r.quiet
}

/// Every frame of a window's ring banner: keeps the message engine and the badge running, and tells the instance's
/// rings whether this window is focused (a focus change decides the notifications again).
pub fn sync(ctx: &SlotCx, window: &mut Window, cx: &mut App) {
    crate::native::notifier::attach(ctx, window, cx);
    crate::native::badge::attach(ctx, window, cx);
    let id = window.window_handle().window_id();
    if let Owner::Instance(instance) = Owner::of_window(id, cx) {
        window_focus(instance, window.is_window_active(), cx);
    }
}

/// A window of `instance` is (or is no longer) focused: its rings are decided again when that changed.
fn window_focus(instance: InstanceId, active: bool, cx: &mut App) {
    let Some(rings) = cx.try_global::<Effects>().and_then(|e| {
        e.instances
            .iter()
            .find(|r| r.instance == instance && r.active != active)
            .map(|r| r.rings.clone())
    }) else {
        return;
    };
    sync_instance(instance, &rings, active, cx);
}

/// Whether a window of `instance` is focused (read from the app: no frame is needed).
fn any_window_active(instance: InstanceId, cx: &mut App) -> bool {
    let ids: Vec<WindowId> = NativeShell::open_windows(cx)
        .into_iter()
        .filter(|(_, i)| *i == Some(instance))
        .map(|(w, _)| w)
        .collect();
    cx.windows()
        .into_iter()
        .filter(|h| ids.contains(&h.window_id()))
        .any(|h| {
            h.update(cx, |_, w, _| w.is_window_active())
                .unwrap_or(false)
        })
}

/// Brings the sound and the notifications of `instance` in line with its rings: called by the app-level observer of
/// its client's state ([`watch_instance`]) on every change, and again when a window of it gains or loses the focus or
/// closes.
pub fn sync_instance(
    instance: InstanceId,
    rings: &[IncomingRing],
    any_window_active: bool,
    cx: &mut App,
) {
    let keys: HashSet<SharedString> = rings.iter().map(ring_key).collect();
    let effects = cx.default_global::<Effects>();
    let before = effects.instances.iter().any(|r| r.playing);
    let at = match effects
        .instances
        .iter()
        .position(|r| r.instance == instance)
    {
        Some(i) => i,
        None => {
            effects.instances.push(Ringing {
                instance,
                rings: Vec::new(),
                playing: false,
                active: false,
                noted: HashSet::new(),
            });
            effects.instances.len() - 1
        }
    };
    let entry = &mut effects.instances[at];
    entry.rings = rings.to_vec();
    entry.active = any_window_active;
    entry.playing = rings.iter().any(audible);
    // Notifications: those of rings gone (or all, once a window is focused) are closed; each new ring is decided once.
    let closed: Vec<SharedString> = entry
        .noted
        .iter()
        .filter(|k| any_window_active || !keys.contains(*k))
        .cloned()
        .collect();
    for k in &closed {
        entry.noted.remove(k);
    }
    let mut raise = Vec::new();
    if !any_window_active {
        for r in rings {
            if entry.noted.insert(ring_key(r)) {
                raise.push(r.clone());
            }
        }
    }
    // An answer that waits for a window goes with its ring.
    effects
        .pending
        .retain(|(i, k)| *i != instance || keys.contains(k));
    let after = effects.instances.iter().any(|r| r.playing);
    if after && !before {
        start_ring(cx);
    }
    if before && !after {
        stop_ring(cx);
    }
    for k in closed {
        close_ring_notification(instance, &k, cx);
    }
    for r in raise {
        ring_notification(instance, &r, cx);
    }
}

/// Decides `instance`'s rings again with what was observed last (a window of it closed).
pub fn resync(instance: InstanceId, cx: &mut App) {
    let Some(rings) = cx.try_global::<Effects>().and_then(|e| {
        e.instances
            .iter()
            .find(|r| r.instance == instance)
            .map(|r| r.rings.clone())
    }) else {
        return;
    };
    let active = any_window_active(instance, cx);
    sync_instance(instance, &rings, active, cx);
}

/// Starts the app-level observer of `instance`'s client (`client`: its `Rc::as_ptr`; `state`: `Client::state`): every
/// change of the state decides its rings ([`sync_instance`]), with or without a window. Once per client; a new client
/// of the instance (a sign-in again) replaces the old one's observer.
pub fn watch_instance(
    instance: InstanceId,
    client: usize,
    mut state: watch::Receiver<Arc<AppState>>,
    cx: &mut App,
) {
    let effects = cx.default_global::<Effects>();
    if effects.watches.iter().any(|w| w.client == client) {
        return;
    }
    let old: Vec<usize> = effects
        .watches
        .iter()
        .filter(|w| w.instance == instance)
        .map(|w| w.client)
        .collect();
    for c in old {
        unwatch_client(c, cx);
    }
    let task = cx.spawn(async move |cx| {
        loop {
            let rings = state.borrow_and_update().ring.rings.clone();
            cx.update(|cx| {
                let active = any_window_active(instance, cx);
                sync_instance(instance, &rings, active, cx);
            });
            if state.changed().await.is_err() {
                break;
            }
        }
        // The client is gone: so are its rings, and this observer's entry (review of Task 3, minor 1: a later client
        // allocated at the same address has the same key, and a stale entry would keep it from being observed).
        cx.update(|cx| {
            let watches = &mut cx.default_global::<Effects>().watches;
            if let Some(at) = watches
                .iter()
                .position(|w| w.client == client && w.instance == instance)
            {
                // This very task: it finishes on its own.
                watches.remove(at)._task.detach();
            }
            forget_instance(instance, cx);
        });
    });
    cx.default_global::<Effects>().watches.push(Watch {
        client,
        instance,
        _task: task,
    });
}

/// Lets go of the client `client` (`Rc::as_ptr`; Forget, sign-out, a removed instance): its observer stops, its rings'
/// notifications are withdrawn, and the sound stops if no other instance rings.
pub fn unwatch_client(client: usize, cx: &mut App) {
    if !cx.has_global::<Effects>() {
        return;
    }
    let effects = cx.global_mut::<Effects>();
    let gone: Vec<InstanceId> = effects
        .watches
        .iter()
        .filter(|w| w.client == client)
        .map(|w| w.instance)
        .collect();
    effects.watches.retain(|w| w.client != client);
    for id in gone {
        forget_instance(id, cx);
    }
}

fn forget_instance(instance: InstanceId, cx: &mut App) {
    sync_instance(instance, &[], true, cx);
    let effects = cx.global_mut::<Effects>();
    effects.instances.retain(|r| r.instance != instance);
    effects.pending.retain(|(i, _)| *i != instance);
}

/// The client (its `Rc::as_ptr`) whose state `instance`'s ring observer follows, if one does.
pub fn watched(instance: InstanceId, cx: &App) -> Option<usize> {
    cx.try_global::<Effects>()
        .and_then(|e| e.watches.iter().find(|w| w.instance == instance))
        .map(|w| w.client)
}

/// Whether a ring sound plays now (some instance rings audibly).
pub fn sound_on(cx: &mut App) -> bool {
    cx.try_global::<Effects>()
        .is_some_and(|e| e.instances.iter().any(|r| r.playing))
}

/// `instance`'s rings as its observer saw them last.
pub fn rings_of(instance: InstanceId, cx: &App) -> Vec<IncomingRing> {
    cx.try_global::<Effects>()
        .and_then(|e| e.instances.iter().find(|r| r.instance == instance))
        .map_or_else(Vec::new, |r| r.rings.clone())
}

/// Answer of the ring `key` waits for `instance`'s window to open (it had none): the window answers it on its first
/// frame that shows the ring ([`take_answer`]). It goes when the ring ends.
pub fn await_window(instance: InstanceId, key: SharedString, cx: &mut App) {
    let pending = &mut cx.default_global::<Effects>().pending;
    pending.retain(|(i, _)| *i != instance);
    pending.push((instance, key));
}

/// The answer waiting for `instance`'s window goes: the window did not open (review of Task 3, minor 2), so no window
/// opened later for another reason answers a call by itself.
pub fn drop_answer(instance: InstanceId, cx: &mut App) {
    if cx.has_global::<Effects>() {
        cx.global_mut::<Effects>()
            .pending
            .retain(|(i, _)| *i != instance);
    }
}

/// Whether an answer waits for `instance`'s window.
pub fn answer_pending(instance: InstanceId, cx: &App) -> bool {
    cx.try_global::<Effects>()
        .is_some_and(|e| e.pending.iter().any(|(i, _)| *i == instance))
}

/// The answer waiting for the instance of `window`, when the ring is among `rings` (what the window shows): taken.
pub fn take_answer(window: WindowId, rings: &[IncomingRing], cx: &mut App) -> Option<SharedString> {
    let Owner::Instance(instance) = Owner::of_window(window, cx) else {
        return None;
    };
    if !cx.has_global::<Effects>() {
        return None;
    }
    let pending = &mut cx.global_mut::<Effects>().pending;
    let at = pending
        .iter()
        .position(|(i, k)| *i == instance && rings.iter().any(|r| ring_key(r) == *k))?;
    Some(pending.remove(at).1)
}

/// Task 19's ring sound starts (`screens::voice_settings::sounds::start_ring`: two pings every 2 s, at most 15 times).
pub fn start_ring(cx: &mut App) {
    crate::screens::voice_settings::sounds::start_ring(cx);
}

/// Task 19's ring sound stops (`screens::voice_settings::sounds::stop_ring`).
pub fn stop_ring(cx: &mut App) {
    crate::screens::voice_settings::sounds::stop_ring(cx);
}

/// The app's in-app record of ring notifications (Task 22's [`RecordingNotifier`]), one per app; the OS shows them through
/// `native::notifier`.
/// App-wide by design (final review sweep): the OS has one notification centre per app, and a ring notification names the
/// instance it rings in, not a client handle.
#[derive(Default)]
pub struct RingNotifier(pub RecordingNotifier);

impl Global for RingNotifier {}

/// The release path: the notifier forgets what it showed (re-review minor; a notification names who rang and where).
pub fn release_notifier(cx: &mut App) {
    if let Some(n) = cx.try_global::<RingNotifier>() {
        n.0.clear();
    }
}

/// Raises the ring's notification through Task 22's notifier: `features::notify::ring_notification` decides (only while
/// no window of the instance is focused, with the permission granted, never for a busy or quiet ring or a DM whose mode
/// is nothing), and the OS shows it with Answer and Decline (`native::notifier::show_ring`). Its click opens the DM
/// (`Notification::channel`). The permission and the settings are the instance's running client's; an instance with no
/// client (a test's) reads none.
pub fn ring_notification(instance: InstanceId, ring: &IncomingRing, cx: &mut App) {
    let input = RingInput {
        channel: ring.channel,
        call_id: ring.call_id.iter().map(|b| format!("{b:02x}")).collect(),
        ring_ts: ring.ring_ts,
        caller_name: ring.caller_name.clone(),
        busy: ring.busy,
        quiet: ring.quiet,
        kind: 3,
        community: None,
    };
    let client = crate::windows::running_client(cx, instance);
    let (permission, me) = client.as_ref().map_or_else(
        || {
            let s = AppState::default();
            (s.notify.permission, s.account.user_id)
        },
        |c| {
            let s = c.state().borrow().clone();
            (s.notify.permission, s.account.user_id)
        },
    );
    let notify_ctx = NotifyContext {
        permission,
        // The slice holds no presence choice of its own yet (the account has none here): online.
        presence: Presence::Online,
        visible: false,
        on_screen: None,
        me,
        roles: None,
    };
    let text = crate::screens::notify::text();
    let decided = match &client {
        Some(client) => notify::ring_notification(&input, &notify_ctx, &client.prefs(), &text),
        None => notify::ring_notification(
            &input,
            &notify_ctx,
            &std::collections::HashMap::<String, String>::new(),
            &text,
        ),
    };
    if let Some(n) = decided {
        cx.default_global::<RingNotifier>().0.deliver(&n);
        crate::native::notifier::show_ring(instance, ring_key(ring), &n, cx);
    }
}

/// Closes the notification of `instance`'s ring `key`: the OS one is withdrawn; the in-app record keeps what it showed
/// (the e2e's record).
pub fn close_ring_notification(instance: InstanceId, key: &SharedString, cx: &mut App) {
    crate::native::notifier::withdraw_ring(instance, key, cx);
}
