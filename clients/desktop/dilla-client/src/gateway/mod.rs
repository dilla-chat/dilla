//! The gateway client: the one WebSocket to dillad that carries every message and event
//! (`protocol/02-delivery-service.md` § Gateway frames). A port of `packages/client-core/src/gateway/gateway.ts`'s
//! connection lifecycle: hello and version negotiation, identify, ready, heartbeats at the hello's interval with the
//! same ack timeout, the hello's `max_frame_bytes` in both directions, and the same reconnect backoff.
//!
//! Wire-identical to the TS client in every frame it writes. One deliberate difference at the upgrade: a native client
//! can set request headers, so it sends `Authorization: Bearer <session>` beside the `dilla.v1` subprotocol instead of
//! minting a single-use `dilla.ticket.<t>` (protocol/02 § Connecting: the bearer is the instance's own first choice
//! after `identify`'s token, the ticket exists only for browsers). `identify` then carries an empty token, as in TS.
//!
//! Frames in, frames out: nothing here keeps sync state (the sync engine, Task 9, does). Events:
//! - `Connected { session }`: `session` is the `ready` frame's bytes ([`ReadyInfo::from_frame`] reads it).
//! - `Frame(bytes)`: every frame gateway.ts forwards to its listeners (delivery-service ops 16-21, presence and typing,
//!   the voice, ring, structure, away and app-build notices, the Bot frames 61-63) and the socket responses (op 60).
//! - `Disconnected { reason }`: a ready connection ended (`Network` when this client tore it down or the transport
//!   failed, `Server(code)` when the instance closed it with a code), or a refusal that stops the gateway for good
//!   (`Server(4004)` revoked, `Server(4003)` unauthenticated or second factor owed, `Server(4006)` versions,
//!   `Server(401)` at the upgrade: see [`WsGateway::refusal`]), or `Shutdown` after [`WsGateway::stop`] (best effort).
//!
//! Every write goes through a per-connection writer task, so a peer that stops reading never holds the heartbeat and
//! ack timers, the reads or `stop()`. wss uses rustls with an explicit aws-lc-rs provider and the platform's roots.

pub mod frames;
pub mod reconnect;

use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use futures_util::{SinkExt, StreamExt};
use tokio::sync::{mpsc, watch};
use tokio::time::Instant;
use tokio_tungstenite::tungstenite::client::IntoClientRequest;
use tokio_tungstenite::tungstenite::http::HeaderValue;
use tokio_tungstenite::tungstenite::protocol::frame::coding::CloseCode;
use tokio_tungstenite::tungstenite::protocol::{CloseFrame, WebSocketConfig};
use tokio_tungstenite::tungstenite::{self, Message};
use zeroize::Zeroizing;

use crate::seams::{DisconnectReason, Gateway, GatewayError, GatewayEvent};
use frames::{FrameError, Op, Value};
use reconnect::Backoff;

pub use crate::auth::AuthToken;

/// The bound on the bytes `send()` may have queued for the writer and not yet written. The instance keeps at most
/// `MaxRequestBytesInFlight` (32 MiB, internal/gateway/request.go) of request and response bodies in flight on one
/// connection and answers E_BUSY past it, so the TS client never usefully has more than that on the wire; past this
/// bound `send()` refuses with [`GatewayError::Full`] instead of buffering without limit behind a peer that stopped
/// reading. One frame is always accepted on an empty queue (a request frame may be up to `MAX_RESPONSE_FRAME`).
pub const WRITER_QUEUE_CAP: usize = 32 << 20;

/// gateway.ts `WIRE_PREFERENCE`: wire 6 (Bot v2) is the only wire this client speaks.
pub const WIRE_PREFERENCE: &[u64] = &[6];
/// gateway.ts `highestCommon(versions[1], [1n])`.
pub const E2EE_PREFERENCE: &[u64] = &[1];
/// gateway.ts `highestCommon(versions[2], [1n])`.
pub const MEDIA_PREFERENCE: &[u64] = &[1];
/// gateway.ts `DEFAULT_MAX_FRAME_BYTES`: the bound on frames read before a hello names its own.
pub const DEFAULT_MAX_FRAME_BYTES: usize = 131_584;
const UPGRADE_REQUIRED: &str = "E_UPGRADE_REQUIRED";
/// The reason of the instance's 4003 close for a session whose account owes a second factor
/// (internal/gateway/heartbeat.go).
const SECOND_FACTOR_REQUIRED: &str = "second factor required";

/// gateway.ts `CLIENT_CLOSE`: the codes this client closes a socket with (4100-4199 are the client's own).
pub struct ClientClose;

impl ClientClose {
    pub const NORMAL: u16 = 1000;
    pub const VERSION: u16 = 4006;
    pub const HELLO_TIMEOUT: u16 = 4100;
    pub const HEARTBEAT_TIMEOUT: u16 = 4101;
    pub const UNDECODABLE: u16 = 4102;
    pub const TOO_LARGE: u16 = 4109;
}

/// The instance's close codes that end the gateway rather than reconnect it.
const CLOSE_UNAUTHENTICATED: u16 = 4003;
const CLOSE_REVOKED: u16 = 4004;

/// The gateway's timers. `Default` is gateway.ts `GATEWAY`; the backoff is always gateway.ts's ([`reconnect`]).
#[derive(Clone, Debug)]
pub struct GatewayOptions {
    /// The heartbeat interval is `min(hello.heartbeat_ms / 2, heartbeat_max)`.
    pub heartbeat_max: Duration,
    /// A heartbeat unanswered this long closes the socket (4101) and reconnects.
    pub ack_timeout: Duration,
    /// Each handshake step (upgrade, hello, ready) must finish within this (4100).
    pub hello_timeout: Duration,
    /// The events channel's capacity. While it is full the connection keeps its heartbeats and `stop()`, but a
    /// consumer that leaves an event untaken for `ack_timeout` gets a reconnect (`Disconnected { Network }`).
    pub event_capacity: usize,
    /// Roots trusted for wss beside the platform's own (a self-hosted instance's private CA; tests).
    pub extra_roots: Vec<rustls::pki_types::CertificateDer<'static>>,
}

