//! The engine's event loop against a scripted gateway server: a slow decrypt on a group's lane must never leave a
//! gateway event untaken (the gateway tears the connection down after `ack_timeout` of that, and reconnects).

use std::sync::atomic::{AtomicU64, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use futures_util::{SinkExt, StreamExt};
use tokio::net::TcpListener;
use tokio_tungstenite::tungstenite::Message;

use super::api::{Api, CoreHandle, InstanceInfo, with_core};
use super::requests::SocketRequests;
use super::{ConnectionEvent, Engine, Membership, SyncHooks};
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
