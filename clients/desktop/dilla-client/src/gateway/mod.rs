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
//!   (`Server(4004)` revoked, `Server(4003)` unauthenticated, `Server(4006)` versions, `Server(401)` at the upgrade:
//!   see [`WsGateway::refusal`]), or `Shutdown` after [`WsGateway::stop`].

pub mod frames;
pub mod reconnect;

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

/// A device session's bearer token, held zeroized and never printed.
#[derive(Clone)]
pub struct AuthToken(Zeroizing<String>);

impl AuthToken {
    pub fn new(token: String) -> Self {
        Self(Zeroizing::new(token))
    }

    pub fn expose(&self) -> &str {
        &self.0
    }
}

impl std::fmt::Debug for AuthToken {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("AuthToken(..)")
    }
}

/// gateway.ts `WIRE_PREFERENCE`: wire 6 (Bot v2) is the only wire this client speaks.
pub const WIRE_PREFERENCE: &[u64] = &[6];
/// gateway.ts `highestCommon(versions[1], [1n])`.
pub const E2EE_PREFERENCE: &[u64] = &[1];
/// gateway.ts `highestCommon(versions[2], [1n])`.
pub const MEDIA_PREFERENCE: &[u64] = &[1];
/// gateway.ts `DEFAULT_MAX_FRAME_BYTES`: the bound on frames read before a hello names its own.
pub const DEFAULT_MAX_FRAME_BYTES: usize = 131_584;
const UPGRADE_REQUIRED: &str = "E_UPGRADE_REQUIRED";

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
    /// The events channel's capacity. A consumer that stops draining it stalls the socket.
    pub event_capacity: usize,
}

impl Default for GatewayOptions {
    fn default() -> Self {
        Self {
            heartbeat_max: Duration::from_millis(15_000),
            ack_timeout: Duration::from_millis(10_000),
            hello_timeout: Duration::from_millis(15_000),
            event_capacity: 1024,
        }
    }
}

/// Why a gateway stopped for good. The app acts on it: sign in again, or ask its user (or the operator) to update.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Refusal {
    /// The session was revoked (close 4004), is not accepted (close 4003), or the upgrade answered 401.
    Revoked,
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

/// The ready connection's writer, as `send` sees it.
struct Link {
    tx: mpsc::UnboundedSender<Vec<u8>>,
    max_frame_bytes: usize,
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
    async fn send(&self, frame: Vec<u8>) -> Result<(), GatewayError> {
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
        link.tx.send(frame).map_err(|_| GatewayError::Closed)
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
}

impl Driver {
    async fn run(mut self) {
        let mut backoff = Backoff::new();
        loop {
            let mut was_ready = false;
            let end = self.attempt(&mut backoff, &mut was_ready).await;
            *lock(&self.shared.link) = None;
            match end {
                End::Stop => {
                    if was_ready {
                        self.emit(GatewayEvent::Disconnected {
                            reason: DisconnectReason::Shutdown,
                        })
                        .await;
                    }
                    return;
                }
                End::Refused { refusal, code } => {
                    *lock(&self.shared.refusal) = Some(refusal);
                    self.emit(GatewayEvent::Disconnected {
                        reason: DisconnectReason::Server(code),
                    })
                    .await;
                    return;
                }
                End::Retry(reason) => {
                    if was_ready {
                        self.emit(GatewayEvent::Disconnected { reason }).await;
                    }
                    let delay = backoff.next_delay(reconnect::jitter());
                    tokio::select! {
                        () = tokio::time::sleep(delay) => {}
                        () = stopped(&mut self.stop) => return,
                    }
                }
            }
        }
    }

    async fn emit(&self, event: GatewayEvent) {
        // A dropped receiver only means nobody listens; the connection's lifecycle goes on regardless.
        let _ = self.events.send(event).await;
    }

    fn request(&self) -> Result<tungstenite::handshake::client::Request, tungstenite::Error> {
        let mut req = self.url.as_str().into_client_request()?;
        let headers = req.headers_mut();
        headers.insert(
            "sec-websocket-protocol",
            HeaderValue::from_static("dilla.v1"),
        );
        let mut bearer = HeaderValue::from_str(&format!("Bearer {}", self.auth.expose()))
            .map_err(|e| tungstenite::Error::HttpFormat(e.into()))?;
        bearer.set_sensitive(true);
        headers.insert("authorization", bearer);
        Ok(req)
    }

