//! The media side of this device's call (session.ts's media half): while `Client::voice().media()` holds a link, a
//! `dilla_media::Call` is in its LiveKit room with the call group's epoch keys, and what the person set (mute, deafen,
//! push to talk) is applied to it. What the media hears goes back into the voice slice: who is speaking, the device
//! notices, the shares that ended; every remote video stream's newest frame is kept here for the stage's tiles.
//!
//! The frame hook is process-wide, so one session runs per process (`dilla_media::MediaError::Busy` otherwise). It runs
//! on a runtime of its own: the client's runtime is the client's.

use std::collections::{BTreeSet, HashMap};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Instant;

use dilla_client::Client;
use dilla_client::features::voice::{CallStatsReport, MediaLink, Voice};
use dilla_client::features::voice_prefs::speaking::SpeakingDetector;
use dilla_client::features::voice_prefs::values::{InputMode, NoisePreference, VoiceValues};
use dilla_client::features::voice_prefs::volume::{GainState, gain_of};
use dilla_client::state::voice::{LocalShare, SfuRemote, VoiceNotice};
use dilla_client::state::{AppState, DeviceId};
use dilla_media::apm::ApmFlags;
use dilla_media::devices::AudioHost;
use dilla_media::noise::NoiseMode;
use dilla_media::sources::OpenError;
use dilla_media::sources::desktop::{self, DesktopSource};
use dilla_media::{
    Call, CallOptions, CaptureSource, EpochFeed, IceServer, LiveKitToken, LocalVideos, MediaEvent,
    Mic, RemoteVideos, ShareKind, ShareOptions,
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
///
/// The share commands name the call they were made for (the UI's `call_id` while the call is `In`): the session keeps
/// a start that reaches it before it has taken up that call's media link, and runs it once it has; one for another call
/// never carries into this one ([`Pending`]). A stop is never kept or dropped: it applies at once to whatever share runs,
/// and cancels every kept start of its kind.
pub enum Command {
    /// A camera share starts or stops, or the screen share stops (a screen share starts with [`Command::ShareScreen`]).
    /// A start names its call; a stop may name none (it applies whatever call runs).
    Share(ShareKind, bool, ShareOptions, Option<[u8; 16]>),
    /// A screen share starts, together with its sound, in one command (Task 44 fix round 4), so no order of the loop's
    /// arms can see the start without it.
    ShareScreen {
        /// The call the share was started in.
        call: [u8; 16],
        /// The source the person chose in the picker; `None`: the system's own dialog chooses.
        source: Option<DesktopSource>,
        options: ShareOptions,
        /// The UI's token of this start, which the window's sound ([`Command::WindowSound`]) names.
        token: u64,
        /// The sound section's state when the share started: the person's choice, or `Unset` (the window's own
        /// application, once found).
        sound: crate::screens::voice::ShareSound,
    },
    /// The application the shared window of the start `u64` plays, once found. It only ever reaches the share that start
    /// brought up, and never one whose sound the person chose.
    WindowSound(u64, Option<dilla_media::app_audio::AppSource>),
    /// The stream quality changed while sharing in the call `[u8; 16]`: the running screen share goes on at it (the same
    /// source).
    Quality(ShareOptions, [u8; 16]),
    /// The person's own choice in the call it names: the running screen share carries the sound of this application from
    /// now on (`None`: no sound, a stop, which may name no call), and so does a share still opening.
    AppAudio(Option<dilla_media::app_audio::AppSource>, Option<[u8; 16]>),
    /// The window's call ended (the person left, or was taken out): no start kept from before runs any more, not even in
    /// a rejoin of the same call id.
    Forget,
}

/// Which of the kept commands a newer one replaces ([`Pending`]).
#[derive(Clone, Copy, PartialEq, Eq)]
enum Slot {
    Screen,
    Camera,
    Quality,
    AppAudio,
    /// The window's sound of the start `u64`.
    WindowSound(u64),
}

impl Command {
    /// The call the command was made for; `None` for the window's sound (it names its start by token instead), for a
    /// stop that names no call, and for [`Command::Forget`].
    fn call(&self) -> Option<[u8; 16]> {
        match self {
            Command::Share(_, _, _, call) | Command::AppAudio(_, call) => *call,
            Command::ShareScreen { call, .. } | Command::Quality(_, call) => Some(*call),
            Command::WindowSound(..) | Command::Forget => None,
        }
    }

    /// A share or its sound switched off: never kept, never dropped, run at once in whatever call runs.
    fn is_stop(&self) -> bool {
        matches!(
            self,
            Command::Share(_, false, ..) | Command::AppAudio(None, _)
        )
    }

    /// `None` for [`Command::Forget`], which is never kept.
    fn slot(&self) -> Option<Slot> {
        Some(match self {
            Command::Share(ShareKind::Screen, ..) | Command::ShareScreen { .. } => Slot::Screen,
            Command::Share(ShareKind::Camera, ..) => Slot::Camera,
            Command::Quality(..) => Slot::Quality,
            Command::AppAudio(..) => Slot::AppAudio,
            Command::WindowSound(token, _) => Slot::WindowSound(*token),
            Command::Forget => return None,
        })
    }
}

/// The share commands that reached the session while it was not in their call: before it took up the call's media
/// link (the client reports the call `In` first, and a move's old room may still be closing), or during the call a move
/// left. Only the newest start of each kind is kept (one screen start, one camera, one quality, one sound choice), in
/// the order they came; a window's sound only while the start it names is kept. A stop is never kept: it cancels the
/// kept start of its kind (whatever call that names). A call's media takes those made for it ([`Pending::take_for`]) and
/// the rest are dropped, so a press for one call never carries into another; the window's call ending
/// ([`Command::Forget`]) and a new link ([`Pending::retain_current`]) drop them too.
#[derive(Default)]
struct Pending(Vec<Command>);

impl Pending {
    /// Keeps `command` (a newer one of its kind replaces the older). A stop only cancels the kept start of its kind,
    /// [`Command::Forget`] cancels everything, and a window's sound whose start is not kept is dropped.
    fn keep(&mut self, command: Command) {
        match command {
            Command::Forget => {
                self.0.clear();
                return;
            }
            Command::WindowSound(token, _) if !self.starts(token) => return,
            _ => {}
        }
        self.cancel(&command);
        if !command.is_stop() {
            self.0.push(command);
        }
    }

    /// Drops the kept command of `command`'s kind; for the screen also the window's sound of the start it replaces
    /// (a newer start keeps its own).
    fn cancel(&mut self, command: &Command) {
        let slot = command.slot();
        self.0.retain(|c| c.slot() != slot);
        if slot == Some(Slot::Screen) {
            let token = match command {
                Command::ShareScreen { token, .. } => Some(*token),
                _ => None,
            };
            self.0
                .retain(|c| !matches!(c, Command::WindowSound(t, _) if Some(*t) != token));
        }
    }

    /// The link changed: only the commands for its call stay kept (none while there is no link).
    fn retain_current(&mut self, call: Option<[u8; 16]>) {
        match call {
            Some(call) => self.0 = self.take_for(call),
            None => self.0.clear(),
        }
    }

    /// Whether the kept screen start is the one `token` names.
    fn starts(&self, token: u64) -> bool {
        self.0
            .iter()
            .any(|c| matches!(c, Command::ShareScreen { token: t, .. } if *t == token))
    }

    /// Whether the loop of the call `call` leaves `command` for another call: it was made for another call, or it is the
    /// window's sound of a start kept for one.
    fn wants(&self, command: &Command, call: [u8; 16]) -> bool {
        match command {
            Command::WindowSound(token, _) => self.starts(*token),
            c => c.call() != Some(call),
        }
    }

    /// The kept commands made for `call`, in order; every other one is dropped.
    ///
    /// Residual carry-over: a start pressed in a call, the person's leave and a rejoin of the same call id (its room had
    /// not finished) all while this session's task never ran and the window's sync never saw the call end
    /// ([`Command::Forget`]) still runs in the rejoin, since it names that same id.
    fn take_for(&mut self, call: [u8; 16]) -> Vec<Command> {
        let mut kept: Vec<Command> = Vec::new();
        for c in std::mem::take(&mut self.0) {
            let ours = match &c {
                Command::WindowSound(token, _) => kept
                    .iter()
                    .any(|k| matches!(k, Command::ShareScreen { token: t, .. } if t == token)),
                c => c.call() == Some(call),
            };
            if ours {
                kept.push(c);
            }
        }
        kept
    }

    /// Drops the commands kept for `call` (its media ended: nothing more runs in it).
    fn forget(&mut self, call: [u8; 16]) {
        let starts: Vec<u64> = self
            .0
            .iter()
            .filter_map(|c| match c {
                Command::ShareScreen { call: c, token, .. } if *c == call => Some(*token),
                _ => None,
            })
            .collect();
        self.0.retain(|c| match c {
            Command::WindowSound(t, _) => !starts.contains(t),
            c => c.call() != Some(call),
        });
    }
}

/// The link's ping read (session.ts `linkTick`), polled beside the call loop's other arms rather than awaited in one, so
/// a stalled read never holds up a leave, the room's end or a command: one read in flight at a time, answering none
/// once [`LINK_STALL`] has passed.
struct PingRead<'a, T = u32> {
    read: Option<std::pin::Pin<Box<dyn std::future::Future<Output = Option<T>> + Send + 'a>>>,
}

impl<T> Default for PingRead<'_, T> {
    fn default() -> Self {
        Self { read: None }
    }
}

impl<'a, T: Send + 'a> PingRead<'a, T> {
    /// A tick: starts a read unless one is still in flight.
    fn start(&mut self, read: impl std::future::Future<Output = Option<T>> + Send + 'a) {
        if self.read.is_none() {
            self.read = Some(Box::pin(async move {
                tokio::time::timeout(LINK_STALL, read).await.ok().flatten()
            }));
        }
    }

    /// The read's answer; pending while none is in flight. Cancel-safe: a read the loop's `select!` drops this for is
    /// kept and answers later.
    async fn answer(&mut self) -> Option<T> {
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
    /// The session's runtime and its loop ([`run`]); `Drop` hands both to a thread that lets the loop leave the call.
    running: Option<(tokio::runtime::Runtime, tokio::task::JoinHandle<()>)>,
    /// `None` once the session is dropping: the closed channel is what tells [`run`] the session is gone.
    commands: Option<mpsc::UnboundedSender<Command>>,
    /// Dropped with the session: [`in_call`]'s loop is cancelled at once (`Ctx::closed`).
    closing: Option<watch::Sender<()>>,
    /// Whether a call's media is live in the session: what the drop waits on.
    live: Arc<MediaLive>,
    /// The room closes of this session's dropped calls (`CallOptions::room_closes`): its end thread awaits them before
    /// the runtime shuts down.
    room_closes: dilla_media::RoomCloses,
    videos: Arc<Mutex<RemoteVideos>>,
    /// What this device sends, for its own tiles ([`MediaSession::local_videos`]).
    local_videos: Arc<Mutex<LocalVideos>>,
    /// Bumped on every new remote or own video frame: the view repaints when it moved.
    pub video_seq: Arc<AtomicU64>,
    /// The devices (hex) whose screen share carries sound.
    screen_sound: Arc<Mutex<BTreeSet<String>>>,
    /// The application the running screen share publishes the sound of ([`MediaSession::app_sound`]).
    app_sound: Arc<Mutex<Option<dilla_media::app_audio::AppSource>>>,
    /// Test hook: while it holds, [`run`] waits before it takes up a call's media link ([`MediaSession::hold_pickup`]).
    #[cfg(feature = "test-hooks")]
    hold: watch::Sender<bool>,
}

