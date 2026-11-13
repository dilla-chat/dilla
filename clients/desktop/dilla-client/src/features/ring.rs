//! Area **ring** (Task 18): ring, answer, decline, busy and timeout; DM call start; the blocked-call flows; the AFK move and its notice.
//!
//! Ports: packages/client-core/src/calls/{ring,sweep,voice}.ts, packages/client-core/src/admin/afk.ts, and the parts of
//! calls/controller.ts and worker/controller.ts that own rings (`rings`, `outgoing`, `decline`, `onSendBlocks`, the DM
//! join's decision 4) and the AFK snapshot (`readAFK`, `incomingAFKAway`, `afkAway`).
//!
//! Receives op 53 `call_ring` and op 55 `afk_away` from the gateway (`crate::sync::route`), through [`AreaFrames`]. The
//! frames are handled in arrival order on one task per client (a ring's check reads the core, which may take a moment,
//! and a state 0 must never overtake its state 1).
//!
//! What this area decides and publishes in [`RingState`]:
//! - [`RingBook`]: incoming rings (ring.ts `Rings`), each checked against the DM list this device holds, its caller,
//!   and the DM's send blocks (decision 4 extended to rings), for 30 s from its arrival here, one per channel, at most
//!   eight;
//! - [`Outgoing`]: the DM call this device opened, whom it rings, who declined (op 53 state 2);
//! - [`CallBlock`]: a call refused before anything of its join was sent ([`Ring::join`], decision 4 and RV-1), with the
//!   changed contact among the call's own participants;
//! - [`AfkSnapshot`] and [`AfkNotice`]: the AFK channel, its away roster (op 55) and the move a read announces.
//!
//! Seams to other areas (each a function of this file, so the wiring is one line at merge): the voice area (Task 17)
//! reports this device's call through [`Ring::set_in_call`] and its rosters through [`Ring::voice_roster`]; the DM
//! mute (Task 22) is read by [`dm_muted`].

use std::collections::{HashMap, HashSet};
use std::sync::{Arc, LazyLock, Mutex, OnceLock, PoisonError, Weak};
use std::time::Duration;

use tokio::sync::{mpsc, oneshot, watch};
use tokio::time::Instant;

use crate::calls::wire::decode_groups;
use crate::calls::{
    CallJoin, CallPhase, CallTarget, Calls, HttpRoutes, JoinRefused, Joined, NoHttp, decline_ring,
};
use crate::client::{Client, ClientError, Inner};
use crate::gateway::frames::{
    AfkAwayFrame, CallRingFrame, Frame, Op, Value, afk_away_of, call_ring_of,
};
use crate::http::routes::{GetAfk, ListDms, NoBody, PutAfkAway, Raw};
use crate::seams::Http;
use crate::state::ring::{
    AFK_AWAY_MAX, AfkNotice, AfkSnapshot, Away, BlockWhen, CallBlock, IncomingRing, Outgoing, RING,
    RINGS_MAX, RingState,
};
use crate::state::{AppState, ChannelId, CommunityId, UserId};
use crate::sync::api::try_core;
use crate::sync::requests::SocketHttp;
use crate::sync::route::AreaFrames;
use crate::sync::value::{DecodeError, at_least, bin16, decode, u53};

// ---- the ring book (ring.ts `Rings`) ----

/// What a ring's checks read (ring.ts `RingDeps`), answered for the ring's moment.
pub trait RingDeps {
    fn own_user(&self) -> Option<UserId>;
    /// The participants (the own user included) of a DM or group DM this device knows; `None` otherwise.
    fn dm_members(&self, channel: ChannelId) -> Option<Vec<UserId>>;
    /// The DM's text group, or its call group when this device holds it, is send-blocked (a core that cannot answer is
    /// blocked: fail closed).
    fn blocked(&self, channel: ChannelId) -> bool;
    /// This device is in a call (in, joining or paused): no sound, no notification.
    fn busy(&self) -> bool;
    /// The DM is muted: the banner only.
    fn quiet(&self, channel: ChannelId) -> bool;
    fn name_of(&self, user: UserId) -> String;
}

/// A ring's local end: at `at`, the ring of `channel` with `ring_ts` (and that deadline) goes, unless a state 0, a decline
/// or a newer ring took it first.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Expiry {
    pub channel: ChannelId,
    pub ring_ts: u64,
    pub at: Instant,
}

/// What a frame did to the book: whether the list changed, and the timer to arm for a new ring.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Step {
    pub changed: bool,
    pub arm: Option<Expiry>,
}

/// The incoming rings of one client (ring.ts `Rings`). Pure: the caller passes the moment and the checks' answers, and
/// arms each [`Step::arm`] it is handed.
#[derive(Clone, Debug, Default)]
pub struct RingBook {
    rings: Vec<IncomingRing>,
}

impl RingBook {
    pub fn list(&self) -> &[IncomingRing] {
        &self.rings
    }

    /// op 53 states 1 (ringing) and 0 (over). State 2 belongs to the caller's side ([`outgoing_declined`]).
    pub fn frame(&mut self, f: &CallRingFrame, now: Instant, deps: &dyn RingDeps) -> Step {
        let channel = ChannelId(f.channel_id);
        if f.state == 0 {
            let ours = self
                .rings
                .iter()
                .any(|r| r.channel == channel && r.call_id == f.call_id);
            return Step {
                changed: ours && self.remove(channel),
                arm: None,
            };
        }
        if f.state != 1 {
            return Step::default();
        }
        // Attacker: an instance ringing in the name of a channel this person does not share with the caller, or as
        // themself.
        let caller = UserId(f.user_id);
        let Some(members) = deps.dm_members(channel) else {
            return Step::default();
        };
        if deps.own_user() == Some(caller) || !members.contains(&caller) {
            return Step::default();
        }
        // Decision 4 extended to rings: a send-blocked DM gets no ring entry at all (an earlier one goes too).
        if deps.blocked(channel) {
            return Step {
                changed: self.remove(channel),
                arm: None,
            };
        }
        let existing = self.rings.iter().position(|r| r.channel == channel);
        if existing.is_none() && self.rings.len() >= RINGS_MAX {
            return Step::default();
        }
        // f10app m8: the same ring again (its call and expires_ts unchanged) keeps its first arrival's 30 s.
        if let Some(i) = existing
            && self.rings[i].call_id == f.call_id
            && self.rings[i].ring_ts == f.expires_ts
        {
            return Step::default();
        }
        // concern 5: expires_ts is a second on the instance's clock, never compared with this device's; the 30 s run
        // from the ring's arrival here. It stays the ring's identity for the decline.
        let ring = IncomingRing {
            call_id: f.call_id,
            channel,
            caller,
            caller_name: deps.name_of(caller),
            ring_ts: f.expires_ts,
            expires_at: now + RING,
            busy: deps.busy(),
            quiet: deps.quiet(channel),
        };
        let arm = Expiry {
            channel,
            ring_ts: ring.ring_ts,
            at: ring.expires_at,
        };
        match existing {
            Some(i) => self.rings[i] = ring,
            None => self.rings.push(ring),
        }
        Step {
            changed: true,
            arm: Some(arm),
        }
    }

