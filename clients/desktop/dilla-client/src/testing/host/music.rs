//! Task 28 wave 2 (lane C): the Bot's music in a real call, for the music timing tests (e2e/voice/bot-music.spec.ts).
//!
//! - [`super::start_music_host`]: a Bot host with dilla-testhost's in-process LiveKit SFU, whose Bot finds the music
//!   fixtures ([`write_fixtures`]: a 30 s 440 Hz and a 5 s 660 Hz Ogg Opus file, made with ffmpeg as
//!   e2e/media/support/fixtures.ts `writeMusicFixtures` makes them) in its state directory.
//! - [`Seat`]: one device in a voice room: its client's own join (`Client::voice`), and its media in a `call-peer`
//!   process of its own (dilla-media's example: the frame hook is process-wide), fed every epoch the call group reaches.
//!   It listens only (no microphone), and measures on the receive side: each verified device's decoded audio turning
//!   audible or quiet, stamped with the wall clock as the frame leaves the decoder (`dilla_media::AudioEdge`).
//! - [`command`] / [`answer`]: a Bot command run as the composer runs it (`Bot::run`, then `run_command` with the
//!   channel `features::bot::room::send_channel` picks: a music command goes with the voice room this device is in), and
//!   its private row's final answer.
//!
//! The sender's Enter and the receiver's first audible frame are both on this machine's wall clock, as the web spec
//! takes both from `performance.timeOrigin + now()`.

use std::io::{BufRead, BufReader, Write};
use std::path::{Path, PathBuf};
use std::process::{Child, ChildStdin, Command, Stdio};
use std::sync::mpsc::{Receiver, channel};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use serde_json::Value;

use super::{Host, hex, repo_root};
use crate::Client;
use crate::calls::MediaEpoch;
use crate::features::bot::RunOutcome;
use crate::features::bot::run::{Text, Texts, Zone};
use crate::state::bot::{Conversation, ResultStatus};
use crate::state::{ChannelId, CommunityId, DeviceId};

/// The fixtures' track names (fixtures.ts `MUSIC_FIXTURES`): 11 characters, a valid video id, so a queue takes them.
pub const LONG: &str = "fixture-30s";
pub const SHORT: &str = "fixture-05s";

/// The web voice specs' `WAIT` on CI.
pub const WAIT: Duration = Duration::from_secs(90);

/// e2e/voice/support/voice.ts `AUDIBLE_RMS` (dilla-media's).
pub const AUDIBLE_RMS: f64 = 0.005;

/// The host flags of a music host: the in-process SFU on ports the host picks (`-sfu-any-port`) with bot-music.spec.ts's 90 s departure timeout,
/// and a one-page web root, so the call starts name the host's own listener as the LiveKit URL (dilla-testhost's
/// `-web-root` rule): the Bot and every device's media reach the SFU there.
pub(super) fn sfu_args(cmd: &mut Command, data: &Path) {
    let web = data.join("web");
    std::fs::create_dir_all(&web).unwrap();
    std::fs::write(web.join("index.html"), "<!doctype html>").unwrap();
    // dillad verifies the tree it serves against its manifest.
    std::fs::write(
        web.join("dilla-manifest.json"),
        r#"{"v":1,"files":[{"path":"index.html","sha256":"fe26c59e91ac8de694b2531dc3bdc1b7faf471d3d7e4e00870af60f5f22897cb","size":15}]}"#,
    )
    .unwrap();
    cmd.arg("-web-root").arg(web);
    // Never ports probed here and released: a parallel test can take one before the host binds it.
    cmd.args(["-sfu", "-sfu-any-port"]);
    cmd.args(["-sfu-departure-timeout", "90"]);
}

/// fixtures.ts `writeMusicFixtures`: the 30 s 440 Hz and the 5 s 660 Hz Ogg Opus files in `dir` (48 kHz stereo, 20 ms
/// frames, the format the Bot forwards), made with ffmpeg once (an existing non-empty file is kept). Answers `dir`.
pub fn write_fixtures(dir: &Path) -> PathBuf {
    std::fs::create_dir_all(dir).unwrap();
    for (stem, seconds, hz) in [(LONG, 30, 440), (SHORT, 5, 660)] {
        let path = dir.join(format!("{stem}.opus"));
        if std::fs::metadata(&path).is_ok_and(|m| m.len() > 0) {
            continue;
        }
        let status = Command::new("ffmpeg")
            .args(["-hide_banner", "-loglevel", "error", "-nostdin", "-y"])
            .args(["-f", "lavfi", "-i"])
            .arg(format!(
                "sine=frequency={hz}:sample_rate=48000:duration={seconds}"
            ))
            .args(["-ac", "2", "-c:a", "libopus", "-b:a", "96k"])
            .args(["-frame_duration", "20", "-f", "ogg"])
            .arg(&path)
            .stdin(Stdio::null())
            .status()
            .expect("ffmpeg (with libopus) makes the music fixtures: install ffmpeg");
        assert!(status.success(), "ffmpeg failed on {}", path.display());
    }
    dir.to_owned()
}

