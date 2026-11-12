//! The gateway client against `protocol/vectors/frames.json`, a scripted in-process server and a real dillad.
//!
//! The real-dillad test needs `target/dilla-testhost` (`go build -o target/dilla-testhost ./cmd/dilla-testhost`) and
//! `target/wasm32-wasip1/release/dilla_core_wasi.wasm` (`cargo build -p dilla-core-wasi --target wasm32-wasip1
//! --release`), or `DILLA_TESTHOST` and `DILLA_CORE_WASI` naming them. CI's desktop-gpui job has no Go toolchain, so it
//! is `#[ignore]`d there; run it with `cargo test -p dilla-client --test gateway -- --ignored`.

use std::net::SocketAddr;
use std::path::PathBuf;
use std::sync::Arc;
use std::time::{Duration, Instant};

use dilla_client::gateway::frames::{self, Frame, FrameError, Op, Value};
use dilla_client::gateway::{AuthToken, GatewayOptions, ReadyInfo, WsGateway};
use dilla_client::seams::{DisconnectReason, Gateway, GatewayError, GatewayEvent};
use futures_util::{SinkExt, StreamExt};
use tokio::net::TcpListener;
use tokio::sync::mpsc;
use tokio_tungstenite::tungstenite::Message;
use tokio_tungstenite::tungstenite::handshake::server::{Request, Response};
use tokio_tungstenite::tungstenite::http::HeaderValue;

fn repo_root() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../..")
}

fn unhex(s: &str) -> Vec<u8> {
    assert!(s.len().is_multiple_of(2), "odd hex {s}");
    (0..s.len())
        .step_by(2)
        .map(|i| u8::from_str_radix(&s[i..i + 2], 16).expect("hex"))
        .collect()
}

fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

/// What this client does with each of the server-side reject vectors: the TS client's verdicts
/// (`packages/client-core/src/gateway/frames.test.ts` CLIENT_VERDICT), an error code or `None` for "accepted".
const CLIENT_VERDICT: &[(&str, Option<&str>)] = &[
    ("three elements", Some("E_FRAME_SHAPE")),
    ("five elements", Some("E_FRAME_SHAPE")),
    ("unknown opcode", None),
    ("server opcode from a client", Some("E_FRAME_SHAPE")),
    ("payload is not an array", Some("E_FRAME_SHAPE")),
    (
        "subscribe with the wrong element count",
        Some("E_FRAME_SHAPE"),
    ),
    ("group_id on a control frame", Some("E_FRAME_SHAPE")),
    ("group_id of the wrong length", Some("E_FRAME_SHAPE")),
    ("commit_ack with no group_id", Some("E_FRAME_SHAPE")),
    ("non-minimal op", Some("E_CBOR_NONCANONICAL")),
    ("map instead of an array", Some("E_CBOR_TYPE")),
    ("old typing has four elements", Some("E_FRAME_SHAPE")),
    ("typing_set channel id has 17 bytes", Some("E_FRAME_SHAPE")),
    ("voice_self flags 4", Some("E_FRAME_SHAPE")),
    ("typing_set has group_id", Some("E_FRAME_SHAPE")),
    ("call_ring state 3", Some("E_FRAME_SHAPE")),
    ("call_ring with 4 elements", Some("E_FRAME_SHAPE")),
    ("community_changed with 2 elements", Some("E_FRAME_SHAPE")),
    ("message.ct with 8 elements", Some("E_FRAME_SHAPE")),
    ("afk_away with 4 elements", Some("E_FRAME_SHAPE")),
    ("request with a text body", Some("E_FRAME_SHAPE")),
    ("response with status 600", Some("E_FRAME_SHAPE")),
];