    /// The ring of a channel ends (answered here, declined, or its call joined).
    pub fn remove(&mut self, channel: ChannelId) -> bool {
        let before = self.rings.len();
        self.rings.retain(|r| r.channel != channel);
        self.rings.len() != before
    }

    /// WB client m6: `busy` follows this device's call, not only the moment the ring arrived.
    pub fn refresh_busy(&mut self, busy: bool) -> bool {
        let mut changed = false;
        for r in self.rings.iter_mut().filter(|r| r.busy != busy) {
            r.busy = busy;
            changed = true;
        }
        changed
    }

    /// The ring a decline answers: its call and its expires_ts both match (a later ring of the same DM stays).
    pub fn remove_call(&mut self, call: [u8; 16], ring_ts: u64) -> bool {
        let before = self.rings.len();
        self.rings
            .retain(|r| !(r.call_id == call && r.ring_ts == ring_ts));
        self.rings.len() != before
    }

    pub fn clear(&mut self) -> bool {
        let changed = !self.rings.is_empty();
        self.rings.clear();
        changed
    }

    /// The local timer (R5): the ring it was armed for ends, when it still shows.
    pub fn expire(&mut self, e: Expiry) -> bool {
        let before = self.rings.len();
        self.rings.retain(|r| {
            !(r.channel == e.channel && r.ring_ts == e.ring_ts && r.expires_at == e.at)
        });
        self.rings.len() != before
    }

    /// m8: after a trust refresh, a ring whose DM is send-blocked now ends.
    pub fn retain_unblocked(&mut self, blocked: impl Fn(ChannelId) -> bool) -> bool {
        let before = self.rings.len();
        self.rings.retain(|r| !blocked(r.channel));
        self.rings.len() != before
    }
}

// ---- the outgoing ring (controller.ts `live.outgoing`) ----

/// A DM call this device just opened: everyone else of the DM who is not in the call yet is rung (an instance that does
/// not say whom it rang rang everyone, WB product walk I6).
pub fn outgoing_start(
    channel: ChannelId,
    members: &[UserId],
    own: Option<UserId>,
    in_call: &HashSet<UserId>,
) -> Outgoing {
    Outgoing {
        channel: Some(channel),
        ringing: members
            .iter()
            .copied()
            .filter(|u| Some(*u) != own && !in_call.contains(u))
            .collect(),
        declined: Vec::new(),
        not_rung: Vec::new(),
    }
}

/// op 53 state 2 (R7): a participant declined the call this device opened.
pub fn outgoing_declined(
    out: &mut Outgoing,
    members: &[UserId],
    own: Option<UserId>,
    f: &CallRingFrame,
) -> bool {
    let user = UserId(f.user_id);
    if f.state != 2
        || out.channel != Some(ChannelId(f.channel_id))
        || !members.contains(&user)
        || Some(user) == own
    {
        return false;
    }
    out.ringing.retain(|u| *u != user);
    if !out.declined.contains(&user) {
        out.declined.push(user);
    }
    true
}

/// controller.ts `shrinkRinging`: whoever is in the call now is no longer rung.
pub fn outgoing_shrink(out: &mut Outgoing, in_call: &HashSet<UserId>) -> bool {
    let (r, n) = (out.ringing.len(), out.not_rung.len());
    out.ringing.retain(|u| !in_call.contains(u));
    out.not_rung.retain(|u| !in_call.contains(u));
    r != out.ringing.len() || n != out.not_rung.len()
}

// ---- the blocked call (decision 4, ruling I2) ----

/// Ruling I2 and review M4 (controller.ts `changedUser`): the changed contact a block names, only among this call's own
/// participants. `changed` is every user the core's trust state holds as changed (a pending key, or a leaf blocked
/// `E_UMK_CHANGED`). The leaf the core's refusal `reported` comes first while the trust state still shows it changed;
/// else the first by user id among `candidates`; never the own user, and nobody when no candidate is known.
pub fn changed_user(
    changed: &[UserId],
    own: Option<UserId>,
    candidates: &[UserId],
    reported: Option<UserId>,
) -> Option<UserId> {
    if let Some(r) = reported
        && Some(r) != own
        && changed.contains(&r)
    {
        return Some(r);
    }
    let mut rows: Vec<UserId> = changed
        .iter()
        .copied()
        .filter(|u| Some(*u) != own && candidates.contains(u))
        .collect();
    rows.sort();
    rows.first().copied()
}

/// The users the core's `trust_state` holds as changed: rows `[user, umk, first_seen, verified, changed_umk|null,
/// blocked|null]` with a changed key or an `E_UMK_CHANGED` block.
pub fn decode_changed(raw: &[u8]) -> Result<Vec<UserId>, DecodeError> {
    Ok(decode_trust(raw)?
        .into_iter()
        .filter_map(|(u, changed)| changed.then_some(u))
        .collect())
}

