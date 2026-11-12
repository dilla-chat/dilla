//! App requests over the gateway socket (Bot v2): a port of `packages/client-core/src/gateway/requests.ts`. One request
//! frame (op 59) per request, answered by a response frame (op 60) with its id, in any order.
//!
//! Requests are written in submission order; while the socket is reconnecting they wait and are written in that order
//! once it is ready again. A request whose answer was lost with a socket, or did not come within the attempt deadline,
//! is written again with the same idempotency key, so the instance's replay record answers it instead of applying it
//! twice. A written mutation is not written again once its first write is [`RESEND_WINDOW`] old, or once the socket
//! reached another instance incarnation: it fails `E_OUTCOME_UNKNOWN` and the caller's lost-answer handling decides.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use tokio::sync::oneshot;
use tokio::task::AbortHandle;
use tokio::time::Instant;

use crate::gateway::WsGateway;
use crate::gateway::frames::{self, MAX_REQUEST_BODY, RequestFrame};
use crate::seams::GatewayError;

use super::api::ApiError;

/// requests.ts `SOCKET_ATTEMPT_TIMEOUT_MS`: just above the instance's 60 s dispatch deadline.
pub const ATTEMPT_TIMEOUT: Duration = Duration::from_millis(65_000);
/// requests.ts `RESEND_WINDOW_MS`: the instance keeps a replay answer for 5 minutes.
pub const RESEND_WINDOW: Duration = Duration::from_secs(4 * 60);
/// requests.ts `SOCKET_REQUESTS`.
pub const MAX_IN_FLIGHT: usize = 64;
const BUSY_PAUSE_MS: u64 = 100;
const BUSY_JITTER_MS: u64 = 100;
const UNAVAILABLE_ATTEMPTS: u32 = 5;
const UNAVAILABLE_BASE_MS: u64 = 500;
const UNAVAILABLE_CAP_MS: u64 = 8_000;
const UNAVAILABLE_JITTER_MS: u64 = 250;
const UNAVAILABLE_MAX_WAIT_MS: u64 = 60_000;
const TIMEOUT_ATTEMPTS: u32 = 5;
/// How soon a request the writer queue refused (`GatewayError::Full`) is tried again.
const FULL_RETRY: Duration = Duration::from_millis(50);

/// An answer: the status and the body, as an HTTP answer would carry them.
#[derive(Clone, Debug)]
pub struct Answer {
    pub status: u16,
    pub body: Vec<u8>,
}

struct Pending {
    seq: u64,
    method: &'static str,
    route: String,
    body: Vec<u8>,
    idem: Option<Vec<u8>>,
    tx: Option<oneshot::Sender<Result<Answer, ApiError>>>,
    unavailable: u32,
    timed_out: u32,
    written: bool,
    first_written_at: Instant,
    written_under: Option<String>,
    deadline: Option<AbortHandle>,
    timer: Option<AbortHandle>,
}

impl Pending {
    fn settle(&mut self, result: Result<Answer, ApiError>) {
        if let Some(t) = self.deadline.take() {
            t.abort();
        }
        if let Some(t) = self.timer.take() {
            t.abort();
        }
        if let Some(tx) = self.tx.take() {
            let _ = tx.send(result);
        }
    }

    /// requests.ts `lostError`: a written mutation's outcome is unknown, anything else never reached the instance.
    fn lost(&self, detail: &str) -> ApiError {
        if self.written && self.method != "GET" {
            ApiError::local(0, "E_OUTCOME_UNKNOWN", detail)
        } else {
            ApiError::local(0, "E_NETWORK", detail)
        }
    }
}

#[derive(Default)]
struct Inner {
    gateway: Option<Arc<WsGateway>>,
    up: bool,
    queue: Vec<Pending>,
    in_flight: HashMap<u64, Pending>,
    waiting: HashMap<u64, Pending>,
    next_seq: u64,
    next_id: u64,
    paused: bool,
    pause_timer: Option<AbortHandle>,
    full_timer: Option<AbortHandle>,
    incarnation: Option<String>,
    stopped_with: Option<ApiError>,
    closed: bool,
}

/// The socket request transport of one client. The gateway it writes to is swapped by the sync engine
/// ([`SocketRequests::attach`]), which also feeds it the gateway's lifecycle and every response frame.
#[derive(Default)]
pub struct SocketRequests {
    inner: Mutex<Inner>,
}