impl Default for GatewayOptions {
    fn default() -> Self {
        Self {
            heartbeat_max: Duration::from_millis(15_000),
            ack_timeout: Duration::from_millis(10_000),
            hello_timeout: Duration::from_millis(15_000),
            event_capacity: 1024,
            extra_roots: Vec::new(),
        }
    }
}

/// Why a gateway stopped for good. The app acts on it: sign in again, or ask its user (or the operator) to update.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Refusal {
    /// The session was revoked (close 4004), is not accepted (close 4003), or the upgrade answered 401: sign in again.
    Revoked,
    /// The account owes a second factor (close 4003, "second factor required"): the session stands, the app asks for
    /// the factor (protocol/09 § Second factors), then connects again.
    SecondFactorRequired,
    /// The instance speaks only wires newer than this client's (or named E_UPGRADE_REQUIRED): this client must update.
    UpgradeRequired,
    /// The instance offers only wires older than this client's: the instance must update.
    InstanceOutdated,
    /// No common version, neither newer nor older throughout (close 4006).
    VersionUnsupported,
}

/// The `hello` (op 0) of a connection.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct HelloInfo {
    pub wire_versions: Vec<u64>,
    pub e2ee_versions: Vec<u64>,
    pub media_versions: Vec<u64>,
    pub heartbeat_ms: u64,
    pub max_frame_bytes: u64,
    pub instance_id: [u8; 16],
    pub generation: u64,
    pub backoff_ms: u64,
    pub backoff_jitter_ms: u64,
}

fn u53(v: &Value) -> Result<u64, FrameError> {
    v.as_uint()
        .filter(|n| *n <= frames::MAX_SAFE_INTEGER)
        .ok_or(FrameError::Shape)
}

fn u64_of(v: &Value) -> Result<u64, FrameError> {
    v.as_uint().ok_or(FrameError::Shape)
}

fn bin<const N: usize>(v: &Value) -> Result<[u8; N], FrameError> {
    v.as_bytes()
        .and_then(|b| <[u8; N]>::try_from(b).ok())
        .ok_or(FrameError::Shape)
}

fn uints(v: &Value) -> Result<Vec<u64>, FrameError> {
    v.as_array()
        .ok_or(FrameError::Shape)?
        .iter()
        .map(u64_of)
        .collect()
}

impl HelloInfo {
    /// gateway.ts `readHello`.
    pub fn from_payload(p: &[Value]) -> Result<Self, FrameError> {
        let [
            wire,
            e2ee,
            media,
            heartbeat,
            max_frame,
            instance,
            generation,
            backoff,
            jitter,
        ] = p
        else {
            return Err(FrameError::Shape);
        };
        Ok(Self {
            wire_versions: uints(wire)?,
            e2ee_versions: uints(e2ee)?,
            media_versions: uints(media)?,
            heartbeat_ms: u53(heartbeat)?,
            max_frame_bytes: u53(max_frame)?,
            instance_id: bin(instance)?,
            generation: u64_of(generation)?,
            backoff_ms: u53(backoff)?,
            backoff_jitter_ms: u53(jitter)?,
        })
    }
}

/// One group of `ready`'s per-group digest: the device's own acknowledged cursor.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct GroupDigest {
    pub group_id: [u8; 16],
    pub epoch: u64,
    pub last_seq: u64,
    pub proposals_outstanding: u64,
}

/// The `ready` (op 3) of a connection.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ReadyInfo {
    pub n: u64,
    pub device_id: [u8; 16],
    pub user_id: [u8; 16],
    pub generation: u64,
    pub resume_token: [u8; 32],
    pub wire: u64,
    pub e2ee: u64,
    pub media: u64,
    pub keypackages_remaining: u64,
    pub groups: Vec<GroupDigest>,
    pub boot_id: [u8; 16],
}

impl ReadyInfo {
    /// Reads the `session` bytes of `GatewayEvent::Connected`: a whole `ready` frame.
    pub fn from_frame(bytes: &[u8]) -> Result<Self, FrameError> {
        let f = frames::decode(bytes)?;
        if f.op != Op::READY {
            return Err(FrameError::Shape);
        }
        Self::from_payload(f.n, &f.payload)
    }

    /// gateway.ts `readReady`, without the check against the hello's selection (the connection does that).
    pub fn from_payload(n: u64, p: &[Value]) -> Result<Self, FrameError> {
        let [
            device,
            user,
            generation,
            resume,
            wire,
            e2ee,
            media,
            kps,
            groups,
            boot,
        ] = p
        else {
            return Err(FrameError::Shape);
        };
        let groups = groups
            .as_array()
            .ok_or(FrameError::Shape)?
            .iter()
            .map(|g| match g.as_array() {
                Some([id, epoch, seq, outstanding]) => Ok(GroupDigest {
                    group_id: bin(id)?,
                    epoch: u64_of(epoch)?,
                    last_seq: u64_of(seq)?,
                    proposals_outstanding: u53(outstanding)?,
                }),
                _ => Err(FrameError::Shape),
            })
            .collect::<Result<_, _>>()?;
        Ok(Self {
            n,
            device_id: bin(device)?,
            user_id: bin(user)?,
            generation: u64_of(generation)?,
            resume_token: bin(resume)?,
            wire: u64_of(wire)?,
            e2ee: u64_of(e2ee)?,
            media: u64_of(media)?,
            keypackages_remaining: u53(kps)?,
            groups,
            boot_id: bin(boot)?,
        })
    }
}

