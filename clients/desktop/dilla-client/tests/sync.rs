//! The sync engine against a real dillad (dilla-testhost): two Rust clients in one channel exchange messages, a client
//! that was away catches up exactly once and in order, and unread counts follow what arrives and what is read.
//!
//! Needs `target/dilla-testhost` (`go build -o target/dilla-testhost ./cmd/dilla-testhost`, or `DILLA_TESTHOST`) and
//! the WASI core (`DILLA_CORE_WASI`, else `target/wasm32-wasip1/release/dilla_core_wasi.wasm`); the tests are
//! `#[ignore]`d so a plain `cargo test` needs no Go toolchain; the desktop-gpui CI job runs them on Linux with
//! `cargo test -p dilla-client --test sync -- --ignored`.
//!
//! Every account is made the way the app makes one: a sign-up from the client core's own identity (the instance's
//! bootstrap invite), its device list published, an authenticator app enrolled over the real `/v1/auth/totp/*` routes
//! (every account owes a second factor before a session opens the gateway). The community, its channel and the
//! invite are set up through the testkit's `HttpDs` on the sign-up session; the stored session is then cleared, so each
//! `Client` establishes its own from the device key.

use std::io::{BufRead, BufReader};
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::{Duration, Instant};

use dilla_client::Client;
use dilla_client::http::routes::{self as r, NoBody, Raw};
use dilla_client::http::{AuthToken, ReqwestHttp};
use dilla_client::instances::Instances;
use dilla_client::keychain::MemoryKeychain;
use dilla_client::seams::{Http, InstanceConfig};
use dilla_client::state::{AppState, ChannelId, TimelineItemState, UserId};
use dilla_client::store::open_store;
use dilla_core::cbor::{Encoder, decode_strict};

fn repo_root() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../..")
        .canonicalize()
        .expect("repo root")
}

struct Host {
    child: std::process::Child,
    public: String,
    control: String,
    invites: Vec<String>,
    data: PathBuf,
}

impl Drop for Host {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
        let _ = std::fs::remove_dir_all(&self.data);
    }
}

fn start_host(tag: &str) -> Host {
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
        "target/sync-test-host-{tag}-{}",
        std::process::id()
    ));
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

