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
use std::sync::{Arc, Mutex, OnceLock, Weak};
use std::time::Duration;

use tokio::sync::{mpsc, oneshot, watch};

use crate::account::Upkeep;
use crate::auth::AuthToken;
use crate::features::Area;
use crate::gateway::frames::Frame;
use crate::gateway::{GatewayOptions, Refusal, WsGateway};
use crate::http::ReqwestHttp;
use crate::prefs::Prefs;
use crate::seams::{Gateway, InstanceConfig, Keychain};
use crate::state::channels::{
    channel_group, channel_group_state, decode_channels, decode_communities,
};
use crate::state::members::decode_members;
use crate::state::messages::TimelineUnavailable;
use crate::state::messages::{build_timeline, decode_outbox, decode_timeline};
use crate::state::unread::{build_badges, decode_activity};
use crate::state::{
    AccountPhase, AppState, Channel, ChannelId, CommunityId, ConnectionStatus, DeviceId, GroupId,
    MessageId, TIMELINE_PAGE, UserId,
};
use crate::store::{StoreError, open_store};
use crate::sync::api::{Api, CoreHandle, Identity, SessionError, try_core, with_core};
use crate::sync::removals::FeedFailure;
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
    /// An area's change to its own slice ([`Inner::publish`]).
    Apply(Box<dyn FnOnce(&mut AppState) + Send>),
    /// The account's second factor was set up ([`Client::factor_completed`]).
    FactorCompleted,
    /// The instance's answer to the factor read [`Client::factor_completed`] starts: whether a factor is still owed.
    FactorChecked(bool),
    /// A job of the engine or an action panicked without poisoning the core: the client fails, as for a poisoned core.
    Panicked,
    /// The removal feeds of ready `generation` were read; the answers are the groups whose feed is still unread.
    RemovalsRead {
        generation: u64,
        answers: FeedAnswers,
    },
    /// A group's removal feed was read to its end ([`SyncHooks::removals_read`]).
    RemovalsCleared([u8; 16]),
    /// A retry of `groups`' feeds ended; the answers are those still unread.
    RemovalsRetried {
        groups: Vec<[u8; 16]>,
        answers: FeedAnswers,
    },
    /// The refreshed own device list leaves this device out (controller.ts `refreshOwnDeviceList` `!listed`): revoked.
    Unlisted,
}

struct Loaded {
    communities: Vec<crate::state::Community>,
    channels: Vec<Channel>,
    members: HashMap<CommunityId, Vec<crate::state::Member>>,
}

/// The engine's hooks: each only queues an update for the publisher, or hands an area its frame.
struct Hooks {
    tx: mpsc::UnboundedSender<Update>,
    /// The client, set once it is built (the engine, and so these hooks, come first). Weak: the client owns the engine.
    inner: Arc<OnceLock<Weak<Inner>>>,
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
        // Task 20: the Bot area's connect hooks (catalog sync and Bot user lookup on ready, secrets dropped on a refusal).
        if let Some(inner) = self.inner.get().and_then(Weak::upgrade) {
            crate::features::bot::on_connection(&inner, &event);
        }
        let _ = self.tx.send(Update::Connection(event));
    }
    fn community_changed(&self, _community: [u8; 16]) {
        let _ = self.tx.send(Update::Structure);
    }
    fn area_frame(&self, area: Area, frame: Frame) {
        if let Some(inner) = self.inner.get().and_then(Weak::upgrade) {
            crate::sync::route::dispatch(area, &inner, &frame);
        }
    }
    fn failed(&self) {
        let _ = self.tx.send(Update::Panicked);
    }
    fn removals_read(&self, group: [u8; 16]) {
        let _ = self.tx.send(Update::RemovalsCleared(group));
    }
    /// controller.ts `onUnexpectedWelcome`: the DM list is read again, so a DM another person opened is listed and its
    /// group joined at once rather than on the list's next poll.
    fn unexpected_welcome(&self, _group: [u8; 16]) {
        if let Some(inner) = self.inner.get().and_then(Weak::upgrade) {
            let reading = inner.clone();
            drop(inner.spawn(async move { crate::features::dms::reload(&reading).await }));
        }
    }
    fn posted(
        &self,
        group: [u8; 16],
        msg: [u8; 16],
        kind: u8,
        target: Option<[u8; 16]>,
        body: &str,
        seq: u64,
    ) {
        // Task 28 wave 2: a confirmed reaction may be a role panel action (controller.ts `onPosted`).
        if let Some(inner) = self.inner.get().and_then(Weak::upgrade) {
            crate::features::role_panels::on_posted(
                &inner,
                group,
                msg,
                kind,
                target,
                body.to_owned(),
                seq,
            );
        }
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
    /// Run before every connection attempt: the instance signer pin ([`pin_check`]).
    pre_connect: Option<crate::gateway::PreConnect>,
}

/// What every handle of the client shares: the area features (`crate::features`) hold it and reach the engine, the API,
/// the socket requests, the runtime and the publisher through it.
pub(crate) struct Inner {
    link: Arc<Link>,
    engine: Arc<Engine>,
    api: Arc<Api>,
    updates: mpsc::UnboundedSender<Update>,
    state: watch::Receiver<Arc<AppState>>,
    runtime: tokio::runtime::Handle,
    prefs: Prefs,
    upkeep: Arc<Upkeep>,
    #[cfg_attr(
        not(feature = "testing"),
        allow(
            dead_code,
            reason = "Client::snapshots (feature testing) is its reader"
        )
    )]
    taps: Taps,
}

/// Every published snapshot, for the subscribers of `Client::snapshots` (feature `testing`): the watch channel keeps
/// only the newest, and a test that must see that a value never appeared needs them all.
type Taps = Arc<Mutex<Vec<mpsc::UnboundedSender<Arc<AppState>>>>>;

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
    /// The instance this client serves (its id, address and data directory).
    instance: InstanceConfig,
    /// This client's number in the process (from 1, never reused).
    serial: u64,
}

/// The last [`Client::serial`] handed out.
static SERIALS: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);