/// What a share asks its [`SourceFactory`] for.
pub struct ShareRequest {
    pub kind: ShareKind,
    pub options: ShareOptions,
    /// The screen or window the person chose in the picker; `None` for the camera, and for a screen under the system
    /// portal (Wayland), whose own dialog chooses.
    pub source: Option<DesktopSource>,
    /// Set when the selection is withdrawn (the share was switched off, the call or the window went away): an open that
    /// is still waiting gives up.
    pub cancel: Arc<AtomicBool>,
}

impl ShareRequest {
    pub fn cancelled(&self) -> bool {
        self.cancel.load(Ordering::Acquire)
    }
}

/// Where share sources come from: called off the call's loop (it may wait for the portal's dialog or a camera), with
/// the request; an error is a selection that gives no source (cancelled or unavailable), which publishes nothing.
pub type SourceFactory =
    Arc<dyn Fn(&ShareRequest) -> Result<Box<dyn CaptureSource>, OpenError> + Send + Sync>;

/// The automation build (and a debug build with `DILLA_TEST_PATTERN=1`) shares the moving test pattern; everything else
/// the real sources (`dilla_media::sources`).
pub fn default_sources() -> SourceFactory {
    let test = cfg!(feature = "automation")
        || (cfg!(debug_assertions) && std::env::var("DILLA_TEST_PATTERN").is_ok_and(|v| v == "1"));
    if test {
        return Arc::new(|r: &ShareRequest| Ok(test_pattern(r.kind, r.options)));
    }
    Arc::new(real_source)
}

/// The moving test pattern at the asked size, at most 720p (it is drawn on the CPU).
pub fn test_pattern(kind: ShareKind, options: ShareOptions) -> Box<dyn CaptureSource> {
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
    Box::new(dilla_media::TestPattern::new(kind, w, h, fps))
}

/// The real sources: the camera, the chosen screen or window, or the portal's choice.
fn real_source(r: &ShareRequest) -> Result<Box<dyn CaptureSource>, OpenError> {
    let cancel = || r.cancelled();
    match r.kind {
        ShareKind::Camera => camera(&cancel),
        // The picker's choice, or (Linux on Wayland, no choice) the system portal's own dialog. Never a source nobody
        // chose: off the portal a request without a choice opens nothing.
        ShareKind::Screen => match &r.source {
            Some(s) => desktop::open(Some(s), r.options.fps, desktop::START_WAIT, &cancel),
            None if desktop::portal() => {
                desktop::open(None, r.options.fps, desktop::PORTAL_WAIT, &cancel)
            }
            None => Err(OpenError::Unavailable("no source was chosen".to_owned())),
        }
        .map(|c| Box::new(c) as Box<dyn CaptureSource>),
    }
}

#[cfg(any(target_os = "linux", target_os = "macos", target_os = "windows"))]
fn camera(cancel: &dyn Fn() -> bool) -> Result<Box<dyn CaptureSource>, OpenError> {
    dilla_media::sources::camera::open(None, desktop::START_WAIT, cancel)
        .map(|c| Box::new(c) as Box<dyn CaptureSource>)
}

#[cfg(not(any(target_os = "linux", target_os = "macos", target_os = "windows")))]
fn camera(_cancel: &dyn Fn() -> bool) -> Result<Box<dyn CaptureSource>, OpenError> {
    Err(OpenError::Unavailable(
        "no camera source on this system yet".to_owned(),
    ))
}

/// Each remote device's playout gains in the call, `(device hex, stream)` to gain: its person's volume (or their
/// stream's) times the output volume of Settings -> Voice (call/volume.ts `gainOf`; deafen is the speaker's own
/// silence). The own devices are not played.
pub fn gains(v: &VoiceValues, s: &AppState) -> std::collections::BTreeMap<(String, bool), f32> {
    let Some(channel) = s.voice.call.channel else {
        return std::collections::BTreeMap::new();
    };
    let state = GainState {
        deafened: false,
        paused: false,
        output: Some(v.output_volume),
    };
    let mut out = std::collections::BTreeMap::new();
    for p in s.voice.participants(channel).iter().filter(|p| !p.own) {
        let user = hex(&p.user_id.0);
        let device = hex(&p.device_id.0);
        out.insert((device.clone(), false), gain_of(&user, &v.volumes, &state));
        out.insert((device, true), gain_of(&user, &v.stream_volumes, &state));
    }
    out
}