#[test]
fn vectors_round_trip() {
    let raw = std::fs::read(repo_root().join("protocol/vectors/frames.json")).expect("frames.json");
    let v: serde_json::Value = serde_json::from_slice(&raw).expect("json");
    let cases = v["cases"].as_array().expect("cases");
    let rejects = v["rejects"].as_array().expect("rejects");
    assert_eq!(cases.len(), 44, "the corpus this codec was written against");
    assert_eq!(rejects.len(), 22);

    for c in cases {
        let name = c["name"].as_str().unwrap();
        let bytes = unhex(c["frame"].as_str().unwrap());
        let gid = c["group_id"].as_str().unwrap();

        // Decode: op, n, group id and the payload's own encoding are the vector's.
        let frame = frames::decode(&bytes).unwrap_or_else(|e| panic!("{name}: decode: {e}"));
        assert_eq!(u64::from(frame.op), c["op"].as_u64().unwrap(), "{name}: op");
        assert_eq!(frame.n, c["n"].as_u64().unwrap(), "{name}: n");
        assert_eq!(
            frame.group_id.map(|g| hex(&g)).unwrap_or_default(),
            gid,
            "{name}: group id"
        );
        let payload = Value::Array(frame.payload.clone()).encode();
        assert_eq!(
            hex(&payload),
            c["payload"].as_str().unwrap(),
            "{name}: payload"
        );

        // Re-encode the decoded frame: byte-identical.
        assert_eq!(
            hex(&frames::encode(&frame).unwrap()),
            c["frame"].as_str().unwrap(),
            "{name}: re-encode"
        );

        // Encode from the vector's own parts: byte-identical too.
        let Value::Array(parts) = Value::decode(&unhex(c["payload"].as_str().unwrap())).unwrap()
        else {
            panic!("{name}: payload is not an array");
        };
        let built = Frame {
            op: u8::try_from(c["op"].as_u64().unwrap()).unwrap(),
            n: c["n"].as_u64().unwrap(),
            group_id: (!gid.is_empty()).then(|| unhex(gid).try_into().unwrap()),
            payload: parts,
        };
        assert_eq!(
            hex(&frames::encode(&built).unwrap()),
            c["frame"].as_str().unwrap(),
            "{name}: encode"
        );
    }

    let mut names: Vec<&str> = rejects
        .iter()
        .map(|r| r["name"].as_str().unwrap())
        .collect();
    let mut known: Vec<&str> = CLIENT_VERDICT.iter().map(|(n, _)| *n).collect();
    names.sort_unstable();
    known.sort_unstable();
    assert_eq!(names, known, "every reject has a client verdict");
    for r in rejects {
        let name = r["name"].as_str().unwrap();
        let want = CLIENT_VERDICT.iter().find(|(n, _)| *n == name).unwrap().1;
        let got = frames::decode(&unhex(r["frame"].as_str().unwrap()))
            .err()
            .map(|e| e.code());
        assert_eq!(got, want, "{name}");
    }
}

/// The versions the hello negotiation offers are gateway.ts's own, read from its source so the two cannot drift.
#[test]
fn negotiation_preferences_are_gateway_ts() {
    let ts =
        std::fs::read_to_string(repo_root().join("packages/client-core/src/gateway/gateway.ts"))
            .unwrap();
    assert!(
        ts.contains("const WIRE_PREFERENCE = [6n] as const;"),
        "gateway.ts's wire preference moved"
    );
    assert!(ts.contains("const e2ee = highestCommon(versions[1], [1n]);"));
    assert!(ts.contains("const media = highestCommon(versions[2], [1n]);"));
    assert!(
        ts.contains("this.send(encodeFrame(Op.identify, 1, null, ['', wire, e2ee, media, 0]));")
    );
    assert_eq!(dilla_client::gateway::WIRE_PREFERENCE, &[6]);
    assert_eq!(dilla_client::gateway::E2EE_PREFERENCE, &[1]);
    assert_eq!(dilla_client::gateway::MEDIA_PREFERENCE, &[1]);
    for line in [
        "heartbeatMaxMs: 15000, ackTimeoutMs: 10000, helloTimeoutMs: 15000,",
        "reconnectBaseMs: 500, reconnectCapMs: 30000, reconnectJitterMs: 500,",
        "const DEFAULT_MAX_FRAME_BYTES = 131584;",
    ] {
        assert!(ts.contains(line), "gateway.ts no longer has {line:?}");
    }
    let o = GatewayOptions::default();
    assert_eq!(o.heartbeat_max, Duration::from_millis(15_000));
    assert_eq!(o.ack_timeout, Duration::from_millis(10_000));
    assert_eq!(o.hello_timeout, Duration::from_millis(15_000));
    assert_eq!(dilla_client::gateway::reconnect::BASE_MS, 500);
    assert_eq!(dilla_client::gateway::reconnect::CAP_MS, 30_000);
    assert_eq!(dilla_client::gateway::reconnect::JITTER_MS, 500);
    assert_eq!(dilla_client::gateway::DEFAULT_MAX_FRAME_BYTES, 131_584);
}

// ---------------------------------------------------------------------------------------------------------------
// A scripted gateway: hello, then ready on identify, then whatever the script says.

#[derive(Clone, Copy)]
struct Script {
    heartbeat_ms: u64,
    max_frame_bytes: u64,
    /// Answer heartbeats on the first connection.
    ack_first: bool,
    /// After `ready`, push this many presence frames (op 48).
    push: usize,
    /// After `ready` (and the push), stop reading the first connection altogether: a peer whose receive window fills.
    deaf_first: bool,
}

impl Default for Script {
    fn default() -> Self {
        Self {
            heartbeat_ms: 30_000,
            max_frame_bytes: 131_584,
            ack_first: true,
            push: 0,
            deaf_first: false,
        }
    }
}

/// What the scripted server saw: one entry per accepted connection, then every frame read on it.
#[derive(Debug)]
enum Seen {
    Upgrade {
        protocol: Option<String>,
        authorization: Option<String>,
    },
    Frame(Vec<u8>),
    /// The server stopped reading.
    Deaf,
    Closed,
}