/// gateway.ts `highestCommon`: the first of this client's preferences the instance offers.
fn highest_common(offered: &[u64], preference: &[u64]) -> Option<u64> {
    preference.iter().copied().find(|v| offered.contains(v))
}

fn newer_than_all(offered: &[u64], preference: &[u64]) -> bool {
    !offered.is_empty() && offered.iter().all(|v| preference.iter().all(|own| v > own))
}

fn older_than_all(offered: &[u64], preference: &[u64]) -> bool {
    !offered.is_empty() && offered.iter().all(|v| preference.iter().all(|own| v < own))
}

/// One write for the writer task: the message and the bytes it counts against [`WRITER_QUEUE_CAP`] (0 for the
/// connection's own frames: heartbeats, identify, the close).
type Queued = (Message, usize);

/// The ready connection's writer, as `send` sees it.
struct Link {
    tx: mpsc::UnboundedSender<Queued>,
    max_frame_bytes: usize,
    /// Bytes `send()` queued that the writer has not written yet.
    queued: Arc<AtomicUsize>,
}

#[derive(Default)]
struct Shared {
    link: Mutex<Option<Link>>,
    hello: Mutex<Option<HelloInfo>>,
    refusal: Mutex<Option<Refusal>>,
}

fn lock<T>(m: &Mutex<T>) -> std::sync::MutexGuard<'_, T> {
    m.lock().unwrap_or_else(std::sync::PoisonError::into_inner)
}

/// The gateway connection: connects at once, keeps reconnecting with gateway.ts's backoff until it is stopped,
/// dropped, or refused for good.
pub struct WsGateway {
    shared: Arc<Shared>,
    events: Mutex<Option<mpsc::Receiver<GatewayEvent>>>,
    stop: watch::Sender<bool>,
}

impl WsGateway {
    /// Opens the gateway of the instance at `base` (its origin; `/gateway` is the path) with the session `auth`.
    /// Must be called inside a tokio runtime: the connection runs as a task on it.
    pub fn connect(base: url::Url, auth: AuthToken) -> WsGateway {
        Self::connect_with(base, auth, GatewayOptions::default())
    }

    pub fn connect_with(base: url::Url, auth: AuthToken, options: GatewayOptions) -> WsGateway {
        let (events_tx, events_rx) = mpsc::channel(options.event_capacity.max(1));
        let (stop, stop_rx) = watch::channel(false);
        let shared = Arc::new(Shared::default());
        let driver = Driver {
            url: gateway_url(&base),
            auth,
            options,
            shared: shared.clone(),
            events: events_tx,
            stop: stop_rx,
            tls: None,
        };
        tokio::spawn(driver.run());
        WsGateway {
            shared,
            events: Mutex::new(Some(events_rx)),
            stop,
        }
    }

    /// Closes the socket (1000) and stops reconnecting; a ready connection then reports `Disconnected { Shutdown }`.
    pub fn stop(&self) {
        let _ = self.stop.send(true);
    }

    /// The hello of the newest connection that reached `ready`.
    pub fn hello(&self) -> Option<HelloInfo> {
        lock(&self.shared.hello).clone()
    }

    /// Why the gateway stopped for good, once it has.
    pub fn refusal(&self) -> Option<Refusal> {
        *lock(&self.shared.refusal)
    }

    /// Whether a connection is ready now (`send` would queue a frame).
    pub fn is_ready(&self) -> bool {
        lock(&self.shared.link).is_some()
    }

    /// [`Gateway::send`] without the future: the write is only queued for the connection's writer, never awaited.
    pub fn send_now(&self, frame: Vec<u8>) -> Result<(), GatewayError> {
        let link = lock(&self.shared.link);
        let Some(link) = link.as_ref() else {
            return Err(GatewayError::Closed);
        };
        let max = if frames::is_request_frame_head(&frame) {
            frames::MAX_RESPONSE_FRAME
        } else {
            link.max_frame_bytes
        };
        if frame.len() > max {
            return Err(GatewayError::TooLarge {
                len: frame.len(),
                max,
            });
        }
        let len = frame.len();
        let before = link.queued.fetch_add(len, Ordering::AcqRel);
        if before > 0 && before.saturating_add(len) > WRITER_QUEUE_CAP {
            link.queued.fetch_sub(len, Ordering::AcqRel);
            return Err(GatewayError::Full {
                queued: before,
                cap: WRITER_QUEUE_CAP,
            });
        }
        link.tx
            .send((Message::Binary(frame.into()), len))
            .map_err(|_| {
                link.queued.fetch_sub(len, Ordering::AcqRel);
                GatewayError::Closed
            })
    }
}

impl Drop for WsGateway {
    fn drop(&mut self) {
        let _ = self.stop.send(true);
    }
}

#[async_trait::async_trait]
impl Gateway for WsGateway {
    /// Writes one encoded frame on the ready connection. `Closed` when there is none (the caller keeps it queued);
    /// `TooLarge` when it is over the hello's `max_frame_bytes` (a request frame, op 59: over the request frame cap),
    /// refused before it is sent, because the instance would close the socket on it.
    /// `Full` when the frames queued for the writer would pass [`WRITER_QUEUE_CAP`].
    async fn send(&self, frame: Vec<u8>) -> Result<(), GatewayError> {
        self.send_now(frame)
    }

    /// The event stream. There is one: a second call answers a receiver that is already closed.
    fn events(&self) -> mpsc::Receiver<GatewayEvent> {
        lock(&self.events)
            .take()
            .unwrap_or_else(|| mpsc::channel(1).1)
    }
}

