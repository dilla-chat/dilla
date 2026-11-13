//! A real dillad (dilla-testhost with its in-process LiveKit) and the people, peers and media
//! processes the call tests need: `tests/calls.rs` here and `dilla-media/tests/e2e_call.rs` (which
//! includes this file by path). Every account is made the way the app makes one (tests/sync.rs):
//! a sign-up from the client core's own identity, its device list, an authenticator app over the
//! real `/v1/auth/totp/*` routes; each `Client` then establishes its own session from its device
//! key.
//!
//! - [`Driver`] is the testkit's native peer (`dilla-testkit web-driver`, the process the web e2e
//!   specs drive): it joins a call group without entering the room and commits only when told.
//! - [`MediaPeer`] is one desktop media participant (`dilla-media`'s `call-peer` example, one
//!   process each: the frame hook is process-wide): a LiveKit connection with the call group's
//!   keys, playing a tone and reporting what it hears.
//!
//! Needs `target/dilla-testhost` (or `DILLA_TESTHOST`) and the WASI core (`DILLA_CORE_WASI`, else
//! `target/wasm32-wasip1/release/dilla_core_wasi.wasm`).

#![allow(dead_code)]

use std::io::{BufRead, BufReader, Read, Write};
use std::net::{TcpListener, TcpStream};
use std::path::{Path, PathBuf};
use std::process::{Child, ChildStdin, Command, Stdio};
use std::sync::Arc;
use std::sync::mpsc::{Receiver, channel};
use std::time::{Duration, Instant};

use dilla_client::Client;
use dilla_client::calls::MediaEpoch;
use dilla_client::http::routes::{self as r, NoBody, Raw};
use dilla_client::http::{AuthToken, ReqwestHttp};
use dilla_client::instances::Instances;
use dilla_client::keychain::MemoryKeychain;
use dilla_client::seams::{Http, InstanceConfig};
use dilla_client::store::open_store;
use dilla_core::cbor::{Encoder, decode_strict};
use serde_json::{Value, json};

pub fn repo_root() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../..")
        .canonicalize()
        .expect("repo root")
}

pub fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

pub fn unhex16(s: &str) -> [u8; 16] {
    let mut out = [0u8; 16];
    for (i, b) in out.iter_mut().enumerate() {
        *b = u8::from_str_radix(&s[2 * i..2 * i + 2], 16).expect("hex");
    }
    out
}

fn free_port() -> u16 {
    TcpListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
        .port()
}

/// The build this test binary belongs to: `target/<profile>`.
fn profile_dir() -> PathBuf {
    let exe = std::env::current_exe().unwrap();
    exe.parent().unwrap().parent().unwrap().to_path_buf()
}

/// Builds `args` once per process (an example or a binary the test spawns), in this build's profile.
fn cargo_build(once: &'static std::sync::Once, args: &[&str]) {
    once.call_once(|| {
        let cargo = std::env::var("CARGO").unwrap_or_else(|_| "cargo".into());
        let mut build = Command::new(cargo);
        build.arg("build").args(args);
        if !cfg!(debug_assertions) {
            build.arg("--release");
        }
        assert!(build.status().unwrap().success(), "cargo build {args:?}");
    });
}

// ---- the instance ----

pub struct Host {
    child: Child,
    pub public: String,
    pub control: String,
    pub invites: Vec<String>,
    data: PathBuf,
}

impl Drop for Host {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
        let _ = std::fs::remove_dir_all(&self.data);
    }
}

/// dilla-testhost with production's ACL and its in-process LiveKit.
pub fn start_host(tag: &str) -> Host {
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
    let data = root.join(format!(
        "target/calls-test-host-{tag}-{}",
        std::process::id()
    ));
    let _ = std::fs::remove_dir_all(&data);
    std::fs::create_dir_all(&data).unwrap();
    let mut child = Command::new(bin)
        .args(["-listen", "127.0.0.1:0", "-control", "127.0.0.1:0"])
        .args(["-log-level", "warn", "-production-acl", "-sfu"])
        .args(["-sfu-port", &free_port().to_string()])
        .args(["-sfu-udp-port", &free_port().to_string()])
        .arg("-core")
        .arg(&wasm)
        .arg("-data-dir")
        .arg(&data)
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
    Host {
        child,
        public: public.unwrap(),
        control: control.unwrap(),
        invites: invites.unwrap(),
        data,
    }
}

