//! Area **voice** (Task 17): the voice-state roster, join and leave, mute and deafen, the speaking signal.
//!
//! Ports: packages/client-core/src/calls/voice.ts (`VoiceBook`), the voice_self rules of
//! packages/client-core/src/worker/controller.ts (`voiceSelf`, `ownVoiceState`), and the call lifecycle of
//! packages/web/src/call/session.ts and packages/web/src/screens/voice/useCall.tsx that is not media.
//!
//! Receives op 50 `voice_state` from the gateway (`crate::sync::route`), through [`AreaFrames`].
//!
//! The media itself (LiveKit, the `dilla-sframe/1` transform, the devices) is `dilla-media`'s, which this crate does not
//! link: a joined call hands the media layer a [`MediaLink`] (the room's URL and token, and every epoch's keys as the
//! call group moves) through [`Voice::media`], never through a slice. The media layer reports back what the person
//! should see ([`Voice::set_speaking`], [`Voice::set_notice`], [`Voice::set_share`]).
//!
//! `Inner` has no per-area storage, so what the area keeps between calls of its handle (the reads in flight, the live
//! call, the last `voice_self`) is held in a registry keyed by the client ([`runtime`]); an entry goes with its client.

use std::collections::HashSet;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex, OnceLock, Weak};
use std::time::Duration;

use tokio::sync::{oneshot, watch};
use zeroize::Zeroizing;

use crate::calls::{CallPhase, Calls, MediaEpoch};
use crate::client::{Client, ClientError, Inner};
use crate::features::ring::{GatedJoin, OnRefused, Ring};
use crate::gateway::frames::{Frame, Op, Value, voice_state_of};
use crate::seams::Http;
use crate::state::voice::{LocalShare, OwnCall, VoiceNotice, VoiceParticipant};
use crate::state::{AppState, ChannelId, CommunityId, DeviceId, UserId};
use crate::sync::route::AreaFrames;

/// controller.ts `VOICE_SELF_RESENDS`: how often per join the own `voice_self` is sent again while the instance's
/// `voice_state` lacks it.
pub const VOICE_SELF_RESENDS: u8 = 3;
/// controller.ts `VOICE_SELF_RECONNECT_SENDS`: how often the reconnect sends a `voice_self` made while offline.
pub const VOICE_SELF_RECONNECT_SENDS: u8 = 1;
/// routes.ts `COMMUNITY_CALLS_MAX`.
pub const COMMUNITY_CALLS_MAX: usize = 4096;
/// stats.ts `STATS_EVERY_MS`: one coarse report per call every 10 s, at most.
pub const STATS_EVERY: Duration = Duration::from_secs(10);
/// How often a joined call's epoch keys are read again from the core (the engine applies the group's commits).
pub const EPOCH_POLL: Duration = Duration::from_millis(250);
/// routes.ts `postCallRtt`: the most an RTT report says, in ms (the instance refuses more).
pub const RTT_MAX_MS: u32 = 5000;

/// What a joined call hands the media layer. The token and the keys are zeroized when the last holder drops them; none
/// of it is ever in a slice, a log or a `Debug`.
pub struct MediaLink {
    pub channel: ChannelId,
    pub call_id: [u8; 16],
    pub group_id: [u8; 16],
    /// The LiveKit URL as the instance answered it.
    pub url: String,
    pub token: Zeroizing<String>,
    /// `(urls, username, credential)`.
    pub ice_servers: Vec<(Vec<String>, String, Zeroizing<String>)>,
    /// The call group's newest epoch keys; the first is the join epoch's.
    pub epochs: watch::Receiver<Option<Arc<MediaEpoch>>>,
}

impl std::fmt::Debug for MediaLink {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("MediaLink")
            .field("url", &self.url)
            .finish_non_exhaustive()
    }
}

/// stats.ts `callStatsReport`: `[candidate_type, relay_protocol|null, rtt_ms, fraction_lost_permille,
/// decrypt_failures, frames_encrypted]`.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct CallStatsReport {
    pub candidate_type: u64,
    pub relay_protocol: Option<u64>,
    pub rtt_ms: u64,
    pub fraction_lost_permille: u64,
    pub decrypt_failures: u64,
    pub frames_encrypted: u64,
}

/// A snapshot read in flight (voice.ts `Read`): the frames that arrived meanwhile, applied again after its snapshot.
struct Read {
    id: u64,
    frames: Vec<Frame>,
    overflow: bool,
}

/// The call this device is in.
#[derive(Clone)]
struct Live {
    channel: ChannelId,
    call_id: [u8; 16],
    group_id: [u8; 16],
}

/// What the area keeps for one client between calls of its handle.
struct Runtime {
    reads: Vec<Read>,
    started: bool,
    /// Bumped by every join and leave: a task of an older one stops.
    generation: u64,
    /// The join in flight: dropping it cancels the join.
    joining: Option<oneshot::Sender<()>>,
    /// Closes once the join in flight has answered, its teardown done (controller.ts `joining.done`).
    join_done: Option<oneshot::Receiver<()>>,
    live: Option<Live>,
    link: watch::Sender<Option<Arc<MediaLink>>>,
    /// The last `voice_self` sent: `(call, flags)`.
    last_self: Option<([u8; 16], u8)>,
    own_in_call: HashSet<[u8; 16]>,
    resends: Option<([u8; 16], u8)>,
    /// controller.ts `selfOffline`: a `voice_self` made while offline, owed to the instance: the reconnect sends it
    /// (at most [`VOICE_SELF_RECONNECT_SENDS`] times), apart from the re-send rule's own budget.
    self_offline: Option<([u8; 16], u8)>,
    last_stats: Option<std::time::Instant>,
}

impl Default for Runtime {
    fn default() -> Self {
        Self {
            reads: Vec::new(),
            started: false,
            generation: 0,
            joining: None,
            join_done: None,
            live: None,
            link: watch::channel(None).0,
            last_self: None,
            own_in_call: HashSet::new(),
            resends: None,
            self_offline: None,
            last_stats: None,
        }
    }
}

type Registry = Mutex<Vec<(Weak<Inner>, Arc<Mutex<Runtime>>)>>;

/// The area's runtime for `inner`'s client (created on first use; entries of stopped clients are dropped).
fn runtime(inner: &Arc<Inner>) -> Arc<Mutex<Runtime>> {
    static REGISTRY: OnceLock<Registry> = OnceLock::new();
    let mut reg = REGISTRY
        .get_or_init(Registry::default)
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner);
    reg.retain(|(w, _)| w.strong_count() > 0);
    let me = Arc::downgrade(inner);
    if let Some((_, rt)) = reg.iter().find(|(w, _)| w.ptr_eq(&me)) {
        return rt.clone();
    }
    let rt = Arc::new(Mutex::new(Runtime::default()));
    reg.push((me, rt.clone()));
    rt
}

fn lock(rt: &Mutex<Runtime>) -> std::sync::MutexGuard<'_, Runtime> {
    rt.lock().unwrap_or_else(std::sync::PoisonError::into_inner)
}