/// `<origin>/gateway`, ws for http and wss for https (gateway.ts: `origin.replace(/^http/, 'ws') + '/gateway'`).
fn gateway_url(base: &url::Url) -> url::Url {
    let mut url = base.clone();
    url.set_path("/gateway");
    url.set_query(None);
    url.set_fragment(None);
    let scheme = match base.scheme() {
        "https" | "wss" => "wss",
        _ => "ws",
    };
    let _ = url.set_scheme(scheme);
    url
}

/// How one connection attempt ended.
enum End {
    /// `stop()` or the gateway was dropped.
    Stop,
    /// The instance or the negotiation refused this client for good.
    Refused { refusal: Refusal, code: u16 },
    /// Reconnect after the backoff.
    Retry(DisconnectReason),
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum Phase {
    Hello,
    ReadyWait,
    Ready,
}

struct Driver {
    url: url::Url,
    auth: AuthToken,
    options: GatewayOptions,
    shared: Arc<Shared>,
    events: mpsc::Sender<GatewayEvent>,
    stop: watch::Receiver<bool>,
    /// The wss client configuration, built once on the first wss attempt.
    tls: Option<Arc<rustls::ClientConfig>>,
}

impl Driver {
    async fn run(mut self) {
        let mut backoff = Backoff::new();
        loop {
            let mut was_ready = false;
            let end = self.attempt(&mut backoff, &mut was_ready).await;
            *lock(&self.shared.link) = None;
            let mut stop = self.stop.clone();
            match end {
                End::Stop => {
                    // Best effort: a consumer that does not drain its events must not keep a stopped driver alive.
                    if was_ready {
                        let _ = self.events.try_send(GatewayEvent::Disconnected {
                            reason: DisconnectReason::Shutdown,
                        });
                    }
                    return;
                }
                End::Refused { refusal, code } => {
                    *lock(&self.shared.refusal) = Some(refusal);
                    let event = GatewayEvent::Disconnected {
                        reason: DisconnectReason::Server(code),
                    };
                    tokio::select! {
                        biased;
                        () = stopped(&mut stop) => {}
                        _ = self.events.send(event) => {}
                    }
                    return;
                }
                End::Retry(reason) => {
                    let wake = Instant::now() + backoff.next_delay(reconnect::jitter());
                    if was_ready {
                        // No socket is open while this waits on the consumer; only stop() has to win over it.
                        tokio::select! {
                            biased;
                            () = stopped(&mut stop) => return,
                            _ = self.events.send(GatewayEvent::Disconnected { reason }) => {}
                        }
                    }
                    tokio::select! {
                        biased;
                        () = stopped(&mut stop) => return,
                        () = tokio::time::sleep_until(wake) => {}
                    }
                }
            }
        }
    }

    fn request(&self) -> Result<tungstenite::handshake::client::Request, tungstenite::Error> {
        let mut req = self.url.as_str().into_client_request()?;
        let headers = req.headers_mut();
        headers.insert(
            "sec-websocket-protocol",
            HeaderValue::from_static("dilla.v1"),
        );
        // No session: no header (the instance answers the upgrade 401, a refusal).
        let bearer = self.auth.with(|token| {
            token
                .map(|token| {
                    let value = Zeroizing::new(format!("Bearer {token}"));
                    HeaderValue::from_str(&value)
                })
                .transpose()
        });
        if let Some(mut bearer) = bearer.map_err(|e| tungstenite::Error::HttpFormat(e.into()))? {
            bearer.set_sensitive(true);
            headers.insert("authorization", bearer);
        }
        Ok(req)
    }

    /// The wss configuration with an explicit aws-lc-rs provider, never the process default: a binary that also
    /// links ring (LiveKit) has no single default, and `ClientConfig::builder()` would panic on the first wss connect.
    fn tls(&mut self) -> Result<Arc<rustls::ClientConfig>, rustls::Error> {
        if let Some(config) = &self.tls {
            return Ok(config.clone());
        }
        let mut roots = rustls::RootCertStore::empty();
        // A certificate the platform store holds but rustls cannot parse is skipped, as browsers skip it.
        let (_added, _skipped) =
            roots.add_parsable_certificates(rustls_native_certs::load_native_certs().certs);
        for cert in &self.options.extra_roots {
            roots.add(cert.clone())?;
        }
        let provider = Arc::new(rustls::crypto::aws_lc_rs::default_provider());
        let config = rustls::ClientConfig::builder_with_provider(provider)
            .with_safe_default_protocol_versions()?
            .with_root_certificates(roots)
            .with_no_client_auth();
        let config = Arc::new(config);
        self.tls = Some(config.clone());
        Ok(config)
    }

