//! A real dillad (dilla-testhost) with accounts made the way the app makes them, for the slice test and the parity
//! world (`examples/parity_world.rs`). The same recipe as `dilla-client/tests/sync.rs`: a sign-up from the client core's
//! own identity (the bootstrap invite), the device list published, an authenticator app enrolled over the real
//! `/v1/auth/totp/*` routes. Each person also keeps the recovery key their sign-up showed, so a new device can sign in.
//!
//! Needs `target/dilla-testhost` (`go build -o target/dilla-testhost ./cmd/dilla-testhost`, or `DILLA_TESTHOST`) and the
//! WASI core (`DILLA_CORE_WASI`, else `target/wasm32-wasip1/release/dilla_core_wasi.wasm`).
#![allow(dead_code)]

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
use dilla_client::state::AppState;
use dilla_client::store::open_store;
use dilla_core::cbor::{Encoder, decode_strict};

pub const PASSWORD: &str = "correct horse battery staple";

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

/// Starts a test host on free ports. `web_root`: the built web client the host serves (the parity world), else none.
pub fn start_host(tag: &str, listen: &str, web_root: Option<&Path>) -> Host {
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
        "target/slice-test-host-{tag}-{}",
        std::process::id()
    ));
    let _ = std::fs::remove_dir_all(&data);
    std::fs::create_dir_all(&data).unwrap();
    let mut cmd = std::process::Command::new(bin);
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
    .arg(&data);
    if let Some(web) = web_root {
        cmd.arg("-web-root").arg(web);
    }
    let mut child = cmd
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

/// RFC 6238 (HMAC-SHA1, six digits) for a base32 secret.
pub fn totp_code(secret_b32: &str, counter: u64) -> String {
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

/// The host's fake clock, in unix seconds.
pub fn clock_now(host: &Host) -> u64 {
    let body = reqwest::blocking_get(&format!("{}/debug/clock", host.control));
    let v: serde_json::Value = serde_json::from_slice(&body).unwrap();
    v["now"].as_u64().unwrap()
}

/// Moves the host's fake clock `seconds` forward.
pub fn advance_clock(host: &Host, seconds: u64) {
    reqwest::blocking_post_json(
        &format!("{}/debug/clock", host.control),
        &format!("{{\"seconds\":{seconds}}}"),
    );
}

/// Tiny blocking HTTP helpers for the control listener, on a runtime of their own.
mod reqwest {
    pub fn blocking_get(url: &str) -> Vec<u8> {
        let url = url.to_owned();
        std::thread::spawn(move || {
            let rt = tokio::runtime::Runtime::new().unwrap();
            rt.block_on(async {
                ::reqwest::get(url)
                    .await
                    .unwrap()
                    .bytes()
                    .await
                    .unwrap()
                    .to_vec()
            })
        })
        .join()
        .unwrap()
    }

    pub fn blocking_post_json(url: &str, body: &str) {
        let (url, body) = (url.to_owned(), body.to_owned());
        std::thread::spawn(move || {
            let rt = tokio::runtime::Runtime::new().unwrap();
            rt.block_on(async {
                let status = ::reqwest::Client::new()
                    .post(url)
                    .header("content-type", "application/json")
                    .body(body)
                    .send()
                    .await
                    .unwrap()
                    .status();
                assert!(status.is_success(), "clock post: {status}");
            });
        })
        .join()
        .unwrap();
    }
}

/// One person on one device: the instance's data root, its keychain, and what sign-up answered and showed.
pub struct Person {
    pub username: String,
    pub cfg: InstanceConfig,
    pub kc: Arc<MemoryKeychain>,
    pub user_id: [u8; 16],
    pub device_id: [u8; 16],
    pub token: String,
    pub recovery_key: String,
    pub totp_secret: String,
    /// The last TOTP counter the instance accepted from this account.
    pub totp_last: u64,
    root: PathBuf,
}

impl Drop for Person {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.root);
    }
}

impl Person {
    /// The next code the instance takes from this account, moving the host's clock on when the replay guard needs it.
    pub fn next_code(&mut self, host: &Host) -> String {
        loop {
            let c = clock_now(host) / 30;
            let want = c.saturating_sub(1).max(self.totp_last + 1);
            if want <= c + 1 {
                self.totp_last = want;
                return totp_code(&self.totp_secret, want);
            }
            advance_clock(host, 30);
        }
    }
}

fn cbor(f: impl FnOnce(&mut Encoder)) -> Vec<u8> {
    let mut e = Encoder::new();
    f(&mut e);
    e.into_vec()
}

