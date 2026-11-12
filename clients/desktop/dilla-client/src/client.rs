//! A running headless client for one instance: the store (Task 6), the HTTP transport (Task 7) and the gateway (Task 8)
//! brought together by the sync engine ([`crate::sync`]), on its own tokio runtime. The UI observes [`AppState`]
//! through [`Client::state`] and acts through the methods here; each action answers on a oneshot so the UI never waits
//! on the network.
//!
//! What the worker's controller (`packages/client-core/src/worker/controller.ts`) does around the engine, narrowed to
//! this task: on every ready the communities, channels and members are read again and the expected groups handed to the
//! engine; every group change re-reads that channel's timeline and the badges; a focused channel is marked read as it
//! changes; a refusal moves the account to its revoked, factor-setup or version-refused phase.

use std::collections::{HashMap, HashSet};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use tokio::sync::{mpsc, oneshot, watch};

use crate::auth::AuthToken;
use crate::gateway::{GatewayOptions, Refusal, WsGateway};
use crate::http::ReqwestHttp;
use crate::seams::{Gateway, InstanceConfig, Keychain};
use crate::state::channels::{
    channel_group, channel_group_state, decode_channels, decode_communities,
};
use crate::state::members::decode_members;
use crate::state::messages::{build_timeline, decode_outbox, decode_timeline};
use crate::state::unread::{build_badges, decode_activity};
use crate::state::{
    AccountPhase, AppState, Channel, ChannelId, CommunityId, ConnectionStatus, DeviceId, GroupId,
    MessageId, TIMELINE_PAGE, UserId,
};
use crate::store::{StoreError, open_store};
use crate::sync::api::{Api, CoreHandle, Identity, SessionError, try_core};
use crate::sync::requests::SocketRequests;
use crate::sync::{ConnectionEvent, Engine, ExpectedGroup, GroupRow, Membership, SyncHooks};

/// Why a client action failed.
#[derive(Debug, Clone, thiserror::Error)]
pub enum ClientError {
    #[error("the local store did not open: {0}")]
    Store(String),
    /// The store holds no finished sign-up or sign-in (identity phase 2).
    #[error("this instance has no signed-in account on this device")]
    NoIdentity,
    /// The instance no longer accepts this device: sign in again.
    #[error("the session was revoked")]
    Revoked,
    /// The account owes a second factor.
    #[error("the account owes a second factor")]
    FactorRequired,
    #[error("the instance could not be reached: {0}")]
    Network(String),
    /// A sync step refused, with its stable code (E_COMMIT_REQUIRED, E_CHANNEL_GONE, a core code, ...).
    #[error("{0}")]
    Sync(String),
    #[error("no such channel")]
    UnknownChannel,
    #[error("the client is stopped")]
    Stopped,
    #[error("the client runtime failed: {0}")]
    Runtime(String),
}

impl From<StoreError> for ClientError {
    fn from(e: StoreError) -> Self {
        Self::Store(e.to_string())
    }
}

impl From<crate::sync::SyncError> for ClientError {
    fn from(e: crate::sync::SyncError) -> Self {
        match e {
            crate::sync::SyncError::Stopped => Self::Stopped,
            crate::sync::SyncError::Api(a) if a.status == 0 && a.code == "E_NETWORK" => {
                Self::Network(a.detail)
            }
            other => Self::Sync(other.code()),
        }
    }
}

fn now_s() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |d| d.as_secs())
}

/// What the publisher is told.
enum Update {
    Group([u8; 16]),
    Membership([u8; 16], Membership),
    Connection(ConnectionEvent),
    FactorRequired,
    /// A re-establishment of a refused session ended.
    Renewed(Renewal),
    Structure,
    Loaded(Box<Loaded>),
    Focus(Option<ChannelId>),
    MarkRead(ChannelId, oneshot::Sender<Result<(), ClientError>>),
}

struct Loaded {
    communities: Vec<crate::state::Community>,
    channels: Vec<Channel>,
    members: HashMap<CommunityId, Vec<crate::state::Member>>,
}

/// The engine's hooks: each only queues an update for the publisher.
struct Hooks {
    tx: mpsc::UnboundedSender<Update>,
}

