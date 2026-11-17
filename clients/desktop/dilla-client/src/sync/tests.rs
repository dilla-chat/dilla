//! The engine's event loop against a scripted gateway server: a slow decrypt on a group's lane must never leave a
//! gateway event untaken (the gateway tears the connection down after `ack_timeout` of that, and reconnects).

use std::sync::atomic::{AtomicU64, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use futures_util::{SinkExt, StreamExt};
use tokio::net::TcpListener;
use tokio_tungstenite::tungstenite::Message;

use super::api::{Api, ApiError, CoreHandle, InstanceInfo, with_core};
use super::requests::SocketRequests;
use super::{
    COMMIT_RETRY_MAX, ConnectionEvent, Engine, JoinRetry, MEMBERSHIP_WAIT, Membership, SyncError,
    SyncHooks, UNFROZEN_POLL, epoch_moved, group_info_epoch, join_retry, wait_unfrozen,
};
use crate::auth::AuthToken;
use crate::gateway::frames::{self, Frame, Op, Value};
use crate::gateway::{GatewayOptions, WsGateway};
use crate::http::ReqwestHttp;
use crate::seams::Gateway;

/// How long each live frame's apply blocks a core thread in this test (0 elsewhere).
static APPLY_DELAY_MS: AtomicU64 = AtomicU64::new(0);

/// Called at the start of every frame job: blocking work on the core, as a slow MLS decrypt is.
pub(super) async fn slow_apply(core: &CoreHandle) {
    let ms = APPLY_DELAY_MS.load(Ordering::Acquire);
    if ms > 0 {
        with_core(core, move |_| std::thread::sleep(Duration::from_millis(ms))).await;
    }
}

struct NoHooks {
    ready: AtomicUsize,
    down: AtomicUsize,
}

impl SyncHooks for NoHooks {
    fn group_changed(&self, _: [u8; 16]) {}
    fn membership(&self, _: [u8; 16], _: Membership) {}
    fn trust_changed(&self) {}
    fn connection(&self, event: ConnectionEvent) {
        match event {
            ConnectionEvent::Ready(_) => self.ready.fetch_add(1, Ordering::AcqRel),
            _ => self.down.fetch_add(1, Ordering::AcqRel),
        };
    }
    fn community_changed(&self, _: [u8; 16]) {}
}

fn frame(op: u8, n: u64, group_id: Option<[u8; 16]>, payload: Vec<Value>) -> Message {
    Message::Binary(
        frames::encode(&Frame {
            op,
            n,
            group_id,
            payload,
        })
        .unwrap()
        .into(),
    )
}

fn hello() -> Message {
    frame(
        Op::HELLO,
        0,
        None,
        vec![
            Value::Array(vec![Value::Uint(6)]),
            Value::Array(vec![Value::Uint(1)]),
            Value::Array(vec![Value::Uint(1)]),
            Value::Uint(400),
            Value::Uint(131_584),
            Value::Bytes(vec![1; 16]),
            Value::Uint(1),
            Value::Uint(50),
            Value::Uint(50),
        ],
    )
}

fn ready() -> Message {
    frame(
        Op::READY,
        1,
        None,
        vec![
            Value::Bytes(vec![2; 16]),
            Value::Bytes(vec![3; 16]),
            Value::Uint(1),
            Value::Bytes(vec![4; 32]),
            Value::Uint(6),
            Value::Uint(1),
            Value::Uint(1),
            Value::Uint(10),
            Value::Array(Vec::new()),
            Value::Bytes(vec![5; 16]),
        ],
    )
}

fn message_ct(seq: u64) -> Message {
    frame(
        Op::MESSAGE_CT,
        seq + 1,
        Some([9; 16]),
        vec![
            Value::Uint(seq),
            Value::Uint(1),
            Value::Bytes(vec![7; 16]),
            Value::Bytes(vec![0; 64]),
            Value::Bytes(vec![8; 32]),
            Value::Uint(1_700_000_000),
        ],
    )
}

/// A gateway server that accepts any number of connections; each gets a hello, a ready after its identify, then
/// `frames` message frames at once; heartbeats are acknowledged. Answers how many connections it accepted.
async fn server(frames: u64) -> (std::net::SocketAddr, Arc<AtomicUsize>) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let connections = Arc::new(AtomicUsize::new(0));
    let seen = connections.clone();
    tokio::spawn(async move {
        while let Ok((tcp, _)) = listener.accept().await {
            seen.fetch_add(1, Ordering::AcqRel);
            tokio::spawn(async move {
                #[allow(clippy::result_large_err)] // the callback's type is tungstenite's
                fn callback(
                    _: &tokio_tungstenite::tungstenite::handshake::server::Request,
                    mut resp: tokio_tungstenite::tungstenite::handshake::server::Response,
                ) -> Result<
                    tokio_tungstenite::tungstenite::handshake::server::Response,
                    tokio_tungstenite::tungstenite::handshake::server::ErrorResponse,
                > {
                    resp.headers_mut()
                        .insert("sec-websocket-protocol", "dilla.v1".parse().unwrap());
                    Ok(resp)
                }
                let Ok(ws) = tokio_tungstenite::accept_hdr_async(tcp, callback).await else {
                    return;
                };
                let (mut tx, mut rx) = ws.split();
                if tx.send(hello()).await.is_err() {
                    return;
                }
                while let Some(Ok(msg)) = rx.next().await {
                    let Message::Binary(bytes) = msg else {
                        if matches!(msg, Message::Close(_)) {
                            return;
                        }
                        continue;
                    };
                    let Ok(f) = frames::decode(&bytes) else {
                        return;
                    };
                    let mut out = Vec::new();
                    if f.op == Op::IDENTIFY {
                        out.push(ready());
                        out.extend((1..=frames).map(message_ct));
                    } else if f.op == Op::HEARTBEAT {
                        out.push(frame(Op::HEARTBEAT_ACK, 0, None, vec![Value::Uint(1)]));
                    }
                    for m in out {
                        if tx.send(m).await.is_err() {
                            return;
                        }
                    }
                }
            });
        }
    });
    (addr, connections)
}