fn hello_frame(s: Script) -> Vec<u8> {
    frames::encode(&Frame {
        op: Op::HELLO,
        n: 0,
        group_id: None,
        payload: vec![
            Value::Array(vec![Value::Uint(6)]),
            Value::Array(vec![Value::Uint(1)]),
            Value::Array(vec![Value::Uint(1)]),
            Value::Uint(s.heartbeat_ms),
            Value::Uint(s.max_frame_bytes),
            Value::Bytes(vec![1; 16]),
            Value::Uint(1),
            Value::Uint(300),
            Value::Uint(300),
        ],
    })
    .unwrap()
}

fn ready_frame(n: u64) -> Vec<u8> {
    frames::encode(&Frame {
        op: Op::READY,
        n,
        group_id: None,
        payload: vec![
            Value::Bytes(vec![8; 16]),
            Value::Bytes(vec![9; 16]),
            Value::Uint(1),
            Value::Bytes(vec![11; 32]),
            Value::Uint(6),
            Value::Uint(1),
            Value::Uint(1),
            Value::Uint(40),
            Value::Array(vec![]),
            Value::Bytes(vec![12; 16]),
        ],
    })
    .unwrap()
}

fn presence_frame() -> Vec<u8> {
    frames::encode(&Frame {
        op: Op::PRESENCE,
        n: 0,
        group_id: None,
        payload: vec![
            Value::Bytes(vec![9; 16]),
            Value::Bytes(vec![8; 16]),
            Value::Uint(1),
            Value::Uint(2),
        ],
    })
    .unwrap()
}

// The upgrade callback's error type is tungstenite's (an HTTP response), not ours.
#[allow(clippy::result_large_err)]
async fn serve_ws<S>(stream: S, script: Script, first: bool, tx: mpsc::UnboundedSender<Seen>)
where
    S: tokio::io::AsyncRead + tokio::io::AsyncWrite + Unpin,
{
    let ack = !first || script.ack_first;
    let up = tx.clone();
    let ws =
        tokio_tungstenite::accept_hdr_async(stream, move |req: &Request, mut resp: Response| {
            let header = |name: &str| {
                req.headers()
                    .get(name)
                    .and_then(|v| v.to_str().ok())
                    .map(str::to_owned)
            };
            let _ = up.send(Seen::Upgrade {
                protocol: header("sec-websocket-protocol"),
                authorization: header("authorization"),
            });
            resp.headers_mut().insert(
                "sec-websocket-protocol",
                HeaderValue::from_static("dilla.v1"),
            );
            Ok(resp)
        })
        .await;
    let Ok(mut ws) = ws else { return };
    ws.send(Message::Binary(hello_frame(script).into()))
        .await
        .unwrap();
    while let Some(Ok(msg)) = ws.next().await {
        let Message::Binary(bytes) = msg else {
            continue;
        };
        let _ = tx.send(Seen::Frame(bytes.to_vec()));
        let Ok(f) = frames::decode(&bytes) else {
            continue;
        };
        if f.op == Op::IDENTIFY {
            let _ = ws.send(Message::Binary(ready_frame(1).into())).await;
            for _ in 0..script.push {
                let _ = ws.send(Message::Binary(presence_frame().into())).await;
            }
            if first && script.deaf_first {
                // Never read again: the client's writes fill the receive window and block.
                let _ = tx.send(Seen::Deaf);
                std::future::pending::<()>().await;
            }
        } else if f.op == Op::HEARTBEAT && ack {
            let ack = frames::encode(&Frame {
                op: Op::HEARTBEAT_ACK,
                n: 0,
                group_id: None,
                payload: vec![Value::Uint(1)],
            })
            .unwrap();
            let _ = ws.send(Message::Binary(ack.into())).await;
        }
        // Without `ack`, every heartbeat is read and dropped: a server that stops answering.
    }
    let _ = tx.send(Seen::Closed);
}

async fn scripted_server(script: Script) -> (SocketAddr, mpsc::UnboundedReceiver<Seen>) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let (tx, rx) = mpsc::unbounded_channel();
    tokio::spawn(async move {
        let mut first = true;
        while let Ok((stream, _)) = listener.accept().await {
            tokio::spawn(serve_ws(stream, script, first, tx.clone()));
            first = false;
        }
    });
    (addr, rx)
}

async fn next_event(rx: &mut mpsc::Receiver<GatewayEvent>, within: Duration) -> GatewayEvent {
    tokio::time::timeout(within, rx.recv())
        .await
        .expect("an event in time")
        .expect("the gateway is running")
}

