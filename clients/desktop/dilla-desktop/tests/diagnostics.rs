//! Task 24's parity, behaviour and keyboard tests for the diagnostics area (standard procedure S4-S6).
//!
//! - Behaviour tests run a real dillad (`target/dilla-testhost` and the WASI core, see `tests/support`), with the client
//!   behind a counting TCP proxy that reads the request line of every HTTP request and counts the WebSocket upgrades.
//!   They are `#[ignore]`d so a plain `cargo test` needs no Go toolchain.
//! - Parity and keyboard tests drive the automation build on a screen fixture, drawn in the settings host (Task 1b,
//!   merged in integration 3 with its `Settings` fixture arm), against the references `shoot.mjs` shot in the parity
//!   world's parity-fixture mode.

mod support;

use std::io::{BufRead, BufReader, Read, Write};
use std::net::{SocketAddr, TcpListener, TcpStream};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use dilla_client::Client;
use dilla_client::gateway::GatewayOptions;
use dilla_client::state::{AccountPhase, AppState, ConnectionStatus};
use dilla_desktop::screens::diagnostics::banner::{BannerPlan, UpdateMessage, plan};
use dilla_parity::screen::assert_screen;

use support::world::{self, Person};

// ---- Parity (S4) ------------------------------------------------------------------------------------------------

fn fixture_path(name: &str) -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../parity/fixtures")
        .join(format!("{name}.json"))
}

const NEEDS_HOST: &str = "the diagnostics/section fixture loads in the settings host";

#[test]
fn parity_diagnostics_section() {
    assert_screen(&fixture_path("diagnostics/section"))
        .unwrap_or_else(|e| panic!("diagnostics/section differs from its web reference: {e}"));
}

#[test]
fn parity_diagnostics_section_b() {
    assert_screen(&fixture_path("diagnostics/section-b"))
        .unwrap_or_else(|e| panic!("diagnostics/section-b differs from its web reference: {e}"));
}

// ---- Keyboard (S6) ----------------------------------------------------------------------------------------------

/// Tab through the section: the only stop is Copy codes (the facts are rows, not controls), and Enter on it is a press.
/// The area has no shortcut of its own (settings/Keyboard.tsx lists none for Diagnostics).
#[test]
fn keyboard_diagnostics_focus_order() {
    let path = fixture_path("diagnostics/section");
    let loaded = dilla_parity::fixture::load(&path, dilla_parity::Os::current())
        .unwrap_or_else(|e| panic!("{NEEDS_HOST}: {e}"));
    let mut app = dilla_parity::Driver::launch_fixture(&path, loaded.shot_path.as_deref(), &[]);
    app.wait_for("diag-copy", Duration::from_secs(30));
    let mut order = Vec::new();
    for _ in 0..3 {
        app.press("tab");
        order.push(app.focused());
    }
    let first = order
        .iter()
        .flatten()
        .position(|id| id == "diag-copy-button");
    assert!(first.is_some(), "Tab reaches Copy codes: {order:?}");
}

// ---- A real dillad behind a counting proxy ----------------------------------------------------------------------

/// What the proxy saw: `"GET /v1/instance"`, `"WS /gateway"`, in order.
type Log = Arc<Mutex<Vec<String>>>;

/// A TCP proxy in front of the host. For each client connection it reads the HTTP request heads (a method, a path
/// without its query, whether it upgrades to a WebSocket, how long its body is), logs them and forwards every byte
/// untouched; the other direction is a plain copy. Chunked bodies are forwarded blind after the head.
struct Proxy {
    addr: SocketAddr,
    log: Log,
    stop: Arc<AtomicBool>,
}

