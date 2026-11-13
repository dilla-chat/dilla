//! Task 28: a test host that runs dilla's real Bot (`dilla-testhost -bot`, Bot v2 task 11), as `e2e/web/support/host.ts`
//! runs it for the web specs with `botRuntime: true` and `advanceClockWithWall: true`.
//!
//! The host supervises `target/dillabot` (built with `-tags dilla_testhost`) and its helper `dilla-bot-core` (release) as
//! dillad does, a child on `<data-dir>-bot/gateway.sock` in `dev_same_user` mode, with the operator runtime credential
//! the host wrote (`-bot-runtime-secret`). The Bot is started and stopped through `/debug/bot/…` on the host's control
//! listener; a start waits for its "bot service healthy" line. The host's fake clock follows the wall clock (one tick a
//! second), because the Bot signs and checks times against its own wall clock.
//!
//! Needs, beside the host's own binaries: `target/dillabot` (`go build -tags dilla_testhost -o target/dillabot
//! ./cmd/dillabot`, or `DILLABOT`) and `dilla-bot-core` (`cargo build -p dilla-bot-core --release`, at
//! `$CARGO_TARGET_DIR/release` else `target/release`, or `DILLA_BOT_CORE`).

use std::io::{BufRead, BufReader};
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use super::{Host, repo_root};

/// How long a Bot start may take to log "bot service healthy" (the web specs' `WAIT_MS` on CI).
pub const BOT_START: Duration = Duration::from_secs(90);

/// Capability bits of an installation (internal/api/bot_installations.go `botCap*`).
pub mod cap {
    pub const MUSIC: u64 = 1;
    pub const WELCOME: u64 = 1 << 1;
    pub const ROLES: u64 = 1 << 2;
    pub const ANNOUNCEMENTS: u64 = 1 << 3;
    pub const WARNINGS: u64 = 1 << 4;
    pub const REMOVAL: u64 = 1 << 5;
    pub const KICK: u64 = 1 << 6;
    pub const BAN: u64 = 1 << 7;
    pub const POLLS: u64 = 1 << 8;
    pub const REMINDERS: u64 = 1 << 9;
}

/// The Bot side of a host started with [`super::start_bot_host`].
pub(super) struct BotSide {
    pub(super) state_dir: PathBuf,
    /// "bot service healthy" lines seen in the host's output (the supervised Bot logs there).
    healthy: Arc<AtomicUsize>,
    /// The Bot intake's "bot command refused" lines, for a failing test to print.
    refusals: Arc<Mutex<Vec<String>>>,
    running: AtomicBool,
    /// Stops the wall-clock follower.
    stop_clock: Arc<AtomicBool>,
}

impl Drop for BotSide {
    fn drop(&mut self) {
        self.stop_clock.store(true, Ordering::SeqCst);
    }
}

fn env_or(var: &str, default: PathBuf) -> PathBuf {
    std::env::var_os(var).map_or(default, PathBuf::from)
}

/// The Bot's binaries: the dillabot built with `-tags dilla_testhost`, and the release helper.
pub(super) fn binaries() -> (PathBuf, PathBuf) {
    let root = repo_root();
    let bot = env_or("DILLABOT", root.join("target/dillabot"));
    let target = env_or("CARGO_TARGET_DIR", root.join("target"));
    let target = if target.is_relative() {
        root.join(target)
    } else {
        target
    };
    let helper = env_or("DILLA_BOT_CORE", target.join("release/dilla-bot-core"));
    assert!(
        bot.exists(),
        "{} missing: go build -tags dilla_testhost -o target/dillabot ./cmd/dillabot",
        bot.display()
    );
    assert!(
        helper.exists(),
        "{} missing: cargo build -p dilla-bot-core --release --locked",
        helper.display()
    );
    (bot, helper)
}

