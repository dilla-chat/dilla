//! The media side of this device's call (session.ts's media half): while `Client::voice().media()` holds a link, a
//! `dilla_media::Call` is in its LiveKit room with the call group's epoch keys, and what the person set (mute, deafen,
//! push to talk) is applied to it. What the media hears goes back into the voice slice: who is speaking, the device
//! notices, the shares that ended; every remote video stream's newest frame is kept here for the stage's tiles.
//!
//! The frame hook is process-wide, so one session runs per process (`dilla_media::MediaError::Busy` otherwise). It runs
//! on a runtime of its own: the client's runtime is the client's.

use std::collections::{BTreeSet, HashMap};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Instant;

use dilla_client::Client;
use dilla_client::features::voice::{CallStatsReport, MediaLink, Voice};
use dilla_client::features::voice_prefs::speaking::SpeakingDetector;
use dilla_client::features::voice_prefs::values::{InputMode, NoisePreference, VoiceValues};
use dilla_client::state::voice::{LocalShare, VoiceNotice};
use dilla_client::state::{AppState, DeviceId};
use dilla_media::apm::ApmFlags;
use dilla_media::noise::NoiseMode;
use dilla_media::{
    Call, CallOptions, CaptureSource, EpochFeed, IceServer, LiveKitToken, MediaEvent, Mic,
    RemoteVideos, ShareKind, ShareOptions,
};
use tokio::sync::{mpsc, watch};
use zeroize::Zeroizing;

/// Turns levels into the set of devices speaking now: one Task 19 `SpeakingDetector` per remote device (speaking.ts: on
/// after `SPEAK_ON_MS` over `SPEAK_RMS`, off after `SPEAK_HOLD_MS` under it). Integration 3: replaces Task 17's
/// stand-in threshold.
pub struct Speaking {
    since: Instant,
    devices: HashMap<String, SpeakingDetector>,
}

impl Default for Speaking {
    fn default() -> Self {
        Self {
            since: Instant::now(),
            devices: HashMap::new(),
        }
    }
}

impl Speaking {
    /// The devices (hex) speaking at `now`, from the newest levels. A device no longer heard is dropped.
    pub fn update(&mut self, levels: &dilla_media::RemoteLevels, now: Instant) -> BTreeSet<String> {
        let ms = now.saturating_duration_since(self.since).as_secs_f64() * 1000.0;
        self.devices.retain(|d, _| levels.contains_key(d));
        levels
            .iter()
            .filter(|(device, rms)| {
                self.devices
                    .entry((*device).clone())
                    .or_default()
                    .update(**rms, ms)
            })
            .map(|(device, _)| device.clone())
            .collect()
    }
}

/// The call's microphone from Task 19's voice values: the chosen input, its noise choice (Automatic is the enhanced
/// suppressor where it runs, Lightweight libwebrtc's), and echo cancellation and gain control as set.
pub fn call_options(v: VoiceValues) -> CallOptions {
    let noise = match v.noise {
        NoisePreference::Off => NoiseMode::Off,
        NoisePreference::Automatic => NoiseMode::Enhanced,
        NoisePreference::Lightweight => NoiseMode::Standard,
    };
    CallOptions {
        mic: Mic::Device { id: v.input, noise },
        apm_flags: ApmFlags {
            echo: v.echo,
            agc: v.agc,
        },
        ..CallOptions::default()
    }
}

/// The microphone is live: not muted or deafened, and in push-to-talk mode only while the key is held.
/// What the call's microphone should be now, `(muted, deafened)`, from the settings as they are now (review I2: the mode
/// is read on every pass, so a switch to push to talk mid-call applies at once, as session.ts `setMode` does).
pub fn mic_want(prefs: &dilla_client::prefs::Prefs, s: &AppState) -> (bool, bool) {
    let ptt = VoiceValues::read(prefs).mode == InputMode::Ptt;
    (!mic_open(s, ptt), s.voice.self_deaf)
}