impl Host {
    /// Copies the `.opus` files of `fixtures` into the Bot's `<state>/music-fixtures` (bot-music.spec.ts: the Bot's
    /// environment is fixed by bot-launch, so a `dilla_testhost` Bot also looks there). Before the Bot builds its music
    /// backend.
    pub(super) fn copy_music_fixtures(&self, fixtures: &Path) {
        let state = &self.bot.as_ref().expect("a Bot host").state_dir;
        let target = state.join("music-fixtures");
        std::fs::create_dir_all(&target).unwrap();
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            std::fs::set_permissions(&target, std::fs::Permissions::from_mode(0o700)).unwrap();
        }
        for entry in std::fs::read_dir(fixtures).unwrap() {
            let path = entry.unwrap().path();
            if path.extension().is_some_and(|e| e == "opus") {
                std::fs::copy(&path, target.join(path.file_name().unwrap())).unwrap();
            }
        }
    }
}

/// The wall clock in milliseconds since the Unix epoch (what `dilla_media::AudioEdge::at_ms` is in).
pub fn wall_ms() -> f64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_secs_f64()
        * 1000.0
}

/// One quiet/audible change of the call audio a [`Seat`] decodes, as a whole (any verified remote stream audible, as
/// the web spec takes the loudest of its call's analysers).
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct Edge {
    pub t: f64,
    pub audible: bool,
}

/// The build this test binary belongs to: `target/<profile>`.
fn profile_dir() -> PathBuf {
    let exe = std::env::current_exe().unwrap();
    exe.parent().unwrap().parent().unwrap().to_path_buf()
}

/// dilla-media's `call-peer` example, built once per process in this build's profile.
fn call_peer() -> PathBuf {
    static BUILT: std::sync::Once = std::sync::Once::new();
    BUILT.call_once(|| {
        let cargo = std::env::var("CARGO").unwrap_or_else(|_| "cargo".into());
        let mut build = Command::new(cargo);
        build.args(["build", "-p", "dilla-media", "--example", "call-peer"]);
        build.current_dir(repo_root());
        if !cfg!(debug_assertions) {
            build.arg("--release");
        }
        assert!(build.status().unwrap().success(), "cargo build call-peer");
    });
    profile_dir().join("examples/call-peer")
}

/// A `call-peer` process: its stdin, and its stdout's JSON events.
struct Peer {
    child: Mutex<Child>,
    stdin: Mutex<ChildStdin>,
    lines: Mutex<Receiver<Value>>,
    /// The room's end on its own (`{"event":"ended","code"}`), passed on as the app's media session does.
    ended: Mutex<Option<tokio::sync::mpsc::UnboundedReceiver<String>>>,
}

impl Peer {
    fn start(url: &str, token: &str) -> Peer {
        let mut child = Command::new(call_peer())
            .args(["--url", url, "--token", token])
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .spawn()
            .expect("start call-peer");
        let stdin = child.stdin.take().unwrap();
        let stdout = BufReader::new(child.stdout.take().unwrap());
        let (tx, lines) = channel();
        let (ended_tx, ended) = tokio::sync::mpsc::unbounded_channel();
        std::thread::spawn(move || {
            for line in stdout.lines().map_while(Result::ok) {
                let Ok(v) = serde_json::from_str::<Value>(&line) else {
                    continue;
                };
                if v["event"] == "ended" {
                    let _ =
                        ended_tx.send(v["code"].as_str().unwrap_or("E_DISCONNECTED").to_owned());
                }
                if tx.send(v).is_err() {
                    break;
                }
            }
        });
        Peer {
            child: Mutex::new(child),
            stdin: Mutex::new(stdin),
            lines: Mutex::new(lines),
            ended: Mutex::new(Some(ended)),
        }
    }

    /// The room's end on its own, once.
    fn take_ended(&self) -> Option<tokio::sync::mpsc::UnboundedReceiver<String>> {
        self.ended.lock().unwrap().take()
    }