impl SyncHooks for Hooks {
    fn group_changed(&self, group: [u8; 16]) {
        let _ = self.tx.send(Update::Group(group));
    }
    fn membership(&self, group: [u8; 16], status: Membership) {
        let _ = self.tx.send(Update::Membership(group, status));
    }
    fn trust_changed(&self) {}
    fn connection(&self, event: ConnectionEvent) {
        let _ = self.tx.send(Update::Connection(event));
    }
    fn community_changed(&self, _community: [u8; 16]) {
        let _ = self.tx.send(Update::Structure);
    }
}

/// The gateway connection's owner: the one place a gateway is started, replaced or stopped.
struct Link {
    engine: Arc<Engine>,
    socket: Arc<SocketRequests>,
    auth: AuthToken,
    server: url::Url,
    options: GatewayOptions,
    gateway: Mutex<Option<Arc<WsGateway>>>,
    updates: mpsc::UnboundedSender<Update>,
    /// The app took the connection down (`Client::disconnect`) and has not asked for it again.
    app_offline: AtomicBool,
}

struct Inner {
    link: Arc<Link>,
    engine: Arc<Engine>,
    api: Arc<Api>,
    updates: mpsc::UnboundedSender<Update>,
    state: watch::Receiver<Arc<AppState>>,
}

/// A runtime that, unless handed on, is shut down in the background when dropped: safe on any thread, an async task's
/// included (a plain drop there panics).
struct RuntimeGuard(Option<tokio::runtime::Runtime>);

impl std::ops::Deref for RuntimeGuard {
    type Target = tokio::runtime::Runtime;
    fn deref(&self) -> &Self::Target {
        self.0
            .as_ref()
            .expect("the runtime is held until it is handed on")
    }
}

impl Drop for RuntimeGuard {
    fn drop(&mut self) {
        if let Some(rt) = self.0.take() {
            rt.shutdown_background();
        }
    }
}

/// One instance's running client. Dropping it stops the engine, closes the gateway and shuts its runtime down.
pub struct Client {
    runtime: Option<tokio::runtime::Runtime>,
    inner: Arc<Inner>,
}

impl Client {
    /// Opens the instance's store, establishes the device session (from the device key; a stored session is used
    /// while it is not within an hour of expiring), connects the gateway and starts syncing, on a runtime of its own.
    ///
    /// It blocks the calling thread until the session is established (a few round trips), so call it from the UI's
    /// thread or `spawn_blocking`, not from an async task where it would stall a worker. It never panics on a caller
    /// inside a tokio runtime: the wait runs on a thread of its own, outside any runtime context (tokio refuses a
    /// `block_on` inside one; `Handle::try_current` cannot tell that case from a legitimate `spawn_blocking` caller).
    /// A failure of that thread is [`ClientError::Runtime`]. A start that fails (offline, revoked, no identity, a store
    /// error) shuts the runtime it built down in the background, so failing does not panic there either.
    pub fn start(cfg: InstanceConfig, kc: Arc<dyn Keychain>) -> Result<Client, ClientError> {
        Self::start_with(cfg, kc, GatewayOptions::default())
    }

