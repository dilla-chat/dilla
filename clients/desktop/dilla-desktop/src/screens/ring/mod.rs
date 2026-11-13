//! Area **ring** (Task 18): the ring banner, DM calls, blocked calls and the AFK notice.
//!
//! Ports packages/web/src/screens/voice/Ring.tsx (`RingFlow`), the DM call and block parts of VoiceView.tsx
//! (`stageNotice`, `dmCallStart`) and its AFK page (`afkTitle`, `afkExplainer`).
//!
//! - [`ring_banner`] (`slots::overlays`): the banner of incoming rings, read from the window's [`RingEnv`]. Answer joins
//!   the ring's DM call through the ring area's gate (`Client::ring().join`) and hands the join to the voice area
//!   ([`effects::hand_to_voice`]); Decline posts the decline. Enter answers and Escape declines the ring with focus; when a
//!   ring arrives while the window is focused, focus moves to it (and never otherwise).
//! - [`block_notice`]: what the voice stage (Task 17) says about a blocked call, and the comparison it offers.
//! - [`afk_page`]: the AFK channel's page body; [`afk_moved`] the move's line for the stage of the channel left.
//! - [`dm_call_start`]: the DM header's call button (start, or join a live call), or nothing while a block holds it.

pub mod effects;
pub mod keys;

use std::collections::{HashMap, HashSet};
use std::rc::Rc;

use dilla_client::state::ring::{BlockWhen, CallBlock, IncomingRing, RingState};
use dilla_client::state::{AppState, ChannelId, CommunityId, UserId};
use dilla_desktop_ui::components::ring::{AfkPage, OnRing, RingBanner, RingFocus, RingRow};
use gpui::{AnyElement, App, AppContext, Global, IntoElement, SharedString, Window, WindowId};

use crate::slots::SlotCx;
use crate::strings::{t, tf};

/// What a window's ring banner reads: its slot context (the state and the client), set by the host on each render.
#[derive(Clone)]
pub struct RingEnv {
    pub ctx: SlotCx,
}

/// Each window's environment and memory: a ring banner belongs to the window (an instance) that drew it.
#[derive(Default)]
struct Windows(Vec<(WindowId, RingEnv, Memory)>);

impl Global for Windows {}

/// What the banner keeps across frames: each ring's focus handles, and the rings already seen (a ring moves focus once,
/// on arrival).
#[derive(Default)]
struct Memory {
    focus: HashMap<SharedString, RingFocus>,
    seen: HashSet<SharedString>,
}

/// The host's current view for the ring banner, set on each render of the host (the shell, Task 8).
pub fn provide(env: RingEnv, window: &Window, cx: &mut App) {
    let id = window.window_handle().window_id();
    let windows = cx.default_global::<Windows>();
    match windows.0.iter_mut().find(|(w, _, _)| *w == id) {
        Some((_, e, _)) => *e = env,
        None => windows.0.push((id, env, Memory::default())),
    }
}

/// A ring's key: its call and the instance's expires_ts (Ring.tsx `key`).
pub fn ring_key(r: &IncomingRing) -> SharedString {
    SharedString::from(format!("{}:{}", hex(&r.call_id), r.ring_ts))
}

fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

/// The ring's line: `call.ring.busy` while this device is in voice (naming where), else `call.ring.incoming`.
pub fn ring_text(r: &IncomingRing, in_call: Option<&str>) -> String {
    match in_call {
        Some(place) if r.busy => tf(
            "call.ring.busy",
            &[("name", &r.caller_name), ("where", place)],
        ),
        _ => tf("call.ring.incoming", &[("name", &r.caller_name)]),
    }
}