    fn send(&self, line: &str) {
        let mut stdin = self.stdin.lock().unwrap();
        // A peer that already quit takes nothing more.
        let _ = writeln!(stdin, "{line}").and_then(|()| stdin.flush());
    }

    /// The next `event`, skipping the others (the epoch acknowledgements the pump does not wait for).
    fn next(&self, event: &str, timeout: Duration) -> Value {
        let lines = self.lines.lock().unwrap();
        let deadline = Instant::now() + timeout;
        loop {
            let left = deadline.saturating_duration_since(Instant::now());
            match lines.recv_timeout(left) {
                Ok(v) if v["event"] == event => return v,
                Ok(v) => assert_ne!(v["event"], "failed", "the media failed: {v}"),
                Err(_) => panic!("call-peer: no {event} within {timeout:?}"),
            }
        }
    }

    fn request(&self, line: &str, event: &str) -> Value {
        self.send(line);
        self.next(event, Duration::from_secs(10))
    }

    fn quit(&self) {
        self.send("quit");
        let mut child = self.child.lock().unwrap();
        let deadline = Instant::now() + Duration::from_secs(10);
        while Instant::now() < deadline {
            if let Ok(Some(_)) = child.try_wait() {
                return;
            }
            std::thread::sleep(Duration::from_millis(50));
        }
        let _ = child.kill();
        let _ = child.wait();
    }
}

impl Drop for Peer {
    fn drop(&mut self) {
        let child = self.child.get_mut().unwrap();
        if let Ok(None) = child.try_wait() {
            let _ = child.kill();
            let _ = child.wait();
        }
    }
}

fn epoch_line(e: &MediaEpoch) -> String {
    let roster: Vec<String> = e
        .roster
        .iter()
        .map(|(l, d, _)| format!("{l}:{}", hex(d)))
        .collect();
    format!(
        "epoch {} {} {} {}",
        e.epoch,
        hex(&*e.base_key),
        e.own_leaf,
        roster.join(",")
    )
}

/// Runs a blocking step off the async worker.
async fn blocking<T: Send + 'static>(f: impl FnOnce() -> T + Send + 'static) -> T {
    tokio::task::spawn_blocking(f).await.unwrap()
}

/// One device in a voice room: its client (the call signalling) and its media (a listen-only `call-peer`).
pub struct Seat {
    pub client: Client,
    pub device: DeviceId,
    peer: Option<Arc<Peer>>,
    pump: Option<tokio::task::JoinHandle<()>>,
    /// Passes the room's end on to the call (`Voice::lost`), as the app's media session does.
    end: Option<tokio::task::JoinHandle<()>>,
    room: Option<ChannelId>,
}

impl Seat {
    /// A started client, its voice area following the instance; seated nowhere yet.
    pub fn new(client: Client) -> Seat {
        client.voice().start();
        let device = client
            .state()
            .borrow()
            .account
            .device_id
            .expect("a signed-in device");
        Seat {
            client,
            device,
            peer: None,
            pump: None,
            end: None,
            room: None,
        }
    }

    /// The room this device sits in.
    pub fn room(&self) -> Option<ChannelId> {
        self.room
    }

    /// Joins `room` (leaving the room it sits in first, as the app's own leave and join), muted, and connects its
    /// media: the join epoch's keys, then every later epoch the call group reaches (the Bot's join, a newcomer's).
    pub async fn join(&mut self, room: ChannelId) {
        self.stand().await;
        super::wait_for(&self.client, WAIT, "the voice room is listed", |s| {
            s.channel(room).is_some()
        })
        .await;
        // The media binary is built before the join, as call_world's MediaPeer is: a cold build (a CI shard that
        // never built the example) took 90 s mid-join, past the instance's 30 s admission join window, so the sweep
        // removed this device's leaf and the call group froze behind that Remove (the Bot's join then got 425 and
        // never played).
        blocking(call_peer).await;
        let voice = self.client.voice();
        voice.set_muted(true);
        voice.join(room).await.unwrap().expect("the join succeeds");
        let link = voice.media().borrow().clone().expect("a media link");
        assert_eq!(link.channel, room);
        let (url, token) = (link.url.clone(), link.token.to_string());
        let peer = Arc::new(blocking(move || Peer::start(&url, &token)).await);
        let mut epochs = link.epochs.clone();
        let fed = peer.clone();
        let first = epochs.borrow_and_update().clone().expect("the join epoch");
        fed.send(&epoch_line(&first));
        self.pump = Some(tokio::spawn(async move {
            let mut last = first.epoch;
            while epochs.changed().await.is_ok() {
                let e = epochs.borrow_and_update().clone();
                if let Some(e) = e.filter(|e| e.epoch != last) {
                    last = e.epoch;
                    fed.send(&epoch_line(&e));
                }
            }
        }));
        let p = peer.clone();
        blocking(move || p.next("connected", Duration::from_secs(30))).await;
        if let Some(mut ended) = peer.take_ended() {
            let (voice, call) = (self.client.voice(), link.call_id);
            self.end = Some(tokio::spawn(async move {
                if let Some(code) = ended.recv().await {
                    let _ = voice.lost(call, &code).await;
                }
            }));
        }
        self.peer = Some(peer);
        self.room = Some(room);
    }