    /// [`Client::start`] with the gateway's timers and roots.
    pub fn start_with(
        cfg: InstanceConfig,
        kc: Arc<dyn Keychain>,
        options: GatewayOptions,
    ) -> Result<Client, ClientError> {
        let core: CoreHandle = Arc::new(Mutex::new(open_store(&cfg, kc.as_ref())?));
        let identity = {
            let guard = core
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            guard.identity().ok().and_then(|raw| Identity::decode(&raw))
        };
        let Some(identity) = identity.filter(|i| i.phase == 2) else {
            return Err(ClientError::NoIdentity);
        };
        let (Some(user_id), Some(device_id)) = (identity.user_id, identity.device_id) else {
            return Err(ClientError::NoIdentity);
        };

        let auth = AuthToken::anonymous();
        let http = ReqwestHttp::new(cfg.server.clone(), auth.clone())
            .map_err(|e| ClientError::Network(e.to_string()))?;
        // Built only now, and held by a guard until the client owns it: a runtime dropped on a caller's async task
        // panics (tokio refuses a blocking shutdown there), so every early return below shuts it down in the background.
        let mut runtime = RuntimeGuard(Some(
            tokio::runtime::Builder::new_multi_thread()
                .worker_threads(2)
                .enable_all()
                .thread_name(format!("dilla-client-{}", cfg.id.0))
                .build()
                .map_err(|e| ClientError::Runtime(e.to_string()))?,
        ));
        let socket = SocketRequests::new();
        let (updates, updates_rx) = mpsc::unbounded_channel();
        let refusals = updates.clone();
        let api = Arc::new(Api::new(
            http,
            socket.clone(),
            auth.clone(),
            core.clone(),
            Arc::new(move |code: &str| {
                if code == "E_FACTOR_REQUIRED" {
                    let _ = refusals.send(Update::FactorRequired);
                }
            }),
        ));
        // On a thread of its own, outside any runtime context the caller may be in (see `start`).
        let instance = std::thread::scope(|s| {
            s.spawn(|| {
                runtime.block_on(async {
                    api.ensure_session(now_s()).await.map_err(|e| match e {
                        SessionError::NoIdentity => ClientError::NoIdentity,
                        SessionError::Revoked | SessionError::Scope => ClientError::Revoked,
                        SessionError::Api(a) if a.status == 0 => ClientError::Network(a.detail),
                        SessionError::Api(a) => ClientError::Sync(a.code),
                        SessionError::Core(c) => ClientError::Sync(c),
                    })?;
                    api.get_instance()
                        .await
                        .map_err(|e| ClientError::Network(e.to_string()))
                })
            })
            .join()
        })
        .map_err(|_| ClientError::Runtime("the start thread panicked".into()))??;

        let hooks = Arc::new(Hooks {
            tx: updates.clone(),
        });
        let engine = Engine::new(core.clone(), api.clone(), hooks, instance);
        let initial = AppState {
            account: crate::state::AccountState {
                phase: AccountPhase::Ready,
                user_id: Some(UserId(user_id)),
                device_id: Some(DeviceId(device_id)),
                username: identity.username.clone(),
            },
            ..AppState::default()
        };
        let (state_tx, state_rx) = watch::channel(Arc::new(initial.clone()));
        let link = Arc::new(Link {
            engine: engine.clone(),
            socket,
            auth,
            server: cfg.server.clone(),
            options,
            gateway: Mutex::new(None),
            app_offline: AtomicBool::new(false),
            updates: updates.clone(),
        });
        let inner = Arc::new(Inner {
            link: link.clone(),
            engine: engine.clone(),
            api: api.clone(),
            updates: updates.clone(),
            state: state_rx,
        });
        let publisher = Publisher {
            state: initial,
            tx: state_tx,
            core,
            engine,
            api,
            link: link.clone(),
            updates,
            own_user: user_id,
            own_device: device_id,
            focus: None,
            not_member: HashSet::new(),
            resyncing: HashSet::new(),
            renew: RenewGate::default(),
        };
        runtime.spawn(watch_poison(
            publisher.core.clone(),
            publisher.updates.clone(),
        ));
        runtime.spawn(publisher.run(updates_rx));
        {
            let _enter = runtime.enter();
            link.connect();
        }
        Ok(Client {
            runtime: runtime.0.take(),
            inner,
        })
    }

    /// The observable state: a new snapshot is published on every change.
    pub fn state(&self) -> watch::Receiver<Arc<AppState>> {
        self.inner.state.clone()
    }