static READ_IDS: AtomicU64 = AtomicU64::new(1);

/// The voice area's handle: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Voice {
    inner: Arc<Inner>,
    calls: Calls,
    states: watch::Receiver<Arc<AppState>>,
}

impl Client {
    /// The [`Voice`] handle.
    pub fn voice(&self) -> Voice {
        Voice {
            inner: self.inner().clone(),
            calls: self.calls(),
            states: self.state(),
        }
    }
}

impl AreaFrames for Voice {
    /// controller.ts `ownVoiceState` and voice.ts `frame`.
    fn on_frame(inner: &Arc<Inner>, f: &Frame) {
        let Ok(v) = voice_state_of(f) else {
            return;
        };
        let rt = runtime(inner);
        let own = inner.snapshot().account.device_id.map(|d| d.0);
        {
            let mut r = lock(&rt);
            for read in &mut r.reads {
                if read.frames.len() >= crate::state::voice::VOICE_MAX {
                    read.overflow = true;
                } else {
                    read.frames.push(f.clone());
                }
            }
            if own == Some(v.device_id) {
                own_voice_state(inner, &mut r, v.call_id, v.flags);
            }
        }
        let f = f.clone();
        inner.publish(move |s| {
            apply_frame(s, &f, own);
        });
    }
}

/// voice.ts `apply` against the published state: the frame lands in the channel whose call it is.
fn apply_frame(s: &mut AppState, f: &Frame, own: Option<[u8; 16]>) {
    let Ok(v) = voice_state_of(f) else {
        return;
    };
    let channels: HashSet<ChannelId> = s.channels.iter().map(|c| c.id).collect();
    let own_channel = s.voice.call.channel;
    let Some(channel) = s.voice.channel_of(v.call_id, |c| {
        channels.contains(&c) || own_channel == Some(c)
    }) else {
        return;
    };
    s.voice.apply(
        channel,
        v.call_id,
        VoiceParticipant {
            user_id: UserId(v.user_id),
            device_id: DeviceId(v.device_id),
            flags: v.flags,
            own: own == Some(v.device_id),
        },
    );
}

/// controller.ts `ownVoiceState`: while this device's own `voice_state` for its call lacks the self bits of the last
/// `voice_self`, it is sent again, at most [`VOICE_SELF_RESENDS`] times per join.
fn own_voice_state(inner: &Arc<Inner>, r: &mut Runtime, call: [u8; 16], flags: u8) {
    if flags & 1 == 0 {
        r.own_in_call.remove(&call);
        return;
    }
    if r.own_in_call.insert(call) {
        r.resends = Some((call, VOICE_SELF_RESENDS));
    }
    let Some((last_call, last)) = r.last_self else {
        return;
    };
    if last_call != call || (flags >> 1) & 3 == last {
        return;
    }
    match &mut r.resends {
        Some((c, left)) if *c == call && *left > 0 => *left -= 1,
        _ => return,
    }
    send_voice_self(inner, call, last);
}

fn send_voice_self(inner: &Arc<Inner>, call: [u8; 16], flags: u8) -> bool {
    let frame = Frame {
        op: Op::VOICE_SELF,
        n: 0,
        group_id: None,
        payload: vec![
            Value::Bytes(call.to_vec()),
            Value::Uint(u64::from(flags & 3)),
        ],
    };
    crate::gateway::frames::encode(&frame).is_ok_and(|b| inner.engine().send_gateway_frame(b))
}

fn rows(raw: &Value) -> Option<&[Value]> {
    match raw {
        Value::Array(a) => Some(a),
        _ => None,
    }
}

fn id16(v: &Value) -> Option<[u8; 16]> {
    match v {
        Value::Bytes(b) => <[u8; 16]>::try_from(b.as_slice()).ok(),
        _ => None,
    }
}

/// One live call of a community (L-HTTP-101): its channel, its id and who is in it.
pub type CommunityCall = (ChannelId, [u8; 16], Vec<crate::calls::CallParticipant>);

/// routes.ts `getCommunityCalls`: `[[channel_id, call_id, [[user, device, flags], ...]], ...]`.
pub fn decode_community_calls(body: &[u8]) -> Result<Vec<CommunityCall>, String> {
    let v = crate::gateway::frames::Value::decode(body).map_err(|e| format!("{e:?}"))?;
    let calls = rows(&v).ok_or("calls")?;
    if calls.len() > COMMUNITY_CALLS_MAX {
        return Err("calls: more than the bound".into());
    }
    let mut out = Vec::with_capacity(calls.len());
    for call in calls {
        let a = rows(call).filter(|a| a.len() >= 3).ok_or("call row")?;
        let channel = id16(&a[0]).ok_or("channel id")?;
        let call_id = id16(&a[1]).ok_or("call id")?;
        let people = rows(&a[2]).ok_or("participants")?;
        if people.len() > crate::calls::wire::CALL_PARTICIPANTS_MAX {
            return Err("participants: more than the bound".into());
        }
        let mut list = Vec::with_capacity(people.len());
        for p in people {
            let r = rows(p).filter(|r| r.len() >= 3).ok_or("participant")?;
            let flags = match r[2] {
                Value::Uint(f) if f <= 31 => f as u8,
                _ => return Err("voice flags".into()),
            };
            list.push(crate::calls::CallParticipant {
                user_id: id16(&r[0]).ok_or("user id")?,
                device_id: id16(&r[1]).ok_or("device id")?,
                flags,
            });
        }
        out.push((ChannelId(channel), call_id, list));
    }
    Ok(out)
}

fn participant_rows(
    people: &[crate::calls::CallParticipant],
    own: Option<[u8; 16]>,
) -> Vec<VoiceParticipant> {
    people
        .iter()
        .map(|p| VoiceParticipant {
            user_id: UserId(p.user_id),
            device_id: DeviceId(p.device_id),
            flags: p.flags,
            own: own == Some(p.device_id),
        })
        .collect()
}

impl Voice {
    fn rt(&self) -> Arc<Mutex<Runtime>> {
        runtime(&self.inner)
    }

    /// Starts following the instance: every community's calls are read on each ready (voice.ts `loadCommunity`, the
    /// worker's `loadCommunity` on ready), and again when a community is added. Idempotent.
    pub fn start(&self) {
        {
            let rt = self.rt();
            let mut r = lock(&rt);
            if r.started {
                return;
            }
            r.started = true;
        }
        let this = self.clone();
        let mut states = self.states.clone();
        drop(self.inner.spawn(async move {
            let mut seen: (u64, Vec<CommunityId>) = (u64::MAX, Vec::new());
            loop {
                let s = states.borrow_and_update().clone();
                let communities: Vec<CommunityId> = s.communities.iter().map(|c| c.id).collect();
                let online = s.connection.status == crate::state::ConnectionStatus::Online;
                if online && (s.connection.readies != seen.0 || communities != seen.1) {
                    let fresh: Vec<CommunityId> = if s.connection.readies == seen.0 {
                        communities
                            .iter()
                            .filter(|c| !seen.1.contains(c))
                            .copied()
                            .collect()
                    } else {
                        communities.clone()
                    };
                    if s.connection.readies != seen.0 {
                        this.send_offline_self();
                    }
                    seen = (s.connection.readies, communities);
                    for c in fresh {
                        // A failed read keeps what the frames show; the next ready reads again.
                        let _ = this.load_community(c).await;
                    }
                }
                if states.changed().await.is_err() {
                    return Ok(());
                }
            }
        }));
    }

