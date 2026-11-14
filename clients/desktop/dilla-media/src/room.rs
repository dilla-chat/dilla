//! One call: the LiveKit room dillad's token opens, with every frame `dilla-sframe/1`.
//!
//! Join order, each step fail closed:
//! 1. the hook is installed (before the process's first PeerConnection) and this call's
//!    [`CallMedia`] becomes the router's current call (one per process);
//! 2. the call group's epoch keys are registered ([`EpochKeys::on_epoch`]); nothing is published
//!    before the first one is installed;
//! 3. the room is joined with LiveKit's publication encryption flag set to `CUSTOM` (a browser drops
//!    a `NONE`-flagged publication whole) and none of LiveKit's own frame crypto: the dilla hook owns
//!    every transform, so its `KeyProvider` is never used;
//! 4. each subscribed track is bound by its own transformer handle (found from the track object,
//!    never from an id the SFU chose) to its participant's device (identity: exactly 32 lowercase
//!    hex digits) and its publication's slot; an unverified stream, a kind/source mismatch or a
//!    `NONE`-flagged publication is never bound, so its frames are held and then dropped;
//! 5. the microphone is published; its sender transformer is bound to the microphone slot as it
//!    attaches.
//!
//! A sender's transformer is never replaced (libwebrtc's ChannelSend takes one transformer delegate
//! per stream, and the vendored glue refuses `RtpSender::set_track` to another track): switching
//! the microphone switches the device under the same track ([`Call::set_mic`]), and muting disables
//! the track ([`Call::set_muted`]); neither touches the sender.

use std::collections::{BTreeMap, BTreeSet, HashMap, VecDeque};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use dilla_core::sframe::Slot;
use futures_util::StreamExt;
use livekit::DisconnectReason;
use livekit::e2ee::key_provider::{KeyProvider, KeyProviderOptions};
use livekit::e2ee::{E2eeOptions, EncryptionType};
use livekit::options::TrackPublishOptions;
use livekit::prelude::*;
use livekit::webrtc::audio_source::native::NativeAudioSource;
use livekit::webrtc::audio_source::{AudioSourceOptions, RtcAudioSource};
use livekit::webrtc::audio_stream::native::NativeAudioStream;
use livekit::webrtc::native::yuv_helper;
use livekit::webrtc::peer_connection_factory::IceServer;
use livekit::webrtc::prelude::{
    AudioFrame, I420Buffer, RtcVideoSource, VideoFrame, VideoResolution, VideoRotation,
};
use livekit::webrtc::video_source::native::NativeVideoSource;
use livekit::webrtc::video_stream::native::NativeVideoStream;
use tokio::sync::{mpsc, watch};
use tokio::task::JoinHandle;
use zeroize::Zeroizing;

use crate::apm::{Apm, ApmFlags};
use crate::capture::{self, CaptureConfig};
use crate::capture_source::{CaptureSource, ShareKind, ShareOptions};
use crate::devices::{self, AudioHost, DeviceId};
use crate::noise::{FRAME_SAMPLES, NoiseMode, NoiseProcessor, RATE, embedded_model};
use crate::playout::{PlayoutSink, Speaker};
use crate::transform::{CallMedia, Router, Stats, hex};
use crate::{EpochKeys, MediaError, MediaEvent, ffi};

/// What dillad's `POST /v1/channels/{id}/calls` answers for this device (L-HTTP-102): the room's
/// LiveKit URL, the token minted for this device's leaf, and the instance's ICE servers (empty: the
/// SFU's own).
#[derive(Clone)]
pub struct LiveKitToken {
    pub url: String,
    pub token: Zeroizing<String>,
    pub ice_servers: Vec<IceServer>,
}

impl std::fmt::Debug for LiveKitToken {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("LiveKitToken")
            .field("url", &self.url)
            .finish_non_exhaustive()
    }
}

/// The microphone a call publishes.
pub enum Mic {
    /// Nothing is published (listen only).
    None,
    /// A device microphone (`None`: the system default), captured by [`capture::Mic`]: echo
    /// cancellation against the call's speaker, gain control and the noise mode (`Enhanced`
    /// included; without its model the microphone runs `Standard` and a
    /// [`MediaEvent::EnhancedUnavailable`] follows, see [`Call::events`]).
    Device {
        id: Option<String>,
        noise: NoiseMode,
    },
    /// 10 ms frames of 48 kHz mono samples the application captures, processed by `noise` before
    /// they are encoded.
    Pcm {
        frames: mpsc::Receiver<Vec<i16>>,
        noise: NoiseMode,
    },
}

pub struct CallOptions {
    pub mic: Mic,
    /// Play the call through the system's speakers ([`Speaker`], which also gives the echo canceller
    /// its reference). Off for a headless participant.
    pub playout: bool,
    /// How long the join waits for the first epoch key before it gives up (nothing published).
    pub key_timeout: Duration,
    /// Echo cancellation and gain control of a device microphone (the person's voice settings).
    pub apm_flags: ApmFlags,
    /// Join muted: the microphone track is muted before it is published, so no frame of the person's voice is ever
    /// encoded before the mute (a muted or push-to-talk join).
    pub muted: bool,
}

impl Default for CallOptions {
    fn default() -> Self {
        Self {
            mic: Mic::Device {
                id: None,
                noise: NoiseMode::Standard,
            },
            playout: true,
            key_timeout: Duration::from_secs(10),
            apm_flags: ApmFlags::default(),
            muted: false,
        }
    }
}

/// The speaking level (RMS of the last 100 ms, 0 to 1) of each verified remote device, by device
/// id (hex). A device appears only once one of its frames has authenticated.
pub type RemoteLevels = BTreeMap<String, f32>;

/// The decoded volume of a remote device that counts as heard: an RMS over one 10 ms frame above this (the web call
/// tests' `AUDIBLE_RMS`, e2e/voice/support/voice.ts).
pub const AUDIBLE_RMS: f32 = 0.005;

/// The most [`AudioEdge`]s a call keeps (the oldest go first).
const MAX_EDGES: usize = 4096;

/// One change of a verified remote audio stream's decoded audio between quiet and audible (the receive side's
/// first-audio measurement, as the web music tests sample their call audio graph): each decoded 10 ms frame is compared
/// with [`AUDIBLE_RMS`], and a change is stamped with the wall clock as the frame comes out of the decoder. A stream that
/// ends (unsubscribed) while audible ends with a quiet edge.
#[derive(Clone, Debug, PartialEq)]
pub struct AudioEdge {
    /// The device (hex).
    pub device: String,
    /// The stream (`<device>:<track sid>`): a device publishes a new track for each music item, say.
    pub stream: String,
    /// When, in milliseconds since the Unix epoch (fractional): comparable with another process's on one machine.
    pub at_ms: f64,
    /// Audible from here on (else quiet).
    pub audible: bool,
}