fn lock(m: &Mutex<Inner>) -> std::sync::MutexGuard<'_, Inner> {
    m.lock().unwrap_or_else(std::sync::PoisonError::into_inner)
}

fn jitter(max: u64) -> u64 {
    if max == 0 {
        return 0;
    }
    let mut b = [0u8; 8];
    let _ = getrandom::fill(&mut b);
    u64::from_le_bytes(b) % max
}

impl SocketRequests {
    pub fn new() -> Arc<Self> {
        let this = Self::default();
        lock(&this.inner).next_id = 1;
        Arc::new(this)
    }

    /// The gateway requests are written to from now on (`None` while there is none).
    pub fn attach(&self, gateway: Option<Arc<WsGateway>>) {
        let mut inner = lock(&self.inner);
        inner.gateway = gateway;
        inner.up = false;
    }

    /// Whether a request can be written now.
    pub fn up(&self) -> bool {
        let inner = lock(&self.inner);
        !inner.closed && inner.up
    }

    /// One request; `idem` is required for every method but GET.
    pub async fn request(
        self: &Arc<Self>,
        method: &'static str,
        route: String,
        body: Vec<u8>,
        idem: Option<Vec<u8>>,
    ) -> Result<Answer, ApiError> {
        if method != "GET" && idem.as_ref().is_none_or(Vec::is_empty) {
            return Err(ApiError::local(
                0,
                "E_IDEMPOTENCY_REQUIRED",
                "a socket mutation needs an idempotency key",
            ));
        }
        if body.len() > MAX_REQUEST_BODY {
            return Err(ApiError::local(
                0,
                "E_FRAME_LIMIT",
                "the request body is over the frame limit",
            ));
        }
        let (tx, rx) = oneshot::channel();
        {
            let mut inner = lock(&self.inner);
            if inner.closed {
                return Err(ApiError::local(
                    0,
                    "E_NETWORK",
                    "the gateway socket was closed",
                ));
            }
            if let Some(stopped) = &inner.stopped_with {
                return Err(stopped.clone());
            }
            let seq = inner.next_seq;
            inner.next_seq += 1;
            inner.queue.push(Pending {
                seq,
                method,
                route,
                body,
                idem,
                tx: Some(tx),
                unavailable: 0,
                timed_out: 0,
                written: false,
                first_written_at: Instant::now(),
                written_under: None,
                deadline: None,
                timer: None,
            });
        }
        self.pump();
        rx.await
            .unwrap_or_else(|_| Err(ApiError::local(0, "E_NETWORK", "the request was dropped")))
    }

    /// The gateway reached `ready` under `incarnation` (instance id, generation and boot id).
    pub fn on_ready(self: &Arc<Self>, incarnation: String) {
        {
            let mut inner = lock(&self.inner);
            inner.stopped_with = None;
            inner.up = true;
            inner.incarnation = Some(incarnation);
            drop_stale_writes(&mut inner);
        }
        self.pump();
    }

    /// The socket went away and the gateway is reconnecting: what was in flight waits for the next socket.
    pub fn on_down(&self) {
        let mut inner = lock(&self.inner);
        inner.up = false;
        requeue_in_flight(&mut inner);
    }

    /// The gateway stopped for good (revoked, a refused version, stop): every waiting request fails, and new ones fail at
    /// once until the gateway starts again. A written mutation fails `E_OUTCOME_UNKNOWN`: it may have been applied.
    pub fn on_stopped(&self, why: ApiError) {
        let mut inner = lock(&self.inner);
        inner.up = false;
        inner.stopped_with = Some(why.clone());
        fail_all(&mut inner, Some(&why));
    }

    /// A new gateway is starting (after a stop): requests are taken again.
    pub fn on_starting(&self) {
        let mut inner = lock(&self.inner);
        inner.stopped_with = None;
    }

    /// Ends this transport for good.
    pub fn close(&self) {
        let mut inner = lock(&self.inner);
        if inner.closed {
            return;
        }
        inner.closed = true;
        inner.up = false;
        fail_all(&mut inner, None);
    }