impl Proxy {
    fn start(upstream: SocketAddr) -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        listener.set_nonblocking(true).unwrap();
        let addr = listener.local_addr().unwrap();
        let log: Log = Arc::default();
        let stop = Arc::new(AtomicBool::new(false));
        let (l, s) = (log.clone(), stop.clone());
        std::thread::spawn(move || {
            while !s.load(Ordering::Relaxed) {
                match listener.accept() {
                    Ok((client, _)) => {
                        client.set_nonblocking(false).unwrap();
                        let Ok(server) = TcpStream::connect(upstream) else {
                            continue;
                        };
                        let (c2, s2) = (client.try_clone().unwrap(), server.try_clone().unwrap());
                        let l = l.clone();
                        std::thread::spawn(move || pump_requests(client, server, &l));
                        std::thread::spawn(move || {
                            let (mut from, mut to) = (s2, c2);
                            let _ = std::io::copy(&mut from, &mut to);
                            let _ = to.shutdown(std::net::Shutdown::Both);
                        });
                    }
                    Err(_) => std::thread::sleep(Duration::from_millis(10)),
                }
            }
        });
        Self { addr, log, stop }
    }

    fn url(&self) -> url::Url {
        url::Url::parse(&format!("http://{}", self.addr)).unwrap()
    }

    fn entries(&self) -> Vec<String> {
        self.log.lock().unwrap().clone()
    }

    fn mark(&self) -> usize {
        self.log.lock().unwrap().len()
    }

    /// The WebSocket upgrades so far.
    fn sockets(&self) -> usize {
        self.entries()
            .iter()
            .filter(|e| e.starts_with("WS "))
            .count()
    }
}

impl Drop for Proxy {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Relaxed);
    }
}

fn pump_requests(mut from: TcpStream, mut to: TcpStream, log: &Log) {
    let mut buf = [0u8; 16 * 1024];
    let mut pending: Vec<u8> = Vec::new();
    let mut body_left = 0usize;
    let mut raw = false;
    loop {
        let n = match from.read(&mut buf) {
            Ok(0) | Err(_) => break,
            Ok(n) => n,
        };
        if raw {
            if to.write_all(&buf[..n]).is_err() {
                break;
            }
            continue;
        }
        pending.extend_from_slice(&buf[..n]);
        while !pending.is_empty() {
            if body_left > 0 {
                let take = body_left.min(pending.len());
                if to.write_all(&pending[..take]).is_err() {
                    return;
                }
                pending.drain(..take);
                body_left -= take;
                continue;
            }
            let Some(end) = pending.windows(4).position(|w| w == b"\r\n\r\n") else {
                break;
            };
            let head_len = end + 4;
            let head = String::from_utf8_lossy(&pending[..end]).into_owned();
            let mut lines = head.lines();
            let request = lines.next().unwrap_or_default();
            let mut parts = request.split(' ');
            let (method, target) = (
                parts.next().unwrap_or_default(),
                parts.next().unwrap_or_default(),
            );
            let path = target.split('?').next().unwrap_or_default();
            let mut upgrade = false;
            let mut chunked = false;
            let mut length = 0usize;
            for header in lines {
                let lower = header.to_ascii_lowercase();
                if lower.starts_with("upgrade:") && lower.contains("websocket") {
                    upgrade = true;
                } else if lower.starts_with("transfer-encoding:") && lower.contains("chunked") {
                    chunked = true;
                } else if let Some(v) = lower.strip_prefix("content-length:") {
                    length = v.trim().parse().unwrap_or(0);
                }
            }
            log.lock().unwrap().push(if upgrade {
                format!("WS {path}")
            } else {
                format!("{method} {path}")
            });
            if to.write_all(&pending[..head_len]).is_err() {
                return;
            }
            pending.drain(..head_len);
            if upgrade || chunked {
                raw = true;
                if to.write_all(&pending).is_err() {
                    return;
                }
                pending.clear();
            } else {
                body_left = length;
            }
        }
    }
    let _ = to.shutdown(std::net::Shutdown::Both);
}

/// The routes that may use HTTP while the socket is up: `PRE_GATEWAY_ROUTES` of
/// packages/client-core/src/http/socket-routes.ts (session establishment repeats), and the binary transfers the spec
/// keeps on HTTP (blobs, GIF media, backup chunks; e2e/web/socket-only.spec.ts `TRANSFERS`).
const ALLOWED_WHILE_UP: &[(&str, &str)] = &[
    ("GET", "/v1/instance"),
    ("PUT", "/v1/users/{}/device-list"),
    ("GET", "/v1/users/{}/device-list"),
    ("GET", "/v1/auth/factors"),
    ("GET", "/v1/instance/limits"),
    ("GET", "/v1/keypackages"),
    ("POST", "/v1/keypackages"),
    ("GET", "/v1/backups"),
    ("GET", "/v1/backups/{}/{}"),
    ("PUT", "/v1/backups/{}/{}"),
    ("DELETE", "/v1/backups/{}/{}"),
    ("GET", "/v1/accounts/me"),
    ("POST", "/v1/gateway/ticket"),
    ("POST", "/v1/devices/{}/sessions"),
    ("DELETE", "/v1/devices/{}/sessions"),
    ("POST", "/v1/devices/{}/sessions/challenge"),
    ("GET", "/v1/channels/{}/blobs/{}"),
    ("PUT", "/v1/channels/{}/blobs/{}"),
    ("DELETE", "/v1/channels/{}/blobs/{}"),
    ("POST", "/v1/channels/{}/blobs/{}/confirm"),
    ("GET", "/v1/communities/{}/gifs/media/{}"),
];