/// The call's audio as a whole, as the web specs hear it (the loudest of every remote analyser): audible while any
/// stream is. Answers each change `(at_ms, audible)` of `edges` (oldest first); a stream's quiet edge with no audible
/// one before it changes nothing.
pub fn heard(edges: &[AudioEdge]) -> Vec<(f64, bool)> {
    let mut on: BTreeSet<&str> = BTreeSet::new();
    let mut out = Vec::new();
    for e in edges {
        let before = !on.is_empty();
        if e.audible {
            on.insert(&e.stream);
        } else {
            on.remove(e.stream.as_str());
        }
        if !on.is_empty() != before {
            out.push((e.at_ms, !on.is_empty()));
        }
    }
    out
}

/// What a call keeps of the audio it decoded: the last second of each remote device (tests and diagnostics), the
/// quiet/audible edges ([`Call::remote_audio_edges`]) and the streams audible now.
#[derive(Default)]
struct Heard {
    tails: HashMap<String, VecDeque<i16>>,
    edges: VecDeque<AudioEdge>,
    audible: BTreeMap<String, String>,
}

impl Heard {
    fn record(&mut self, edge: AudioEdge) {
        if edge.audible {
            self.audible
                .insert(edge.stream.clone(), edge.device.clone());
        } else {
            self.audible.remove(&edge.stream);
        }
        if self.edges.len() == MAX_EDGES {
            self.edges.pop_front();
        }
        self.edges.push_back(edge);
    }

    /// Forgets the edges; each stream audible now starts the new record with an audible edge at `at_ms`.
    fn restart(&mut self, at_ms: f64) {
        self.edges = self
            .audible
            .iter()
            .map(|(stream, device)| AudioEdge {
                device: device.clone(),
                stream: stream.clone(),
                at_ms,
                audible: true,
            })
            .collect();
    }
}

type Tails = Arc<Mutex<Heard>>;

/// Follows one remote audio stream's audibility; when its reader ends (the track ended or was unsubscribed, or the
/// reader was aborted) while audible, the stream's record ends with a quiet edge.
struct EdgeTracker {
    heard: Tails,
    device: String,
    stream: String,
    audible: bool,
}

impl Drop for EdgeTracker {
    fn drop(&mut self) {
        if self.audible {
            let mut heard = self.heard.lock().unwrap_or_else(|p| p.into_inner());
            heard.record(AudioEdge {
                device: self.device.clone(),
                stream: self.stream.clone(),
                at_ms: wall_ms(),
                audible: false,
            });
        }
    }
}

/// The wall clock in milliseconds since the Unix epoch.
fn wall_ms() -> f64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0.0, |d| d.as_secs_f64() * 1000.0)
}

/// The RMS of one frame of samples, 0 to 1.
pub fn frame_rms(samples: &[i16]) -> f32 {
    if samples.is_empty() {
        return 0.0;
    }
    let sum: f32 = samples
        .iter()
        .map(|s| {
            let x = f32::from(*s) / 32768.0;
            x * x
        })
        .sum();
    (sum / samples.len() as f32).sqrt()
}

/// Follows one stream's frames: answers the edge a frame makes (`None`: no change). A frame of an unverified device is
/// quiet, so a device's first audible edge is never earlier than its first authenticated frame.
fn edge_of(
    audible: &mut bool,
    (device, stream): (&str, &str),
    samples: &[i16],
    verified: bool,
    at_ms: f64,
) -> Option<AudioEdge> {
    let now = verified && frame_rms(samples) > AUDIBLE_RMS;
    (now != *audible).then(|| {
        *audible = now;
        AudioEdge {
            device: device.to_owned(),
            stream: stream.to_owned(),
            at_ms,
            audible: now,
        }
    })
}

/// The newest decoded frame of one remote video stream, as RGBA rows.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct RemoteFrame {
    pub width: u32,
    pub height: u32,
    pub rgba: Arc<Vec<u8>>,
    /// Frames decoded on this stream so far.
    pub frames: u64,
}

/// One remote video stream: its device (hex) and slot.
#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct StreamKey {
    pub device: String,
    pub kind: ShareKind,
}

/// Every verified remote video stream's newest frame.
pub type RemoteVideos = BTreeMap<StreamKey, RemoteFrame>;

/// Why a call's room ended on its own (session.ts `endCodeOf`): the caller ends the call ("lost", session.ts
/// `onRoomEnd` → `leave('lost')`), so the signalling never stays in a room this device is no longer in.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum RoomEnd {
    /// The room was deleted or closed: the call ended.
    CallEnded,
    /// This device was removed from the room (a voice takeover by a newer device, a kick, an AFK move).
    Removed,
    /// The connection was lost for good (the SDK's reconnects gave up), or any other reason.
    Disconnected,
}

impl RoomEnd {
    pub fn of(reason: DisconnectReason) -> RoomEnd {
        match reason {
            DisconnectReason::RoomDeleted | DisconnectReason::RoomClosed => RoomEnd::CallEnded,
            DisconnectReason::ParticipantRemoved => RoomEnd::Removed,
            _ => RoomEnd::Disconnected,
        }
    }

    /// The web's error code for it.
    pub fn code(self) -> &'static str {
        match self {
            RoomEnd::CallEnded => "E_CALL_ENDED",
            RoomEnd::Removed => "E_REMOVED",
            RoomEnd::Disconnected => "E_DISCONNECTED",
        }
    }
}

/// Says the room ended (`None`: its events stopped with no reason, a lost connection); the first end stands.
fn end_room(ended: &watch::Sender<Option<RoomEnd>>, why: Option<RoomEnd>) {
    let why = why.unwrap_or(RoomEnd::Disconnected);
    ended.send_if_modified(|e| {
        if e.is_some() {
            return false;
        }
        *e = Some(why);
        true
    });
}

/// stats.ts `RTT_MAX`: the most a ping says, in ms.
const RTT_MAX_MS: f64 = 60_000.0;

/// stats.ts `rttOf` over one peer connection's report: the current round-trip time of the pair in use (the transport's
/// selected pair, else a nominated pair that succeeded), in whole ms. `None` when no pair is in use or it was never
/// answered (no round trip measured yet).
pub fn rtt_ms_of(report: &[livekit::webrtc::stats::RtcStats]) -> Option<u32> {
    use livekit::webrtc::stats::{IceCandidatePairState, RtcStats};
    let pairs = || {
        report.iter().filter_map(|s| match s {
            RtcStats::CandidatePair(p) => Some(p),
            _ => None,
        })
    };
    let selected = report.iter().find_map(|s| match s {
        RtcStats::Transport(t) if !t.transport.selected_candidate_pair_id.is_empty() => {
            Some(t.transport.selected_candidate_pair_id.as_str())
        }
        _ => None,
    });
    let pair = selected
        .and_then(|id| pairs().find(|p| p.rtc.id == id))
        .or_else(|| {
            pairs().find(|p| {
                p.candidate_pair.nominated
                    && p.candidate_pair.state == Some(IceCandidatePairState::Succeeded)
            })
        })?;
    let p = &pair.candidate_pair;
    if p.responses_received == 0 && p.current_round_trip_time <= 0.0 {
        return None;
    }
    Some(
        (p.current_round_trip_time * 1000.0)
            .round()
            .clamp(0.0, RTT_MAX_MS) as u32,
    )
}