impl Host {
    /// The LiveKit URL a call start answered, reached on loopback through dillad's own `/rtc` gate (its protocol,
    /// token, leaf and grant checks): dillad hands the instance domain's `wss://` URL,
    /// whose `/rtc` gate this host serves on its own listener, and that name resolves to nothing here. dilla-testhost
    /// itself hands a browser `ws://<listen>` for the same reason (its `LiveKitURL` with a web root); the path is kept.
    pub fn rtc_url(&self, answered: &str) -> String {
        let rest = answered.split_once("://").map_or(answered, |(_, r)| r);
        let path = rest.find('/').map_or("", |i| &rest[i..]);
        let authority = self
            .public
            .split_once("://")
            .map_or(self.public.as_str(), |(_, r)| r)
            .trim_end_matches('/');
        format!("ws://{authority}{path}")
    }
}

/// `GET <control>/debug/clock`'s `now`: the host's fake clock, for the authenticator code.
fn host_now(control: &str) -> u64 {
    let addr = control.trim_start_matches("http://");
    let mut s = TcpStream::connect(addr).unwrap();
    write!(
        s,
        "GET /debug/clock HTTP/1.1\r\nHost: {addr}\r\nConnection: close\r\n\r\n"
    )
    .unwrap();
    let mut answer = String::new();
    s.read_to_string(&mut answer).unwrap();
    let body = answer.split_once("\r\n\r\n").expect("an HTTP answer").1;
    let (start, end) = (body.find('{').unwrap(), body.rfind('}').unwrap());
    let v: Value = serde_json::from_str(&body[start..=end]).unwrap();
    v["now"].as_u64().unwrap()
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

// ---- people ----

/// One person on one device: the instance's data root, its keychain, and what sign-up answered.
pub struct Person {
    pub cfg: InstanceConfig,
    pub kc: Arc<MemoryKeychain>,
    pub user_id: [u8; 16],
    pub device_id: [u8; 16],
    pub token: String,
    _root: TempRoot,
}

struct TempRoot(PathBuf);
impl Drop for TempRoot {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

fn cbor(f: impl FnOnce(&mut Encoder)) -> Vec<u8> {
    let mut e = Encoder::new();
    f(&mut e);
    e.into_vec()
}

/// signup.ts `submit` from a fresh client core, then the device list and an authenticator app.
pub async fn sign_up(host: &Host, invite: &str, username: &str, tag: &str) -> Person {
    let root = repo_root().join(format!(
        "target/calls-test-client-{tag}-{username}-{}",
        std::process::id()
    ));
    let _ = std::fs::remove_dir_all(&root);
    let mut instances = Instances::load(&root).unwrap();
    let cfg = instances.add(host.public.parse().unwrap()).unwrap();
    let kc = Arc::new(MemoryKeychain::new());
    let mut core = open_store(&cfg, kc.as_ref()).expect("store");

    let anon = ReqwestHttp::new(cfg.server.clone(), AuthToken::anonymous()).unwrap();
    let Raw(doc) = anon
        .call::<r::GetInstance>(&[], NoBody)
        .await
        .expect("instance");
    let (instance_id, signer) = decode_strict(&doc, |d| {
        let len = d.array_len()?;
        for _ in 0..3 {
            d.skip()?;
        }
        let id = d.bytes_exact::<16>()?;
        for _ in 4..10 {
            d.skip()?;
        }
        let signer = d.bytes_exact::<32>()?;
        for _ in 11..len {
            d.skip()?;
        }
        Ok((id, signer))
    })
    .expect("instance document");
    core.signup_begin(&instance_id).expect("signup begin");
    let body = core
        .signup_request(
            invite,
            username,
            username,
            Some("correct horse battery staple"),
        )
        .expect("signup request");
    let Raw(created) = anon
        .call::<r::PostAccount>(&[], Raw(body))
        .await
        .expect("account");
    let (user_id, device_id, token, expires) = decode_strict(&created, |d| {
        let len = d.array_len()?;
        let u = d.bytes_exact::<16>()?;
        let dev = d.bytes_exact::<16>()?;
        let t = d.text()?.to_owned();
        let e = d.uint()?;
        for _ in 4..len {
            d.skip()?;
        }
        Ok((u, dev, t, e))
    })
    .expect("account answer");
    core.session_store(&token, expires, expires).unwrap();
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_secs();
    core.signup_complete(&user_id, username, now, &signer)
        .expect("signup complete");

    let authed = ReqwestHttp::new(cfg.server.clone(), AuthToken::new(token.as_str())).unwrap();
    authed
        .call::<r::PutDeviceList>(&[&hex(&user_id)], Raw(core.device_list_body().unwrap()))
        .await
        .expect("device list");
    core.device_list_published().unwrap();

    let Raw(enrol) = authed
        .call::<r::TotpEnroll>(
            &[],
            Raw(cbor(|e| {
                e.array(1).null();
            })),
        )
        .await
        .expect("totp enroll");
    let secret = decode_strict(&enrol, |d| {
        let len = d.array_len()?;
        let s = d.text()?.to_owned();
        for _ in 1..len {
            d.skip()?;
        }
        Ok(s)
    })
    .unwrap();
    let control = host.control.clone();
    let now = tokio::task::spawn_blocking(move || host_now(&control))
        .await
        .unwrap();
    let code = totp_code(&secret, now / 30);
    authed
        .call::<r::TotpConfirm>(
            &[],
            Raw(cbor(|e| {
                e.array(1).text(&code);
            })),
        )
        .await
        .expect("totp confirm");
    drop(core);
    Person {
        cfg,
        kc,
        user_id,
        device_id,
        token,
        _root: TempRoot(root),
    }
}

/// Forgets the sign-up session in the store, so the client establishes its own from the device key.
pub fn clear_session(p: &Person) {
    let mut core = open_store(&p.cfg, p.kc.as_ref()).unwrap();
    core.session_clear().unwrap();
}

/// The testkit's delivery-service client on `p`'s sign-up session (blocking).
pub fn ds_of(host: &Host, p: &Person) -> dilla_testkit::HttpDs {
    dilla_testkit::HttpDs::with_session(
        &host.public,
        dilla_core::ids::DeviceId::from_bytes(p.device_id),
        p.token.clone(),
    )
    .unwrap()
}

/// A community of `owner` with one voice channel, joined by `members` with an invite (blocking). Answers the community
/// and the voice channel.
pub fn voice_community(host: &Host, owner: &Person, members: &[&Person]) -> ([u8; 16], [u8; 16]) {
    let mut ds = ds_of(host, owner);
    let c = ds.create_community("gpui calls").unwrap();
    ds.create_text_channel(&c, "general").unwrap();
    let (voice, _, _) = ds.create_channel(&c, 1, "voice").unwrap();
    let code = ds.create_invite(&c, 10, 3600).unwrap();
    drop(ds);
    for m in members {
        ds_of(host, m).join_community(&c, &code).unwrap();
    }
    (c, voice)
}

/// `Client::start` blocks on its own runtime, so it is called off the test's.
pub async fn start(p: &Person) -> Client {
    let (cfg, kc) = (p.cfg.clone(), p.kc.clone());
    tokio::task::spawn_blocking(move || Client::start(cfg, kc).expect("client starts"))
        .await
        .unwrap()
}

/// Waits until the client's gateway is ready (its socket carries requests).
pub async fn connected(client: &Client) {
    let mut rx = client.state();
    let deadline = Instant::now() + Duration::from_secs(30);
    loop {
        if rx.borrow_and_update().connection.status == dilla_client::state::ConnectionStatus::Online
        {
            return;
        }
        let left = deadline.saturating_duration_since(Instant::now());
        assert!(!left.is_zero(), "the gateway never became ready");
        let _ = tokio::time::timeout(left, rx.changed()).await;
    }
}

// ---- the testkit's native peer ----

/// `dilla-testkit web-driver`: one JSON request per line, one answer per line.
pub struct Driver {
    child: Child,
    stdin: ChildStdin,
    lines: Receiver<Value>,
    next: u64,
}

impl Driver {
    pub fn start(host: &Host) -> Driver {
        static BUILT: std::sync::Once = std::sync::Once::new();
        cargo_build(&BUILT, &["-p", "dilla-testkit", "--bin", "dilla-testkit"]);
        let bin = profile_dir().join("dilla-testkit");
        let mut child = Command::new(bin)
            .args(["web-driver", "--ds", &host.public, "--seed"])
            .arg((std::process::id() & 0xff_ffff).to_string())
            .env("DILLA_TESTKIT_INVITE", host.invites.join(","))
            .env("DILLA_TESTKIT_CONTROL", &host.control)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::inherit())
            .spawn()
            .expect("dilla-testkit web-driver");
        let stdin = child.stdin.take().unwrap();
        let stdout = BufReader::new(child.stdout.take().unwrap());
        let (tx, lines) = channel();
        std::thread::spawn(move || {
            for line in stdout.lines().map_while(Result::ok) {
                if let Ok(v) = serde_json::from_str::<Value>(&line)
                    && tx.send(v).is_err()
                {
                    break;
                }
            }
        });
        Driver {
            child,
            stdin,
            lines,
            next: 1,
        }
    }

    /// One request, its answer (blocking; a refusal panics).
    pub fn request(&mut self, op: &str, mut args: Value) -> Value {
        let id = self.next;
        self.next += 1;
        args["id"] = json!(id);
        args["op"] = json!(op);
        writeln!(self.stdin, "{args}").unwrap();
        self.stdin.flush().unwrap();
        let answer = self
            .lines
            .recv_timeout(Duration::from_secs(120))
            .unwrap_or_else(|_| panic!("no answer to {op}"));
        assert_eq!(answer["id"], json!(id), "{answer}");
        assert_eq!(answer["ok"], json!(true), "{op}: {answer}");
        answer
    }
}

impl Drop for Driver {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

// ---- one desktop media participant ----

/// `dilla-media`'s `call-peer` example: a LiveKit connection publishing a tone (examples/call-peer.rs).
pub struct MediaPeer {
    child: Child,
    stdin: ChildStdin,
    lines: Receiver<Value>,
    seen: Vec<Value>,
}

impl MediaPeer {
    pub fn start(url: &str, token: &str, tone: u32) -> MediaPeer {
        Self::start_with(url, token, tone, &[])
    }

    /// [`MediaPeer::start`] with more `call-peer` arguments (`--muted 1`).
    pub fn start_with(url: &str, token: &str, tone: u32, extra: &[&str]) -> MediaPeer {
        static BUILT: std::sync::Once = std::sync::Once::new();
        cargo_build(&BUILT, &["-p", "dilla-media", "--example", "call-peer"]);
        let bin = profile_dir().join("examples/call-peer");
        let mut child = Command::new(bin)
            .args(["--url", url, "--token", token, "--tone", &tone.to_string()])
            .args(extra)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .spawn()
            .unwrap();
        let stdin = child.stdin.take().unwrap();
        let stdout = BufReader::new(child.stdout.take().unwrap());
        let (tx, lines) = channel();
        std::thread::spawn(move || {
            for line in stdout.lines().map_while(Result::ok) {
                if let Ok(v) = serde_json::from_str::<Value>(&line)
                    && tx.send(v).is_err()
                {
                    break;
                }
            }
        });
        MediaPeer {
            child,
            stdin,
            lines,
            seen: Vec::new(),
        }
    }

    pub fn send(&mut self, line: &str) {
        writeln!(self.stdin, "{line}").unwrap();
        self.stdin.flush().unwrap();
    }

    pub fn next(&mut self, event: &str, timeout: Duration) -> Value {
        let deadline = Instant::now() + timeout;
        loop {
            let left = deadline.saturating_duration_since(Instant::now());
            match self.lines.recv_timeout(left) {
                Ok(v) if v["event"] == event => return v,
                Ok(v) => {
                    assert_ne!(v["event"], "failed", "{v}");
                    self.seen.push(v);
                }
                Err(_) => panic!("no {event} within {timeout:?}; saw {:?}", self.seen),
            }
        }
    }

    /// Hands the call group's epoch keys to the participant (what `dilla_media::EpochFeed::publish` takes).
    pub fn epoch(&mut self, e: &MediaEpoch) {
        let roster: Vec<String> = e
            .roster
            .iter()
            .map(|(l, d, _)| format!("{l}:{}", hex(d)))
            .collect();
        self.send(&format!(
            "epoch {} {} {} {}",
            e.epoch,
            hex(&*e.base_key),
            e.own_leaf,
            roster.join(",")
        ));
        self.next("epoch", Duration::from_secs(5));
    }

    pub fn stats(&mut self) -> Value {
        self.send("stats");
        self.next("stats", Duration::from_secs(5))
    }

    /// Waits until this participant has decoded audio from `device` peaking at `hz`.
    pub fn hears(&mut self, device: &str, hz: f64, timeout: Duration) -> Value {
        let deadline = Instant::now() + timeout;
        loop {
            let s = self.stats();
            if s["peaks"][device]
                .as_f64()
                .is_some_and(|p| (p - hz).abs() <= 20.0)
            {
                return s;
            }
            assert!(
                Instant::now() < deadline,
                "no {hz} Hz from {device} within {timeout:?}: {s}"
            );
            std::thread::sleep(Duration::from_millis(250));
        }
    }

    /// Leaves the room cleanly (the page's pagehide leave) and waits for the process to end.
    pub fn quit(mut self) {
        self.send("quit");
        let deadline = Instant::now() + Duration::from_secs(10);
        while Instant::now() < deadline {
            if let Ok(Some(_)) = self.child.try_wait() {
                return;
            }
            std::thread::sleep(Duration::from_millis(50));
        }
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

impl Drop for MediaPeer {
    fn drop(&mut self) {
        let _ = writeln!(self.stdin, "quit");
        let _ = self.stdin.flush();
        std::thread::sleep(Duration::from_millis(200));
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}