/// A Go ServeMux pattern with `{}` for one segment against a path.
fn matches(pattern: &str, path: &str) -> bool {
    let (p, q): (Vec<_>, Vec<_>) = (pattern.split('/').collect(), path.split('/').collect());
    p.len() == q.len() && p.iter().zip(&q).all(|(a, b)| *a == "{}" || a == b)
}

/// Every HTTP request in `entries` that a client with its socket up must not make.
fn offenders(entries: &[String]) -> Vec<String> {
    entries
        .iter()
        .filter(|e| !e.starts_with("WS "))
        .filter(|e| {
            let (method, path) = e.split_once(' ').unwrap_or_default();
            !ALLOWED_WHILE_UP
                .iter()
                .any(|(m, p)| *m == method && matches(p, path))
        })
        .cloned()
        .collect()
}

#[test]
fn the_allow_list_matcher_reads_patterns() {
    assert!(matches(
        "/v1/users/{}/device-list",
        "/v1/users/ab12/device-list"
    ));
    assert!(!matches("/v1/users/{}/device-list", "/v1/users/ab12/other"));
    assert!(!matches("/v1/instance", "/v1/instance/limits"));
    let seen = vec![
        "POST /v1/gateway/ticket".to_owned(),
        "WS /gateway".to_owned(),
        "GET /v1/communities".to_owned(),
        "GET /v1/channels/ab/blobs/cd".to_owned(),
    ];
    assert_eq!(offenders(&seen), ["GET /v1/communities"]);
}

/// A dilla-testhost process on `listen`, serving `data`; `wire` is its `-wire` flag.
struct TestHost {
    child: Child,
    public: String,
    control: String,
    invites: Vec<String>,
}