    fn spawn<T: Send + 'static>(
        &self,
        f: impl std::future::Future<Output = Result<T, ClientError>> + Send + 'static,
    ) -> oneshot::Receiver<Result<T, ClientError>> {
        let (tx, rx) = oneshot::channel();
        match &self.runtime {
            Some(rt) => {
                rt.spawn(async move {
                    let _ = tx.send(guarded(f).await);
                });
            }
            None => {
                let _ = tx.send(Err(ClientError::Stopped));
            }
        }
        rx
    }

    /// Queues `text` as a post in `channel` (opening the channel's text group first: a Welcome, a join, or a new group).
    /// Answers the message id once the post is in the outbox; the timeline shows it pending until the instance stored
    /// it.
    pub fn send_text(
        &self,
        channel: ChannelId,
        text: String,
    ) -> oneshot::Receiver<Result<MessageId, ClientError>> {
        let inner = self.inner.clone();
        self.spawn(async move {
            let g = inner.open(channel).await?;
            let msg = inner.engine.send_text(g, text).await?;
            Ok(MessageId(msg))
        })
    }

    /// Opens `channel`'s text group (join or register), so its timeline is kept.
    pub fn open_channel(&self, channel: ChannelId) -> oneshot::Receiver<Result<(), ClientError>> {
        let inner = self.inner.clone();
        self.spawn(async move { inner.open(channel).await.map(|_| ()) })
    }

    /// The channel the UI shows (or none): it is marked read now and whenever it changes while focused.
    pub fn focus(&self, channel: Option<ChannelId>) {
        let _ = self.inner.updates.send(Update::Focus(channel));
    }

    /// Marks everything `channel` holds as read.
    pub fn mark_read(&self, channel: ChannelId) -> oneshot::Receiver<Result<(), ClientError>> {
        let (tx, rx) = oneshot::channel();
        if self
            .inner
            .updates
            .send(Update::MarkRead(channel, tx))
            .is_err()
        {
            let (tx, rx) = oneshot::channel();
            let _ = tx.send(Err(ClientError::Stopped));
            return rx;
        }
        rx
    }

    /// Reads the communities, channels and members again.
    pub fn refresh(&self) {
        let _ = self.inner.updates.send(Update::Structure);
    }

    /// Drops the gateway connection and keeps it down (a laptop asleep, a lost network): requests wait, the timeline
    /// stays as it is.
    pub fn disconnect(&self) {
        self.inner.link.app_offline.store(true, Ordering::Release);
        self.inner.link.disconnect();
    }

    /// Connects the gateway again after [`Client::disconnect`]: a fresh session, then the ready's catch-up.
    pub fn reconnect(&self) {
        if let Some(rt) = &self.runtime {
            let _enter = rt.enter();
            self.inner.link.app_offline.store(false, Ordering::Release);
            self.inner.link.connect();
        }
    }

    /// Stops syncing and closes the gateway; the store stays as it is.
    pub fn stop(mut self) {
        self.shutdown();
    }

    fn shutdown(&mut self) {
        self.inner.engine.stop();
        self.inner.link.disconnect();
        self.inner.link.socket.close();
        if let Some(rt) = self.runtime.take() {
            rt.shutdown_background();
        }
    }
}

impl Drop for Client {
    fn drop(&mut self) {
        self.shutdown();
    }
}

impl Link {
    /// A new gateway on the shared session token; its events go to the engine's event loop. Must run inside the
    /// client's runtime.
    fn connect(&self) {
        let mut slot = self
            .gateway
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        if slot.is_some() || self.engine.stopped() {
            return;
        }
        let gateway = Arc::new(WsGateway::connect_with(
            self.server.clone(),
            self.auth.clone(),
            self.options.clone(),
        ));
        let events = gateway.events();
        self.engine.set_gateway(Some(gateway.clone()));
        self.socket.attach(Some(gateway.clone()));
        self.socket.on_starting();
        tokio::spawn(self.engine.clone().run_events(gateway.clone(), events));
        let _ = self.updates.send(Update::Connection(ConnectionEvent::Down));
        *slot = Some(gateway);
    }

    /// Stops the gateway; its late events are ignored from here on.
    fn disconnect(&self) {
        let gateway = self
            .gateway
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .take();
        if let Some(gateway) = gateway {
            self.engine.set_gateway(None);
            gateway.stop();
            self.socket.attach(None);
            self.socket.on_down();
            let _ = self.updates.send(Update::Connection(ConnectionEvent::Down));
        }
    }

    /// A gateway that stopped for good is replaced by a new one (after a re-authentication), unless the app took the
    /// connection down meanwhile ([`Client::disconnect`]): its own reconnect then uses the renewed session.
    fn restart(&self) {
        if self.app_offline.load(Ordering::Acquire) {
            return;
        }
        self.disconnect();
        self.connect();
    }
}