#[tokio::test]
async fn heartbeat_timeout_reconnects() {
    let script = Script {
        heartbeat_ms: 200,
        max_frame_bytes: 131_584,
        ack_first: false,
        ..Script::default()
    };
    let (addr, mut seen) = scripted_server(script).await;
    let base = url::Url::parse(&format!("http://{addr}")).unwrap();
    // Only the ack timeout is shortened (10 s in gateway.ts); the heartbeat interval is the hello's, and the backoff is
    // gateway.ts's own.
    let options = GatewayOptions {
        ack_timeout: Duration::from_millis(300),
        ..GatewayOptions::default()
    };
    let gw = WsGateway::connect_with(base, AuthToken::new("tok-1".into()), options);
    let mut events = gw.events();

    let GatewayEvent::Connected { session } = next_event(&mut events, Duration::from_secs(5)).await
    else {
        panic!("expected Connected first");
    };
    let ready = ReadyInfo::from_frame(&session).unwrap();
    assert_eq!((ready.wire, ready.e2ee, ready.media), (6, 1, 1));

    // The upgrade offered dilla.v1 with the bearer, and identify came first with the negotiated versions.
    let Some(Seen::Upgrade {
        protocol,
        authorization,
    }) = seen.recv().await
    else {
        panic!("upgrade")
    };
    assert_eq!(protocol.as_deref(), Some("dilla.v1"));
    assert_eq!(authorization.as_deref(), Some("Bearer tok-1"));
    let Some(Seen::Frame(identify)) = seen.recv().await else {
        panic!("identify")
    };
    assert_eq!(
        hex(&identify),
        "840101f6856006010100",
        "identify ['', 6, 1, 1, 0], n = 1"
    );

    let t0 = Instant::now();
    let GatewayEvent::Disconnected { reason } =
        next_event(&mut events, Duration::from_secs(5)).await
    else {
        panic!("expected Disconnected after the unanswered heartbeat");
    };
    assert_eq!(reason, DisconnectReason::Network);
    // heartbeat every min(200 / 2, 15000) = 100 ms, the first unanswered one times out after 300 ms.
    let down = t0.elapsed();
    assert!(down < Duration::from_secs(2), "took {down:?} to notice");

    let t1 = Instant::now();
    let GatewayEvent::Connected { .. } = next_event(&mut events, Duration::from_secs(5)).await
    else {
        panic!("expected Connected again");
    };
    // gateway.ts after a ready: min(30000, 500 * 2^0) + random * 500, so [500, 1000) ms, plus the handshake.
    let back = t1.elapsed();
    assert!(
        back >= Duration::from_millis(500),
        "reconnected after {back:?}, before the backoff floor"
    );
    assert!(
        back < Duration::from_millis(1_800),
        "reconnected after {back:?}"
    );

    // The heartbeats carried [last_n, 0] with n = 0: last_n is the ready's n (1).
    let mut beats = 0;
    while let Ok(Some(s)) = tokio::time::timeout(Duration::from_millis(50), seen.recv()).await {
        if let Seen::Frame(b) = s
            && b[1] == Op::HEARTBEAT
        {
            assert_eq!(hex(&b), "840600f6820100");
            beats += 1;
        }
    }
    assert!(beats >= 1, "at least one heartbeat was written");
    gw.stop();
}

#[tokio::test]
async fn oversized_frame_refused() {
    // The bound must still admit the ready frame (106 bytes here): a larger read closes the socket 4109.
    let script = Script {
        heartbeat_ms: 30_000,
        max_frame_bytes: 128,
        ack_first: true,
        ..Script::default()
    };
    let (addr, mut seen) = scripted_server(script).await;
    let base = url::Url::parse(&format!("http://{addr}")).unwrap();
    let gw = WsGateway::connect(base, AuthToken::new("tok-2".into()));
    let mut events = gw.events();
    let GatewayEvent::Connected { .. } = next_event(&mut events, Duration::from_secs(5)).await
    else {
        panic!("expected Connected");
    };

    let filler = |pad: usize| {
        frames::encode(&Frame {
            op: Op::PRESENCE_ACTIVITY,
            n: 0,
            group_id: None,
            payload: vec![Value::Bytes(vec![0; pad])],
        })
        .unwrap()
    };
    let at_limit = filler(128 - 8);
    assert_eq!(at_limit.len(), 128);
    let over = filler(128 - 7);
    assert_eq!(over.len(), 129);

    match gw.send(over).await {
        Err(GatewayError::TooLarge { len: 129, max: 128 }) => {}
        other => panic!("expected TooLarge, got {other:?}"),
    }
    gw.send(at_limit.clone())
        .await
        .expect("a frame at the limit is sent");

    // The server saw the upgrade, identify, then the frame at the limit, and never the oversized one.
    let mut frames_seen = Vec::new();
    while let Ok(Some(s)) = tokio::time::timeout(Duration::from_millis(300), seen.recv()).await {
        if let Seen::Frame(b) = s {
            frames_seen.push(b);
        }
    }
    assert_eq!(
        frames_seen.len(),
        2,
        "identify and the frame at the limit: {frames_seen:?}"
    );
    assert_eq!(frames_seen[1], at_limit);

    // A request frame (op 59) is bounded by the request body cap, not the hello's bound.
    let request = frames::encode_request(&frames::RequestFrame {
        id: 1,
        method: "POST".into(),
        route: "/v1/communities".into(),
        body: vec![0x80; 200],
        idem: None,
    })
    .unwrap();
    gw.send(request)
        .await
        .expect("a request frame over the hello bound is sent");
    let body_over = frames::encode_request(&frames::RequestFrame {
        id: 2,
        method: "POST".into(),
        route: "/v1/x".into(),
        body: vec![0; frames::MAX_REQUEST_BODY + 1],
        idem: None,
    });
    assert!(matches!(body_over, Err(FrameError::Limit)));

    gw.stop();
    let GatewayEvent::Disconnected { reason } =
        next_event(&mut events, Duration::from_secs(5)).await
    else {
        panic!("expected Disconnected on stop");
    };
    assert_eq!(reason, DisconnectReason::Shutdown);
    assert!(matches!(
        gw.send(vec![0x84]).await,
        Err(GatewayError::Closed)
    ));
}