impl Client {
    /// Opens the instance's store, establishes the device session (from the device key; a stored session is used
    /// while it is not within an hour of either expiry) and reads the instance document, on a runtime of its own; then
    /// starts the gateway and the sync engine in the background and returns.
    ///
    /// It blocks the calling thread only for the session and the instance document (a few round trips), so call it
    /// from the UI's thread or `spawn_blocking`, not from an async task where it would stall a worker. It never panics
    /// on a caller inside a tokio runtime: the wait runs on a thread of its own, outside any runtime context (tokio
    /// refuses a `block_on` inside one; `Handle::try_current` cannot tell that case from a legitimate
    /// `spawn_blocking` caller).
    ///
    /// It fails with [`ClientError::Store`] (the store did not open), [`ClientError::NoIdentity`] (no finished
    /// sign-up or sign-in), [`ClientError::Revoked`] (the instance refused the device's challenge),
    /// [`ClientError::Network`] (the instance could not be reached), [`ClientError::Sync`] (any other refusal, with
    /// its code) or [`ClientError::Runtime`] (the runtime or its thread failed). A start that fails shuts the runtime
    /// it built down in the background, so failing does not panic there either. What happens after a successful start
    /// is reported in [`Client::state`], never as an error here: the gateway connecting, an owed second factor
    /// ([`AccountPhase::FactorSetup`]), a refused version, a revocation.
    pub fn start(cfg: InstanceConfig, kc: Arc<dyn Keychain>) -> Result<Client, ClientError> {
        Self::start_with(cfg, kc, GatewayOptions::default())
    }

    /// [`Client::start`] with the gateway's timers and roots.
    pub fn start_with(
        cfg: InstanceConfig,
        kc: Arc<dyn Keychain>,
        options: GatewayOptions,
    ) -> Result<Client, ClientError> {
        Self::start_tapped(cfg, kc, options, Taps::default())
    }

    /// Feature `testing`: [`Client::start`], with every snapshot it ever publishes also sent to the receiver (the first
    /// one included), so a test can show that a value appeared in no snapshot at all.
    #[cfg(feature = "testing")]
    pub fn start_observed(
        cfg: InstanceConfig,
        kc: Arc<dyn Keychain>,
    ) -> Result<(Client, mpsc::UnboundedReceiver<Arc<AppState>>), ClientError> {
        let (tx, rx) = mpsc::unbounded_channel();
        let taps = Taps::new(Mutex::new(vec![tx]));
        let client = Self::start_tapped(cfg, kc, GatewayOptions::default(), taps)?;
        Ok((client, rx))
    }