    async fn attempt(&mut self, backoff: &mut Backoff, was_ready: &mut bool) -> End {
        if *self.stop.borrow() {
            return End::Stop;
        }
        let Ok(request) = self.request() else {
            return End::Retry(DisconnectReason::Network);
        };
        let connector = if self.url.scheme() == "wss" {
            match self.tls() {
                Ok(config) => Some(tokio_tungstenite::Connector::Rustls(config)),
                Err(_) => return End::Retry(DisconnectReason::Network),
            }
        } else {
            None
        };
        // The socket reads every frame up to the request frame cap; the hello's own bound is applied per frame below,
        // so an oversized frame is a 4109 close, as in gateway.ts, not a library error.
        let config = WebSocketConfig::default()
            .max_message_size(Some(frames::MAX_RESPONSE_FRAME + 1))
            .max_frame_size(Some(frames::MAX_RESPONSE_FRAME + 1));
        let connecting = tokio_tungstenite::connect_async_tls_with_config(
            request,
            Some(config),
            true,
            connector,
        );
        let mut stop = self.stop.clone();
        let ws = tokio::select! {
            () = stopped(&mut stop) => return End::Stop,
            r = tokio::time::timeout(self.options.hello_timeout, connecting) => match r {
                Ok(Ok((ws, _))) => ws,
                Ok(Err(tungstenite::Error::Http(resp))) if resp.status() == 401 => {
                    return End::Refused { refusal: Refusal::Revoked, code: 401 };
                }
                Ok(Err(_)) | Err(_) => return End::Retry(DisconnectReason::Network),
            },
        };
        let (sink, stream) = ws.split();
        let (out, out_rx) = mpsc::unbounded_channel();
        let queued = Arc::new(AtomicUsize::new(0));
        let writer = tokio::spawn(write_loop(sink, out_rx, queued.clone()));
        let mut conn = Conn {
            out,
            queued,
            phase: Phase::Hello,
            hello: None,
            selected: (0, 0, 0),
            last_n: 0,
            refused_upgrade: false,
            handshake_by: Instant::now() + self.options.hello_timeout,
            beat_at: None,
            ack_by: None,
        };
        let end = conn.run(self, stream, stop, backoff, was_ready).await;
        // The writer ends once it has written what was queued, the close last. A peer that no longer reads must not
        // keep the socket: after a second the writer is aborted, which drops the socket.
        drop(conn);
        tokio::spawn(async move {
            let abort = writer.abort_handle();
            if tokio::time::timeout(Duration::from_secs(1), writer)
                .await
                .is_err()
            {
                abort.abort();
            }
        });
        end
    }
}

type WsStream =
    tokio_tungstenite::WebSocketStream<tokio_tungstenite::MaybeTlsStream<tokio::net::TcpStream>>;

/// Every write of a connection, in order, on its own task: a write blocked on a peer that stops reading never holds
/// the connection's timers, its reads or `stop()`.
async fn write_loop(
    mut sink: futures_util::stream::SplitSink<WsStream, Message>,
    mut queue: mpsc::UnboundedReceiver<Queued>,
    queued: Arc<AtomicUsize>,
) {
    while let Some((message, counted)) = queue.recv().await {
        let last = matches!(message, Message::Close(_));
        let sent = sink.send(message).await;
        queued.fetch_sub(counted, Ordering::AcqRel);
        if sent.is_err() || last {
            return;
        }
    }
}

/// One socket's state: gateway.ts's per-attempt fields.
struct Conn {
    /// The writer task's queue.
    out: mpsc::UnboundedSender<Queued>,
    /// The bytes `send()` has queued on `out` (shared with the writer and the ready link).
    queued: Arc<AtomicUsize>,
    phase: Phase,
    hello: Option<HelloInfo>,
    selected: (u64, u64, u64),
    last_n: u64,
    refused_upgrade: bool,
    handshake_by: Instant,
    beat_at: Option<Instant>,
    ack_by: Option<Instant>,
}

enum Flow {
    Continue,
    End(End),
}

/// Resolves once the gateway is stopped or dropped. The watch guard is released before this returns.
async fn stopped(rx: &mut watch::Receiver<bool>) {
    let _ = rx.wait_for(|stopped| *stopped).await;
}

async fn until(at: Option<Instant>) {
    match at {
        Some(at) => tokio::time::sleep_until(at).await,
        None => std::future::pending().await,
    }
}

/// The shortest heartbeat interval the client accepts. gateway.ts has no explicit floor (a JS timer is clamped to a few
/// milliseconds by the runtime); this is the same guard made explicit, so a server-sent `heartbeat_ms` of 0 or 1 cannot
/// make the client send heartbeats in a tight loop.
const MIN_BEAT_INTERVAL: Duration = Duration::from_millis(10);

/// `min(heartbeat_ms / 2, heartbeat_max)`, never below [`MIN_BEAT_INTERVAL`].
fn beat_interval_for(heartbeat_ms: u64, heartbeat_max: Duration) -> Duration {
    Duration::from_millis(heartbeat_ms / 2)
        .min(heartbeat_max)
        .max(MIN_BEAT_INTERVAL)
}

impl Conn {
    async fn run(
        &mut self,
        d: &Driver,
        mut stream: futures_util::stream::SplitStream<WsStream>,
        mut stop: watch::Receiver<bool>,
        backoff: &mut Backoff,
        was_ready: &mut bool,
    ) -> End {
        loop {
            let handshake = (self.phase != Phase::Ready).then_some(self.handshake_by);
            let flow = tokio::select! {
                biased;
                () = stopped(&mut stop) => self.close_with(ClientClose::NORMAL, End::Stop),
                () = until(self.beat_at) => self.beat(&d.options),
                // Inbound before the deadlines: an ack (or a ready) already on the socket wins over a deadline that
                // expired while it waited, so a stall elsewhere never turns into a spurious 4101 or 4100.
                msg = stream.next() => match msg {
                    None | Some(Err(_)) => Flow::End(End::Retry(DisconnectReason::Network)),
                    Some(Ok(Message::Close(frame))) => Flow::End(self.on_close(frame)),
                    Some(Ok(Message::Binary(bytes))) => {
                        self.on_binary(d, &mut stop, &bytes, backoff, was_ready).await
                    }
                    Some(Ok(Message::Text(_))) => self.teardown(ClientClose::UNDECODABLE),
                    Some(Ok(_)) => Flow::Continue,
                },
                () = until(handshake) => self.teardown(ClientClose::HELLO_TIMEOUT),
                () = until(self.ack_by) => self.teardown(ClientClose::HEARTBEAT_TIMEOUT),
            };
            if let Flow::End(end) = flow {
                return end;
            }
        }
    }

    /// Queues a message for the writer; false when the writer is gone (its socket failed).
    fn queue(&self, message: Message) -> bool {
        self.out.send((message, 0)).is_ok()
    }