pub fn mic_open(s: &AppState, ptt_mode: bool) -> bool {
    !(s.voice.self_mute || s.voice.self_deaf) && (!ptt_mode || s.voice.ptt_held)
}

/// stats.ts `LINK_EVERY_MS`: how often the link's ping is read.
pub const LINK_EVERY: std::time::Duration = std::time::Duration::from_secs(2);
/// A ping read that takes longer says none (session.ts `LINK_STALL_MS` takes the stats away after 5 s; here the read
/// runs inside the call's loop, so it is bounded under one read interval and never holds the loop a tick long).
const LINK_STALL: std::time::Duration = std::time::Duration::from_millis(1_500);

/// What a share asks for.
pub enum Command {
    Share(ShareKind, bool, ShareOptions),
    /// The stream quality changed while sharing: the screen share starts again at it.
    Quality(ShareOptions),
}

/// The link's ping read (session.ts `linkTick`), polled beside the call loop's other arms rather than awaited in one, so
/// a stalled read never holds up a leave, the room's end or a command: one read in flight at a time, answering none
/// once [`LINK_STALL`] has passed.
#[derive(Default)]
struct PingRead<'a> {
    read: Option<std::pin::Pin<Box<dyn std::future::Future<Output = Option<u32>> + Send + 'a>>>,
}

impl<'a> PingRead<'a> {
    /// A tick: starts a read unless one is still in flight.
    fn start(&mut self, read: impl std::future::Future<Output = Option<u32>> + Send + 'a) {
        if self.read.is_none() {
            self.read = Some(Box::pin(async move {
                tokio::time::timeout(LINK_STALL, read).await.ok().flatten()
            }));
        }
    }

    /// The read's answer; pending while none is in flight. Cancel-safe: a read the loop's `select!` drops this for is
    /// kept and answers later.
    async fn answer(&mut self) -> Option<u32> {
        let Some(read) = self.read.as_mut() else {
            return std::future::pending().await;
        };
        let ms = read.await;
        self.read = None;
        ms
    }
}

/// session.ts `RTT_STEP_MS`: a ping is reported to the instance again once it moved this much.
const RTT_STEP_MS: u32 = 5;

/// session.ts `publishRtt` / `reportRtt` (Signal task 8c): what of this device's ping goes to the instance, for the
/// others' per-person ping ([`Voice::report_rtt`]): a sample that moved by [`RTT_STEP_MS`] or more since the last one
/// stored, one report in flight at most; a refused report is not counted as sent, so the next sample tries again.
#[derive(Default)]
struct RttReport {
    sent: Option<u32>,
    busy: bool,
}

impl RttReport {
    /// The report to send for the sample `ms`, if any (clamped to what the instance takes); it is then in flight.
    fn want(&mut self, ms: u32) -> Option<u32> {
        let ms = ms.min(dilla_client::features::voice::RTT_MAX_MS);
        if self.busy
            || self
                .sent
                .is_some_and(|sent| sent.abs_diff(ms) < RTT_STEP_MS)
        {
            return None;
        }
        self.busy = true;
        Some(ms)
    }

    /// The report of `ms` answered: `stored` when the instance took it.
    fn answered(&mut self, ms: u32, stored: bool) {
        self.busy = false;
        if stored {
            self.sent = Some(ms);
        }
    }
}

fn lock_rtt(rtt: &Mutex<RttReport>) -> std::sync::MutexGuard<'_, RttReport> {
    rtt.lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
}

/// The running session (see the module docs). Dropping it leaves the call's media.
pub struct MediaSession {
    _runtime: tokio::runtime::Runtime,
    commands: mpsc::UnboundedSender<Command>,
    videos: Arc<Mutex<RemoteVideos>>,
    /// Bumped on every new remote video frame: the view repaints when it moved.
    pub video_seq: Arc<AtomicU64>,
}

