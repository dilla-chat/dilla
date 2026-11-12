//! The sync engine: a port of `packages/client-core/src/sync/{engine,channel,send,joinall}.ts` over
//! `dilla_core::client::ClientCore`. The core decides and persists (decryption, seq order, de-duplication, the outbox);
//! the engine transports, in the same order and with the same recoveries as the TS engine:
//!
//! - **Live frames** (ops 16 and 19) are applied in seq order on the group's lane: a seq below the group's `next_seq`
//!   is a duplicate and dropped, a seq above it is a gap and starts a catch-up instead, and only the expected seq is
//!   applied. A frame for a group still registering or joining is held (at most 512) until it is active.
//! - **Catch-up** reads messages and handshakes from `next_seq` in pages and applies them up to [`timeline::through_of`],
//!   until both pages come back short. It runs on every ready for every active group (after the heal check) and on
//!   every gap, so a device that was away gets exactly what it missed, once, in order.
//! - **Sends** go through the core's outbox (`send_prepare` → `send_encrypt` → `POST message` → `send_confirm`), one
//!   at a time per group, with send.ts's answers to a lost response (catch up, then wait for the echo before a resend),
//!   429 (slow mode), 425 (a commit is due), 422 at a stale epoch (re-frame once), 403 (resync) and 401.
//! - **Membership**: Welcomes (ready, op 20, before every join), external joins of every expected group (join-all) and
//!   of an opened channel, registration of a channel's first group, resync of a lost group.
//! - **Commits** of pending proposals on `commit.needed` (op 17) or after the instance's backoff when this device
//!   volunteers.
//!
//! Every heavy step (MLS, SQLCipher) runs on the blocking pool ([`api::with_core`]); the gateway's event loop
//! ([`Engine::run_events`]) only decodes and hands work to lanes, so a slow decrypt never stalls the event stream (the
//! gateway tears a connection down when an event is left untaken for its ack timeout).
//!
//! Not ported in this task (see the report): the moderation-removal feed recovery (removals.ts) that runs before a
//! community group's timeline is published, attachments (`beforeSend`), DMs, calls and Bot groups.

pub mod api;
pub mod lanes;
pub mod requests;
pub mod timeline;
pub mod trust;
pub mod value;

use std::collections::{HashMap, HashSet};
use std::future::Future;
use std::pin::Pin;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use dilla_core::client::ClientError as CoreError;
use tokio::sync::mpsc;
use tokio::task::AbortHandle;
use tokio::time::Instant;

use crate::gateway::frames::{self, Frame, Op, Value};
use crate::gateway::{ReadyInfo, WsGateway};
use crate::seams::GatewayEvent;
use crate::state::hex;

use api::{Api, ApiError, CoreHandle, InstanceInfo, with_core};
use lanes::Lanes;
use timeline::{
    ApplyResult, HANDSHAKE_PAGE, MESSAGE_PAGE, PageInfo, live_row, through_of, welcome_page,
};
use value::{DecodeError, Rows, arr, bin16, opt, u53, uint};

type BoxFut<T> = Pin<Box<dyn Future<Output = T> + Send + 'static>>;
type Gid = [u8; 16];

/// engine.ts `SYNC`.
pub const COMMIT_RETRY_MAX: u32 = 5;
pub const COMMIT_JITTER_MS: u64 = 400;
pub const MEMBERSHIP_WAIT: Duration = Duration::from_millis(2000);
pub const ECHO_WAIT: Duration = Duration::from_millis(5000);
pub const REGISTER_RETRY_MAX: u32 = 3;
pub const CURSOR_DEBOUNCE: Duration = Duration::from_millis(30_000);
pub const JOIN_ALL_RETRY: Duration = Duration::from_millis(60_000);
/// engine.ts `HELD_MAX`: frames held per group while it registers or joins.
pub const HELD_MAX: usize = 512;

/// errors.ts `LEAF_VERDICTS`.
const LEAF_VERDICTS: &[&str] = &[
    "E_DEVICE_UNLISTED",
    "E_DEVICE_REVOKED",
    "E_CREDENTIAL",
    "E_TIER_MISMATCH",
];
/// errors.ts `FAILED_LEAF`.
const FAILED_LEAF: &[&str] = &[
    "E_DEVICE_UNLISTED",
    "E_DEVICE_REVOKED",
    "E_CREDENTIAL",
    "E_TIER_MISMATCH",
    "E_UMK_CHANGED",
];

/// Why a sync step failed.
#[derive(Debug, Clone, thiserror::Error)]
pub enum SyncError {
    #[error("{0}")]
    Api(ApiError),
    #[error("{0}")]
    Core(CoreError),
    #[error("{0}")]
    Decode(DecodeError),
    #[error("E_SYNC_STOPPED")]
    Stopped,
    #[error("E_REGISTER_RACE")]
    RegisterRace,
    #[error("E_CHANNEL_GONE")]
    ChannelGone,
}

impl SyncError {
    /// The stable code (channel.ts `codeOf`).
    pub fn code(&self) -> String {
        match self {
            Self::Api(e) => e.code.clone(),
            Self::Core(e) => e.code.to_owned(),
            Self::Decode(_) => "E_CORE_DECODE".into(),
            Self::Stopped => "E_SYNC_STOPPED".into(),
            Self::RegisterRace => "E_REGISTER_RACE".into(),
            Self::ChannelGone => "E_CHANNEL_GONE".into(),
        }
    }

    fn api(&self) -> Option<&ApiError> {
        match self {
            Self::Api(e) => Some(e),
            _ => None,
        }
    }
}

impl From<ApiError> for SyncError {
    fn from(e: ApiError) -> Self {
        Self::Api(e)
    }
}
impl From<CoreError> for SyncError {
    fn from(e: CoreError) -> Self {
        Self::Core(e)
    }
}
impl From<DecodeError> for SyncError {
    fn from(e: DecodeError) -> Self {
        Self::Decode(e)
    }
}

/// core-port.ts `GroupInfo`: `[group_id, kind, community_id|null, target_id, state, epoch, next_seq, proposals_pending,
/// pending_commit]`. States: 0 registering, 1 joining, 2 active, 3 needs resync, 4 gone.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct GroupRow {
    pub group_id: Gid,
    pub kind: u64,
    pub community_id: Option<Gid>,
    pub target_id: Gid,
    pub state: u8,
    pub epoch: u64,
    pub next_seq: u64,
    pub proposals_pending: u64,
    pub pending_commit: bool,
}

impl GroupRow {
    fn read(v: &Value) -> Result<Self, DecodeError> {
        let a = arr(v, 9)?;
        let state = u53(&a[4])?;
        if state > 4 {
            return Err(DecodeError("group state"));
        }
        Ok(Self {
            group_id: bin16(&a[0])?,
            kind: u53(&a[1])?,
            community_id: opt(&a[2], bin16)?,
            target_id: bin16(&a[3])?,
            state: state as u8,
            epoch: uint(&a[5])?,
            next_seq: uint(&a[6])?,
            proposals_pending: u53(&a[7])?,
            pending_commit: uint(&a[8])? == 1,
        })
    }

    pub fn decode_all(raw: &[u8]) -> Result<Vec<Self>, DecodeError> {
        Rows::of(raw)?.iter().map(Self::read).collect()
    }

    pub fn decode_one(raw: &[u8]) -> Result<Option<Self>, DecodeError> {
        match value::decode(raw)? {
            Value::Null => Ok(None),
            v => Self::read(&v).map(Some),
        }
    }
}

/// core-port.ts `ExpectedGroup`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct ExpectedGroup {
    pub group_id: Gid,
    pub community_id: Option<Gid>,
    pub channel_id: Gid,
    pub policy_version: u64,
}

fn encode_expected(list: &[ExpectedGroup]) -> Vec<u8> {
    Value::Array(
        list.iter()
            .map(|e| {
                Value::Array(vec![
                    Value::Bytes(e.group_id.to_vec()),
                    e.community_id
                        .map_or(Value::Null, |c| Value::Bytes(c.to_vec())),
                    Value::Bytes(e.channel_id.to_vec()),
                    Value::Uint(e.policy_version),
                ])
            })
            .collect(),
    )
    .encode()
}

/// What a membership report says (engine.ts `onMembership`).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Membership {
    Resyncing,
    NotMember,
    Member,
}