/// Every pinned contact of the core's `trust_state`, and whether it is changed (see [`decode_changed`]).
pub fn decode_trust(raw: &[u8]) -> Result<Vec<(UserId, bool)>, DecodeError> {
    let v = decode(raw)?;
    let rows = v.as_array().ok_or(DecodeError("trust rows"))?;
    let mut out = Vec::new();
    for row in rows {
        let a = at_least(row, 6)?;
        let pending = !matches!(a[4], Value::Null);
        let blocked = matches!(&a[5], Value::Text(code) if code == "E_UMK_CHANGED");
        out.push((UserId(bin16(&a[0])?), pending || blocked));
    }
    Ok(out)
}

// ---- AFK (admin/afk.ts, worker/controller.ts) ----

/// afk.ts `AFK_BUILTIN_POSITION` (R42): the built-in AFK channel's position, reserved by the server.
pub const AFK_BUILTIN_POSITION: u64 = 1 << 30;

/// afk.ts `isBuiltinAFK` (R-AFK8): by the snapshot's channel id when it names one, else the channel at exactly the
/// reservation.
pub fn is_builtin_afk(id: ChannelId, position: u64, afk: Option<ChannelId>) -> bool {
    match afk {
        Some(afk) => id == afk,
        None => position == AFK_BUILTIN_POSITION,
    }
}

/// controller.ts `awayPatched` (AW4): the roster after one op 55 change (`since` 0 = cleared), or `None` when it changes
/// nothing (a clear of a member who is not away, or a set no newer than the held since).
pub fn away_patched(away: &[Away], change: &Away) -> Option<Vec<Away>> {
    let at = away.iter().position(|a| a.user == change.user);
    if change.since == 0 {
        let at = at?;
        let mut next = away.to_vec();
        next.remove(at);
        return Some(next);
    }
    if at.is_some_and(|i| away[i].since >= change.since) {
        return None;
    }
    let mut next: Vec<Away> = away
        .iter()
        .filter(|a| a.user != change.user)
        .cloned()
        .collect();
    next.push(change.clone());
    next.sort_by_key(|a| a.user);
    Some(next)
}

/// routes.ts `getAFK`: `[channel|null, timeout_seconds, occupants, reason?, away?]`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct AfkRead {
    pub channel: Option<ChannelId>,
    pub timeout_seconds: u64,
    pub reason: u8,
    pub away: Vec<Away>,
}

/// routes.ts `getAFK`'s decode: a row with since 0 is skipped, a repeated user keeps its first row (fix round 1, N3);
/// every bound the TS client checks.
pub fn decode_afk(raw: &[u8]) -> Result<AfkRead, DecodeError> {
    let v = decode(raw)?;
    let a = at_least(&v, 3)?;
    let channel = match &a[0] {
        Value::Null => None,
        c => Some(ChannelId(bin16(c)?)),
    };
    let timeout_seconds = u53(&a[1])?;
    match a[2].as_array() {
        Some(o) if o.len() <= 1024 => {}
        _ => return Err(DecodeError("AFK occupants")),
    }
    // routes.ts `pendingReason`: 0, 1, 3 as they are, any other value is 2.
    let reason = match a.get(3).map(u53).transpose()? {
        None | Some(0) => 0,
        Some(1) => 1,
        Some(3) => 3,
        Some(_) => 2,
    };
    let mut away: Vec<Away> = Vec::new();
    if let Some(rows) = a.get(4) {
        let rows = match rows.as_array() {
            Some(r) if r.len() <= AFK_AWAY_MAX => r,
            _ => return Err(DecodeError("AFK away")),
        };
        for row in rows {
            let r = at_least(row, 2)?;
            let user = UserId(bin16(&r[0])?);
            let since = u53(&r[1])?;
            if since == 0 || away.iter().any(|x| x.user == user) {
                continue;
            }
            away.push(Away { user, since });
        }
    }
    away.sort_by_key(|x| x.user);
    Ok(AfkRead {
        channel,
        timeout_seconds,
        reason,
        away,
    })
}

/// The AFK reads of one client: each community's newest read and the op 55 frames received while it is in flight
/// (fix round 1, I1), and the seq every landed read takes (R-AFK4: it only rises).
#[derive(Debug, Default)]
pub struct AfkBook {
    reads: HashMap<CommunityId, (u64, Vec<Away>)>,
    /// When each community's last read failed: no new read starts within [`AFK_RETRY`] of it.
    failed: HashMap<CommunityId, Instant>,
    next_read: u64,
    landed: u64,
}

/// How long after a failed AFK read no new one starts (a view that asks on every frame reads at most this often).
pub const AFK_RETRY: Duration = Duration::from_secs(5);

impl AfkBook {
    /// A read starts at `now`: its token, or `None` while a read of the community is in flight or within [`AFK_RETRY`]
    /// of its last failure (a view asking on every frame causes one read, not one per frame).
    pub fn begin(&mut self, community: CommunityId, now: Instant) -> Option<u64> {
        if self.reads.contains_key(&community) {
            return None;
        }
        if self
            .failed
            .get(&community)
            .is_some_and(|at| now.saturating_duration_since(*at) < AFK_RETRY)
        {
            return None;
        }
        self.next_read += 1;
        self.reads.insert(community, (self.next_read, Vec::new()));
        Some(self.next_read)
    }

    /// op 55: kept for the read in flight, and the held snapshot patched in place (its seq stays, so no move is
    /// announced). Answers the patched snapshot, or `None` when nothing is held or nothing changed.
    pub fn frame(&mut self, held: Option<&AfkSnapshot>, f: &AfkAwayFrame) -> Option<AfkSnapshot> {
        let change = Away {
            user: UserId(f.user_id),
            since: f.since,
        };
        // Bounded as the read is (routes.ts AFK_AWAY_MAX): an instance flooding op 55 grows neither the buffer nor the
        // roster past it.
        if let Some((_, buffer)) = self.reads.get_mut(&CommunityId(f.community_id))
            && buffer.len() < AFK_AWAY_MAX
        {
            buffer.push(change.clone());
        }
        let held = held?;
        let away = away_patched(&held.away, &change)?;
        if away.len() > AFK_AWAY_MAX {
            return None;
        }
        Some(AfkSnapshot {
            away,
            ..held.clone()
        })
    }