/// Where share sources come from. The real screen and camera sources are the next plan's; the automation build (and
/// a debug build with `DILLA_TEST_PATTERN=1`) uses the moving test pattern; a release build never does. Anything else
/// has none, which is a cancelled selection.
pub type SourceFactory =
    Arc<dyn Fn(ShareKind, ShareOptions) -> Option<Box<dyn CaptureSource>> + Send + Sync>;

pub fn default_sources() -> SourceFactory {
    Arc::new(|kind, options: ShareOptions| {
        let test = cfg!(feature = "automation")
            || (cfg!(debug_assertions)
                && std::env::var("DILLA_TEST_PATTERN").is_ok_and(|v| v == "1"));
        test.then(|| {
            // The pattern at the asked size, at most 720p (it is drawn on the CPU).
            let (w, h) = match kind {
                ShareKind::Camera => (640, 360),
                ShareKind::Screen => options
                    .resolution
                    .frame()
                    .map_or((1280, 720), |(w, h)| (w.min(1280), h.min(720))),
            };
            let fps = if kind == ShareKind::Screen {
                options.fps.min(30)
            } else {
                15
            };
            Box::new(dilla_media::TestPattern::new(kind, w, h, fps)) as Box<dyn CaptureSource>
        })
    })
}

impl MediaSession {
    pub fn start(client: &Client, sources: SourceFactory) -> std::io::Result<MediaSession> {
        let runtime = tokio::runtime::Builder::new_multi_thread()
            .worker_threads(2)
            .thread_name("dilla-call")
            .enable_all()
            .build()?;
        let (commands, rx) = mpsc::unbounded_channel();
        let videos = Arc::new(Mutex::new(RemoteVideos::new()));
        let video_seq = Arc::new(AtomicU64::new(0));
        let ctx = Ctx {
            voice: client.voice(),
            states: client.state(),
            prefs: client.prefs(),
            videos: videos.clone(),
            video_seq: video_seq.clone(),
            sources,
        };
        runtime.spawn(run(ctx, rx));
        Ok(MediaSession {
            _runtime: runtime,
            commands,
            videos,
            video_seq,
        })
    }

    /// Starts or stops a share.
    pub fn share(&self, kind: ShareKind, on: bool, options: ShareOptions) {
        let _ = self.commands.send(Command::Share(kind, on, options));
    }

    /// Applies a new stream quality to the running screen share.
    pub fn quality(&self, options: ShareOptions) {
        let _ = self.commands.send(Command::Quality(options));
    }

    /// Every remote video stream's newest frame.
    pub fn videos(&self) -> RemoteVideos {
        self.videos
            .lock()
            .unwrap_or_else(|p| p.into_inner())
            .clone()
    }
}

struct Ctx {
    voice: Voice,
    states: watch::Receiver<Arc<AppState>>,
    prefs: dilla_client::prefs::Prefs,
    videos: Arc<Mutex<RemoteVideos>>,
    video_seq: Arc<AtomicU64>,
    sources: SourceFactory,
}

impl Ctx {
    /// Task 19's validated voice settings (`voice.*` in the instance's prefs): what Settings -> Voice wrote.
    fn values(&self) -> VoiceValues {
        VoiceValues::read(&self.prefs)
    }

    fn ptt_mode(&self) -> bool {
        self.values().mode == InputMode::Ptt
    }

    /// The call's microphone from Settings -> Voice (integration 3: Task 19's values, so its echo and gain control and
    /// its noise choice reach the call mic): Automatic is the enhanced suppressor where it runs, Lightweight libwebrtc's.
    fn options(&self) -> CallOptions {
        call_options(self.values())
    }
}

async fn run(ctx: Ctx, mut commands: mpsc::UnboundedReceiver<Command>) {
    let mut links = ctx.voice.media();
    loop {
        let link = links.borrow_and_update().clone();
        if let Some(link) = link {
            in_call(&ctx, &link, &mut links, &mut commands).await;
        }
        if links.changed().await.is_err() {
            return;
        }
    }
}