    fn begin_read(&self) -> u64 {
        let id = READ_IDS.fetch_add(1, Ordering::Relaxed);
        let rt = self.rt();
        lock(&rt).reads.push(Read {
            id,
            frames: Vec::new(),
            overflow: false,
        });
        id
    }

    /// Ends read `id`: its frames, or `None` when more arrived than it keeps (its snapshot is then older than what the
    /// frames showed, and is not placed).
    fn end_read(&self, id: u64) -> Option<Vec<Frame>> {
        let rt = self.rt();
        let mut r = lock(&rt);
        let at = r.reads.iter().position(|x| x.id == id)?;
        let read = r.reads.remove(at);
        (!read.overflow).then_some(read.frames)
    }

    /// voice.ts `loadCommunity`: every live call of the community's voice channels (L-HTTP-101); a voice channel of
    /// the community it does not list has none.
    pub async fn load_community(&self, community: CommunityId) -> Result<(), ClientError> {
        use crate::http::routes::{GetCommunityCalls, NoBody};
        let id = self.begin_read();
        let answer = self
            .calls
            .routes()
            .0
            .call::<GetCommunityCalls>(&[&community.hex()], NoBody)
            .await;
        let frames = self.end_read(id);
        let raw = answer.map_err(|e| ClientError::Network(e.to_string()))?;
        let listed = decode_community_calls(&raw.0).map_err(ClientError::Sync)?;
        let Some(frames) = frames else {
            return Ok(());
        };
        let own = self.inner.snapshot().account.device_id.map(|d| d.0);
        self.inner.publish(move |s| {
            let listed_ids: HashSet<ChannelId> = listed.iter().map(|(c, _, _)| *c).collect();
            let voice_channels: Vec<ChannelId> = s
                .channels
                .iter()
                .filter(|c| c.community_id == community && c.kind == 1)
                .map(|c| c.id)
                .collect();
            for (channel, call, people) in &listed {
                s.voice
                    .place(*channel, Some(*call), participant_rows(people, own));
            }
            for ch in &voice_channels {
                if !listed_ids.contains(ch) {
                    s.voice.calls.remove(ch);
                }
            }
            let scope: HashSet<ChannelId> = listed_ids.into_iter().chain(voice_channels).collect();
            for f in &frames {
                if let Ok(v) = voice_state_of(f)
                    && s.voice
                        .channel_of(v.call_id, |c| scope.contains(&c))
                        .is_some_and(|c| scope.contains(&c))
                {
                    apply_frame(s, f, own);
                }
            }
        });
        Ok(())
    }

    /// voice.ts `loadChannel`: one channel's call (L-HTTP-100).
    pub async fn load_channel(&self, channel: ChannelId) -> Result<(), ClientError> {
        use crate::calls::CallRoutes;
        let id = self.begin_read();
        let answer = self.calls.routes().get_channel_call(&channel.0).await;
        let frames = self.end_read(id);
        let snap = answer.map_err(|e| ClientError::Sync(e.code.unwrap_or(e.detail)))?;
        let Some(frames) = frames else {
            return Ok(());
        };
        let own = self.inner.snapshot().account.device_id.map(|d| d.0);
        self.inner.publish(move |s| {
            s.voice.place(
                channel,
                snap.call_id,
                participant_rows(&snap.participants, own),
            );
            for f in &frames {
                if let Ok(v) = voice_state_of(f)
                    && s.voice.channel_of(v.call_id, |c| c == channel) == Some(channel)
                {
                    apply_frame(s, f, own);
                }
            }
        });
        Ok(())
    }

    /// The media layer's link to the joined call: `Some` while this device is in a call, `None` otherwise. The desktop
    /// joins the room when it turns `Some` and leaves it when it turns `None`.
    pub fn media(&self) -> watch::Receiver<Option<Arc<MediaLink>>> {
        lock(&self.rt()).link.subscribe()
    }

    /// Joins a voice channel's call (a channel of the published list: its community binds the call group). Leaves the
    /// call this device is in first. The phase moves in `AppState::voice.call` as the join goes ([`CallPhase`]); the
    /// answer comes once it has joined or ended without a call (its `E_*` code then also in `call.error`).
    pub fn join(&self, channel: ChannelId) -> oneshot::Receiver<Result<(), ClientError>> {
        let this = self.clone();
        let (done_tx, done_rx) = oneshot::channel::<()>();
        self.inner.spawn_holding(done_tx, async move {
            let community = this
                .inner
                .snapshot()
                .channel(channel)
                .map(|c| c.community_id)
                .ok_or(ClientError::UnknownChannel)?;
            this.join_inner(channel, Some(community), None, done_rx)
                .await
        })
    }

    /// Joins a DM's or group DM's call (no community). Task 18's ring and DM call flows use it.
    pub fn join_dm(&self, channel: ChannelId) -> oneshot::Receiver<Result<(), ClientError>> {
        let this = self.clone();
        let (done_tx, done_rx) = oneshot::channel::<()>();
        self.inner.spawn_holding(done_tx, async move {
            this.join_inner(channel, None, None, done_rx).await
        })
    }

    /// Takes over a join the ring area's gate already let through ([`Ring::join`]'s [`GatedJoin`]): this device follows
    /// it as its own call, as [`Voice::join`] does (the phases, then the media, the dock and the stage). The desktop
    /// answers a ring with [`Voice::join_dm`], which leaves this device's call before the gate posts anything; this entry
    /// is for a caller that already holds a gated join (`join_gated_follows_the_handed_join` in tests/ring.rs).
    pub fn join_gated(
        &self,
        channel: ChannelId,
        community: Option<CommunityId>,
        join: GatedJoin,
    ) -> oneshot::Receiver<Result<(), ClientError>> {
        let this = self.clone();
        let (done_tx, done_rx) = oneshot::channel::<()>();
        self.inner.spawn_holding(done_tx, async move {
            this.join_inner(channel, community, Some(join), done_rx)
                .await
        })
    }

    /// The ring area's handle on this client (Task 18): every join goes through its gate, a mid-call block is recorded
    /// with it.
    fn ring(&self) -> Ring {
        Ring::of(self.inner.clone(), self.calls.clone())
    }