    /// Queues the close frame and ends the attempt with `end`. The writer sends it after what was already queued.
    fn close_with(&mut self, code: u16, end: End) -> Flow {
        let frame = CloseFrame {
            code: CloseCode::from(code),
            reason: "".into(),
        };
        let _ = self.queue(Message::Close(Some(frame)));
        Flow::End(end)
    }

    /// gateway.ts `teardown(code, true)`: close with the client's code, then reconnect after the backoff.
    fn teardown(&mut self, code: u16) -> Flow {
        self.close_with(code, End::Retry(DisconnectReason::Network))
    }

    fn write(&self, frame: &frames::Frame) -> bool {
        frames::encode(frame).is_ok_and(|bytes| self.queue(Message::Binary(bytes.into())))
    }

    /// gateway.ts `scheduleBeat`: `[last_n, 0]`, n = 0; the first beat without an ack arms the ack timeout.
    fn beat(&mut self, options: &GatewayOptions) -> Flow {
        let beat = frames::Frame {
            op: Op::HEARTBEAT,
            n: 0,
            group_id: None,
            payload: vec![Value::Uint(self.last_n), Value::Uint(0)],
        };
        if !self.write(&beat) {
            return Flow::End(End::Retry(DisconnectReason::Network));
        }
        let now = Instant::now();
        if self.ack_by.is_none() {
            self.ack_by = Some(now + options.ack_timeout);
        }
        self.beat_at = Some(now + self.beat_interval(options));
        Flow::Continue
    }

    fn beat_interval(&self, options: &GatewayOptions) -> Duration {
        let heartbeat_ms = self.hello.as_ref().map_or(0, |h| h.heartbeat_ms);
        beat_interval_for(heartbeat_ms, options.heartbeat_max)
    }

    /// Hands one event to the consumer. While the channel is full the heartbeats go on and `stop()` still wins; a
    /// consumer that has not taken the event within the ack timeout gets a teardown (close 1000, `Disconnected
    /// { Network }`, reconnect) instead of a socket the instance would time out anyway.
    async fn deliver(
        &mut self,
        d: &Driver,
        stop: &mut watch::Receiver<bool>,
        event: GatewayEvent,
    ) -> Flow {
        let stall = Instant::now() + d.options.ack_timeout;
        let send = d.events.send(event);
        tokio::pin!(send);
        loop {
            tokio::select! {
                biased;
                () = stopped(stop) => return self.close_with(ClientClose::NORMAL, End::Stop),
                // A dropped receiver only means nobody listens; the lifecycle goes on regardless.
                _ = &mut send => return Flow::Continue,
                () = until(self.beat_at) => {
                    if let Flow::End(end) = self.beat(&d.options) {
                        return Flow::End(end);
                    }
                }
                () = tokio::time::sleep_until(stall) => return self.teardown(ClientClose::NORMAL),
            }
        }
    }

    /// gateway.ts `onClose`: 4004 is a revocation and 4006 a version refusal, both final. 4003 is final too, since this
    /// client's credential is the session itself (gateway.ts would re-mint a ticket, get 401 and stop the same way);
    /// with the reason "second factor required" (the instance's liveness sweep, internal/gateway/heartbeat.go) it means
    /// the account owes a second factor, not a new sign-in. Any other close reconnects.
    fn on_close(&self, frame: Option<CloseFrame>) -> End {
        let Some(frame) = frame else {
            return End::Retry(DisconnectReason::Network);
        };
        let code = u16::from(frame.code);
        match code {
            CLOSE_UNAUTHENTICATED if frame.reason.as_str() == SECOND_FACTOR_REQUIRED => {
                End::Refused {
                    refusal: Refusal::SecondFactorRequired,
                    code,
                }
            }
            CLOSE_REVOKED | CLOSE_UNAUTHENTICATED => End::Refused {
                refusal: Refusal::Revoked,
                code,
            },
            ClientClose::VERSION => {
                let upgrade = frame.reason.as_str() == UPGRADE_REQUIRED || self.refused_upgrade;
                let refusal = if upgrade {
                    Refusal::UpgradeRequired
                } else {
                    Refusal::VersionUnsupported
                };
                End::Refused { refusal, code }
            }
            _ => End::Retry(DisconnectReason::Server(code)),
        }
    }