    /// The read `token` landed: its snapshot with the frames received meanwhile replayed in order, and the next seq;
    /// `None` when a newer read superseded it.
    pub fn land(
        &mut self,
        community: CommunityId,
        token: u64,
        read: AfkRead,
    ) -> Option<AfkSnapshot> {
        let (current, _) = self.reads.get(&community)?;
        if *current != token {
            return None;
        }
        let (_, frames) = self.reads.remove(&community)?;
        let mut away = read.away;
        for change in &frames {
            if let Some(next) = away_patched(&away, change)
                && next.len() <= AFK_AWAY_MAX
            {
                away = next;
            }
        }
        self.failed.remove(&community);
        self.landed += 1;
        Some(AfkSnapshot {
            channel: read.channel,
            timeout_seconds: read.timeout_seconds,
            pending: read.reason != 0,
            pending_reason: read.reason,
            seq: self.landed,
            away,
        })
    }

    /// The read `token` failed at `now`: its buffer goes (a newer read keeps its own), and no new read starts for
    /// [`AFK_RETRY`].
    pub fn fail(&mut self, community: CommunityId, token: u64, now: Instant) {
        if self.reads.get(&community).is_some_and(|(t, _)| *t == token) {
            self.reads.remove(&community);
            self.failed.insert(community, now);
        }
    }
}

/// R-AFK4: the move a landed snapshot announces (a pending cut), keyed by its seq.
pub fn afk_notice(community: CommunityId, snapshot: &AfkSnapshot) -> Option<AfkNotice> {
    snapshot.pending.then_some(AfkNotice {
        community,
        channel: snapshot.channel,
        reason: snapshot.pending_reason,
        seq: snapshot.seq,
    })
}

// ---- the client's ring area ----

/// A DM this device knows (`GET /v1/dms`): its kind (3 a DM, 4 a group DM) and members, the own user included.
#[derive(Clone, Debug, PartialEq, Eq)]
struct Dm {
    #[allow(
        dead_code,
        reason = "the notification's mode reads it (Task 22's RingInput.kind)"
    )]
    kind: u8,
    members: Vec<UserId>,
}

/// The outgoing call this device opened, with its DM's members.
#[derive(Clone, Debug)]
struct Live {
    channel: ChannelId,
    members: Vec<UserId>,
}

#[derive(Default)]
struct PerClient {
    book: Mutex<RingBook>,
    dms: Mutex<HashMap<ChannelId, Dm>>,
    live: Mutex<Option<Live>>,
    outgoing: Mutex<Option<Outgoing>>,
    in_call: Mutex<Option<String>>,
    afk: Mutex<AfkBook>,
    queue: OnceLock<mpsc::Sender<Frame>>,
    /// When the DM list was last read: at most once per [`DMS_REREAD`].
    last_dms_read: Mutex<Option<Instant>>,
    /// Each channel's ring timer: re-arming aborts the one before.
    timers: Mutex<HashMap<ChannelId, tokio::task::AbortHandle>>,
}

/// The bound on the area's frame queue: frames past it are dropped (op 53 and 55 are droppable, gateway/frames.ts
/// `DROPPABLE_OPS`).
pub const FRAME_QUEUE: usize = 256;
/// The least time between two reads of the DM list (a ring for an unknown DM reads it again).
pub const DMS_REREAD: Duration = Duration::from_secs(5);

fn guard<T>(m: &Mutex<T>) -> std::sync::MutexGuard<'_, T> {
    m.lock().unwrap_or_else(PoisonError::into_inner)
}

type Clients = Vec<(Weak<Inner>, Arc<PerClient>)>;
static CLIENTS: LazyLock<Mutex<Clients>> = LazyLock::new(Mutex::default);

/// The ring area's state of `inner`'s client, made on first use.
fn per_client(inner: &Arc<Inner>) -> Arc<PerClient> {
    let mut clients = guard(&CLIENTS);
    clients.retain(|(w, _)| w.strong_count() > 0);
    let mine = Arc::downgrade(inner);
    if let Some((_, state)) = clients.iter().find(|(w, _)| Weak::ptr_eq(w, &mine)) {
        return state.clone();
    }
    let state = Arc::new(PerClient::default());
    clients.push((mine, state.clone()));
    state
}

/// The ring area's handle for ring: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Ring {
    inner: Arc<Inner>,
    calls: Calls<NoHttp>,
}

impl Client {
    /// The [`Ring`] handle.
    pub fn ring(&self) -> Ring {
        Ring {
            inner: self.inner().clone(),
            calls: self.calls(),
        }
    }
}

impl Ring {
    /// The handle from a client's parts (the voice area joins through this gate, Task 17).
    pub(crate) fn of(inner: Arc<Inner>, calls: Calls<NoHttp>) -> Self {
        Self { inner, calls }
    }
}

impl AreaFrames for Ring {
    fn on_frame(inner: &Arc<Inner>, f: &Frame) {
        if f.op != Op::CALL_RING && f.op != Op::AFK_AWAY {
            return;
        }
        let pc = per_client(inner);
        let tx = pc.queue.get_or_init(|| {
            let (tx, rx) = mpsc::channel(FRAME_QUEUE);
            drop(inner.spawn(worker(Arc::downgrade(inner), Arc::downgrade(&pc), rx)));
            tx
        });
        // Full (an instance flooding the area): the frame is dropped, never queued without bound.
        let _ = tx.try_send(f.clone());
    }
}

/// The client's frames of this area, in arrival order. It ends once the client (or its ring state) is gone.
async fn worker(
    inner: Weak<Inner>,
    pc: Weak<PerClient>,
    mut rx: mpsc::Receiver<Frame>,
) -> Result<(), ClientError> {
    while let Some(f) = rx.recv().await {
        let (Some(inner), Some(pc)) = (inner.upgrade(), pc.upgrade()) else {
            break;
        };
        if f.op == Op::CALL_RING {
            // A frame that does not read is dropped (L-GW-33).
            if let Ok(ring) = call_ring_of(&f) {
                on_ring(&inner, &pc, &ring).await;
            }
        } else if let Ok(away) = afk_away_of(&f) {
            on_afk_away(&inner, &pc, &away);
        }
    }
    Ok(())
}