/// The host flags of a Bot host (host.ts `hostArgs` with `botRuntime`).
pub(super) fn args(
    data: &std::path::Path,
    wasm: &std::path::Path,
) -> (Vec<std::ffi::OsString>, PathBuf) {
    let (bot, helper) = binaries();
    let state = data.join("bot-state");
    let token = state.join("runtime.token");
    let mut a: Vec<std::ffi::OsString> = Vec::new();
    for (flag, value) in [
        ("-bot-runtime-secret", token.as_os_str()),
        ("-bot-executable", bot.as_os_str()),
        ("-bot-helper", helper.as_os_str()),
        ("-bot-wasi", wasm.as_os_str()),
        ("-bot-state-dir", state.as_os_str()),
    ] {
        a.push(flag.into());
        a.push(value.to_owned());
    }
    a.push("-bot".into());
    (a, state)
}

/// Reads the host's stderr: every line goes on to this process's stderr, and the Bot's health and refusal lines are
/// counted.
pub(super) fn watch(stderr: std::process::ChildStderr, state_dir: PathBuf) -> BotSide {
    let healthy = Arc::new(AtomicUsize::new(0));
    let refusals = Arc::new(Mutex::new(Vec::new()));
    let (h, r) = (healthy.clone(), refusals.clone());
    std::thread::spawn(move || {
        for line in BufReader::new(stderr).lines() {
            let Ok(line) = line else { break };
            if line.contains("bot service healthy") {
                h.fetch_add(1, Ordering::SeqCst);
            }
            if line.contains("bot command refused") {
                r.lock().unwrap().push(line.clone());
            }
            eprintln!("{line}");
        }
    });
    BotSide {
        state_dir,
        healthy,
        refusals,
        running: AtomicBool::new(false),
        stop_clock: Arc::new(AtomicBool::new(false)),
    }
}

/// The wall-clock follower (wall-clock.ts): each second, the whole wall seconds since the last applied one move the
/// host's fake clock on, so a deliberate lead (`advance_clock`) is kept.
pub(super) fn follow_wall_clock(control: String, stop: Arc<AtomicBool>) {
    std::thread::spawn(move || {
        let rt = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap();
        let http = ::reqwest::Client::new();
        let url = format!("{control}/debug/clock");
        let mut through = Instant::now();
        while !stop.load(Ordering::SeqCst) {
            std::thread::sleep(Duration::from_millis(250));
            let seconds = through.elapsed().as_secs();
            if seconds == 0 {
                continue;
            }
            let ok = rt.block_on(async {
                http.post(&url)
                    .header("content-type", "application/json")
                    .body(format!("{{\"seconds\":{seconds}}}"))
                    .send()
                    .await
                    .is_ok_and(|r| r.status().is_success())
            });
            if !ok {
                // The host is gone (or going): nothing more to follow.
                return;
            }
            through += Duration::from_secs(seconds);
        }
    });
}

impl BotSide {
    pub(super) fn stop_clock(&self) -> Arc<AtomicBool> {
        self.stop_clock.clone()
    }

    /// The host is dropped: the clock stops following, and a running Bot is stopped through the control listener.
    pub(super) fn stop_on_drop(&self, control: &str) {
        self.stop_clock.store(true, Ordering::SeqCst);
        if self.running.load(Ordering::SeqCst) {
            let _ = post(&format!("{control}/debug/bot/stop"));
        }
    }
}

/// A fresh data directory for a Bot host: `$DILLA_E2E_HOST_DATA_ROOT/<pid>-<n>` (default root /tmp/dw), short enough for
/// the Bot's socket path; `tag` names it in the host's log line only.
pub(super) fn data_dir(tag: &str) -> PathBuf {
    static NEXT: AtomicUsize = AtomicUsize::new(0);
    let root = env_or("DILLA_E2E_HOST_DATA_ROOT", PathBuf::from("/tmp/dw"));
    std::fs::create_dir_all(&root).unwrap();
    let dir = root.join(format!(
        "{}-{}",
        std::process::id(),
        NEXT.fetch_add(1, Ordering::SeqCst)
    ));
    eprintln!("bot test host {tag}: {}", dir.display());
    dir
}