/// A local share being published.
struct LocalShare {
    sid: TrackSid,
    feeder: JoinHandle<()>,
    sent: Arc<std::sync::atomic::AtomicU64>,
}

pub struct Call {
    room: Room,
    router: Arc<Router>,
    media: Arc<CallMedia>,
    local: Option<LocalAudioTrack>,
    apm: Apm,
    host: Arc<dyn AudioHost>,
    capture: Option<Arc<capture::Mic>>,
    speaker: Option<Arc<Speaker>>,
    events_tx: mpsc::UnboundedSender<MediaEvent>,
    events: Mutex<Option<mpsc::UnboundedReceiver<MediaEvent>>>,
    levels: watch::Receiver<RemoteLevels>,
    tails: Tails,
    muted: Arc<AtomicBool>,
    apm_flags: ApmFlags,
    videos: watch::Receiver<RemoteVideos>,
    shares: Mutex<HashMap<ShareKind, LocalShare>>,
    shares_ended: mpsc::UnboundedSender<ShareKind>,
    shares_ended_rx: Mutex<Option<mpsc::UnboundedReceiver<ShareKind>>>,
    ended: watch::Receiver<Option<RoomEnd>>,
    tasks: Vec<JoinHandle<()>>,
}

/// Ends a call's media when dropped while armed (a failed or cancelled join).
struct EndOnDrop {
    router: Arc<Router>,
    media: Arc<CallMedia>,
    armed: bool,
}

impl Drop for EndOnDrop {
    fn drop(&mut self) {
        if self.armed {
            self.router.end(&self.media);
        }
    }
}

/// A participant identity as protocol/05 reads it: exactly 32 lowercase hex digits.
pub fn device_of(identity: &str) -> Option<[u8; 16]> {
    if identity.len() != 32
        || !identity
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
    {
        return None;
    }
    let mut out = [0u8; 16];
    for (i, b) in out.iter_mut().enumerate() {
        *b = u8::from_str_radix(&identity[2 * i..2 * i + 2], 16).ok()?;
    }
    Some(out)
}

/// A publication's slot from its LiveKit source; `Unknown` is refused.
pub fn slot_of(source: TrackSource) -> Option<Slot> {
    match source {
        TrackSource::Microphone => Some(Slot::Microphone),
        TrackSource::Camera => Some(Slot::Camera),
        TrackSource::Screenshare => Some(Slot::ScreenVideo),
        TrackSource::ScreenshareAudio => Some(Slot::ScreenAudio),
        TrackSource::Unknown => None,
    }
}

impl Call {
    /// Joins with the default options: the system microphone with standard noise suppression,
    /// played through the speakers.
    pub async fn join(token: LiveKitToken, keys: Arc<dyn EpochKeys>) -> Result<Call, MediaError> {
        Self::join_with(token, keys, CallOptions::default()).await
    }

    pub async fn join_with(
        token: LiveKitToken,
        keys: Arc<dyn EpochKeys>,
        options: CallOptions,
    ) -> Result<Call, MediaError> {
        Self::join_on(token, keys, options, devices::system()).await
    }

    /// As [`join_with`](Call::join_with) on another audio host (a fake one in tests).
    pub async fn join_on(
        token: LiveKitToken,
        keys: Arc<dyn EpochKeys>,
        options: CallOptions,
        host: Arc<dyn AudioHost>,
    ) -> Result<Call, MediaError> {
        let router = Router::get()?;
        let media = CallMedia::new();
        router.begin(media.clone())?;
        // Ends the call's media on every way out but success, a dropped (cancelled) join future
        // included: the router is free again and the keys are dropped.
        let mut guard = EndOnDrop {
            router: router.clone(),
            media: media.clone(),
            armed: true,
        };
        let call = Self::connect(token, keys, options, host, router, media).await?;
        guard.armed = false;
        Ok(call)
    }

    async fn connect(
        token: LiveKitToken,
        keys: Arc<dyn EpochKeys>,
        options: CallOptions,
        host: Arc<dyn AudioHost>,
        router: Arc<Router>,
        media: Arc<CallMedia>,
    ) -> Result<Call, MediaError> {
        let (epoch_tx, mut epoch_rx) = watch::channel::<Option<u64>>(None);
        {
            // Weak: the registration never keeps an ended call's keys alive, and it is dropped
            // (answers false) once the call has ended.
            let media = Arc::downgrade(&media);
            keys.on_epoch_while(Box::new(move |epoch, base_key, roster, own_leaf| {
                let Some(media) = media.upgrade() else {
                    return false;
                };
                match media.install_epoch(epoch, base_key, &roster, own_leaf) {
                    Ok(()) => {
                        let _ = epoch_tx.send(Some(epoch));
                        true
                    }
                    Err(MediaError::Closed) => false,
                    Err(e) => {
                        log::warn!("dilla-media: epoch {epoch} not installed: {e}");
                        true
                    }
                }
            }));
        }
        let got_key = tokio::time::timeout(options.key_timeout, epoch_rx.wait_for(Option::is_some))
            .await
            .is_ok_and(|r| r.is_ok());
        if !got_key {
            return Err(MediaError::NoEpoch);
        }

        let mut room_options = RoomOptions::default();
        // The publication must be flagged encrypted (a browser drops a NONE-flagged one whole). With
        // the dilla hook installed LiveKit attaches no FrameCryptor and no packet trailer
        // (core/vendor/webrtc-sys, DILLA PATCH), so this key provider is never used. The deprecated
        // `e2ee` field, because `encryption` also encrypts the data channel, which dilla does not.
        #[allow(deprecated)]
        {
            room_options.e2ee = Some(E2eeOptions {
                encryption_type: EncryptionType::Custom,
                key_provider: KeyProvider::new(KeyProviderOptions::default()),
            });
        }
        room_options.auto_subscribe = true;
        room_options.adaptive_stream = false;
        room_options.dynacast = false;
        room_options.rtc_config.ice_servers = token.ice_servers.clone();

        let apm = Apm::default();
        let (events_tx, events_rx) = mpsc::unbounded_channel();
        // The speaker first: it gives the microphone's echo canceller its reference.
        let speaker = if options.playout {
            Some(Arc::new(Speaker::open_with(
                host.clone(),
                apm.clone(),
                Some(events_tx.clone()),
                None,
            )?))
        } else {
            None
        };

        let (room, events) = Room::connect(&token.url, &token.token, room_options)
            .await
            .map_err(|e| MediaError::Room(e.to_string()))?;

        let (levels_tx, levels) = watch::channel(RemoteLevels::new());
        let (videos_tx, videos) = watch::channel(RemoteVideos::new());
        let (shares_ended, shares_ended_rx) = mpsc::unbounded_channel();
        let tails: Tails = Arc::default();
        let muted = Arc::new(AtomicBool::new(options.muted));
        let (ended_tx, ended) = watch::channel(None);
        let mut tasks = vec![tokio::spawn(remote_tracks(
            events,
            media.clone(),
            (levels_tx, videos_tx),
            tails.clone(),
            speaker.as_ref().map(|s| s.sink()),
            ended_tx,
        ))];

        let mut call = Call {
            room,
            router,
            media,
            local: None,
            apm,
            host,
            capture: None,
            speaker,
            events_tx,
            events: Mutex::new(Some(events_rx)),
            levels,
            tails,
            muted,
            apm_flags: options.apm_flags,
            videos,
            shares: Mutex::new(HashMap::new()),
            shares_ended,
            shares_ended_rx: Mutex::new(Some(shares_ended_rx)),
            ended,
            tasks: Vec::new(),
        };
        match call.publish_mic(options.mic, &mut tasks).await {
            Ok(()) => {
                call.tasks = tasks;
                Ok(call)
            }
            Err(e) => {
                call.tasks = tasks;
                let e = retry_safe(e, call.media.has_sealed());
                call.leave().await;
                Err(e)
            }
        }
    }