    /// A response frame (op 60).
    pub fn on_response(self: &Arc<Self>, bytes: &[u8]) {
        let Ok(r) = frames::decode_response(bytes) else {
            return;
        };
        {
            let mut inner = lock(&self.inner);
            // An answer for a socket since replaced, a timed-out attempt, or a duplicate.
            let Some(mut p) = inner.in_flight.remove(&r.id) else {
                return;
            };
            if let Some(t) = p.deadline.take() {
                t.abort();
            }
            // Any answer frees room at the instance.
            resume(&mut inner);
            let code = (r.status == 429 || r.status == 503)
                .then(|| ApiError::from_body(r.status, &r.body));
            match code {
                Some(e) if r.status == 429 && e.code == "E_BUSY" => {
                    insert(&mut inner, p);
                    self.pause(&mut inner);
                }
                Some(e)
                    if r.status == 503
                        && e.code == "E_UNAVAILABLE"
                        && p.unavailable + 1 < UNAVAILABLE_ATTEMPTS =>
                {
                    p.unavailable += 1;
                    let backoff = e.retry_after_ms.unwrap_or_else(|| {
                        UNAVAILABLE_CAP_MS.min(UNAVAILABLE_BASE_MS << (p.unavailable - 1).min(16))
                    });
                    let wait = backoff.min(UNAVAILABLE_MAX_WAIT_MS) + jitter(UNAVAILABLE_JITTER_MS);
                    let seq = p.seq;
                    let this = Arc::clone(self);
                    p.timer = Some(
                        tokio::spawn(async move {
                            tokio::time::sleep(Duration::from_millis(wait)).await;
                            this.retry_waiting(seq);
                        })
                        .abort_handle(),
                    );
                    inner.waiting.insert(seq, p);
                }
                _ => p.settle(Ok(Answer {
                    status: r.status,
                    body: r.body,
                })),
            }
        }
        self.pump();
    }

    fn retry_waiting(self: &Arc<Self>, seq: u64) {
        {
            let mut inner = lock(&self.inner);
            let Some(mut p) = inner.waiting.remove(&seq) else {
                return;
            };
            p.timer = None;
            if inner.closed {
                p.settle(Err(ApiError::local(
                    0,
                    "E_NETWORK",
                    "the gateway socket was closed",
                )));
                return;
            }
            insert(&mut inner, p);
        }
        self.pump();
    }

    fn pause(self: &Arc<Self>, inner: &mut Inner) {
        inner.paused = true;
        if let Some(t) = inner.pause_timer.take() {
            t.abort();
        }
        let ms = BUSY_PAUSE_MS + jitter(BUSY_JITTER_MS);
        let this = Arc::clone(self);
        inner.pause_timer = Some(
            tokio::spawn(async move {
                tokio::time::sleep(Duration::from_millis(ms)).await;
                {
                    let mut inner = lock(&this.inner);
                    inner.pause_timer = None;
                    inner.paused = false;
                }
                this.pump();
            })
            .abort_handle(),
        );
    }

    /// Writes what the queue holds while there is room: the one choke point every (re)write passes.
    fn pump(self: &Arc<Self>) {
        let mut inner = lock(&self.inner);
        if inner.closed || !inner.up || inner.paused {
            return;
        }
        let Some(gateway) = inner.gateway.clone() else {
            return;
        };
        while !inner.queue.is_empty() && inner.in_flight.len() < MAX_IN_FLIGHT {
            if let Some(why) = stale_reason(&inner, &inner.queue[0]) {
                let mut p = inner.queue.remove(0);
                p.settle(Err(ApiError::local(0, "E_OUTCOME_UNKNOWN", why)));
                continue;
            }
            let id = inner.next_id;
            let p = &inner.queue[0];
            let frame = frames::encode_request(&RequestFrame {
                id,
                method: p.method.to_owned(),
                route: p.route.clone(),
                body: p.body.clone(),
                idem: p.idem.clone(),
            });
            let frame = match frame {
                Ok(f) => f,
                Err(e) => {
                    let mut p = inner.queue.remove(0);
                    p.settle(Err(ApiError::local(
                        0,
                        e.code(),
                        "the request frame was refused",
                    )));
                    continue;
                }
            };
            match gateway.send_now(frame) {
                Ok(()) => {}
                Err(GatewayError::Full { .. }) => {
                    // The writer is behind: try again shortly, in the same order.
                    if inner.full_timer.is_none() {
                        let this = Arc::clone(self);
                        inner.full_timer = Some(
                            tokio::spawn(async move {
                                tokio::time::sleep(FULL_RETRY).await;
                                lock(&this.inner).full_timer = None;
                                this.pump();
                            })
                            .abort_handle(),
                        );
                    }
                    return;
                }
                Err(GatewayError::TooLarge { .. }) => {
                    let mut p = inner.queue.remove(0);
                    p.settle(Err(ApiError::local(
                        0,
                        "E_FRAME_LIMIT",
                        "the request frame is over the bound",
                    )));
                    continue;
                }
                // Closed (or anything newer): it stays queued for the next ready.
                Err(_) => return,
            }
            inner.next_id += 1;
            let mut p = inner.queue.remove(0);
            if !p.written {
                p.first_written_at = Instant::now();
                p.written_under = inner.incarnation.clone();
            }
            p.written = true;
            let this = Arc::clone(self);
            p.deadline = Some(
                tokio::spawn(async move {
                    tokio::time::sleep(ATTEMPT_TIMEOUT).await;
                    this.on_deadline(id);
                })
                .abort_handle(),
            );
            inner.in_flight.insert(id, p);
        }
    }

