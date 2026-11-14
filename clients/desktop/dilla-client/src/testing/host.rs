//! A real dillad (dilla-testhost) with accounts made the way the app makes them (feature `testing-host`). The sign-up
//! recipe is signup.ts `submit` from the client core's own identity (the bootstrap invite), the device list published, an
//! authenticator app enrolled over the real `/v1/auth/totp/*` routes and the sealed backups uploaded. Each person also
//! keeps the recovery key their sign-up showed, so a new device can sign in. Taken out of `tests/sync.rs`,
//! `examples/peer.rs` and the desktop's `tests/support/world.rs`, which now all use this one.
//!
//! Needs `target/dilla-testhost` (`go build -o target/dilla-testhost ./cmd/dilla-testhost`, or `DILLA_TESTHOST`) and the
//! WASI core (`DILLA_CORE_WASI`, else `target/wasm32-wasip1/release/dilla_core_wasi.wasm`).

use std::io::{BufRead, BufReader};
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::{Duration, Instant};

use crate::Client;
use crate::http::routes::{self as r, NoBody, Raw};
use crate::http::{AuthToken, ReqwestHttp};
use crate::instances::Instances;
use crate::keychain::MemoryKeychain;
use crate::seams::{Http, InstanceConfig};
use crate::state::AppState;
use crate::store::open_store;
use dilla_core::cbor::{Encoder, decode_strict};

pub mod bot;
pub mod music;

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
    /// The Bot side of a [`start_bot_host`] host.
    bot: Option<bot::BotSide>,
}

impl Drop for Host {
    fn drop(&mut self) {
        if let Some(b) = &self.bot {
            // The Bot stops first, while its instance still serves (dilla-testhost's own order): a killed host would
            // leave its supervised child behind.
            b.stop_on_drop(&self.control);
        }
        let _ = self.child.kill();
        let _ = self.child.wait();
        let _ = std::fs::remove_dir_all(&self.data);
        if self.bot.is_some() {
            let _ = std::fs::remove_dir_all(bot::socket_dir(&self.data));
        }
    }
}

/// Starts a test host on free ports. `web_root`: the built web client the host serves (the parity world), else none.
/// A host that serves the web client is the parity world's, so it also runs in the host's parity-fixture mode
/// (`-parity-fixture`: GET /v1/instance carries the marker that makes the web client's diagnostics deterministic).
pub fn start_host(tag: &str, listen: &str, web_root: Option<&Path>) -> Host {
    launch(tag, listen, web_root, false)
}

/// [`start_host`] with dilla's real Bot supervised beside the instance (`-bot`, see [`bot`]): the Bot is started and
/// healthy when this answers, and the host's fake clock follows the wall clock. Blocking.
pub fn start_bot_host(tag: &str, listen: &str) -> Host {
    let host = launch(tag, listen, None, true);
    host.start_bot();
    host
}

/// Task 28 wave 2 (lane C): [`start_bot_host`] whose Bot plays music into calls, as e2e/voice/bot-music.spec.ts starts
/// its host (`botRuntime`, `advanceClockWithWall`, an SFU with a 90 s departure timeout): dilla-testhost's in-process
/// LiveKit SFU, reached by the Bot and by each device's media on the host's own listener (`ws://<listen>`, the `-web-root`
/// rule), and the music fixtures ([`music::write_fixtures`]) copied into the Bot's state directory before it starts, where
/// a `dilla_testhost` Bot plays them through the same ffmpeg as a real track. Blocking.
pub fn start_music_host(tag: &str) -> Host {
    let listen = format!("127.0.0.1:{}", music::free_port());
    let host = launch_with(tag, &listen, None, true, true);
    let fixtures = music::write_fixtures(&repo_root().join("target/dilla-bot-music-fixtures"));
    host.copy_music_fixtures(&fixtures);
    host.start_bot();
    host
}

fn launch(tag: &str, listen: &str, web_root: Option<&Path>, with_bot: bool) -> Host {
    launch_with(tag, listen, web_root, with_bot, false)
}