impl Inner {
    /// The channel's text group, opened.
    async fn open(&self, channel: ChannelId) -> Result<[u8; 16], ClientError> {
        let known = self.state.borrow().channel(channel).cloned();
        let known = match known {
            Some(c) => c,
            None => {
                // Not listed yet (created a moment ago): read the structure once.
                let loaded = load_structure(&self.api).await?;
                loaded
                    .channels
                    .into_iter()
                    .find(|c| c.id == channel)
                    .ok_or(ClientError::UnknownChannel)?
            }
        };
        if !known.is_text() {
            return Err(ClientError::UnknownChannel);
        }
        let g = self
            .engine
            .open_channel(
                Some(known.community_id.0),
                channel.0,
                known.text_group_id.map(|g| g.0),
            )
            .await?;
        let _ = self.updates.send(Update::Group(g));
        Ok(g)
    }
}

async fn load_structure(api: &Api) -> Result<Loaded, ClientError> {
    let net = |e: crate::sync::api::ApiError| ClientError::Sync(e.code);
    let communities = decode_communities(&api.list_communities().await.map_err(net)?)
        .map_err(|e| ClientError::Sync(e.to_string()))?;
    let mut channels = Vec::new();
    let mut members = HashMap::new();
    for c in &communities {
        let raw = api.list_channels(&c.id.0).await.map_err(net)?;
        let mut list = decode_channels(c.id, &raw).map_err(|e| ClientError::Sync(e.to_string()))?;
        list.sort_by_key(|ch| ch.position);
        channels.extend(list);
        let mut all = Vec::new();
        for page in api.list_members(&c.id.0).await.map_err(net)? {
            all.extend(decode_members(&page).map_err(|e| ClientError::Sync(e.to_string()))?);
        }
        members.insert(c.id, all);
    }
    Ok(Loaded {
        communities,
        channels,
        members,
    })
}

/// An action's answer, also when it unwinds (a core call on a poisoned core ends its caller): then
/// [`ClientError::Stopped`], never a dropped oneshot.
async fn guarded<T>(
    f: impl std::future::Future<Output = Result<T, ClientError>>,
) -> Result<T, ClientError> {
    use futures_util::FutureExt;
    std::panic::AssertUnwindSafe(f)
        .catch_unwind()
        .await
        .unwrap_or(Err(ClientError::Stopped))
}

/// What a refused session (gateway 4004, 4003, upgrade 401) leads to.
#[derive(Debug, PartialEq, Eq)]
enum OnRefused {
    /// Start a renewal.
    Renew,
    /// A renewal is already out: its verdict decides (a gateway the app reconnected on the old token was refused).
    Wait,
    /// This ready's one renewal was spent and the new session was refused too.
    SignOut,
}

/// What a finished renewal leads to.
#[derive(Debug, PartialEq, Eq)]
enum AfterRenewal {
    /// Start a gateway on the new session.
    Restart,
    /// The app took the connection down meanwhile: its own reconnect uses the new session.
    Stay,
    SignOut,
}

/// The renewals of refused sessions: one per ready, one at a time.
#[derive(Debug, Default)]
struct RenewGate {
    in_flight: bool,
    /// A renewal ran since the last ready.
    spent: bool,
}

impl RenewGate {
    fn refused(&mut self) -> OnRefused {
        if self.in_flight {
            OnRefused::Wait
        } else if self.spent {
            OnRefused::SignOut
        } else {
            self.in_flight = true;
            self.spent = true;
            OnRefused::Renew
        }
    }

    fn done(&mut self, outcome: Renewal, app_offline: bool) -> AfterRenewal {
        self.in_flight = false;
        match outcome {
            Renewal::SignedOut => AfterRenewal::SignOut,
            Renewal::Renewed if app_offline => AfterRenewal::Stay,
            Renewal::Renewed => AfterRenewal::Restart,
        }
    }

    fn ready(&mut self) {
        self.spent = false;
    }
}

/// The account phase once a gateway is ready: a ready never undoes a final verdict (revoked, a failed core) nor the
/// factor the account owes.
fn phase_on_ready(current: AccountPhase) -> AccountPhase {
    match current {
        AccountPhase::Revoked | AccountPhase::Failed | AccountPhase::FactorSetup => current,
        _ => AccountPhase::Ready,
    }
}

/// How a re-establishment of a refused session ended.
#[derive(Debug, PartialEq, Eq)]
enum Renewal {
    Renewed,
    SignedOut,
}

/// The wait before the `n`th retry of a re-establishment that did not reach a verdict: 1 s doubling to 30 s.
fn renewal_backoff(n: u32) -> Duration {
    Duration::from_secs((1u64 << n.saturating_sub(1).min(5)).min(30))
}