/// What a ring's checks answered for one channel, read before the book is touched.
struct Answers {
    own: Option<UserId>,
    channel: ChannelId,
    members: Option<Vec<UserId>>,
    blocked: bool,
    busy: bool,
    quiet: bool,
    names: HashMap<UserId, String>,
}

impl RingDeps for Answers {
    fn own_user(&self) -> Option<UserId> {
        self.own
    }
    fn dm_members(&self, channel: ChannelId) -> Option<Vec<UserId>> {
        (channel == self.channel)
            .then(|| self.members.clone())
            .flatten()
    }
    fn blocked(&self, channel: ChannelId) -> bool {
        channel != self.channel || self.blocked
    }
    fn busy(&self) -> bool {
        self.busy
    }
    fn quiet(&self, _channel: ChannelId) -> bool {
        self.quiet
    }
    fn name_of(&self, user: UserId) -> String {
        self.names
            .get(&user)
            .cloned()
            .unwrap_or_else(|| user.hex()[..8].to_owned())
    }
}

async fn on_ring(inner: &Arc<Inner>, pc: &Arc<PerClient>, f: &CallRingFrame) {
    let channel = ChannelId(f.channel_id);
    let own = inner.snapshot().account.user_id;
    if f.state == 2 {
        let members = guard(&pc.live)
            .as_ref()
            .filter(|l| l.channel == channel)
            .map(|l| l.members.clone());
        let Some(members) = members else {
            return;
        };
        let mut out = guard(&pc.outgoing);
        if let Some(o) = out.as_mut()
            && outgoing_declined(o, &members, own, f)
        {
            publish_outgoing(inner, o.clone());
        }
        return;
    }
    let step = if f.state == 1 {
        // A DM this device does not list yet (opened a moment ago): its list is read once more before the check.
        if !guard(&pc.dms).contains_key(&channel) {
            load_dms(inner, pc).await;
        }
        let members = guard(&pc.dms).get(&channel).map(|d| d.members.clone());
        let blocked = members.is_some() && channel_blocked(inner, channel).await;
        let state = inner.snapshot();
        let answers = Answers {
            own,
            channel,
            names: members
                .iter()
                .flatten()
                .map(|u| (*u, name_of(&state, *u)))
                .collect(),
            members,
            blocked,
            busy: guard(&pc.in_call).is_some(),
            quiet: dm_muted(inner, channel),
        };
        guard(&pc.book).frame(f, Instant::now(), &answers)
    } else {
        guard(&pc.book).frame(f, Instant::now(), &NoDeps)
    };
    if let Some(e) = step.arm {
        arm(inner, pc, e);
    }
    if step.changed {
        publish_rings(inner, pc);
    }
}

/// State 0 needs no check.
struct NoDeps;

impl RingDeps for NoDeps {
    fn own_user(&self) -> Option<UserId> {
        None
    }
    fn dm_members(&self, _: ChannelId) -> Option<Vec<UserId>> {
        None
    }
    fn blocked(&self, _: ChannelId) -> bool {
        true
    }
    fn busy(&self) -> bool {
        false
    }
    fn quiet(&self, _: ChannelId) -> bool {
        false
    }
    fn name_of(&self, user: UserId) -> String {
        user.hex()[..8].to_owned()
    }
}

/// R5: a local timer ends the ring at its deadline when no state 0 arrives. One sleeper per channel: re-arming aborts
/// the one before. Runs on the client's runtime (the area's worker calls it).
fn arm(inner: &Arc<Inner>, pc: &Arc<PerClient>, e: Expiry) {
    let (weak, weak_pc) = (Arc::downgrade(inner), Arc::downgrade(pc));
    let sleeper = tokio::spawn(async move {
        tokio::time::sleep_until(e.at).await;
        if let (Some(inner), Some(pc)) = (weak.upgrade(), weak_pc.upgrade())
            && guard(&pc.book).expire(e)
        {
            publish_rings(&inner, &pc);
        }
    });
    // At most one handle per channel: a finished sleeper's handle stays until the channel rings again.
    if let Some(previous) = guard(&pc.timers).insert(e.channel, sleeper.abort_handle()) {
        previous.abort();
    }
}

fn publish_rings(inner: &Arc<Inner>, pc: &Arc<PerClient>) {
    let rings = guard(&pc.book).list().to_vec();
    inner.publish(move |s| s.ring.rings = rings);
}

fn publish_outgoing(inner: &Arc<Inner>, out: Outgoing) {
    inner.publish(move |s| s.ring.outgoing = Some(out));
}

fn on_afk_away(inner: &Arc<Inner>, pc: &Arc<PerClient>, f: &AfkAwayFrame) {
    let community = CommunityId(f.community_id);
    let held = inner.snapshot().ring.afk.get(&community).cloned();
    let patched = guard(&pc.afk).frame(held.as_ref(), f);
    if let Some(next) = patched {
        inner.publish(move |s| {
            s.ring.afk.insert(community, next);
        });
    }
}

/// controller.ts `nameOf`: a member's name from the first loaded member list that has them, else the first 8 hex
/// digits of their id.
pub fn name_of(state: &AppState, user: UserId) -> String {
    state
        .members
        .values()
        .flatten()
        .find(|m| m.user_id == user)
        .map(|m| {
            if m.display.is_empty() {
                m.username.clone()
            } else {
                m.display.clone()
            }
        })
        .unwrap_or_else(|| user.hex()[..8].to_owned())
}

/// The DM is muted (`isMuted`, Task 22's `features::notify::is_muted` over the instance prefs): the ring shows as a
/// banner only.
pub(crate) fn dm_muted(inner: &Arc<Inner>, channel: ChannelId) -> bool {
    crate::features::notify::is_muted(inner.prefs(), channel, None)
}