/// The ring banner (`slots::overlays`): `None` without a ring. Ring.tsx's `RingFlow`.
pub fn ring_banner(window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    let id = window.window_handle().window_id();
    let env = cx
        .try_global::<Windows>()?
        .0
        .iter()
        .find(|(w, _, _)| *w == id)
        .map(|(_, e, _)| e.clone())?;
    let state = env.ctx.state.clone();
    effects::sync(&env.ctx, window, cx);
    let ring: &RingState = &state.ring;
    let keys: Vec<SharedString> = ring.rings.iter().map(ring_key).collect();
    // Each ring's focus handles, made once; those of rings gone are dropped.
    let mut fresh = Vec::new();
    for k in &keys {
        let exists = cx
            .global::<Windows>()
            .0
            .iter()
            .any(|(w, _, m)| *w == id && m.focus.contains_key(k));
        if !exists {
            fresh.push((k.clone(), RingFocus::new(cx)));
        }
    }
    let (rows_focus, arrived) = {
        let memory = &mut cx
            .global_mut::<Windows>()
            .0
            .iter_mut()
            .find(|(w, _, _)| *w == id)?
            .2;
        memory.focus.extend(fresh);
        memory.focus.retain(|k, _| keys.contains(k));
        let arrived: Vec<SharedString> = keys
            .iter()
            .filter(|k| !memory.seen.contains(*k))
            .cloned()
            .collect();
        memory.seen.retain(|k| keys.contains(k));
        memory.seen.extend(arrived.iter().cloned());
        let focus: Vec<RingFocus> = keys.iter().map(|k| memory.focus[k].clone()).collect();
        (focus, arrived)
    };
    if keys.is_empty() {
        return None;
    }
    // A ring that arrives while the window is focused takes the focus (Enter answers, Escape declines); a ring never
    // takes it from another window.
    if let Some(first) = arrived.first()
        && window.is_window_active()
        && let Some(i) = keys.iter().position(|k| k == first)
    {
        window.focus(&rows_focus[i].row, cx);
    }
    let rows = ring
        .rings
        .iter()
        .zip(rows_focus)
        .map(|(r, focus)| RingRow {
            key: ring_key(r),
            name: r.caller_name.clone().into(),
            text: ring_text(r, ring.in_call.as_deref()).into(),
            busy: r.busy,
            focus,
        })
        .collect();
    let on_answer: OnRing = {
        let env = env.clone();
        Rc::new(move |key, window, cx| answer(&env, key, window, cx))
    };
    let on_decline: OnRing = Rc::new(move |key, window, cx| decline(&env, key, window, cx));
    Some(
        RingBanner::new(
            "ring-banner",
            rows,
            t("call.ring.answer"),
            t("call.ring.decline"),
            on_answer,
            on_decline,
        )
        .into_any_element(),
    )
}

fn find<'a>(state: &'a AppState, key: &SharedString) -> Option<&'a IncomingRing> {
    state.ring.rings.iter().find(|r| ring_key(r) == *key)
}

/// Answer: the ring's DM call, joined through the ring area's gate; the voice area takes the join.
fn answer(env: &RingEnv, key: &SharedString, window: &mut Window, cx: &mut App) {
    let Some(client) = env.ctx.client.clone() else {
        return;
    };
    let Some(ring) = find(&env.ctx.state, key) else {
        return;
    };
    // One call per device: a call another instance window is in is left first.
    crate::screens::voice::leave_other_calls(crate::screens::voice::wid(window), cx);
    let joining = client.ring().join(ring.channel, None, String::new());
    effects::hand_to_voice(client, ring.channel, joining, window, cx);
}

/// Decline: only the ring this decline answers (its call and expires_ts); the ring goes here at once.
fn decline(env: &RingEnv, key: &SharedString, _window: &mut Window, cx: &mut App) {
    let Some(client) = env.ctx.client.clone() else {
        return;
    };
    let Some(ring) = find(&env.ctx.state, key) else {
        return;
    };
    let answered = client.ring().decline(ring.call_id, ring.ring_ts);
    cx.background_spawn(async move {
        let _ = answered.await;
    })
    .detach();
}

// ---- the blocked call (VoiceView.tsx `stageNotice`, `joinHeld`) ----

/// What the stage says about a blocked call, and the comparison it offers (the changed contact).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct BlockNotice {
    pub text: String,
    /// `voice.pausedAction` ("compare safety numbers") for the named contact.
    pub compare: Option<UserId>,
    /// Focus moves to the notice once, when this text first shows.
    pub focus_once: bool,
}

/// True while a block holds the join of `channel` back (flow 06: no join button until the cause is gone). A block of a
/// join refused before anything was sent holds until the ring area clears it (its DM's text group or its changed contact
/// cleared, `Ring::refresh_blocks`); a block that only made this device leave a call offers the join again (product walk
/// M10).
pub fn join_held(state: &AppState, channel: ChannelId) -> bool {
    state
        .ring
        .blocked
        .as_ref()
        .is_some_and(|b| b.channel == channel && b.when == BlockWhen::BeforeJoin)
}