/// signup.ts `submit` from a fresh client core, then the device list and an authenticator app.
pub async fn sign_up(
    host: &Host,
    invite: &str,
    username: &str,
    display: &str,
    tag: &str,
) -> Person {
    let root = repo_root().join(format!(
        "target/slice-test-client-{tag}-{username}-{}",
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
    let recovery_key = core.signup_begin(&instance_id).expect("signup begin");
    let body = core
        .signup_request(invite, username, display, Some(PASSWORD))
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
    let control = host.control.clone();
    let clock = tokio::task::spawn_blocking(move || {
        let body = reqwest::blocking_get(&format!("{control}/debug/clock"));
        serde_json::from_slice::<serde_json::Value>(&body).unwrap()["now"]
            .as_u64()
            .unwrap()
    })
    .await
    .unwrap();
    let counter = clock / 30;
    let code = totp_code(&secret, counter);
    authed
        .call::<r::TotpConfirm>(
            &[],
            Raw(cbor(|e| {
                e.array(1).text(&code);
            })),
        )
        .await
        .expect("totp confirm");
    // The sealed root and state objects, as the web worker uploads them on its first ready past the factor gate (ensureBackups; a factor owed refuses them): a new
    // device recovers from them.
    let sealed = core.sealed_objects().unwrap();
    let (root_obj, state) = decode_strict(&sealed, |d| {
        d.array(3)?;
        let root = d.bytes()?.to_vec();
        let state = d.bytes()?.to_vec();
        d.skip()?;
        Ok((root, state))
    })
    .expect("sealed objects");
    for (kind, object) in [("0", &root_obj), ("1", &state)] {
        authed
            .call::<r::PutBackup>(
                &[kind],
                Raw(cbor(|e| {
                    e.array(1).bytes(object);
                })),
            )
            .await
            .expect("backup");
    }
    core.state_sealed_uploaded().unwrap();

    drop(core);
    Person {
        username: username.to_owned(),
        cfg,
        kc,
        user_id,
        device_id,
        token,
        recovery_key,
        totp_secret: secret,
        totp_last: counter,
        root,
    }
}

/// Forgets the sign-up session in the store, so the client establishes its own from the device key.
pub fn clear_session(p: &Person) {
    let mut core = open_store(&p.cfg, p.kc.as_ref()).unwrap();
    core.session_clear().unwrap();
}

/// A community named `name` with one text channel, #general, owned by `owner`; every one of `members` joins it.
/// Answers the community id.
pub fn community(host: &Host, owner: &Person, name: &str, members: &[&Person]) -> [u8; 16] {
    community_with(host, owner, name, members, &[])
}

/// A channel to make: its kind (0 text, 1 voice, 2 category), its name and the name of the category it sits in.
pub type ChannelSpec<'a> = (u64, &'a str, Option<&'a str>);

/// [`community`] with more channels after #general, made in order (a category before the channels in it).
pub fn community_with(
    host: &Host,
    owner: &Person,
    name: &str,
    members: &[&Person],
    channels: &[ChannelSpec<'_>],
) -> [u8; 16] {
    use dilla_core::ids::DeviceId;
    use dilla_testkit::HttpDs;
    let mut ds = HttpDs::with_session(
        &host.public,
        DeviceId::from_bytes(owner.device_id),
        owner.token.clone(),
    )
    .unwrap();
    let c = ds.create_community(name).unwrap();
    ds.create_text_channel(&c, "general").unwrap();
    let mut made: Vec<(&str, [u8; 16])> = Vec::new();
    for (kind, channel, parent) in channels {
        let (id, _, _) = match parent {
            Some(parent) => {
                let parent = made
                    .iter()
                    .find(|(n, _)| n == parent)
                    .expect("the category is made first")
                    .1;
                ds.create_channel_in(&c, *kind, channel, &parent).unwrap()
            }
            None => ds.create_channel(&c, *kind, channel).unwrap(),
        };
        made.push((channel, id));
    }
    let code = ds.create_invite(&c, 64, 3600).unwrap();
    drop(ds);
    for m in members {
        let mut ds = HttpDs::with_session(
            &host.public,
            DeviceId::from_bytes(m.device_id),
            m.token.clone(),
        )
        .unwrap();
        ds.join_community(&c, &code).unwrap();
    }
    c
}

/// `Client::start` blocks on its own runtime, so it is called off the caller's.
pub async fn start(p: &Person) -> Client {
    let (cfg, kc) = (p.cfg.clone(), p.kc.clone());
    tokio::task::spawn_blocking(move || Client::start(cfg, kc).expect("client starts"))
        .await
        .unwrap()
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
        assert!(!left.is_zero(), "timed out waiting for {what}");
        let _ = tokio::time::timeout(left, rx.changed()).await;
    }
}