    /// gateway.ts `onMessage`.
    async fn on_binary(
        &mut self,
        d: &Driver,
        stop: &mut watch::Receiver<bool>,
        bytes: &[u8],
        backoff: &mut Backoff,
        was_ready: &mut bool,
    ) -> Flow {
        let ready = self.phase == Phase::Ready;
        let response = ready && frames::is_response_frame_head(bytes);
        let bound = if response {
            frames::MAX_RESPONSE_FRAME
        } else {
            self.hello.as_ref().map_or(DEFAULT_MAX_FRAME_BYTES, |h| {
                usize::try_from(h.max_frame_bytes).unwrap_or(usize::MAX)
            })
        };
        if bytes.len() > bound {
            return self.teardown(ClientClose::TOO_LARGE);
        }
        if response {
            if frames::decode_response(bytes).is_err() {
                return self.teardown(ClientClose::UNDECODABLE);
            }
            return self
                .deliver(d, stop, GatewayEvent::Frame(bytes.to_vec()))
                .await;
        }
        let frame = match frames::decode(bytes) {
            Ok(f) => f,
            // A malformed voice, ring, structure, away, app-build or Bot frame of a ready session is dropped and the
            // socket stays open; any other undecodable frame is a protocol failure.
            Err(_)
                if ready
                    && frames::frame_op(bytes)
                        .is_some_and(|op| frames::DROPPABLE_OPS.contains(&op)) =>
            {
                return Flow::Continue;
            }
            Err(_) => return self.teardown(ClientClose::UNDECODABLE),
        };
        self.last_n = self.last_n.max(frame.n);
        match self.phase {
            Phase::Hello => self.on_hello(d, &frame),
            Phase::ReadyWait => match frame.op {
                Op::READY => {
                    self.on_ready(d, stop, bytes, &frame, backoff, was_ready)
                        .await
                }
                Op::INVALID_SESSION | Op::RECONNECT => self.reconnect_frame(&frame, backoff),
                Op::ERROR => {
                    if frame.payload.get(1).and_then(Value::as_text) == Some(UPGRADE_REQUIRED) {
                        self.refused_upgrade = true;
                    }
                    Flow::Continue
                }
                _ => Flow::Continue,
            },
            Phase::Ready => match frame.op {
                Op::HEARTBEAT_ACK => {
                    self.ack_by = None;
                    Flow::Continue
                }
                Op::HANDSHAKE..=Op::MESSAGE_DELETED
                | Op::PRESENCE
                | Op::TYPING
                | Op::VOICE_STATE
                | Op::CALL_RING
                | Op::COMMUNITY_CHANGED
                | Op::AFK_AWAY
                | Op::BOT_CATALOG
                | Op::BOT_REPLY
                | Op::BOT_STATE_CHANGED => {
                    self.deliver(d, stop, GatewayEvent::Frame(bytes.to_vec()))
                        .await
                }
                Op::APP_BUILD if self.selected.0 >= 5 => {
                    self.deliver(d, stop, GatewayEvent::Frame(bytes.to_vec()))
                        .await
                }
                Op::INVALID_SESSION | Op::RECONNECT => self.reconnect_frame(&frame, backoff),
                _ => Flow::Continue,
            },
        }
    }

    fn on_hello(&mut self, d: &Driver, frame: &frames::Frame) -> Flow {
        if frame.op != Op::HELLO {
            return self.teardown(ClientClose::UNDECODABLE);
        }
        let Ok(hello) = HelloInfo::from_payload(&frame.payload) else {
            return self.teardown(ClientClose::UNDECODABLE);
        };
        let wire = highest_common(&hello.wire_versions, WIRE_PREFERENCE);
        let e2ee = highest_common(&hello.e2ee_versions, E2EE_PREFERENCE);
        let media = highest_common(&hello.media_versions, MEDIA_PREFERENCE);
        let (Some(wire), Some(e2ee), Some(media)) = (wire, e2ee, media) else {
            let refusal = if wire.is_none() && newer_than_all(&hello.wire_versions, WIRE_PREFERENCE)
            {
                Refusal::UpgradeRequired
            } else if wire.is_none() && older_than_all(&hello.wire_versions, WIRE_PREFERENCE) {
                Refusal::InstanceOutdated
            } else {
                Refusal::VersionUnsupported
            };
            return self.close_with(
                ClientClose::VERSION,
                End::Refused {
                    refusal,
                    code: ClientClose::VERSION,
                },
            );
        };
        self.selected = (wire, e2ee, media);
        self.hello = Some(hello);
        let identify = frames::Frame {
            op: Op::IDENTIFY,
            n: 1,
            group_id: None,
            payload: vec![
                Value::Text(String::new()),
                Value::Uint(wire),
                Value::Uint(e2ee),
                Value::Uint(media),
                Value::Uint(0),
            ],
        };
        if !self.write(&identify) {
            return Flow::End(End::Retry(DisconnectReason::Network));
        }
        self.phase = Phase::ReadyWait;
        self.handshake_by = Instant::now() + d.options.hello_timeout;
        Flow::Continue
    }

    async fn on_ready(
        &mut self,
        d: &Driver,
        stop: &mut watch::Receiver<bool>,
        bytes: &[u8],
        frame: &frames::Frame,
        backoff: &mut Backoff,
        was_ready: &mut bool,
    ) -> Flow {
        // gateway.ts `readReady`: a ready that does not echo the versions identify selected is a protocol failure.
        let echoes = ReadyInfo::from_payload(frame.n, &frame.payload)
            .is_ok_and(|info| (info.wire, info.e2ee, info.media) == self.selected);
        let (true, Some(hello)) = (echoes, self.hello.clone()) else {
            return self.teardown(ClientClose::UNDECODABLE);
        };
        backoff.ready();
        self.phase = Phase::Ready;
        *was_ready = true;
        let max_frame_bytes = usize::try_from(hello.max_frame_bytes).unwrap_or(usize::MAX);
        *lock(&d.shared.link) = Some(Link {
            tx: self.out.clone(),
            max_frame_bytes,
            queued: self.queued.clone(),
        });
        *lock(&d.shared.hello) = Some(hello);
        self.beat_at = Some(Instant::now() + self.beat_interval(&d.options));
        self.deliver(
            d,
            stop,
            GatewayEvent::Connected {
                session: bytes.to_vec(),
            },
        )
        .await
    }