    async fn join_inner(
        &self,
        channel: ChannelId,
        community: Option<CommunityId>,
        given: Option<GatedJoin>,
        done_rx: oneshot::Receiver<()>,
    ) -> Result<(), ClientError> {
        let snap = self.inner.snapshot();
        if snap.voice.call.channel == Some(channel) && snap.voice.in_call() {
            // Already in it: a handed-over join is dropped, which cancels it.
            drop(given);
            return Ok(());
        }
        if snap.voice.in_call() {
            self.leave_inner().await?;
        }
        let rt = self.rt();
        let (abort_tx, abort_rx) = oneshot::channel::<()>();
        // `done_rx` closes once this join has answered (its sender is held by the spawned task until the answer is sent:
        // [`Inner::spawn_holding`]): a leave meanwhile waits for it (controller.ts `joining.done`), so the cancelled
        // join's answer is in before the leave's.
        let generation = {
            let mut r = lock(&rt);
            r.generation += 1;
            r.joining = Some(abort_tx);
            r.join_done = Some(done_rx);
            r.last_self = None;
            r.generation
        };
        self.inner.publish(move |s| {
            s.voice.call = OwnCall {
                phase: CallPhase::Joining,
                channel: Some(channel),
                community,
                ..OwnCall::default()
            };
        });
        let refusal = |inner: Arc<Inner>| -> OnRefused {
            let rt = rt.clone();
            Box::new(move |code: &str| {
                // A leave or a newer join came first: a cancelled join's end publishes nothing over theirs.
                if lock(&rt).generation != generation {
                    return;
                }
                let code = code.to_owned();
                inner.publish(move |s| {
                    if s.voice.call.channel == Some(channel) {
                        s.voice.call = OwnCall {
                            error: Some(code),
                            ..OwnCall::default()
                        };
                    }
                });
            })
        };
        let refused = |code: String| refusal(self.inner.clone())(&code);
        // Every join goes through the ring area's gate (Task 18, decision 4): a DM whose text group is send-blocked is
        // refused before anything reaches the instance (its block published for the stage), and the outcome ends a ring
        // of the channel. An answered ring hands its join over already gated. The gate runs `refusal` before it
        // publishes a block, so a published block never sits beside this call still joining; the `refused` calls
        // below are then no-ops (the call already left `channel`), and cover the answers the gate has no hook for.
        let join = match given {
            Some(mut join) => {
                join.on_refused(refusal(self.inner.clone()));
                join
            }
            None => match self
                .ring()
                .join_then(
                    channel,
                    community,
                    String::new(),
                    Some(refusal(self.inner.clone())),
                )
                .await
            {
                Ok(Ok(Ok(join))) => join,
                Ok(Ok(Err(block))) => {
                    log::warn!("voice: the join was blocked by its DM gate: {}", block.code);
                    refused(block.code.clone());
                    return Err(ClientError::Sync(block.code));
                }
                Ok(Err(e)) => {
                    log::warn!("voice: the join's gate failed: {e}");
                    refused(e.to_string());
                    return Err(e);
                }
                Err(_) => {
                    log::warn!("voice: the join's gate did not answer (client stopped)");
                    return Err(ClientError::Stopped);
                }
            },
        };
        let mut phase = join.phase();
        let publish_phase = {
            let inner = self.inner.clone();
            async move {
                while phase.changed().await.is_ok() {
                    let p = *phase.borrow_and_update();
                    if p == CallPhase::Idle || p == CallPhase::In {
                        continue;
                    }
                    inner.publish(move |s| {
                        if s.voice.call.channel == Some(channel)
                            && s.voice.call.phase != CallPhase::Idle
                        {
                            s.voice.call.phase = p;
                        }
                    });
                }
                // The join ended (its phase sender went with it): the outcome branch answers.
                std::future::pending::<()>().await;
            }
        };
        let cancel = join.cancel_handle();
        let answer = join.outcome();
        tokio::pin!(answer);
        let outcome = tokio::select! {
            o = &mut answer => Some(o),
            _ = abort_rx => None,
            () = publish_phase => None,
        };
        if outcome.is_none() {
            // Left (or another join) while joining: the join is cancelled, and its own task leaves what it holds (a
            // call it started, its group: `leave_if_cancelled`) before it answers. That answer is awaited, so a leave
            // waiting on this join answers only after the teardown (controller.ts `leave`: `await joining.done`).
            cancel();
            let _ = answer.await;
        }
        {
            let mut r = lock(&rt);
            if r.generation == generation {
                r.joining = None;
                // Settled: a later leave has nothing of this join to wait for.
                r.join_done = None;
            }
        }
        let joined = match outcome {
            // Left (or another join) while joining: cancelled and torn down above.
            None => return Err(ClientError::Sync("E_CANCELLED".into())),
            Some(Err(e)) => {
                log::warn!("voice: the join's task failed: {e}");
                refused(e.to_string());
                return Err(e);
            }
            Some(Ok(Err(r))) => {
                // join.rs logged the step, status and code that refused it.
                refused(r.code.clone());
                return Err(ClientError::Sync(r.code));
            }
            Some(Ok(Ok(j))) => j,
        };
        let live = Live {
            channel,
            call_id: joined.started.call_id,
            group_id: joined.group_id,
        };
        let first = Arc::new(joined.media);
        let (epochs_tx, epochs) = watch::channel(Some(first.clone()));
        let link = Arc::new(MediaLink {
            channel,
            call_id: live.call_id,
            group_id: live.group_id,
            url: joined.started.livekit_url.clone(),
            token: joined.started.token.clone(),
            ice_servers: joined.started.ice_servers.clone(),
            epochs,
        });
        let current = {
            let mut r = lock(&rt);
            let current = r.generation == generation;
            if current {
                r.live = Some(live.clone());
                r.link.send_replace(Some(link));
            }
            current
        };
        if !current {
            // A leave raced the outcome: the call is left at once.
            let _ = self
                .calls
                .leave(live.group_id, live.call_id, joined.started.created)
                .await;
            return Err(ClientError::Sync("E_CANCELLED".into()));
        }
        let (epoch, code, roster) = epoch_fields(&first);
        let call_id = live.call_id;
        let group_id = live.group_id;
        self.inner.publish(move |s| {
            s.voice.call = OwnCall {
                phase: CallPhase::In,
                channel: Some(channel),
                community,
                call_id: Some(call_id),
                group_id: Some(group_id),
                epoch: Some(epoch),
                code: Some(code),
                error: None,
                roster,
                ping_ms: None,
                rate_kbps: None,
            };
        });
        // Product walk M5: a call started muted or deafened says so to the others at once.
        let flags = self.inner.snapshot().voice.self_flags();
        if flags != 0 {
            self.voice_self(call_id, flags);
        }
        self.follow_epochs(generation, live, epochs_tx, first.epoch);
        let _ = self.load_channel(channel).await;
        Ok(())
    }