    async fn attempt(&mut self, backoff: &mut Backoff, was_ready: &mut bool) -> End {
        if *self.stop.borrow() {
            return End::Stop;
        }
        let Ok(request) = self.request() else {
            return End::Retry(DisconnectReason::Network);
        };
        // The socket reads every frame up to the request frame cap; the hello's own bound is applied per frame below,
        // so an oversized frame is a 4109 close, as in gateway.ts, not a library error.
        let config = WebSocketConfig::default()
            .max_message_size(Some(frames::MAX_RESPONSE_FRAME + 1))
            .max_frame_size(Some(frames::MAX_RESPONSE_FRAME + 1));
        let connecting =
            tokio_tungstenite::connect_async_tls_with_config(request, Some(config), true, None);
        let ws = tokio::select! {
            () = stopped(&mut self.stop) => return End::Stop,
            r = tokio::time::timeout(self.options.hello_timeout, connecting) => match r {
                Ok(Ok((ws, _))) => ws,
                Ok(Err(tungstenite::Error::Http(resp))) if resp.status() == 401 => {
                    return End::Refused { refusal: Refusal::Revoked, code: 401 };
                }
                Ok(Err(_)) | Err(_) => return End::Retry(DisconnectReason::Network),
            },
        };
        let (sink, stream) = ws.split();
        let (out_tx, out_rx) = mpsc::unbounded_channel();
        let mut conn = Conn {
            sink,
            phase: Phase::Hello,
            hello: None,
            selected: (0, 0, 0),
            last_n: 0,
            refused_upgrade: false,
            handshake_by: Instant::now() + self.options.hello_timeout,
            beat_at: None,
            ack_by: None,
            out_tx,
        };
        conn.run(self, stream, out_rx, backoff, was_ready).await
    }
}

type WsStream =
    tokio_tungstenite::WebSocketStream<tokio_tungstenite::MaybeTlsStream<tokio::net::TcpStream>>;

/// One socket's state: gateway.ts's per-attempt fields.
struct Conn {
    sink: futures_util::stream::SplitSink<WsStream, Message>,
    phase: Phase,
    hello: Option<HelloInfo>,
    selected: (u64, u64, u64),
    last_n: u64,
    refused_upgrade: bool,
    handshake_by: Instant,
    beat_at: Option<Instant>,
    ack_by: Option<Instant>,
    out_tx: mpsc::UnboundedSender<Vec<u8>>,
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

impl Conn {
    async fn run(
        &mut self,
        d: &Driver,
        mut stream: futures_util::stream::SplitStream<WsStream>,
        mut out_rx: mpsc::UnboundedReceiver<Vec<u8>>,
        backoff: &mut Backoff,
        was_ready: &mut bool,
    ) -> End {
        let mut stop = d.stop.clone();
        loop {
            let handshake = (self.phase != Phase::Ready).then_some(self.handshake_by);
            let flow = tokio::select! {
                biased;
                () = stopped(&mut stop) => {
                    self.close(ClientClose::NORMAL, "").await;
                    Flow::End(End::Stop)
                }
                () = until(handshake) => self.teardown(ClientClose::HELLO_TIMEOUT).await,
                () = until(self.ack_by) => self.teardown(ClientClose::HEARTBEAT_TIMEOUT).await,
                () = until(self.beat_at) => self.beat(&d.options).await,
                Some(bytes) = out_rx.recv() => match self.sink.send(Message::Binary(bytes.into())).await {
                    Ok(()) => Flow::Continue,
                    Err(_) => Flow::End(End::Retry(DisconnectReason::Network)),
                },
                msg = stream.next() => match msg {
                    None | Some(Err(_)) => Flow::End(End::Retry(DisconnectReason::Network)),
                    Some(Ok(Message::Close(frame))) => Flow::End(self.on_close(frame)),
                    Some(Ok(Message::Binary(bytes))) => self.on_binary(d, &bytes, backoff, was_ready).await,
                    Some(Ok(Message::Text(_))) => self.teardown(ClientClose::UNDECODABLE).await,
                    Some(Ok(_)) => Flow::Continue,
                },
            };
            if let Flow::End(end) = flow {
                return end;
            }
        }
    }

    /// Sends a close frame, bounded: a peer that no longer reads must not hold the reconnect.
    async fn close(&mut self, code: u16, reason: &str) {
        let frame = CloseFrame {
            code: CloseCode::from(code),
            reason: reason.to_owned().into(),
        };
        let _ = tokio::time::timeout(
            Duration::from_secs(1),
            self.sink.send(Message::Close(Some(frame))),
        )
        .await;
    }

    /// gateway.ts `teardown(code, true)`: close with the client's code, then reconnect after the backoff.
    async fn teardown(&mut self, code: u16) -> Flow {
        self.close(code, "").await;
        Flow::End(End::Retry(DisconnectReason::Network))
    }

    async fn write(&mut self, frame: &frames::Frame) -> Result<(), ()> {
        let bytes = frames::encode(frame).map_err(|_| ())?;
        self.sink
            .send(Message::Binary(bytes.into()))
            .await
            .map_err(|_| ())
    }

