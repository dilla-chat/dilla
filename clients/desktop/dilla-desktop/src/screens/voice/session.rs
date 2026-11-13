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
use std::time::{Duration, Instant};

use dilla_client::Client;
use dilla_client::features::voice::{CallStatsReport, MediaLink, Voice};
use dilla_client::state::voice::{LocalShare, VoiceNotice};
use dilla_client::state::{AppState, DeviceId};
use dilla_media::apm::ApmFlags;
use dilla_media::noise::NoiseMode;
use dilla_media::{
    Call, CallOptions, CaptureSource, EpochFeed, IceServer, LiveKitToken, MediaEvent, Mic,
    RemoteVideos, ShareKind,
};
use tokio::sync::{mpsc, watch};
use zeroize::Zeroizing;

/// A remote device counts as speaking while its level is over this RMS (speaking.ts's default threshold, about
/// -34 dBFS), and for [`HANGOVER`] after.
pub const SPEAKING_RMS: f32 = 0.02;
/// How long a device stays lit after its level drops (speaking.ts `HOLD_MS`).
pub const HANGOVER: Duration = Duration::from_millis(300);

/// Turns levels into the set of devices speaking now, with a hangover (Task 19's `SpeakingDetector` replaces the
/// threshold once it merges).
#[derive(Default)]
pub struct Speaking {
    loud: HashMap<String, Instant>,
}

impl Speaking {
    /// The devices (hex) speaking at `now`, from the newest levels.
    pub fn update(&mut self, levels: &dilla_media::RemoteLevels, now: Instant) -> BTreeSet<String> {
        for (device, rms) in levels {
            if *rms >= SPEAKING_RMS {
                self.loud.insert(device.clone(), now);
            }
        }
        self.loud
            .retain(|d, at| levels.contains_key(d) && now.duration_since(*at) < HANGOVER);
        self.loud.keys().cloned().collect()
    }
}

/// The microphone is live: not muted or deafened, and in push-to-talk mode only while the key is held.
pub fn mic_open(s: &AppState, ptt_mode: bool) -> bool {
    !(s.voice.self_mute || s.voice.self_deaf) && (!ptt_mode || s.voice.ptt_held)
}