/// The rings and the block checked against the trust as it stands now (`refresh_blocks` after its pass; Task 13's trust
/// watcher after any pass that changed what the trust slice shows): a ring whose DM is send-blocked ends, and a block
/// whose cause cleared goes.
pub(crate) async fn recheck_blocks(inner: &Arc<Inner>) {
    let pc = per_client(inner);
    let channels: Vec<ChannelId> = guard(&pc.book).list().iter().map(|r| r.channel).collect();
    let mut blocked = HashSet::new();
    for c in channels {
        if channel_blocked(inner, c).await {
            blocked.insert(c);
        }
    }
    if guard(&pc.book).retain_unblocked(|c| blocked.contains(&c)) {
        publish_rings(inner, &pc);
    }
    let Some(block) = inner.snapshot().ring.blocked.clone() else {
        return;
    };
    let is_dm = guard(&pc.dms).contains_key(&block.channel);
    let held = if is_dm {
        text_block(inner, block.channel).await.is_some()
    } else {
        block.when == BlockWhen::Left
    };
    let contact_held = match block.changed_user {
        Some(u) => changed_users(inner).await.contains(&u),
        None => held,
    };
    if !(held || contact_held) {
        inner.publish(move |s| {
            if s.ring.blocked.as_ref() == Some(&block) {
                s.ring.blocked = None;
            }
        });
    }
}

fn socket(inner: &Arc<Inner>) -> SocketHttp<NoHttp> {
    SocketHttp::new(inner.socket_requests(), NoHttp)
}

/// routes.ts `listDms`: `[channel, kind, members|null]` rows.
fn decode_dms(raw: &[u8]) -> Result<HashMap<ChannelId, Dm>, DecodeError> {
    let v = decode(raw)?;
    let rows = v.as_array().ok_or(DecodeError("DM rows"))?;
    let mut out = HashMap::new();
    for row in rows {
        let a = at_least(row, 3)?;
        let kind = match u53(&a[1])? {
            3 => 3,
            4 => 4,
            _ => return Err(DecodeError("DM kind")),
        };
        let members = match &a[2] {
            Value::Null => Vec::new(),
            m => m
                .as_array()
                .ok_or(DecodeError("DM members"))?
                .iter()
                .map(|u| bin16(u).map(UserId))
                .collect::<Result<_, _>>()?,
        };
        out.insert(ChannelId(bin16(&a[0])?), Dm { kind, members });
    }
    Ok(out)
}

/// Whether a read of the DM list may start at `now` (none in the last [`DMS_REREAD`]); if so, `now` is recorded.
pub fn dms_read_due(last: &mut Option<Instant>, now: Instant) -> bool {
    if last.is_some_and(|at| now.saturating_duration_since(at) < DMS_REREAD) {
        return false;
    }
    *last = Some(now);
    true
}

/// Reads this device's DM list again, at most once per [`DMS_REREAD`] (a ring flood for unknown channels reads it no
/// more often). A failed read keeps the list it had.
async fn load_dms(inner: &Arc<Inner>, pc: &Arc<PerClient>) {
    if !dms_read_due(&mut guard(&pc.last_dms_read), Instant::now()) {
        return;
    }
    if let Ok(Raw(raw)) = socket(inner).call::<ListDms>(&[], NoBody).await
        && let Ok(dms) = decode_dms(&raw)
    {
        *guard(&pc.dms) = dms;
    }
}

/// controller.ts `channelBlocked`: any active group of the channel (its text group, or its call group when this device
/// holds it) is send-blocked. A core that cannot answer is blocked (fail closed).
async fn channel_blocked(inner: &Arc<Inner>, channel: ChannelId) -> bool {
    let core = inner.engine().core().clone();
    try_core(&core, move |c| {
        let Ok(raw) = c.groups() else {
            return true;
        };
        let Ok(groups) = decode_groups(&raw) else {
            return true;
        };
        groups
            .iter()
            .filter(|g| g.state == 2 && g.target_id == channel.0)
            .any(|g| !matches!(c.send_block_code(&g.group_id), Ok(None)))
    })
    .await
    .unwrap_or(true)
}

/// controller.ts `textBlocked` for a DM (decision 4): the code its active text group is send-blocked with. A gate the
/// core cannot read is held (`E_CORE`): fail closed.
async fn text_block(inner: &Arc<Inner>, channel: ChannelId) -> Option<String> {
    let core = inner.engine().core().clone();
    try_core(&core, move |c| {
        let Ok(raw) = c.groups() else {
            return Some("E_CORE".to_owned());
        };
        let Ok(groups) = decode_groups(&raw) else {
            return Some("E_CORE".to_owned());
        };
        let text = groups
            .iter()
            .find(|g| g.kind == 0 && g.state == 2 && g.target_id == channel.0)?;
        match c.send_block_code(&text.group_id) {
            Ok(code) => code.map(str::to_owned),
            Err(_) => Some("E_CORE".to_owned()),
        }
    })
    .await
    .unwrap_or_else(|_| Some("E_CORE".to_owned()))
}

/// The users the core's trust state holds as changed (none when it cannot be read).
async fn changed_users(inner: &Arc<Inner>) -> Vec<UserId> {
    let core = inner.engine().core().clone();
    try_core(&core, |c| c.trust_state().ok())
        .await
        .ok()
        .flatten()
        .and_then(|raw| decode_changed(&raw).ok())
        .unwrap_or_default()
}

/// A join through [`Ring::join`]: the call's [`CallJoin`], whose outcome also ends the channel's ring, starts the
/// outgoing ring of a DM call this device opened, and publishes a trust refusal as the channel's [`CallBlock`].
pub struct GatedJoin {
    join: CallJoin,
    inner: Arc<Inner>,
    pc: Arc<PerClient>,
    channel: ChannelId,
    /// The DM's members; `None` for a voice channel.
    dm: Option<Vec<UserId>>,
}

impl GatedJoin {
    pub fn phase(&self) -> watch::Receiver<CallPhase> {
        self.join.phase()
    }

    pub fn cancel(&self) {
        self.join.cancel();
    }

    /// The join's outcome ([`CallJoin::outcome`]); dropping this before it answers cancels the join.
    pub async fn outcome(self) -> Result<Result<Joined, JoinRefused>, ClientError> {
        let GatedJoin {
            join,
            inner,
            pc,
            channel,
            dm,
        } = self;
        let answer = join.outcome().await;
        // The bookkeeping runs on the client's runtime (it reads the instance), whatever executor awaits this.
        let then = match &answer {
            Ok(Ok(joined)) => Then::Joined {
                created: joined.started.created,
            },
            Ok(Err(refused)) if refused.phase == crate::calls::join::Phase::Blocked => {
                Then::Blocked {
                    code: refused.code.clone(),
                    users: refused.users.clone(),
                    reported: refused.reported,
                }
            }
            _ => Then::Nothing,
        };
        let task = inner.clone();
        let done = inner.spawn(async move {
            let inner = task;
            bookkeeping(&inner, &pc, channel, dm, then).await;
            Ok(())
        });
        let _ = done.await;
        answer
    }
}