fn launch_with(
    tag: &str,
    listen: &str,
    web_root: Option<&Path>,
    with_bot: bool,
    sfu: bool,
) -> Host {
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
    let data = if with_bot {
        // The Bot's socket is `<data-dir>-bot/gateway.sock`: a Unix socket path holds 107 bytes, so it lives under a
        // short root (host.ts `DILLA_E2E_HOST_DATA_ROOT`, default /tmp/dw), not under a worktree's target/.
        bot::data_dir(tag)
    } else {
        root.join(format!(
            "target/slice-test-host-{tag}-{}",
            std::process::id()
        ))
    };
    let _ = std::fs::remove_dir_all(&data);
    std::fs::create_dir_all(&data).unwrap();
    #[cfg(unix)]
    if with_bot {
        // Private from the start (t28-bot-review M4), not only once the Bot starts.
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&data, std::fs::Permissions::from_mode(0o700)).unwrap();
    }
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
        cmd.arg("-web-root").arg(web).arg("-parity-fixture");
    }
    if sfu {
        music::sfu_args(&mut cmd, &data);
    }
    let bot_state = with_bot.then(|| {
        let (args, state) = bot::args(&data, &wasm);
        cmd.args(args);
        state
    });
    let mut child = cmd
        .stdout(std::process::Stdio::piped())
        .stderr(if with_bot {
            std::process::Stdio::piped()
        } else {
            std::process::Stdio::inherit()
        })
        .spawn()
        .expect("start dilla-testhost");
    let bot = bot_state.map(|state| bot::watch(child.stderr.take().unwrap(), state));
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
    let control = control.unwrap();
    if let Some(b) = &bot {
        bot::follow_wall_clock(control.clone(), b.stop_clock());
    }
    Host {
        child,
        public: public.unwrap(),
        control,
        invites: invites.unwrap(),
        data,
        bot,
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
    /// The data root to delete with the person; `None` when the caller owns it ([`sign_up_at`]).
    cleanup: Option<PathBuf>,
}