    fn start_tapped(
        cfg: InstanceConfig,
        kc: Arc<dyn Keychain>,
        options: GatewayOptions,
        taps: Taps,
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
        // Every refused Api call is recorded in the diagnostics (Task 24: controller.ts `recordError`).
        let refused_errors = updates.clone();
        let api = Arc::new(Api::new(
            http,
            socket.clone(),
            auth.clone(),
            core.clone(),
            Arc::new(move |code: &str| {
                let _ = refused_errors.send(Update::Apply(Box::new(
                    crate::features::diagnostics::error_update(code),
                )));
                refusal_recorded(code);
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
                    let instance = api
                        .get_instance()
                        .await
                        .map_err(|e| ClientError::Network(e.to_string()))?;
                    // controller.ts `enterReady` (b) and (c), (d): the instance's signer key against the pin, then a
                    // list this device has not published yet (a sign-in whose publish failed after the core wrote the
                    // enrolment), then an enrolled session in place of the pending one it may still hold.
                    check_instance_signer_pin(&core, &instance).await?;
                    if !identity.list_published {
                        publish_own_list(&api, &core, &user_id, &device_id).await?;
                    }
                    // controller.ts `enterReady`'s factor gate: an account that owes a second factor stops at
                    // factor setup, with no gateway (the instance refuses it until a factor is added).
                    let owed = factors_owed(&api).await.map_err(|e| match e.status {
                        0 => ClientError::Network(e.detail),
                        _ => ClientError::Sync(e.code),
                    })?;
                    // controller.ts `keyPackagesOwed`: a device's first batch goes up before it is ready (before any
                    // join), past the factor gate (an owed factor refuses it). The instance's own count decides, so a
                    // device that already has its batch publishes nothing; a refusal is left to the ready's refill.
                    if !owed {
                        let _ = crate::account::top_up_key_packages(&core, &api).await;
                    }
                    Ok::<_, ClientError>((instance, owed))
                })
            })
            .join()
        })
        .map_err(|_| ClientError::Runtime("the start thread panicked".into()))??;
        let (instance, owed) = instance;

        let area_inner = Arc::new(OnceLock::new());
        let hooks = Arc::new(Hooks {
            tx: updates.clone(),
            inner: area_inner.clone(),
        });
        let engine = Engine::new(core.clone(), api.clone(), hooks, instance);
        let upkeep = Upkeep::new(
            core.clone(),
            api.clone(),
            runtime.handle().clone(),
            user_id,
            device_id,
        );
        let initial = AppState {
            account: crate::state::AccountState {
                phase: if owed {
                    AccountPhase::FactorSetup
                } else {
                    AccountPhase::Ready
                },
                user_id: Some(UserId(user_id)),
                device_id: Some(DeviceId(device_id)),
                username: identity.username.clone(),
            },
            ..AppState::default()
        };
        let (state_tx, state_rx) = watch::channel(Arc::new(initial.clone()));
        // A tap set before the start gets the initial snapshot first (before the publisher can publish any other).
        lock_taps(&taps).retain(|t| t.send(state_rx.borrow().clone()).is_ok());
        let link = Arc::new(Link {
            engine: engine.clone(),
            socket,
            auth,
            server: cfg.server.clone(),
            options,
            gateway: Mutex::new(None),
            app_offline: AtomicBool::new(false),
            updates: updates.clone(),
            pre_connect: Some(pin_check(api.clone(), core.clone())),
        });
        let inner = Arc::new(Inner {
            link: link.clone(),
            engine: engine.clone(),
            api: api.clone(),
            updates: updates.clone(),
            state: state_rx,
            runtime: runtime.handle().clone(),
            prefs: Prefs::instance(&cfg.data_dir),
            upkeep: upkeep.clone(),
            taps: taps.clone(),
        });
        let _ = area_inner.set(Arc::downgrade(&inner));
        // The key package status after each refill attempt (Task 24's Diagnostics section).
        let kp_inner = Arc::downgrade(&inner);
        inner.upkeep.set_key_package_observer(Some(Arc::new(
            move |status: &crate::account::KeyPackageStatus| {
                if let Some(inner) = kp_inner.upgrade() {
                    crate::features::diagnostics::Diagnostics::of(inner).set_key_packages(
                        crate::state::diagnostics::KeyPackageStatus {
                            count: status.count,
                            last_attempt_ms: status.last_attempt.map(|s| s.saturating_mul(1000)),
                            last_error: status.last_error.clone(),
                        },
                    );
                }
            },
        )));
        // Each answered socket request's round trip, by route pattern (Task 24's slowest routes).
        let timed = Arc::downgrade(&inner);
        link.socket
            .set_observer(Some(Arc::new(move |pattern: &str, ms: f64| {
                if let Some(inner) = timed.upgrade() {
                    crate::features::diagnostics::Diagnostics::of(inner).record_route(pattern, ms);
                }
            })));
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
            listed: None,
            renew: RenewGate::default(),
            upkeep,
            inner: area_inner,
            removals_due: false,
            removals: RemovalGate::default(),
            taps,
        };
        runtime.spawn(watch_poison(
            publisher.core.clone(),
            publisher.updates.clone(),
        ));
        runtime.spawn(publisher.run(updates_rx));
        if !owed {
            let _enter = runtime.enter();
            link.connect();
        }
        Ok(Client {
            runtime: runtime.0.take(),
            inner,
            instance: cfg,
            serial: SERIALS.fetch_add(1, std::sync::atomic::Ordering::Relaxed) + 1,
        })
    }

    /// The instance this client serves, as it was started (Task 28 item 23: the desktop's step-up and factor screens
    /// read the address here).
    pub fn instance(&self) -> &InstanceConfig {
        &self.instance
    }

    /// This client's number in the process: unique among every client started in it, never reused (a view keys what a
    /// client opened by it; a client's address can be reused after it drops).
    pub fn serial(&self) -> u64 {
        self.serial
    }

    /// The observable state: a new snapshot is published on every change.
    pub fn state(&self) -> watch::Receiver<Arc<AppState>> {
        self.inner.state.clone()
    }

    /// Feature `testing`: every snapshot published from now on, in order (the current one first). [`Client::state`]
    /// coalesces; this is for a test that must show a value never appeared in any snapshot.
    #[cfg(feature = "testing")]
    pub fn snapshots(&self) -> mpsc::UnboundedReceiver<Arc<AppState>> {
        let (tx, rx) = mpsc::unbounded_channel();
        let mut taps = lock_taps(&self.inner.taps);
        let _ = tx.send(self.inner.state.borrow().clone());
        taps.push(tx);
        rx
    }

    /// This instance's non-secret preferences (`<data_dir>/prefs.json`).
    pub fn prefs(&self) -> Prefs {
        self.inner.prefs.clone()
    }

    /// What the area handles (`crate::features`) are built from.
    pub(crate) fn inner(&self) -> &Arc<Inner> {
        &self.inner
    }

    fn spawn<T: Send + 'static>(
        &self,
        f: impl std::future::Future<Output = Result<T, ClientError>> + Send + 'static,
    ) -> oneshot::Receiver<Result<T, ClientError>> {
        let (tx, rx) = oneshot::channel();
        let updates = self.inner.updates.clone();
        match &self.runtime {
            Some(rt) => {
                rt.spawn(async move {
                    let _ = tx.send(guarded(f, &updates).await);
                });
            }
            None => {
                let _ = tx.send(Err(ClientError::Stopped));
            }
        }
        rx
    }

    /// Feature `testing`: how many sends a leaf verdict refused and the trust retry queued again.
    #[cfg(feature = "testing")]
    pub fn trust_retries(&self) -> u64 {
        self.inner.engine.trust_retries()
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

    /// The account's second factor was set up (by the factor screens, over the instance's routes): the account leaves
    /// [`AccountPhase::FactorSetup`] and the gateway, refused while the factor was owed, connects again on the same
    /// client (no rebuild). Its ready publishes the key packages and backups the owed account could not. If the
    /// instance still finds a factor owed, the refusal brings the account back to `FactorSetup`. Does nothing in any
    /// other phase.
    pub fn factor_completed(&self) {
        let _ = self.inner.updates.send(Update::FactorCompleted);
    }

    /// The last key-package publish, as Diagnostics shows it (controller.ts `keyPackages`).
    pub fn key_packages(&self) -> crate::account::KeyPackageStatus {
        self.inner.upkeep.key_package_status()
    }

    /// The last ready's backup pass: what failed in it, if anything (a failed own-list refresh skips the repair).
    pub fn backups(&self) -> crate::account::BackupStatus {
        self.inner.upkeep.backup_status()
    }

    /// The instance holds a backup root this device did not seal (enrol.ts `E_ROOT_MISMATCH`, BACKUPS-RECOVERY-04): every
    /// later recovery from it would fail, so the UI alerts. Cleared by a later backup pass that finds the roots equal.
    pub fn backup_root_mismatch(&self) -> bool {
        self.inner.upkeep.root_mismatch()
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

    /// Task 26 (Forget this device): a probe that says when this client's store is closed, i.e. its core and its SQLite
    /// connection were dropped (every job, every area handle and the runtime let go of them). A wipe waits for it.
    pub fn store_probe(&self) -> StoreProbe {
        StoreProbe(Arc::downgrade(self.inner.engine.core()))
    }

    /// Task 26 (Forget this device): stops the upkeep, the engine and the gateway now, through a shared handle (views
    /// may still hold the client). The runtime goes when the last handle drops ([`Client::stop`] or the drop).
    pub fn halt(&self) {
        self.inner.upkeep.stop();
        // Task 20: a halted client's Bot reply secrets and held runs go now, not when its last holder lets go.
        crate::features::bot::stopped(&self.inner);
        self.inner.engine.stop();
        self.inner.link.disconnect();
        self.inner.link.socket.close();
    }

    fn shutdown(&mut self) {
        self.inner.upkeep.stop();
        // Task 20: the Bot area's reply secrets and held runs go with the client.
        crate::features::bot::stopped(&self.inner);
        self.inner.engine.stop();
        self.inner.link.disconnect();
        self.inner.link.socket.close();
        if let Some(rt) = self.runtime.take() {
            rt.shutdown_background();
        }
    }
}

/// [`Client::store_probe`]: holds the store only weakly.
#[derive(Clone)]
pub struct StoreProbe(std::sync::Weak<std::sync::Mutex<dilla_core::client::ClientCore>>);

impl StoreProbe {
    /// The core and its connection are gone: nothing can write the store any more.
    pub fn is_closed(&self) -> bool {
        self.0.strong_count() == 0
    }
}

impl std::fmt::Debug for StoreProbe {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "StoreProbe(closed: {})", self.is_closed())
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
    ///
    /// `app_offline` is read under the gateway's lock: [`Client::disconnect`] sets it before it takes that lock, so a
    /// connect racing a disconnect (a renewal's restart) either sees it and connects nothing, or installs its gateway
    /// before the disconnect takes it down. The app's offline choice always wins.
    fn connect(&self) {
        let mut slot = self
            .gateway
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        if slot.is_some() || self.engine.stopped() || self.app_offline.load(Ordering::Acquire) {
            return;
        }
        let gateway = Arc::new(WsGateway::connect_checked(
            self.server.clone(),
            self.auth.clone(),
            self.options.clone(),
            self.pre_connect.clone(),
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

// The area features' access to the client (Task 1's scaffold): what a Wave 1 task needs without editing this file.
#[allow(
    dead_code,
    reason = "the Wave 1 areas (crate::features) are the callers"
)]
impl Inner {
    /// Applies `f` to the published [`AppState`] on the publisher's loop, then publishes the new snapshot. An area
    /// changes only its own slice.
    pub(crate) fn publish(&self, f: impl FnOnce(&mut AppState) + Send + 'static) {
        let _ = self.updates.send(Update::Apply(Box::new(f)));
    }

    /// The gateway's request transport (op 59/60) for areas that request over the socket.
    pub(crate) fn socket_requests(&self) -> Arc<SocketRequests> {
        self.link.socket.clone()
    }

    /// The latest published snapshot.
    pub(crate) fn snapshot(&self) -> Arc<AppState> {
        self.state.borrow().clone()
    }

    pub(crate) fn api(&self) -> &Arc<Api> {
        &self.api
    }

    pub(crate) fn engine(&self) -> &Arc<Engine> {
        &self.engine
    }

    pub(crate) fn prefs(&self) -> &Prefs {
        &self.prefs
    }

    /// Runs `f` on the client's runtime; its answer comes on the oneshot ([`ClientError::Stopped`] if it unwinds, a
    /// closed oneshot once the client stopped).
    pub(crate) fn spawn<T: Send + 'static>(
        &self,
        f: impl std::future::Future<Output = Result<T, ClientError>> + Send + 'static,
    ) -> oneshot::Receiver<Result<T, ClientError>> {
        let (tx, rx) = oneshot::channel();
        let updates = self.updates.clone();
        self.runtime.spawn(async move {
            let _ = tx.send(guarded(f, &updates).await);
        });
        rx
    }

    /// The account's upkeep (key packages, backups: [`crate::account`]).
    pub(crate) fn upkeep(&self) -> &Arc<Upkeep> {
        &self.upkeep
    }

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

tokio::task_local! {
    /// While a command runs under [`guarded`]: the stable code the Api refusal hook last recorded for it, so the
    /// command's failure with that same code is not recorded a second time (integration-1 follow-up 4).
    static REFUSAL_RECORDED: std::cell::RefCell<Option<String>>;
}

/// The Api refusal hook recorded `code`: a command running under [`guarded`] notes it (outside one, nothing to note).
fn refusal_recorded(code: &str) {
    let code = crate::features::diagnostics::stable_code(code).to_owned();
    let _ = REFUSAL_RECORDED.try_with(|r| *r.borrow_mut() = Some(code));
}

/// An action's answer, also when it unwinds: then [`ClientError::Stopped`], never a dropped oneshot. The unwind is also
/// reported to the publisher ([`Update::Panicked`]): a core call on a poisoned core is the publisher's `Failed`
/// already, and any other panic means engine code stopped half way (a lane, a timer, an outbox row), so the client fails
/// as for a poisoned core instead of carrying on as if nothing happened.
async fn guarded<T>(
    f: impl std::future::Future<Output = Result<T, ClientError>>,
    updates: &mpsc::UnboundedSender<Update>,
) -> Result<T, ClientError> {
    use futures_util::FutureExt;
    let (answer, recorded) = REFUSAL_RECORDED
        .scope(std::cell::RefCell::new(None), async {
            let answer = std::panic::AssertUnwindSafe(f).catch_unwind().await;
            (answer, REFUSAL_RECORDED.with(|r| r.borrow_mut().take()))
        })
        .await;
    match answer {
        Ok(Err(e)) => {
            // A failed command is recorded in the diagnostics by its stable code (Task 24), unless the Api refusal hook
            // recorded this very code for it already.
            let code = crate::features::diagnostics::client_error_code(&e);
            if recorded.as_deref() != Some(code) {
                let _ = updates.send(Update::Apply(Box::new(
                    crate::features::diagnostics::error_update(code),
                )));
            }
            Err(e)
        }
        Ok(answer) => answer,
        Err(_) => {
            let _ = updates.send(Update::Panicked);
            Err(ClientError::Stopped)
        }
    }
}

/// controller.ts `checkInstanceSignerPin`: the instance's external-sender key, read over the device's authenticated
/// session, must be the one this device pinned at enrolment (the core writes only a missing pin, as for a store
/// enrolled before the check existed, and refuses a changed one or another instance). A refusal fails the start closed.
pub(crate) async fn check_instance_signer_pin(
    core: &CoreHandle,
    instance: &crate::sync::api::InstanceInfo,
) -> Result<(), ClientError> {
    let (id, key) = (instance.instance_id, instance.external_sender_pub);
    with_core(core, move |c| c.instance_signer_key_pin(&id, &key))
        .await
        .map_err(|e| ClientError::Sync(e.code.to_string()))
}

/// controller.ts `mintTicket`: before every gateway connection attempt (the reconnects included) the instance's signer
/// key is read over the session again and held to the pin, as at the start ([`check_instance_signer_pin`]). An instance
/// that cannot be read now (unreachable, a 5xx) is tried again after the backoff; a document that does not decode, a
/// changed key or another instance stops the gateway, closed (`failInstanceSignerPin`).
fn pin_check(api: Arc<Api>, core: CoreHandle) -> crate::gateway::PreConnect {
    Arc::new(move || {
        let (api, core) = (api.clone(), core.clone());
        Box::pin(async move {
            let read = api.get_instance().await;
            let pinned = match &read {
                Ok(instance) => Some(check_instance_signer_pin(&core, instance).await),
                Err(_) => None,
            };
            pin_verdict(read.as_ref().map(|_| ()), pinned)
        })
    })
}

/// [`pin_check`]'s verdict from the instance read (`Err`: its refusal) and, once read, the pin check.
pub(crate) fn pin_verdict(
    read: Result<(), &crate::sync::api::ApiError>,
    pinned: Option<Result<(), ClientError>>,
) -> crate::gateway::PreConnectVerdict {
    use crate::gateway::PreConnectVerdict as V;
    match (read, pinned) {
        (Err(e), _) if e.code == "E_DECODE" => V::Refuse,
        (Err(_), _) => V::Retry,
        (Ok(()), Some(Ok(()))) => V::Go,
        (Ok(()), _) => V::Refuse,
    }
}

/// signup.ts `publishDeviceList`'s 409 branch, before any adoption: what the served list's version says about this
/// device's unpublished candidate.
#[derive(Debug, PartialEq, Eq)]
pub(crate) enum ListConflict {
    /// No list is served, or an older one than the candidate: the 409 stands (a list race).
    Race,
    /// The instance serves the candidate's version or a newer one (a PUT whose answer was lost, or a later list): the
    /// served list is adopted through the core's verified update, and decides whether this device is listed.
    Adopt,
}

pub(crate) fn list_conflict(served_version: Option<u64>, candidate_version: u64) -> ListConflict {
    match served_version {
        Some(v) if v >= candidate_version => ListConflict::Adopt,
        _ => ListConflict::Race,
    }
}

/// The version of a signed device-list body (`[version, ...]`).
fn list_body_version(body: &[u8]) -> Option<u64> {
    let v = crate::sync::value::decode(body).ok()?;
    crate::sync::value::uint(&crate::sync::value::arr(&v, 4).ok()?[0]).ok()
}

/// signup.ts `publishDeviceList` for a store whose own list is not published yet, then session.ts `establish` for the
/// enrolled session the list makes possible. A 409 whose served list is at least the candidate's version is adopted
/// (verified by the core, a page at a time); one that leaves this device out (`E_DEVICE_UNLISTED`) is a revocation, as
/// at the web's ready. An older or missing one is a list race. A 401 is a revoked row.
async fn publish_own_list(
    api: &Api,
    core: &CoreHandle,
    user: &[u8; 16],
    device: &[u8; 16],
) -> Result<(), ClientError> {
    let body = with_core(core, |c| c.device_list_body())
        .await
        .map_err(|e| ClientError::Sync(e.code.to_string()))?;
    let candidate = list_body_version(&body).ok_or_else(|| ClientError::Sync("E_DECODE".into()))?;
    match api.put_device_list(user, body).await {
        Ok(()) => {}
        Err(e) if e.status == 409 => {
            let served = api
                .get_device_list(user)
                .await
                .map_err(crate::features::profile::refusal)?;
            if list_conflict(served.map(|s| s.version), candidate) == ListConflict::Race {
                return Err(ClientError::Sync("E_LIST_RACE".into()));
            }
            // controller.ts `enterReady`: a served list that leaves this enrolled device out (`E_DEVICE_UNLISTED` at
            // phase 2) is a revocation, as a refused session is.
            if !crate::features::devices::adopt_served_list(api, core, user, device).await? {
                return Err(ClientError::Revoked);
            }
        }
        Err(e) if e.status == 401 => return Err(ClientError::Revoked),
        Err(e) if e.status == 0 => return Err(ClientError::Network(e.detail)),
        Err(e) => return Err(ClientError::Sync(e.code)),
    }
    let published = with_core(core, |c| c.identity())
        .await
        .ok()
        .and_then(|raw| Identity::decode(&raw))
        .is_some_and(|i| i.list_published);
    if !published {
        with_core(core, |c| c.device_list_published())
            .await
            .map_err(|e| ClientError::Sync(e.code.to_string()))?;
    }
    api.establish().await.map_err(|e| match e {
        SessionError::NoIdentity => ClientError::NoIdentity,
        SessionError::Revoked | SessionError::Scope => ClientError::Revoked,
        SessionError::Api(a) if a.status == 0 => ClientError::Network(a.detail),
        SessionError::Api(a) => ClientError::Sync(a.code),
        SessionError::Core(c) => ClientError::Sync(c),
    })
}

/// routes.ts `getFactors`' `owed` (`GET /v1/auth/factors`, element 0): whether the account owes a second factor.
async fn factors_owed(api: &Api) -> Result<bool, crate::sync::api::ApiError> {
    use crate::http::routes::{self as r, NoBody, Raw};
    let Raw(raw) = api.http_call::<r::GetFactors>(&[], NoBody, true).await?;
    let decode =
        |e: crate::sync::value::DecodeError| crate::sync::api::ApiError::local(0, "E_DECODE", e.0);
    let v = crate::sync::value::decode(&raw).map_err(decode)?;
    let a = crate::sync::value::at_least(&v, 5).map_err(decode)?;
    Ok(crate::sync::value::uint(&a[0]).map_err(decode)? == 1)
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
    /// The account owes a second factor: factor setup, with no gateway.
    FactorSetup,
}

/// The renewals of refused sessions: one per ready, one at a time.
#[derive(Debug, Default)]
struct RenewGate {
    in_flight: bool,
    /// A renewal ran since the last ready.
    spent: bool,
    /// A gateway reached ready while the renewal was out (the app reconnected meanwhile, on a session the HTTP
    /// layer's own re-authentication had already renewed): that gateway is working, and the renewal must not restart it.
    ready_meanwhile: bool,
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
            self.ready_meanwhile = false;
            OnRefused::Renew
        }
    }

    fn done(&mut self, outcome: Renewal, app_offline: bool) -> AfterRenewal {
        self.in_flight = false;
        let ready_meanwhile = std::mem::take(&mut self.ready_meanwhile);
        match outcome {
            Renewal::SignedOut => AfterRenewal::SignOut,
            Renewal::FactorOwed => AfterRenewal::FactorSetup,
            Renewal::Renewed if app_offline || ready_meanwhile => AfterRenewal::Stay,
            Renewal::Renewed => AfterRenewal::Restart,
        }
    }

    fn ready(&mut self) {
        self.spent = false;
        if self.in_flight {
            self.ready_meanwhile = true;
        }
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
    /// Renewed, and the account owes a second factor (the factor read after the renewal).
    FactorOwed,
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
    /// The communities the last structure load listed (`None` before the first): one listed again after a load left
    /// it out (a kick, a lifted ban or a leave, then a join) is the engine's [`Engine::rejoined`].
    listed: Option<HashSet<CommunityId>>,
    /// The renewals of refused sessions (one per ready, one at a time).
    renew: RenewGate,
    /// The account's upkeep, started on every ready.
    upkeep: Arc<Upkeep>,
    /// The client, once built (what the account and removal entry points take).
    inner: Arc<OnceLock<Weak<Inner>>>,
    /// A ready came since the last structure load: the next one reads the removal feeds first.
    removals_due: bool,
    /// Which timelines may not be read yet because a removal feed is unread.
    removals: RemovalGate,
    taps: Taps,
}

fn lock_taps(taps: &Taps) -> std::sync::MutexGuard<'_, Vec<mpsc::UnboundedSender<Arc<AppState>>>> {
    taps.lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
}