    async fn publish_mic(
        &mut self,
        mic: Mic,
        tasks: &mut Vec<JoinHandle<()>>,
    ) -> Result<(), MediaError> {
        let (track, feeder) = match mic {
            Mic::None => return Ok(()),
            Mic::Device { id, noise } => {
                let mut mic = capture::Mic::open_with(
                    CaptureConfig {
                        host: self.host.clone(),
                        apm: self.apm.clone(),
                        model: Arc::new(embedded_model),
                        flags: self.apm_flags,
                    },
                    id.map(DeviceId),
                    noise,
                )?;
                let frames = mic
                    .take_frames()
                    .ok_or_else(|| MediaError::Audio("the microphone has no frames".into()))?;
                if let Some(mut events) = mic.take_events() {
                    let tx = self.events_tx.clone();
                    tasks.push(tokio::spawn(async move {
                        while let Some(e) = events.recv().await {
                            let _ = tx.send(e);
                        }
                    }));
                }
                self.capture = Some(Arc::new(mic));
                // The device chain has processed the frames (echo, gain, noise): the feeder only
                // mutes and forwards.
                let source = native_source();
                let track = LocalAudioTrack::create_audio_track(
                    "microphone",
                    RtcAudioSource::Native(source.clone()),
                );
                (
                    track,
                    Some((frames, NoiseProcessor::new(NoiseMode::Off)?, source)),
                )
            }
            Mic::Pcm { frames, noise } => {
                let processor = NoiseProcessor::new(noise)?;
                let source = native_source();
                let track = LocalAudioTrack::create_audio_track(
                    "microphone",
                    RtcAudioSource::Native(source.clone()),
                );
                (track, Some((frames, processor, source)))
            }
        };
        let track_id = track.rtc_track().id();
        self.media.expect_local(&track_id, Slot::Microphone);
        // A muted join: the track is muted (silence, still sealed) before it is published.
        if self.muted.load(Ordering::SeqCst) {
            track.mute();
        }
        self.room
            .local_participant()
            .publish_track(
                LocalTrack::Audio(track.clone()),
                TrackPublishOptions {
                    source: TrackSource::Microphone,
                    // protocol/05 "No plaintext path": no RED, no pre-connect buffer, no frame
                    // metadata; DTX's zero-byte frames carry no content.
                    dtx: false,
                    red: false,
                    preconnect_buffer: false,
                    ..Default::default()
                },
            )
            .await
            .map_err(|e| MediaError::Room(e.to_string()))?;
        // The sender transformer was bound as it attached (expect_local); one that attached
        // otherwise is bound here, and a sender without one is a failure: its frames are dropped.
        match ffi::sender_handle_audio(&track.rtc_track()) {
            Some(handle) => match self.media.bind_local(handle, Slot::Microphone) {
                Ok(()) | Err(MediaError::AlreadyBound(_)) => {}
                Err(e) => return Err(e),
            },
            None => {
                return Err(MediaError::Room(
                    "the microphone has no sender transformer".into(),
                ));
            }
        }
        if let Some((mut frames, processor, source)) = feeder {
            // Frames captured while the join was under way are stale: the feeder starts empty.
            while frames.try_recv().is_ok() {}
            tasks.push(tokio::spawn(feed_pcm(
                frames,
                processor,
                source,
                self.muted.clone(),
            )));
        }
        self.local = Some(track);
        Ok(())
    }

    /// Switches the device microphone (`id` from [`crate::devices::inputs`], `None`: the system
    /// default) and its noise mode under the same track and sender, so the sender's transformer
    /// and its binding stay. Every [`NoiseMode`] is accepted; the mode that runs is returned
    /// (`Standard` when `Enhanced` could not load, with [`MediaEvent::EnhancedUnavailable`]).
    pub async fn set_mic(
        &self,
        id: Option<&str>,
        noise: NoiseMode,
    ) -> Result<NoiseMode, MediaError> {
        let capture = self.capture.clone().ok_or_else(|| {
            MediaError::Unsupported("this call does not publish a device microphone".into())
        })?;
        let device = id.map(|i| DeviceId(i.to_owned()));
        // The switch waits for the capture worker (which may load the enhanced model): never on
        // an async worker thread.
        tokio::task::spawn_blocking(move || capture.switch(device, noise))
            .await
            .map_err(|e| MediaError::Audio(e.to_string()))?
    }

    /// Plays on another output device (`None`: the system default).
    pub async fn set_speaker(&self, id: Option<&str>) -> Result<(), MediaError> {
        let speaker = self
            .speaker
            .clone()
            .ok_or_else(|| MediaError::Unsupported("this call does not play audio".into()))?;
        let device = id.map(|i| DeviceId(i.to_owned()));
        tokio::task::spawn_blocking(move || speaker.switch(device))
            .await
            .map_err(|e| MediaError::Audio(e.to_string()))?
    }

    /// What the audio devices did that the user should see ([`MediaEvent`]), once.
    pub fn events(&self) -> Option<mpsc::UnboundedReceiver<MediaEvent>> {
        self.events.lock().unwrap_or_else(|p| p.into_inner()).take()
    }

    /// Mutes or unmutes the microphone: the track is disabled (libwebrtc encodes silence, still
    /// sealed) and the SFU is told; a PCM microphone also stops forwarding its samples.
    pub fn set_muted(&self, muted: bool) {
        self.muted.store(muted, Ordering::SeqCst);
        if let Some(track) = &self.local {
            if muted {
                track.mute();
            } else {
                track.unmute();
            }
        }
    }

    pub fn is_muted(&self) -> bool {
        self.muted.load(Ordering::SeqCst)
    }

    /// The speaking level of each verified remote device.
    pub fn remote_audio_levels(&self) -> watch::Receiver<RemoteLevels> {
        self.levels.clone()
    }

    /// The last second of decoded audio from `device` (hex), once it is verified.
    pub fn remote_audio_tail(&self, device: &str) -> Option<Vec<i16>> {
        if !self.media.verified_devices().contains(device) {
            return None;
        }
        let heard = self.tails.lock().unwrap_or_else(|p| p.into_inner());
        heard.tails.get(device).map(|t| t.iter().copied().collect())
    }