fn engine(hooks: Arc<NoHooks>) -> Arc<Engine> {
    let conn = rusqlite::Connection::open_in_memory().unwrap();
    let core: CoreHandle = Arc::new(Mutex::new(
        dilla_core::client::ClientCore::open(Arc::new(Mutex::new(conn))).unwrap(),
    ));
    let auth = AuthToken::new("token");
    let http =
        ReqwestHttp::new(url::Url::parse("http://127.0.0.1:9").unwrap(), auth.clone()).unwrap();
    let api = Arc::new(Api::new(
        http,
        SocketRequests::new(),
        auth,
        core.clone(),
        Arc::new(|_: &str| {}),
    ));
    let instance = InstanceInfo {
        instance_id: [1; 16],
        policy_version: 1,
        external_sender_pub: [0; 32],
    };
    Engine::new(core, api, hooks, instance)
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn slow_decrypt_does_not_reconnect() {
    // Each frame's apply blocks a core thread for 1.5 s, longer than the 1 s the gateway gives an untaken event (in a
    // channel of 4) before it tears the connection down: 12 frames are 18 s of work on the group's lane.
    APPLY_DELAY_MS.store(1500, Ordering::Release);
    let (addr, connections) = server(12).await;
    let hooks = Arc::new(NoHooks {
        ready: AtomicUsize::new(0),
        down: AtomicUsize::new(0),
    });
    let engine = engine(hooks.clone());
    let options = GatewayOptions {
        heartbeat_max: Duration::from_millis(200),
        ack_timeout: Duration::from_millis(1000),
        hello_timeout: Duration::from_secs(5),
        event_capacity: 4,
        extra_roots: Vec::new(),
    };
    let gateway = Arc::new(WsGateway::connect_with(
        url::Url::parse(&format!("http://{addr}")).unwrap(),
        AuthToken::new("token"),
        options,
    ));
    let events = gateway.events();
    engine.set_gateway(Some(gateway.clone()));
    tokio::spawn(engine.clone().run_events(gateway.clone(), events));
    tokio::time::sleep(Duration::from_secs(4)).await;
    APPLY_DELAY_MS.store(0, Ordering::Release);
    assert_eq!(hooks.ready.load(Ordering::Acquire), 1, "one ready");
    assert_eq!(
        hooks.down.load(Ordering::Acquire),
        0,
        "the connection was never torn down"
    );
    assert_eq!(connections.load(Ordering::Acquire), 1, "no reconnect");
    engine.stop();
    gateway.stop();
}

/// Wave 2 H (recovery re-review): a removal feed refused for an unjudged moderator reads the leaves again (controller.ts
/// `onReady`'s `fetchedLeaves.clear()`), and keeps what earlier passes found: a contact not on their list stays so (no
/// Compare, the warning shown) until a pass reads them again.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_removal_retry_keeps_the_trust_annotations() {
    let hooks = Arc::new(NoHooks {
        ready: AtomicUsize::new(0),
        down: AtomicUsize::new(0),
    });
    let engine = engine(hooks);
    let user = [7u8; 16];
    engine.trust.lock().await.mark_unlisted_for_test(user);
    let before = super::trust::annotations_of(&engine).await;
    assert!(before.unlisted.contains(&user));
    engine.forget_before_removal_retry().await;
    assert_eq!(
        super::trust::annotations_of(&engine).await,
        before,
        "the annotations are kept"
    );
    engine.stop();
}