/// `stageNotice`'s blocked case for `channel`: `name_of` names the changed contact.
pub fn block_notice(
    state: &AppState,
    channel: ChannelId,
    name_of: impl Fn(UserId) -> String,
) -> Option<BlockNotice> {
    let block: &CallBlock = state
        .ring
        .blocked
        .as_ref()
        .filter(|b| b.channel == channel)?;
    let held = join_held(state, channel);
    let changed = if block.code == "E_UMK_CHANGED" {
        block.changed_user
    } else {
        None
    };
    let mid_call = block.when == BlockWhen::Left;
    let text = if mid_call {
        t(if held {
            "voice.blocked"
        } else {
            "voice.blockedLeft"
        })
        .to_owned()
    } else if let Some(user) = changed {
        tf("voice.blockedChanged", &[("name", &name_of(user))])
    } else {
        t(if held {
            "voice.blockedJoin"
        } else {
            "voice.blockedRetry"
        })
        .to_owned()
    };
    Some(BlockNotice {
        text,
        compare: if mid_call { None } else { changed },
        focus_once: true,
    })
}

/// `dmCallStart`: the DM header's call button (`call.start`, or `call.join` while a call is live), or `None` while this
/// device is in that call or a block holds the join.
pub fn dm_call_start(
    state: &AppState,
    channel: ChannelId,
    in_this_call: bool,
    live: bool,
) -> Option<&'static str> {
    if in_this_call || join_held(state, channel) {
        return None;
    }
    Some(if live {
        t("call.join")
    } else {
        t("call.start")
    })
}

// ---- AFK (VoiceView.tsx `afkTitle`, `afkExplainer`; afk.ts) ----

/// `afkTitle`: who is away, as the AFK page's title says it.
pub fn afk_title(away: bool, names: &[String]) -> String {
    if away {
        return t("afk.title.you").to_owned();
    }
    match names {
        [] => t("afk.title.nobody").to_owned(),
        [one] => tf("afk.title.one", &[("name", one)]),
        many => tf(
            "afk.title.many",
            &[("names", &many.join(t("callStage.preview.and")))],
        ),
    }
}

/// `afkExplainer`: the server's timeout in minutes (`Math.round`), or the line for no moves.
pub fn afk_explainer(timeout_seconds: u64) -> String {
    if timeout_seconds == 0 {
        return t("afk.explain.off").to_owned();
    }
    let minutes = (timeout_seconds + 30) / 60;
    if minutes == 1 {
        t("afk.explain.one").to_owned()
    } else {
        tf("afk.explain", &[("minutes", &minutes.to_string())])
    }
}

/// The AFK page's Come back: the voice area's way out of the AFK channel.
pub type OnComeBack = Rc<dyn Fn(&mut Window, &mut App)>;

/// The AFK page of `community`'s built-in AFK channel: the title, the explainer, and Go AFK or Come back. `None` while
/// the community's AFK snapshot is not read (the page asks for it). `come_back` is the voice area's way out (it opens a
/// text channel this person can send in, Jonas Q3).
pub fn afk_page(ctx: &SlotCx, community: CommunityId, come_back: OnComeBack) -> Option<AnyElement> {
    let state = &ctx.state;
    let Some(snapshot) = state.ring.afk.get(&community) else {
        if let Some(client) = ctx.client.clone() {
            drop(client.ring().load_afk(community));
        }
        return None;
    };
    let me = state.account.user_id;
    let away = me.is_some_and(|u| state.ring.is_away(community, u));
    let names: Vec<String> = snapshot
        .away
        .iter()
        .filter(|a| Some(a.user) != me)
        .map(|a| dilla_client::features::ring::name_of(state, a.user))
        .collect();
    let client = ctx.client.clone();
    let page = AfkPage::new(
        "afk-page",
        afk_title(away, &names),
        afk_explainer(snapshot.timeout_seconds),
        t(if away { "afk.comeBack" } else { "afk.go" }),
        move |_, window, cx| {
            if away {
                come_back(window, cx);
            } else if let Some(client) = client.clone() {
                let answered = client.ring().go_afk(community);
                cx.background_spawn(async move {
                    let _ = answered.await;
                })
                .detach();
            }
        },
    );
    Some(page.into_any_element())
}

/// R-AFK4: the line the stage of the channel left shows once after an AFK move ("Moved to {channel} for inactivity",
/// the choice on another device, or a newer device's takeover), with `channel_name` naming the AFK channel.
pub fn afk_moved(state: &AppState, channel_name: impl Fn(ChannelId) -> String) -> Option<String> {
    let notice = state.ring.afk_notice.as_ref()?;
    Some(match notice.reason {
        2 => t("afk.elsewhere").to_owned(),
        3 => t("afk.takenOver").to_owned(),
        _ => tf(
            "afk.moved",
            &[(
                "channel",
                &notice.channel.map(&channel_name).unwrap_or_default(),
            )],
        ),
    })
}