/// What a share asks for.
pub enum Command {
    Share(ShareKind, bool),
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
/// `DILLA_TEST_FRAMES=1`) uses the moving test pattern, anything else has none, which is a cancelled selection.
pub type SourceFactory = Arc<dyn Fn(ShareKind) -> Option<Box<dyn CaptureSource>> + Send + Sync>;

pub fn default_sources() -> SourceFactory {
    Arc::new(|kind| {
        let test = cfg!(feature = "automation")
            || std::env::var("DILLA_TEST_FRAMES").is_ok_and(|v| v == "1");
        test.then(|| {
            let (w, h) = match kind {
                ShareKind::Camera => (640, 360),
                ShareKind::Screen => (1280, 720),
            };
            Box::new(dilla_media::TestPattern::new(kind, w, h, 15)) as Box<dyn CaptureSource>
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
    pub fn share(&self, kind: ShareKind, on: bool) {
        let _ = self.commands.send(Command::Share(kind, on));
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
    fn ptt_mode(&self) -> bool {
        self.prefs.get::<String>("voice.mode").as_deref() == Some("ptt")
    }

    fn options(&self) -> CallOptions {
        let noise = match self.prefs.get::<String>("voice.noise").as_deref() {
            Some("off") => NoiseMode::Off,
            Some("enhanced") => NoiseMode::Enhanced,
            _ => NoiseMode::Standard,
        };
        CallOptions {
            mic: Mic::Device {
                id: self.prefs.get::<String>("voice.input"),
                noise,
            },
            apm_flags: ApmFlags {
                echo: self.prefs.get::<bool>("voice.echo").unwrap_or(true),
                agc: self.prefs.get::<bool>("voice.agc").unwrap_or(true),
            },
            ..CallOptions::default()
        }
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
                password: credential.to_string(),
            })
            .collect(),
    }
}

/// Hands each epoch the call group reaches to the media.
fn feed_epochs(link: &MediaLink) -> (Arc<EpochFeed>, tokio::task::JoinHandle<()>) {
    let feed = EpochFeed::new();
    let mut epochs = link.epochs.clone();
    let out = feed.clone();
    let task = tokio::spawn(async move {
        loop {
            let e = epochs.borrow_and_update().clone();
            if let Some(e) = e {
                out.publish(dilla_media::MediaEpoch {
                    epoch: e.epoch,
                    base_key: Zeroizing::new(*e.base_key),
                    roster: e.roster.iter().map(|(l, d, _)| (*l, *d)).collect(),
                    own_leaf: e.own_leaf,
                });
            }
            if epochs.changed().await.is_err() {
                return;
            }
        }
    });
    (feed, task)
}

async fn in_call(
    ctx: &Ctx,
    link: &MediaLink,
    links: &mut watch::Receiver<Option<Arc<MediaLink>>>,
    commands: &mut mpsc::UnboundedReceiver<Command>,
) {
    let (feed, feeder) = feed_epochs(link);
    let call = match Call::join_with(token_of(link), feed.clone(), ctx.options()).await {
        Ok(call) => call,
        Err(dilla_media::MediaError::Audio(e)) => {
            // No microphone: the call goes on listen-only, and the person is told.
            log::warn!("voice: the microphone did not open ({e}): listening only");
            ctx.voice.set_notice(Some(VoiceNotice::MicLost));
            let listen = CallOptions {
                mic: Mic::None,
                ..ctx.options()
            };
            match Call::join_with(token_of(link), feed, listen).await {
                Ok(call) => call,
                Err(e) => return failed(ctx, feeder, &e),
            }
        }
        Err(e) => return failed(ctx, feeder, &e),
    };
    let ptt = ctx.ptt_mode();
    let mut states = ctx.states.clone();
    let mut levels = call.remote_audio_levels();
    let mut videos = call.remote_videos();
    let mut events = call.events();
    let mut ended = call.shares_ended();
    let mut speaking = Speaking::default();
    let mut stats = tokio::time::interval(dilla_client::features::voice::STATS_EVERY);
    let own = ctx.states.borrow().account.device_id;
    let mut applied: Option<(bool, bool)> = None;
    loop {
        let s = states.borrow_and_update().clone();
        let want = (!mic_open(&s, ptt), s.voice.self_deaf);
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
                Command::Share(kind, on) => share(ctx, &call, kind, on).await,
            },
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
    call.leave().await;
}

fn failed(ctx: &Ctx, feeder: tokio::task::JoinHandle<()>, e: &dilla_media::MediaError) {
    log::warn!("voice: the call's media did not start: {e}");
    feeder.abort();
    ctx.voice.set_notice(Some(VoiceNotice::SpeakerLost));
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
async fn share(ctx: &Ctx, call: &Call, kind: ShareKind, on: bool) {
    let mut next = ctx.states.borrow().voice.share;
    if !on {
        call.unpublish_video(kind).await;
        set_kind(&mut next, kind, false);
        let _ = ctx.voice.set_share(next).await;
        return;
    }
    let Some(source) = (ctx.sources)(kind) else {
        ctx.voice.set_notice(Some(VoiceNotice::ShareCancelled));
        return;
    };
    let audio = source.has_audio();
    set_kind(&mut next, kind, true);
    if kind == ShareKind::Screen {
        next.screen_audio = audio;
    }
    if let Ok(Ok(())) = ctx.voice.set_share(next).await {
        if let Err(e) = call.publish_video(source).await {
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

    #[test]
    fn speaking_lights_over_the_threshold_and_holds_for_the_hangover() {
        let mut s = Speaking::default();
        let t0 = Instant::now();
        let mut levels = dilla_media::RemoteLevels::new();
        levels.insert("aa".into(), 0.05);
        levels.insert("bb".into(), 0.001);
        assert_eq!(s.update(&levels, t0), BTreeSet::from(["aa".to_owned()]));
        levels.insert("aa".into(), 0.0);
        assert!(
            s.update(&levels, t0 + Duration::from_millis(200))
                .contains("aa")
        );
        assert!(
            s.update(&levels, t0 + Duration::from_millis(400))
                .is_empty()
        );
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