/// The Bot's socket directory, `<data-dir>-bot` (cmd/dilla-testhost/bot.go `botConfig`).
pub(super) fn socket_dir(data: &std::path::Path) -> PathBuf {
    let mut name = data.file_name().unwrap_or_default().to_owned();
    name.push("-bot");
    data.with_file_name(name)
}

fn post(url: &str) -> u16 {
    let url = url.to_owned();
    std::thread::spawn(move || {
        let rt = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap();
        rt.block_on(async {
            ::reqwest::Client::new()
                .post(url)
                .header("content-type", "application/json")
                .body("{}")
                .send()
                .await
                .map_or(0, |r| r.status().as_u16())
        })
    })
    .join()
    .unwrap()
}

impl Host {
    /// Moves the host's frozen fake clock to the wall clock and keeps it there (one tick a second), as a real instance's
    /// clock runs: for tests that compare the instance's times with the time that passed (a Bot host does this itself).
    /// The follower ends with the host.
    pub fn follow_wall_clock(&self) {
        let wall = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_secs();
        let behind = wall.saturating_sub(super::clock_now(self));
        if behind > 0 {
            super::advance_clock(self, behind);
        }
        follow_wall_clock(self.control.clone(), Arc::new(AtomicBool::new(false)));
    }

    fn bot_side(&self) -> &BotSide {
        self.bot
            .as_ref()
            .expect("a host started with start_bot_host")
    }

    /// Starts the supervised Bot (POST /debug/bot/start) and waits for its "bot service healthy" line. Blocking.
    pub fn start_bot(&self) {
        let b = self.bot_side();
        assert!(
            !b.running.load(Ordering::SeqCst),
            "the host's Bot is already running"
        );
        let token = b.state_dir.join("runtime.token");
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let mode = |p: &std::path::Path, m: u32| {
                std::fs::set_permissions(p, std::fs::Permissions::from_mode(m)).unwrap();
            };
            mode(&b.state_dir, 0o700);
            mode(&self.data, 0o700);
            mode(&token, 0o600);
        }
        let before = b.healthy.load(Ordering::SeqCst);
        assert_eq!(post(&format!("{}/debug/bot/start", self.control)), 204);
        let deadline = Instant::now() + BOT_START;
        while b.healthy.load(Ordering::SeqCst) <= before {
            if Instant::now() > deadline {
                let _ = post(&format!("{}/debug/bot/stop", self.control));
                panic!(
                    "the supervised Bot did not become healthy within {BOT_START:?}; see the host's output"
                );
            }
            std::thread::sleep(Duration::from_millis(100));
        }
        b.running.store(true, Ordering::SeqCst);
    }

    /// Stops the supervised Bot (SIGTERM, then SIGKILL after the supervisor's grace) and waits for it. Blocking.
    pub fn stop_bot(&self) {
        let b = self.bot_side();
        assert_eq!(post(&format!("{}/debug/bot/stop", self.control)), 204);
        b.running.store(false, Ordering::SeqCst);
    }

    /// [`Host::stop_bot`], then [`Host::start_bot`] (host.ts `restartBotRuntime`).
    pub fn restart_bot(&self) {
        self.stop_bot();
        self.start_bot();
    }

    /// The Bot intake's "bot command refused" lines so far (a failing first command names the gate that refused it).
    pub fn bot_refusals(&self) -> Vec<String> {
        self.bot_side().refusals.lock().unwrap().clone()
    }

    /// The operator runtime credential's installation list (GET /debug/bot/installations): each row's community id and
    /// state (3 ready).
    pub fn bot_installations(&self) -> Vec<([u8; 16], u64)> {
        use crate::gateway::frames::Value;
        let token = std::fs::read_to_string(self.bot_side().state_dir.join("runtime.token"))
            .expect("the runtime credential");
        let url = format!("{}/debug/bot/installations", self.control);
        let body = std::thread::spawn(move || {
            let rt = tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build()
                .unwrap();
            rt.block_on(async {
                let r = ::reqwest::Client::new()
                    .get(url)
                    .bearer_auth(token.trim())
                    .header("accept", "application/cbor")
                    .send()
                    .await
                    .unwrap();
                assert!(r.status().is_success(), "installations: {}", r.status());
                r.bytes().await.unwrap().to_vec()
            })
        })
        .join()
        .unwrap();
        let Ok(Value::Array(rows)) = Value::decode(&body) else {
            panic!("installations: not a CBOR array");
        };
        rows.into_iter()
            .map(|row| {
                let Value::Array(f) = row else {
                    panic!("installation row: not an array")
                };
                let community = match &f[1] {
                    Value::Bytes(b) => <[u8; 16]>::try_from(b.as_slice()).unwrap(),
                    other => panic!("installation community: {other:?}"),
                };
                let state = match &f[2] {
                    Value::Uint(s) => *s,
                    other => panic!("installation state: {other:?}"),
                };
                (community, state)
            })
            .collect()
    }
}