/// Waits for the first `Seen` the predicate accepts.
async fn wait_seen(
    seen: &mut mpsc::UnboundedReceiver<Seen>,
    within: Duration,
    want: impl Fn(&Seen) -> bool,
) {
    tokio::time::timeout(within, async {
        while let Some(s) = seen.recv().await {
            if want(&s) {
                return;
            }
        }
        panic!("the scripted server ended");
    })
    .await
    .expect("the scripted server saw it in time");
}

fn heartbeats_seen(seen: &mut mpsc::UnboundedReceiver<Seen>) -> usize {
    let mut beats = 0;
    while let Ok(s) = seen.try_recv() {
        if let Seen::Frame(b) = s
            && b.get(1) == Some(&Op::HEARTBEAT)
        {
            beats += 1;
        }
    }
    beats
}

/// A peer that stops reading while a large write is in flight: the write blocks, the heartbeat behind it is never
/// answered, and the ack timeout still fires, because no write is awaited on the connection's own loop.
#[tokio::test]
async fn blocked_write_still_times_out() {
    let script = Script {
        heartbeat_ms: 200,
        max_frame_bytes: 4 << 20,
        ack_first: false,
        deaf_first: true,
        ..Script::default()
    };
    let (addr, mut seen) = scripted_server(script).await;
    let base = url::Url::parse(&format!("http://{addr}")).unwrap();
    let ack_timeout = Duration::from_millis(500);
    let options = GatewayOptions {
        ack_timeout,
        ..GatewayOptions::default()
    };
    let gw = WsGateway::connect_with(base, AuthToken::new("tok-deaf".into()), options);
    let mut events = gw.events();
    let GatewayEvent::Connected { .. } = next_event(&mut events, Duration::from_secs(5)).await
    else {
        panic!("expected Connected");
    };
    wait_seen(&mut seen, Duration::from_secs(5), |s| {
        matches!(s, Seen::Deaf)
    })
    .await;

    // 32 MiB of frames, far past the socket buffers of a peer that does not read.
    let big = frames::encode(&Frame {
        op: 99,
        n: 0,
        group_id: None,
        payload: vec![Value::Bytes(vec![0; (4 << 20) - 16])],
    })
    .unwrap();
    let t0 = Instant::now();
    for _ in 0..8 {
        let sent = tokio::time::timeout(Duration::from_secs(1), gw.send(big.clone())).await;
        assert!(sent.is_ok(), "send() must not wait on the socket");
    }
    let GatewayEvent::Disconnected { reason } =
        next_event(&mut events, Duration::from_secs(5)).await
    else {
        panic!("expected Disconnected");
    };
    assert_eq!(reason, DisconnectReason::Network);
    // One heartbeat interval (100 ms) plus the ack timeout, plus margin.
    let took = t0.elapsed();
    assert!(
        took < ack_timeout + Duration::from_millis(1_500),
        "noticed after {took:?}"
    );
    gw.stop();
}

