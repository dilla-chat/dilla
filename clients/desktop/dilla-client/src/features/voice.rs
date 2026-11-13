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

use crate::calls::{CallPhase, CallTarget, Calls, MediaEpoch};
use crate::client::{Client, ClientError, Inner};
use crate::gateway::frames::{Frame, Op, Value, voice_state_of};
use crate::seams::Http;
use crate::state::voice::{LocalShare, OwnCall, VoiceNotice, VoiceParticipant};
use crate::state::{AppState, ChannelId, CommunityId, DeviceId, UserId};
use crate::sync::route::AreaFrames;

/// controller.ts `VOICE_SELF_RESENDS`: how often per join the own `voice_self` is sent again while the instance's
/// `voice_state` lacks it.
pub const VOICE_SELF_RESENDS: u8 = 3;
/// routes.ts `COMMUNITY_CALLS_MAX`.
pub const COMMUNITY_CALLS_MAX: usize = 4096;
/// stats.ts `STATS_EVERY_MS`: one coarse report per call every 10 s, at most.
pub const STATS_EVERY: Duration = Duration::from_secs(10);
/// How often a joined call's epoch keys are read again from the core (the engine applies the group's commits).
pub const EPOCH_POLL: Duration = Duration::from_millis(250);

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
    live: Option<Live>,
    link: watch::Sender<Option<Arc<MediaLink>>>,
    /// The last `voice_self` sent: `(call, flags)`.
    last_self: Option<([u8; 16], u8)>,
    own_in_call: HashSet<[u8; 16]>,
    resends: Option<([u8; 16], u8)>,
    last_stats: Option<std::time::Instant>,
}

impl Default for Runtime {
    fn default() -> Self {
        Self {
            reads: Vec::new(),
            started: false,
            generation: 0,
            joining: None,
            live: None,
            link: watch::channel(None).0,
            last_self: None,
            own_in_call: HashSet::new(),
            resends: None,
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

/// routes.ts `getCommunityCalls`: `[[channel_id, call_id, [[user, device, flags], ...]], ...]`.
pub fn decode_community_calls(
    body: &[u8],
) -> Result<Vec<(ChannelId, [u8; 16], Vec<crate::calls::CallParticipant>)>, String> {
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
        self.inner.spawn(async move {
            let community = this
                .inner
                .snapshot()
                .channel(channel)
                .map(|c| c.community_id)
                .ok_or(ClientError::UnknownChannel)?;
            this.join_inner(channel, Some(community)).await
        })
    }

    /// Joins a DM's or group DM's call (no community). Task 18's ring and DM call flows use it.
    pub fn join_dm(&self, channel: ChannelId) -> oneshot::Receiver<Result<(), ClientError>> {
        let this = self.clone();
        self.inner
            .spawn(async move { this.join_inner(channel, None).await })
    }

    async fn join_inner(
        &self,
        channel: ChannelId,
        community: Option<CommunityId>,
    ) -> Result<(), ClientError> {
        let snap = self.inner.snapshot();
        if snap.voice.call.channel == Some(channel) && snap.voice.in_call() {
            return Ok(());
        }
        if snap.voice.in_call() {
            self.leave_inner().await?;
        }
        let rt = self.rt();
        let (abort_tx, abort_rx) = oneshot::channel::<()>();
        let generation = {
            let mut r = lock(&rt);
            r.generation += 1;
            r.joining = Some(abort_tx);
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
        let join = self.calls.join(CallTarget {
            channel: channel.0,
            community: community.map(|c| c.0),
            vdec: String::new(),
        });
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
        let outcome = tokio::select! {
            o = join.outcome() => Some(o),
            _ = abort_rx => None,
            () = publish_phase => None,
        };
        {
            let mut r = lock(&rt);
            if r.generation == generation {
                r.joining = None;
            }
        }
        let refused = |code: String| {
            self.inner.publish(move |s| {
                if s.voice.call.channel == Some(channel) {
                    s.voice.call = OwnCall {
                        error: Some(code),
                        ..OwnCall::default()
                    };
                }
            });
        };
        let joined = match outcome {
            // Left (or another join) while joining: dropping the join cancelled it.
            None => return Err(ClientError::Sync("E_CANCELLED".into())),
            Some(Err(e)) => {
                refused(e.to_string());
                return Err(e);
            }
            Some(Ok(Err(r))) => {
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
                if lock(&this.rt()).generation != generation || this.inner.engine().stopped() {
                    return Ok(());
                }
                match this.calls.media_epoch(&live.group_id) {
                    Ok(m) if m.epoch > last => {
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
                    Err(e) => {
                        // The gate refused the epoch (an identity change, a stale device list): the error stands
                        // until the next epoch passes; the ring area (Task 18) owns the paused flows.
                        let code = e.code;
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

    /// Leaves the call (or stops the join in flight): this device's self-Remove first, then the media goes (its link
    /// turns `None`), and the call ends when this device was alone in it.
    pub fn leave(&self) -> oneshot::Receiver<Result<(), ClientError>> {
        let this = self.clone();
        self.inner.spawn(async move { this.leave_inner().await })
    }

    async fn leave_inner(&self) -> Result<(), ClientError> {
        let rt = self.rt();
        let (live, joining) = {
            let mut r = lock(&rt);
            r.generation += 1;
            r.link.send_replace(None);
            r.last_self = None;
            (r.live.take(), r.joining.take())
        };
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
        let Some(live) = live else {
            return Ok(());
        };
        let alone = own.is_some_and(|d| {
            snap.voice
                .participants(live.channel)
                .iter()
                .all(|p| p.device_id == d)
        });
        self.calls
            .leave(live.group_id, live.call_id, alone)
            .await
            .map_err(|_| ClientError::Stopped)??;
        Ok(())
    }

    /// controller.ts `voiceSelf`: kept as the last one for the call, sent when online.
    fn voice_self(&self, call: [u8; 16], flags: u8) {
        lock(&self.rt()).last_self = Some((call, flags));
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
        let frame = Frame {
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
        };
        let sent = crate::gateway::frames::encode(&frame)
            .is_ok_and(|b| self.inner.engine().send_gateway_frame(b));
        if sent {
            r.last_stats = Some(std::time::Instant::now());
        }
        sent
    }
}

fn code_of(e: &crate::seams::HttpError) -> String {
    match e {
        crate::seams::HttpError::Status {
            code: Some(code), ..
        } => code.clone(),
        other => other.to_string(),
    }
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