    /// The attempt ran out its deadline: its late answer is dropped and the request is written again with the same idem,
    /// or fails once the attempts are spent.
    fn on_deadline(self: &Arc<Self>, id: u64) {
        {
            let mut inner = lock(&self.inner);
            let Some(mut p) = inner.in_flight.remove(&id) else {
                return;
            };
            p.deadline = None;
            p.timed_out += 1;
            if p.timed_out < TIMEOUT_ATTEMPTS {
                insert(&mut inner, p);
            } else {
                let e = p.lost("the request timed out");
                p.settle(Err(e));
            }
        }
        self.pump();
    }
}

fn resume(inner: &mut Inner) {
    inner.paused = false;
    if let Some(t) = inner.pause_timer.take() {
        t.abort();
    }
}

/// Puts a request back by its submission order.
fn insert(inner: &mut Inner, p: Pending) {
    let at = inner
        .queue
        .iter()
        .position(|q| q.seq > p.seq)
        .unwrap_or(inner.queue.len());
    inner.queue.insert(at, p);
}

fn requeue_in_flight(inner: &mut Inner) {
    let lost: Vec<Pending> = inner.in_flight.drain().map(|(_, p)| p).collect();
    for mut p in lost {
        if let Some(t) = p.deadline.take() {
            t.abort();
        }
        insert(inner, p);
    }
    drop_stale_writes(inner);
    resume(inner);
}

/// requests.ts `staleReason`.
fn stale_reason(inner: &Inner, p: &Pending) -> Option<&'static str> {
    if !p.written || p.method == "GET" {
        return None;
    }
    if p.first_written_at.elapsed() >= RESEND_WINDOW {
        return Some("the request was written more than 4 minutes ago and its answer was lost");
    }
    match (&p.written_under, &inner.incarnation) {
        (Some(then), Some(now)) if then != now => {
            Some("the instance restarted after the request was written and its answer was lost")
        }
        _ => None,
    }
}

fn drop_stale_writes(inner: &mut Inner) {
    let mut i = 0;
    while i < inner.queue.len() {
        if let Some(why) = stale_reason(inner, &inner.queue[i]) {
            let mut p = inner.queue.remove(i);
            p.settle(Err(ApiError::local(0, "E_OUTCOME_UNKNOWN", why)));
        } else {
            i += 1;
        }
    }
}

/// requests.ts `failAll`.
fn fail_all(inner: &mut Inner, error: Option<&ApiError>) {
    resume(inner);
    let mut all: Vec<Pending> = std::mem::take(&mut inner.queue);
    all.extend(inner.waiting.drain().map(|(_, p)| p));
    all.extend(inner.in_flight.drain().map(|(_, p)| p));
    for mut p in all {
        let e = match error {
            None => p.lost("the gateway socket was stopped"),
            Some(e) if p.written && p.method != "GET" => ApiError::local(
                0,
                "E_OUTCOME_UNKNOWN",
                &format!("no answer before {}", e.code),
            ),
            Some(e) => e.clone(),
        };
        p.settle(Err(e));
    }
}