    /// Reads the call group's keys again as its epoch moves, handing each new epoch to the media layer.
    fn follow_epochs(
        &self,
        generation: u64,
        live: Live,
        epochs: watch::Sender<Option<Arc<MediaEpoch>>>,
        mut last: u64,
    ) {
        let this = self.clone();
        drop(self.inner.spawn(async move {
            loop {
                tokio::time::sleep(EPOCH_POLL).await;
                if this.inner.engine().stopped() {
                    // The client stopped: the media goes with it.
                    let rt = this.rt();
                    let r = lock(&rt);
                    if r.generation == generation {
                        r.link.send_replace(None);
                    }
                    return Ok(());
                }
                if lock(&this.rt()).generation != generation {
                    return Ok(());
                }
                let judged = match this.calls.media_epoch(&live.group_id) {
                    // A leaf verdict is judged against the cached signed list of the leaf's user, which predates a
                    // device that user added since (a known contact's new device joining the call). Its leaf is
                    // unlisted only until a trust pass reads the newer list, which the engine starts on the epoch's
                    // apply but this poll can outrun. So, as controller.ts `sendWithTrustRetry` answers the same
                    // verdict, the group's leaves are read again and the gate judges once more; the core alone
                    // decides that second answer, and a device the fresh list leaves out or revokes still refuses.
                    // The media keys are dropped first, so nothing is sealed or opened while the refresh runs.
                    Err(e) if is_leaf_verdict(&e.code) => {
                        if epochs.borrow().is_some() {
                            epochs.send_replace(None);
                        }
                        let _ = tokio::time::timeout(
                            crate::calls::session::WAIT_LIST,
                            this.inner.engine().refresh_group_trust(live.group_id),
                        )
                        .await;
                        if this.inner.engine().stopped()
                            || lock(&this.rt()).generation != generation
                        {
                            continue;
                        }
                        this.calls.media_epoch(&live.group_id)
                    }
                    other => other,
                };
                match judged {
                    Ok(m) if hands_on(m.epoch, last, epochs.borrow().is_none()) => {
                        last = m.epoch;
                        let m = Arc::new(m);
                        let (epoch, code, roster) = epoch_fields(&m);
                        epochs.send_replace(Some(m));
                        this.inner.publish(move |s| {
                            if s.voice.call.group_id == Some(live.group_id) {
                                s.voice.call.epoch = Some(epoch);
                                s.voice.call.code = Some(code);
                                s.voice.call.roster = roster;
                                s.voice.call.error = None;
                            }
                        });
                    }
                    Ok(_) => {}
                    // controller.ts `onGroupChanged`: this device's own leaf is gone (another member committed its
                    // Remove: a voice takeover, the instance's leave report) or the group's state is lost. A call group
                    // is never resynced, so the call ends here as removed, with no self-Remove.
                    Err(_) if this.calls.own_leaf_gone(&live.group_id) => {
                        this.removed(generation, live).await;
                        return Ok(());
                    }
                    Err(e) => {
                        // WB security I1 (session.ts `dropKeys`): the gate refused the group's newest epoch (an
                        // identity change, a stale or unlisted device list). Every media key is dropped (the epochs
                        // turn `None`), so nothing is sealed under the old epoch and a removed member stops hearing
                        // this device; the next epoch the gate passes is handed on. The error stands until then; the
                        // ring area (Task 18) owns the paused flows.
                        if epochs.borrow().is_some() {
                            epochs.send_replace(None);
                        }
                        let code = e.code;
                        // controller.ts `rekey`: a changed identity (E_UMK_CHANGED) pauses this device, which stays in the
                        // call; any other trust block (unlisted, revoked, credential, tier, binding) blocks the call and
                        // the device leaves, the block recorded for the stage's notice (Task 18, RV-3).
                        if refused_epoch(&code) == RefusedEpoch::Leave {
                            let participants: Vec<UserId> = this
                                .inner
                                .snapshot()
                                .voice
                                .participants(live.channel)
                                .iter()
                                .map(|p| p.user_id)
                                .collect();
                            let recorded = this.ring().record_block(
                                live.channel,
                                code,
                                crate::state::ring::BlockWhen::Left,
                                participants,
                            );
                            let _ = recorded.await;
                            let _ = this.leave_inner().await;
                            return Ok(());
                        }
                        this.inner.publish(move |s| {
                            if s.voice.call.group_id == Some(live.group_id) {
                                s.voice.call.error = Some(code);
                            }
                        });
                    }
                }
            }
        }));
    }

    /// Leaves the call (or stops the join in flight). The media goes first: the link turns `None` at once, so the media
    /// layer stops sealing and leaves the room, then this device's self-Remove is posted, and the call ends when this
    /// device was alone in it. (The web posts the self-Remove before it disconnects; here the keys stop first, so
    /// nothing is sent while the Remove is in flight. The instance's own leave report covers a Remove that fails.)
    ///
    /// Review HL I2: it answers `Ok` only once the instance acknowledged the leave (it took the self-Remove, a Remove
    /// of this leaf stands there already, or it ended the call this device was alone in), or when there was no call to
    /// leave; a Remove refused otherwise, or whose answer was lost, answers `E_LEAVE_UNACKNOWLEDGED` (the call is left
    /// here all the same).
    pub fn leave(&self) -> oneshot::Receiver<Result<(), ClientError>> {
        let this = self.clone();
        self.inner.spawn(async move {
            match this.leave_acknowledged().await? {
                true => Ok(()),
                false => Err(ClientError::Sync("E_LEAVE_UNACKNOWLEDGED".into())),
            }
        })
    }

    /// [`Voice::leave`] for a caller that moves on whatever the instance answered (a join's own leave, a trust block,
    /// a lost room).
    async fn leave_inner(&self) -> Result<(), ClientError> {
        self.leave_acknowledged().await.map(|_| ())
    }

    /// The leave; answers whether the instance acknowledged it (no call to leave: yes).
    async fn leave_acknowledged(&self) -> Result<bool, ClientError> {
        let rt = self.rt();
        let (live, joining) = {
            let mut r = lock(&rt);
            r.generation += 1;
            r.link.send_replace(None);
            r.last_self = None;
            r.self_offline = None;
            r.resends = None;
            r.own_in_call.clear();
            (r.live.take(), (r.joining.take(), r.join_done.take()))
        };
        let (joining, join_done) = joining;
        drop(joining);
        let own = self.inner.snapshot().account.device_id;
        let snap = self.inner.snapshot();
        self.inner.publish(move |s| {
            if let (Some(ch), Some(d)) = (s.voice.call.channel, own) {
                s.voice.drop_device(ch, d);
            }
            s.voice.call = OwnCall::default();
            s.voice.speaking.clear();
            s.voice.share = LocalShare::default();
        });
        // A join in flight was cancelled just above: this answers once it has torn down (its self-Remove posted, a call
        // it started left), as controller.ts `leave` awaits `joining.done`.
        if let Some(done) = join_done {
            let _ = done.await;
        }
        let Some(live) = live else {
            return Ok(true);
        };
        let alone = own.is_some_and(|d| {
            snap.voice
                .participants(live.channel)
                .iter()
                .all(|p| p.device_id == d)
        });
        let left = self
            .calls
            .leave(live.group_id, live.call_id, alone)
            .await
            .map_err(|_| ClientError::Stopped)??;
        Ok(left.proposed || left.ended)
    }