/// What keeps a timeline unread until its removal feed is: a ready's read in flight (every timeline), and each group
/// whose read failed or could not run (its timeline), retried with the key-package backoff.
#[derive(Debug, Default)]
struct RemovalGate {
    /// The newest ready whose feeds are being read (a token: an older ready's answer does not clear a newer one).
    generation: u64,
    pending: Option<u64>,
    /// Groups whose read failed transiently: retried with the backoff.
    unread: HashSet<[u8; 16]>,
    /// Groups whose read failed for good: shown as the channel's state, not retried (a later ready reads them again).
    failed: HashMap<[u8; 16], TimelineUnavailable>,
    delay_ms: u64,
    retrying: bool,
    /// Orders the answers: each clearing is a tick, and a read's answer for a group cleared after the read started
    /// (by an apply's or an open's own read) is stale.
    clock: u64,
    cleared_at: HashMap<[u8; 16], u64>,
}

/// The answer of one read of removal feeds: when it started ([`RemovalGate::clock`]) and what is still unread.
type FeedAnswers = (u64, HashMap<[u8; 16], FeedFailure>);

impl RemovalGate {
    /// A ready's read starts: its generation and the tick it starts at.
    fn start(&mut self) -> (u64, u64) {
        self.generation += 1;
        self.pending = Some(self.generation);
        (self.generation, self.clock)
    }