    /// gateway.ts `reconnectFrame`: `reconnect` names its own wait, `invalid_session` none; close normally, reconnect.
    fn reconnect_frame(&mut self, frame: &frames::Frame, backoff: &mut Backoff) -> Flow {
        let after = match frame.op {
            Op::RECONNECT => frame.payload.get(1).and_then(|v| u53(v).ok()).unwrap_or(0),
            _ => 0,
        };
        backoff.set_after(after);
        self.teardown(ClientClose::NORMAL)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn beat_interval_is_clamped() {
        let max = Duration::from_millis(15_000);
        assert_eq!(
            beat_interval_for(30_000, max),
            Duration::from_millis(15_000)
        );
        assert_eq!(beat_interval_for(100_000, max), max);
        assert_eq!(beat_interval_for(100, max), Duration::from_millis(50));
        assert_eq!(beat_interval_for(0, max), MIN_BEAT_INTERVAL);
        assert_eq!(beat_interval_for(1, max), MIN_BEAT_INTERVAL);
        assert_eq!(beat_interval_for(15, max), MIN_BEAT_INTERVAL);
    }

    #[test]
    fn negotiation_mirrors_gateway_ts() {
        assert_eq!(highest_common(&[5, 6, 7], WIRE_PREFERENCE), Some(6));
        assert_eq!(highest_common(&[7], WIRE_PREFERENCE), None);
        assert!(newer_than_all(&[7, 8], WIRE_PREFERENCE));
        assert!(!newer_than_all(&[], WIRE_PREFERENCE));
        assert!(older_than_all(&[4, 5], WIRE_PREFERENCE));
        assert!(!older_than_all(&[5, 7], WIRE_PREFERENCE));
    }

    #[test]
    fn closes_map_to_refusals() {
        let (out, _queue) = mpsc::unbounded_channel();
        let conn = Conn {
            out,
            queued: Arc::default(),
            phase: Phase::Ready,
            hello: None,
            selected: (6, 1, 1),
            last_n: 0,
            refused_upgrade: false,
            handshake_by: Instant::now(),
            beat_at: None,
            ack_by: None,
        };
        let close = |code: u16, reason: &str| {
            conn.on_close(Some(CloseFrame {
                code: CloseCode::from(code),
                reason: reason.to_owned().into(),
            }))
        };
        // internal/gateway/heartbeat.go: the liveness sweep's close for an account that owes a second factor.
        assert!(matches!(
            close(4003, "second factor required"),
            End::Refused {
                refusal: Refusal::SecondFactorRequired,
                code: 4003
            }
        ));
        assert!(matches!(
            close(4003, ""),
            End::Refused {
                refusal: Refusal::Revoked,
                code: 4003
            }
        ));
        assert!(matches!(
            close(4004, "revoked"),
            End::Refused {
                refusal: Refusal::Revoked,
                code: 4004
            }
        ));
        assert!(matches!(
            close(4006, "E_UPGRADE_REQUIRED"),
            End::Refused {
                refusal: Refusal::UpgradeRequired,
                code: 4006
            }
        ));
        assert!(matches!(
            close(4006, "E_VERSION"),
            End::Refused {
                refusal: Refusal::VersionUnsupported,
                ..
            }
        ));
        assert!(matches!(
            close(4010, "going away"),
            End::Retry(DisconnectReason::Server(4010))
        ));
        assert!(matches!(
            conn.on_close(None),
            End::Retry(DisconnectReason::Network)
        ));
    }

    #[test]
    fn gateway_url_from_origin() {
        let u = |s: &str| gateway_url(&url::Url::parse(s).unwrap()).to_string();
        assert_eq!(u("http://127.0.0.1:8443"), "ws://127.0.0.1:8443/gateway");
        assert_eq!(
            u("https://chat.example/app?x=1#y"),
            "wss://chat.example/gateway"
        );
    }

    #[test]
    fn token_is_not_printed() {
        assert_eq!(
            format!("{:?}", AuthToken::new("secret")),
            "AuthToken(<redacted>)"
        );
    }

    /// A gateway whose ready link writes into `queue` and nowhere else: nothing drains it unless the test does.
    fn gateway_with_link() -> (WsGateway, mpsc::UnboundedReceiver<Queued>, Arc<AtomicUsize>) {
        let (tx, queue) = mpsc::unbounded_channel();
        let queued = Arc::new(AtomicUsize::new(0));
        let shared = Arc::new(Shared::default());
        *lock(&shared.link) = Some(Link {
            tx,
            max_frame_bytes: DEFAULT_MAX_FRAME_BYTES,
            queued: queued.clone(),
        });
        let (stop, _) = watch::channel(false);
        let gw = WsGateway {
            shared,
            events: Mutex::new(None),
            stop,
        };
        (gw, queue, queued)
    }

    fn request_frame(body: usize) -> Vec<u8> {
        frames::encode_request(&frames::RequestFrame {
            id: 1,
            method: "POST".into(),
            route: "/v1/groups/00000000000000000000000000000000/message".into(),
            body: vec![0; body],
            idem: Some(vec![1; 16]),
        })
        .unwrap()
    }

    #[test]
    fn writer_queue_is_capped() {
        let (gw, mut queue, queued) = gateway_with_link();
        // One frame is always taken on an empty queue, however large (a 16 MiB request frame).
        let max = request_frame(frames::MAX_REQUEST_BODY);
        gw.send_now(max.clone())
            .expect("an empty queue takes one frame");
        assert!(matches!(
            gw.send_now(max).expect_err("two max frames pass the cap"),
            GatewayError::Full { .. }
        ));
        drain(&mut queue, &queued);
        // 10 MiB frames: three fit under 32 MiB, the fourth does not, and nothing is queued for it.
        let ten = request_frame(10 << 20);
        for _ in 0..3 {
            gw.send_now(ten.clone()).expect("within the cap");
        }
        let full = gw.send_now(ten.clone()).expect_err("past the cap");
        assert!(
            matches!(full, GatewayError::Full { cap, queued: q } if cap == WRITER_QUEUE_CAP && q == 3 * ten.len()),
            "{full:?}"
        );
        assert_eq!(queued.load(Ordering::Acquire), 3 * ten.len());
        // The writer writes one: its bytes leave the count and the next frame is taken.
        let (_, counted) = queue.try_recv().unwrap();
        queued.fetch_sub(counted, Ordering::AcqRel);
        gw.send_now(ten)
            .expect("room again once the writer wrote one");
    }

    fn drain(queue: &mut mpsc::UnboundedReceiver<Queued>, queued: &AtomicUsize) {
        while let Ok((_, counted)) = queue.try_recv() {
            queued.fetch_sub(counted, Ordering::AcqRel);
        }
    }
}