/// What a join's outcome leaves to do.
enum Then {
    Joined {
        created: bool,
    },
    Blocked {
        code: String,
        users: Vec<UserId>,
        reported: Option<UserId>,
    },
    Nothing,
}

/// A joined call ends its channel's ring, starts the outgoing ring of a DM call it opened and ends that channel's block; a
/// trust refusal is published as the channel's block.
async fn bookkeeping(
    inner: &Arc<Inner>,
    pc: &Arc<PerClient>,
    channel: ChannelId,
    dm: Option<Vec<UserId>>,
    then: Then,
) {
    {
        match then {
            Then::Joined { created } => {
                if guard(&pc.book).remove(channel) {
                    publish_rings(inner, pc);
                }
                let own = inner.snapshot().account.user_id;
                if let Some(members) = dm
                    && created
                {
                    let out = outgoing_start(channel, &members, own, &HashSet::new());
                    *guard(&pc.live) = Some(Live { channel, members });
                    *guard(&pc.outgoing) = Some(out.clone());
                    publish_outgoing(inner, out);
                }
                inner.publish(move |s| {
                    if s.ring
                        .blocked
                        .as_ref()
                        .is_some_and(|b| b.channel == channel)
                    {
                        s.ring.blocked = None;
                    }
                });
            }
            Then::Blocked {
                code,
                users,
                reported,
            } => {
                // Ruling I2 and review M4 (controller.ts:325): the changed contact among this call's own
                // participants only: the DM's members, else the room's users as the join last read them; the leaf
                // the core's refusal named comes first. With none known, nobody is named.
                let candidates = dm.unwrap_or(users);
                publish_block(
                    inner,
                    channel,
                    code,
                    BlockWhen::BeforeJoin,
                    &candidates,
                    reported,
                )
                .await;
            }
            Then::Nothing => {}
        }
    }
}

async fn publish_block(
    inner: &Arc<Inner>,
    channel: ChannelId,
    code: String,
    when: BlockWhen,
    candidates: &[UserId],
    reported: Option<UserId>,
) -> CallBlock {
    let changed_user = if code == "E_UMK_CHANGED" {
        let own = inner.snapshot().account.user_id;
        changed_user(&changed_users(inner).await, own, candidates, reported)
    } else {
        None
    };
    let block = CallBlock {
        channel,
        code,
        changed_user,
        when,
    };
    let published = block.clone();
    inner.publish(move |s| s.ring.blocked = Some(published));
    block
}

impl Ring {
    /// Joins the call of `channel` (a DM's when `community` is `None`): decision 4 first, before anything of the join
    /// is read or sent. A DM whose text group is send-blocked is refused here with its code and changed contact, and
    /// published as [`RingState::blocked`]; nothing reaches the instance. Otherwise the join runs ([`Calls::join`]:
    /// its own gate, `call_join_check`, refuses a poisoned call tree before its commit is posted, RV-1), and its
    /// outcome ends the ring ([`GatedJoin::outcome`]). Answering a ring is this join of the ring's DM.
    pub fn join(
        &self,
        channel: ChannelId,
        community: Option<CommunityId>,
        vdec: String,
    ) -> oneshot::Receiver<Result<Result<GatedJoin, CallBlock>, ClientError>> {
        let inner = self.inner.clone();
        let calls = self.calls.clone();
        let pc = per_client(&inner);
        self.inner.spawn(async move {
            // Review M5: a channel this device lists as a DM gets the DM gate whatever `community` says.
            let listed_dm = guard(&pc.dms).contains_key(&channel);
            let dm = if community.is_none() || listed_dm {
                if !listed_dm {
                    load_dms(&inner, &pc).await;
                }
                let members = guard(&pc.dms).get(&channel).map(|d| d.members.clone());
                let Some(members) = members else {
                    return Err(ClientError::UnknownChannel);
                };
                // Decision 4: refused while the DM's text group is send-blocked, before anything reaches the instance.
                if let Some(code) = text_block(&inner, channel).await {
                    let block =
                        publish_block(&inner, channel, code, BlockWhen::BeforeJoin, &members, None)
                            .await;
                    return Ok(Err(block));
                }
                Some(members)
            } else {
                None
            };
            let join = calls.join(CallTarget {
                channel: channel.0,
                community: community.map(|c| c.0),
                vdec,
            });
            Ok(Ok(GatedJoin {
                join,
                inner,
                pc,
                channel,
                dm,
            }))
        })
    }

    /// callDecline: the ring goes here whatever the instance answers (a 404 is expected after the ring ended); only the
    /// ring this decline answers (its call and expires_ts).
    pub fn decline(
        &self,
        call: [u8; 16],
        ring_ts: u64,
    ) -> oneshot::Receiver<Result<(), ClientError>> {
        let pc = per_client(&self.inner);
        if guard(&pc.book).remove_call(call, ring_ts) {
            publish_rings(&self.inner, &pc);
        }
        let routes = HttpRoutes(socket(&self.inner));
        self.inner.spawn(async move {
            match decline_ring(&routes, &call, ring_ts).await {
                // An answer of the instance (a refusal included) ends here; a transport failure is the caller's.
                Ok(()) => Ok(()),
                Err(e) if e.status.is_some() => Ok(()),
                Err(e) => Err(ClientError::Network(e.detail)),
            }
        })
    }