impl Drop for TestHost {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

fn spawn_host(data: &Path, listen: &str, wire: Option<u64>) -> TestHost {
    let root = world::repo_root();
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
    std::fs::create_dir_all(data).unwrap();
    let mut cmd = Command::new(bin);
    cmd.args([
        "-listen",
        listen,
        "-control",
        "127.0.0.1:0",
        "-log-level",
        "warn",
        "-production-acl",
    ])
    .arg("-core")
    .arg(&wasm)
    .arg("-data-dir")
    .arg(data);
    if let Some(wire) = wire {
        cmd.arg(format!("-wire={wire}"));
    }
    let mut child = cmd
        .stdout(Stdio::piped())
        .stderr(Stdio::inherit())
        .spawn()
        .expect("start dilla-testhost");
    let mut lines = BufReader::new(child.stdout.take().unwrap()).lines();
    let (mut public, mut control, mut invites) = (None, None, None);
    while public.is_none() || control.is_none() || invites.is_none() {
        let line = lines.next().expect("dilla-testhost exited").unwrap();
        if let Some(rest) = line.strip_prefix("public ") {
            public = Some(rest.trim().to_owned());
        } else if let Some(rest) = line.strip_prefix("control ") {
            control = Some(rest.trim().to_owned());
        }
        if let Some(codes) = line.split("DILLA_TESTKIT_INVITE=").nth(1) {
            let codes = codes.split_whitespace().next().unwrap_or_default();
            invites = Some(codes.split(',').map(str::to_owned).collect::<Vec<_>>());
        }
    }
    std::thread::spawn(move || for _ in lines {});
    TestHost {
        child,
        public: public.unwrap(),
        control: control.unwrap(),
        invites: invites.unwrap(),
    }
}

fn free_port() -> u16 {
    TcpListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
        .port()
}

fn scratch_dir(tag: &str) -> PathBuf {
    let dir = world::repo_root().join(format!("target/{tag}-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    dir
}

/// Alice, signed up on `host` (an authenticator app, a password, sealed backups), alone in a community with #general.
/// Her session is cleared so the client establishes its own from the device key.
async fn alice_in_a_community(host: &TestHost, root: &Path) -> (Person, [u8; 16]) {
    use dilla_core::ids::DeviceId;
    use dilla_testkit::HttpDs;
    let alice = world::sign_up_at(
        &host.public,
        &host.control,
        root,
        host.invites.last().unwrap(),
        "alice",
        "Alice",
    )
    .await;
    let (public, device, token) = (host.public.clone(), alice.device_id, alice.token.clone());
    let community = tokio::task::spawn_blocking(move || {
        let mut ds = HttpDs::with_session(&public, DeviceId::from_bytes(device), token).unwrap();
        let c = ds.create_community("diagnostics").unwrap();
        ds.create_text_channel(&c, "general").unwrap();
        c
    })
    .await
    .unwrap();
    world::clear_session(&alice);
    (alice, community)
}

/// Starts `alice`'s client against `server` (the proxy), with the gateway's timers of `options`.
async fn start_via(alice: &Person, server: url::Url, options: GatewayOptions) -> Client {
    let mut cfg = alice.cfg.clone();
    cfg.server = server;
    let kc = alice.kc.clone();
    tokio::task::spawn_blocking(move || {
        Client::start_with(cfg, kc, options).expect("client starts")
    })
    .await
    .unwrap()
}

async fn online_with_channels(client: &Client) -> Arc<AppState> {
    world::wait_for(
        client,
        Duration::from_secs(60),
        "online with its channels",
        |s| s.connection.status == ConnectionStatus::Online && !s.channels.is_empty(),
    )
    .await
}

// ---- Behaviour (S5) ---------------------------------------------------------------------------------------------

/// e2e/web/socket-only.spec.ts, test 1 ("no app request uses HTTP while the socket is up"): once the gateway socket has
/// been opened, a session of reading and writing (open a channel, send, mark read, refresh the structure) makes no
/// HTTP request beyond the pre-gateway routes and the binary transfers.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn no_app_http_while_socket_up() {
    let data = scratch_dir("diag-http-data");
    let root = scratch_dir("diag-http-root");
    let port = free_port();
    let host = tokio::task::spawn_blocking({
        let data = data.clone();
        move || spawn_host(&data, &format!("127.0.0.1:{port}"), None)
    })
    .await
    .unwrap();
    let (alice, _community) = alice_in_a_community(&host, &root).await;
    let proxy = Proxy::start(format!("127.0.0.1:{port}").parse().unwrap());
    let client = start_via(&alice, proxy.url(), GatewayOptions::default()).await;
    let state = online_with_channels(&client).await;
    assert!(
        proxy.sockets() >= 1,
        "the gateway socket went through the proxy"
    );
    let up = proxy.mark();

    let general = state
        .channels
        .iter()
        .find(|c| c.name == "general")
        .unwrap()
        .id;
    client
        .open_channel(general)
        .await
        .unwrap()
        .expect("opens #general");
    client
        .send_text(general, "over the socket".into())
        .await
        .unwrap()
        .expect("queued");
    world::wait_for(
        &client,
        Duration::from_secs(30),
        "the message to be stored",
        |s| {
            s.timelines.get(&general).is_some_and(|t| {
                t.items
                    .iter()
                    .any(|i| i.body == "over the socket" && i.seq.is_some())
            })
        },
    )
    .await;
    client
        .mark_read(general)
        .await
        .unwrap()
        .expect("marks read");
    client.refresh();
    tokio::time::sleep(Duration::from_secs(2)).await;

    let after = proxy.entries()[up..].to_vec();
    assert_eq!(
        offenders(&after),
        Vec::<String>::new(),
        "HTTP requests made while the socket was up"
    );
    client.stop();
    drop(host);
    let _ = std::fs::remove_dir_all(&data);
    let _ = std::fs::remove_dir_all(&root);
}

/// e2e/web/realtime-transport.spec.ts, "wire 6 stays free of periodic version/activity/stats HTTP for fifteen idle
/// minutes": the heartbeat interval is the server's (`hello.heartbeat_ms / 2`, here capped at 200 ms), so fifteen of its
/// intervals pass in a few seconds. Idle and online, the client makes no HTTP request and opens no second socket.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn idle_socket_no_periodic_http() {
    const BEAT: Duration = Duration::from_millis(200);
    const INTERVALS: u32 = 15;
    let data = scratch_dir("diag-idle-data");
    let root = scratch_dir("diag-idle-root");
    let port = free_port();
    let host = tokio::task::spawn_blocking({
        let data = data.clone();
        move || spawn_host(&data, &format!("127.0.0.1:{port}"), None)
    })
    .await
    .unwrap();
    let (alice, _community) = alice_in_a_community(&host, &root).await;
    let proxy = Proxy::start(format!("127.0.0.1:{port}").parse().unwrap());
    let options = GatewayOptions {
        heartbeat_max: BEAT,
        ..GatewayOptions::default()
    };
    let client = start_via(&alice, proxy.url(), options).await;
    let state = online_with_channels(&client).await;
    // Let the first reconciliation after ready finish before the quiet window starts.
    tokio::time::sleep(Duration::from_secs(3)).await;
    let (up, sockets, readies) = (proxy.mark(), proxy.sockets(), state.connection.readies);

    let started = Instant::now();
    tokio::time::sleep(BEAT * (INTERVALS + 1)).await;
    assert!(started.elapsed() >= BEAT * INTERVALS);

    let idle = proxy.entries()[up..].to_vec();
    assert_eq!(idle, Vec::<String>::new(), "requests while idle and online");
    assert_eq!(proxy.sockets(), sockets, "no new socket while idle");
    let now = client.state().borrow().clone();
    assert_eq!(
        (now.connection.status, now.connection.readies),
        (ConnectionStatus::Online, readies),
        "still the same ready"
    );
    client.stop();
    drop(host);
    let _ = std::fs::remove_dir_all(&data);
    let _ = std::fs::remove_dir_all(&root);
}

/// e2e/web/version-reload.spec.ts, the desktop leg: the host is replaced by one that offers only wire 7 (newer than this
/// client's 6). The client is refused from the hello: the phase is `VersionRefused { upgrade: true }`, the banner slot
/// shows "Update dilla to continue", no socket is opened again, and the channels are kept. The composer's draft is Task
/// 9's; its assertion belongs to Task 28 once the composer persists drafts.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn older_client_refused_banner_no_reconnect_keeps_draft() {
    let data = scratch_dir("diag-ver-data");
    let root = scratch_dir("diag-ver-root");
    let port = free_port();
    let listen = format!("127.0.0.1:{port}");
    let wire6 = tokio::task::spawn_blocking({
        let (data, listen) = (data.clone(), listen.clone());
        move || spawn_host(&data, &listen, None)
    })
    .await
    .unwrap();
    let (alice, _community) = alice_in_a_community(&wire6, &root).await;
    let proxy = Proxy::start(listen.parse().unwrap());
    let client = start_via(&alice, proxy.url(), GatewayOptions::default()).await;
    let state = online_with_channels(&client).await;
    let channels = state.channels.len();
    assert_eq!(state.account.phase, AccountPhase::Ready);
    assert_eq!(
        plan(&state, UpdateMessage::Required),
        None,
        "no banner while online"
    );