/// A consumer that does not drain a full event channel: heartbeats go on while the gateway waits, and a consumer more
/// than the ack timeout behind gets the documented teardown (`Disconnected { Network }`, then a reconnect).
#[tokio::test]
async fn full_event_channel_keeps_heartbeats() {
    let script = Script {
        heartbeat_ms: 100,
        push: 50,
        ..Script::default()
    };
    let (addr, mut seen) = scripted_server(script).await;
    let base = url::Url::parse(&format!("http://{addr}")).unwrap();
    let options = GatewayOptions {
        ack_timeout: Duration::from_millis(1_000),
        event_capacity: 2,
        ..GatewayOptions::default()
    };
    let gw = WsGateway::connect_with(base, AuthToken::new("tok-slow".into()), options);
    let mut events = gw.events();

    tokio::time::sleep(Duration::from_millis(700)).await;
    // A beat every 50 ms (heartbeat_ms / 2) while the channel is full.
    let beats = heartbeats_seen(&mut seen);
    assert!(
        beats >= 5,
        "only {beats} heartbeats while the event channel was full"
    );

    tokio::time::sleep(Duration::from_millis(800)).await;
    let mut log = Vec::new();
    let found = tokio::time::timeout(Duration::from_secs(5), async {
        while let Some(e) = events.recv().await {
            let done = matches!(
                e,
                GatewayEvent::Disconnected {
                    reason: DisconnectReason::Network
                }
            );
            log.push(e);
            if done {
                return;
            }
        }
    })
    .await;
    assert!(
        found.is_ok(),
        "no Disconnected{{Network}} after the stall: {log:?}"
    );
    assert!(
        matches!(log.first(), Some(GatewayEvent::Connected { .. })),
        "{log:?}"
    );
    gw.stop();
}