    /// Leaves the room (its media first, then the call), if it sits in one.
    pub async fn stand(&mut self) {
        if let Some(pump) = self.pump.take() {
            pump.abort();
        }
        if let Some(end) = self.end.take() {
            end.abort();
        }
        if let Some(peer) = self.peer.take() {
            blocking(move || peer.quit()).await;
        }
        if self.room.take().is_some() {
            let _ = self.client.voice().leave().await;
        }
    }

    fn peer(&self) -> Arc<Peer> {
        self.peer.clone().expect("seated")
    }

    /// bot-music.spec.ts `listen`: the measurement starts over (the edges so far are forgotten).
    pub async fn listen(&self) {
        let p = self.peer();
        blocking(move || p.request("listen", "listening")).await;
    }

    /// The quiet/audible edges of the call audio since the last [`Seat::listen`].
    pub async fn edges(&self) -> Vec<Edge> {
        self.edges_raw()
            .await
            .0
            .iter()
            .map(|e| Edge {
                t: e["t"].as_f64().unwrap(),
                audible: e["audible"].as_bool().unwrap(),
            })
            .collect()
    }

    /// The call audio's edges (`heard`) and each stream's own (`edges`), as `call-peer` reports them.
    pub async fn edges_raw(&self) -> (Vec<Value>, Vec<Value>) {
        let p = self.peer();
        let v = blocking(move || p.request("edges", "edges")).await;
        let list = |k: &str| v[k].as_array().cloned().unwrap_or_default();
        (list("heard"), list("edges"))
    }