    // The wire-6 host goes, a wire-7 host takes its port and its data.
    drop(wire6);
    let wire7 = tokio::task::spawn_blocking({
        let (data, listen) = (data.clone(), listen.clone());
        move || spawn_host(&data, &listen, Some(7))
    })
    .await
    .unwrap();

    let refused = world::wait_for(
        &client,
        Duration::from_secs(120),
        "the version refusal",
        |s| matches!(s.account.phase, AccountPhase::VersionRefused { .. }),
    )
    .await;
    assert_eq!(
        refused.account.phase,
        AccountPhase::VersionRefused { upgrade: true }
    );
    assert_eq!(refused.connection.status, ConnectionStatus::Offline);
    assert_eq!(
        plan(&refused, UpdateMessage::Required),
        Some(BannerPlan::UpdateRequired(UpdateMessage::Required))
    );

    // No reconnect loop: no socket in the quiet window after the refusal.
    let sockets = proxy.sockets();
    tokio::time::sleep(Duration::from_secs(4)).await;
    assert_eq!(
        proxy.sockets(),
        sockets,
        "gateway sockets opened in 4 s after the refusal"
    );
    let now = client.state().borrow().clone();
    assert!(matches!(
        now.account.phase,
        AccountPhase::VersionRefused { .. }
    ));

    // The work stays.
    assert_eq!(now.channels.len(), channels, "the structure is kept");
    client.stop();
    drop(wire7);
    let _ = std::fs::remove_dir_all(&data);
    let _ = std::fs::remove_dir_all(&root);
}