    /// Every quiet/audible edge of the verified remote devices' decoded audio since the call joined or the last
    /// [`Call::clear_audio_edges`], oldest first (at most the last 4096).
    pub fn remote_audio_edges(&self) -> Vec<AudioEdge> {
        let heard = self.tails.lock().unwrap_or_else(|p| p.into_inner());
        heard.edges.iter().cloned().collect()
    }

    /// Forgets the edges seen so far (a measurement starts over): each stream audible now starts the new record with an
    /// audible edge at this moment, so the record says what is heard from here on.
    pub fn clear_audio_edges(&self) {
        let mut heard = self.tails.lock().unwrap_or_else(|p| p.into_inner());
        heard.restart(wall_ms());
    }

    /// What this call's transform counted.
    pub fn stats(&self) -> Stats {
        self.media.stats()
    }

    /// Deafens or undeafens: every remote source plays silence (the echo reference with it), and
    /// nothing queues up meanwhile.
    /// WB security I1: the core's gate refused the call group's newest epoch, so every media key is dropped and
    /// nothing is sealed or opened until a newer epoch is installed ([`CallMedia::drop_keys`]).
    pub fn drop_keys(&self) {
        self.media.drop_keys();
    }

    /// This call's keys and bindings, for whoever follows the call group: it calls [`CallMedia::drop_keys`] in
    /// order with the epochs it installs, so a drop never lands after a newer install.
    pub fn media(&self) -> Arc<CallMedia> {
        self.media.clone()
    }

    /// The epoch the microphone seals under, `None` while no key is installed (or after [`Call::drop_keys`]).
    pub fn sending_epoch(&self) -> Option<u64> {
        self.media.sending_epoch()
    }

    pub fn set_deafened(&self, on: bool) {
        if let Some(speaker) = &self.speaker {
            speaker.sink().set_deafened(on);
        }
    }

    /// Every verified remote video stream's newest frame.
    pub fn remote_videos(&self) -> watch::Receiver<RemoteVideos> {
        self.videos.clone()
    }

    /// The shares that ended on their own (the source stopped), once.
    pub fn shares_ended(&self) -> Option<mpsc::UnboundedReceiver<ShareKind>> {
        self.shares_ended_rx
            .lock()
            .unwrap_or_else(|p| p.into_inner())
            .take()
    }

    /// The link's ping now (session.ts `readLink`: the publisher's connection, else the subscriber's), in ms; `None`
    /// while no round trip is measured or the stats cannot be read. It never goes to the server.
    pub async fn rtt_ms(&self) -> Option<u32> {
        let stats = self.room.get_stats().await.ok()?;
        rtt_ms_of(&stats.publisher_stats).or_else(|| rtt_ms_of(&stats.subscriber_stats))
    }

    /// The room's end on its own ([`RoomEnd`]): `None` while this device is in it. A [`Call::leave`] never sets it.
    pub fn ended(&self) -> watch::Receiver<Option<RoomEnd>> {
        self.ended.clone()
    }

    /// How many frames the share of `kind` has handed the encoder.
    pub fn share_frames(&self, kind: ShareKind) -> Option<u64> {
        self.shares
            .lock()
            .unwrap_or_else(|p| p.into_inner())
            .get(&kind)
            .map(|s| s.sent.load(Ordering::Relaxed))
    }

    /// Publishes `source` as the camera or the screen share: a VP8 video track whose sender
    /// transformer is bound to the camera or screen slot before its first frame (fail closed: a
    /// sender without one is unpublished and refused). A share of the same kind is replaced.
    pub async fn publish_video(&self, source: Box<dyn CaptureSource>) -> Result<(), MediaError> {
        self.publish_video_with(source, ShareOptions::default())
            .await
    }

    /// [`Call::publish_video`] with the person's stream quality (the encoder's bitrate and frame-rate bound).
    pub async fn publish_video_with(
        &self,
        mut source: Box<dyn CaptureSource>,
        quality: ShareOptions,
    ) -> Result<(), MediaError> {
        let kind = source.kind();
        self.unpublish_video(kind).await;
        let (width, height) = source.size();
        let (slot, track_source, name) = match kind {
            ShareKind::Camera => (Slot::Camera, TrackSource::Camera, "camera"),
            ShareKind::Screen => (Slot::ScreenVideo, TrackSource::Screenshare, "screen"),
        };
        let rtc =
            NativeVideoSource::new(VideoResolution { width, height }, kind == ShareKind::Screen);
        let track = LocalVideoTrack::create_video_track(name, RtcVideoSource::Native(rtc.clone()));
        let track_id = track.rtc_track().id();
        self.media.expect_local(&track_id, slot);
        let publication = self
            .room
            .local_participant()
            .publish_track(
                LocalTrack::Video(track.clone()),
                TrackPublishOptions {
                    source: track_source,
                    video_codec: livekit::options::VideoCodec::VP8,
                    video_encoding: Some(livekit::options::VideoEncoding {
                        max_bitrate: quality.bitrate_bps(None),
                        max_framerate: f64::from(quality.fps),
                    }),
                    simulcast: false,
                    red: false,
                    dtx: false,
                    preconnect_buffer: false,
                    ..Default::default()
                },
            )
            .await
            .map_err(|e| MediaError::Room(e.to_string()))?;
        let sid = publication.sid();
        let bound = match ffi::sender_handle_video(&track.rtc_track()) {
            Some(handle) => match self.media.bind_local(handle, slot) {
                Ok(()) | Err(MediaError::AlreadyBound(_)) => Ok(()),
                Err(e) => Err(e),
            },
            None => Err(MediaError::Room(format!(
                "the {name} has no sender transformer"
            ))),
        };
        if let Err(e) = bound {
            let _ = self.room.local_participant().unpublish_track(&sid).await;
            return Err(e);
        }
        let sent = Arc::new(std::sync::atomic::AtomicU64::new(0));
        let counter = sent.clone();
        let ended = self.shares_ended.clone();
        let feeder = tokio::spawn(async move {
            let mut tick = tokio::time::interval(source.interval());
            let mut i420 = I420Buffer::new(width, height);
            loop {
                tick.tick().await;
                let Some(frame) = source.next_frame() else {
                    let _ = ended.send(kind);
                    return;
                };
                if frame.width != width || frame.height != height {
                    log::warn!("dilla-media: a {name} frame of another size dropped");
                    continue;
                }
                let (sy, su, sv) = i420.strides();
                let (y, u, v) = i420.data_mut();
                // libyuv's ABGR is R, G, B, A in memory: the RGBA rows as they are.
                yuv_helper::abgr_to_i420(
                    &frame.rgba,
                    width * 4,
                    y,
                    sy,
                    u,
                    su,
                    v,
                    sv,
                    width as i32,
                    height as i32,
                );
                let video = VideoFrame {
                    rotation: VideoRotation::VideoRotation0,
                    timestamp_us: 0,
                    frame_metadata: None,
                    buffer: &i420,
                };
                rtc.capture_frame(&video);
                counter.fetch_add(1, Ordering::Relaxed);
            }
        });
        self.shares
            .lock()
            .unwrap_or_else(|p| p.into_inner())
            .insert(kind, LocalShare { sid, feeder, sent });
        Ok(())
    }