    /// session.ts `onRoomEnd` → `leave('lost')`: the call's LiveKit room ended on its own (a newer device of this user
    /// took voice over, the room closed, the connection gave up for good). The call is left as a leave leaves it (its
    /// self-Remove posted; one the instance already holds is fine), `call.error` says why (`code`: `E_REMOVED`,
    /// `E_CALL_ENDED`, `E_DISCONNECTED`), and the community's AFK snapshot is read again, so a takeover's notice ("Voice
    /// moved to your other device.") shows. Only the call `call` ends: a newer call of this device is left alone.
    pub fn lost(&self, call: [u8; 16], code: &str) -> oneshot::Receiver<Result<(), ClientError>> {
        let this = self.clone();
        let code = code.to_owned();
        self.inner.spawn(async move {
            let current = lock(&this.rt())
                .live
                .as_ref()
                .is_some_and(|l| l.call_id == call);
            if !current {
                return Ok(());
            }
            let community = this.inner.snapshot().voice.call.community;
            let _ = this.leave_inner().await;
            this.after_end(community, Some(code)).await;
            Ok(())
        })
    }

    /// controller.ts `lost`: this device's own leaf is gone from the call group of `live` (generation `generation`), so
    /// the call ends with no self-Remove: the media's link goes, the group is forgotten, the own entry leaves the room,
    /// and the AFK snapshot is read again (a takeover's notice).
    async fn removed(&self, generation: u64, live: Live) {
        {
            let rt = self.rt();
            let mut r = lock(&rt);
            if r.generation != generation {
                return;
            }
            r.generation += 1;
            r.link.send_replace(None);
            r.live = None;
            r.last_self = None;
            r.self_offline = None;
            r.resends = None;
            r.own_in_call.clear();
        }
        self.calls.forget(&live.group_id);
        let snap = self.inner.snapshot();
        let (own, community) = (snap.account.device_id, snap.voice.call.community);
        self.inner
            .publish(move |s| ended_removed(s, live.channel, own));
        self.after_end(community, None).await;
    }

    /// After a call ended on its own: its reason stands in `call.error` (unless a newer call started meanwhile), and the
    /// community's AFK snapshot is read again, so the takeover or AFK move it announces shows.
    async fn after_end(&self, community: Option<CommunityId>, error: Option<String>) {
        if error.is_some() {
            self.inner.publish(move |s| {
                if s.voice.call.phase == CallPhase::Idle {
                    s.voice.call.error = error;
                }
            });
        }
        if let Some(c) = community {
            let _ = self.ring().load_afk(c).await;
        }
    }

    /// controller.ts `voiceSelf`: kept as the last one for the call (before the online check, so the re-send rule
    /// covers a press made offline), and sent only while online; one made offline is owed to the reconnect.
    fn voice_self(&self, call: [u8; 16], flags: u8) {
        let online =
            self.inner.snapshot().connection.status == crate::state::ConnectionStatus::Online;
        let rt = self.rt();
        let mut r = lock(&rt);
        r.last_self = Some((call, flags));
        if !online {
            r.self_offline = Some((call, VOICE_SELF_RECONNECT_SENDS));
            return;
        }
        r.self_offline = None;
        drop(r);
        send_voice_self(&self.inner, call, flags);
    }

    /// controller.ts `sendOfflineSelf`: the reconnect sends the `voice_self` made while offline, for the call it was
    /// made in, at most [`VOICE_SELF_RECONNECT_SENDS`] times.
    fn send_offline_self(&self) {
        let rt = self.rt();
        let mut r = lock(&rt);
        let (Some((owed_call, left)), Some((call, flags))) = (r.self_offline, r.last_self) else {
            r.self_offline = None;
            return;
        };
        if owed_call != call || left == 0 {
            r.self_offline = None;
            return;
        }
        r.self_offline = (left > 1).then_some((call, left - 1));
        drop(r);
        send_voice_self(&self.inner, call, flags);
    }

    /// The flags changed: the call this device is in is told (`voice_self`).
    fn self_changed(&self) {
        let rt = self.rt();
        let call = lock(&rt).live.as_ref().map(|l| l.call_id);
        let inner = self.inner.clone();
        let this = self.clone();
        // The flags are read after the publisher applied the press.
        inner.publish(move |s| {
            if let Some(call) = call
                && s.voice.call.phase == CallPhase::In
            {
                this.voice_self(call, s.voice.self_flags());
            }
        });
    }

    /// Mutes or unmutes this device's microphone in the current call (and before one: a join starts muted). Unmuting
    /// a deafened device undeafens it too (session.ts `setMuted`). The user panel (Task 8) calls it.
    pub fn set_muted(&self, muted: bool) {
        self.inner.publish(move |s| {
            s.voice.self_mute = muted;
            if !muted {
                s.voice.self_deaf = false;
            }
        });
        self.self_changed();
    }

    /// Deafens or undeafens this device. A deafened device is muted too; undeafening restores the mute it had before
    /// (session.ts `setDeafened`). The user panel (Task 8) calls it.
    pub fn set_deafened(&self, deafened: bool) {
        self.inner.publish(move |s| {
            s.voice.self_deaf = deafened;
        });
        self.self_changed();
    }

    /// Push to talk is held (`true`) or released.
    pub fn set_ptt_held(&self, held: bool) {
        self.inner.publish(move |s| s.voice.ptt_held = held);
    }

    /// The devices heard speaking now (the media layer's speaking detector).
    pub fn set_speaking(&self, devices: std::collections::BTreeSet<DeviceId>) {
        self.inner.publish(move |s| {
            if s.voice.speaking != devices {
                s.voice.speaking = devices;
            }
        });
    }

    /// The link's ping the media read for the call `call` (`None`: none measured now). A call left meanwhile, or
    /// another call, takes nothing.
    pub fn set_ping(&self, call: [u8; 16], ms: Option<u32>) {
        self.inner.publish(move |s| {
            let c = &mut s.voice.call;
            if c.call_id == Some(call) && c.phase == CallPhase::In && c.ping_ms != ms {
                c.ping_ms = ms;
            }
        });
    }

    /// The link's rate, `(up, down)` kb/s (session.ts `linkTick`'s sample), for the dock and the call bar; ignored unless it
    /// is this device's live call.
    pub fn set_rate(&self, call: [u8; 16], kbps: Option<(u32, u32)>) {
        self.inner.publish(move |s| {
            let c = &mut s.voice.call;
            if c.call_id == Some(call) && c.phase == CallPhase::In && c.rate_kbps != kbps {
                c.rate_kbps = kbps;
            }
        });
    }

    /// What the media layer reported, or `None` to dismiss it.
    pub fn set_notice(&self, notice: Option<VoiceNotice>) {
        self.inner.publish(move |s| s.voice.notice = notice);
    }