    /// bot-music.spec.ts `firstAudio`: the first time at or after `after` (wall ms) this device's call audio turned
    /// audible; waits at most [`WAIT`].
    pub async fn first_audio(&self, after: f64) -> f64 {
        let deadline = Instant::now() + WAIT;
        loop {
            if let Some(e) = self
                .edges()
                .await
                .into_iter()
                .find(|e| e.audible && e.t >= after)
            {
                return e.t;
            }
            if Instant::now() >= deadline {
                let p = self.peer();
                let stats = blocking(move || p.request("stats", "stats")).await;
                let state = self.client.state().borrow().clone();
                panic!(
                    "the Bot was not heard within {WAIT:?} of {after}: call {:?}, ring {:?}, media {stats}",
                    state.voice.call, state.ring
                );
            }
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
    }

    /// The loudest remote device's level now (RMS of the last 100 ms), as `audibleLevel` reads the page's call audio.
    pub async fn level(&self) -> f64 {
        let p = self.peer();
        let v = blocking(move || p.request("stats", "stats")).await;
        v["levels"]
            .as_object()
            .map(|o| o.values().filter_map(Value::as_f64).fold(0.0, f64::max))
            .unwrap_or(0.0)
    }

    /// The frequency (Hz, 10 Hz steps) with the most power in the last second of the loudest remote device's audio.
    pub async fn peak_hz(&self) -> f64 {
        let p = self.peer();
        let v = blocking(move || p.request("stats", "stats")).await;
        let loudest = v["levels"].as_object().and_then(|o| {
            o.iter()
                .filter_map(|(d, l)| Some((d.clone(), l.as_f64()?)))
                .max_by(|a, b| a.1.total_cmp(&b.1))
        });
        loudest
            .and_then(|(d, _)| v["peaks"][d.as_str()].as_f64())
            .unwrap_or(0.0)
    }

    /// bot-music.spec.ts `quiet`: waits until this device hears nothing, read from the decoded frames themselves (the
    /// call audio's newest edge is a quiet one, or there is none), not from the 100 ms level, which lags a stop that the
    /// next track follows within one window.
    pub async fn quiet(&self) {
        self.listen().await;
        let deadline = Instant::now() + WAIT;
        loop {
            let edges = self.edges().await;
            if edges.last().is_none_or(|e| !e.audible) {
                return;
            }
            assert!(
                Instant::now() < deadline,
                "never quiet within {WAIT:?}: edges {edges:?}"
            );
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
    }

    /// Waits until this device hears something.
    pub async fn audible(&self) {
        self.level_until("audible", |l| l > AUDIBLE_RMS).await;
    }

    async fn level_until(&self, what: &str, ok: impl Fn(f64) -> bool) {
        let deadline = Instant::now() + WAIT;
        loop {
            let level = self.level().await;
            if ok(level) {
                return;
            }
            assert!(
                Instant::now() < deadline,
                "never {what} within {WAIT:?}: level {level}"
            );
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
    }
}

impl Drop for Seat {
    fn drop(&mut self) {
        if let Some(pump) = self.pump.take() {
            pump.abort();
        }
        if let Some(end) = self.end.take() {
            end.abort();
        }
        if let Some(peer) = self.peer.take() {
            peer.quit();
        }
    }
}

/// bot-music.spec.ts `createCommunity`: a host whose Bot plays music, Ada's server with #general and the voice `rooms`
/// (made before the Bot is enabled, so a first `/play` in each joins a call the Bot was never in), the Bot enabled with
/// Music by Ada, and Bea a member holding a "Music DJ" role with the music bit.
pub struct World {
    pub host: Host,
    pub ada: super::Person,
    pub bea: super::Person,
    pub community: CommunityId,
    /// #general, where the commands are typed.
    pub general: ChannelId,
    /// The voice rooms, in the order asked.
    pub rooms: Vec<ChannelId>,
    /// Ada's first device, started (it enabled the Bot); a test takes it.
    pub ada_client: Option<Client>,
}

impl World {
    pub async fn new(tag: &str, rooms: &[&str]) -> World {
        let host = blocking({
            let tag = tag.to_owned();
            move || super::start_music_host(&tag)
        })
        .await;
        let invite = host.invites.last().unwrap().clone();
        let ada = super::sign_up(&host, &invite, "ada", "Ada", tag).await;
        let bea = super::sign_up(&host, &invite, "bea", "Bea", tag).await;
        let names: Vec<String> = rooms.iter().map(|r| (*r).to_owned()).collect();
        let (host, ada, bea, community) = blocking(move || {
            let specs: Vec<super::ChannelSpec<'_>> =
                names.iter().map(|n| (1, n.as_str(), None)).collect();
            let c = super::community_with(&host, &ada, "Music without lag", &[&bea], &specs);
            super::clear_session(&ada);
            super::clear_session(&bea);
            (host, ada, bea, c)
        })
        .await;
        let ada_client = super::start(&ada).await;
        super::bot::enable_bot(&ada_client, community, super::bot::cap::MUSIC).await;
        let community = CommunityId(community);
        let s = super::wait_for(&ada_client, WAIT, "the rooms", |s| {
            rooms
                .iter()
                .all(|r| s.channel_named(community, r).is_some())
                && s.channel_named(community, "general").is_some()
        })
        .await;
        let general = s.channel_named(community, "general").unwrap().id;
        let rooms = rooms
            .iter()
            .map(|r| s.channel_named(community, r).unwrap().id)
            .collect();
        let roles = ada_client.roles();
        let dj = roles
            .create_role(
                community,
                "Music DJ",
                0,
                false,
                false,
                crate::features::roles::perm::CONTROL_MUSIC,
            )
            .await
            .expect("the DJ role");
        roles
            .grant_role(community, crate::state::UserId(bea.user_id), dj)
            .await
            .expect("Bea is a DJ");
        World {
            host,
            ada,
            bea,
            community,
            general,
            rooms,
            ada_client: Some(ada_client),
        }
    }

    /// A started device of `person` whose Bot catalog is verified (its commands run).
    pub async fn device(&self, person: &super::Person) -> Client {
        let c = super::start(person).await;
        self.ready(&c).await;
        c
    }

    /// Waits until `client` holds the Bot's catalog, verified, and lists #general.
    pub async fn ready(&self, client: &Client) {
        super::bot::wait_for_catalog(client, self.community.0, WAIT).await;
        let general = self.general;
        super::wait_for(client, WAIT, "#general", |s| s.channel(general).is_some()).await;
    }
}

/// The result rows' copy: the tests read the Bot's own answers, so the client's texts are their debug names.
pub fn texts() -> Texts {
    Arc::new(|t: &Text| format!("{t:?}"))
}

fn utc() -> Zone {
    Zone {
        name: "UTC".into(),
        offset_at: Arc::new(|_| 0),
    }
}

/// A Bot command typed in `here` and sent as the composer sends it: parsed (`Bot::run`), then run sealed with the
/// channel the desktop picks (`features::bot::room::send_channel`: a music command goes with the voice room this
/// device is in). Answers the wall time of the send (the Enter) and the private row's id.
pub fn command(
    client: &Client,
    community: CommunityId,
    here: ChannelId,
    draft: &str,
) -> (f64, u64) {
    let conversation = Conversation {
        channel: here,
        thread: None,
    };
    let enter = wall_ms();
    match client.bot().run(conversation, Some(community), draft) {
        RunOutcome::Bot { definition, args } => {
            let send = crate::features::bot::room::send_channel(
                &client.state().borrow(),
                &definition.id,
                here,
            );
            let row = client.bot().run_command(
                conversation,
                community,
                Some(send),
                definition,
                args,
                &utc(),
                texts(),
            );
            (enter, row)
        }
        other => panic!("{draft} is a Bot command here, not {other:?}"),
    }
}

/// bot-music.spec.ts `answer`: waits for the row's final answer and answers its text; it must contain `expected`.
pub async fn answer(client: &Client, here: ChannelId, row: u64, expected: &str) -> String {
    let conversation = Conversation {
        channel: here,
        thread: None,
    };
    let s = super::wait_for(client, WAIT, "the Bot's answer", |s| {
        s.bot
            .results(&conversation)
            .iter()
            .any(|r| r.id == row && r.status != ResultStatus::Pending)
    })
    .await;
    let row = s
        .bot
        .results(&conversation)
        .iter()
        .find(|r| r.id == row)
        .unwrap()
        .clone();
    assert!(
        row.status == ResultStatus::Success && row.text.contains(expected),
        "the Bot answered {row:?}, not {expected:?}"
    );
    row.text
}

/// bot-music.spec.ts `run`: a command and its final answer.
pub async fn run(
    client: &Client,
    community: CommunityId,
    here: ChannelId,
    draft: &str,
    expected: &str,
) -> String {
    let (_, row) = command(client, community, here, draft);
    answer(client, here, row, expected).await
}

/// `/play` of `track`.
pub fn play(track: &str) -> String {
    format!("/play query-or-url:\"{track}\"")
}

/// bot-music.spec.ts `timedPlay`: `/play track` from `sender`, measured from its Enter to `receiver`'s first audible
/// frame, in ms; the answer is "Now playing". The receiver listens afresh first.
pub async fn timed_play(
    sender: &Client,
    community: CommunityId,
    here: ChannelId,
    receiver: &Seat,
    track: &str,
) -> f64 {
    receiver.listen().await;
    let (enter, row) = command(sender, community, here, &play(track));
    let heard = receiver.first_audio(enter).await;
    answer(sender, here, row, "Now playing: ").await;
    let ms = heard - enter;
    eprintln!("G3 run /play {track}: {ms:.0} ms");
    ms
}

/// The percentile of `values` as the web specs take it: the sorted value at `ceil(p/100 * n) - 1`.
pub fn percentile(values: &[f64], p: f64) -> f64 {
    let mut sorted = values.to_vec();
    sorted.sort_by(f64::total_cmp);
    let at = ((p / 100.0) * sorted.len() as f64).ceil() as usize;
    sorted[at.clamp(1, sorted.len()) - 1]
}

/// bot-music.spec.ts `record`: the runs' summary, printed for the log.
pub fn record(name: &str, values: &[f64]) {
    let shown: Vec<String> = values.iter().map(|v| format!("{v:.0}")).collect();
    eprintln!(
        "G3 {name}: runs {} p50 {:.0} p95 {:.0} max {:.0} values [{}]",
        values.len(),
        percentile(values, 50.0),
        percentile(values, 95.0),
        values.iter().copied().fold(f64::MIN, f64::max),
        shown.join(", ")
    );
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn percentiles_are_the_web_specs() {
        let v: Vec<f64> = (1..=10).map(f64::from).collect();
        assert_eq!(percentile(&v, 50.0), 5.0);
        assert_eq!(percentile(&v, 95.0), 10.0);
        assert_eq!(percentile(&[3.0], 95.0), 3.0);
    }
}
