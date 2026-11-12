//! A real dillad (dilla-testhost) and the people on it, for Task 2's client tests (`removals`, `keypackages`,
//! `backups`, `lifecycle`). The helpers are `tests/sync.rs`'s (Task 5 moves those into `src/testing.rs` behind the
//! `testing` feature; until it lands, Task 2's tests carry their own copy here), plus a gateway socket of a sign-up
//! session for the routes the instance serves only as gateway requests.
//!
//! Needs `target/dilla-testhost` (`go build -o target/dilla-testhost ./cmd/dilla-testhost`, or `DILLA_TESTHOST`) and
//! the WASI core (`DILLA_CORE_WASI`, else `target/wasm32-wasip1/release/dilla_core_wasi.wasm`).
#![allow(dead_code)]

use std::io::{BufRead, BufReader};
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::{Duration, Instant};

use dilla_client::Client;
use dilla_client::gateway::frames::is_response_frame_head;
use dilla_client::gateway::{GatewayOptions, WsGateway};
use dilla_client::http::routes::{self as r, NoBody, Raw};
use dilla_client::http::{AuthToken, ReqwestHttp};
use dilla_client::instances::Instances;
use dilla_client::keychain::MemoryKeychain;
use dilla_client::seams::{Gateway, GatewayEvent, Http, InstanceConfig};
use dilla_client::state::{AppState, ChannelId, TimelineItemState, UserId};
use dilla_client::store::open_store;
use dilla_client::sync::requests::SocketRequests;
use dilla_core::cbor::{Encoder, decode_strict};

pub const IGNORE: &str = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)";

pub fn repo_root() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../..")
        .canonicalize()
        .expect("repo root")
}