fn hex(b: &[u8; 16]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

impl MediaSession {
    pub fn start(client: &Client, sources: SourceFactory) -> std::io::Result<MediaSession> {
        Self::start_on(client, sources, dilla_media::devices::system())
    }

    /// As [`start`](MediaSession::start) on another audio host (a fake one in tests).
    pub fn start_on(
        client: &Client,
        sources: SourceFactory,
        host: Arc<dyn AudioHost>,
    ) -> std::io::Result<MediaSession> {
        let runtime = tokio::runtime::Builder::new_multi_thread()
            .worker_threads(2)
            .thread_name("dilla-call")
            .enable_all()
            .build()?;
        let (commands, rx) = mpsc::unbounded_channel();
        let live = Arc::new(MediaLive::default());
        let room_closes = dilla_media::RoomCloses::default();
        let (closing, closed) = watch::channel(());
        let videos = Arc::new(Mutex::new(RemoteVideos::new()));
        let local_videos = Arc::new(Mutex::new(LocalVideos::new()));
        let video_seq = Arc::new(AtomicU64::new(0));
        let screen_sound = Arc::new(Mutex::new(BTreeSet::new()));
        let app_sound = Arc::new(Mutex::new(None));
        #[cfg(feature = "test-hooks")]
        let (hold, held) = watch::channel(false);
        let ctx = Ctx {
            voice: client.voice(),
            states: client.state(),
            prefs: client.prefs(),
            videos: videos.clone(),
            local_videos: local_videos.clone(),
            video_seq: video_seq.clone(),
            screen_sound: screen_sound.clone(),
            app_sound: app_sound.clone(),
            sources,
            host,
            live: live.clone(),
            room_closes: room_closes.clone(),
            closed,
            #[cfg(feature = "test-hooks")]
            held,
        };
        let task = runtime.spawn(run(ctx, rx));
        Ok(MediaSession {
            running: Some((runtime, task)),
            commands: Some(commands),
            closing: Some(closing),
            live,
            room_closes,
            videos,
            local_videos,
            video_seq,
            screen_sound,
            app_sound,
            #[cfg(feature = "test-hooks")]
            hold,
        })
    }

    /// The window's call ended: no share start kept from before runs any more ([`Command::Forget`]).
    pub fn forget_kept(&self) {
        self.send(Command::Forget);
    }

    /// Test hook: while `hold` is set, the session waits before it takes up a call's media link, as a busy or starved
    /// session does (the client already reports the call `In` meanwhile).
    #[cfg(feature = "test-hooks")]
    pub fn hold_pickup(&self, hold: bool) {
        self.hold.send_replace(hold);
    }

    fn send(&self, command: Command) {
        if let Some(commands) = &self.commands {
            let _ = commands.send(command);
        }
    }

    /// Starts or stops the camera, or stops the screen share ([`MediaSession::share_screen`] starts one). A start runs in
    /// the call `call` (one the session has not taken up yet runs once it has; a start with no call never runs); a stop
    /// applies at once to whatever share runs.
    pub fn share(&self, kind: ShareKind, on: bool, options: ShareOptions, call: Option<[u8; 16]>) {
        self.send(Command::Share(kind, on, options, call));
    }

    /// Starts a screen share of `source` (`None`: the system's own dialog chooses) with the sound section's state
    /// `sound`, in the call `call`. `token` names this start for [`MediaSession::window_sound`].
    pub fn share_screen(
        &self,
        source: Option<DesktopSource>,
        options: ShareOptions,
        token: u64,
        sound: crate::screens::voice::ShareSound,
        call: [u8; 16],
    ) {
        self.send(Command::ShareScreen {
            call,
            source,
            options,
            token,
            sound,
        });
    }

    /// The application the shared window of the start `token` plays. While that start is opening it waits for it; once
    /// the start is up it plays at once; a start that failed or was replaced drops it, and so does the person's own
    /// choice.
    pub fn window_sound(&self, token: u64, app: Option<dilla_media::app_audio::AppSource>) {
        self.send(Command::WindowSound(token, app));
    }

    /// The application whose sound the running screen share publishes now (what the share menu's sound row shows).
    pub fn app_sound(&self) -> Option<dilla_media::app_audio::AppSource> {
        self.app_sound
            .lock()
            .unwrap_or_else(|p| p.into_inner())
            .clone()
    }

    /// The person's choice: shares the sound of `source` with the running screen share (`None` stops it) at once, and
    /// with a screen share still opening once it is up.
    pub fn app_audio(
        &self,
        source: Option<dilla_media::app_audio::AppSource>,
        call: Option<[u8; 16]>,
    ) {
        self.send(Command::AppAudio(source, call));
    }

    /// Applies a new stream quality to the running screen share of the call `call`.
    pub fn quality(&self, options: ShareOptions, call: [u8; 16]) {
        self.send(Command::Quality(options, call));
    }

    /// The devices (hex) whose screen share carries sound: a remote screen without it shows the "no sound" mark.
    pub fn screen_sound(&self) -> BTreeSet<String> {
        self.screen_sound
            .lock()
            .unwrap_or_else(|p| p.into_inner())
            .clone()
    }

    /// Whether this session's call media is live: from the start of a join (the process's media router may be this
    /// session's from then on) until the call's media ended or its join was dropped (the router is free again). A join
    /// in another window waits for it to clear ([`super::after_leaving_other_calls`]): the router takes one call per
    /// process, so a join whose media starts before it clears fails with `E_CALL_BUSY`.
    pub fn media_live(&self) -> bool {
        self.live.is_live()
    }

    /// Whether this session's call media is live, or its left call's room is still closing (for [`ROOM_CLOSE_WAIT`] at
    /// most). This window's own next join waits for it to clear ([`super::rooms_open_for_join`]), so a rejoin never
    /// starts while its own old room is still connected.
    pub fn room_open(&self) -> bool {
        self.live.room_open()
    }

    /// Every remote video stream's newest frame.
    pub fn videos(&self) -> RemoteVideos {
        self.videos
            .lock()
            .unwrap_or_else(|p| p.into_inner())
            .clone()
    }

    /// What this device sends now, for its own tiles' self-view (session.ts `localCamera` / `localScreen`): each running
    /// share's newest sent frame ([`dilla_media::Call::local_videos`]), empty while nothing is sent.
    pub fn local_videos(&self) -> LocalVideos {
        self.local_videos
            .lock()
            .unwrap_or_else(|p| p.into_inner())
            .clone()
    }
}

impl Drop for MediaSession {
    /// Leaves the call the way a leave does, not by tearing its runtime down under it. Dropping the runtime cancelled the
    /// call's task: the `Call` was dropped on a runtime that was shutting down, so its room was never closed and its
    /// PeerConnection (with its sockets, threads and frame transformers) lived until the process exited. Here the
    /// session's end is signalled first (`closing` and the commands channel close), which cancels a join in flight or
    /// the call's loop and ends [`run`] (a call in progress leaves first: `Call::leave` closes its room); the runtime is
    /// dropped after that, on a thread of its own so the window never waits on the network. That wait is bounded
    /// ([`end_wait`]); past it the runtime is shut down with whatever is still running.
    ///
    /// The call's media itself ends before this returns, as it did when the runtime was dropped here: the shares stop
    /// (their sources are let go) and the media router is free for the next call at once; only the room's close runs
    /// on after. The window waits for that only while a call's media is live ([`MediaLive`]), and
    /// [`super::QUIT_WAIT`] at most; a call that already ended (a leave, its room closing) is not waited for.
    fn drop(&mut self) {
        let Some((runtime, task)) = self.running.take() else {
            return;
        };
        // Closing these ends the session's loop (see `run` and `in_call`): a join or a call's loop is cancelled at once.
        self.closing.take();
        self.commands.take();
        if !self.live.wait_ended(super::QUIT_WAIT) {
            log::warn!(
                "voice: the call's media did not end within {:?} of its window",
                super::QUIT_WAIT
            );
        }
        let wait = end_wait();
        let room_closes = self.room_closes.clone();
        let ended = std::thread::Builder::new()
            .name("dilla-call-end".into())
            .spawn(move || {
                // The session's loop first, then every room one of this session's dropped calls is still closing on
                // this runtime (a join cancelled after its room connected drops its call): the shutdown below would
                // cancel them. Only this session's: another window's slow close holds nothing up here.
                if runtime
                    .block_on(async {
                        tokio::time::timeout(wait, async {
                            let _ = task.await;
                            room_closes.closed().await;
                        })
                        .await
                    })
                    .is_err()
                {
                    log::warn!(
                        "voice: the call did not end within {wait:?}: its runtime is shut down"
                    );
                }
                runtime.shutdown_timeout(super::QUIT_WAIT);
            });
        if let Err(e) = ended {
            // The runtime came back with the error and is dropped here: the call's room is not closed.
            log::warn!("voice: the call's end could not run on a thread of its own: {e}");
        }
    }
}

/// How long a dropped session's call may take to end. A join in flight waits for its first key at most the call's
/// `key_timeout` (the desktop joins with `CallOptions::default()`'s) and then leaves; the leave gets [`super::QUIT_WAIT`],
/// what quitting the app gives a call's leave.
fn end_wait() -> std::time::Duration {
    CallOptions::default().key_timeout + super::QUIT_WAIT
}

struct Ctx {
    voice: Voice,
    app_sound: Arc<Mutex<Option<dilla_media::app_audio::AppSource>>>,
    states: watch::Receiver<Arc<AppState>>,
    prefs: dilla_client::prefs::Prefs,
    videos: Arc<Mutex<RemoteVideos>>,
    local_videos: Arc<Mutex<LocalVideos>>,
    video_seq: Arc<AtomicU64>,
    screen_sound: Arc<Mutex<BTreeSet<String>>>,
    sources: SourceFactory,
    host: Arc<dyn AudioHost>,
    /// Set while a call's media is live ([`in_call`]); [`MediaSession`]'s drop waits for it to clear.
    live: Arc<MediaLive>,
    /// This session's room closes, given to every call it joins.
    room_closes: dilla_media::RoomCloses,
    /// Changes (closes) when the session is dropped.
    closed: watch::Receiver<()>,
    /// Test hook ([`MediaSession::hold_pickup`]).
    #[cfg(feature = "test-hooks")]
    held: watch::Receiver<bool>,
}

/// Whether a call's media is live in the session: from the start of a join until the call's media ended (or the join
/// or the call's task went). A dropped session waits for it to clear, and only while it is set: a call that already ended (and is
/// only closing its room) holds nothing up.
///
/// Call fix round B1: after the media ends, the call's room may still be closing ([`RoomClosing`]). This window's next
/// join waits for that too ([`MediaLive::room_open`]), so a rejoin never starts beside its own old room; the dropped
/// session's wait does not.
#[derive(Default)]
struct MediaLive {
    live: Mutex<bool>,
    cleared: std::sync::Condvar,
    /// How many left calls' rooms are closing ([`close_room`]): a count, so one close that ends never clears another.
    closing: std::sync::atomic::AtomicU32,
}

/// A live call's media; clears [`MediaLive`] when dropped, however the call's task ends.
struct LiveGuard(Arc<MediaLive>);

/// A left call's room that is still closing; counts in [`MediaLive::room_open`] until dropped.
struct RoomClosing(Arc<MediaLive>);

impl LiveGuard {
    /// The media ended and the room's close begins: the room stays open (never a moment closed in between) while the
    /// media is no longer live.
    fn closing(self) -> RoomClosing {
        self.0.closing.fetch_add(1, Ordering::SeqCst);
        RoomClosing(self.0.clone())
    }
}

impl Drop for RoomClosing {
    fn drop(&mut self) {
        self.0.closing.fetch_sub(1, Ordering::SeqCst);
    }
}

/// How long a left call's room close holds this window's next join and this session's next call ([`close_room`]):
/// less than the join's own bound ([`super::HANDOVER_WAIT`]), so a close that hangs delays the join and the next call's
/// media by this much at most, and fails neither.
pub const ROOM_CLOSE_WAIT: std::time::Duration = std::time::Duration::from_secs(5);

/// Closes a left call's room (`leave`: `Call::leave`) while `closing` holds it open for this window's next join, for
/// [`ROOM_CLOSE_WAIT`] at most. Past it this returns, so the session's loop takes the next call's link and its media
/// starts; the rest of the close is handed to the runtime (as `Drop for Call` hands a dropped call's close), registered
/// in `closes` so the session's drop waits for it, and never cancelled: a room whose close is dropped stays connected.
async fn close_room(
    closing: RoomClosing,
    closes: dilla_media::RoomCloses,
    leave: impl std::future::Future<Output = ()> + Send + 'static,
) {
    let mut leave = Box::pin(leave);
    if tokio::time::timeout(ROOM_CLOSE_WAIT, leave.as_mut())
        .await
        .is_err()
    {
        log::warn!(
            "voice: the left call's room did not close within {ROOM_CLOSE_WAIT:?}; it closes on in the background"
        );
        drop(closing);
        closes.add(tokio::spawn(leave));
    }
}

impl MediaLive {
    fn begin(self: &Arc<Self>) -> LiveGuard {
        *self.live.lock().unwrap_or_else(|p| p.into_inner()) = true;
        LiveGuard(self.clone())
    }

    fn is_live(&self) -> bool {
        *self.live.lock().unwrap_or_else(|p| p.into_inner())
    }

    /// Whether a call's media is live or its left room is still closing (bounded: [`close_room`]).
    fn room_open(&self) -> bool {
        self.closing.load(Ordering::SeqCst) > 0 || self.is_live()
    }

    /// Waits until no call's media is live, at most `bound`; true when none is.
    fn wait_ended(&self, bound: std::time::Duration) -> bool {
        let live = self.live.lock().unwrap_or_else(|p| p.into_inner());
        let (live, _) = self
            .cleared
            .wait_timeout_while(live, bound, |live| *live)
            .unwrap_or_else(|p| p.into_inner());
        !*live
    }
}

impl Drop for LiveGuard {
    fn drop(&mut self) {
        *self.0.live.lock().unwrap_or_else(|p| p.into_inner()) = false;
        self.0.cleared.notify_all();
    }
}

/// Resolves once the session is dropped (`Ctx::closed`'s sender is gone).
async fn session_closed(mut closed: watch::Receiver<()>) {
    while closed.changed().await.is_ok() {}
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
        CallOptions {
            room_closes: self.room_closes.clone(),
            ..call_options(self.values())
        }
    }
}

async fn run(ctx: Ctx, mut commands: mpsc::UnboundedReceiver<Command>) {
    let mut links = ctx.voice.media();
    let mut pending = Pending::default();
    loop {
        #[cfg(feature = "test-hooks")]
        if links.borrow().is_some() && !pickup_released(&ctx).await {
            return;
        }
        let link = links.borrow_and_update().clone();
        if let Some(link) = link {
            in_call(&ctx, &link, &mut links, &mut commands, &mut pending).await;
            // The call's media ended: a command for it still queued (a press as it ended, which the loop's last pass did
            // not take) never runs, not even in a rejoin of the same call.
            while let Ok(c) = commands.try_recv() {
                pending.keep(c);
            }
            pending.forget(link.call_id);
            // A move replaces the link while the old call runs (its loop saw the change and left): the new call's link
            // is already here, so it is joined now, not after some later change.
            // A dropped session (its commands channel closed) joins nothing more, a move's next call included.
            if commands.is_closed() {
                return;
            }
            if links
                .borrow()
                .as_ref()
                .is_some_and(|l| l.call_id != link.call_id)
            {
                continue;
            }
        }
        // Biased, the session's end first: a dropped session whose link changed at the same moment joins nothing. A
        // command meanwhile is kept for the call it names, which the next link may be (the client reports a call `In`
        // before this loop has seen its link).
        loop {
            tokio::select! {
                biased;
                cmd = commands.recv() => match cmd {
                    None => return,
                    Some(c) => pending.keep(c),
                },
                r = links.changed() => {
                    if r.is_err() {
                        return;
                    }
                    // A start kept for a call that is not the new link's never runs.
                    pending.retain_current(links.borrow().as_ref().map(|l| l.call_id));
                    break;
                }
            }
        }
    }
}