    /// The voice area (Task 17) reports this device's call: `Some(where)` while it is in, joining or paused in a call
    /// (`where` is the busy text's channel or DM name), `None` once it left. Rings follow it (WB client m6); a call
    /// that ends takes its outgoing ring with it, and a call that starts ends an earlier block's notice.
    pub fn set_in_call(&self, place: Option<String>) {
        let pc = per_client(&self.inner);
        let busy = place.is_some();
        *guard(&pc.in_call) = place.clone();
        if guard(&pc.book).refresh_busy(busy) {
            publish_rings(&self.inner, &pc);
        }
        if !busy {
            *guard(&pc.live) = None;
            *guard(&pc.outgoing) = None;
        }
        self.inner.publish(move |s| {
            if busy {
                s.ring.blocked = None;
            } else {
                s.ring.outgoing = None;
            }
            s.ring.in_call = place;
        });
    }

    /// The voice area (Task 17) reports who is in `channel`'s call now (op 50): they are no longer rung.
    pub fn voice_roster(&self, channel: ChannelId, users: &[UserId]) {
        let pc = per_client(&self.inner);
        let in_call: HashSet<UserId> = users.iter().copied().collect();
        let mut out = guard(&pc.outgoing);
        if let Some(o) = out.as_mut()
            && o.channel == Some(channel)
            && outgoing_shrink(o, &in_call)
        {
            publish_outgoing(&self.inner, o.clone());
        }
    }

    /// A call left because of a trust block mid-call (the voice area's rekey: RV-3's `blocked`, an unlisted device):
    /// published as the channel's [`CallBlock`] with the changed contact among `participants`.
    pub fn record_block(
        &self,
        channel: ChannelId,
        code: String,
        when: BlockWhen,
        participants: Vec<UserId>,
    ) -> oneshot::Receiver<Result<(), ClientError>> {
        let inner = self.inner.clone();
        self.inner.spawn(async move {
            publish_block(&inner, channel, code, when, &participants, None).await;
            Ok(())
        })
    }

    /// After a trust refresh (onSendBlocks, m8): the engine's trust is read again; a ring whose DM is send-blocked now
    /// ends, and a block whose cause cleared (its DM's text group no longer blocked, or its changed contact verified)
    /// goes, so the join is offered again.
    pub fn refresh_blocks(&self) -> oneshot::Receiver<Result<(), ClientError>> {
        let inner = self.inner.clone();
        self.inner.spawn(async move {
            inner.engine().refresh_trust().await;
            recheck_blocks(&inner).await;
            Ok(())
        })
    }

    /// Decision 4's read alone, sending nothing: the code `channel`'s DM text group is send-blocked with, if any. The DM
    /// call button reads it to offer or hold the call.
    pub fn dm_blocked(
        &self,
        channel: ChannelId,
    ) -> oneshot::Receiver<Result<Option<String>, ClientError>> {
        let inner = self.inner.clone();
        self.inner
            .spawn(async move { Ok(text_block(&inner, channel).await) })
    }

    /// Whether the core's trust state holds `user` as changed (a pending key, or a leaf blocked `E_UMK_CHANGED`).
    /// `None` while the core has no key of `user` pinned.
    pub fn contact_trust(
        &self,
        user: UserId,
    ) -> oneshot::Receiver<Result<Option<bool>, ClientError>> {
        let core = self.inner.engine().core().clone();
        self.inner.spawn(async move {
            Ok(try_core(&core, |c| c.trust_state().ok())
                .await
                .ok()
                .flatten()
                .and_then(|raw| decode_trust(&raw).ok())
                .and_then(|rows| rows.into_iter().find(|(u, _)| *u == user))
                .map(|(_, changed)| changed))
        })
    }

    /// The person dismissed the block notice.
    pub fn clear_block(&self) {
        self.inner.publish(|s| s.ring.blocked = None);
    }

    /// Reads `community`'s AFK snapshot (readAFK): the frames received while it is in flight are replayed onto it, and
    /// a read that says this device has a pending cut announces the move once ([`RingState::afk_notice`]). A failed read
    /// keeps the snapshot held.
    pub fn load_afk(&self, community: CommunityId) -> oneshot::Receiver<Result<(), ClientError>> {
        let inner = self.inner.clone();
        let pc = per_client(&inner);
        // Review I2: one read at a time per community, and none within AFK_RETRY of a failure: a view that asks on every
        // frame reads once.
        let Some(token) = guard(&pc.afk).begin(community, Instant::now()) else {
            return self.inner.spawn(async { Ok(()) });
        };
        self.inner.spawn(async move {
            let read = socket(&inner)
                .call::<GetAfk>(&[&community.hex()], NoBody)
                .await
                .map_err(|e| ClientError::Network(e.to_string()))
                .and_then(|Raw(raw)| {
                    decode_afk(&raw).map_err(|e| ClientError::Sync(e.to_string()))
                });
            let read = match read {
                Ok(r) => r,
                Err(e) => {
                    guard(&pc.afk).fail(community, token, Instant::now());
                    return Err(e);
                }
            };
            let Some(snapshot) = guard(&pc.afk).land(community, token, read) else {
                return Ok(());
            };
            let notice = afk_notice(community, &snapshot);
            inner.publish(move |s| {
                s.ring.afk.insert(community, snapshot);
                if let Some(n) = notice {
                    s.ring.afk_notice = Some(n);
                }
            });
            Ok(())
        })
    }

    /// R-AW3: the person chooses AFK in `community` (`PUT …/afk/away`): the instance cuts every device of this user from
    /// its call and lists them away; op 55 brings the change.
    pub fn go_afk(&self, community: CommunityId) -> oneshot::Receiver<Result<(), ClientError>> {
        let http = socket(&self.inner);
        self.inner.spawn(async move {
            // `[]`: an empty CBOR array.
            http.call::<PutAfkAway>(&[&community.hex()], Raw(vec![0x80]))
                .await
                .map_err(|e| ClientError::Network(e.to_string()))
        })
    }

    /// The move's notice was read (the person moved on).
    pub fn dismiss_afk_notice(&self) {
        self.inner.publish(|s| s.ring.afk_notice = None);
    }

    /// resign and reset: every ring and the outgoing ring go.
    pub fn clear(&self) {
        let pc = per_client(&self.inner);
        guard(&pc.book).clear();
        *guard(&pc.outgoing) = None;
        *guard(&pc.live) = None;
        self.inner.publish(|s| {
            s.ring = RingState {
                afk: std::mem::take(&mut s.ring.afk),
                ..RingState::default()
            };
        });
    }
}