// ---- an opened channel's external join refused 425 E_COMMIT_REQUIRED or 422 E_COMMIT_INVALID ----

fn refused(status: u16, code: &str, retry_after_ms: Option<u64>) -> SyncError {
    let mut e = ApiError::local(status, code, "");
    e.retry_after_ms = retry_after_ms;
    SyncError::Api(e)
}

/// 422 `E_COMMIT_INVALID` naming the clause that refused the commit (its fourth field).
fn invalid(rule: &str) -> SyncError {
    let mut e = dilla_core::cbor::Encoder::new();
    e.array(4)
        .text("E_COMMIT_INVALID")
        .text("refused")
        .null()
        .text(rule);
    SyncError::Api(ApiError::from_body(422, &e.into_vec()))
}

/// A freeze waits for a member's commit, at most the instance's hint (its 2 s commit deadline), never longer and never
/// less than one poll; a stale epoch (422) is retried only once the group is known to have moved; a conflict (409) is
/// retried at once; everything else, and every refusal once the tries are spent, is the open's answer.
#[test]
fn an_open_retries_a_freeze_a_conflict_and_a_moved_epoch_within_its_budget() {
    let two = Duration::from_millis(2_000);
    assert_eq!(MEMBERSHIP_WAIT, two);
    assert_eq!(
        join_retry(&refused(425, "E_COMMIT_REQUIRED", Some(2_000)), 1),
        JoinRetry::Unfrozen(two)
    );
    assert_eq!(
        join_retry(&refused(425, "E_COMMIT_REQUIRED", Some(800)), 1),
        JoinRetry::Unfrozen(Duration::from_millis(800))
    );
    assert_eq!(
        join_retry(&refused(425, "E_COMMIT_REQUIRED", Some(60_000)), 1),
        JoinRetry::Unfrozen(two),
        "capped at the commit deadline"
    );
    assert_eq!(
        join_retry(&refused(425, "E_COMMIT_REQUIRED", None), 1),
        JoinRetry::Unfrozen(two)
    );
    assert_eq!(
        join_retry(&refused(425, "E_COMMIT_REQUIRED", Some(0)), 1),
        JoinRetry::Unfrozen(UNFROZEN_POLL)
    );
    assert_eq!(
        join_retry(&refused(422, "E_COMMIT_INVALID", None), 1),
        JoinRetry::IfMoved
    );
    for rule in ["epoch", "structural", "external_joiner"] {
        assert_eq!(join_retry(&invalid(rule), 1), JoinRetry::IfMoved, "{rule}");
    }
    // The freeze met inside the commit (a proposal made, or a member online, after the instance's freeze check).
    assert_eq!(
        join_retry(&invalid("outstanding_proposals"), 1),
        JoinRetry::Unfrozen(two)
    );
    assert_eq!(
        join_retry(&invalid("outstanding_proposals"), COMMIT_RETRY_MAX),
        JoinRetry::Fail
    );
    assert_eq!(
        join_retry(&refused(409, "E_COMMIT_CONFLICT", None), 1),
        JoinRetry::Now
    );
    for other in [
        refused(422, "E_INVALID_REQUEST", None),
        refused(425, "E_TOO_EARLY", None),
        refused(403, "E_FORBIDDEN", None),
        refused(409, "E_REMOVE_PENDING", None),
        refused(409, "E_GROUP_CLOSED", None),
        SyncError::Stopped,
        SyncError::ChannelGone,
    ] {
        assert_eq!(join_retry(&other, 1), JoinRetry::Fail, "{other}");
    }
    for spent in [
        refused(425, "E_COMMIT_REQUIRED", Some(2_000)),
        refused(422, "E_COMMIT_INVALID", None),
        refused(409, "E_COMMIT_CONFLICT", None),
    ] {
        assert_eq!(
            join_retry(&spent, COMMIT_RETRY_MAX),
            JoinRetry::Fail,
            "{spent}: the tries are spent, the refusal is surfaced"
        );
        assert_ne!(join_retry(&spent, COMMIT_RETRY_MAX - 1), JoinRetry::Fail);
    }
}