/// The owner enables the community's Bot with `capabilities` as Server settings → Bot does (a password step-up, then the
/// configure route), and waits until the instance reads the installation ready (state 3, "admin.bot.status.ready") and
/// this device holds the Bot's catalog with its command key verified against the pinned Bot user. Answers the
/// installation.
pub async fn enable_bot(
    client: &crate::Client,
    community: [u8; 16],
    capabilities: u64,
) -> crate::state::bot_admin::Installation {
    use crate::features::factors::StepUpMethod;
    use crate::state::CommunityId;
    let community = CommunityId(community);
    super::wait_for(client, Duration::from_secs(30), "the community", |s| {
        s.members.contains_key(&community)
    })
    .await;
    let token = client
        .factors()
        .step_up(
            StepUpMethod::Password,
            zeroize::Zeroizing::new(super::PASSWORD.to_owned()),
        )
        .await
        .expect("the password steps up");
    let admin = client.bot_admin();
    admin
        .configure_installation(
            community,
            crate::features::bot_admin::new_request_id(),
            None,
            capabilities,
            token.0,
        )
        .await
        .expect("an answer")
        .expect("the Bot is enabled");
    let deadline = Instant::now() + BOT_START;
    let installation = loop {
        let row = admin
            .refresh_installation(community)
            .await
            .expect("an answer")
            .expect("the installation reads");
        if let Some(i) = row.filter(|i| i.state == 3) {
            break i;
        }
        assert!(
            Instant::now() < deadline,
            "the Bot never became ready: {:?}",
            client
                .state()
                .borrow()
                .bot_admin
                .installation
                .get(&community)
        );
        tokio::time::sleep(Duration::from_secs(1)).await;
    };
    wait_for_catalog(client, community.0, Duration::from_secs(90)).await;
    installation
}

/// Waits until this device holds `community`'s Bot catalog with commands and a verified key, reading it again each second
/// (op 61 pushes it too).
pub async fn wait_for_catalog(client: &crate::Client, community: [u8; 16], within: Duration) {
    let community = crate::state::CommunityId(community);
    let deadline = Instant::now() + within;
    loop {
        let ok = client
            .state()
            .borrow()
            .bot
            .catalogs
            .get(&community)
            .is_some_and(|v| !v.commands.is_empty() && v.fingerprint.is_some());
        if ok {
            return;
        }
        assert!(
            Instant::now() < deadline,
            "no verified Bot catalog: {:?}",
            client.state().borrow().bot.catalogs.get(&community)
        );
        let _ = client.bot().sync_catalog(community).await;
        tokio::time::sleep(Duration::from_secs(1)).await;
    }
}