fn token_of(link: &MediaLink) -> LiveKitToken {
    LiveKitToken {
        url: link.url.clone(),
        token: link.token.clone(),
        ice_servers: link
            .ice_servers
            .iter()
            .map(|(urls, username, credential)| IceServer {
                urls: urls.clone(),
                username: username.clone(),
                // livekit's `IceServer` takes a plain `String`: this copy is not zeroized (the TURN credential is a
                // short-lived per-call secret the SFU minted, not a key).
                password: credential.to_string(),
            })
            .collect(),
    }
}

/// Where the epoch feeder drops the keys once the call's media exists.
type MediaSlot = Arc<std::sync::OnceLock<Arc<dilla_media::transform::CallMedia>>>;

/// Hands each epoch the call group reaches to the media, in order; an epoch the core's gate refused (the link's epochs
/// turn `None`) drops every media key (WB security I1) until the next one.
fn feed_epochs(link: &MediaLink) -> (Arc<EpochFeed>, MediaSlot, tokio::task::JoinHandle<()>) {
    let feed = EpochFeed::new();
    let slot: MediaSlot = Arc::default();
    let mut epochs = link.epochs.clone();
    let out = feed.clone();
    let media = slot.clone();
    let task = tokio::spawn(async move {
        let mut suspended = false;
        loop {
            let e = epochs.borrow_and_update().clone();
            apply_epoch(
                e.as_deref(),
                &mut suspended,
                &out,
                media.get().map(|m| &**m),
            );
            if epochs.changed().await.is_err() {
                return;
            }
        }
    });
    (feed, slot, task)
}

/// One step of the epoch feeder: `None` (the gate refused the newest epoch) suspends the call's keys; an epoch goes to
/// the feed, and (ruling R24) the epoch a refusal suspended, passing again, is installed into the call's media directly
/// (the feed already has it), so its suspended sender resumes with its counter.
pub fn apply_epoch(
    e: Option<&dilla_client::calls::MediaEpoch>,
    suspended: &mut bool,
    out: &EpochFeed,
    media: Option<&dilla_media::transform::CallMedia>,
) {
    let Some(e) = e else {
        if let Some(m) = media {
            m.drop_keys();
            *suspended = true;
        }
        return;
    };
    let roster: Vec<(u16, [u8; 16])> = e.roster.iter().map(|(l, d, _)| (*l, *d)).collect();
    if *suspended
        && out.current() == Some(e.epoch)
        && let Some(m) = media
        && let Err(err) = m.install_epoch(e.epoch, &e.base_key, &roster, e.own_leaf)
    {
        log::warn!("voice: the epoch did not resume: {err}");
    }
    *suspended = false;
    out.publish(dilla_media::MediaEpoch {
        epoch: e.epoch,
        base_key: Zeroizing::new(*e.base_key),
        roster,
        own_leaf: e.own_leaf,
    });
}