    /// Stops the share of `kind` (nothing when there is none).
    pub async fn unpublish_video(&self, kind: ShareKind) {
        let share = self
            .shares
            .lock()
            .unwrap_or_else(|p| p.into_inner())
            .remove(&kind);
        if let Some(share) = share {
            share.feeder.abort();
            if let Err(e) = self
                .room
                .local_participant()
                .unpublish_track(&share.sid)
                .await
            {
                log::warn!("dilla-media: unpublish: {e}");
            }
        }
    }

    /// This device's identity in the room.
    pub fn identity(&self) -> String {
        self.room.local_participant().identity().as_str().to_owned()
    }

    /// Leaves the room and ends the call's media: its keys are dropped and every later frame of
    /// this process is dropped until the next call.
    pub async fn leave(mut self) {
        for (_, share) in self
            .shares
            .lock()
            .unwrap_or_else(|p| p.into_inner())
            .drain()
        {
            share.feeder.abort();
        }
        self.media.close();
        for t in self.tasks.drain(..) {
            t.abort();
        }
        if let Err(e) = self.room.close().await {
            log::warn!("dilla-media: room close: {e}");
        }
        self.router.end(&self.media);
        self.capture.take();
        self.speaker.take();
    }
}

impl Drop for Call {
    fn drop(&mut self) {
        // A call dropped without `leave` still ends its media at once; the room closes with it.
        self.router.end(&self.media);
        for t in self.tasks.drain(..) {
            t.abort();
        }
    }
}

/// A failed join's error as the caller may act on it. An `Audio` error invites a listen-only retry of the same join
/// (a new `CallMedia` for the same epoch, whose sender's counter starts at 0): that is only safe while this attempt
/// sealed nothing under that epoch's key. Today every `Audio` failure comes before the microphone is published (the
/// speaker and the capture open first), so nothing was sealed; should one ever come later, it is reported as a room
/// failure instead, which nobody retries under the same key.
pub(crate) fn retry_safe(e: MediaError, sealed: bool) -> MediaError {
    match e {
        MediaError::Audio(detail) if sealed => {
            MediaError::Room(format!("the join failed after sealing frames: {detail}"))
        }
        other => other,
    }
}

/// Feeds the application's 10 ms frames through the noise processor into the published source.
async fn feed_pcm(
    mut frames: mpsc::Receiver<Vec<i16>>,
    mut processor: NoiseProcessor,
    source: NativeAudioSource,
    muted: Arc<AtomicBool>,
) {
    while let Some(mut samples) = frames.recv().await {
        if samples.len() != FRAME_SAMPLES {
            log::warn!(
                "dilla-media: a microphone frame of {} samples dropped",
                samples.len()
            );
            continue;
        }
        if muted.load(Ordering::SeqCst) {
            samples.fill(0);
        } else if let Err(e) = processor.process(&mut samples) {
            // The processor silenced the frame: never the unprocessed samples.
            log::warn!("dilla-media: noise processor: {e}");
        }
        let frame = AudioFrame {
            data: samples.into(),
            sample_rate: RATE,
            num_channels: 1,
            samples_per_channel: FRAME_SAMPLES as u32,
        };
        if source.capture_frame(&frame).await.is_err() {
            break;
        }
    }
}

/// Binds each subscribed track and measures each verified device's audio. The room's end on its own (its
/// `Disconnected`, or its events stopping) is said on `ended`.
async fn remote_tracks(
    mut events: mpsc::UnboundedReceiver<RoomEvent>,
    media: Arc<CallMedia>,
    (levels, videos): (watch::Sender<RemoteLevels>, watch::Sender<RemoteVideos>),
    tails: Tails,
    playout: Option<PlayoutSink>,
    ended: watch::Sender<Option<RoomEnd>>,
) {
    let mut readers: HashMap<String, JoinHandle<()>> = HashMap::new();
    let mut why = None;
    while let Some(event) = events.recv().await {
        match event {
            RoomEvent::TrackSubscribed {
                track,
                publication,
                participant,
            } => {
                let identity = participant.identity().as_str().to_owned();
                // An unverified stream (identity not 32 lowercase hex), a source without a slot or a
                // NONE-flagged publication is never bound: its frames are held, then dropped.
                let (Some(device), Some(slot)) =
                    (device_of(&identity), slot_of(publication.source()))
                else {
                    log::warn!("dilla-media: unverified stream from {identity:?}: never played");
                    continue;
                };
                if publication.encryption_type() == EncryptionType::None {
                    log::warn!(
                        "dilla-media: a NONE-flagged publication of {identity}: never played"
                    );
                    continue;
                }
                let audio_slot = matches!(slot, Slot::Microphone | Slot::ScreenAudio);
                let handle = match (&track, audio_slot) {
                    (RemoteTrack::Audio(t), true) => ffi::receiver_handle_audio(&t.rtc_track()),
                    (RemoteTrack::Video(t), false) => ffi::receiver_handle_video(&t.rtc_track()),
                    _ => {
                        log::warn!(
                            "dilla-media: a track of {identity} whose kind does not match its source"
                        );
                        continue;
                    }
                };
                let Some(handle) = handle else {
                    log::warn!("dilla-media: a track of {identity} has no receiver transformer");
                    continue;
                };
                if let Err(e) = media.bind_remote(handle, device, slot) {
                    log::warn!("dilla-media: {e}");
                    continue;
                }
                if let RemoteTrack::Video(t) = &track {
                    let key = format!("{}:{}", hex(&device), publication.sid().as_str());
                    let stream = StreamKey {
                        device: hex(&device),
                        kind: if slot == Slot::Camera {
                            ShareKind::Camera
                        } else {
                            ShareKind::Screen
                        },
                    };
                    let reader = tokio::spawn(read_video(
                        t.rtc_track(),
                        stream,
                        media.clone(),
                        videos.clone(),
                    ));
                    if let Some(old) = readers.insert(key, reader) {
                        old.abort();
                    }
                }
                if let RemoteTrack::Audio(t) = track {
                    let key = format!("{}:{}", hex(&device), publication.sid().as_str());
                    let reader = tokio::spawn(read_audio(
                        t.rtc_track(),
                        (hex(&device), key.clone()),
                        media.clone(),
                        levels.clone(),
                        tails.clone(),
                        playout.clone().map(|p| (p, key.clone())),
                    ));
                    if let Some(old) = readers.insert(key, reader) {
                        old.abort();
                    }
                }
            }
            RoomEvent::TrackUnsubscribed {
                publication,
                participant,
                ..
            } => {
                if let Some(device) = device_of(participant.identity().as_str()) {
                    let key = format!("{}:{}", hex(&device), publication.sid().as_str());
                    if let Some(r) = readers.remove(&key) {
                        r.abort();
                    }
                    if let Some(p) = &playout {
                        p.remove(&key);
                    }
                    let kind = match publication.source() {
                        TrackSource::Camera => Some(ShareKind::Camera),
                        TrackSource::Screenshare => Some(ShareKind::Screen),
                        _ => None,
                    };
                    if let Some(kind) = kind {
                        videos.send_modify(|v| {
                            v.remove(&StreamKey {
                                device: hex(&device),
                                kind,
                            });
                        });
                    } else {
                        levels.send_modify(|l| {
                            l.remove(&hex(&device));
                        });
                    }
                }
            }
            RoomEvent::Disconnected { reason } => {
                why = Some(RoomEnd::of(reason));
                break;
            }
            _ => {}
        }
    }
    for (_, r) in readers {
        r.abort();
    }
    // A leave aborts this task before it closes the room, so only an end on its own reaches here.
    end_room(&ended, why);
}