    /// Takes a read's answers, skipping a group cleared since the read started.
    fn apply(&mut self, (started, answers): FeedAnswers) {
        for (g, why) in answers {
            if self.cleared_at.get(&g).is_some_and(|at| *at > started) {
                continue;
            }
            match why {
                FeedFailure::Transient(_) => {
                    self.failed.remove(&g);
                    self.unread.insert(g);
                }
                FeedFailure::Terminal(why) => {
                    self.unread.remove(&g);
                    self.failed.insert(g, why);
                }
            }
        }
        if self.unread.is_empty() {
            self.delay_ms = 0;
        }
    }

    /// A ready's read ended: answers whether it was the newest one.
    fn read(&mut self, generation: u64, answers: FeedAnswers) -> bool {
        self.apply(answers);
        if self.pending == Some(generation) {
            self.pending = None;
            true
        } else {
            false
        }
    }

    fn cleared(&mut self, group: &[u8; 16]) {
        self.clock += 1;
        self.cleared_at.insert(*group, self.clock);
        self.unread.remove(group);
        self.failed.remove(group);
        if self.unread.is_empty() {
            self.delay_ms = 0;
        }
    }

    /// Whether `group`'s timeline may be read now.
    fn readable(&self, group: &[u8; 16]) -> bool {
        self.pending.is_none() && !self.unread.contains(group) && !self.failed.contains_key(group)
    }