    /// What this device publishes besides its microphone. Starting a camera or screen share asks the instance for a
    /// sharing slot first (`POST /v1/calls/{id}/share`); stopping the last one frees it.
    pub fn set_share(&self, share: LocalShare) -> oneshot::Receiver<Result<(), ClientError>> {
        let this = self.clone();
        self.inner.spawn(async move {
            use crate::http::routes::{NoBody, Raw, ShareCall, UnshareCall};
            let call = lock(&this.rt()).live.as_ref().map(|l| l.call_id);
            let Some(call) = call else {
                return Err(ClientError::Sync("E_NO_CALL".into()));
            };
            let before = this.inner.snapshot().voice.share;
            let was = before.camera || before.screen;
            let now = share.camera || share.screen;
            let http = &this.calls.routes().0;
            let hex = crate::state::hex(&call);
            if now && !was {
                http.call::<ShareCall>(&[&hex], Raw(vec![0x80]))
                    .await
                    .map_err(|e| ClientError::Sync(code_of(&e)))?;
            } else if !now && was {
                let _ = http.call::<UnshareCall>(&[&hex], NoBody).await;
            }
            this.inner.publish(move |s| s.voice.share = share);
            Ok(())
        })
    }

    /// controller.ts `rtt` (Signal task 8c): this device's RTT to the SFU in ms (0..=5000, clamped), or `None` to clear it,
    /// for the others' per-person ping: `POST /v1/calls/{id}/rtt` `[ms]` or `[]`, which the instance writes as this
    /// device's `dilla.rtt` attribute (no call token may write its own attributes). `true` once stored; `false` when the
    /// instance refused it by rate (429) or by room (404, a reconnect not landed yet), so the caller does not count it as
    /// sent and its next sample tries again. `E_NO_CALL` with no call.
    pub fn report_rtt(&self, ms: Option<u32>) -> oneshot::Receiver<Result<bool, ClientError>> {
        let this = self.clone();
        self.inner.spawn(async move {
            use crate::http::routes::{PostCallRtt, Raw};
            let call = lock(&this.rt()).live.as_ref().map(|l| l.call_id);
            let Some(call) = call else {
                return Err(ClientError::Sync("E_NO_CALL".into()));
            };
            let body = match ms {
                Some(ms) => Value::Array(vec![Value::Uint(u64::from(ms.min(RTT_MAX_MS)))]).encode(),
                None => vec![0x80],
            };
            let hex = crate::state::hex(&call);
            match this
                .calls
                .routes()
                .0
                .call::<PostCallRtt>(&[&hex], Raw(body))
                .await
            {
                Ok(()) => Ok(true),
                Err(crate::seams::HttpError::Status {
                    status: 429 | 404, ..
                }) => Ok(false),
                Err(e) => Err(ClientError::Sync(code_of(&e))),
            }
        })
    }

    /// controller.ts `stats`: one coarse report of the call (op 58), at most every [`STATS_EVERY`]; `false` when it was
    /// not sent (no call, throttled, offline).
    pub fn send_stats(&self, report: CallStatsReport) -> bool {
        let rt = self.rt();
        let mut r = lock(&rt);
        let Some(call) = r.live.as_ref().map(|l| l.call_id) else {
            return false;
        };
        if r.last_stats.is_some_and(|t| t.elapsed() < STATS_EVERY) {
            return false;
        }
        let sent =
            stats_frame(call, &report).is_ok_and(|b| self.inner.engine().send_gateway_frame(b));
        if sent {
            r.last_stats = Some(std::time::Instant::now());
        }
        sent
    }
}

/// The gateway frame of one coarse report of `call` (op 58, `n` 0: a fire-and-forget frame on the open socket, never a
/// request and never HTTP).
pub fn stats_frame(
    call: [u8; 16],
    report: &CallStatsReport,
) -> Result<Vec<u8>, crate::gateway::frames::FrameError> {
    crate::gateway::frames::encode(&Frame {
        op: Op::CALL_STATS,
        n: 0,
        group_id: None,
        payload: vec![
            Value::Bytes(call.to_vec()),
            Value::Array(vec![
                Value::Uint(report.candidate_type),
                report.relay_protocol.map_or(Value::Null, Value::Uint),
                Value::Uint(report.rtt_ms),
                Value::Uint(report.fraction_lost_permille),
                Value::Uint(report.decrypt_failures),
                Value::Uint(report.frames_encrypted),
            ]),
        ],
    })
}

fn code_of(e: &crate::seams::HttpError) -> String {
    match e {
        crate::seams::HttpError::Status {
            code: Some(code), ..
        } => code.clone(),
        other => other.to_string(),
    }
}

/// What a call does when the gate refuses its group's newest epoch mid-call (controller.ts `rekey`).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum RefusedEpoch {
    /// The media keys are dropped and the call stays, its error shown, until an epoch passes again (ruling R24): a
    /// changed identity (`E_UMK_CHANGED`) and any refusal that is not a trust block.
    Pause,
    /// A trust block other than a changed identity (unlisted, revoked, credential, tier, binding): the block is recorded
    /// for the stage's notice (`BlockWhen::Left`) and this device leaves.
    Leave,
}

/// controller.ts `rekey`'s choice for a refused epoch's `code`.
pub fn refused_epoch(code: &str) -> RefusedEpoch {
    if code != "E_UMK_CHANGED" && crate::calls::join::is_trust_block(code) {
        RefusedEpoch::Leave
    } else {
        RefusedEpoch::Pause
    }
}

/// Whether a refused epoch's `code` is one of the core's leaf verdicts (errors.ts `LEAF_VERDICTS`): judged against a
/// cached signed list, so the follower reads the group's leaves again before it acts on it.
pub fn is_leaf_verdict(code: &str) -> bool {
    crate::sync::trust::LEAF_VERDICTS.contains(&code)
}

/// Whether an epoch the gate passed goes to the media: a newer one, or (ruling R24) the same one again after a refusal
/// suspended it (the person re-trusted with no commit between), which resumes the call.
pub fn hands_on(epoch: u64, last: u64, suspended: bool) -> bool {
    epoch > last || (suspended && epoch == last)
}

fn epoch_fields(m: &MediaEpoch) -> (u64, String, Vec<(UserId, DeviceId, u16)>) {
    (
        m.epoch,
        m.code.clone(),
        m.roster
            .iter()
            .map(|(leaf, device, user)| (UserId(*user), DeviceId(*device), *leaf))
            .collect(),
    )
}