/// Establishes a session until the instance gives a verdict: success renews, a refusal of the device (revoked, wrong
/// scope, no identity) signs out, and anything else (the instance unreachable, a 5xx, a store hiccup) is retried after
/// `backoff(n)`.
async fn renew<F, Fut>(mut establish: F, backoff: impl Fn(u32) -> Duration) -> Renewal
where
    F: FnMut() -> Fut,
    Fut: std::future::Future<Output = Result<(), SessionError>>,
{
    let mut retries = 0u32;
    loop {
        match establish().await {
            Ok(()) => return Renewal::Renewed,
            Err(SessionError::Revoked | SessionError::Scope | SessionError::NoIdentity) => {
                return Renewal::SignedOut;
            }
            Err(SessionError::Api(_) | SessionError::Core(_)) => {
                retries += 1;
                tokio::time::sleep(backoff(retries)).await;
            }
        }
    }
}

/// Owns the [`AppState`] and publishes every change.
struct Publisher {
    state: AppState,
    tx: watch::Sender<Arc<AppState>>,
    core: CoreHandle,
    engine: Arc<Engine>,
    api: Arc<Api>,
    link: Arc<Link>,
    updates: mpsc::UnboundedSender<Update>,
    own_user: [u8; 16],
    own_device: [u8; 16],
    focus: Option<ChannelId>,
    not_member: HashSet<GroupId>,
    resyncing: HashSet<GroupId>,
    /// The renewals of refused sessions (one per ready, one at a time).
    renew: RenewGate,
}

impl Publisher {
    async fn run(mut self, mut rx: mpsc::UnboundedReceiver<Update>) {
        while let Some(first) = rx.recv().await {
            let mut batch = vec![first];
            while let Ok(more) = rx.try_recv() {
                batch.push(more);
            }
            let mut groups: HashSet<[u8; 16]> = HashSet::new();
            let mut structure = false;
            for update in batch {
                match update {
                    Update::Group(g) => {
                        groups.insert(g);
                    }
                    Update::Membership(g, status) => {
                        let g = GroupId(g);
                        self.resyncing.remove(&g);
                        self.not_member.remove(&g);
                        match status {
                            Membership::Resyncing => {
                                self.resyncing.insert(g);
                            }
                            Membership::NotMember => {
                                self.not_member.insert(g);
                            }
                            Membership::Member => {}
                        }
                        groups.insert(g.0);
                    }
                    Update::Connection(event) => structure |= self.on_connection(event),
                    Update::FactorRequired => {
                        self.state.account.phase = AccountPhase::FactorSetup;
                    }
                    Update::Renewed(outcome) => {
                        let offline = self.link.app_offline.load(Ordering::Acquire);
                        match self.renew.done(outcome, offline) {
                            AfterRenewal::Restart => self.link.restart(),
                            AfterRenewal::Stay => {}
                            AfterRenewal::SignOut => self.sign_out(),
                        }
                    }
                    Update::Structure => structure = true,
                    Update::Loaded(loaded) => self.on_loaded(*loaded).await,
                    Update::Focus(channel) => {
                        self.focus = channel;
                        if let Some(c) = channel {
                            let _ = self.mark_read(c).await;
                            groups.insert([0; 16]);
                        }
                    }
                    Update::MarkRead(channel, done) => {
                        let _ = done.send(self.mark_read(channel).await);
                        groups.insert([0; 16]);
                    }
                }
            }
            if self.core.is_poisoned() {
                self.fail();
                self.publish();
                // Every action still queued answers Stopped (none is dropped unanswered); later ones find the channel
                // closed and answer Stopped themselves.
                rx.close();
                while let Ok(update) = rx.try_recv() {
                    if let Update::MarkRead(_, done) = update {
                        let _ = done.send(Err(ClientError::Stopped));
                    }
                }
                return;
            }
            if structure {
                self.load_structure();
            }
            if !groups.is_empty() {
                self.refresh_groups(&groups).await;
            }
            self.publish();
        }
    }

    fn publish(&self) {
        self.tx.send_replace(Arc::new(self.state.clone()));
    }

    /// The device is revoked: final for this client (a later ready never says otherwise, see [`phase_on_ready`]).
    fn sign_out(&mut self) {
        self.link.disconnect();
        self.state.connection.status = ConnectionStatus::Offline;
        self.state.account.phase = AccountPhase::Revoked;
    }

