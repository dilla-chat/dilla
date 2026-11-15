//! What a ring does outside the banner: its sound (Task 19's synth), its OS notification (Task 22's notifier) and the
//! hand-over of an answered call to the voice area (Task 17). Each is one function here, so the wiring of an area that
//! merges later is one line in it (listed in Task 18's report).
//!
//! - The ring sound plays while any ring is neither busy nor quiet (useSounds.tsx: no sound while in a call or for a muted
//!   DM), and stops when the last such ring ends.
//! - A ring raises one notification while this window is not focused (flow 06: Ring.tsx `useRingNotifications`), closed
//!   when the ring ends or the window is focused again; a busy or quiet ring raises none. It goes to the OS
//!   (`native::notifier::show_ring`, with Answer and Decline) and to the in-app record ([`RingNotifier`]).
//! - Every frame also keeps the client's message notifications running (`native::notifier::attach`).

use std::collections::HashSet;

use dilla_client::features::notify::{
    self, Notifier, NotifyContext, Presence, RecordingNotifier, RingInput,
};
use dilla_client::state::ring::IncomingRing;
use gpui::{App, Global, SharedString, Window, WindowId};

use super::ring_key;
use crate::slots::SlotCx;

/// Per window: whether the ring sound plays, and the rings whose notification was raised (or decided against: a ring
/// that may not notify is remembered too, so a later frame never raises it).
#[derive(Default)]
struct Effects(Vec<(WindowId, bool, HashSet<SharedString>)>);

impl Global for Effects {}

/// A ring rings audibly: not while this device is in a call, not for a muted DM.
pub fn audible(r: &IncomingRing) -> bool {
    !r.busy && !r.quiet
}

/// Brings the sound and the notifications in line with the rings (called on every frame of the banner).
pub fn sync(ctx: &SlotCx, window: &mut Window, cx: &mut App) {
    let id = window.window_handle().window_id();
    crate::native::notifier::attach(ctx, window, cx);
    let rings = &ctx.state.ring.rings;
    let want_sound = rings.iter().any(audible);
    let visible = window.is_window_active();
    let keys: HashSet<SharedString> = rings.iter().map(ring_key).collect();
    let effects = cx.default_global::<Effects>();
    // One sound for every window that rings (review M2): it starts with the first and stops with the last.
    let before = effects.0.iter().any(|(_, p, _)| *p);
    let at = match effects.0.iter().position(|(w, _, _)| *w == id) {
        Some(i) => i,
        None => {
            effects.0.push((id, false, HashSet::new()));
            effects.0.len() - 1
        }
    };
    let (_, playing, noted) = &mut effects.0[at];
    *playing = want_sound;
    // Notifications: those of rings gone (or all, once the window is focused again) are closed; each new ring is decided
    // once.
    let closed: Vec<SharedString> = noted
        .iter()
        .filter(|k| visible || !keys.contains(*k))
        .cloned()
        .collect();
    for k in &closed {
        noted.remove(k);
    }
    let mut raise = Vec::new();
    if !visible {
        for r in rings {
            let k = ring_key(r);
            if noted.insert(k) {
                raise.push(r.clone());
            }
        }
    }
    let after = effects.0.iter().any(|(_, p, _)| *p);
    let (start, stop) = (after && !before, before && !after);
    if start {
        start_ring(cx);
    }
    if stop {
        stop_ring(cx);
    }
    for k in closed {
        close_ring_notification(id, &k, cx);
    }
    for r in raise {
        ring_notification(id, &r, ctx, cx);
    }
}

/// Whether a ring sound plays now (some window rings audibly).
pub fn sound_on(cx: &mut App) -> bool {
    cx.try_global::<Effects>()
        .is_some_and(|e| e.0.iter().any(|(_, p, _)| *p))
}

/// `window` no longer rings (it closed, or its client was released): its part goes at once, and the sound stops if no
/// other window rings (review M2: it played on for up to 15 cycles, and one window's stop silenced another's ring).
pub fn window_gone(window: WindowId, cx: &mut App) {
    if !cx.has_global::<Effects>() {
        return;
    }
    let effects = cx.global_mut::<Effects>();
    let before = effects.0.iter().any(|(_, p, _)| *p);
    effects.0.retain(|(w, _, _)| *w != window);
    let after = effects.0.iter().any(|(_, p, _)| *p);
    if before && !after {
        stop_ring(cx);
    }
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
/// App-wide by design (final review sweep): the OS has one notification centre per app, and a ring notification names the window it
/// rings in, not a client handle.
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
/// the window is hidden, with the permission granted, never for a busy or quiet ring or a DM whose mode is nothing), and
/// the OS shows it with Answer and Decline (`native::notifier::show_ring`). Its click opens the DM
/// (`Notification::channel`). Called only while the window is not focused. A window with no client (a fixture's) reads
/// no stored settings.
pub fn ring_notification(window: WindowId, ring: &IncomingRing, ctx: &SlotCx, cx: &mut App) {
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
    let notify_ctx = NotifyContext {
        permission: ctx.state.notify.permission,
        // The slice holds no presence choice of its own yet (the account has none here): online.
        presence: Presence::Online,
        visible: false,
        on_screen: ctx.channel,
        me: ctx.state.account.user_id,
        roles: None,
    };
    let text = crate::screens::notify::text();
    let decided = match &ctx.client {
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
        crate::native::notifier::show_ring(window, super::ring_key(ring), &n, cx);
    }
}

/// Closes the notification of the ring `key` raised in `window`: the OS one is withdrawn; the in-app record keeps what
/// it showed (the e2e's record).
pub fn close_ring_notification(window: WindowId, key: &SharedString, cx: &mut App) {
    crate::native::notifier::withdraw_ring(window, key, cx);
}