/// What the engine tells the client model (engine.ts `SyncDeps` callbacks). Each must return at once: the model does
/// its own reads elsewhere.
pub trait SyncHooks: Send + Sync {
    /// A group's rows or its outbox changed (`onGroupChanged`, `onOutboxChanged`).
    fn group_changed(&self, group: Gid);
    /// `onMembership`.
    fn membership(&self, group: Gid, status: Membership);
    /// A leaf's trust verdict may have changed (`onTrustChanged`).
    fn trust_changed(&self);
    /// The gateway's ready, its loss and its refusals.
    fn connection(&self, event: ConnectionEvent);
    /// `community.changed` (op 54): the community's structure changed; the model reads it again.
    fn community_changed(&self, community: Gid);
}

/// The gateway's lifecycle as the model sees it.
#[derive(Clone, Debug)]
pub enum ConnectionEvent {
    Ready(Box<ReadyInfo>),
    Down,
    /// The gateway stopped for good, with the close code (or 401 for a refused upgrade).
    Refused(crate::gateway::Refusal, u16),
}

#[derive(Default)]
struct JoinAllState {
    running: bool,
    again: bool,
    retry: Option<AbortHandle>,
}

#[derive(Default)]
struct St {
    ready: Option<ReadyInfo>,
    /// The hello's backoff (volunteer commits wait it out).
    backoff: (u64, u64),
    expected: Vec<ExpectedGroup>,
    opened: HashMap<Gid, ExpectedGroup>,
    held: HashMap<Gid, Vec<Frame>>,
    heal_blocked: HashSet<Gid>,
    echo_wait: HashMap<Gid, (Gid, AbortHandle)>,
    epoch_wait: HashMap<Gid, AbortHandle>,
    slow_wait: HashMap<Gid, (AbortHandle, Instant)>,
    slowed: HashSet<Gid>,
    queued: Vec<Gid>,
    count425: HashMap<Gid, u32>,
    reframed_once: HashSet<(Gid, Gid)>,
    trust_retried: HashMap<(Gid, Gid), bool>,
    resync_tried: HashSet<Gid>,
    recovery_generation: HashMap<Gid, u64>,
    quiet: HashSet<Gid>,
    lost_reported: HashSet<Gid>,
    volunteer: HashMap<Gid, AbortHandle>,
    cursor_timer: HashMap<Gid, AbortHandle>,
    timers: Vec<AbortHandle>,
    join_all: JoinAllState,
}

/// The sync engine of one client.
pub struct Engine {
    core: CoreHandle,
    api: Arc<Api>,
    lanes: Arc<Lanes>,
    hooks: Arc<dyn SyncHooks>,
    instance: InstanceInfo,
    st: Mutex<St>,
    stopped: AtomicBool,
    trust: tokio::sync::Mutex<trust::TrustMemory>,
    gateway: Mutex<Option<Arc<WsGateway>>>,
}

fn key(g: &Gid) -> String {
    format!("g:{}", hex(g))
}

fn now_s() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |d| d.as_secs())
}

fn random_below(max: u64) -> u64 {
    let mut b = [0u8; 8];
    let _ = getrandom::fill(&mut b);
    u64::from_le_bytes(b) % max.max(1)
}

impl Engine {
    pub fn new(
        core: CoreHandle,
        api: Arc<Api>,
        hooks: Arc<dyn SyncHooks>,
        instance: InstanceInfo,
    ) -> Arc<Self> {
        Arc::new(Self {
            core,
            api,
            lanes: Arc::default(),
            hooks,
            instance,
            st: Mutex::new(St {
                backoff: (300, 300),
                ..St::default()
            }),
            stopped: AtomicBool::new(false),
            trust: tokio::sync::Mutex::default(),
            gateway: Mutex::new(None),
        })
    }

    fn st(&self) -> std::sync::MutexGuard<'_, St> {
        self.st
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// Stopped by [`Engine::stop`], or for good because the core's lock is poisoned (a core call panicked).
    pub fn stopped(&self) -> bool {
        self.stopped.load(Ordering::Acquire) || self.core.is_poisoned()
    }

    /// engine.ts `stop`: every timer is cancelled and nothing new starts; jobs already running see `stopped()`.
    pub fn stop(&self) {
        if self.stopped.swap(true, Ordering::AcqRel) {
            return;
        }
        let mut st = self.st();
        for t in st.timers.drain(..) {
            t.abort();
        }
        for (_, (_, t)) in st.echo_wait.drain() {
            t.abort();
        }
        for (_, t) in st.epoch_wait.drain() {
            t.abort();
        }
        for (_, (t, _)) in st.slow_wait.drain() {
            t.abort();
        }
        for (_, t) in st.volunteer.drain() {
            t.abort();
        }
        for (_, t) in st.cursor_timer.drain() {
            t.abort();
        }
        if let Some(t) = st.join_all.retry.take() {
            t.abort();
        }
    }

    pub fn api(&self) -> &Arc<Api> {
        &self.api
    }

    pub fn core(&self) -> &CoreHandle {
        &self.core
    }

    pub fn instance(&self) -> InstanceInfo {
        self.instance
    }

    fn arm<F, Fut>(self: &Arc<Self>, after: Duration, f: F) -> AbortHandle
    where
        F: FnOnce(Arc<Self>) -> Fut + Send + 'static,
        Fut: Future<Output = ()> + Send + 'static,
    {
        let this = Arc::clone(self);
        let handle = tokio::spawn(async move {
            tokio::time::sleep(after).await;
            if !this.stopped() {
                f(this).await;
            }
        })
        .abort_handle();
        let mut st = self.st();
        st.timers.retain(|t| !t.is_finished());
        st.timers.push(handle.clone());
        handle
    }

    // ---- reads of the core ----

    /// Read by the publisher too: a poisoned core answers nothing here instead of unwinding its caller.
    pub async fn groups(&self) -> Vec<GroupRow> {
        api::try_core(&self.core, |c| c.groups())
            .await
            .ok()
            .and_then(Result::ok)
            .and_then(|raw| GroupRow::decode_all(&raw).ok())
            .unwrap_or_default()
    }

    pub async fn row(&self, g: Gid) -> Option<GroupRow> {
        with_core(&self.core, move |c| c.group_row(&g))
            .await
            .ok()
            .and_then(|raw| GroupRow::decode_one(&raw).ok())
            .flatten()
    }

    async fn state_of(&self, g: Gid) -> Option<u8> {
        self.row(g).await.map(|r| r.state)
    }

    async fn outbox(&self, g: Gid) -> Vec<crate::state::messages::OutboxRow> {
        with_core(&self.core, move |c| c.outbox(&g))
            .await
            .ok()
            .and_then(|raw| crate::state::messages::decode_outbox(&raw).ok())
            .unwrap_or_default()
    }

    fn report_membership(&self, g: Gid, status: Membership) {
        {
            let mut st = self.st();
            if status == Membership::NotMember {
                st.lost_reported.insert(g);
            } else {
                st.lost_reported.remove(&g);
            }
        }
        self.hooks.membership(g, status);
    }

    // ---- the expected groups ----

    /// engine.ts `setExpected`: a set that gained a group starts a join-all pass (once there is a ready).
    pub fn set_expected(self: &Arc<Self>, groups: Vec<ExpectedGroup>) {
        let gained = {
            let mut st = self.st();
            let before: HashSet<Gid> = st.expected.iter().map(|g| g.group_id).collect();
            let gained = groups.iter().any(|g| !before.contains(&g.group_id));
            st.expected = groups;
            gained && st.ready.is_some()
        };
        if gained && !self.stopped() {
            self.request_join_all();
        }
    }

    fn expected(&self) -> Vec<ExpectedGroup> {
        let st = self.st();
        let mut by: Vec<ExpectedGroup> = Vec::new();
        for g in st.expected.iter().chain(st.opened.values()) {
            if let Some(at) = by.iter().position(|e| e.group_id == g.group_id) {
                by[at] = *g;
            } else {
                by.push(*g);
            }
        }
        by
    }

    fn add_expected(&self, g: ExpectedGroup) {
        self.st().opened.insert(g.group_id, g);
    }

    // ---- the gateway's events ----

    /// Drains the gateway's events until it ends. Nothing here waits on the core or the network: a response frame goes
    /// to the request transport, a group frame to its lane, and everything else to its own task.
    pub async fn run_events(
        self: Arc<Self>,
        gateway: Arc<WsGateway>,
        mut events: mpsc::Receiver<GatewayEvent>,
    ) {
        while let Some(event) = events.recv().await {
            self.on_event(&gateway, event);
        }
    }