pub struct Host {
    child: std::process::Child,
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
    let data = root.join(format!("target/t2-test-host-{tag}-{}", std::process::id()));
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

pub fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
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

pub fn cbor(f: impl FnOnce(&mut Encoder)) -> Vec<u8> {
    let mut e = Encoder::new();
    f(&mut e);
    e.into_vec()
}

/// The instance's clock (the testhost's `/debug/clock`), in Unix seconds.
pub async fn host_now(host: &Host) -> u64 {
    let clock: serde_json::Value = serde_json::from_slice(
        &reqwest::get(format!("{}/debug/clock", host.control))
            .await
            .unwrap()
            .bytes()
            .await
            .unwrap(),
    )
    .unwrap();
    clock["now"].as_u64().unwrap()
}

/// Moves the instance's clock on by `seconds`.
pub async fn advance_clock(host: &Host, seconds: u64) {
    let advanced = reqwest::Client::new()
        .post(format!("{}/debug/clock", host.control))
        .header("content-type", "application/json")
        .body(format!(r#"{{"seconds":{seconds}}}"#))
        .send()
        .await
        .unwrap();
    assert!(
        advanced.status().is_success(),
        "clock: {}",
        advanced.status()
    );
}

/// signup.ts `submit` from a fresh client core, then the device list and (unless `owe_factor`) an authenticator app.
/// An account that owes its second factor can establish a session, but the gateway refuses it.
pub async fn sign_up_with(
    host: &Host,
    invite: &str,
    username: &str,
    tag: &str,
    owe_factor: bool,
) -> Person {
    let root = repo_root().join(format!(
        "target/t2-test-client-{tag}-{username}-{}",
        std::process::id()
    ));
    let _ = std::fs::remove_dir_all(&root);
    let mut instances = Instances::load(&root).unwrap();
    let base = url::Url::parse(&host.public).unwrap();
    let cfg = instances.add(base.clone()).unwrap();
    let kc = Arc::new(MemoryKeychain::new());
    let mut core = open_store(&cfg, kc.as_ref()).expect("store");

    let anon = ReqwestHttp::new(base.clone(), AuthToken::anonymous()).unwrap();
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
    // The instance's clock, not this machine's: the device list entry's `added_at` is compared with times the
    // instance records (a removal's acceptance), and the testhost's clock is its own.
    let now = host_now(host).await;
    core.signup_complete(&user_id, username, now, &signer)
        .expect("signup complete");

    let authed = ReqwestHttp::new(base.clone(), AuthToken::new(token.as_str())).unwrap();
    authed
        .call::<r::PutDeviceList>(&[&hex(&user_id)], Raw(core.device_list_body().unwrap()))
        .await
        .expect("device list");
    core.device_list_published().unwrap();
    drop(core);
    let person = Person {
        cfg,
        kc,
        user_id,
        device_id,
        token,
        _root: TempRoot(root),
    };
    if !owe_factor {
        enrol_totp(host, &person).await;
    }
    person
}

pub async fn sign_up(host: &Host, invite: &str, username: &str, tag: &str) -> Person {
    sign_up_with(host, invite, username, tag, false).await
}

/// An authenticator app, enrolled and confirmed with the host's clock over the sign-up session.
pub async fn enrol_totp(host: &Host, p: &Person) {
    let authed = authed(host, p);
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
    let code = totp_code(&secret, host_now(host).await / 30);
    authed
        .call::<r::TotpConfirm>(
            &[],
            Raw(cbor(|e| {
                e.array(1).text(&code);
            })),
        )
        .await
        .expect("totp confirm");
}

/// HTTP on the person's sign-up session.
pub fn authed(host: &Host, p: &Person) -> ReqwestHttp {
    ReqwestHttp::new(
        url::Url::parse(&host.public).unwrap(),
        AuthToken::new(p.token.as_str()),
    )
    .unwrap()
}

/// Forgets the sign-up session in the store, so the client establishes its own from the device key.
pub fn clear_session(p: &Person) {
    let mut core = open_store(&p.cfg, p.kc.as_ref()).unwrap();
    core.session_clear().unwrap();
}

/// A community with one text channel; `b` joins it with an invite. Answers the community id.
pub fn community(host: &Host, a: &Person, b: &Person) -> [u8; 16] {
    use dilla_core::ids::DeviceId;
    use dilla_testkit::HttpDs;
    let mut ds = HttpDs::with_session(
        &host.public,
        DeviceId::from_bytes(a.device_id),
        a.token.clone(),
    )
    .unwrap();
    let c = ds.create_community("gpui").unwrap();
    ds.create_text_channel(&c, "general").unwrap();
    let code = ds.create_invite(&c, 10, 3600).unwrap();
    drop(ds);
    let mut ds = HttpDs::with_session(
        &host.public,
        DeviceId::from_bytes(b.device_id),
        b.token.clone(),
    )
    .unwrap();
    ds.join_community(&c, &code).unwrap();
    c
}

/// Two people in one community, signed up, set up and with their sessions cleared.
pub async fn two_people(tag: &str) -> (Host, Person, Person, [u8; 16]) {
    let host = tokio::task::spawn_blocking({
        let tag = tag.to_owned();
        move || start_host(&tag)
    })
    .await
    .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let a = sign_up(&host, &invite, "alice", tag).await;
    let b = sign_up(&host, &invite, "bob", tag).await;
    let (host, a, b, c) = tokio::task::spawn_blocking(move || {
        let c = community(&host, &a, &b);
        clear_session(&a);
        clear_session(&b);
        (host, a, b, c)
    })
    .await
    .unwrap();
    (host, a, b, c)
}

/// `Client::start` blocks on its own runtime, so it is called off the test's.
pub async fn start(p: &Person) -> Client {
    let (cfg, kc) = (p.cfg.clone(), p.kc.clone());
    tokio::task::spawn_blocking(move || Client::start(cfg, kc).expect("client starts"))
        .await
        .unwrap()
}

/// Stops a client off the test's runtime (its drop shuts its own runtime down) and waits a moment for its socket to go.
pub async fn stop(client: Client) {
    tokio::task::spawn_blocking(move || client.stop())
        .await
        .unwrap();
    tokio::time::sleep(Duration::from_millis(300)).await;
}

/// Waits (at most `within`) until `pred` holds for the client's state, answering that state.
pub async fn wait_for(
    client: &Client,
    within: Duration,
    what: &str,
    pred: impl Fn(&AppState) -> bool,
) -> Arc<AppState> {
    let mut rx = client.state();
    let deadline = Instant::now() + within;
    loop {
        {
            let s = rx.borrow_and_update().clone();
            if pred(&s) {
                return s;
            }
        }
        let left = deadline.saturating_duration_since(Instant::now());
        assert!(
            !left.is_zero(),
            "timed out waiting for {what}: {:#?}",
            rx.borrow().clone()
        );
        let _ = tokio::time::timeout(left, rx.changed()).await;
    }
}

/// Polls `check` every 200 ms until it answers `Some`, at most `within`.
pub async fn poll<T, F, Fut>(within: Duration, what: &str, mut check: F) -> T
where
    F: FnMut() -> Fut,
    Fut: std::future::Future<Output = Option<T>>,
{
    let deadline = Instant::now() + within;
    loop {
        if let Some(v) = check().await {
            return v;
        }
        assert!(Instant::now() < deadline, "timed out waiting for {what}");
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
}

pub async fn general(client: &Client, community: [u8; 16]) -> ChannelId {
    let s = wait_for(client, Duration::from_secs(30), "the channel list", |s| {
        s.channel_named(dilla_client::state::CommunityId(community), "general")
            .is_some()
    })
    .await;
    s.channel_named(dilla_client::state::CommunityId(community), "general")
        .unwrap()
        .id
}

/// Both clients hold the channel's group: `a` registers it, `b` joins it.
pub async fn both_in(a: &Client, b: &Client, channel: ChannelId) {
    a.open_channel(channel)
        .await
        .unwrap()
        .expect("a opens the channel");
    b.open_channel(channel)
        .await
        .unwrap()
        .expect("b joins the channel");
}

pub fn bodies_from(s: &AppState, channel: ChannelId, author: [u8; 16]) -> Vec<String> {
    s.timelines
        .get(&channel)
        .map(|t| {
            t.items
                .iter()
                .filter(|i| {
                    i.state == TimelineItemState::Ok && i.sender_user == Some(UserId(author))
                })
                .map(|i| i.body.clone())
                .collect()
        })
        .unwrap_or_default()
}

/// The device's own KeyPackage count (`GET /v1/keypackages`), over its sign-up session.
pub async fn key_package_count(host: &Host, p: &Person) -> u64 {
    let reply = authed(host, p)
        .call::<r::GetKeyPackageCount>(&[], NoBody)
        .await
        .expect("key package count");
    assert_eq!(reply.status, 200);
    decode_strict(&reply.body, |d| {
        let len = d.array_len()?;
        let n = d.uint()?;
        for _ in 1..len {
            d.skip()?;
        }
        Ok(n)
    })
    .unwrap()
}

/// `GET /v1/instance/limits`: `(keypackages_per_device, keypackage_refill_threshold)`.
pub async fn key_package_limits(host: &Host) -> (u64, u64) {
    let anon = ReqwestHttp::new(
        url::Url::parse(&host.public).unwrap(),
        AuthToken::anonymous(),
    )
    .unwrap();
    let Raw(raw) = anon
        .call::<r::GetLimits>(&[], NoBody)
        .await
        .expect("limits");
    decode_strict(&raw, |d| {
        let len = d.array_len()?;
        let mut v = Vec::new();
        for _ in 0..len {
            match d.uint() {
                Ok(n) => v.push(n),
                Err(_) => v.push(0),
            }
        }
        Ok((v[4], v[5]))
    })
    .unwrap()
}

/// A gateway connection on `p`'s sign-up session, for the routes the instance serves only as gateway requests.
pub struct Socket {
    gateway: Arc<WsGateway>,
    requests: Arc<SocketRequests>,
    pump: tokio::task::JoinHandle<()>,
}

impl Socket {
    pub async fn open(host: &Host, p: &Person) -> Socket {
        let gateway = Arc::new(WsGateway::connect_with(
            url::Url::parse(&host.public).unwrap(),
            AuthToken::new(p.token.as_str()),
            GatewayOptions::default(),
        ));
        let requests = SocketRequests::new();
        requests.attach(Some(gateway.clone()));
        requests.on_starting();
        let mut events = gateway.events();
        let r = requests.clone();
        let pump = tokio::spawn(async move {
            while let Some(event) = events.recv().await {
                match event {
                    GatewayEvent::Connected { .. } => r.on_ready("test".into()),
                    GatewayEvent::Frame(bytes) if is_response_frame_head(&bytes) => {
                        r.on_response(&bytes)
                    }
                    GatewayEvent::Frame(_) => {}
                    GatewayEvent::Disconnected { .. } => r.on_down(),
                }
            }
        });
        Socket {
            gateway,
            requests,
            pump,
        }
    }

    /// One request, answering `(status, body)`.
    pub async fn request(
        &self,
        method: &'static str,
        path: String,
        body: Vec<u8>,
    ) -> (u16, Vec<u8>) {
        let idem = (method != "GET").then(|| {
            let mut b = vec![0u8; 16];
            getrandom::fill(&mut b).unwrap();
            b
        });
        let answer = tokio::time::timeout(
            Duration::from_secs(30),
            self.requests.request(method, path, body, idem),
        )
        .await
        .expect("the socket request answered")
        .expect("the socket request");
        (answer.status, answer.body)
    }
}

impl Drop for Socket {
    fn drop(&mut self) {
        self.gateway.stop();
        self.requests.close();
        self.pump.abort();
    }
}
