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

use std::collections::{BTreeMap, HashMap, VecDeque};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use dilla_core::sframe::Slot;
use futures_util::StreamExt;
use livekit::e2ee::key_provider::{KeyProvider, KeyProviderOptions};
use livekit::e2ee::{E2eeOptions, EncryptionType};
use livekit::options::TrackPublishOptions;
use livekit::prelude::*;
use livekit::webrtc::audio_source::native::NativeAudioSource;
use livekit::webrtc::audio_source::{AudioSourceOptions, RtcAudioSource};
use livekit::webrtc::audio_stream::native::NativeAudioStream;
use livekit::webrtc::peer_connection_factory::IceServer;
use livekit::webrtc::prelude::AudioFrame;
use livekit::{PlatformAudio, RecordingDeviceId};
use tokio::sync::{mpsc, watch};
use tokio::task::JoinHandle;
use zeroize::Zeroizing;

use crate::noise::{FRAME_SAMPLES, NoiseMode, NoiseProcessor, RATE};
use crate::transform::{CallMedia, Router, Stats, hex};
use crate::{EpochKeys, MediaError, ffi};

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
    /// A platform microphone (`None`: the system default), through libwebrtc's audio device module
    /// and its audio processing. `Off` and `Standard` only: the device path gives no samples to
    /// process in between, so `Enhanced` is refused (use [`Mic::Pcm`] with a capture of your own).
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
    /// Play the call through the platform's speakers (the audio device module). Off for a headless
    /// participant.
    pub playout: bool,
    /// How long the join waits for the first epoch key before it gives up (nothing published).
    pub key_timeout: Duration,
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
        }
    }
}

/// The speaking level (RMS of the last 100 ms, 0 to 1) of each verified remote device, by device
/// id (hex). A device appears only once one of its frames has authenticated.
pub type RemoteLevels = BTreeMap<String, f32>;

/// The last second of decoded audio of each remote device (tests and diagnostics).
type Tails = Arc<Mutex<HashMap<String, VecDeque<i16>>>>;