/// Test hook: waits while [`MediaSession::hold_pickup`] holds; `false` once the session is dropped.
#[cfg(feature = "test-hooks")]
async fn pickup_released(ctx: &Ctx) -> bool {
    let mut held = ctx.held.clone();
    tokio::select! {
        biased;
        () = session_closed(ctx.closed.clone()) => false,
        r = held.wait_for(|h| !*h) => r.is_ok(),
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
    pending: &mut Pending,
) {
    // The share commands made for this call before its media took it up run once it has joined; one made for another
    // call (a window's press as its last call ended) never carries into this one.
    while let Ok(c) = commands.try_recv() {
        pending.keep(c);
    }
    let kept = pending.take_for(link.call_id);
    let (feed, slot, feeder) = feed_epochs(link);
    // A muted or push-to-talk join: the microphone is muted before it is published. A microphone the OS denies the app
    // would only send silence (macOS, Windows): the call joins listen-only and says so (Package 7, beta.5's
    // `voice.micDenied` after a native Deny).
    let options = if crate::native::permissions::microphone_denied() {
        ctx.voice.set_notice(Some(VoiceNotice::MicDenied));
        CallOptions {
            mic: Mic::None,
            muted: true,
            ..ctx.options()
        }
    } else {
        CallOptions {
            muted: !mic_open(&ctx.states.borrow(), ctx.ptt_mode()),
            ..ctx.options()
        }
    };
    // The join runs while the person may already have moved on (Move to, another join, a leave): a join whose link is
    // no longer the current one is dropped at once (its guard ends its media), and it never fails or leaves the call
    // that replaced it.
    // From the join until `end_media` below (or a return before it) the call's media is live: a dropped session waits
    // for it to end. A dropped session cancels the join outright, as it cancels the call's loop: the join's guard ends
    // its media (the router is free) before this guard, declared first, goes.
    let live = ctx.live.begin();
    let joined = tokio::select! {
        biased;
        () = session_closed(ctx.closed.clone()) => {
            feeder.abort();
            return;
        }
        r = Call::join_on(token_of(link), feed.clone(), options, ctx.host.clone()) => r,
        () = superseded(links.clone(), link.call_id) => {
            feeder.abort();
            return;
        }
    };
    let call = match joined {
        Ok(call) => call,
        Err(dilla_media::MediaError::Audio(e)) => {
            // No microphone: the call goes on listen-only, and the person is told (voice.micMissing, as beta.5). No
            // output device is no failure: the call plays nowhere until one appears, and says nothing (beta.5).
            log::warn!("voice: the microphone did not open ({e}): listening only");
            ctx.voice.set_notice(Some(VoiceNotice::MicLost));
            let listen = CallOptions {
                mic: Mic::None,
                muted: true,
                ..ctx.options()
            };
            let joined = tokio::select! {
                biased;
                () = session_closed(ctx.closed.clone()) => {
                    feeder.abort();
                    return;
                }
                r = Call::join_on(token_of(link), feed, listen, ctx.host.clone()) => r,
                () = superseded(links.clone(), link.call_id) => {
                    feeder.abort();
                    return;
                }
            };
            match joined {
                Ok(call) => call,
                Err(e) => return failed(ctx, links, link.call_id, feeder, &e),
            }
        }
        Err(e) => return failed(ctx, links, link.call_id, feeder, &e),
    };
    let _ = slot.set(call.media());
    // An epoch refused before the call existed: its keys go now.
    if link.epochs.borrow().is_none() {
        call.drop_keys();
    }
    let mut states = ctx.states.clone();
    let mut levels = call.remote_audio_levels();
    let mut videos = call.remote_videos();
    let mut local_videos = call.local_videos();
    let mut remotes = call.remote_participants();
    let mut events = call.events();
    let mut ended = call.shares_ended();
    let mut app_ended = call.app_audio_ended();
    let mut room_end = call.ended();
    let mut speaking = Speaking::default();
    // The remote devices with a verified audio track, and those with a video stream: the media peers the stage's tiles are.
    let mut sfu_peers: Vec<SfuRemote> = Vec::new();
    let mut audio_peers: BTreeSet<DeviceId> = BTreeSet::new();
    let mut video_peers: BTreeSet<DeviceId> = BTreeSet::new();
    let mut screen_peers: BTreeSet<DeviceId> = BTreeSet::new();
    let mut stats = tokio::time::interval(dilla_client::features::voice::STATS_EVERY);
    let mut link_read = tokio::time::interval(LINK_EVERY);
    let mut ping: PingRead<'_, dilla_media::LinkRead> = PingRead::default();
    // The previous link read and when it answered: the rate is what moved since (stats.ts `linkSample`).
    let mut last_link: Option<(Instant, dilla_media::LinkRead)> = None;
    let rtt = Arc::new(Mutex::new(RttReport::default()));
    let own = ctx.states.borrow().account.device_id;
    let mut applied: Option<(bool, bool)> = None;
    let mut opens = Opens::default();
    // The application sound asked for before the screen share is up: it starts with the share.
    let mut sound = AppSoundSlot::default();
    let (opened_tx, mut opened) = mpsc::unbounded_channel::<Opened>();
    let mut gained = std::collections::BTreeMap::new();
    // The loop is cancelled outright when the session is dropped: whatever an arm was awaiting (a share being
    // published, an answer from the client) is let go, as a dropped runtime let it go, and the call's media ends at
    // once below.
    let session = async {
        for c in kept {
            handle_command(ctx, &call, &mut opens, &opened_tx, &mut sound, c).await;
        }
        loop {
            let s = states.borrow_and_update().clone();
            // The settings are read on every pass: a mode switch publishes the voice values, which wakes this loop.
            let want = mic_want(&ctx.prefs, &s);
            if applied != Some(want) {
                call.set_muted(want.0);
                call.set_deafened(want.1);
                applied = Some(want);
            }
            // The per-person volumes (the stage's person menu writes them, which wakes this loop as a mode switch does).
            let now = gains(&ctx.values(), &s);
            if now != gained {
                for ((device, stream), gain) in &now {
                    if gained.get(&(device.clone(), *stream)) != Some(gain) {
                        call.set_gain(device, *stream, *gain);
                    }
                }
                gained = now;
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
                        let heard = levels.borrow_and_update().clone();
                        audio_peers = heard.keys().filter_map(|h| unhex(h).map(DeviceId)).collect();
                        audio_peers.retain(|d| Some(*d) != own);
                        publish_peers(ctx, &sfu_peers, &audio_peers, &video_peers, &screen_peers);
                        let now = speaking.update(&heard, Instant::now());
                        let mut set: BTreeSet<DeviceId> = now.iter().filter_map(|h| unhex(h).map(DeviceId)).collect();
                        set.retain(|d| Some(*d) != own);
                        ctx.voice.set_speaking(set);
                    }
                }
                r = remotes.changed() => {
                    if r.is_ok() {
                        // Every remote of the room, in its order: an identity that is not a device id is a stranger's
                        // stream, which the stage shows as unverified.
                        sfu_peers = remotes
                            .borrow_and_update()
                            .iter()
                            .map(|h| match unhex(h) {
                                Some(d) => SfuRemote::Device(DeviceId(d)),
                                None => SfuRemote::Other(h.clone()),
                            })
                            .filter(|r| !matches!(r, SfuRemote::Device(d) if Some(*d) == own))
                            .collect();
                        publish_peers(ctx, &sfu_peers, &audio_peers, &video_peers, &screen_peers);
                    }
                }
                r = videos.changed() => {
                    if r.is_ok() {
                        let next = videos.borrow_and_update().clone();
                        video_peers = next
                            .keys()
                            .filter_map(|k| unhex(&k.device).map(DeviceId))
                            .filter(|d| Some(*d) != own)
                            .collect();
                        screen_peers = next
                            .keys()
                            .filter(|k| k.kind == ShareKind::Screen)
                            .filter_map(|k| unhex(&k.device).map(DeviceId))
                            .filter(|d| Some(*d) != own)
                            .collect();
                        publish_peers(ctx, &sfu_peers, &audio_peers, &video_peers, &screen_peers);
                        *ctx.videos.lock().unwrap_or_else(|p| p.into_inner()) = next;
                        *ctx.screen_sound.lock().unwrap_or_else(|p| p.into_inner()) = call.remote_screen_sound();
                        ctx.video_seq.fetch_add(1, Ordering::Relaxed);
                    }
                }
                r = local_videos.changed() => {
                    if r.is_ok() {
                        let next = local_videos.borrow_and_update().clone();
                        *ctx.local_videos.lock().unwrap_or_else(|p| p.into_inner()) = next;
                        ctx.video_seq.fetch_add(1, Ordering::Relaxed);
                    }
                }
                Some(e) = async { match events.as_mut() { Some(rx) => rx.recv().await, None => std::future::pending().await } } => {
                    ctx.voice.set_notice(Some(notice_of(e)));
                }
                Some(kind) = async { match ended.as_mut() { Some(rx) => rx.recv().await, None => std::future::pending().await } } => {
                    // Client review M1: a replaced share's end can land after its replacement went live (its feeder
                    // took it out of the call, then awaited the unpublish while the new one was published here). The
                    // replacement is the call's share of the kind now: that end is stale, and the new share keeps its
                    // UI and its sound.
                    if !share_end_is_current(call.share_frames(kind)) {
                        continue;
                    }
                    if kind == ShareKind::Screen {
                        // The share's sound ends with it (the media ends only its video), and no lookup or choice
                        // reaches the ended share.
                        perform(ctx, &call, sound.ended()).await;
                    }
                    let mut share = ctx.states.borrow().voice.share;
                    set_kind(&mut share, kind, false);
                    let _ = ctx.voice.set_share(share).await;
                }
                Some(()) = async { match app_ended.as_mut() { Some(rx) => rx.recv().await, None => std::future::pending().await } } => {
                    // The application went away: the share goes on without sound, and says so.
                    let mut share = ctx.states.borrow().voice.share;
                    share.screen_audio = false;
                    *ctx.app_sound.lock().unwrap_or_else(|p| p.into_inner()) = None;
                    let _ = ctx.voice.set_share(share).await;
                    ctx.voice.set_notice(Some(VoiceNotice::ShareWithoutSound));
                }
                // `None`: the session was dropped (`Drop for MediaSession`), and the call is left below.
                // A command for another call (the one a move goes to, pressed there while this call's loop still ran) is
                // kept for it.
                // A stop applies at once to whatever share runs, and cancels any kept start of its kind.
                cmd = commands.recv() => match cmd {
                    None => break,
                    Some(Command::Forget) => pending.keep(Command::Forget),
                    Some(c) if c.is_stop() => {
                        pending.cancel(&c);
                        handle_command(ctx, &call, &mut opens, &opened_tx, &mut sound, c).await;
                    }
                    Some(c) if pending.wants(&c, link.call_id) => pending.keep(c),
                    Some(c) => handle_command(ctx, &call, &mut opens, &opened_tx, &mut sound, c).await,
                },
                Some(done) = opened.recv() => {
                    if let Some(result) = opens.finish(done.kind, done.generation, done.result) {
                        let outcome = publish_share(ctx, &call, done.kind, result, done.options).await;
                        if done.kind == ShareKind::Screen {
                            let act = match outcome {
                                Published::Up => sound.opened(done.token),
                                Published::Kept => sound.not_opened(done.token),
                                Published::Down => sound.down(done.token),
                            };
                            perform(ctx, &call, act).await;
                        }
                    }
                }
                // session.ts `linkTick`: the link's ping, for the dock and the call bar; a read that does not settle within
                // [`LINK_STALL`] says none. The read runs beside the other arms (w2-fix2 re-review), never blocking them.
                _ = link_read.tick() => ping.start(call.link()),
                read = ping.answer() => {
                    let ms = read.as_ref().and_then(|r| r.rtt_ms);
                    ctx.voice.set_ping(link.call_id, ms);
                    // A read that failed is no data: the next one spans the gap from the last that answered.
                    if let Some(read) = read {
                        let now = Instant::now();
                        if let Some((at, before)) = &last_link {
                            let ms = now.duration_since(*at).as_secs_f64() * 1000.0;
                            ctx.voice.set_rate(link.call_id, read.rate_since(before, ms));
                            ctx.voice
                                .set_loss(link.call_id, Some(read.loss_since(before)));
                        }
                        last_link = Some((now, read));
                    }
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
    };
    tokio::select! {
        biased;
        () = session_closed(ctx.closed.clone()) => {}
        () = session => {}
    }
    // Shares still opening let their sources go now, not after the room's close.
    drop(opened);
    drop(opens);
    ctx.videos.lock().unwrap_or_else(|p| p.into_inner()).clear();
    ctx.local_videos
        .lock()
        .unwrap_or_else(|p| p.into_inner())
        .clear();
    ctx.video_seq.fetch_add(1, Ordering::Relaxed);
    // The call's end stops every share and the application sound with them (`Call::end_media`): none is published.
    *ctx.app_sound.lock().unwrap_or_else(|p| p.into_inner()) = None;
    ctx.voice.set_speaking(BTreeSet::new());
    feeder.abort();
    drop(ping);
    // The media ends at once (shares, keys, the router), then the room is left. The live guard becomes the room's
    // closing one: a dropped session does not wait for the close, this window's next join and this session's next call
    // do (bounded; a close that hangs goes on in the background).
    call.end_media();
    close_room(live.closing(), ctx.room_closes.clone(), call.leave()).await;
}

/// Runs one share command in the call's loop (or one kept for this call from before its media took it up).
async fn handle_command(
    ctx: &Ctx,
    call: &Call,
    opens: &mut Opens,
    opened_tx: &mpsc::UnboundedSender<Opened>,
    sound: &mut AppSoundSlot,
    command: Command,
) {
    match command {
        Command::Share(kind, false, _, _) => {
            if kind == ShareKind::Screen {
                sound.stopped();
            }
            stop_share(ctx, call, opens, kind).await;
        }
        Command::Share(kind, true, options, _) => {
            // Not sent for a screen (`share_screen` is); one would start without any sound.
            if kind == ShareKind::Screen {
                sound.started(NO_TOKEN, &crate::screens::voice::ShareSound::Person(None));
            }
            opens.start(ctx, opened_tx, kind, None, options, NO_TOKEN);
        }
        Command::ShareScreen {
            source,
            options,
            token,
            sound: chosen,
            ..
        } => {
            sound.started(token, &chosen);
            opens.start(ctx, opened_tx, ShareKind::Screen, source, options, token);
        }
        Command::AppAudio(source, _) => perform(ctx, call, sound.asked(source)).await,
        Command::WindowSound(token, app) => {
            perform(ctx, call, sound.window_found(token, app)).await;
        }
        // The kept starts already went (the loop's own `Pending`).
        Command::Forget => {}
        Command::Quality(options, _) => {
            if ctx.states.borrow().voice.share.screen
                && let Err(e) = call.configure_video(ShareKind::Screen, options).await
            {
                log::warn!("voice: the new stream quality did not apply: {e}");
            }
        }
    }
}

/// Whether `call_id` is still the call this device's media should be in.
fn is_current(links: &watch::Receiver<Option<Arc<MediaLink>>>, call_id: [u8; 16]) -> bool {
    links
        .borrow()
        .as_ref()
        .is_some_and(|l| l.call_id == call_id)
}

/// Resolves once `call_id` is no longer the current media link (left, or replaced by another call's).
async fn superseded(mut links: watch::Receiver<Option<Arc<MediaLink>>>, call_id: [u8; 16]) {
    loop {
        if !is_current(&links, call_id) {
            return;
        }
        if links.changed().await.is_err() {
            return;
        }
    }
}

/// The media did not start (the room refused, no key in time): the person is told the join failed and the call is
/// left, so the signalling never stays "in" a call nobody hears.
/// Tells the state which remote devices are in the call's SFU room, which of them are verified (an authenticated audio or
/// video stream) and which send a screen: the stage's tiles (`voice::tiles`).
fn publish_peers(
    ctx: &Ctx,
    sfu: &[SfuRemote],
    audio: &BTreeSet<DeviceId>,
    video: &BTreeSet<DeviceId>,
    screens: &BTreeSet<DeviceId>,
) {
    // A device with an authenticated stream is in the room whatever the participant list said first.
    let verified: BTreeSet<DeviceId> = audio.union(video).copied().collect();
    let mut remotes: Vec<SfuRemote> = sfu.to_vec();
    for d in &verified {
        if !remotes.contains(&SfuRemote::Device(*d)) {
            remotes.push(SfuRemote::Device(*d));
        }
    }
    ctx.voice
        .set_media_peers(remotes, verified, screens.clone());
}

fn failed(
    ctx: &Ctx,
    links: &watch::Receiver<Option<Arc<MediaLink>>>,
    call_id: [u8; 16],
    feeder: tokio::task::JoinHandle<()>,
    e: &dilla_media::MediaError,
) {
    log::warn!("voice: the call's media did not start: {e}");
    feeder.abort();
    // A join that failed because the person moved on (the instance dropped this device from the old call) says nothing
    // and leaves nothing: the leave would end the call that replaced it.
    if !is_current(links, call_id) {
        return;
    }
    ctx.voice.set_notice(Some(VoiceNotice::MediaFailed));
    drop(ctx.voice.leave());
}

/// Whether a share's end (`Call::shares_ended`) is the call's current share of its kind going: `live` is what the call
/// holds of that kind now (`Call::share_frames`). An ended share takes itself out of the call before it is reported,
/// and this loop publishes shares in line, so a share still held when the end is read is a newer one that replaced it.
fn share_end_is_current(live: Option<u64>) -> bool {
    live.is_none()
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

/// A source the factory opened (or did not) for the share of `kind`, as the `generation`-th selection of that kind.
struct Opened {
    kind: ShareKind,
    generation: u64,
    /// The UI's token of the start ([`Command::ShareScreen`]); [`NO_TOKEN`] for a camera.
    token: u64,
    options: ShareOptions,
    result: Result<Box<dyn CaptureSource>, OpenError>,
}

/// Sets its flag when dropped: a selection whose waiting task goes away (the call's runtime shutting down with its
/// window) withdraws the open it waits for.
struct CancelOnDrop(Arc<AtomicBool>);

impl Drop for CancelOnDrop {
    fn drop(&mut self) {
        self.0.store(true, Ordering::Release);
    }
}

/// The selections in flight, one per kind at most: a newer one, or switching the share off, withdraws the older one, and
/// whatever it opens late is dropped (its capture stops) without being published.
#[derive(Default)]
struct Opens {
    next: u64,
    pending: HashMap<ShareKind, (u64, Arc<AtomicBool>)>,
}

impl Opens {
    /// Asks the factory for a source off the call's loop; its answer comes back on `done`.
    fn start(
        &mut self,
        ctx: &Ctx,
        done: &mpsc::UnboundedSender<Opened>,
        kind: ShareKind,
        source: Option<DesktopSource>,
        options: ShareOptions,
        token: u64,
    ) {
        self.withdraw(kind);
        self.next += 1;
        let generation = self.next;
        let cancel = Arc::new(AtomicBool::new(false));
        self.pending.insert(kind, (generation, cancel.clone()));
        let request = ShareRequest {
            kind,
            options,
            source,
            cancel: cancel.clone(),
        };
        let (factory, done) = (ctx.sources.clone(), done.clone());
        tokio::spawn(async move {
            let _withdraw = CancelOnDrop(cancel);
            let result = tokio::task::spawn_blocking(move || factory(&request))
                .await
                .unwrap_or_else(|e| Err(OpenError::Unavailable(e.to_string())));
            let _ = done.send(Opened {
                kind,
                generation,
                token,
                options,
                result,
            });
        });
    }

    /// Withdraws the selection of `kind` in flight, if any.
    fn withdraw(&mut self, kind: ShareKind) {
        if let Some((_, cancel)) = self.pending.remove(&kind) {
            cancel.store(true, Ordering::Release);
        }
    }

    /// The answer of a selection: kept when it is still the current one of its kind, dropped otherwise.
    fn finish<T>(&mut self, kind: ShareKind, generation: u64, result: T) -> Option<T> {
        match self.pending.get(&kind) {
            Some((current, _)) if *current == generation => {
                self.pending.remove(&kind);
                Some(result)
            }
            _ => None,
        }
    }
}

/// What the loop does with the screen share's application sound.
#[derive(Debug, PartialEq)]
enum Act {
    Nothing,
    Publish(dilla_media::app_audio::AppSource),
    Unpublish,
}

/// The token of a start the UI did not name (a camera, or a screen start without a picker's source): no window's sound
/// names it, because the UI's tokens start at 1.
const NO_TOKEN: u64 = 0;

/// The screen share's application sound, decided in one place so the published sound is never another application's
/// than the one the live share shows (Task 44 fix rounds 1 to 4). Every answer is an [`Act`] that [`perform`] carries
/// out, and no order in which the loop handles its arms changes the outcome:
/// - A start carries its own sound ([`Command::ShareScreen`]); while it opens the running share (if any) keeps exactly
///   its own sound, and when it comes up its sound replaces the old one entirely (none: the old one goes). A start that
///   fails, is cancelled or is replaced by a newer start takes its sound with it.
/// - The window's application ([`Command::WindowSound`]) names its start by token and only ever reaches the share that
///   start brought up: waiting while it opens, at once once it is up, never after it failed, was replaced or stopped.
/// - The person's own choice ([`Command::AppAudio`]) applies to the running share at once and to the one opening when it
///   comes up, and no window's application replaces it afterwards.
#[derive(Default)]
struct AppSoundSlot {
    /// The start being opened (the newest; an older one was withdrawn with it).
    opening: Option<StartSound>,
    /// The start whose share is up.
    live: Option<StartSound>,
}

/// One start's sound.
#[derive(Clone)]
struct StartSound {
    token: u64,
    sound: Option<dilla_media::app_audio::AppSource>,
    /// The person chose `sound`: a window's application no longer applies.
    person: bool,
}

impl AppSoundSlot {
    /// A screen share starts (`token`: the UI's; `chosen`: the sound section's state then). The running share keeps its
    /// sound until this one is up.
    fn started(&mut self, token: u64, chosen: &crate::screens::voice::ShareSound) {
        use crate::screens::voice::ShareSound;
        let (sound, person) = match chosen {
            ShareSound::Person(app) => (app.clone(), true),
            // A share starts with no window's application of its own (`ShareSound::on_share`): it is looked up anew.
            ShareSound::Unset | ShareSound::Window(_) => (None, false),
        };
        self.opening = Some(StartSound {
            token,
            sound,
            person,
        });
    }

    /// `Command::AppAudio(source)`: the person's choice (`None`: no sound). Switching off always unpublishes whatever is
    /// published, with or without a share that is up.
    fn asked(&mut self, source: Option<dilla_media::app_audio::AppSource>) -> Act {
        if let Some(opening) = &mut self.opening {
            opening.sound = source.clone();
            opening.person = true;
        }
        if let Some(live) = &mut self.live {
            live.sound = source.clone();
            live.person = true;
        }
        match source {
            Some(app) if self.live.is_some() => Act::Publish(app),
            Some(_) => Act::Nothing,
            None => Act::Unpublish,
        }
    }

    /// `Command::WindowSound`: the shared window's application, for the start `token`.
    fn window_found(&mut self, token: u64, app: Option<dilla_media::app_audio::AppSource>) -> Act {
        let Some(app) = app else {
            return Act::Nothing;
        };
        let applies = |start: &Option<StartSound>| {
            start
                .as_ref()
                .is_some_and(|s| s.token == token && !s.person)
        };
        if applies(&self.opening) {
            if let Some(opening) = &mut self.opening {
                opening.sound = Some(app);
            }
            return Act::Nothing;
        }
        if applies(&self.live) {
            if let Some(live) = &mut self.live {
                live.sound = Some(app.clone());
            }
            return Act::Publish(app);
        }
        Act::Nothing
    }

    /// The start `token` opened: its share is up (replacing the old one, if any) with exactly its own sound.
    fn opened(&mut self, token: u64) -> Act {
        let start = match self.opening.take() {
            Some(start) if start.token == token => start,
            // Not the start being opened (`Opens::finish` drops those, so this does not happen): the share that is up
            // gets no sound, and nothing found later applies to it.
            other => {
                self.opening = other;
                StartSound {
                    token,
                    sound: None,
                    person: true,
                }
            }
        };
        let act = start.sound.clone().map_or(Act::Unpublish, Act::Publish);
        self.live = Some(start);
        act
    }

    /// The start `token` did not open or was cancelled: the running share (if any) is untouched, so its sound is too.
    fn not_opened(&mut self, token: u64) -> Act {
        if self.opening.as_ref().is_some_and(|s| s.token == token) {
            self.opening = None;
        }
        Act::Nothing
    }

    /// The start `token` opened but its track did not publish: the share is over, and its sound goes.
    fn down(&mut self, token: u64) -> Act {
        self.not_opened(token);
        self.live = None;
        Act::Unpublish
    }

    /// The screen share's source ended by itself: the share is over, and its sound goes with it.
    fn ended(&mut self) -> Act {
        self.stopped();
        Act::Unpublish
    }

    /// A screen share was stopped: what waited for it goes, and so does every later lookup.
    fn stopped(&mut self) {
        self.opening = None;
        self.live = None;
    }
}

/// Carries out an [`Act`].
async fn perform(ctx: &Ctx, call: &Call, act: Act) {
    match act {
        Act::Nothing => {}
        Act::Publish(app) => app_audio(ctx, call, Some(app)).await,
        Act::Unpublish => app_audio(ctx, call, None).await,
    }
}

/// Starts or stops the screen share's application sound (parity plan Package 3). Only with a running screen share; the
/// instance's sharing slot says whether the share carries sound. A capture that cannot start leaves the share silent and
/// says so.
async fn app_audio(ctx: &Ctx, call: &Call, source: Option<dilla_media::app_audio::AppSource>) {
    let mut share = ctx.states.borrow().voice.share;
    let Some(source) = source.filter(|_| share.screen) else {
        *ctx.app_sound.lock().unwrap_or_else(|p| p.into_inner()) = None;
        call.unpublish_app_audio().await;
        if share.screen_audio {
            share.screen_audio = false;
            let _ = ctx.voice.set_share(share).await;
        }
        return;
    };
    match call.publish_app_audio(&source).await {
        Ok(_) => {
            *ctx.app_sound.lock().unwrap_or_else(|p| p.into_inner()) = Some(source);
            share.screen_audio = true;
            let _ = ctx.voice.set_share(share).await;
            ctx.voice.set_notice(None);
        }
        Err(e) => {
            *ctx.app_sound.lock().unwrap_or_else(|p| p.into_inner()) = None;
            log::warn!("voice: the application sound did not start: {e}");
            ctx.voice.set_notice(Some(VoiceNotice::ShareWithoutSound));
        }
    }
}

/// Stops a share: a selection in flight is withdrawn, the track is unpublished (its source let go) and the instance's
/// sharing slot is freed.
async fn stop_share(ctx: &Ctx, call: &Call, opens: &mut Opens, kind: ShareKind) {
    opens.withdraw(kind);
    let mut next = ctx.states.borrow().voice.share;
    call.unpublish_video(kind).await;
    if kind == ShareKind::Screen {
        // The share's sound goes with it.
        *ctx.app_sound.lock().unwrap_or_else(|p| p.into_inner()) = None;
        call.unpublish_app_audio().await;
    }
    set_kind(&mut next, kind, false);
    let _ = ctx.voice.set_share(next).await;
}

/// Publishes an opened source: the instance's sharing slot first, then the track. A selection that gave no source (the
/// person cancelled it, the source was unavailable) publishes nothing and says so.
async fn publish_share(
    ctx: &Ctx,
    call: &Call,
    kind: ShareKind,
    result: Result<Box<dyn CaptureSource>, OpenError>,
    options: ShareOptions,
) -> Published {
    let source = match result {
        Ok(source) => source,
        Err(e) => {
            if e != OpenError::Cancelled {
                log::warn!("voice: the {kind:?} share did not open: {e}");
            }
            ctx.voice.set_notice(Some(VoiceNotice::ShareCancelled));
            return Published::Kept;
        }
    };
    let mut next = ctx.states.borrow().voice.share;
    let audio = source.has_audio();
    set_kind(&mut next, kind, true);
    if kind == ShareKind::Screen {
        next.screen_audio = audio;
    }
    match ctx.voice.set_share(next).await {
        Ok(Ok(())) => {}
        refused => {
            // The instance refused the sharing slot (all sharers taken, not in the call's room, ...): nothing is
            // published, and the person is told the share did not start.
            let code = match &refused {
                Ok(Err(e)) => refusal_code(e),
                _ => "stopped",
            };
            log::warn!("voice: the {kind:?} share's slot was refused ({code})");
            ctx.voice.set_notice(Some(VoiceNotice::ShareCancelled));
            return Published::Kept;
        }
    }
    if let Err(e) = call.publish_video_with(source, options).await {
        log::warn!("voice: the share did not start: {e}");
        set_kind(&mut next, kind, false);
        let _ = ctx.voice.set_share(next).await;
        return Published::Down;
    }
    if kind == ShareKind::Screen && !audio {
        ctx.voice.set_notice(Some(VoiceNotice::ShareWithoutSound));
    }
    Published::Up
}

/// A refusal's stable code for the log: an instance's or the core's `E_*` code (only the code, never text after it),
/// else only the kind of failure (never its text, which may carry an address).
fn refusal_code(e: &dilla_client::ClientError) -> &str {
    use dilla_client::ClientError;
    match e {
        ClientError::Sync(code) if code.starts_with("E_") => code
            .split(|c: char| !(c.is_ascii_uppercase() || c.is_ascii_digit() || c == '_'))
            .next()
            .unwrap_or("sync"),
        ClientError::Sync(_) => "sync",
        ClientError::Network(_) => "network",
        ClientError::Revoked => "revoked",
        ClientError::Stopped => "stopped",
        _ => "other",
    }
}

/// What [`publish_share`] did.
enum Published {
    /// The source is published: the share is up (or switched to it).
    Up,
    /// Nothing was published (the source did not open, or the instance refused): a running share is untouched.
    Kept,
    /// The track did not publish: the share's slot is freed, the share is over.
    Down,
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

    const CALL_A: [u8; 16] = [1; 16];
    const CALL_B: [u8; 16] = [2; 16];
    const OPTIONS: ShareOptions = ShareOptions {
        resolution: dilla_media::ShareResolution::P720,
        fps: 15,
    };

    fn start(token: u64, call: [u8; 16]) -> Command {
        Command::ShareScreen {
            call,
            source: None,
            options: OPTIONS,
            token,
            sound: crate::screens::voice::ShareSound::Unset,
        }
    }

    /// What a list of kept commands is, for the assertions: (kind, call's first byte or token).
    fn shape(commands: &[Command]) -> Vec<(&'static str, u64)> {
        let byte = |call: &Option<[u8; 16]>| call.map_or(0, |c| u64::from(c[0]));
        commands
            .iter()
            .map(|c| match c {
                Command::ShareScreen { token, .. } => ("start", *token),
                Command::Share(ShareKind::Screen, _, _, call) => ("screen", byte(call)),
                Command::Share(ShareKind::Camera, _, _, call) => ("camera", byte(call)),
                Command::Quality(_, call) => ("quality", u64::from(call[0])),
                Command::AppAudio(_, call) => ("sound", byte(call)),
                Command::WindowSound(token, _) => ("window", *token),
                Command::Forget => ("forget", 0),
            })
            .collect()
    }

    fn camera(on: bool, call: Option<[u8; 16]>) -> Command {
        Command::Share(ShareKind::Camera, on, OPTIONS, call)
    }

    /// Share-drop flake: a share made for a call before the session took it up runs in that call, the newest of each
    /// kind only; one made for another call is dropped.
    #[test]
    fn kept_commands_run_only_in_their_call_newest_of_each_kind() {
        let mut p = Pending::default();
        p.keep(start(1, CALL_A));
        p.keep(Command::Quality(OPTIONS, CALL_A));
        p.keep(start(2, CALL_A));
        p.keep(Command::Quality(OPTIONS, CALL_B));
        p.keep(camera(true, Some(CALL_A)));
        assert_eq!(
            shape(&p.take_for(CALL_A)),
            [("start", 2), ("camera", 1)],
            "the newest start and camera of call A, not call B's quality"
        );
        assert!(p.take_for(CALL_A).is_empty(), "taken once");
        assert!(p.take_for(CALL_B).is_empty(), "the rest was dropped");
    }

    /// Security review S1: a stop is never kept (nor dropped for naming another call or none): it cancels the kept
    /// start of its kind, whatever call that names, and the loop runs it at once.
    #[test]
    fn a_stop_cancels_the_kept_start_of_its_kind_and_is_never_kept() {
        let mut p = Pending::default();
        p.keep(camera(true, Some(CALL_A)));
        p.keep(start(1, CALL_B));
        p.keep(Command::AppAudio(None, Some(CALL_A)));
        assert!(p.0.iter().all(|c| !c.is_stop()), "no stop kept");
        p.keep(camera(false, None));
        assert_eq!(shape(&p.0), [("start", 1)], "the camera start went");
        p.keep(Command::Share(
            ShareKind::Screen,
            false,
            OPTIONS,
            Some(CALL_A),
        ));
        assert!(
            p.0.is_empty(),
            "a stop naming call A cancels call B's start too"
        );
        assert!(camera(false, None).is_stop());
        assert!(Command::AppAudio(None, None).is_stop());
        assert!(!camera(true, Some(CALL_A)).is_stop());
    }

    /// A window's sound follows the start it names: kept with it, gone when a newer start or a stop replaces it, and
    /// never kept without one.
    #[test]
    fn a_kept_window_sound_follows_its_start() {
        let mut p = Pending::default();
        p.keep(Command::WindowSound(1, None));
        assert!(p.0.is_empty(), "no start of its own");
        p.keep(start(1, CALL_A));
        p.keep(Command::WindowSound(1, None));
        assert_eq!(shape(&p.0), [("start", 1), ("window", 1)]);
        assert!(p.wants(&Command::WindowSound(1, None), CALL_B));
        p.keep(start(2, CALL_A));
        assert_eq!(
            shape(&p.0),
            [("start", 2)],
            "the replaced start's sound went"
        );
        p.keep(Command::WindowSound(2, None));
        p.keep(Command::Share(ShareKind::Screen, false, OPTIONS, None));
        assert!(p.0.is_empty(), "a stop takes the start and its sound");
    }

    /// A call's loop takes its own commands and leaves the others for the call they name; a call whose media ended
    /// forgets what was kept for it.
    #[test]
    fn a_call_leaves_another_call_s_commands_and_forgets_its_own() {
        let mut p = Pending::default();
        assert!(!p.wants(&start(1, CALL_A), CALL_A));
        assert!(p.wants(&start(1, CALL_B), CALL_A));
        assert!(
            !p.wants(&Command::WindowSound(1, None), CALL_A),
            "no start kept"
        );
        p.keep(start(1, CALL_A));
        p.keep(Command::WindowSound(1, None));
        p.keep(camera(true, Some(CALL_B)));
        p.forget(CALL_A);
        assert_eq!(shape(&p.0), [("camera", 2)]);
        assert_eq!(shape(&p.take_for(CALL_B)), [("camera", 2)]);
    }

    /// Security review S2: a new link keeps only its own call's starts (none without a link), and the window's call
    /// ending ([`Command::Forget`]) drops every kept start, so neither carries into a later call.
    #[test]
    fn a_new_link_or_the_call_s_end_drops_other_kept_starts() {
        let mut p = Pending::default();
        p.keep(start(1, CALL_A));
        p.keep(Command::WindowSound(1, None));
        p.keep(camera(true, Some(CALL_B)));
        p.retain_current(Some(CALL_B));
        assert_eq!(shape(&p.0), [("camera", 2)], "only the new link's call");
        p.retain_current(None);
        assert!(p.0.is_empty(), "no link, nothing kept");
        p.keep(start(2, CALL_A));
        p.keep(Command::Forget);
        assert!(p.0.is_empty(), "the call's end drops the kept start");
        p.keep(start(3, CALL_A));
        assert_eq!(
            shape(&p.take_for(CALL_A)),
            [("start", 3)],
            "a press after the end is kept"
        );
    }

    /// Client review M1: the end of a share that a newer one replaced (the call holds a share of the kind when the end is
    /// read) is stale: the new share's flag, UI and sound stay. With nothing held, the end is the share's going.
    #[test]
    fn a_replaced_share_s_late_end_is_ignored() {
        assert!(!share_end_is_current(Some(0)), "a new share, no frame yet");
        assert!(!share_end_is_current(Some(120)));
        assert!(share_end_is_current(None));
    }

    fn app(pid: u32, label: &str) -> dilla_media::app_audio::AppSource {
        dilla_media::app_audio::AppSource {
            serial: u64::from(pid),
            pid,
            label: label.to_owned(),
        }
    }

    fn music() -> dilla_media::app_audio::AppSource {
        app(2, "Music")
    }

    /// What waits in the loop's command queue behind a start.
    enum Queued {
        Ask(Option<dilla_media::app_audio::AppSource>),
        Found(u64, Option<dilla_media::app_audio::AppSource>),
    }

    /// The slot driven the way the loop drives it, with what the session publishes (`published`: what the sound row
    /// shows). Commands queue in order; an open's end may be handled before what is queued (the loop's `select!` is
    /// not biased), so the tests say when the queue is handled ([`Sim::deliver`]).
    #[derive(Default)]
    struct Sim {
        slot: AppSoundSlot,
        published: Option<dilla_media::app_audio::AppSource>,
        live: bool,
        /// The token of the start being opened.
        opening: Option<u64>,
        queue: std::collections::VecDeque<Queued>,
    }

    impl Sim {
        fn run(&mut self, act: Act) {
            match act {
                Act::Nothing => {}
                Act::Publish(app) => self.published = self.live.then_some(app),
                Act::Unpublish => self.published = None,
            }
        }

        /// The person picks a source: the UI's `share_source` sends one [`Command::ShareScreen`] with the sound
        /// section's state. What was sent before the start is handled before it.
        fn start(&mut self, token: u64, sound: &crate::screens::voice::ShareSound) {
            self.deliver();
            self.slot.started(token, sound);
            self.opening = Some(token);
        }

        /// The person's own choice in the sound section.
        fn ask(&mut self, source: Option<dilla_media::app_audio::AppSource>) {
            self.queue.push_back(Queued::Ask(source));
        }

        /// The window's lookup lands.
        fn found(&mut self, token: u64, app: Option<dilla_media::app_audio::AppSource>) {
            self.queue.push_back(Queued::Found(token, app));
        }

        /// The loop handles what is queued.
        fn deliver(&mut self) {
            while let Some(q) = self.queue.pop_front() {
                let act = match q {
                    Queued::Ask(source) => self.slot.asked(source),
                    Queued::Found(token, app) => self.slot.window_found(token, app),
                };
                self.run(act);
            }
        }

        /// The open ends: `Published::Up`, `Kept` (failed or cancelled) or `Down`.
        fn end(&mut self, outcome: Published) {
            let token = self.opening.take().expect("a start is opening");
            let act = match outcome {
                Published::Up => {
                    self.live = true;
                    self.slot.opened(token)
                }
                Published::Kept => self.slot.not_opened(token),
                Published::Down => {
                    self.live = false;
                    self.slot.down(token)
                }
            };
            self.run(act);
        }

        /// The share's source ends by itself (the loop's `ended` arm).
        fn ended(&mut self) {
            self.deliver();
            self.live = false;
            self.opening = None;
            let act = self.slot.ended();
            self.run(act);
        }

        /// The person stops the share.
        fn stop(&mut self) {
            self.deliver();
            self.slot.stopped();
            self.live = false;
            self.opening = None;
            self.published = None;
        }
    }

    use crate::screens::voice::ShareSound;

    /// A share running window `a`'s sound (start 1, its lookup, its open).
    fn live_with(a: &dilla_media::app_audio::AppSource) -> Sim {
        let mut sim = Sim::default();
        sim.start(1, &ShareSound::Unset);
        sim.found(1, Some(a.clone()));
        sim.deliver();
        sim.end(Published::Up);
        assert_eq!(sim.published.as_ref(), Some(a), "window A shares its sound");
        sim
    }

    /// Task 44 fix round 3: a share running window A's sound, switched to window B: while B opens, A's share keeps
    /// A's sound (B's application, found meanwhile, waits); B failing or being cancelled leaves A's sound published;
    /// B opening replaces it with B's.
    #[test]
    fn a_source_switch_keeps_the_old_sound_until_the_new_source_opens() {
        let (a, b) = (app(10, "A"), app(20, "B"));
        for outcome in [Published::Kept, Published::Up] {
            let mut sim = live_with(&a);
            // The switch to B (the UI's share_sound went Window(a) to Unset): nothing changes while B opens.
            sim.start(2, &ShareSound::Unset);
            sim.deliver();
            assert_eq!(sim.published, Some(a.clone()), "while B opens");
            sim.found(2, Some(b.clone()));
            sim.deliver();
            assert_eq!(sim.published, Some(a.clone()), "B's sound waits for B");
            let up = matches!(outcome, Published::Up);
            sim.end(outcome);
            assert_eq!(sim.published, Some(if up { b.clone() } else { a.clone() }));
            // B's lookup landing after a failed start is dropped.
            if !up {
                sim.found(2, Some(b.clone()));
                sim.deliver();
                assert_eq!(sim.published, Some(a.clone()), "a late B is dropped");
            }
        }
        // A window whose application is silent replaces A's sound with none once it opens.
        let mut sim = live_with(&a);
        sim.start(2, &ShareSound::Unset);
        sim.deliver();
        assert_eq!(sim.published, Some(a));
        sim.end(Published::Up);
        assert_eq!(sim.published, None);
    }

    /// Task 44 fix round 4: the start and its sound are one command, so the order in which a source's open and the
    /// queued commands are handled (the loop's `select!` is not biased) never matters: a switch from A to B, or a first
    /// share, ends with exactly the window's own application whether its lookup lands before or after the open, and
    /// B's share never carries A's sound, not even for one pass of the loop.
    #[test]
    fn a_switch_brings_the_new_window_s_sound_whichever_lands_first() {
        let (a, b) = (app(10, "A"), app(20, "B"));
        for lookup_first in [true, false] {
            let mut sim = live_with(&a);
            sim.start(2, &ShareSound::Unset);
            sim.found(2, Some(b.clone()));
            if lookup_first {
                sim.deliver();
                assert_eq!(
                    sim.published,
                    Some(a.clone()),
                    "A's share keeps A's sound while B opens"
                );
                sim.end(Published::Up);
            } else {
                // B opens before anything queued behind its start is handled.
                sim.end(Published::Up);
                assert_eq!(sim.published, None, "B's share never carries A's sound");
                sim.deliver();
            }
            assert_eq!(
                sim.published,
                Some(b.clone()),
                "lookup first: {lookup_first}"
            );
            // A first share, in either order.
            let mut sim = Sim::default();
            sim.start(1, &ShareSound::Unset);
            sim.found(1, Some(a.clone()));
            if lookup_first {
                sim.deliver();
                sim.end(Published::Up);
            } else {
                sim.end(Published::Up);
                assert_eq!(sim.published, None);
                sim.deliver();
            }
            assert_eq!(
                sim.published,
                Some(a.clone()),
                "first share, lookup first: {lookup_first}"
            );
        }
    }

    /// Task 44 fix round 4: window A's lookup lands while A is still opening, then the person picks B, which opens
    /// before its queued commands are handled: A's application never reaches B's share, and A's late lookup is dropped.
    #[test]
    fn a_superseded_start_s_sound_never_reaches_the_next_share() {
        let (a, b) = (app(10, "A"), app(20, "B"));
        let mut sim = Sim::default();
        sim.start(1, &ShareSound::Unset);
        sim.found(1, Some(a.clone()));
        sim.deliver();
        sim.start(2, &ShareSound::Unset);
        sim.end(Published::Up);
        assert_eq!(sim.published, None, "B's share never carries A's sound");
        sim.deliver();
        sim.found(1, Some(a.clone()));
        sim.deliver();
        assert_eq!(sim.published, None, "a late A is dropped");
        sim.found(2, Some(b.clone()));
        sim.deliver();
        assert_eq!(sim.published, Some(b));
    }

    /// Task 44 fix round 4: the person's own choice in the sound section, made while a source switch is still opening,
    /// applies to the running share at once and stays whatever the switch does: switched off, A's share goes silent
    /// immediately, stays silent when B fails, and B (opening) stays silent even when B's lookup was already on its way;
    /// an application chosen meanwhile plays at once and goes on with B.
    #[test]
    fn the_person_s_sound_choice_applies_at_once_during_a_switch() {
        let (a, b, x) = (app(10, "A"), app(20, "B"), app(30, "X"));
        for up in [false, true] {
            for lookup_before in [true, false] {
                let mut sim = live_with(&a);
                sim.start(2, &ShareSound::Unset);
                sim.deliver();
                if lookup_before {
                    sim.found(2, Some(b.clone()));
                }
                sim.ask(None);
                sim.deliver();
                assert_eq!(sim.published, None, "switched off at once");
                sim.found(2, Some(b.clone()));
                sim.deliver();
                assert_eq!(sim.published, None, "B's lookup never overrides the person");
                sim.end(if up { Published::Up } else { Published::Kept });
                assert_eq!(sim.published, None, "off stays off");
                sim.found(2, Some(b.clone()));
                sim.deliver();
                assert_eq!(sim.published, None, "a late B stays off");
            }
        }
        let mut sim = live_with(&a);
        sim.start(2, &ShareSound::Unset);
        sim.deliver();
        sim.ask(Some(x.clone()));
        sim.deliver();
        assert_eq!(
            sim.published,
            Some(x.clone()),
            "the chosen application at once"
        );
        sim.found(2, Some(b));
        sim.deliver();
        sim.end(Published::Up);
        assert_eq!(sim.published, Some(x), "and with B");
    }

    /// Task 44 fix round 4: two quick switches A to B to C, C opening before the queued commands are handled: C's share
    /// carries neither A's nor B's sound, only C's own once found, and B's lookup is dropped whenever it lands.
    #[test]
    fn two_quick_switches_keep_only_the_last_start_s_sound() {
        let (a, b, c) = (app(10, "A"), app(20, "B"), app(30, "C"));
        let mut sim = live_with(&a);
        sim.start(2, &ShareSound::Unset);
        sim.start(3, &ShareSound::Unset);
        sim.found(2, Some(b.clone()));
        sim.end(Published::Up);
        assert_eq!(
            sim.published, None,
            "C's share carries neither A's nor B's sound"
        );
        sim.deliver();
        assert_eq!(sim.published, None, "B's lookup is dropped");
        sim.found(3, Some(c.clone()));
        sim.deliver();
        assert_eq!(sim.published, Some(c.clone()));
        sim.found(2, Some(b));
        sim.deliver();
        assert_eq!(sim.published, Some(c));
    }

    /// Task 44 fix rounds 2 and 3: a window share whose application is found and whose open then fails leaves no
    /// sound for the next share (a screen, or a window with a silent application), and the person's own choice goes out.
    #[test]
    fn a_failed_window_share_leaves_no_sound_for_the_next_share() {
        let mut sim = Sim::default();
        sim.start(1, &ShareSound::Unset);
        sim.found(1, Some(music()));
        sim.deliver();
        sim.end(Published::Kept);
        sim.start(2, &ShareSound::Unset);
        sim.end(Published::Up);
        sim.deliver();
        assert_eq!(sim.published, None);
        // A person who switched the sound off gets none; one who chose an application gets it.
        let mut sim = Sim::default();
        sim.start(1, &ShareSound::Person(None));
        // The UI looks nothing up for a person who chose (voice/mod.rs `share_source`).
        sim.deliver();
        sim.end(Published::Up);
        assert_eq!(sim.published, None);
        let mut sim = Sim::default();
        sim.start(1, &ShareSound::Person(Some(app(30, "Radio"))));
        sim.deliver();
        sim.end(Published::Up);
        assert_eq!(sim.published, Some(app(30, "Radio")));
    }

    /// Task 44 fix round 3: a screen share whose track did not publish is over and its application sound goes with
    /// it ([`AppSoundSlot::down`] is what the loop performs), and a stop drops what waited and every late lookup.
    #[test]
    fn a_failed_screen_start_unpublishes_its_sound() {
        let mut sim = live_with(&music());
        sim.start(2, &ShareSound::Unset);
        sim.deliver();
        sim.end(Published::Down);
        assert_eq!(sim.published, None);
        let mut sim = live_with(&music());
        sim.stop();
        sim.found(1, Some(music()));
        sim.deliver();
        assert_eq!(sim.published, None, "a lookup after the stop is dropped");
    }

    /// Task 44 fix round 5: a screen share whose source ends by itself takes its application sound with it (what the
    /// session publishes, and so the sound row, is none), and nothing found or chosen later reaches the ended share.
    #[test]
    fn a_share_that_ends_by_itself_takes_its_sound_with_it() {
        let (a, x) = (app(10, "A"), app(30, "X"));
        let mut sim = live_with(&a);
        sim.ended();
        assert_eq!(sim.published, None, "the ended share's sound goes");
        // The row then reads the UI's sound, which the share's end clears too.
        assert_eq!(ShareSound::Window(a.clone()).ended().app(), None);
        sim.found(1, Some(a));
        sim.ask(Some(x));
        sim.deliver();
        assert_eq!(sim.published, None, "nothing reaches the ended share");
    }

    /// Task 44 fix round 5: the person switching the sound off unpublishes whatever is published, with or without a
    /// share that is up (a share still opening, or none at all).
    #[test]
    fn switching_the_sound_off_always_unpublishes() {
        for opening in [false, true] {
            let mut sim = Sim {
                published: Some(app(10, "A")),
                ..Sim::default()
            };
            if opening {
                sim.start(1, &ShareSound::Unset);
            }
            sim.ask(None);
            sim.deliver();
            assert_eq!(sim.published, None, "opening: {opening}");
        }
    }

    /// Share fix review I1: a dropped session waits only while a call's media is live. A call whose media already
    /// ended (and which is only closing its room) holds the drop up not at all; a live one holds it up until its media
    /// ends, however its task ends, and no longer than the bound.
    #[test]
    fn a_dropped_session_waits_only_while_a_call_s_media_is_live() {
        use std::time::{Duration, Instant};
        let live = Arc::new(MediaLive::default());
        // No call, or one that ended: at once, whatever the bound.
        let at = Instant::now();
        assert!(live.wait_ended(Duration::from_secs(3)));
        drop(live.begin());
        assert!(live.wait_ended(Duration::from_secs(3)));
        assert!(
            at.elapsed() < Duration::from_millis(500),
            "{:?}",
            at.elapsed()
        );
        // A live call: the bound passes without its end.
        let guard = live.begin();
        assert!(!live.wait_ended(Duration::from_millis(50)));
        // Its media ends on another thread: the wait ends with it, well before the bound.
        let at = Instant::now();
        let ender = std::thread::spawn(move || {
            std::thread::sleep(Duration::from_millis(20));
            drop(guard);
        });
        assert!(live.wait_ended(Duration::from_secs(3)));
        assert!(at.elapsed() < Duration::from_secs(1), "{:?}", at.elapsed());
        ender.join().unwrap();
    }

    /// Call fix round B1: a left call's room is open until its close ends. The window's next join waits for that
    /// ([`MediaLive::room_open`]), while a dropped session's wait stays on the media only (the close runs on after it).
    #[tokio::test(start_paused = true)]
    async fn a_left_call_s_room_stays_open_until_its_close_ends() {
        let live = Arc::new(MediaLive::default());
        assert!(!live.room_open(), "no call");
        let guard = live.begin();
        assert!(live.room_open(), "a live call's room is open");
        let (release, held) = tokio::sync::oneshot::channel::<()>();
        let close = tokio::spawn(close_room(
            guard.closing(),
            dilla_media::RoomCloses::default(),
            async move {
                let _ = held.await;
            },
        ));
        tokio::task::yield_now().await;
        assert!(
            live.wait_ended(std::time::Duration::ZERO),
            "the media ended: a dropped session does not wait for the close"
        );
        tokio::time::sleep(ROOM_CLOSE_WAIT / 2).await;
        assert!(
            live.room_open(),
            "the close is held open: the next join waits"
        );
        release.send(()).unwrap();
        close.await.unwrap();
        assert!(!live.room_open(), "the close ended: the next join goes on");
    }

    /// Call fix round B1 (review focus 3) and review I1: a close that never ends holds the next join for
    /// [`ROOM_CLOSE_WAIT`] only, which is less than the join's own bound ([`super::super::HANDOVER_WAIT`]). Then
    /// `close_room` returns, so the session's loop takes the next call's link and its media starts; the close runs on
    /// on the runtime, never cancelled, registered with the session's room closes (its drop waits for it).
    #[tokio::test(start_paused = true)]
    async fn a_room_close_that_hangs_does_not_hold_the_next_call() {
        assert!(ROOM_CLOSE_WAIT < super::super::HANDOVER_WAIT);
        let live = Arc::new(MediaLive::default());
        let closes = dilla_media::RoomCloses::default();
        let guard = live.begin();
        let ended = Arc::new(std::sync::atomic::AtomicBool::new(false));
        let close_ended = ended.clone();
        let (release, held) = tokio::sync::oneshot::channel::<()>();
        let close = tokio::spawn(close_room(guard.closing(), closes.clone(), async move {
            let _ = held.await;
            close_ended.store(true, Ordering::SeqCst);
        }));
        tokio::time::sleep(ROOM_CLOSE_WAIT - std::time::Duration::from_millis(10)).await;
        assert!(live.room_open());
        assert!(!close.is_finished());
        tokio::time::sleep(std::time::Duration::from_millis(20)).await;
        assert!(!live.room_open(), "past the bound the next join goes on");
        assert!(
            close.is_finished(),
            "the session's loop is free for the next call's media"
        );
        close.await.unwrap();
        // The next call's media starts while the old close still runs.
        let next = live.begin();
        assert!(live.room_open());
        tokio::time::sleep(std::time::Duration::from_secs(60)).await;
        assert!(!ended.load(Ordering::SeqCst), "the close still runs");
        // Released, the close is not polled before the registry's wait: that wait (the session drop's) covers it.
        release.send(()).unwrap();
        closes.closed().await;
        assert!(
            ended.load(Ordering::SeqCst),
            "the session's room closes waited for the close, which ran to its end"
        );
        drop(next);
    }

    /// Call fix review m2: two rooms closing at once (an old close handed to the runtime, a newer call's close) keep the
    /// room open until both ended; one ending never clears the other.
    #[test]
    fn overlapping_closes_do_not_clear_each_other() {
        let live = Arc::new(MediaLive::default());
        let a = live.begin().closing();
        let b = live.begin().closing();
        assert!(live.room_open());
        drop(a);
        assert!(live.room_open(), "b still closes");
        drop(b);
        assert!(!live.room_open());
    }

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

    /// Package 8b (call/volume.ts `gainOf`): each other device in the call plays at its person's volume times the output
    /// volume, its stream at the person's stream volume; this device's own are not played.
    #[test]
    fn the_person_volumes_become_the_call_s_gains() {
        use dilla_client::state::voice::VoiceParticipant;
        let mut s = AppState::default();
        let channel = dilla_client::state::ChannelId([9; 16]);
        s.voice.call.channel = Some(channel);
        s.voice.place(
            channel,
            Some([9; 16]),
            vec![
                VoiceParticipant {
                    user_id: dilla_client::state::UserId([0xaa; 16]),
                    device_id: DeviceId([1; 16]),
                    flags: 0,
                    own: true,
                },
                VoiceParticipant {
                    user_id: dilla_client::state::UserId([0xbb; 16]),
                    device_id: DeviceId([2; 16]),
                    flags: 0,
                    own: false,
                },
            ],
        );
        let bob = "bb".repeat(16);
        let dev = "02".repeat(16);
        let mut v = VoiceValues::default();
        v.volumes.insert(bob.clone(), 50);
        v.stream_volumes.insert(bob, 0);
        v.output_volume = 200;
        let g = gains(&v, &s);
        assert_eq!(g.len(), 2, "the own device is not played: {g:?}");
        assert_eq!(g[&(dev.clone(), false)], 1.0, "50 % of 200 %");
        assert_eq!(g[&(dev, true)], 0.0, "the stream mute");
        s.voice.call.channel = None;
        assert!(gains(&v, &s).is_empty(), "no call, no gains");
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