/// `stop()` is honoured while the gateway waits on a consumer that never drains its events.
#[tokio::test]
async fn stop_is_prompt_with_a_full_event_channel() {
    let script = Script {
        push: 20,
        ..Script::default()
    };
    let (addr, mut seen) = scripted_server(script).await;
    let base = url::Url::parse(&format!("http://{addr}")).unwrap();
    let options = GatewayOptions {
        event_capacity: 1,
        ..GatewayOptions::default()
    };
    let gw = WsGateway::connect_with(base, AuthToken::new("tok-stuck".into()), options);
    let _events = gw.events();
    wait_seen(
        &mut seen,
        Duration::from_secs(5),
        |s| matches!(s, Seen::Frame(b) if b.get(1) == Some(&Op::IDENTIFY)),
    )
    .await;
    tokio::time::sleep(Duration::from_millis(300)).await;

    gw.stop();
    wait_seen(&mut seen, Duration::from_secs(2), |s| {
        matches!(s, Seen::Closed)
    })
    .await;
    let t0 = Instant::now();
    while !matches!(gw.send(vec![0x84]).await, Err(GatewayError::Closed)) {
        assert!(
            t0.elapsed() < Duration::from_secs(1),
            "the driver did not end"
        );
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
}

/// wss end to end against a local TLS server, through the gateway's own rustls configuration (its explicit aws-lc-rs
/// provider, never the process default). `tests/fixtures/tls/` holds a throwaway test CA (`ca.der`, its key discarded)
/// and a leaf for IP 127.0.0.1 and `localhost` (`server.der`, PKCS#8 `server.key.der`), valid for 100 years, made with
/// `openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256` and `openssl x509 -req`.
#[tokio::test]
async fn wss_connects_with_the_explicit_provider() {
    use tokio_rustls::rustls;
    let fixtures = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("tests/fixtures/tls");
    let read = |name: &str| std::fs::read(fixtures.join(name)).unwrap();
    let ca = rustls::pki_types::CertificateDer::from(read("ca.der"));
    let leaf = rustls::pki_types::CertificateDer::from(read("server.der"));
    let key = rustls::pki_types::PrivateKeyDer::Pkcs8(read("server.key.der").into());
    let server = rustls::ServerConfig::builder_with_provider(Arc::new(
        rustls::crypto::aws_lc_rs::default_provider(),
    ))
    .with_safe_default_protocol_versions()
    .unwrap()
    .with_no_client_auth()
    .with_single_cert(vec![leaf], key)
    .unwrap();
    let acceptor = tokio_rustls::TlsAcceptor::from(Arc::new(server));
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let (tx, mut seen) = mpsc::unbounded_channel();
    tokio::spawn(async move {
        while let Ok((tcp, _)) = listener.accept().await {
            let (acceptor, tx) = (acceptor.clone(), tx.clone());
            tokio::spawn(async move {
                if let Ok(tls) = acceptor.accept(tcp).await {
                    serve_ws(tls, Script::default(), true, tx).await;
                }
            });
        }
    });

    let base = url::Url::parse(&format!("https://127.0.0.1:{}", addr.port())).unwrap();
    let options = GatewayOptions {
        extra_roots: vec![ca],
        ..GatewayOptions::default()
    };
    let gw = WsGateway::connect_with(base, AuthToken::new("tok-tls".into()), options);
    let mut events = gw.events();
    let event = next_event(&mut events, Duration::from_secs(10)).await;
    let GatewayEvent::Connected { session } = event else {
        panic!("expected Connected over wss, got {event:?}");
    };
    assert_eq!(ReadyInfo::from_frame(&session).unwrap().wire, 6);
    let Some(Seen::Upgrade { authorization, .. }) = seen.recv().await else {
        panic!("upgrade")
    };
    assert_eq!(authorization.as_deref(), Some("Bearer tok-tls"));
    gw.stop();
}

/// The same server without the test CA among the roots: the handshake fails and the gateway never connects.
#[tokio::test]
async fn wss_refuses_an_unknown_ca() {
    use tokio_rustls::rustls;
    let fixtures = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("tests/fixtures/tls");
    let read = |name: &str| std::fs::read(fixtures.join(name)).unwrap();
    let leaf = rustls::pki_types::CertificateDer::from(read("server.der"));
    let key = rustls::pki_types::PrivateKeyDer::Pkcs8(read("server.key.der").into());
    let server = rustls::ServerConfig::builder_with_provider(Arc::new(
        rustls::crypto::aws_lc_rs::default_provider(),
    ))
    .with_safe_default_protocol_versions()
    .unwrap()
    .with_no_client_auth()
    .with_single_cert(vec![leaf], key)
    .unwrap();
    let acceptor = tokio_rustls::TlsAcceptor::from(Arc::new(server));
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let (tx, mut seen) = mpsc::unbounded_channel();
    tokio::spawn(async move {
        while let Ok((tcp, _)) = listener.accept().await {
            let (acceptor, tx) = (acceptor.clone(), tx.clone());
            tokio::spawn(async move {
                if let Ok(tls) = acceptor.accept(tcp).await {
                    serve_ws(tls, Script::default(), true, tx).await;
                }
            });
        }
    });
    let base = url::Url::parse(&format!("https://127.0.0.1:{}", addr.port())).unwrap();
    let gw = WsGateway::connect(base, AuthToken::new("tok-tls".into()));
    let mut events = gw.events();
    assert!(
        tokio::time::timeout(Duration::from_millis(1_500), events.recv())
            .await
            .is_err(),
        "no event without trust"
    );
    assert!(
        seen.try_recv().is_err(),
        "the upgrade never reached the server"
    );
    gw.stop();
}

// ---------------------------------------------------------------------------------------------------------------
// A real dillad (dilla-testhost: dillad with its /debug control listener).

struct Host {
    child: std::process::Child,
    public: String,
    control: String,
    _data: TempDir,
}

impl Drop for Host {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

struct TempDir(PathBuf);
impl Drop for TempDir {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

fn start_host() -> Host {
    use std::io::{BufRead, BufReader};
    let root = repo_root();
    let bin = std::env::var_os("DILLA_TESTHOST")
        .map_or_else(|| root.join("target/dilla-testhost"), PathBuf::from);
    let wasm = std::env::var_os("DILLA_CORE_WASI").map_or_else(
        || root.join("target/wasm32-wasip1/release/dilla_core_wasi.wasm"),
        PathBuf::from,
    );
    assert!(
        bin.exists(),
        "{} missing: go build -o target/dilla-testhost ./cmd/dilla-testhost",
        bin.display()
    );
    assert!(
        wasm.exists(),
        "{} missing: cargo build -p dilla-core-wasi --target wasm32-wasip1 --release",
        wasm.display()
    );
    let data = root.join(format!("target/gateway-test-host-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&data);
    std::fs::create_dir_all(&data).unwrap();
    let mut child = std::process::Command::new(bin)
        .args([
            "-listen",
            "127.0.0.1:0",
            "-control",
            "127.0.0.1:0",
            "-log-level",
            "warn",
            "-production-acl",
        ])
        .arg("-core")
        .arg(&wasm)
        .arg("-data-dir")
        .arg(&data)
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::inherit())
        .spawn()
        .expect("start dilla-testhost");
    let mut lines = BufReader::new(child.stdout.take().unwrap()).lines();
    let (mut public, mut control) = (None, None);
    while public.is_none() || control.is_none() {
        let line = lines.next().expect("dilla-testhost exited").unwrap();
        if let Some(rest) = line.strip_prefix("public ") {
            public = Some(rest.trim().to_owned());
        } else if let Some(rest) = line.strip_prefix("control ") {
            control = Some(rest.trim().to_owned());
        }
    }
    // Keep draining stdout so the host never blocks on a full pipe.
    std::thread::spawn(move || for _ in lines {});
    Host {
        child,
        public: public.unwrap(),
        control: control.unwrap(),
        _data: TempDir(data),
    }
}

fn rand16() -> [u8; 16] {
    use std::hash::{BuildHasher, Hasher};
    let mut out = [0u8; 16];
    for half in out.chunks_mut(8) {
        let mut h = std::collections::hash_map::RandomState::new().build_hasher();
        h.write_u128(Instant::now().elapsed().as_nanos());
        half.copy_from_slice(&h.finish().to_be_bytes());
    }
    out
}

fn totp_code(secret_b32: &str, counter: u64) -> String {
    use hmac::{Hmac, KeyInit, Mac};
    let mut key = Vec::new();
    let (mut buf, mut bits) = (0u32, 0u32);
    for c in secret_b32.trim_end_matches('=').chars() {
        let v = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
            .find(c.to_ascii_uppercase())
            .expect("base32") as u32;
        buf = (buf << 5) | v;
        bits += 5;
        if bits >= 8 {
            bits -= 8;
            key.push((buf >> bits) as u8);
        }
    }
    let mut mac = Hmac::<sha1::Sha1>::new_from_slice(&key).unwrap();
    mac.update(&counter.to_be_bytes());
    let d = mac.finalize().into_bytes();
    let o = usize::from(d[d.len() - 1] & 0x0f);
    let word = u32::from_be_bytes([d[o] & 0x7f, d[o + 1], d[o + 2], d[o + 3]]);
    format!("{:06}", word % 1_000_000)
}

/// Seeds one person through the control listener, then enrols and confirms an authenticator app over the real
/// `/v1/auth/totp/*` routes (every account owes a second factor before its session opens a gateway), and answers its
/// session token.
async fn enrolled_session(host: &Host) -> String {
    let http = reqwest::Client::new();
    let device = rand16();
    let seed = format!(
        r#"[{{"username":"gw{u}","display":"Gateway","bot":false,"umk_pub":"{k}","ssk_pub":"{k}","sig_umk_ssk":"{s}",
            "devices":[{{"device_id":"{d}","dsk_pub":"{k}","tier":0,"signer_tier":0,"credential":"80"}}]}}]"#,
        u = hex(&device[..6]),
        k = hex(&[7; 32]),
        s = hex(&[7; 64]),
        d = hex(&device),
    );
    let out = http
        .post(format!("{}/debug/seed", host.control))
        .header("content-type", "application/json")
        .body(seed)
        .send()
        .await
        .unwrap();
    assert!(out.status().is_success(), "seed: {}", out.status());
    let out: serde_json::Value = serde_json::from_slice(&out.bytes().await.unwrap()).unwrap();
    let token = out[0]["tokens"][hex(&device)].as_str().unwrap().to_owned();

    let cbor = |f: &dyn Fn(&mut dilla_core::cbor::Encoder)| {
        let mut e = dilla_core::cbor::Encoder::new();
        f(&mut e);
        e.into_vec()
    };
    let post = async |path: &str, body: Vec<u8>| {
        let r = http
            .post(format!("{}{path}", host.public))
            .header("authorization", format!("Bearer {token}"))
            .header("content-type", "application/cbor")
            .body(body)
            .send()
            .await
            .unwrap();
        assert!(r.status().is_success(), "{path}: {}", r.status());
        r.bytes().await.unwrap().to_vec()
    };
    let enrol = post(
        "/v1/auth/totp/enroll",
        cbor(&|e| {
            e.array(1).null();
        }),
    )
    .await;
    let secret = dilla_core::cbor::decode_strict(&enrol, |d| {
        d.array(2)?;
        let s = d.text()?.to_owned();
        d.skip()?;
        Ok(s)
    })
    .unwrap();
    let clock: serde_json::Value = serde_json::from_slice(
        &http
            .get(format!("{}/debug/clock", host.control))
            .send()
            .await
            .unwrap()
            .bytes()
            .await
            .unwrap(),
    )
    .unwrap();
    let code = totp_code(&secret, clock["now"].as_u64().unwrap() / 30);
    post(
        "/v1/auth/totp/confirm",
        cbor(&|e| {
            e.array(1).text(&code);
        }),
    )
    .await;
    token
}

#[tokio::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (no Go toolchain in the desktop-gpui CI job)"]
async fn hello_negotiates_same_versions_as_ts() {
    let host = tokio::task::spawn_blocking(start_host).await.unwrap();
    let token = enrolled_session(&host).await;
    let gw = WsGateway::connect(
        url::Url::parse(&host.public).unwrap(),
        AuthToken::new(token),
    );
    let mut events = gw.events();
    let event = next_event(&mut events, Duration::from_secs(20)).await;
    let GatewayEvent::Connected { session } = event else {
        panic!(
            "expected Connected, got {event:?} (refusal {:?})",
            gw.refusal()
        );
    };
    let ready = ReadyInfo::from_frame(&session).unwrap();
    // gateway.ts: wire = highestCommon(hello wires, [6]), e2ee and media = highestCommon(…, [1]); the ready echoes them.
    assert_eq!((ready.wire, ready.e2ee, ready.media), (6, 1, 1));
    let hello = gw.hello().expect("the hello of the ready connection");
    assert!(
        hello.wire_versions.contains(&6),
        "dillad offers wire 6: {:?}",
        hello.wire_versions
    );
    assert!(hello.heartbeat_ms > 0 && hello.max_frame_bytes > 0);
    gw.stop();
}