pub struct Call {
    room: Room,
    router: Arc<Router>,
    media: Arc<CallMedia>,
    local: Option<LocalAudioTrack>,
    platform: Option<PlatformAudio>,
    levels: watch::Receiver<RemoteLevels>,
    tails: Tails,
    muted: Arc<AtomicBool>,
    /// The published microphone is a platform device (only that one can be switched).
    device_mic: bool,
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
        if let Mic::Device {
            noise: NoiseMode::Enhanced,
            ..
        } = options.mic
        {
            return Err(MediaError::Unsupported(
                "enhanced noise suppression needs PCM capture (Mic::Pcm)".into(),
            ));
        }
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
        let call = Self::connect(token, keys, options, router, media).await?;
        guard.armed = false;
        Ok(call)
    }

    async fn connect(
        token: LiveKitToken,
        keys: Arc<dyn EpochKeys>,
        options: CallOptions,
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
                let base_key = Zeroizing::new(base_key);
                match media.install_epoch(epoch, &base_key, &roster, own_leaf) {
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

        let platform = if options.playout || matches!(options.mic, Mic::Device { .. }) {
            Some(PlatformAudio::new().map_err(|e| MediaError::Audio(e.to_string()))?)
        } else {
            None
        };

        let (room, events) = Room::connect(&token.url, &token.token, room_options)
            .await
            .map_err(|e| MediaError::Room(e.to_string()))?;

        let (levels_tx, levels) = watch::channel(RemoteLevels::new());
        let tails: Tails = Arc::default();
        let muted = Arc::new(AtomicBool::new(false));
        let mut tasks = vec![tokio::spawn(remote_tracks(
            events,
            media.clone(),
            levels_tx,
            tails.clone(),
        ))];

        let mut call = Call {
            room,
            router,
            media,
            local: None,
            platform,
            levels,
            tails,
            muted,
            device_mic: false,
            tasks: Vec::new(),
        };
        match call.publish_mic(options.mic, &mut tasks).await {
            Ok(()) => {
                call.tasks = tasks;
                Ok(call)
            }
            Err(e) => {
                call.tasks = tasks;
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
        let device = matches!(mic, Mic::Device { .. });
        let (track, feeder) = match mic {
            Mic::None => return Ok(()),
            Mic::Device { id, noise } => {
                let platform = self
                    .platform
                    .as_ref()
                    .ok_or_else(|| MediaError::Audio("no audio device module".into()))?;
                if let Some(id) = id {
                    platform
                        .set_recording_device(&RecordingDeviceId::from_unchecked_guid(&id))
                        .map_err(|e| MediaError::Audio(e.to_string()))?;
                }
                platform
                    .set_noise_suppression(noise == NoiseMode::Standard, false)
                    .map_err(|e| MediaError::Audio(e.to_string()))?;
                (
                    LocalAudioTrack::create_audio_track("microphone", platform.rtc_source()),
                    None,
                )
            }
            Mic::Pcm { frames, noise } => {
                let processor = NoiseProcessor::new(noise)?;
                let source = NativeAudioSource::new(
                    AudioSourceOptions {
                        echo_cancellation: false,
                        noise_suppression: false,
                        auto_gain_control: false,
                    },
                    RATE,
                    1,
                    100,
                );
                let track = LocalAudioTrack::create_audio_track(
                    "microphone",
                    RtcAudioSource::Native(source.clone()),
                );
                (track, Some((frames, processor, source)))
            }
        };
        let track_id = track.rtc_track().id();
        self.media.expect_local(&track_id, Slot::Microphone);
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
        if let Some((frames, processor, source)) = feeder {
            tasks.push(tokio::spawn(feed_pcm(
                frames,
                processor,
                source,
                self.muted.clone(),
            )));
        }
        self.local = Some(track);
        self.device_mic = device;
        Ok(())
    }

    /// Switches the platform microphone (`id` from [`crate::devices::inputs`]) under the same track
    /// and sender, so the sender's transformer and its binding stay.
    pub fn set_mic(&self, id: &str) -> Result<(), MediaError> {
        let platform = self
            .platform
            .as_ref()
            .filter(|_| self.local.is_some() && self.device_mic)
            .ok_or_else(|| {
                MediaError::Unsupported("this call does not publish a device microphone".into())
            })?;
        platform
            .switch_recording_device(&RecordingDeviceId::from_unchecked_guid(id))
            .map_err(|e| MediaError::Audio(e.to_string()))
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
        let tails = self.tails.lock().unwrap_or_else(|p| p.into_inner());
        tails.get(device).map(|t| t.iter().copied().collect())
    }

    /// What this call's transform counted.
    pub fn stats(&self) -> Stats {
        self.media.stats()
    }

    /// This device's identity in the room.
    pub fn identity(&self) -> String {
        self.room.local_participant().identity().as_str().to_owned()
    }

    /// Leaves the room and ends the call's media: its keys are dropped and every later frame of
    /// this process is dropped until the next call.
    pub async fn leave(mut self) {
        self.media.close();
        for t in self.tasks.drain(..) {
            t.abort();
        }
        if let Err(e) = self.room.close().await {
            log::warn!("dilla-media: room close: {e}");
        }
        self.router.end(&self.media);
        self.platform.take();
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

/// Binds each subscribed track and measures each verified device's audio.
async fn remote_tracks(
    mut events: mpsc::UnboundedReceiver<RoomEvent>,
    media: Arc<CallMedia>,
    levels: watch::Sender<RemoteLevels>,
    tails: Tails,
) {
    let mut readers: HashMap<String, JoinHandle<()>> = HashMap::new();
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
                if let RemoteTrack::Audio(t) = track {
                    let key = format!("{}:{}", hex(&device), publication.sid().as_str());
                    let reader = tokio::spawn(read_audio(
                        t.rtc_track(),
                        hex(&device),
                        media.clone(),
                        levels.clone(),
                        tails.clone(),
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
                    levels.send_modify(|l| {
                        l.remove(&hex(&device));
                    });
                }
            }
            RoomEvent::Disconnected { .. } => break,
            _ => {}
        }
    }
    for (_, r) in readers {
        r.abort();
    }
}

async fn read_audio(
    track: livekit::webrtc::prelude::RtcAudioTrack,
    device: String,
    media: Arc<CallMedia>,
    levels: watch::Sender<RemoteLevels>,
    tails: Tails,
) {
    let mut stream = NativeAudioStream::new(track, RATE as i32, 1);
    let mut window: Vec<f32> = Vec::with_capacity(RATE as usize / 10);
    while let Some(frame) = stream.next().await {
        {
            let mut t = tails.lock().unwrap_or_else(|p| p.into_inner());
            let tail = t.entry(device.clone()).or_default();
            tail.extend(frame.data.iter().copied());
            while tail.len() > RATE as usize {
                tail.pop_front();
            }
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