async fn read_audio(
    track: livekit::webrtc::prelude::RtcAudioTrack,
    (device, key): (String, String),
    media: Arc<CallMedia>,
    levels: watch::Sender<RemoteLevels>,
    tails: Tails,
    playout: Option<(PlayoutSink, String)>,
) {
    // The source's playout slot is freed when this reader ends however it ends (its track ended,
    // or it was aborted mid-push): a push that raced the abort can never keep a slot.
    let _free = playout.as_ref().map(|(sink, key)| FreeOnDrop {
        sink: sink.clone(),
        key: key.clone(),
    });
    // The stream's audibility; it ends quiet with the reader, however the reader ends.
    let mut tracker = EdgeTracker {
        heard: tails.clone(),
        device: device.clone(),
        stream: key,
        audible: false,
    };
    let mut stream = NativeAudioStream::new(track, RATE as i32, 1);
    let mut window: Vec<f32> = Vec::with_capacity(RATE as usize / 10);
    while let Some(frame) = stream.next().await {
        // Stamped as the frame leaves the decoder.
        let at_ms = wall_ms();
        let edge = edge_of(
            &mut tracker.audible,
            (&device, &tracker.stream),
            &frame.data,
            media.verified_devices().contains(&device),
            at_ms,
        );
        {
            let mut heard = tails.lock().unwrap_or_else(|p| p.into_inner());
            let tail = heard.tails.entry(device.clone()).or_default();
            tail.extend(frame.data.iter().copied());
            while tail.len() > RATE as usize {
                tail.pop_front();
            }
            if let Some(edge) = edge {
                heard.record(edge);
            }
        }
        if let Some((sink, key)) = &playout {
            sink.push(key, &frame.data);
        }
        window.extend(frame.data.iter().map(|s| f32::from(*s) / 32768.0));
        if window.len() >= RATE as usize / 10 {
            let rms = (window.iter().map(|x| x * x).sum::<f32>() / window.len() as f32).sqrt();
            window.clear();
            // Only a device whose frames authenticated has a level (protocol/05 "verified").
            if media.verified_devices().contains(&device) {
                levels.send_modify(|l| {
                    l.insert(device.clone(), rms);
                });
            }
        }
    }
}

/// The largest remote video frame converted for a tile (4K UHD); a larger one is dropped.
pub const MAX_VIDEO_W: u32 = 3840;
pub const MAX_VIDEO_H: u32 = 2160;

/// Frees a remote source's playout slot when its reader ends.
struct FreeOnDrop {
    sink: PlayoutSink,
    key: String,
}

impl Drop for FreeOnDrop {
    fn drop(&mut self) {
        self.sink.remove(&self.key);
    }
}

/// Takes a stream out of the remote videos when its reader ends.
struct Gone(watch::Sender<RemoteVideos>, StreamKey);

impl Drop for Gone {
    fn drop(&mut self) {
        self.0.send_modify(|v| {
            v.remove(&self.1);
        });
    }
}

/// Decodes one verified remote video stream into RGBA, keeping only its newest frame (a slow view
/// drops frames, never queues them). Frames of a device that has not authenticated are not shown.
async fn read_video(
    track: livekit::webrtc::prelude::RtcVideoTrack,
    key: StreamKey,
    media: Arc<CallMedia>,
    videos: watch::Sender<RemoteVideos>,
) {
    use livekit::webrtc::video_frame::VideoFormatType;
    use livekit::webrtc::video_frame::native::VideoFrameBufferExt;
    let _gone = Gone(videos.clone(), key.clone());
    let mut stream = NativeVideoStream::new(track);
    let mut frames = 0u64;
    let mut warned = false;
    while let Some(frame) = stream.next().await {
        if !media.verified_devices().contains(&key.device) {
            continue;
        }
        let buffer = frame.buffer.as_ref();
        let (w, h) = (buffer.width(), buffer.height());
        // A stream larger than 4K is not converted: its RGBA would be unbounded memory per frame.
        if w == 0 || h == 0 || w > MAX_VIDEO_W || h > MAX_VIDEO_H {
            if !warned {
                log::warn!(
                    "dilla-media: a {w}x{h} stream from {} is larger than {MAX_VIDEO_W}x{MAX_VIDEO_H}: not shown",
                    key.device
                );
                warned = true;
            }
            continue;
        }
        let mut rgba = vec![0u8; (w * h * 4) as usize];
        if let Some(native) = buffer.as_native() {
            native.to_argb(VideoFormatType::ABGR, &mut rgba, w * 4, w as i32, h as i32);
        } else if let Some(i420) = buffer.as_i420() {
            i420.to_argb(VideoFormatType::ABGR, &mut rgba, w * 4, w as i32, h as i32);
        } else {
            continue;
        }
        frames += 1;
        let next = RemoteFrame {
            width: w,
            height: h,
            rgba: Arc::new(rgba),
            frames,
        };
        let k = key.clone();
        videos.send_modify(move |v| {
            v.insert(k, next);
        });
    }
}

/// The published microphone's source: the application (or the capture chain) does the processing.
fn native_source() -> NativeAudioSource {
    NativeAudioSource::new(
        AudioSourceOptions {
            echo_cancellation: false,
            noise_suppression: false,
            auto_gain_control: false,
        },
        RATE,
        1,
        100,
    )
}

#[cfg(test)]
mod retry_tests {
    use super::*;

    #[test]
    fn an_audio_failure_after_sealing_is_not_retryable() {
        assert!(matches!(
            retry_safe(MediaError::Audio("mic".into()), false),
            MediaError::Audio(_)
        ));
        assert!(matches!(
            retry_safe(MediaError::Audio("mic".into()), true),
            MediaError::Room(_)
        ));
        assert!(matches!(
            retry_safe(MediaError::NoEpoch, true),
            MediaError::NoEpoch
        ));
    }
}

#[cfg(test)]
mod edge_tests {
    use super::*;

    fn tone(amplitude: f32) -> Vec<i16> {
        (0..480)
            .map(|i| {
                let t = i as f32 / RATE as f32;
                ((std::f32::consts::TAU * 440.0 * t).sin() * amplitude * 32767.0) as i16
            })
            .collect()
    }