/// The state a call whose own leaf is gone ends in (controller.ts `lost`: `phase: 'removed'`): idle, with the removal
/// said in `call.error` (`E_REMOVED`, the code a takeover's lost room says too) in the same snapshot, so the stage never
/// shows a silent idle; the own entry leaves the room, and the speaking set and the share go.
fn ended_removed(s: &mut AppState, channel: ChannelId, own: Option<DeviceId>) {
    if let Some(d) = own {
        s.voice.drop_device(channel, d);
    }
    s.voice.call = OwnCall {
        error: Some("E_REMOVED".into()),
        ..OwnCall::default()
    };
    s.voice.speaking.clear();
    s.voice.share = LocalShare::default();
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::gateway::frames::Value as V;

    fn frame(user: u8, device: u8, call: [u8; 16], flags: u64) -> Frame {
        Frame {
            op: Op::VOICE_STATE,
            n: 0,
            group_id: None,
            payload: vec![
                V::Bytes(vec![user; 16]),
                V::Bytes(vec![device; 16]),
                V::Bytes(call.to_vec()),
                V::Uint(flags),
            ],
        }
    }

    /// w2-fix2 re-review (controller.ts `lost`: `phase: 'removed'`): a call whose own leaf is gone ends idle with the
    /// removal said (`E_REMOVED`) in the same snapshot, never as a silent idle; the own entry, the speaking set and the
    /// share go with it.
    #[test]
    fn a_removed_call_ends_saying_so() {
        let ch = ChannelId([7; 16]);
        let own = DeviceId([2; 16]);
        let mut s = AppState::default();
        s.voice.call = OwnCall {
            phase: CallPhase::In,
            channel: Some(ch),
            call_id: Some([9; 16]),
            ..OwnCall::default()
        };
        s.voice.share.camera = true;
        ended_removed(&mut s, ch, Some(own));
        assert_eq!(s.voice.call.phase, CallPhase::Idle);
        assert_eq!(s.voice.call.error.as_deref(), Some("E_REMOVED"));
        assert_eq!(s.voice.call.call_id, None);
        assert!(!s.voice.share.camera);
        assert!(s.voice.participants(ch).iter().all(|p| p.device_id != own));
    }

    fn state_with_channel(ch: [u8; 16]) -> AppState {
        let mut s = AppState::default();
        s.channels.push(crate::state::Channel {
            id: ChannelId(ch),
            community_id: CommunityId([1; 16]),
            kind: 1,
            mode: 0,
            name: "Lounge".into(),
            topic: String::new(),
            parent_id: None,
            position: 0,
            slowmode: 0,
            text_group_id: None,
            group: crate::state::ChannelGroupState::Unsupported,
        });
        s
    }

    #[test]
    fn a_frame_for_a_known_channel_lands_there_and_marks_the_own_device() {
        let mut s = state_with_channel([4; 16]);
        apply_frame(&mut s, &frame(1, 1, [4; 16], 1), Some([1; 16]));
        apply_frame(&mut s, &frame(2, 2, [4; 16], 3), Some([1; 16]));
        let rows = s.voice.participants(ChannelId([4; 16]));
        assert_eq!(rows.len(), 2);
        assert!(rows[0].own && !rows[1].own);
        assert!(rows[1].muted());
    }

    #[test]
    fn a_frame_for_an_unknown_call_is_dropped() {
        let mut s = state_with_channel([4; 16]);
        apply_frame(&mut s, &frame(1, 1, [9; 16], 1), None);
        assert!(s.voice.calls.is_empty());
    }

    #[test]
    fn community_calls_decode_and_refuse_bad_flags() {
        let ok = V::Array(vec![V::Array(vec![
            V::Bytes(vec![4; 16]),
            V::Bytes(vec![4; 16]),
            V::Array(vec![V::Array(vec![
                V::Bytes(vec![1; 16]),
                V::Bytes(vec![2; 16]),
                V::Uint(17),
            ])]),
        ])]);
        let got = decode_community_calls(&ok.encode()).unwrap();
        assert_eq!(got.len(), 1);
        assert_eq!(got[0].2[0].flags, 17);
        let bad = V::Array(vec![V::Array(vec![
            V::Bytes(vec![4; 16]),
            V::Bytes(vec![4; 16]),
            V::Array(vec![V::Array(vec![
                V::Bytes(vec![1; 16]),
                V::Bytes(vec![2; 16]),
                V::Uint(32),
            ])]),
        ])]);
        assert!(decode_community_calls(&bad.encode()).is_err());
    }

    /// Review I3 (controller.ts `rekey`): a changed identity pauses the call and the device stays; every other trust block
    /// (core-port.ts `TRUST_BLOCKS`, E_BINDING included) leaves it; any other refusal keeps R24's suspend and resume.
    #[test]
    fn a_refused_epoch_pauses_on_a_changed_identity_and_leaves_on_any_other_trust_block() {
        assert_eq!(refused_epoch("E_UMK_CHANGED"), RefusedEpoch::Pause);
        for code in [
            "E_CREDENTIAL",
            "E_DEVICE_UNLISTED",
            "E_DEVICE_REVOKED",
            "E_TIER_MISMATCH",
            "E_BINDING",
        ] {
            assert_eq!(refused_epoch(code), RefusedEpoch::Leave, "{code}");
        }
        for code in ["E_NETWORK", "E_EPOCH_STALE", "E_CORE_DECODE", ""] {
            assert_eq!(refused_epoch(code), RefusedEpoch::Pause, "{code}");
        }
    }

    /// A leaf verdict (judged against a cached list) reads the group's leaves again before the follower acts on it; a
    /// changed identity, a binding refusal, a pending leaf or any other code is acted on as it stands.
    #[test]
    fn only_a_leaf_verdict_refreshes_the_leaves_before_judging_again() {
        for code in [
            "E_DEVICE_UNLISTED",
            "E_DEVICE_REVOKED",
            "E_CREDENTIAL",
            "E_TIER_MISMATCH",
        ] {
            assert!(is_leaf_verdict(code), "{code}");
        }
        for code in [
            "E_UMK_CHANGED",
            "E_BINDING",
            "E_DEVICE_LIST_STALE",
            "E_NETWORK",
            "",
        ] {
            assert!(!is_leaf_verdict(code), "{code}");
        }
    }

    /// Ruling R24: a refusal, then the same epoch passing again, resumes; an older one never does.
    #[test]
    fn the_same_epoch_after_a_refusal_resumes_the_call() {
        assert!(hands_on(5, 4, false), "a newer epoch");
        assert!(
            !hands_on(4, 4, false),
            "the same epoch while running changes nothing"
        );
        assert!(
            hands_on(4, 4, true),
            "the same epoch after a refusal resumes"
        );
        assert!(!hands_on(3, 4, true), "an older one never");
    }

    #[test]
    fn the_media_link_debug_shows_no_token() {
        let (_tx, epochs) = watch::channel(None);
        let link = MediaLink {
            channel: ChannelId([1; 16]),
            call_id: [1; 16],
            group_id: [2; 16],
            url: "wss://sfu".into(),
            token: Zeroizing::new("secret-token".into()),
            ice_servers: vec![(
                vec!["turn:x".into()],
                "u".into(),
                Zeroizing::new("cred".into()),
            )],
            epochs,
        };
        let shown = format!("{link:?}");
        assert!(
            !shown.contains("secret-token") && !shown.contains("cred"),
            "{shown}"
        );
    }
}