    /// Answers whether the structure is to be read again.
    fn on_connection(&mut self, event: ConnectionEvent) -> bool {
        match event {
            ConnectionEvent::Ready(info) => {
                self.renew.ready();
                self.state.connection.status = ConnectionStatus::Online;
                self.state.connection.generation = Some(info.generation);
                self.state.connection.wire_version = Some(info.wire);
                self.state.connection.readies += 1;
                self.state.account.phase = phase_on_ready(self.state.account.phase);
                let engine = self.engine.clone();
                tokio::spawn(async move { engine.reset_trust_memory().await });
                true
            }
            ConnectionEvent::Down => {
                self.state.connection.status = ConnectionStatus::Connecting;
                self.state.connection.wire_version = None;
                false
            }
            // controller.ts `onRevokedSocket`: every refused session (4004 from the session sweep, which also closes a
            // session past its expiry; 4003; an upgrade 401) is established again from the device key, and the gateway
            // started on the new session. Only an establishment the instance refuses (the device's challenge is not
            // answered, or the scope is wrong) signs out; a network failure or a 5xx is tried again after a backoff,
            // connecting meanwhile. One renewal per ready: a second refusal before the next ready signs out. While a
            // renewal is out, a refusal (a gateway the app reconnected on the old token) waits for its verdict.
            ConnectionEvent::Refused(Refusal::Revoked, _) => {
                match self.renew.refused() {
                    OnRefused::Renew => {
                        self.state.connection.status = ConnectionStatus::Connecting;
                        let (api, updates) = (self.api.clone(), self.updates.clone());
                        tokio::spawn(async move {
                            let outcome = renew(|| api.establish(), renewal_backoff).await;
                            let _ = updates.send(Update::Renewed(outcome));
                        });
                    }
                    OnRefused::Wait => {
                        self.state.connection.status = ConnectionStatus::Connecting;
                    }
                    OnRefused::SignOut => self.sign_out(),
                }
                false
            }
            ConnectionEvent::Refused(refusal, _) => {
                self.state.connection.status = ConnectionStatus::Offline;
                self.state.account.phase = match refusal {
                    Refusal::Revoked => AccountPhase::Revoked,
                    Refusal::SecondFactorRequired => AccountPhase::FactorSetup,
                    Refusal::UpgradeRequired => AccountPhase::VersionRefused { upgrade: true },
                    Refusal::InstanceOutdated | Refusal::VersionUnsupported => {
                        AccountPhase::VersionRefused { upgrade: false }
                    }
                };
                false
            }
        }
    }

    /// Reads the structure off the publisher's loop; the result comes back as `Loaded`.
    fn load_structure(&self) {
        let api = self.api.clone();
        let updates = self.updates.clone();
        tokio::spawn(async move {
            if let Ok(loaded) = load_structure(&api).await {
                let _ = updates.send(Update::Loaded(Box::new(loaded)));
            }
        });
    }

    async fn on_loaded(&mut self, loaded: Loaded) {
        let policy = self.engine.instance().policy_version;
        let expected = loaded
            .channels
            .iter()
            .filter(|c| c.is_text())
            .filter_map(|c| {
                c.text_group_id.map(|g| ExpectedGroup {
                    group_id: g.0,
                    community_id: Some(c.community_id.0),
                    channel_id: c.id.0,
                    policy_version: policy,
                })
            })
            .collect();
        self.state.communities = loaded.communities;
        self.state.channels = loaded.channels;
        self.state.members = loaded.members;
        self.engine.set_expected(expected);
        // Every channel's group state and timeline, and the badges.
        let all: HashSet<[u8; 16]> = self.state.channels.iter().map(|c| c.id.0).collect();
        self.refresh_channels(&all).await;
    }

    /// Re-reads what the changed groups show: the bound channels' timelines and every badge.
    async fn refresh_groups(&mut self, groups: &HashSet<[u8; 16]>) {
        let rows = self.engine.groups().await;
        let channels: HashSet<[u8; 16]> = rows
            .iter()
            .filter(|r| r.kind == 0 && groups.contains(&r.group_id))
            .map(|r| r.target_id)
            .collect();
        // A focused channel that changed is read as it changes.
        if let Some(focus) = self.focus
            && channels.contains(&focus.0)
        {
            let _ = self.mark_read(focus).await;
        }
        self.refresh_channels(&channels).await;
    }