impl Drop for Person {
    fn drop(&mut self) {
        if let Some(root) = &self.cleanup {
            let _ = std::fs::remove_dir_all(root);
        }
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

/// [`sign_up_at`] on a data root of its own under `target/`, deleted with the person.
pub async fn sign_up(
    host: &Host,
    invite: &str,
    username: &str,
    display: &str,
    tag: &str,
) -> Person {
    let root = repo_root().join(format!(
        "target/testing-client-{tag}-{username}-{}",
        std::process::id()
    ));
    let _ = std::fs::remove_dir_all(&root);
    let mut person = sign_up_at(
        &host.public,
        &host.control,
        &root,
        invite,
        username,
        display,
    )
    .await;
    person.cleanup = Some(root);
    person
}

/// signup.ts `submit` from a fresh client core, then the device list, an authenticator app and the sealed backups.
/// `root` is the instance data root (the caller's to delete); `public` and `control` are the host's listeners. A
/// failing step panics with its name: this is test tooling.
pub async fn sign_up_at(
    public: &str,
    control: &str,
    root: &Path,
    invite: &str,
    username: &str,
    display: &str,
) -> Person {
    sign_up_with(public, control, root, invite, username, display, true).await
}

/// [`sign_up`], except that the store is never told its device list was published: the PUT landed on the instance but
/// its answer was lost, as a crash or a dropped connection right after it leaves a device.
pub async fn sign_up_answer_lost(
    host: &Host,
    invite: &str,
    username: &str,
    display: &str,
    tag: &str,
) -> Person {
    let root = repo_root().join(format!(
        "target/testing-client-{tag}-{username}-{}",
        std::process::id()
    ));
    let _ = std::fs::remove_dir_all(&root);
    let mut person = sign_up_with(
        &host.public,
        &host.control,
        &root,
        invite,
        username,
        display,
        false,
    )
    .await;
    person.cleanup = Some(root);
    person
}

async fn sign_up_with(
    public: &str,
    control: &str,
    root: &Path,
    invite: &str,
    username: &str,
    display: &str,
    list_answered: bool,
) -> Person {
    let mut instances = Instances::load(root).unwrap();
    let base = url::Url::parse(public).unwrap();
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
    if list_answered {
        core.device_list_published().unwrap();
    }
    // An authenticator app, confirmed with the host's clock.
    let Raw(enrol) = authed
        .call::<r::TotpEnroll>(
            &[],
            r::Secret(zeroize::Zeroizing::new(cbor(|e| {
                e.array(1).null();
            }))),
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
    let control = control.to_owned();
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
            r::Secret(zeroize::Zeroizing::new(cbor(|e| {
                e.array(1).text(&code);
            }))),
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
        cleanup: None,
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
    let placed: Vec<PlacedChannel<'_>> = channels.iter().map(|c| (*c, None)).collect();
    community_placed(host, owner, name, members, &placed)
}

/// A channel to make at a position of its own (`None`: the instance's default).
pub type PlacedChannel<'a> = (ChannelSpec<'a>, Option<u64>);

/// [`community_with`], each channel at its position: a screen fixture's `position`s, which the web client orders by.
pub fn community_placed(
    host: &Host,
    owner: &Person,
    name: &str,
    members: &[&Person],
    channels: &[PlacedChannel<'_>],
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
    for ((kind, channel, parent), position) in channels {
        let parent = parent.map(|parent| {
            made.iter()
                .find(|(n, _)| *n == parent)
                .expect("the category is made first")
                .1
        });
        let (id, _, _) = match (parent, position) {
            (parent, Some(at)) => ds
                .create_channel_at(&c, *kind, channel, parent.as_ref(), *at)
                .unwrap(),
            (Some(parent), None) => ds.create_channel_in(&c, *kind, channel, &parent).unwrap(),
            (None, None) => ds.create_channel(&c, *kind, channel).unwrap(),
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

/// A new device of `person`'s account, signed in as the app does on a fresh install (password, authenticator code,
/// recovery key), on a data root of its own under `target/` (named by `tag`), and started. Answers the client and the
/// data root (the caller's to delete).
pub async fn second_device(host: &Host, person: &mut Person, tag: &str) -> (Client, PathBuf) {
    use crate::signin::{Owed, SignIn};
    let root = repo_root().join(format!(
        "target/testing-client-{tag}-{}-second-{}",
        person.username,
        std::process::id()
    ));
    let _ = std::fs::remove_dir_all(&root);
    let cfg = Instances::load(&root)
        .expect("an instance list")
        .add(person.cfg.server.clone())
        .expect("an instance");
    let kc = Arc::new(MemoryKeychain::new());
    let mut sign_in = SignIn::new(cfg.clone(), kc.clone()).expect("a sign-in");
    let owed = sign_in
        .login(&person.username, PASSWORD)
        .await
        .expect("the password");
    assert_eq!(owed, Owed::Code);
    let code = person.next_code(host);
    sign_in.code(&code).await.expect("the code");
    sign_in
        .add_device(&person.recovery_key)
        .await
        .expect("the recovery key");
    drop(sign_in);
    let client =
        tokio::task::spawn_blocking(move || Client::start(cfg, kc).expect("the device starts"))
            .await
            .unwrap();
    (client, root)
}

/// [`second_device`]'s sign-in with its own list PUT failing as `fault` says, not started: the key step answers that the
/// start must finish it. Answers the device's instance, keychain and data root (the caller's to delete).
pub async fn second_device_faulted(
    host: &Host,
    person: &mut Person,
    tag: &str,
    fault: crate::signin::ListPutFault,
) -> (InstanceConfig, Arc<MemoryKeychain>, PathBuf) {
    use crate::signin::{Enrolled, Owed, SignIn};
    let root = repo_root().join(format!(
        "target/testing-client-{tag}-{}-faulted-{}",
        person.username,
        std::process::id()
    ));
    let _ = std::fs::remove_dir_all(&root);
    let cfg = Instances::load(&root)
        .expect("an instance list")
        .add(person.cfg.server.clone())
        .expect("an instance");
    let kc = Arc::new(MemoryKeychain::new());
    let mut sign_in = SignIn::new(cfg.clone(), kc.clone()).expect("a sign-in");
    sign_in.fault_list_put(fault);
    let owed = sign_in
        .login(&person.username, PASSWORD)
        .await
        .expect("the password");
    assert_eq!(owed, Owed::Code);
    let code = person.next_code(host);
    sign_in.code(&code).await.expect("the code");
    let enrolled = sign_in
        .add_device(&person.recovery_key)
        .await
        .expect("the recovery key");
    assert!(
        matches!(enrolled, Enrolled::FinishAtStart(_)),
        "the faulted list PUT leaves the start to finish: {enrolled:?}"
    );
    drop(sign_in);
    (cfg, kc, root)
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