async fn in_call(
    ctx: &Ctx,
    link: &MediaLink,
    links: &mut watch::Receiver<Option<Arc<MediaLink>>>,
    commands: &mut mpsc::UnboundedReceiver<Command>,
) {
    let (feed, slot, feeder) = feed_epochs(link);
    // A muted or push-to-talk join: the microphone is muted before it is published.
    let options = CallOptions {
        muted: !mic_open(&ctx.states.borrow(), ctx.ptt_mode()),
        ..ctx.options()
    };
    let call = match Call::join_with(token_of(link), feed.clone(), options).await {
        Ok(call) => call,
        Err(dilla_media::MediaError::Audio(e)) => {
            // No microphone: the call goes on listen-only, and the person is told.
            log::warn!("voice: the microphone did not open ({e}): listening only");
            ctx.voice.set_notice(Some(VoiceNotice::MicLost));
            let listen = CallOptions {
                mic: Mic::None,
                muted: true,
                ..ctx.options()
            };
            match Call::join_with(token_of(link), feed, listen).await {
                Ok(call) => call,
                Err(e) => return failed(ctx, feeder, &e),
            }
        }
        Err(e) => return failed(ctx, feeder, &e),
    };
    let _ = slot.set(call.media());
    // An epoch refused before the call existed: its keys go now.
    if link.epochs.borrow().is_none() {
        call.drop_keys();
    }
    let mut states = ctx.states.clone();
    let mut levels = call.remote_audio_levels();
    let mut videos = call.remote_videos();
    let mut events = call.events();
    let mut ended = call.shares_ended();
    let mut room_end = call.ended();
    let mut speaking = Speaking::default();
    let mut stats = tokio::time::interval(dilla_client::features::voice::STATS_EVERY);
    let mut link_read = tokio::time::interval(LINK_EVERY);
    let mut ping = PingRead::default();
    let rtt = Arc::new(Mutex::new(RttReport::default()));
    let own = ctx.states.borrow().account.device_id;
    let mut applied: Option<(bool, bool)> = None;
    loop {
        let s = states.borrow_and_update().clone();
        // The settings are read on every pass: a mode switch publishes the voice values, which wakes this loop.
        let want = mic_want(&ctx.prefs, &s);
        if applied != Some(want) {
            call.set_muted(want.0);
            call.set_deafened(want.1);
            applied = Some(want);
        }
        tokio::select! {
            r = links.changed() => {
                if r.is_err() || links.borrow().as_ref().is_none_or(|l| l.call_id != link.call_id) {
                    break;
                }
            }
            r = states.changed() => if r.is_err() { break },
            // session.ts `onRoomEnd` → `leave('lost')`: the room ended on its own (a newer device took voice over,
            // the room closed, the connection gave up). The call ends, saying why, and the takeover notice shows.
            r = room_end.changed() => {
                let end = *room_end.borrow_and_update();
                if r.is_err() || end.is_some() {
                    let code = end.map_or("E_DISCONNECTED", dilla_media::RoomEnd::code);
                    log::warn!("voice: the call's room ended on its own ({code})");
                    drop(ctx.voice.lost(link.call_id, code));
                    break;
                }
            }
            r = levels.changed() => {
                if r.is_ok() {
                    let now = speaking.update(&levels.borrow_and_update(), Instant::now());
                    let mut set: BTreeSet<DeviceId> = now.iter().filter_map(|h| unhex(h).map(DeviceId)).collect();
                    set.retain(|d| Some(*d) != own);
                    ctx.voice.set_speaking(set);
                }
            }
            r = videos.changed() => {
                if r.is_ok() {
                    let next = videos.borrow_and_update().clone();
                    *ctx.videos.lock().unwrap_or_else(|p| p.into_inner()) = next;
                    ctx.video_seq.fetch_add(1, Ordering::Relaxed);
                }
            }
            Some(e) = async { match events.as_mut() { Some(rx) => rx.recv().await, None => std::future::pending().await } } => {
                ctx.voice.set_notice(Some(notice_of(e)));
            }
            Some(kind) = async { match ended.as_mut() { Some(rx) => rx.recv().await, None => std::future::pending().await } } => {
                let mut share = ctx.states.borrow().voice.share;
                set_kind(&mut share, kind, false);
                let _ = ctx.voice.set_share(share).await;
            }
            Some(cmd) = commands.recv() => match cmd {
                Command::Share(kind, on, options) => share(ctx, &call, kind, on, options).await,
                Command::Quality(options) => {
                    if ctx.states.borrow().voice.share.screen {
                        share(ctx, &call, ShareKind::Screen, true, options).await;
                    }
                }
            },
            // session.ts `linkTick`: the link's ping, for the dock and the call bar; a read that does not settle within
            // [`LINK_STALL`] says none. The read runs beside the other arms (w2-fix2 re-review), never blocking them.
            _ = link_read.tick() => ping.start(call.rtt_ms()),
            ms = ping.answer() => {
                ctx.voice.set_ping(link.call_id, ms);
                // session.ts `publishRtt`: the others see this device's ping through the instance.
                let send = ms.and_then(|ms| lock_rtt(&rtt).want(ms));
                if let Some(send) = send {
                    let (answer, rtt) = (ctx.voice.report_rtt(Some(send)), rtt.clone());
                    tokio::spawn(async move {
                        let stored = matches!(answer.await, Ok(Ok(true)));
                        lock_rtt(&rtt).answered(send, stored);
                    });
                }
            }
            _ = stats.tick() => {
                let st = call.stats();
                ctx.voice.send_stats(CallStatsReport {
                    frames_encrypted: st.encrypted.values().sum(),
                    decrypt_failures: st.failed.values().sum(),
                    ..CallStatsReport::default()
                });
            }
        }
    }
    ctx.videos.lock().unwrap_or_else(|p| p.into_inner()).clear();
    ctx.video_seq.fetch_add(1, Ordering::Relaxed);
    ctx.voice.set_speaking(BTreeSet::new());
    feeder.abort();
    drop(ping);
    call.leave().await;
}