    /// The gateway the client runs from now on (`None`: none). Set before the gateway's events are read, so only its
    /// events are acted on.
    pub fn set_gateway(&self, gateway: Option<Arc<WsGateway>>) {
        *self
            .gateway
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner) = gateway;
    }

    /// Whether `gateway` is the one the client runs now: a replaced gateway's late events (its `Disconnected` after a
    /// reconnect) must not touch the new connection's requests or state.
    fn is_current(&self, gateway: &Arc<WsGateway>) -> bool {
        self.gateway
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .as_ref()
            .is_some_and(|current| Arc::ptr_eq(current, gateway))
    }

    /// One event, handled without blocking.
    pub fn on_event(self: &Arc<Self>, gateway: &Arc<WsGateway>, event: GatewayEvent) {
        if !self.is_current(gateway) {
            return;
        }
        match event {
            GatewayEvent::Connected { session } => {
                let Ok(info) = ReadyInfo::from_frame(&session) else {
                    return;
                };
                let hello = gateway.hello();
                let incarnation = format!(
                    "{}:{}:{}",
                    hello
                        .as_ref()
                        .map(|h| hex(&h.instance_id))
                        .unwrap_or_default(),
                    info.generation,
                    hex(&info.boot_id)
                );
                if let Some(h) = &hello {
                    self.st().backoff = (h.backoff_ms, h.backoff_jitter_ms);
                }
                self.api.socket().on_ready(incarnation);
                self.hooks
                    .connection(ConnectionEvent::Ready(Box::new(info.clone())));
                self.on_ready(info);
            }
            GatewayEvent::Frame(bytes) => {
                if frames::is_response_frame_head(&bytes) {
                    self.api.socket().on_response(&bytes);
                    return;
                }
                let Ok(frame) = frames::decode(&bytes) else {
                    return;
                };
                self.on_frame(gateway, frame);
            }
            GatewayEvent::Disconnected { reason } => match gateway.refusal() {
                Some(refusal) => {
                    let code = match refusal {
                        crate::gateway::Refusal::Revoked
                        | crate::gateway::Refusal::SecondFactorRequired => {
                            ApiError::local(401, "E_UNAUTHENTICATED", "the session was revoked")
                        }
                        _ => ApiError::local(
                            426,
                            "E_UPGRADE_REQUIRED",
                            "the instance refused this client's version",
                        ),
                    };
                    self.api.socket().on_stopped(code);
                    let code = match reason {
                        crate::seams::DisconnectReason::Server(code) => code,
                        _ => 0,
                    };
                    self.hooks
                        .connection(ConnectionEvent::Refused(refusal, code));
                }
                None => {
                    self.api.socket().on_down();
                    self.hooks.connection(ConnectionEvent::Down);
                }
            },
        }
    }

    fn on_frame(self: &Arc<Self>, gateway: &Arc<WsGateway>, f: Frame) {
        if self.stopped() {
            return;
        }
        if f.op == Op::COMMUNITY_CHANGED {
            if let Ok(c) = frames::community_changed_of(&f) {
                self.hooks.community_changed(c);
            }
            return;
        }
        let Some(g) = f.group_id else { return };
        match f.op {
            Op::COMMIT_NEEDED => {
                let Some(round) = f.payload.get(3).and_then(Value::as_uint) else {
                    return;
                };
                let this = Arc::clone(self);
                let gateway = gateway.clone();
                tokio::spawn(async move {
                    if this.state_of(g).await != Some(2) {
                        return;
                    }
                    let ack = Frame {
                        op: Op::COMMIT_ACK,
                        n: 0,
                        group_id: Some(g),
                        payload: vec![Value::Uint(round)],
                    };
                    if let Ok(bytes) = frames::encode(&ack) {
                        let _ = gateway.send_now(bytes);
                    }
                    this.disarm_volunteer(g);
                    this.st().quiet.remove(&g);
                    this.request_commit(g, 0);
                });
            }
            Op::HANDSHAKE | Op::MESSAGE_CT => {
                let this = Arc::clone(self);
                self.lanes
                    .spawn(&key(&g), async move { this.frame_job(f).await });
            }
            Op::MESSAGE_DELETED => {
                let this = Arc::clone(self);
                self.lanes
                    .spawn(&key(&g), async move { this.deleted_job(f).await });
            }
            Op::WELCOME => {
                let this = Arc::clone(self);
                self.lanes
                    .spawn("w", async move { this.welcome_frame(f).await });
            }
            _ => {}
        }
    }

    /// engine.ts `onEvent` ready: the Welcome step first, then every active group's heal check, catch-up and drain,
    /// every lost group's resync, then a join-all pass.
    fn on_ready(self: &Arc<Self>, info: ReadyInfo) {
        if self.stopped() {
            return;
        }
        {
            let mut st = self.st();
            st.ready = Some(info.clone());
            st.quiet.clear();
            st.resync_tried.clear();
            st.heal_blocked.clear();
        }
        let this = Arc::clone(self);
        self.lanes.spawn("w", async move {
            let groups = this.groups().await;
            {
                let mut st = this.st();
                for g in groups.iter().filter(|g| g.state == 2) {
                    st.heal_blocked.insert(g.group_id);
                }
            }
            for d in &info.groups {
                if d.proposals_outstanding > 0
                    && groups
                        .iter()
                        .any(|g| g.group_id == d.group_id && g.state == 2)
                {
                    this.arm_volunteer(d.group_id);
                }
            }
            this.welcome_step().await;
            if this.stopped() {
                return;
            }
            for g in &groups {
                let id = g.group_id;
                if g.state == 2 {
                    match this.ensure_group_healed(g, info.generation).await {
                        Ok(()) => {
                            this.st().heal_blocked.remove(&id);
                            if !this.stopped() && this.state_of(id).await == Some(2) {
                                this.request_catch_up(id);
                                this.request_drain(id);
                            }
                        }
                        // A 404 catch-up can still find a removed member; any other failed repair holds the group.
                        Err(e) if e.api().is_some_and(|a| a.status == 404) && !this.stopped() => {
                            this.request_catch_up(id)
                        }
                        Err(_) => {}
                    }
                } else if g.state == 3 && !this.st().resync_tried.contains(&id) {
                    let t = Arc::clone(&this);
                    this.lanes.coalesce(&key(&id), "resync", async move {
                        t.resync(id).await;
                    });
                }
            }
            if !this.stopped() {
                this.request_join_all();
            }
            // Leaves this device has not judged yet (a new member's) are read now, not only when a send needs them.
            this.refresh_trust().await;
        });
    }

    /// engine.ts `ensureGroupHealed`.
    async fn ensure_group_healed(
        &self,
        group: &GroupRow,
        generation: u64,
    ) -> Result<(), SyncError> {
        let g = group.group_id;
        let status = self.api.get_group_heal_state(&g).await?;
        if status.generation != generation {
            return Err(SyncError::Api(ApiError::local(
                0,
                "E_SYNC_HEAL_GENERATION",
                "",
            )));
        }
        if !status.needs_heal {
            return Ok(());
        }
        let snapshot = with_core(&self.core, move |c| c.group_heal_snapshot(&g)).await?;
        {
            let v = value::decode(&snapshot)?;
            let a = arr(&v, 3)?;
            if a[0].as_bytes().is_none() || a[1].as_array().is_none() || a[2].as_bytes().is_none() {
                return Err(SyncError::Decode(DecodeError("heal snapshot")));
            }
        }
        let (epoch, next_seq) = self.api.post_group_heal(&g, snapshot).await?;
        let mismatch = || SyncError::Api(ApiError::local(0, "E_SYNC_HEAL_MISMATCH", ""));
        let current = self.row(g).await;
        if current
            .as_ref()
            .is_none_or(|r| r.state != 2 || r.epoch != epoch || r.next_seq != next_seq)
        {
            return Err(mismatch());
        }
        let confirmed = self.api.get_group_heal_state(&g).await?;
        if confirmed.generation != generation
            || confirmed.needs_heal
            || confirmed.epoch != epoch
            || confirmed.next_seq != next_seq
        {
            return Err(mismatch());
        }
        Ok(())
    }

    /// engine.ts `frameJob`: the ordering rule. Below `next_seq`: a duplicate, dropped. Above it: a gap, caught up
    /// instead. Equal: applied.
    fn frame_job(self: Arc<Self>, f: Frame) -> BoxFut<()> {
        Box::pin(async move {
            let Some(g) = f.group_id else { return };
            if self.stopped() {
                return;
            }
            #[cfg(test)]
            tests::slow_apply(&self.core).await;
            let Some(r) = self.row(g).await else { return };
            if r.state == 3 || r.state == 4 {
                return;
            }
            if r.state == 0 || r.state == 1 {
                let mut st = self.st();
                let held = st.held.entry(g).or_default();
                held.push(f);
                if held.len() > HELD_MAX {
                    held.remove(0);
                }
                return;
            }
            let Ok(row) = live_row(&f) else {
                self.request_catch_up(g);
                return;
            };
            if row.seq < r.next_seq {
                return;
            }
            if row.seq > r.next_seq {
                self.clone().catch_up_now(g, Source::CatchUp).await;
                return;
            }
            let (hs, ms, seq) = (row.handshakes, row.messages, row.seq);
            let applied = with_core(&self.core, move |c| c.group_apply(&g, &hs, &ms, seq)).await;
            match applied
                .map_err(SyncError::from)
                .and_then(|raw| Ok(ApplyResult::decode(&raw)?))
            {
                Ok(result) => {
                    let active = result.state == 2;
                    self.clone().apply_hook(g, result, Source::Frame).await;
                    if active {
                        self.arm_cursor(g);
                    }
                }
                Err(_) => {
                    // A core refusal may have quarantined earlier rows atomically before it answered.
                    self.report_current(g);
                    self.request_catch_up(g);
                }
            }
        })
    }

    /// engine.ts `deletedJob`: `message.deleted` `[seq, by]`.
    async fn deleted_job(self: Arc<Self>, f: Frame) {
        let Some(g) = f.group_id else { return };
        if self.stopped() {
            return;
        }
        let Some(seq) = (f.payload.len() == 2 && f.payload[1].as_uint().is_some())
            .then(|| f.payload[0].as_uint())
            .flatten()
        else {
            return;
        };
        if self.row(g).await.is_none_or(|r| r.state == 4) {
            return;
        }
        match with_core(&self.core, move |c| c.message_deleted(&g, seq)).await {
            Ok(raw) => {
                if ApplyResult::decode(&raw).is_ok_and(|r| !r.new_seqs.is_empty()) {
                    self.hooks.group_changed(g);
                }
            }
            Err(_) => self.report_current(g),
        }
    }

    fn report_current(&self, g: Gid) {
        self.hooks.group_changed(g);
    }

    fn arm_cursor(self: &Arc<Self>, g: Gid) {
        if self.st().cursor_timer.contains_key(&g) {
            return;
        }
        let handle = self.arm(CURSOR_DEBOUNCE, move |this| async move {
            this.st().cursor_timer.remove(&g);
            let t = Arc::clone(&this);
            this.lanes
                .spawn(&key(&g), async move { t.cursor_step(g).await });
        });
        self.st().cursor_timer.insert(g, handle);
    }

    fn disarm_cursor(&self, g: Gid) {
        if let Some(t) = self.st().cursor_timer.remove(&g) {
            t.abort();
        }
    }

    /// engine.ts `cursorStep`: acknowledge what this device holds, so the instance can prune behind it.
    async fn cursor_step(&self, g: Gid) {
        if self.stopped() || self.state_of(g).await != Some(2) {
            return;
        }
        let Ok(body) = with_core(&self.core, move |c| c.cursor_body(&g)).await else {
            return;
        };
        let Ok(v) = value::decode(&body) else { return };
        let Ok([seq, epoch]) = arr(&v, 2).map(|a| [a[0].as_uint(), a[1].as_uint()]) else {
            return;
        };
        let (Some(seq), Some(epoch)) = (seq, epoch) else {
            return;
        };
        if self.api.post_cursor(&g, body).await.is_ok() && !self.stopped() {
            let _ = with_core(&self.core, move |c| c.cursor_acked(&g, seq, epoch)).await;
        }
    }

    /// engine.ts `catchUpNow`: true only when it read the group to the end with the group still active.
    pub fn catch_up_now(self: Arc<Self>, g: Gid, source: Source) -> BoxFut<bool> {
        Box::pin(async move {
            self.disarm_cursor(g);
            let mut carry: Option<u64> = None;
            let result: Result<(), SyncError> = async {
                loop {
                    if self.stopped() {
                        return Err(SyncError::Stopped);
                    }
                    let Some(r) = self.row(g).await.filter(|r| r.state == 2) else {
                        return Err(SyncError::Stopped);
                    };
                    let from = r.next_seq;
                    let ms = self.api.get_messages(&g, from, MESSAGE_PAGE).await?;
                    if self.stopped() {
                        return Err(SyncError::Stopped);
                    }
                    let hs = self.api.get_handshakes(&g, from, HANDSHAKE_PAGE).await?;
                    if self.stopped() {
                        return Err(SyncError::Stopped);
                    }
                    let through = through_of(
                        PageInfo {
                            count: ms.count,
                            last_seq: ms.last,
                        },
                        PageInfo {
                            count: hs.count,
                            last_seq: hs.last,
                        },
                        carry,
                    );
                    if through >= from {
                        let (h, m) = (hs.raw.clone(), ms.raw.clone());
                        let raw =
                            with_core(&self.core, move |c| c.group_apply(&g, &h, &m, through))
                                .await?;
                        let result = ApplyResult::decode(&raw)?;
                        self.clone().apply_hook(g, result, source).await;
                    }
                    carry = hs.last;
                    if self.state_of(g).await != Some(2) {
                        return Err(SyncError::Stopped);
                    }
                    if ms.count < MESSAGE_PAGE
                        && hs.count < HANDSHAKE_PAGE
                        && hs.last.is_none_or(|h| h <= through)
                    {
                        return Ok(());
                    }
                }
            }
            .await;
            match result {
                Ok(()) => {
                    // The DS answered every read: this device holds its leaf, whatever an earlier refusal reported.
                    if self.st().lost_reported.contains(&g) {
                        self.report_membership(g, Membership::Member);
                    }
                    self.cursor_step(g).await;
                    true
                }
                Err(e) => {
                    if matches!(e, SyncError::Core(_)) {
                        self.report_current(g);
                    }
                    let lost = self.membership_lost(g, &e).await;
                    let pruned = e.api().is_some_and(|a| a.is(410, "E_PRUNED"));
                    if (lost || pruned) && !self.st().resync_tried.contains(&g) {
                        self.clone().resync(g).await;
                    } else if lost {
                        self.relost(g);
                    }
                    false
                }
            }
        })
    }

    /// engine.ts `membershipLost`: 404 E_NOT_FOUND on a group read of an active group (this device holds no leaf).
    async fn membership_lost(&self, g: Gid, e: &SyncError) -> bool {
        e.api().is_some_and(|a| a.is(404, "E_NOT_FOUND")) && self.state_of(g).await == Some(2)
    }

    fn relost(&self, g: Gid) {
        if !self.st().lost_reported.contains(&g) {
            self.report_membership(g, Membership::NotMember);
        }
    }

    /// engine.ts `applyHook` (without the moderation-removal recovery; see the module doc).
    fn apply_hook(self: Arc<Self>, g: Gid, r: ApplyResult, source: Source) -> BoxFut<()> {
        Box::pin(async move {
            self.hooks.group_changed(g);
            if r.own_adopted {
                let rows = self.outbox(g).await;
                let held: HashSet<Gid> = rows.iter().map(|x| x.msg_id).collect();
                let resume = {
                    let mut st = self.st();
                    st.count425.retain(|m, _| held.contains(m));
                    st.reframed_once.retain(|(gg, m)| {
                        *gg != g || rows.iter().any(|x| x.msg_id == *m && x.state != 2)
                    });
                    match st.echo_wait.get(&g) {
                        Some((msg, _))
                            if !rows.iter().any(|x| x.msg_id == *msg && x.state == 1) =>
                        {
                            if let Some((_, t)) = st.echo_wait.remove(&g) {
                                t.abort();
                            }
                            true
                        }
                        _ => false,
                    }
                };
                if resume {
                    self.request_drain(g);
                }
            }
            if r.epoch_changed {
                let waited = self.st().epoch_wait.remove(&g);
                if let Some(t) = waited {
                    t.abort();
                    self.request_drain(g);
                } else if !self.st().echo_wait.contains_key(&g)
                    && self.outbox(g).await.iter().any(|x| x.state == 0)
                {
                    self.request_drain(g);
                }
            }
            if r.state == 3 && !self.st().resync_tried.contains(&g) {
                self.clone().resync(g).await;
            }
            if source != Source::Commit && r.state == 2 {
                if r.proposals_pending > 0 {
                    self.arm_volunteer(g);
                } else {
                    self.disarm_volunteer(g);
                }
            }
            // A leaf the core cannot judge yet holds its rows: read its signed list now.
            if r.epoch_changed || !r.new_seqs.is_empty() {
                let this = Arc::clone(&self);
                tokio::spawn(async move { this.refresh_trust().await });
            }
        })
    }

    pub fn request_catch_up(self: &Arc<Self>, g: Gid) {
        if self.stopped() {
            return;
        }
        let this = Arc::clone(self);
        self.lanes.coalesce(&key(&g), "catch-up", async move {
            this.catch_up_now(g, Source::CatchUp).await;
        });
    }

    pub fn request_drain(self: &Arc<Self>, g: Gid) {
        if self.stopped() || self.st().heal_blocked.contains(&g) {
            return;
        }
        let this = Arc::clone(self);
        self.lanes
            .coalesce(&key(&g), "drain", async move { this.drain_one(g).await });
    }

    fn request_commit(self: &Arc<Self>, g: Gid, attempt: u32) {
        if self.stopped() {
            return;
        }
        let this = Arc::clone(self);
        self.lanes.coalesce(&key(&g), "commit", async move {
            this.commit_now(g, attempt).await;
        });
    }

    /// engine.ts `commitNow`.
    fn commit_now(self: Arc<Self>, g: Gid, attempt: u32) -> BoxFut<bool> {
        Box::pin(async move {
            let Some(row) = self.row(g).await.filter(|r| r.state == 2) else {
                return false;
            };
            if self.stopped() {
                return false;
            }
            let mut built = false;
            let result: Result<(), SyncError> = async {
                let proposals = self.api.get_proposals(&g).await?;
                if self.stopped() {
                    return Err(SyncError::Stopped);
                }
                let active = Rows::of(&proposals)?
                    .iter()
                    .any(|p| arr(p, 5).ok().and_then(|a| a[4].as_uint()) == Some(0));
                if !active && row.proposals_pending == 0 {
                    return Err(SyncError::Stopped);
                }
                let p = proposals.clone();
                let adds = with_core(&self.core, move |c| c.proposed_adds(&g, &p)).await?;
                for candidate in Rows::of(&adds)?.iter() {
                    let a = arr(candidate, 3)?;
                    let (index, user, cached) = (uint(&a[0])?, bin16(&a[1])?, uint(&a[2])?);
                    let Ok((head, history)) =
                        trust::read_device_list_history(&self.api, &user, cached).await
                    else {
                        return Err(SyncError::Api(ApiError::local(
                            0,
                            "E_DEVICE_LIST_STALE",
                            "",
                        )));
                    };
                    if self.stopped() {
                        return Err(SyncError::Stopped);
                    }
                    let head_list = trust::signed_head_list(&head).map_err(|_| {
                        SyncError::Api(ApiError::local(0, "E_DEVICE_LIST_STALE", ""))
                    })?;
                    if let Ok(Some(att)) = trust::head_attestation(&head) {
                        let _ = with_core(&self.core, move |c| c.account_kind_ingest(&user, &att))
                            .await;
                    }
                    let p = proposals.clone();
                    let now = now_s();
                    let ingested = with_core(&self.core, move |c| {
                        c.device_list_ingest_add(&g, &p, index, &user, &history, &head_list, now)
                    })
                    .await;
                    // A candidate whose leaf fails is left out of this commit by the core, never a refusal that stalls
                    // this device's commits.
                    if let Err(e) = ingested
                        && !FAILED_LEAF.contains(&e.code)
                    {
                        return Err(e.into());
                    }
                    self.hooks.trust_changed();
                }
                let p = proposals.clone();
                let body = with_core(&self.core, move |c| c.commit_build(&g, &p)).await?;
                built = true;
                self.api.post_commit(&g, body).await?;
                if self.stopped() {
                    return Err(SyncError::Stopped);
                }
                let raw = with_core(&self.core, move |c| c.commit_confirm(&g)).await?;
                let result = ApplyResult::decode(&raw)?;
                self.clone().apply_hook(g, result, Source::Commit).await;
                Ok(())
            }
            .await;
            match result {
                Ok(()) => {
                    self.disarm_volunteer(g);
                    self.st().quiet.remove(&g);
                    self.request_drain(g);
                    true
                }
                Err(e) => {
                    // Only a commit this attempt built is aborted (a pending call-rotation commit is left alone).
                    if built {
                        let _ = with_core(&self.core, move |c| c.commit_abort(&g)).await;
                    }
                    if let Some(a) = e.api() {
                        if a.is(409, "E_COMMIT_CONFLICT") {
                            self.clone().catch_up_now(g, Source::Commit).await;
                            if self.row(g).await.is_some_and(|r| r.proposals_pending > 0) {
                                if attempt < COMMIT_RETRY_MAX {
                                    let wait =
                                        Duration::from_millis(random_below(COMMIT_JITTER_MS));
                                    self.arm(wait, move |this| async move {
                                        this.request_commit(g, attempt + 1)
                                    });
                                } else {
                                    self.st().quiet.insert(g);
                                }
                            }
                        } else if a.status == 425 || a.status == 422 {
                            self.clone().catch_up_now(g, Source::Commit).await;
                            if a.status == 422 {
                                self.st().quiet.insert(g);
                            }
                        } else if (a.is(403, "E_LEAF_NOT_CURRENT")
                            || self.membership_lost(g, &e).await)
                            && !self.st().resync_tried.contains(&g)
                        {
                            self.clone().resync(g).await;
                        } else if self.membership_lost(g, &e).await {
                            self.relost(g);
                        }
                    }
                    self.request_drain(g);
                    false
                }
            }
        })
    }

    fn arm_volunteer(self: &Arc<Self>, g: Gid) {
        let (base, jitter) = {
            let st = self.st();
            if st.volunteer.contains_key(&g) || st.quiet.contains(&g) {
                return;
            }
            st.backoff
        };
        let wait = Duration::from_millis(base + random_below(jitter));
        let handle = self.arm(wait, move |this| async move {
            this.st().volunteer.remove(&g);
            this.request_commit(g, 0);
        });
        self.st().volunteer.insert(g, handle);
    }

    fn disarm_volunteer(&self, g: Gid) {
        if let Some(t) = self.st().volunteer.remove(&g) {
            t.abort();
        }
    }

    /// engine.ts `activate`: the group is active now; its held frames are applied in seq order, then a catch-up and a
    /// drain are queued behind them.
    fn activate(self: Arc<Self>, g: Gid) -> BoxFut<()> {
        Box::pin(async move {
            self.hooks.group_changed(g);
            let frames = self.st().held.remove(&g).unwrap_or_default();
            let mut ordered: Vec<(u64, Frame)> = Vec::new();
            for f in frames {
                match f.payload.first().and_then(Value::as_uint) {
                    Some(seq) => ordered.push((seq, f)),
                    None => self.request_catch_up(g),
                }
            }
            ordered.sort_by_key(|(seq, _)| *seq);
            for (_, f) in ordered {
                self.clone().frame_job(f).await;
            }
            self.request_catch_up(g);
            self.request_drain(g);
        })
    }

    /// engine.ts `resync`.
    fn resync(self: Arc<Self>, g: Gid) -> BoxFut<bool> {
        Box::pin(async move {
            if self.row(g).await.is_some_and(|r| r.kind == 1) {
                // A call group is never re-joined here: its join is the call controller's alone.
                self.st().resync_tried.insert(g);
                self.report_membership(g, Membership::NotMember);
                return false;
            }
            let ok = self.clone().resync_group(g, false).await;
            if ok {
                self.st().heal_blocked.remove(&g);
            }
            ok
        })
    }

    /// channel.ts `resyncGroup`.
    async fn resync_group(self: Arc<Self>, g: Gid, from_open: bool) -> bool {
        if !from_open && self.st().resync_tried.contains(&g) {
            return false;
        }
        let Some(row) = self.row(g).await else {
            return false;
        };
        self.st().resync_tried.insert(g);
        self.report_membership(g, Membership::Resyncing);
        let eg = ExpectedGroup {
            group_id: g,
            community_id: row.community_id,
            channel_id: row.target_id,
            policy_version: self.instance.policy_version,
        };
        match self.clone().external_join(eg, &mut 0).await {
            Ok(()) => true,
            Err(e) => {
                // A refusal before the join leaves an active row in state 2; it is never discarded.
                if self.state_of(g).await == Some(1) {
                    let _ = with_core(&self.core, move |c| c.group_discard(&g)).await;
                }
                self.hooks.group_changed(g);
                self.report_membership(g, Membership::NotMember);
                if row.state == 2 {
                    self.fail_outbox(g, &e.code()).await;
                }
                false
            }
        }
    }

    /// channel.ts `failOutbox`.
    async fn fail_outbox(&self, g: Gid, code: &str) {
        {
            let mut st = self.st();
            if let Some((_, t)) = st.echo_wait.remove(&g) {
                t.abort();
            }
            if let Some(t) = st.epoch_wait.remove(&g) {
                t.abort();
            }
            if let Some((t, _)) = st.slow_wait.remove(&g) {
                t.abort();
            }
        }
        let mut changed = false;
        for r in self.outbox(g).await {
            if r.state == 2 {
                continue;
            }
            let (msg, code) = (r.msg_id, code.to_owned());
            let _ = with_core(&self.core, move |c| c.send_fail(&msg, &code)).await;
            let mut st = self.st();
            st.count425.remove(&msg);
            st.slowed.remove(&msg);
            st.reframed_once.remove(&(g, msg));
            changed = true;
        }
        if changed {
            self.hooks.group_changed(g);
        }
    }

    /// channel.ts `externalJoin`: GroupInfo and tree, `group_join_external`, `POST resync`, `group_joined`, activate.
    async fn external_join(
        self: Arc<Self>,
        eg: ExpectedGroup,
        joins: &mut u32,
    ) -> Result<(), SyncError> {
        let g = eg.group_id;
        loop {
            if self.stopped() {
                return Err(SyncError::Stopped);
            }
            let info = self.api.get_group_info(&g).await?;
            let tree = self.api.get_group_tree(&g).await?;
            let built = with_core(&self.core, move |c| {
                c.group_join_external(
                    &eg.group_id,
                    eg.community_id.as_ref(),
                    &eg.channel_id,
                    eg.policy_version,
                    &info,
                    &tree,
                )
            })
            .await;
            let body = match built {
                Ok(body) => body,
                Err(e) if e.code == "E_CORE_INPUT" => {
                    *joins += 1;
                    if *joins < COMMIT_RETRY_MAX {
                        continue;
                    }
                    return Err(e.into());
                }
                Err(e) => return Err(e.into()),
            };
            *joins += 1;
            let answer = match self.api.post_resync(&g, body).await {
                Ok(a) => a,
                Err(e) => {
                    if self.state_of(g).await == Some(1) {
                        let _ = with_core(&self.core, move |c| c.group_discard(&g)).await;
                    }
                    return Err(e.into());
                }
            };
            let seq = answer.0;
            with_core(&self.core, move |c| c.group_joined(&g, seq)).await?;
            *self.st().recovery_generation.entry(g).or_default() += 1;
            self.clone().activate(g).await;
            return Ok(());
        }
    }

    // ---- Welcomes ----

    /// channel.ts `welcomeStep`: every Welcome waiting for this device, in pages of 64.
    pub async fn welcome_step(self: &Arc<Self>) {
        let mut after = 0u64;
        loop {
            if self.stopped() {
                return;
            }
            let Ok(page) = self.api.get_welcomes(after).await else {
                return;
            };
            if page.count == 0 {
                return;
            }
            self.apply_welcome_rows(page.raw).await;
            match page.last {
                Some(last) if page.count >= 64 => after = last,
                _ => return,
            }
        }
    }

    async fn welcome_frame(self: Arc<Self>, f: Frame) {
        if self.stopped() {
            return;
        }
        if let Ok(body) = welcome_page(&f) {
            self.apply_welcome_rows(body).await;
        }
    }

    /// channel.ts `applyWelcomeRows` and `judgeWelcomes`.
    async fn apply_welcome_rows(self: &Arc<Self>, raw: Vec<u8>) {
        let expected = encode_expected(&self.expected());
        let Ok(Ok(outcomes)) = with_core(&self.core, move |c| c.welcomes_apply(&raw, &expected))
            .await
            .map(|raw| Rows::of(&raw).map(|rows| rows.iter().cloned().collect::<Vec<_>>()))
        else {
            return;
        };
        for o in outcomes {
            let Ok(a) = arr(&o, 4) else { continue };
            let (Ok(id), Ok(g), Ok(outcome)) = (uint(&a[0]), bin16(&a[1]), uint(&a[2])) else {
                continue;
            };
            if outcome == 3 {
                // Kept for a group this device does not expect (a DM it has not listed yet).
                continue;
            }
            if outcome == 1 && self.state_of(g).await != Some(2) {
                continue;
            }
            let _ = self.api.delete_welcome(id).await;
            if outcome == 0 {
                let this = Arc::clone(self);
                self.lanes
                    .spawn(&key(&g), async move { this.activate(g).await });
            }
        }
    }

    /// engine.ts `pollWelcomes`.
    async fn poll_welcomes(self: &Arc<Self>) {
        let this = Arc::clone(self);
        self.lanes
            .run("w", async move { this.welcome_step().await })
            .await;
    }

    // ---- join-all (joinall.ts) ----

    fn request_join_all(self: &Arc<Self>) {
        if self.stopped() {
            return;
        }
        {
            let mut st = self.st();
            if let Some(t) = st.join_all.retry.take() {
                t.abort();
            }
            if st.join_all.running {
                st.join_all.again = true;
                return;
            }
            st.join_all.running = true;
        }
        let this = Arc::clone(self);
        tokio::spawn(async move { this.join_all_pass().await });
    }

    async fn join_all_pass(self: Arc<Self>) {
        let mut failed = 0;
        let mut list = self.expected();
        // Channels first, then DMs.
        list.sort_by_key(|g| g.community_id.is_none());
        let mut todo = Vec::new();
        for g in list {
            if matches!(
                self.state_of(g.group_id).await,
                None | Some(0) | Some(1) | Some(4)
            ) {
                todo.push(g);
            }
        }
        if !todo.is_empty() {
            self.poll_welcomes().await;
        }
        for g in todo {
            if self.stopped() {
                break;
            }
            if !self.clone().join_one(g).await {
                failed += 1;
            }
        }
        let again = {
            let mut st = self.st();
            st.join_all.running = false;
            std::mem::take(&mut st.join_all.again)
        };
        if self.stopped() {
            return;
        }
        if failed > 0 {
            let handle = self.arm(JOIN_ALL_RETRY, |this| async move {
                this.st().join_all.retry = None;
                this.request_join_all();
            });
            self.st().join_all.retry = Some(handle);
        }
        if again {
            self.request_join_all();
        }
    }

    /// joinall.ts `joinOne`: true when joined or already held.
    async fn join_one(self: Arc<Self>, eg: ExpectedGroup) -> bool {
        let g = eg.group_id;
        let mut joins = 0u32;
        loop {
            if self.stopped() {
                return true;
            }
            let this = Arc::clone(&self);
            let result = self
                .lanes
                .run(&key(&g), async move {
                    let state = this.state_of(g).await;
                    if matches!(state, Some(2) | Some(3)) {
                        return Ok(());
                    }
                    if matches!(state, Some(0) | Some(1)) {
                        let _ = with_core(&this.core, move |c| c.group_discard(&g)).await;
                    }
                    this.clone().external_join(eg, &mut joins).await
                })
                .await;
            // Wait for the jobs the join queued (the activation's catch-up) to be in line.
            self.lanes.run(&key(&g), async {}).await;
            match result {
                Ok(()) => return true,
                Err(SyncError::Stopped) => return true,
                Err(e)
                    if e.api().is_some_and(|a| a.is(409, "E_COMMIT_CONFLICT"))
                        && joins < COMMIT_RETRY_MAX =>
                {
                    continue;
                }
                Err(_) => return false,
            }
        }
    }

    // ---- opening a channel (channel.ts openChannelFlow) ----

    /// channel.ts `openChannelFlow` on the channel's lane: the channel's local text group when it is active; else a
    /// Welcome or an external join of the group the instance names; else a new group registered for it.
    pub async fn open_channel(
        self: &Arc<Self>,
        community: Option<Gid>,
        channel: Gid,
        text_group: Option<Gid>,
    ) -> Result<Gid, SyncError> {
        if self.stopped() {
            return Err(SyncError::Stopped);
        }
        let this = Arc::clone(self);
        self.lanes
            .run(&format!("c:{}", hex(&channel)), async move {
                this.open_channel_flow(community, channel, text_group).await
            })
            .await
    }

    async fn open_channel_flow(
        self: &Arc<Self>,
        community: Option<Gid>,
        channel: Gid,
        text_group: Option<Gid>,
    ) -> Result<Gid, SyncError> {
        let mut text_group = text_group;
        let mut joins = 0u32;
        let mut registrations = 0u32;
        let policy = self.instance.policy_version;
        let expected = |g: Gid| ExpectedGroup {
            group_id: g,
            community_id: community,
            channel_id: channel,
            policy_version: policy,
        };
        if community.is_none() && text_group.is_none() {
            text_group = self.dm_group_of(channel).await?;
        }
        if let Some(g) = text_group {
            self.add_expected(expected(g));
        }
        loop {
            if self.stopped() {
                return Err(SyncError::Stopped);
            }
            let local = self
                .groups()
                .await
                .into_iter()
                .find(|r| r.target_id == channel && r.kind == 0 && r.state != 4);
            if let Some(local) = local {
                let g = local.group_id;
                if local.state == 2 {
                    return Ok(g);
                }
                if local.state == 3 {
                    let this = Arc::clone(self);
                    let ran = self
                        .lanes
                        .run(&key(&g), async move {
                            if this.state_of(g).await != Some(3) {
                                return false;
                            }
                            this.clone().resync_group(g, true).await;
                            true
                        })
                        .await;
                    if ran {
                        return if self.state_of(g).await == Some(2) {
                            Ok(g)
                        } else {
                            Err(SyncError::Api(ApiError::local(
                                0,
                                "E_NOT_MEMBER",
                                "the resync was refused",
                            )))
                        };
                    }
                    continue;
                }
                // A row still registering or joining once its lane is free was never finished.
                let this = Arc::clone(self);
                self.lanes
                    .run(&key(&g), async move {
                        if matches!(this.state_of(g).await, Some(0) | Some(1)) {
                            let _ = with_core(&this.core, move |c| c.group_discard(&g)).await;
                        }
                    })
                    .await;
                continue;
            }
            if let Some(g) = text_group {
                self.poll_welcomes().await;
                if self.state_of(g).await == Some(2) {
                    return Ok(g);
                }
                let this = Arc::clone(self);
                let eg = expected(g);
                let mut budget = joins;
                let joined = self
                    .lanes
                    .run(&key(&g), async move {
                        if this.state_of(g).await == Some(2) {
                            return (Ok(()), budget);
                        }
                        let r = this.clone().external_join(eg, &mut budget).await;
                        (r, budget)
                    })
                    .await;
                joins = joined.1;
                match joined.0 {
                    Ok(()) => return Ok(g),
                    Err(e)
                        if e.api().is_some_and(|a| a.is(425, "E_COMMIT_REQUIRED"))
                            && joins < COMMIT_RETRY_MAX =>
                    {
                        tokio::time::sleep(MEMBERSHIP_WAIT).await;
                        continue;
                    }
                    Err(e)
                        if e.api().is_some_and(|a| a.is(409, "E_COMMIT_CONFLICT"))
                            && joins < COMMIT_RETRY_MAX =>
                    {
                        continue;
                    }
                    Err(e) => return Err(e),
                }
            }
            if registrations >= REGISTER_RETRY_MAX {
                return Err(SyncError::RegisterRace);
            }
            registrations += 1;
            let mut g = [0u8; 16];
            getrandom::fill(&mut g)
                .map_err(|_| SyncError::Api(ApiError::local(0, "E_RANDOM", "")))?;
            let this = Arc::clone(self);
            let external = self.instance.external_sender_pub;
            let registered = self
                .lanes
                .run(&key(&g), async move {
                    let body = with_core(&this.core, move |c| {
                        c.group_create(&g, community.as_ref(), &channel, policy, &external)
                    })
                    .await?;
                    let next = match this.api.post_group(body).await {
                        Ok(n) => n,
                        Err(e) => {
                            if this.state_of(g).await == Some(0) {
                                let _ = with_core(&this.core, move |c| c.group_discard(&g)).await;
                            }
                            return Err(SyncError::from(e));
                        }
                    };
                    with_core(&this.core, move |c| c.group_registered(&g, next)).await?;
                    this.clone().activate(g).await;
                    Ok::<(), SyncError>(())
                })
                .await;
            match registered {
                Ok(()) => return Ok(g),
                Err(e) if e.api().is_some_and(|a| a.is(409, "E_GROUP_EXISTS")) => {
                    // Someone registered the channel's group first: join theirs.
                    text_group = match community {
                        None => self.dm_group_of(channel).await?,
                        Some(c) => {
                            let raw = self.api.list_channels(&c).await?;
                            let rows = crate::state::channels::decode_channels(
                                crate::state::CommunityId(c),
                                &raw,
                            )?;
                            rows.into_iter()
                                .find(|r| r.id.0 == channel)
                                .ok_or(SyncError::ChannelGone)?
                                .text_group_id
                                .map(|g| g.0)
                        }
                    };
                    if let Some(g) = text_group {
                        self.add_expected(expected(g));
                    }
                }
                Err(e) => return Err(e),
            }
        }
    }

    async fn dm_group_of(&self, channel: Gid) -> Result<Option<Gid>, SyncError> {
        match self.api.get_channel_text_group(&channel).await {
            Ok(g) => Ok(g),
            Err(e) if e.is(404, "E_NOT_FOUND") => Err(SyncError::ChannelGone),
            Err(e) => Err(e.into()),
        }
    }

    // ---- sending (send.ts) ----

    /// send.ts `sendMessage`: queues a text post (type 0) in the group's outbox and asks for a drain. The core's refusal
    /// is answered at once and nothing else runs.
    pub async fn send_text(self: &Arc<Self>, g: Gid, text: String) -> Result<Gid, SyncError> {
        if self.stopped() {
            return Err(SyncError::Stopped);
        }
        let request = Value::Array(vec![
            Value::Uint(0),
            Value::Null,
            Value::Text(text),
            Value::Array(Vec::new()),
            Value::Null,
        ])
        .encode();
        let now = now_s();
        let raw = with_core(&self.core, move |c| c.send_prepare(&g, &request, now)).await?;
        let v = value::decode(&raw)?;
        let msg = bin16(&arr(&v, 1)?[0])?;
        self.st().queued.push(msg);
        self.hooks.group_changed(g);
        self.request_drain(g);
        Ok(msg)
    }

    /// send.ts `afterLostResponse`.
    async fn after_lost_response(self: &Arc<Self>, g: Gid, msg: Gid) {
        self.clone().catch_up_now(g, Source::CatchUp).await;
        let row = self.outbox(g).await.into_iter().find(|x| x.msg_id == msg);
        if row.as_ref().is_none_or(|r| r.state != 1) {
            self.hooks.group_changed(g);
            self.request_drain(g);
            return;
        }
        if !self.st().echo_wait.contains_key(&g) {
            self.arm_echo_wait(g, msg);
        }
    }

    /// send.ts `armEchoWait`: waits, catches up once more, and requeues the row only when that catch-up reached the
    /// server and did not see the upload; a catch-up that did not reach it proves nothing, so the wait starts again
    /// (a resend after it would store a second copy).
    fn arm_echo_wait(self: &Arc<Self>, g: Gid, msg: Gid) {
        let handle = self.arm(ECHO_WAIT, move |this| async move {
            let t = Arc::clone(&this);
            this.lanes.spawn(&key(&g), async move {
                let reached = t.clone().catch_up_now(g, Source::CatchUp).await;
                let fresh = t.outbox(g).await.into_iter().find(|x| x.msg_id == msg);
                if fresh.as_ref().is_some_and(|r| r.state == 1) && !reached {
                    t.st().echo_wait.remove(&g);
                    if !t.stopped() && t.state_of(g).await == Some(2) {
                        t.arm_echo_wait(g, msg);
                    }
                    return;
                }
                if fresh.is_some_and(|r| r.state == 1) {
                    let _ = with_core(&t.core, move |c| c.send_requeue(&msg)).await;
                    t.hooks.group_changed(g);
                }
                t.st().echo_wait.remove(&g);
                t.request_drain(g);
            });
        });
        self.st().echo_wait.insert(g, (msg, handle));
    }

    /// send.ts `drainOne`: one outbox row of the group, the oldest queued (in the order this engine queued them).
    async fn drain_one(self: Arc<Self>, g: Gid) {
        if self.stopped()
            || self.st().heal_blocked.contains(&g)
            || self.st().echo_wait.contains_key(&g)
        {
            return;
        }
        if self.state_of(g).await != Some(2) {
            return;
        }
        let mut rows = self.outbox(g).await;
        {
            // inQueueOrder: rows this engine queued keep their order; older rows keep the core's place ahead of them.
            let st = self.st();
            let rank = |m: &Gid| {
                st.queued
                    .iter()
                    .position(|q| q == m)
                    .map_or(-1, |p| p as i64)
            };
            rows.sort_by_key(|r| rank(&r.msg_id));
        }
        if let Some(in_flight) = rows.iter().find(|x| x.state == 1) {
            let msg = in_flight.msg_id;
            self.after_lost_response(g, msg).await;
            return;
        }
        let row = {
            let st = self.st();
            if st.slow_wait.contains_key(&g) {
                // foldBesideWait: only a fold (types 1..6) not itself slowed and not aimed at a queued post.
                let mut pick = None;
                for x in rows.iter().filter(|x| x.state == 0 && x.kind != 0) {
                    if st.slowed.contains(&x.msg_id) {
                        break;
                    }
                    let targets_queued = x.reply_to.is_some_and(|t| {
                        rows.iter()
                            .any(|y| y.kind == 0 && y.state == 0 && y.msg_id == t)
                    });
                    if !targets_queued {
                        pick = Some(x.clone());
                        break;
                    }
                }
                pick
            } else {
                rows.iter().find(|x| x.state == 0).cloned()
            }
        };
        let Some(row) = row else { return };
        let msg = row.msg_id;
        let reframe = (g, msg);
        if self.st().trust_retried.get(&reframe) == Some(&true) {
            return;
        }
        let sealed = with_core(&self.core, move |c| c.send_encrypt(&msg)).await;
        let body = match sealed {
            Ok(raw) => match value::decode(&raw).ok().and_then(|v| {
                v.as_array()
                    .filter(|a| a.len() == 2)
                    .and_then(|a| a[1].as_bytes().map(<[u8]>::to_vec))
            }) {
                Some(body) => body,
                None => return,
            },
            Err(e) if e.code == "E_CORE_STATE" => {
                let group = self.row(g).await;
                if group.as_ref().is_some_and(|r| r.pending_commit) {
                    return;
                }
                if group.is_some_and(|r| r.proposals_pending > 0) {
                    if self.st().quiet.contains(&g) {
                        return;
                    }
                    if !self.clone().commit_now(g, 0).await {
                        self.st().quiet.insert(g);
                    }
                    self.request_drain(g);
                }
                return;
            }
            Err(e)
                if LEAF_VERDICTS.contains(&e.code)
                    && !self.st().trust_retried.contains_key(&reframe) =>
            {
                // The leaf may be listed in a newer list than the cached one: refresh the group's trust once.
                self.st().trust_retried.insert(reframe, true);
                let this = Arc::clone(&self);
                tokio::spawn(async move {
                    this.refresh_trust().await;
                    if let Some(p) = this.st().trust_retried.get_mut(&reframe) {
                        *p = false;
                    }
                    if !this.stopped() {
                        this.request_drain(g);
                    }
                });
                return;
            }
            Err(e) => {
                let code = e.code;
                let _ = with_core(&self.core, move |c| c.send_fail(&msg, code)).await;
                {
                    let mut st = self.st();
                    st.reframed_once.remove(&reframe);
                    st.slowed.remove(&msg);
                    st.trust_retried.remove(&reframe);
                }
                self.hooks.group_changed(g);
                self.request_drain(g);
                return;
            }
        };
        match self.api.post_message(&g, body, &msg).await {
            Ok(answer) => {
                if self.stopped() {
                    return;
                }
                if with_core(&self.core, move |c| c.send_confirm(&msg, &answer))
                    .await
                    .is_err()
                {
                    if !self
                        .outbox(g)
                        .await
                        .iter()
                        .any(|x| x.msg_id == msg && x.state != 2)
                    {
                        self.st().reframed_once.remove(&reframe);
                    }
                    self.hooks.group_changed(g);
                    self.st().count425.remove(&msg);
                    self.request_catch_up(g);
                    self.request_drain(g);
                    return;
                }
                {
                    let mut st = self.st();
                    st.reframed_once.remove(&reframe);
                    st.trust_retried.remove(&reframe);
                    st.count425.remove(&msg);
                    st.queued.retain(|q| *q != msg);
                    st.slowed.remove(&msg);
                }
                self.hooks.group_changed(g);
                self.request_drain(g);
            }
            Err(e) => {
                if self.stopped() {
                    return;
                }
                self.on_post_refused(g, &row, e).await;
            }
        }
    }

    /// send.ts `drainOne`'s refusal half.
    async fn on_post_refused(
        self: Arc<Self>,
        g: Gid,
        row: &crate::state::messages::OutboxRow,
        e: ApiError,
    ) {
        let msg = row.msg_id;
        let reframe = (g, msg);
        let requeue = |this: &Arc<Self>| {
            let core = this.core.clone();
            async move {
                let _ = with_core(&core, move |c| c.send_requeue(&msg)).await;
            }
        };
        let fail = |this: &Arc<Self>, code: String| {
            let core = this.core.clone();
            {
                let mut st = this.st();
                st.reframed_once.remove(&reframe);
                st.slowed.remove(&msg);
            }
            async move {
                let _ = with_core(&core, move |c| c.send_fail(&msg, &code)).await;
            }
        };
        let epoch_refused = e.is(422, "E_COMMIT_INVALID")
            && e.extra.first().and_then(Value::as_text) == Some("epoch");
        if epoch_refused && !self.st().reframed_once.contains(&reframe) {
            // Refused before it was stored: re-frame the same row at the current epoch, once.
            let reached = self.clone().catch_up_now(g, Source::CatchUp).await;
            let current = self.outbox(g).await.into_iter().find(|x| x.msg_id == msg);
            if !reached
                || current.as_ref().is_none_or(|r| r.state != 1)
                || self.state_of(g).await != Some(2)
            {
                if current.is_some_and(|r| r.state == 1) {
                    fail(&self, e.code.clone()).await;
                }
                self.hooks.group_changed(g);
                self.request_drain(g);
                return;
            }
            requeue(&self).await;
            self.st().reframed_once.insert(reframe);
            self.hooks.group_changed(g);
            self.request_drain(g);
            return;
        }
        match e.status {
            429 => {
                // Slow mode: the row goes back, the group posts nothing until the later deadline, folds beside it go on.
                requeue(&self).await;
                self.hooks.group_changed(g);
                let wait = Duration::from_millis(e.retry_after_ms.unwrap_or(1000));
                let until = Instant::now() + wait;
                let later = {
                    let mut st = self.st();
                    st.slowed.insert(msg);
                    st.slow_wait.get(&g).is_none_or(|(_, old)| until > *old)
                };
                if later {
                    let handle = self.arm(wait, move |this| async move {
                        this.st().slow_wait.remove(&g);
                        this.request_drain(g);
                    });
                    if let Some((old, _)) = self.st().slow_wait.insert(g, (handle, until)) {
                        old.abort();
                    }
                }
                self.request_drain(g);
            }
            425 => {
                requeue(&self).await;
                self.hooks.group_changed(g);
                let n = {
                    let mut st = self.st();
                    let n = st.count425.entry(msg).or_default();
                    *n += 1;
                    *n
                };
                if n >= COMMIT_RETRY_MAX {
                    fail(&self, "E_COMMIT_REQUIRED".into()).await;
                    self.hooks.group_changed(g);
                    self.request_drain(g);
                } else {
                    let handle = self.arm(MEMBERSHIP_WAIT, move |this| async move {
                        this.st().epoch_wait.remove(&g);
                        this.request_commit(g, 0);
                        this.request_drain(g);
                    });
                    if let Some(old) = self.st().epoch_wait.insert(g, handle) {
                        old.abort();
                    }
                }
            }
            403 if e.code == "E_LEAF_NOT_CURRENT" => {
                requeue(&self).await;
                self.hooks.group_changed(g);
                if !self.st().resync_tried.contains(&g) {
                    self.clone().resync_group(g, false).await;
                }
            }
            401 => {
                requeue(&self).await;
                self.hooks.group_changed(g);
            }
            // 502 and 504 come from a proxy and say nothing about whether the instance stored it.
            0 | 500 | 502 | 504 => self.after_lost_response(g, msg).await,
            _ => {
                fail(&self, e.code.clone()).await;
                self.hooks.group_changed(g);
                self.request_drain(g);
            }
        }
    }

    // ---- trust ----

    /// The controller's `refreshTrust`: reads the signed device lists of the leaves the core cannot judge yet, and
    /// releases the rows they held.
    pub async fn refresh_trust(&self) {
        if self.stopped() {
            return;
        }
        if trust::refresh_once(&self.api, &self.core, &self.trust, now_s()).await {
            self.hooks.trust_changed();
            for g in self.groups().await.iter().filter(|g| g.state == 2) {
                self.hooks.group_changed(g.group_id);
            }
        }
    }

    /// Forgets which leaves were fetched, so the next refresh reads them all again (a new ready).
    pub async fn reset_trust_memory(&self) {
        *self.trust.lock().await = trust::TrustMemory::default();
    }
}

/// engine.ts `applyHook`'s `source`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Source {
    Frame,
    CatchUp,
    Commit,
}

#[cfg(test)]
mod tests;