    /// The receive-side first-audio measurement: an edge at each change between quiet and audible frames, none while a
    /// state holds, and never an audible edge for a device whose frames have not authenticated.
    #[test]
    fn decoded_frames_make_an_edge_at_each_change_of_audibility() {
        let mut audible = false;
        let loud = tone(0.5);
        let silent = vec![0i16; 480];
        let faint = tone(0.001);
        assert!(frame_rms(&faint) < AUDIBLE_RMS && frame_rms(&loud) > AUDIBLE_RMS);
        assert_eq!(
            edge_of(&mut audible, ("d", "d:1"), &silent, true, 1.0),
            None
        );
        assert_eq!(edge_of(&mut audible, ("d", "d:1"), &faint, true, 2.0), None);
        assert_eq!(
            edge_of(&mut audible, ("d", "d:1"), &loud, false, 3.0),
            None,
            "unverified audio is not heard"
        );
        let up = edge_of(&mut audible, ("d", "d:1"), &loud, true, 4.0).expect("audible");
        assert_eq!((up.at_ms, up.audible, up.device.as_str()), (4.0, true, "d"));
        assert_eq!(edge_of(&mut audible, ("d", "d:1"), &loud, true, 5.0), None);
        let down = edge_of(&mut audible, ("d", "d:1"), &silent, true, 6.0).expect("quiet");
        assert_eq!((down.at_ms, down.audible), (6.0, false));
    }

    fn edge(stream: &str, at_ms: f64, audible: bool) -> AudioEdge {
        AudioEdge {
            device: "d".into(),
            stream: stream.into(),
            at_ms,
            audible,
        }
    }

    /// The call as a whole is audible while any stream is: a device's next track overlapping its last one makes no
    /// gap, and only the last quiet stream makes the call quiet.
    #[test]
    fn the_call_is_heard_while_any_stream_is_audible() {
        let edges = [
            edge("d:1", 1.0, true),
            edge("d:2", 2.0, true),
            edge("d:1", 3.0, false),
            edge("d:2", 4.0, false),
            edge("d:3", 5.0, false),
            edge("d:3", 6.0, true),
        ];
        assert_eq!(heard(&edges), [(1.0, true), (4.0, false), (6.0, true)]);
    }

    /// A stream whose reader ends while audible ends quiet, and a new record starts with what is audible now.
    #[test]
    fn an_ended_stream_ends_quiet_and_a_restart_keeps_what_is_audible() {
        let tails: Tails = Arc::default();
        let tracker = EdgeTracker {
            heard: tails.clone(),
            device: "d".into(),
            stream: "d:1".into(),
            audible: false,
        };
        tails.lock().unwrap().record(edge("d:1", 1.0, true));
        tails.lock().unwrap().record(edge("d:2", 2.0, true));
        tails.lock().unwrap().restart(3.0);
        let kept: Vec<(String, f64)> = tails
            .lock()
            .unwrap()
            .edges
            .iter()
            .map(|e| (e.stream.clone(), e.at_ms))
            .collect();
        assert_eq!(kept, [("d:1".to_owned(), 3.0), ("d:2".to_owned(), 3.0)]);
        let mut tracker = tracker;
        tracker.audible = true;
        drop(tracker);
        let last = tails.lock().unwrap().edges.back().cloned().unwrap();
        assert_eq!((last.stream.as_str(), last.audible), ("d:1", false));
        assert!(!tails.lock().unwrap().audible.contains_key("d:1"));
    }
}

#[cfg(test)]
mod end_tests {
    use super::*;

    /// session.ts `endCodeOf`: a deleted or closed room is the call's end, a removal (a voice takeover, a kick) is
    /// E_REMOVED, and anything else is a lost connection.
    #[test]
    fn a_room_end_says_why_as_the_web_does() {
        assert_eq!(
            RoomEnd::of(DisconnectReason::RoomDeleted),
            RoomEnd::CallEnded
        );
        assert_eq!(
            RoomEnd::of(DisconnectReason::RoomClosed),
            RoomEnd::CallEnded
        );
        assert_eq!(
            RoomEnd::of(DisconnectReason::ParticipantRemoved),
            RoomEnd::Removed
        );
        for other in [
            DisconnectReason::UnknownReason,
            DisconnectReason::SignalClose,
            DisconnectReason::ServerShutdown,
            DisconnectReason::DuplicateIdentity,
        ] {
            assert_eq!(RoomEnd::of(other), RoomEnd::Disconnected, "{other:?}");
        }
        assert_eq!(RoomEnd::CallEnded.code(), "E_CALL_ENDED");
        assert_eq!(RoomEnd::Removed.code(), "E_REMOVED");
        assert_eq!(RoomEnd::Disconnected.code(), "E_DISCONNECTED");
    }

    /// The room's end is said once: the first reason stands, and a room whose events stop with no reason (the SDK
    /// dropped the stream) has ended too, as a lost connection.
    #[test]
    fn the_first_room_end_stands() {
        let (tx, rx) = watch::channel(None);
        end_room(&tx, Some(RoomEnd::Removed));
        end_room(&tx, Some(RoomEnd::Disconnected));
        assert_eq!(*rx.borrow(), Some(RoomEnd::Removed));
        let (tx, rx) = watch::channel(None);
        end_room(&tx, None);
        assert_eq!(*rx.borrow(), Some(RoomEnd::Disconnected));
    }
}

#[cfg(test)]
mod rtt_tests {
    use super::*;
    use livekit::webrtc::stats::{self as st, RtcStats};

    fn pair(id: &str, rtt: f64, nominated: bool, answered: bool) -> RtcStats {
        let mut p = st::CandidatePairStats::default();
        p.rtc.id = id.into();
        p.candidate_pair.current_round_trip_time = rtt;
        p.candidate_pair.nominated = nominated;
        p.candidate_pair.state = Some(st::IceCandidatePairState::Succeeded);
        p.candidate_pair.responses_received = u64::from(answered);
        RtcStats::CandidatePair(p)
    }

    fn transport(selected: &str) -> RtcStats {
        let mut t = st::TransportStats::default();
        t.transport.selected_candidate_pair_id = selected.into();
        RtcStats::Transport(t)
    }

    /// stats.ts `rttOf`: the transport's selected pair's current round-trip time (else a nominated, succeeded pair), in
    /// whole ms; a report with no pair, or a pair never answered, knows none.
    #[test]
    fn the_ping_is_the_selected_pair_s_round_trip() {
        let report = [
            pair("a", 0.250, true, true),
            pair("b", 0.0124, false, true),
            transport("b"),
        ];
        assert_eq!(
            rtt_ms_of(&report),
            Some(12),
            "the selected pair, not the nominated one"
        );
        assert_eq!(rtt_ms_of(&[pair("a", 0.0456, true, true)]), Some(46));
        assert_eq!(
            rtt_ms_of(&[pair("a", 0.0004, true, true)]),
            Some(0),
            "under 1 ms"
        );
        assert_eq!(
            rtt_ms_of(&[pair("a", 0.0, true, false)]),
            None,
            "never answered"
        );
        assert_eq!(
            rtt_ms_of(&[pair("a", 0.02, false, true)]),
            None,
            "no pair in use"
        );
        assert_eq!(rtt_ms_of(&[]), None);
    }
}