/// The media did not start (the room refused, no key in time): the person is told the join failed and the call is
/// left, so the signalling never stays "in" a call nobody hears.
fn failed(ctx: &Ctx, feeder: tokio::task::JoinHandle<()>, e: &dilla_media::MediaError) {
    log::warn!("voice: the call's media did not start: {e}");
    feeder.abort();
    ctx.voice.set_notice(Some(VoiceNotice::MediaFailed));
    drop(ctx.voice.leave());
}

fn set_kind(share: &mut LocalShare, kind: ShareKind, on: bool) {
    match kind {
        ShareKind::Camera => share.camera = on,
        ShareKind::Screen => {
            share.screen = on;
            if !on {
                share.screen_audio = false;
            }
        }
    }
}

/// Starts or stops a share: the instance's sharing slot first, then the track. A selection that gives no source (the
/// person cancelled it) publishes nothing and says so.
async fn share(ctx: &Ctx, call: &Call, kind: ShareKind, on: bool, options: ShareOptions) {
    let mut next = ctx.states.borrow().voice.share;
    if !on {
        call.unpublish_video(kind).await;
        set_kind(&mut next, kind, false);
        let _ = ctx.voice.set_share(next).await;
        return;
    }
    let Some(source) = (ctx.sources)(kind, options) else {
        ctx.voice.set_notice(Some(VoiceNotice::ShareCancelled));
        return;
    };
    let audio = source.has_audio();
    set_kind(&mut next, kind, true);
    if kind == ShareKind::Screen {
        next.screen_audio = audio;
    }
    if let Ok(Ok(())) = ctx.voice.set_share(next).await {
        if let Err(e) = call.publish_video_with(source, options).await {
            log::warn!("voice: the share did not start: {e}");
            set_kind(&mut next, kind, false);
            let _ = ctx.voice.set_share(next).await;
            return;
        }
        if kind == ShareKind::Screen && !audio {
            ctx.voice.set_notice(Some(VoiceNotice::ShareWithoutSound));
        }
    }
}

fn notice_of(e: MediaEvent) -> VoiceNotice {
    match e {
        MediaEvent::EnhancedUnavailable => VoiceNotice::EnhancedUnavailable,
        MediaEvent::DevicesChanged => VoiceNotice::DevicesChanged,
        MediaEvent::MicLost => VoiceNotice::MicLost,
        MediaEvent::SpeakerLost => VoiceNotice::SpeakerLost,
        MediaEvent::EnhancedFailed => VoiceNotice::EnhancedFailed,
    }
}

