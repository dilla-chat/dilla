//! What a ring does outside the banner: its sound (Task 19's synth), its OS notification (Task 22's notifier) and the
//! hand-over of an answered call to the voice area (Task 17). Each is one function here, so the wiring of an area that
//! merges later is one line in it (listed in Task 18's report).
//!
//! - The ring sound plays while any ring is neither busy nor quiet (useSounds.tsx: no sound while in a call or for a muted
//!   DM), and stops when the last such ring ends.
//! - A ring raises one notification while this window is not focused (flow 06: Ring.tsx `useRingNotifications`), closed
//!   when the ring ends or the window is focused again; a busy or quiet ring raises none.

use std::collections::HashSet;

use std::rc::Rc;

use dilla_client::features::notify::{
    self, Notifier, NotifyContext, Presence, RecordingNotifier, RingInput,
};
use dilla_client::features::ring::GatedJoin;
use dilla_client::state::ChannelId;
use dilla_client::state::ring::{CallBlock, IncomingRing};
use dilla_client::{Client, ClientError};
use gpui::{App, Global, SharedString, Window, WindowId};
use tokio::sync::oneshot;

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
    let rings = &ctx.state.ring.rings;
    let want_sound = rings.iter().any(audible);
    let visible = window.is_window_active();
    let keys: HashSet<SharedString> = rings.iter().map(ring_key).collect();
    let effects = cx.default_global::<Effects>();
    let at = match effects.0.iter().position(|(w, _, _)| *w == id) {
        Some(i) => i,
        None => {
            effects.0.push((id, false, HashSet::new()));
            effects.0.len() - 1
        }
    };
    let (_, playing, noted) = &mut effects.0[at];
    let start = want_sound && !*playing;
    let stop = !want_sound && *playing;
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
    if start {
        start_ring(cx);
    }
    if stop {
        stop_ring(cx);
    }
    for k in closed {
        close_ring_notification(&k, cx);
    }
    for r in raise {
        ring_notification(&r, ctx, cx);
    }
}

/// Task 19's ring sound starts (`screens::voice_settings::sounds::start_ring`). Until Task 19 merges this plays nothing;
/// the controller replaces the body with `crate::screens::voice_settings::sounds::start_ring(cx)` at merge.
pub fn start_ring(_cx: &mut App) {}

/// Task 19's ring sound stops (`screens::voice_settings::sounds::stop_ring`). Wired at merge as [`start_ring`] is.
pub fn stop_ring(_cx: &mut App) {}

/// The app's notifier (Task 22's in-app [`RecordingNotifier`]: the OS delivery is the next plan's), one per app.
#[derive(Default)]
pub struct RingNotifier(pub RecordingNotifier);

impl Global for RingNotifier {}

/// Raises the ring's notification through Task 22's notifier: `features::notify::ring_notification` decides (only while
/// the window is hidden, with the permission granted, never for a busy or quiet ring or a DM whose mode is nothing), and
/// the notifier shows it. Its click opens the DM (`Notification::channel`). Called only while the window is not focused.
pub fn ring_notification(ring: &IncomingRing, ctx: &SlotCx, cx: &mut App) {
    let Some(client) = &ctx.client else { return };
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
    if let Some(n) = notify::ring_notification(&input, &notify_ctx, &client.prefs(), &text) {
        cx.default_global::<RingNotifier>().0.deliver(&n);
    }
}

/// Closes the notification of the ring `key` (tag `dilla-ring-<call>-<ts>`). The in-app notifier keeps what it showed
/// (the e2e's record); a ring's next notification replaces it by its tag, so nothing is closed here until the OS delivery
/// (the next plan) can withdraw one.
pub fn close_ring_notification(_key: &SharedString, _cx: &mut App) {}

/// The answered ring's join, handed to the voice area (Task 17: `Voice::join_gated`), which follows it as this device's
/// call: the phases, the media, the dock and the stage. A block refused before anything was sent is already published
/// (`RingState::blocked`) for the stage's notice.
pub fn hand_to_voice(
    client: Rc<Client>,
    channel: ChannelId,
    joining: oneshot::Receiver<Result<Result<GatedJoin, CallBlock>, ClientError>>,
    _window: &mut Window,
    cx: &mut App,
) {
    cx.spawn(async move |_| {
        if let Ok(Ok(Ok(join))) = joining.await {
            let _ = client.voice().join_gated(channel, None, join).await;
        }
    })
    .detach();
}