    /// gateway.ts `scheduleBeat`: `[last_n, 0]`, n = 0; the first beat without an ack arms the ack timeout.
    async fn beat(&mut self, options: &GatewayOptions) -> Flow {
        let beat = frames::Frame {
            op: Op::HEARTBEAT,
            n: 0,
            group_id: None,
            payload: vec![Value::Uint(self.last_n), Value::Uint(0)],
        };
        if self.write(&beat).await.is_err() {
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
        let half = Duration::from_millis(self.hello.as_ref().map_or(0, |h| h.heartbeat_ms / 2));
        half.min(options.heartbeat_max)
            .max(Duration::from_millis(1))
    }

    /// gateway.ts `onClose`: 4004 is a revocation and 4006 a version refusal, both final; 4003 is final too, since
    /// this client's credential is the session itself (gateway.ts would re-mint a ticket, get 401 and stop the same
    /// way). Any other close reconnects.
    fn on_close(&self, frame: Option<CloseFrame>) -> End {
        let Some(frame) = frame else {
            return End::Retry(DisconnectReason::Network);
        };
        let code = u16::from(frame.code);
        match code {
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
            return self.teardown(ClientClose::TOO_LARGE).await;
        }
        if response {
            if frames::decode_response(bytes).is_err() {
                return self.teardown(ClientClose::UNDECODABLE).await;
            }
            d.emit(GatewayEvent::Frame(bytes.to_vec())).await;
            return Flow::Continue;
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
            Err(_) => return self.teardown(ClientClose::UNDECODABLE).await,
        };
        self.last_n = self.last_n.max(frame.n);
        match self.phase {
            Phase::Hello => self.on_hello(d, &frame).await,
            Phase::ReadyWait => match frame.op {
                Op::READY => self.on_ready(d, bytes, &frame, backoff, was_ready).await,
                Op::INVALID_SESSION | Op::RECONNECT => self.reconnect_frame(&frame, backoff).await,
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
                    d.emit(GatewayEvent::Frame(bytes.to_vec())).await;
                    Flow::Continue
                }
                Op::APP_BUILD if self.selected.0 >= 5 => {
                    d.emit(GatewayEvent::Frame(bytes.to_vec())).await;
                    Flow::Continue
                }
                Op::INVALID_SESSION | Op::RECONNECT => self.reconnect_frame(&frame, backoff).await,
                _ => Flow::Continue,
            },
        }
    }

    async fn on_hello(&mut self, d: &Driver, frame: &frames::Frame) -> Flow {
        if frame.op != Op::HELLO {
            return self.teardown(ClientClose::UNDECODABLE).await;
        }
        let Ok(hello) = HelloInfo::from_payload(&frame.payload) else {
            return self.teardown(ClientClose::UNDECODABLE).await;
        };
        let wire = highest_common(&hello.wire_versions, WIRE_PREFERENCE);
        let e2ee = highest_common(&hello.e2ee_versions, E2EE_PREFERENCE);
        let media = highest_common(&hello.media_versions, MEDIA_PREFERENCE);
        let (Some(wire), Some(e2ee), Some(media)) = (wire, e2ee, media) else {
            self.close(ClientClose::VERSION, "").await;
            let refusal = if wire.is_none() && newer_than_all(&hello.wire_versions, WIRE_PREFERENCE)
            {
                Refusal::UpgradeRequired
            } else if wire.is_none() && older_than_all(&hello.wire_versions, WIRE_PREFERENCE) {
                Refusal::InstanceOutdated
            } else {
                Refusal::VersionUnsupported
            };
            return Flow::End(End::Refused {
                refusal,
                code: ClientClose::VERSION,
            });
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
        if self.write(&identify).await.is_err() {
            return Flow::End(End::Retry(DisconnectReason::Network));
        }
        self.phase = Phase::ReadyWait;
        self.handshake_by = Instant::now() + d.options.hello_timeout;
        Flow::Continue
    }

    async fn on_ready(
        &mut self,
        d: &Driver,
        bytes: &[u8],
        frame: &frames::Frame,
        backoff: &mut Backoff,
        was_ready: &mut bool,
    ) -> Flow {
        // gateway.ts `readReady`: a ready that does not echo the versions identify selected is a protocol failure.
        let echoes = ReadyInfo::from_payload(frame.n, &frame.payload)
            .is_ok_and(|info| (info.wire, info.e2ee, info.media) == self.selected);
        let (true, Some(hello)) = (echoes, self.hello.clone()) else {
            return self.teardown(ClientClose::UNDECODABLE).await;
        };
        backoff.ready();
        self.phase = Phase::Ready;
        *was_ready = true;
        let max_frame_bytes = usize::try_from(hello.max_frame_bytes).unwrap_or(usize::MAX);
        *lock(&d.shared.link) = Some(Link {
            tx: self.out_tx.clone(),
            max_frame_bytes,
        });
        *lock(&d.shared.hello) = Some(hello);
        d.emit(GatewayEvent::Connected {
            session: bytes.to_vec(),
        })
        .await;
        self.beat_at = Some(Instant::now() + self.beat_interval(&d.options));
        Flow::Continue
    }

    /// gateway.ts `reconnectFrame`: `reconnect` names its own wait, `invalid_session` none; close normally, reconnect.
    async fn reconnect_frame(&mut self, frame: &frames::Frame, backoff: &mut Backoff) -> Flow {
        let after = match frame.op {
            Op::RECONNECT => frame.payload.get(1).and_then(|v| u53(v).ok()).unwrap_or(0),
            _ => 0,
        };
        backoff.set_after(after);
        self.teardown(ClientClose::NORMAL).await
    }
}

#[cfg(test)]
mod tests {
    use super::*;

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
            format!("{:?}", AuthToken::new("secret".into())),
            "AuthToken(..)"
        );
    }
}