fn unhex(h: &str) -> Option<[u8; 16]> {
    if h.len() != 32 {
        return None;
    }
    let mut out = [0u8; 16];
    for (i, b) in out.iter_mut().enumerate() {
        *b = u8::from_str_radix(h.get(2 * i..2 * i + 2)?, 16).ok()?;
    }
    Some(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn epoch(n: u64, key: u8) -> dilla_client::calls::MediaEpoch {
        dilla_client::calls::MediaEpoch {
            epoch: n,
            base_key: Zeroizing::new([key; 16]),
            own_leaf: 0,
            roster: vec![(0, [1; 16], [2; 16]), (1, [3; 16], [4; 16])],
            code: String::new(),
        }
    }

    /// session.ts `publishRtt` / `reportRtt`: a sample goes to the instance when it moved by [`RTT_STEP_MS`] or more
    /// since the last stored one (the first always), one report in flight at most; a refused one (429, 404) is not
    /// counted as sent, so the next sample tries again; clamped to 0..=5000.
    #[test]
    fn the_rtt_is_reported_when_it_moved_one_at_a_time() {
        let mut r = RttReport::default();
        assert_eq!(r.want(40), Some(40), "the first sample");
        assert_eq!(r.want(80), None, "one in flight");
        r.answered(40, true);
        assert_eq!(r.want(44), None, "moved less than the step");
        assert_eq!(r.want(45), Some(45));
        r.answered(45, false);
        assert_eq!(r.want(46), Some(46), "a refused report was not sent");
        r.answered(46, true);
        assert_eq!(r.want(9_999), Some(5000), "clamped");
    }

    /// w2-fix2 re-review: the ping read is polled beside the call loop's other arms, never awaited in one: a stalled
    /// read does not hold up a leave (or the room's end), it answers none after [`LINK_STALL`], one read is in flight at a
    /// time, and an answered read clears the slot for the next tick.
    #[tokio::test(start_paused = true)]
    async fn a_stalled_ping_read_never_holds_up_the_loop() {
        let mut ping = PingRead::default();
        ping.start(std::future::pending::<Option<u32>>());
        // A second tick while the first read stalls starts nothing new (it would answer at once).
        ping.start(std::future::ready(Some(5)));
        let started = tokio::time::Instant::now();
        let leave = std::future::ready(());
        let left = tokio::select! {
            biased;
            ms = ping.answer() => Err(ms),
            () = leave => Ok(()),
        };
        assert_eq!(left, Ok(()), "the leave is handled while the read stalls");
        assert_eq!(started.elapsed(), std::time::Duration::ZERO);
        assert_eq!(ping.answer().await, None, "a stalled read says none");
        assert_eq!(started.elapsed(), LINK_STALL);
        ping.start(std::future::ready(Some(42)));
        assert_eq!(ping.answer().await, Some(42), "the next tick reads again");
    }

    /// The production feeder (ruling R24): a refusal suspends the call's keys, the same epoch passing again resumes
    /// them (through the direct install: the feed refuses an epoch it has), and a newer epoch installs normally.
    #[test]
    fn the_feeder_suspends_and_resumes_the_same_epoch_and_installs_a_newer_one() {
        use dilla_media::EpochKeys as _;
        let media = dilla_media::transform::CallMedia::new();
        let feed = EpochFeed::new();
        let target = Arc::downgrade(&media);
        feed.on_epoch_while(Box::new(move |e, k, r, l| {
            target
                .upgrade()
                .is_some_and(|m| m.install_epoch(e, k, &r, l).is_ok())
        }));
        let mut suspended = false;
        apply_epoch(Some(&epoch(4, 7)), &mut suspended, &feed, Some(&media));
        assert_eq!(media.sending_epoch(), Some(4));
        apply_epoch(None, &mut suspended, &feed, Some(&media));
        assert_eq!(media.sending_epoch(), None, "suspended: nothing seals");
        assert!(suspended);
        apply_epoch(Some(&epoch(4, 7)), &mut suspended, &feed, Some(&media));
        assert_eq!(media.sending_epoch(), Some(4), "the same epoch resumes");
        assert!(!suspended);
        apply_epoch(None, &mut suspended, &feed, Some(&media));
        apply_epoch(Some(&epoch(5, 9)), &mut suspended, &feed, Some(&media));
        assert_eq!(media.sending_epoch(), Some(5), "a newer epoch installs");
        // A refusal then an older epoch never seals again.
        apply_epoch(None, &mut suspended, &feed, Some(&media));
        apply_epoch(Some(&epoch(4, 7)), &mut suspended, &feed, Some(&media));
        assert_eq!(media.sending_epoch(), None);
    }

    /// Integration 3: Task 19's detector per device (speaking.ts): lit after 50 ms over the threshold, held 300 ms
    /// after it drops, and a device no longer heard is gone.
    #[test]
    fn speaking_lights_after_its_onset_and_holds_for_the_hangover() {
        let mut s = Speaking::default();
        let t0 = s.since;
        let at = |ms: u64| t0 + std::time::Duration::from_millis(ms);
        let mut levels = dilla_media::RemoteLevels::new();
        levels.insert("aa".into(), 0.05);
        levels.insert("bb".into(), 0.001);
        assert!(s.update(&levels, at(0)).is_empty(), "not yet: the onset");
        assert_eq!(s.update(&levels, at(60)), BTreeSet::from(["aa".to_owned()]));
        levels.insert("aa".into(), 0.0);
        assert!(s.update(&levels, at(260)).contains("aa"), "held");
        assert!(s.update(&levels, at(400)).contains("aa"), "held 300 ms");
        assert!(s.update(&levels, at(600)).is_empty(), "off after the hold");
        levels.insert("aa".into(), 0.05);
        let _ = s.update(&levels, at(700));
        assert!(s.update(&levels, at(760)).contains("aa"));
        levels.remove("aa");
        assert!(s.update(&levels, at(780)).is_empty(), "no longer heard");
    }

    /// Integration 3: what Settings -> Voice wrote (Task 19's values) is the call's microphone: its device, its noise
    /// choice, and echo cancellation and gain control switched off reach the call (ApmFlags, one copy in dilla-media).
    #[test]
    fn the_voice_settings_reach_the_call_mic() {
        let v = VoiceValues {
            input: Some("usb-mic".into()),
            noise: NoisePreference::Automatic,
            echo: false,
            agc: false,
            ..VoiceValues::default()
        };
        let o = call_options(v);
        match o.mic {
            Mic::Device { id, noise } => {
                assert_eq!(id.as_deref(), Some("usb-mic"));
                assert_eq!(noise, NoiseMode::Enhanced);
            }
            _ => panic!("a device microphone"),
        }
        assert!(!o.apm_flags.echo && !o.apm_flags.agc);
        let d = call_options(VoiceValues::default());
        assert!(d.apm_flags.echo && d.apm_flags.agc, "on by default");
        assert!(matches!(
            d.mic,
            Mic::Device {
                id: None,
                noise: NoiseMode::Standard
            }
        ));
    }

    /// Review I2 (session.ts `setMode` applies live): what the call's mic does is read from the settings on every pass
    /// of the loop, so switching to push to talk mid-call mutes it at once (the switch publishes the voice values,
    /// which wakes the loop).
    #[test]
    fn a_mode_switch_mid_call_is_applied() {
        let dir = std::env::temp_dir().join(format!("dilla-i2-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let prefs = dilla_client::prefs::Prefs::instance(&dir);
        let s = AppState::default();
        assert_eq!(mic_want(&prefs, &s), (false, false), "voice activity: open");
        prefs.set("voice.mode", &"ptt").unwrap();
        assert_eq!(
            mic_want(&prefs, &s),
            (true, false),
            "push to talk, key up: muted"
        );
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn the_mic_is_open_only_unmuted_undeafened_and_while_ptt_is_held_in_ptt_mode() {
        let mut s = AppState::default();
        assert!(mic_open(&s, false));
        assert!(!mic_open(&s, true));
        s.voice.ptt_held = true;
        assert!(mic_open(&s, true));
        s.voice.self_deaf = true;
        assert!(!mic_open(&s, true));
        s.voice.self_deaf = false;
        s.voice.self_mute = true;
        assert!(!mic_open(&s, false));
    }
}