    async fn refresh_channels(&mut self, channels: &HashSet<[u8; 16]>) {
        let rows = self.engine.groups().await;
        let (not_member, resyncing) = (self.not_member.clone(), self.resyncing.clone());
        for ch in &mut self.state.channels {
            ch.group = channel_group_state(ch, &rows, &|g| not_member.contains(g), &|g| {
                resyncing.contains(g)
            });
        }
        for channel in channels {
            let id = ChannelId(*channel);
            let group_state = self.state.channel(id).map(|c| c.group).unwrap_or_default();
            let Some(group) = channel_group(id, &rows).filter(|g| g.state != 4).cloned() else {
                continue;
            };
            let timeline = self.read_timeline(id, group_state, &group).await;
            if let Some(t) = timeline {
                self.state.timelines.insert(id, t);
            }
        }
        let channel_of: HashMap<GroupId, ChannelId> = rows
            .iter()
            .filter(|r| r.kind == 0)
            .map(|r| (GroupId(r.group_id), ChannelId(r.target_id)))
            .collect();
        if let Ok(Ok(Ok(activity))) = try_core(&self.core, |c| {
            c.activity().map(|raw| decode_activity(&raw))
        })
        .await
        {
            self.state.unread = build_badges(&activity, &channel_of);
        }
    }

    async fn read_timeline(
        &self,
        channel: ChannelId,
        group_state: crate::state::ChannelGroupState,
        group: &GroupRow,
    ) -> Option<crate::state::Timeline> {
        let g = group.group_id;
        let (rows, outbox) = try_core(&self.core, move |c| {
            (c.timeline(&g, 0, TIMELINE_PAGE), c.outbox(&g))
        })
        .await
        .ok()?;
        let rows = decode_timeline(&rows.ok()?).ok()?;
        let outbox = outbox
            .ok()
            .and_then(|raw| decode_outbox(&raw).ok())
            .unwrap_or_default();
        Some(build_timeline(
            channel,
            group_state,
            &rows,
            &outbox,
            self.own_user,
            self.own_device,
            TIMELINE_PAGE,
        ))
    }

    /// controller.ts `markRead`: the marker goes to the group's head (the core clamps it), so the thread replies the
    /// main timeline does not list are read too.
    async fn mark_read(&self, channel: ChannelId) -> Result<(), ClientError> {
        let rows = self.engine.groups().await;
        let Some(group) = channel_group(channel, &rows)
            .filter(|g| g.state != 4)
            .cloned()
        else {
            return Ok(());
        };
        let g = group.group_id;
        let newest = try_core(&self.core, move |c| c.timeline(&g, 0, 1))
            .await
            .ok()
            .and_then(Result::ok)
            .and_then(|raw| decode_timeline(&raw).ok())
            .and_then(|rows| rows.iter().map(|r| r.seq).max());
        let Some(newest) = newest else {
            return Ok(());
        };
        let head = group.next_seq.saturating_sub(1).max(newest);
        let now = now_s();
        try_core(&self.core, move |c| c.mark_read(&g, head, now))
            .await
            .map_err(|e| ClientError::Sync(e.to_string()))?
            .map_err(|e| ClientError::Sync(e.code.to_owned()))
    }

    /// The core is poisoned: stop syncing for good (no job uses it again), drop the gateway, and say so.
    fn fail(&mut self) {
        self.engine.stop();
        self.link.disconnect();
        self.link.socket.close();
        self.state.connection.status = ConnectionStatus::Offline;
        self.state.account.phase = AccountPhase::Failed;
    }
}

/// Wakes the publisher once a second while the core's lock is not poisoned; a panicked core call ends no update of its
/// own, so this is what turns it into [`AccountPhase::Failed`].
async fn watch_poison(core: CoreHandle, updates: mpsc::UnboundedSender<Update>) {
    loop {
        tokio::time::sleep(Duration::from_secs(1)).await;
        if core.is_poisoned() {
            let _ = updates.send(Update::Structure);
            return;
        }
    }
}

#[cfg(test)]
mod tests;