    /// The next retry's wait, the tick it starts at and the groups it reads, unless one is out or nothing transient
    /// is unread.
    fn next_retry(&mut self) -> Option<(Duration, u64, Vec<[u8; 16]>)> {
        if self.retrying || self.unread.is_empty() {
            return None;
        }
        self.retrying = true;
        self.delay_ms = crate::account::keypackages::next_delay_ms(self.delay_ms);
        Some((
            Duration::from_millis(self.delay_ms),
            self.clock,
            self.unread.iter().copied().collect(),
        ))
    }

    /// A retry ended. Its groups are taken off first (a group that read is not in the answers), then its answers.
    fn retried(&mut self, groups: &[[u8; 16]], answers: FeedAnswers) {
        self.retrying = false;
        for g in groups {
            if !answers.1.contains_key(g) {
                self.unread.remove(g);
            }
        }
        self.apply(answers);
    }
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
            let mut panicked = false;
            let mut reread_all = false;
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
                    Update::FactorRequired => self.factor_required(),
                    Update::Renewed(outcome) => {
                        let offline = self.link.app_offline.load(Ordering::Acquire);
                        match self.renew.done(outcome, offline) {
                            AfterRenewal::Restart => self.link.restart(),
                            AfterRenewal::Stay => {}
                            AfterRenewal::SignOut => self.sign_out(),
                            AfterRenewal::FactorSetup => self.factor_required(),
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
                    Update::Apply(f) => f(&mut self.state),
                    Update::FactorCompleted => self.check_factor(),
                    Update::FactorChecked(owed) => self.factor_completed(owed),
                    Update::Panicked => panicked = true,
                    Update::Unlisted => self.sign_out(),
                    Update::RemovalsRead {
                        generation,
                        answers,
                    } => {
                        // The newest ready's read: every channel the gate held back is read now (its structure
                        // load may have failed, and nothing else would read them).
                        reread_all |= self.removals.read(generation, answers);
                        self.retry_removals();
                    }
                    Update::RemovalsCleared(g) => {
                        self.removals.cleared(&g);
                        groups.insert(g);
                    }
                    Update::RemovalsRetried {
                        groups: tried,
                        answers,
                    } => {
                        self.removals.retried(&tried, answers);
                        groups.extend(tried);
                        self.retry_removals();
                    }
                }
            }
            if self.core.is_poisoned() || panicked {
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
            if reread_all {
                let all: HashSet<[u8; 16]> = self
                    .engine
                    .groups()
                    .await
                    .iter()
                    .map(|r| r.group_id)
                    .collect();
                groups.extend(all);
            }
            if !groups.is_empty() {
                self.refresh_groups(&groups).await;
            }
            self.publish();
        }
    }

    fn publish(&self) {
        let snapshot = Arc::new(self.state.clone());
        // Under the taps' lock, so a subscriber added meanwhile sees either this snapshot as its first or receives it.
        let mut taps = lock_taps(&self.taps);
        taps.retain(|t| t.send(snapshot.clone()).is_ok());
        self.tx.send_replace(snapshot);
    }

    /// The device is revoked: final for this client (a later ready never says otherwise, see [`phase_on_ready`]).
    fn sign_out(&mut self) {
        // A sign-up's kept join (Task 25, `signup.pendingJoin`) goes with the session (the web clears it on sign-out).
        let core = self.core.clone();
        tokio::spawn(async move {
            let _ = try_core(&core, |c| {
                c.setting_delete(crate::features::signup::PENDING_JOIN_KEY)
            })
            .await;
        });
        self.upkeep.stop();
        // Task 20: a revoked device forgets every Bot reply secret and held run (TS `enterRevoked`).
        crate::features::bot::revoked(&self.engine);
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
                self.after_ready(&info);
                self.removals_due = true;
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
                            let outcome = match renew(|| api.establish(), renewal_backoff).await {
                                // The new session is fine, but the account owes a factor: the gateway would refuse it.
                                Renewal::Renewed if factors_owed(&api).await.unwrap_or(false) => {
                                    Renewal::FactorOwed
                                }
                                other => other,
                            };
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
            // controller.ts `failInstanceSignerPin`: the instance's signer key is no longer the pinned one (or another
            // instance answers). The gateway stopped; the client fails closed, and a restart's start refuses it too.
            ConnectionEvent::Refused(Refusal::InstanceSignerChanged, _) => {
                self.state.connection.status = ConnectionStatus::Offline;
                self.state.account.phase = AccountPhase::Failed;
                false
            }
            ConnectionEvent::Refused(Refusal::SecondFactorRequired, _) => {
                self.state.connection.status = ConnectionStatus::Offline;
                self.state.account.phase = AccountPhase::FactorSetup;
                false
            }
            ConnectionEvent::Refused(Refusal::UpgradeRequired, _) => {
                self.state.connection.status = ConnectionStatus::Offline;
                self.state.account.phase = AccountPhase::VersionRefused { upgrade: true };
                false
            }
            ConnectionEvent::Refused(
                Refusal::InstanceOutdated | Refusal::VersionUnsupported,
                _,
            ) => {
                self.state.connection.status = ConnectionStatus::Offline;
                self.state.account.phase = AccountPhase::VersionRefused { upgrade: false };
                false
            }
        }
    }

    /// controller.ts `onReady`, the account's part: the key packages topped up from the ready's count, and the backup
    /// pass in the controller's order (ensure, refresh the own device list, repair only a listed device). A device the
    /// refreshed list leaves out is signed out ([`Update::Unlisted`]); every other failure is kept in the upkeep's
    /// [`crate::account::BackupStatus`] ([`Client::backups`]) and the next ready tries again. None of it holds the ready
    /// up. (The removal feeds are read by the structure load this ready starts, before any timeline:
    /// [`Publisher::load_structure`].)
    fn after_ready(&self, info: &crate::gateway::ReadyInfo) {
        if self.state.account.phase != AccountPhase::Ready {
            return;
        }
        let Some(inner) = self.inner.get().and_then(Weak::upgrade) else {
            return;
        };
        let remaining = u32::try_from(info.keypackages_remaining).unwrap_or(u32::MAX);
        crate::account::refill_key_packages(&inner, remaining);
        // controller.ts `retryPendingRolePanelActions`: every stored role panel action of this device, sent again.
        crate::features::role_panels::on_ready(&inner);
        let backups = inner.clone();
        let updates = self.updates.clone();
        drop(inner.spawn(async move {
            match crate::account::backups_after_ready(&backups).await {
                Ok(crate::account::backups::AfterReady::Unlisted) => {
                    let _ = updates.send(Update::Unlisted);
                }
                Ok(crate::account::backups::AfterReady::Listed { .. }) => {}
                // Recorded in the upkeep's BackupStatus (Client::backups); the next ready tries again.
                Err(e) => return Err(ClientError::Sync(e.to_string())),
            }
            Ok(())
        }));
    }

    /// controller.ts `onFactorRequired`: the account owes a second factor (a refusal named `E_FACTOR_REQUIRED`, the
    /// gateway's "second factor required", or a renewed session the factor read finds owing): factor setup, with the
    /// gateway stopped, since the instance refuses it until a factor is added (a refused gateway left running would be
    /// taken for a revoked session and sign the device out).
    fn factor_required(&mut self) {
        if matches!(
            self.state.account.phase,
            AccountPhase::Revoked | AccountPhase::Failed
        ) {
            return;
        }
        self.state.account.phase = AccountPhase::FactorSetup;
        self.state.connection.status = ConnectionStatus::Offline;
        self.link.disconnect();
    }

    /// [`Client::factor_completed`]: the factors are read again (controller.ts `factorSetupDone`), off the loop.
    fn check_factor(&self) {
        if self.state.account.phase != AccountPhase::FactorSetup {
            return;
        }
        let (api, updates) = (self.api.clone(), self.updates.clone());
        tokio::spawn(async move {
            // An unreadable answer leaves the account where it is; the factor screens may complete again.
            if let Ok(owed) = factors_owed(&api).await {
                let _ = updates.send(Update::FactorChecked(owed));
            }
        });
    }

    /// The factor read after [`Client::factor_completed`]: still owed, the account stays in `FactorSetup`; else it
    /// leaves it and connects again on the same client, unless the app has taken the connection down (its own
    /// reconnect then connects).
    fn factor_completed(&mut self, owed: bool) {
        if owed || self.state.account.phase != AccountPhase::FactorSetup {
            return;
        }
        self.state.account.phase = AccountPhase::Loading;
        self.renew = RenewGate::default();
        if self.link.app_offline.load(Ordering::Acquire) {
            self.state.connection.status = ConnectionStatus::Offline;
        } else {
            self.state.connection.status = ConnectionStatus::Connecting;
            self.link.restart();
        }
    }

    /// Reads the structure off the publisher's loop; the result comes back as `Loaded`, whose handling reads every
    /// channel's timeline. After a ready, every community group's removal feed is read first
    /// ([`crate::sync::removals::recover_moderation_removals`]): a removal made while this device was away is a
    /// tombstone before any timeline shows the message.
    fn load_structure(&mut self) {
        let api = self.api.clone();
        let updates = self.updates.clone();
        let removals = std::mem::take(&mut self.removals_due)
            .then(|| self.inner.get().and_then(Weak::upgrade))
            .flatten();
        // Until the feeds are read, no timeline is read (a trust refresh or a catch-up of this ready may report a
        // group changed first): the store may still hold a message removed while this device was away.
        let generation = removals.as_ref().map(|_| self.removals.start());
        tokio::spawn(async move {
            if let (Some(inner), Some((generation, started))) = (removals, generation) {
                let unread = crate::sync::removals::recover_moderation_removals(&inner).await;
                drop(inner);
                let _ = updates.send(Update::RemovalsRead {
                    generation,
                    answers: (started, unread),
                });
            }
            if let Ok(loaded) = load_structure(&api).await {
                let _ = updates.send(Update::Loaded(Box::new(loaded)));
            }
        });
    }

    /// Retries the unread removal feeds after the backoff (5 s doubling to 5 min, as a key-package publish); a feed
    /// read on the way (an apply, an open) clears its group without waiting.
    fn retry_removals(&mut self) {
        let Some((wait, started, groups)) = self.removals.next_retry() else {
            return;
        };
        let inner = self.inner.get().cloned();
        let updates = self.updates.clone();
        tokio::spawn(async move {
            tokio::time::sleep(wait).await;
            let still = match inner.and_then(|w| w.upgrade()) {
                Some(inner) => {
                    crate::sync::removals::retry_moderation_removals(&inner, groups.clone()).await
                }
                None => groups
                    .iter()
                    .map(|g| (*g, FeedFailure::Transient("E_SYNC_STOPPED".into())))
                    .collect(),
            };
            let _ = updates.send(Update::RemovalsRetried {
                groups,
                answers: (started, still),
            });
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
        let now: HashSet<CommunityId> = loaded.communities.iter().map(|c| c.id).collect();
        // controller.ts `dropCommunity`: a server this account is no longer in shows none of its channels' timelines.
        let dropped: Vec<ChannelId> = self
            .state
            .channels
            .iter()
            .filter(|c| !now.contains(&c.community_id))
            .map(|c| c.id)
            .collect();
        for id in &dropped {
            self.state.timelines.remove(id);
            self.state.messages.unavailable.remove(id);
        }
        if let Some(before) = self.listed.replace(now.clone()) {
            let rejoined: Vec<CommunityId> = now.difference(&before).copied().collect();
            if !rejoined.is_empty() {
                // Jonas (2026-11-13), stricter than the web: a server joined again keeps its history hidden until each
                // text group's removal feed is read (a moderator may have removed a message while the account was
                // out). The rejoin's activation reads it at once (`Engine::activate`); a group that stays active is
                // read by the retry, whose 404 resyncs a lost leaf first.
                let rows = self.engine.groups().await;
                let groups = loaded
                    .channels
                    .iter()
                    .filter(|c| rejoined.contains(&c.community_id))
                    .filter_map(|c| c.text_group_id.map(|g| g.0))
                    .chain(
                        rows.iter()
                            .filter(|r| {
                                r.kind == 0
                                    && r.community_id
                                        .is_some_and(|c| rejoined.contains(&CommunityId(c)))
                            })
                            .map(|r| r.group_id),
                    );
                self.removals.unread.extend(groups);
                self.retry_removals();
            }
            // controller.ts `joinPath`: a server joined again resyncs what was refused while the account was out. This
            // client learns of the join from the list (it may have been made elsewhere), so a server the previous load
            // left out is one joined again.
            for c in rejoined {
                self.engine.rejoined(c.0);
            }
        }
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
            // A channel of a server this account is not in (kicked, banned, left) shows no timeline, whatever its
            // group still holds (controller.ts `dropCommunity`).
            if group
                .community_id
                .is_some_and(|c| !self.state.communities.iter().any(|x| x.id.0 == c))
            {
                self.state.timelines.remove(&id);
                continue;
            }
            // A removal feed not read yet: the store may hold a message removed meanwhile. The timeline is shown as
            // loading (no timeline), never with what the store holds; a feed that cannot be read is the channel's
            // state (`messages.unavailable`), with why.
            if !self.removals.readable(&group.group_id) {
                if self.removals.pending.is_none() {
                    self.state.timelines.remove(&id);
                }
                match self.removals.failed.get(&group.group_id) {
                    Some(why) => {
                        self.state.messages.unavailable.insert(id, why.clone());
                    }
                    None => {
                        self.state.messages.unavailable.remove(&id);
                    }
                }
                continue;
            }
            self.state.messages.unavailable.remove(&id);
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

    /// The core is poisoned, or a job panicked: stop syncing for good (no job uses it again), drop the gateway, and say
    /// so.
    fn fail(&mut self) {
        self.upkeep.stop();
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