/// A 422 is retried only when the group is known to be past the epoch the refused commit was staged on: an unreadable
/// epoch, or one that did not move, leaves the refusal as the answer.
#[test]
fn a_stale_epoch_is_retried_only_when_the_group_moved() {
    assert!(epoch_moved(Some(3), Some(4)));
    assert!(!epoch_moved(Some(3), Some(3)));
    assert!(!epoch_moved(Some(3), None));
    assert!(!epoch_moved(None, Some(4)));
    assert!(!epoch_moved(None, None));
}

/// `GET /v1/groups/{id}/info` answers `[epoch, group_info, tree_hash, next_seq]`.
#[test]
fn the_group_info_epoch_is_its_first_field() {
    let mut e = dilla_core::cbor::Encoder::new();
    e.array(4).uint(7).bytes(b"info").bytes(&[0; 32]).uint(12);
    assert_eq!(group_info_epoch(&e.into_vec()), Some(7));
    assert_eq!(group_info_epoch(b"\xff"), None);
    let mut e = dilla_core::cbor::Encoder::new();
    e.array(0);
    assert_eq!(group_info_epoch(&e.into_vec()), None);
}

/// The epoch reads of a frozen-group wait: `answers` in turn (then the last one again), counted.
fn epochs(
    answers: Vec<Option<u64>>,
) -> (
    Arc<AtomicUsize>,
    impl FnMut() -> std::future::Ready<Option<u64>>,
) {
    let reads = Arc::new(AtomicUsize::new(0));
    let counted = reads.clone();
    let read = move || {
        let n = counted.fetch_add(1, Ordering::AcqRel);
        std::future::ready(answers[n.min(answers.len() - 1)])
    };
    (reads, read)
}

/// The freeze ends with a member's commit, usually well before the instance's deadline: the wait reads the epoch every
/// 100 ms and ends at the first read past the staged one (a failed read does not end it), not after the whole hint.
#[tokio::test(start_paused = true)]
async fn a_frozen_wait_ends_when_the_epoch_moves() {
    let (reads, read) = epochs(vec![Some(5), None, Some(5), Some(6)]);
    let start = tokio::time::Instant::now();
    wait_unfrozen(read, Some(5), MEMBERSHIP_WAIT, || false).await;
    assert_eq!(start.elapsed(), UNFROZEN_POLL * 4);
    assert_eq!(reads.load(Ordering::Acquire), 4);
}

/// A freeze nobody lifts is bounded by the hint, one read per poll; then the open stages again (and the next refusal,
/// or the spent budget, answers).
#[tokio::test(start_paused = true)]
async fn a_frozen_wait_is_bounded_by_the_hint() {
    let (reads, read) = epochs(vec![Some(5)]);
    let start = tokio::time::Instant::now();
    wait_unfrozen(read, Some(5), MEMBERSHIP_WAIT, || false).await;
    assert_eq!(start.elapsed(), MEMBERSHIP_WAIT);
    assert_eq!(reads.load(Ordering::Acquire), 20);
    // A hint that is not a whole number of polls ends exactly at the hint.
    let (reads, read) = epochs(vec![Some(5)]);
    let start = tokio::time::Instant::now();
    wait_unfrozen(read, Some(5), Duration::from_millis(250), || false).await;
    assert_eq!(start.elapsed(), Duration::from_millis(250));
    assert_eq!(reads.load(Ordering::Acquire), 3);
}

/// Without a staged epoch there is nothing to compare: the wait is the hint, with no reads. A stopped engine does not
/// wait.
#[tokio::test(start_paused = true)]
async fn a_frozen_wait_without_a_staged_epoch_sleeps_the_hint_and_a_stop_ends_it() {
    let (reads, read) = epochs(vec![Some(6)]);
    let start = tokio::time::Instant::now();
    wait_unfrozen(read, None, MEMBERSHIP_WAIT, || false).await;
    assert_eq!(start.elapsed(), MEMBERSHIP_WAIT);
    assert_eq!(reads.load(Ordering::Acquire), 0);
    let (reads, read) = epochs(vec![Some(5)]);
    let start = tokio::time::Instant::now();
    wait_unfrozen(read, Some(5), MEMBERSHIP_WAIT, || true).await;
    assert_eq!(start.elapsed(), Duration::ZERO);
    assert_eq!(reads.load(Ordering::Acquire), 0);
}