fn hex(b: &[u8]) -> String {
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
struct Person {
    cfg: InstanceConfig,
    kc: Arc<MemoryKeychain>,
    user_id: [u8; 16],
    device_id: [u8; 16],
    token: String,
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
async fn sign_up(host: &Host, invite: &str, username: &str, tag: &str) -> Person {
    let root = repo_root().join(format!(
        "target/sync-test-client-{tag}-{username}-{}",
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
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_secs();
    core.signup_complete(&user_id, username, now, &signer)
        .expect("signup complete");

    let authed = ReqwestHttp::new(base.clone(), AuthToken::new(token.as_str())).unwrap();
    authed
        .call::<r::PutDeviceList>(&[&hex(&user_id)], Raw(core.device_list_body().unwrap()))
        .await
        .expect("device list");
    core.device_list_published().unwrap();

    // An authenticator app, confirmed with the host's clock.
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
    let clock: serde_json::Value = serde_json::from_slice(
        &reqwest::get(format!("{}/debug/clock", host.control))
            .await
            .unwrap()
            .bytes()
            .await
            .unwrap(),
    )
    .unwrap();
    let code = totp_code(&secret, clock["now"].as_u64().unwrap() / 30);
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
fn clear_session(p: &Person) {
    let mut core = open_store(&p.cfg, p.kc.as_ref()).unwrap();
    core.session_clear().unwrap();
}

/// A community with one text channel; `b` joins it with an invite. Answers the community id.
fn community(host: &Host, a: &Person, b: &Person) -> [u8; 16] {
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
async fn two_people(tag: &str) -> (Host, Person, Person, [u8; 16]) {
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
async fn start(p: &Person) -> Client {
    let (cfg, kc) = (p.cfg.clone(), p.kc.clone());
    tokio::task::spawn_blocking(move || Client::start(cfg, kc).expect("client starts"))
        .await
        .unwrap()
}

/// Waits (at most `within`) until `pred` holds for the client's state, answering that state.
async fn wait_for(
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

async fn general(client: &Client, community: [u8; 16]) -> ChannelId {
    let s = wait_for(client, Duration::from_secs(30), "the channel list", |s| {
        s.channel_named(dilla_client::state::CommunityId(community), "general")
            .is_some()
    })
    .await;
    s.channel_named(dilla_client::state::CommunityId(community), "general")
        .unwrap()
        .id
}

/// Both clients hold the channel's group: `a` registers it, `b` joins it, and `a` has applied `b`'s join.
async fn both_in(a: &Client, b: &Client, channel: ChannelId) {
    a.open_channel(channel)
        .await
        .unwrap()
        .expect("a opens the channel");
    b.open_channel(channel)
        .await
        .unwrap()
        .expect("b joins the channel");
}

fn bodies_from(s: &AppState, channel: ChannelId, author: [u8; 16]) -> Vec<String> {
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

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn two_devices_exchange_text() {
    let (_host, pa, pb, c) = two_people("exchange").await;
    let a = start(&pa).await;
    let b = start(&pb).await;
    let channel = general(&a, c).await;
    general(&b, c).await;
    both_in(&a, &b, channel).await;

    a.send_text(channel, "hello from gpui".into())
        .await
        .unwrap()
        .expect("queued");
    let sent = Instant::now();
    let s = wait_for(&b, Duration::from_secs(5), "b to show a's message", |s| {
        bodies_from(s, channel, pa.user_id) == ["hello from gpui"]
    })
    .await;
    assert!(sent.elapsed() <= Duration::from_secs(5));
    let item = s.timelines[&channel]
        .items
        .iter()
        .find(|i| i.body == "hello from gpui")
        .unwrap();
    assert_eq!(
        item.sender_user,
        Some(UserId(pa.user_id)),
        "a is the author"
    );
    assert!(!item.own);
    // And a sees its own message confirmed (stored, with a seq), once.
    wait_for(
        &a,
        Duration::from_secs(5),
        "a's message to be stored",
        |s| {
            s.timelines.get(&channel).is_some_and(|t| {
                t.items
                    .iter()
                    .filter(|i| i.body == "hello from gpui")
                    .count()
                    == 1
                    && t.items
                        .iter()
                        .any(|i| i.body == "hello from gpui" && i.own && i.seq.is_some())
            })
        },
    )
    .await;
    // The way back: b answers, a reads it.
    b.send_text(channel, "hello from b".into())
        .await
        .unwrap()
        .expect("queued");
    wait_for(&a, Duration::from_secs(5), "a to show b's message", |s| {
        s.timelines
            .get(&channel)
            .is_some_and(|t| t.items.iter().any(|i| i.body == "hello from b" && !i.own))
    })
    .await;
}

/// Controller ruling: a session the instance stops taking (here: expired on its clock) is re-established from the
/// device key and the gateway started again on it; the account is not signed out.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn expired_session_reauthenticates() {
    use dilla_client::state::{AccountPhase, ConnectionStatus};
    let (host, pa, pb, c) = two_people("reauth").await;
    let a = start(&pa).await;
    let b = start(&pb).await;
    let channel = general(&a, c).await;
    general(&b, c).await;
    both_in(&a, &b, channel).await;
    tokio::time::sleep(Duration::from_secs(2)).await;
    a.disconnect();
    // Past every session lifetime on the instance's clock.
    let advanced = reqwest::Client::new()
        .post(format!("{}/debug/clock", host.control))
        .header("content-type", "application/json")
        .body(r#"{"seconds":3456000}"#)
        .send()
        .await
        .unwrap();
    assert!(
        advanced.status().is_success(),
        "clock: {}",
        advanced.status()
    );
    a.reconnect();
    let s = wait_for(&a, Duration::from_secs(30), "a online again", |s| {
        s.connection.status == ConnectionStatus::Online || s.account.phase != AccountPhase::Ready
    })
    .await;
    assert_eq!(s.account.phase, AccountPhase::Ready, "not signed out");
    // The new session carries traffic: a's post reaches b.
    a.send_text(channel, "after the renewal".into())
        .await
        .unwrap()
        .expect("queued");
    wait_for(&b, Duration::from_secs(20), "b to read it", |s| {
        bodies_from(s, channel, pa.user_id) == ["after the renewal"]
    })
    .await;
}

/// Fix round 2 (N1): a start that fails, called straight from an async task, answers its error instead of panicking
/// (the runtime it built is shut down in the background, never dropped there). The store is signed up and its session
/// cleared, so the start reaches the network: the server it names does not answer.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn start_offline_from_an_async_task_is_an_error() {
    let (_host, pa, _pb, _c) = two_people("offline").await;
    let mut cfg = pa.cfg.clone();
    cfg.server = url::Url::parse("http://127.0.0.1:9").unwrap();
    let started = Client::start(cfg, pa.kc.clone());
    assert!(
        matches!(started, Err(dilla_client::ClientError::Network(_))),
        "{:?}",
        started.err()
    );
}

/// Fix round 1: the instance's session sweep closes a live connection 4004 (not resumable) whose session no longer
/// resolves: deleted, pruned or past its hard expiry (protocol/02 § Device sessions item 6, internal/gateway/heartbeat.go
/// `sweepSessions`). As controller.ts `onRevokedSocket` does, the client establishes a new session from the device key
/// and carries on; only a device whose challenge is refused would be signed out.
///
/// The session rows are deleted and the clock moved one second (one liveness tick, which runs the sweep) while the
/// connection is up: a clock jump past the expiry itself trips the heartbeat deadline first, and the reconnect then
/// meets 4003 at identify (`expired_session_reauthenticates` covers that path).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn live_expiry_sweep_reauthenticates() {
    use dilla_client::state::{AccountPhase, ConnectionStatus};
    let (host, pa, pb, c) = two_people("sweep").await;
    // Called straight from the async test: start must not panic inside a runtime (fix round 1, ruling 3).
    let a = Client::start(pa.cfg.clone(), pa.kc.clone()).expect("a starts from inside a runtime");
    let b = start(&pb).await;
    let channel = general(&a, c).await;
    general(&b, c).await;
    both_in(&a, &b, channel).await;
    tokio::time::sleep(Duration::from_secs(2)).await;
    assert_eq!(
        a.state().borrow().connection.status,
        ConnectionStatus::Online
    );
    let readies = a.state().borrow().connection.readies;
    // Every session of a's device goes (the sign-up session's token still authorises the delete).
    let base = url::Url::parse(&host.public).unwrap();
    ReqwestHttp::new(base, AuthToken::new(pa.token.as_str()))
        .unwrap()
        .call::<r::DeleteSessions>(&[&hex(&pa.device_id)], NoBody)
        .await
        .expect("sessions deleted");
    // One liveness tick on the instance's clock, with a still connected: its sweep closes the socket 4004.
    let advanced = reqwest::Client::new()
        .post(format!("{}/debug/clock", host.control))
        .header("content-type", "application/json")
        .body(r#"{"seconds":1,"sweep":true}"#)
        .send()
        .await
        .unwrap();
    assert!(
        advanced.status().is_success(),
        "clock: {}",
        advanced.status()
    );
    // The sweep cuts a's socket and a comes back on a renewed session: a new ready (the renewal can be quicker than
    // the published "connecting" in between, so the count is the proof, not that transient state).
    let s = wait_for(
        &a,
        Duration::from_secs(40),
        "a's ready on a renewed session",
        |s| {
            (s.connection.readies > readies && s.connection.status == ConnectionStatus::Online)
                || s.account.phase != AccountPhase::Ready
        },
    )
    .await;
    assert_eq!(s.account.phase, AccountPhase::Ready, "a is not signed out");
    wait_for(&b, Duration::from_secs(40), "b online again", |s| {
        s.connection.status == ConnectionStatus::Online && s.account.phase == AccountPhase::Ready
    })
    .await;
    a.send_text(channel, "after the sweep".into())
        .await
        .unwrap()
        .expect("queued");
    wait_for(&b, Duration::from_secs(20), "b to read it", |s| {
        bodies_from(s, channel, pa.user_id) == ["after the sweep"]
    })
    .await;
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn resume_after_sleep_no_gap_no_dup() {
    let (host, pa, pb, c) = two_people("resume").await;
    let a = start(&pa).await;
    let b = start(&pb).await;
    let channel = general(&a, c).await;
    general(&b, c).await;
    both_in(&a, &b, channel).await;

    // Let b's join settle (its activation catch-up done), so what it gets later comes from the catch-up after its ready.
    tokio::time::sleep(Duration::from_secs(2)).await;
    // b's gateway goes away for 30 s while a sends five messages.
    b.disconnect();
    wait_for(&b, Duration::from_secs(5), "b offline", |s| {
        s.connection.status != dilla_client::state::ConnectionStatus::Online
    })
    .await;
    let want: Vec<String> = (1..=5).map(|i| format!("while you slept {i}")).collect();
    for text in &want {
        a.send_text(channel, text.clone())
            .await
            .unwrap()
            .expect("queued");
    }
    wait_for(&a, Duration::from_secs(10), "a's five stored", |s| {
        s.timelines.get(&channel).is_some_and(|t| {
            want.iter()
                .all(|w| t.items.iter().any(|i| &i.body == w && i.seq.is_some()))
        })
    })
    .await;
    // The instance parks b's connection for its resume window (120 s on its clock) and would hand the parked frames to
    // b's next connection; past the window they are gone, as after a real sleep, and b must catch up on its own.
    let advanced = reqwest::Client::new()
        .post(format!("{}/debug/clock", host.control))
        .header("content-type", "application/json")
        .body(r#"{"seconds":150}"#)
        .send()
        .await
        .unwrap();
    assert!(
        advanced.status().is_success(),
        "clock: {}",
        advanced.status()
    );
    tokio::time::sleep(Duration::from_secs(30)).await;
    assert!(
        bodies_from(&b.state().borrow(), channel, pa.user_id).is_empty(),
        "nothing arrived while b was away"
    );

    b.reconnect();
    let s = wait_for(&b, Duration::from_secs(20), "b to catch up", |s| {
        bodies_from(s, channel, pa.user_id).len() >= 5
    })
    .await;
    // Give any duplicate a moment to show, then check exactly those five, in order.
    tokio::time::sleep(Duration::from_secs(2)).await;
    let got = bodies_from(&b.state().borrow(), channel, pa.user_id);
    assert_eq!(
        got,
        want,
        "exactly the five, in order, no duplicates (first read: {:?})",
        bodies_from(&s, channel, pa.user_id)
    );
    let seqs: Vec<u64> = b.state().borrow().timelines[&channel]
        .items
        .iter()
        .filter(|i| i.sender_user == Some(UserId(pa.user_id)))
        .map(|i| i.seq.unwrap())
        .collect();
    assert!(
        seqs.windows(2).all(|w| w[0] < w[1]),
        "ascending seqs: {seqs:?}"
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn unread_counts() {
    let (_host, pa, pb, c) = two_people("unread").await;
    let a = start(&pa).await;
    let b = start(&pb).await;
    let channel = general(&a, c).await;
    general(&b, c).await;
    both_in(&a, &b, channel).await;
    // b looks at nothing: the channel is unfocused.
    b.focus(None);
    for i in 1..=3 {
        a.send_text(channel, format!("unread {i}"))
            .await
            .unwrap()
            .expect("queued");
    }
    wait_for(&b, Duration::from_secs(10), "three unread", |s| {
        bodies_from(s, channel, pa.user_id).len() == 3 && s.unread_of(channel) == 3
    })
    .await;
    b.mark_read(channel).await.unwrap().expect("marked read");
    wait_for(&b, Duration::from_secs(5), "nothing unread", |s| {
        s.unread_of(channel) == 0
    })
    .await;
}
