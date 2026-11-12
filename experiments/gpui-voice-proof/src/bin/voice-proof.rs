//! One LiveKit participant with the dilla-sframe/1 hook installed, driven over stdin/stdout by the
//! Playwright proof (browser/voice-proof.spec.ts).
//!
//! ```text
//! voice-proof --url WS --token JWT --leaf N [--publish audio|video] [--tone HZ]
//!             [--width W --height H --fps F]
//!             [--epoch E --base-key HEX --roster LEAF:DEVICE,...] [--control passthrough]
//! ```
//!
//! `--epoch` installs that epoch on both sides before the room is joined, as the first
//! `install` + `send` would, so no frame is published before a key exists.
//!
//! stdin, one command per line:
//!   install <epoch> <base key hex> <leaf>:<device hex>,...   receive side (KeyRing::install_epoch)
//!   send <epoch> <base key hex>                             send side (SframeSender new / rekey)
//!   stats                                                   prints {"event":"stats",...}
//!   quit
//! stdout: one JSON object per line ({"event": ...}).

use std::collections::VecDeque;
use std::sync::Arc;
use std::time::{Duration, Instant};

use futures_util::StreamExt;
use gpui_voice_proof::media::{Hook, PassthroughControl, ProofMedia};
use gpui_voice_proof::sframe::{NK, Slot};
use gpui_voice_proof::signal::{self, Tone};
use livekit::e2ee::key_provider::{KeyProvider, KeyProviderOptions};
use livekit::e2ee::{E2eeOptions, EncryptionType};
use livekit::options::{TrackPublishOptions, VideoCodec, VideoEncoding};
use livekit::prelude::*;
use livekit::webrtc::audio_source::native::NativeAudioSource;
use livekit::webrtc::audio_source::{AudioSourceOptions, RtcAudioSource};
use livekit::webrtc::audio_stream::native::NativeAudioStream;
use livekit::webrtc::prelude::{
    AudioFrame, I420Buffer, VideoFrame, VideoResolution, VideoRotation,
};
use livekit::webrtc::video_source::RtcVideoSource;
use livekit::webrtc::video_source::native::NativeVideoSource;
use livekit::webrtc::video_stream::native::NativeVideoStream;
use serde_json::json;
use tokio::io::{AsyncBufReadExt, BufReader};
use tokio::sync::Mutex;

struct Args {
    url: String,
    token: String,
    leaf: u16,
    publish: Option<String>,
    tone: f64,
    width: u32,
    height: u32,
    fps: u32,
    epoch: Option<(u64, String, String)>,
    control: Option<String>,
}

fn parse_roster(s: &str) -> Vec<(u16, [u8; 16])> {
    s.split(',')
        .map(|e| {
            let (l, d) = e.split_once(':').expect("leaf:device");
            (l.parse().expect("leaf"), unhex::<16>(d))
        })
        .collect()
}

fn args() -> Args {
    let mut a = Args {
        url: String::new(),
        token: String::new(),
        leaf: 0,
        publish: None,
        tone: 440.0,
        width: 640,
        height: 360,
        fps: 30,
        epoch: None,
        control: None,
    };
    let (mut epoch, mut key, mut roster) = (None, None, None);
    let mut it = std::env::args().skip(1);
    while let Some(k) = it.next() {
        let v = it.next().unwrap_or_else(|| panic!("{k} needs a value"));
        match k.as_str() {
            "--url" => a.url = v,
            "--token" => a.token = v,
            "--leaf" => a.leaf = v.parse().expect("--leaf"),
            "--publish" => a.publish = Some(v),
            "--tone" => a.tone = v.parse().expect("--tone"),
            "--width" => a.width = v.parse().expect("--width"),
            "--height" => a.height = v.parse().expect("--height"),
            "--fps" => a.fps = v.parse().expect("--fps"),
            "--epoch" => epoch = Some(v.parse().expect("--epoch")),
            "--base-key" => key = Some(v),
            "--roster" => roster = Some(v),
            "--control" => a.control = Some(v),
            _ => panic!("unknown argument {k}"),
        }
    }
    a.epoch = match (epoch, key, roster) {
        (Some(e), Some(k), Some(r)) => Some((e, k, r)),
        (None, None, None) => None,
        _ => panic!("--epoch, --base-key and --roster go together"),
    };
    a
}

fn emit(v: serde_json::Value) {
    println!("{v}");
}

fn unhex<const N: usize>(s: &str) -> [u8; N] {
    assert_eq!(s.len(), 2 * N, "expected {N} hex bytes");
    let mut out = [0u8; N];
    for (i, b) in out.iter_mut().enumerate() {
        *b = u8::from_str_radix(&s[2 * i..2 * i + 2], 16).expect("hex digit");
    }
    out
}

/// A participant identity as protocol/05 reads it: exactly 32 lowercase hex digits.
fn device_of(identity: &str) -> Option<[u8; 16]> {
    (identity.len() == 32
        && identity
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b)))
    .then(|| unhex::<16>(identity))
}

fn slot_of(source: TrackSource) -> Option<Slot> {
    match source {
        TrackSource::Microphone => Some(Slot::Microphone),
        TrackSource::Camera => Some(Slot::Camera),
        TrackSource::Screenshare => Some(Slot::ScreenVideo),
        TrackSource::ScreenshareAudio => Some(Slot::ScreenAudio),
        TrackSource::Unknown => None,
    }
}

/// What the decoded remote media looked like.
#[derive(Default)]
struct Received {
    audio_frames: u64,
    /// The last second of decoded mono samples.
    audio_tail: VecDeque<f32>,
    /// Peak frequencies of each completed second, in order.
    audio_peaks: Vec<f64>,
    video_frames: u64,
    video_size: (u32, u32),
}

#[tokio::main]
async fn main() {
    let a = args();
    let media = ProofMedia::new(a.leaf);
    if let Some((epoch, key, roster)) = &a.epoch {
        media.install_epoch(*epoch, &unhex::<NK>(key), &parse_roster(roster));
        media
            .send_epoch(*epoch, &unhex::<NK>(key))
            .expect("send epoch");
    }
    // `--control passthrough` is the negative control only (REPORT.md): a hook that passes every
    // frame unchanged, whose plaintext a dilla receiver must refuse.
    let hook: Box<dyn webrtc_sys::dilla_transform::FrameTransform> = match a.control.as_deref() {
        None => Box::new(Hook(media.clone())),
        Some("passthrough") => Box::new(PassthroughControl),
        Some(other) => panic!("--control {other}: only passthrough"),
    };
    assert!(
        webrtc_sys::dilla_transform::install(hook),
        "the dilla hook must be the first and only one"
    );

    let mut options = RoomOptions::default();
    // The publication's encryption flag must say encrypted (a browser drops a NONE-flagged one
    // whole). LiveKit attaches no FrameCryptor transformer while the dilla hook is installed
    // (core/vendor/webrtc-sys, DILLA PATCH), so the key provider is never used. The deprecated
    // `e2ee` field is used rather than `encryption` because that one also encrypts the data
    // channel, which dilla does not.
    #[allow(deprecated)]
    {
        options.e2ee = Some(E2eeOptions {
            encryption_type: EncryptionType::Custom,
            key_provider: KeyProvider::new(KeyProviderOptions::default()),
        });
    }
    options.auto_subscribe = true;
    let (room, mut events) = Room::connect(&a.url, &a.token, options)
        .await
        .expect("connect");
    emit(json!({"event": "connected", "identity": room.local_participant().identity().as_str()}));

    let received = Arc::new(Mutex::new(Received::default()));

    // Remote tracks: bind each to its device and slot, then decode it.
    {
        let media = media.clone();
        let received = received.clone();
        tokio::spawn(async move {
            while let Some(ev) = events.recv().await {
                if let RoomEvent::TrackSubscribed {
                    track,
                    publication,
                    participant,
                } = ev
                {
                    let track_id = track.rtc_track().id();
                    let identity = participant.identity().as_str().to_owned();
                    let source = publication.source();
                    // An unverified stream (identity not 32 lowercase hex, or no slot) is never
                    // bound, so its frames are held, then dropped.
                    if let (Some(device), Some(slot)) = (device_of(&identity), slot_of(source)) {
                        media.bind_remote(&track_id, device, slot);
                    }
                    emit(
                        json!({"event": "subscribed", "track_id": track_id, "identity": identity,
                        "source": format!("{source:?}")}),
                    );
                    match track {
                        RemoteTrack::Audio(t) => {
                            let received = received.clone();
                            tokio::spawn(async move {
                                let mut stream =
                                    NativeAudioStream::new(t.rtc_track(), signal::RATE as i32, 1);
                                let mut since = 0usize;
                                while let Some(frame) = stream.next().await {
                                    let mut r = received.lock().await;
                                    r.audio_frames += 1;
                                    for &s in frame.data.iter() {
                                        r.audio_tail.push_back(f32::from(s) / 32768.0);
                                        if r.audio_tail.len() > signal::RATE as usize {
                                            r.audio_tail.pop_front();
                                        }
                                    }
                                    since += frame.data.len();
                                    if since >= signal::RATE as usize {
                                        since = 0;
                                        let tail: Vec<f32> = r.audio_tail.iter().copied().collect();
                                        r.audio_peaks.push(signal::peak_hz(&tail, signal::RATE));
                                    }
                                }
                            });
                        }
                        RemoteTrack::Video(t) => {
                            let received = received.clone();
                            tokio::spawn(async move {
                                let mut stream = NativeVideoStream::new(t.rtc_track());
                                while let Some(frame) = stream.next().await {
                                    let mut r = received.lock().await;
                                    r.video_frames += 1;
                                    r.video_size = (frame.buffer.width(), frame.buffer.height());
                                }
                            });
                        }
                    }
                }
            }
        });
    }

    // Local media.
    match a.publish.as_deref() {
        Some("audio") => {
            let source =
                NativeAudioSource::new(AudioSourceOptions::default(), signal::RATE, 1, 100);
            let track =
                LocalAudioTrack::create_audio_track("tone", RtcAudioSource::Native(source.clone()));
            let track_id = track.rtc_track().id();
            media.bind_local(&track_id, Slot::Microphone);
            room.local_participant()
                .publish_track(
                    LocalTrack::Audio(track),
                    TrackPublishOptions {
                        source: TrackSource::Microphone,
                        dtx: false,
                        red: false,
                        ..Default::default()
                    },
                )
                .await
                .expect("publish audio");
            emit(json!({"event": "published", "kind": "audio", "track_id": track_id}));
            let hz = a.tone;
            tokio::spawn(async move {
                let mut tone = Tone::new(hz);
                loop {
                    let samples = tone.next_frame();
                    let frame = AudioFrame {
                        data: samples.into(),
                        sample_rate: signal::RATE,
                        num_channels: 1,
                        samples_per_channel: signal::FRAME as u32,
                    };
                    if source.capture_frame(&frame).await.is_err() {
                        break;
                    }
                }
            });
        }
        Some("video") => {
            let (w, h, fps) = (a.width, a.height, a.fps);
            let source = NativeVideoSource::new(
                VideoResolution {
                    width: w,
                    height: h,
                },
                false,
            );
            let track = LocalVideoTrack::create_video_track(
                "pattern",
                RtcVideoSource::Native(source.clone()),
            );
            let track_id = track.rtc_track().id();
            media.bind_local(&track_id, Slot::Camera);
            let bitrate = if w * h >= 1920 * 1080 {
                4_000_000
            } else {
                1_000_000
            };
            room.local_participant()
                .publish_track(
                    LocalTrack::Video(track),
                    TrackPublishOptions {
                        source: TrackSource::Camera,
                        video_codec: VideoCodec::VP8,
                        simulcast: false,
                        video_encoding: Some(VideoEncoding {
                            max_bitrate: bitrate,
                            max_framerate: f64::from(fps),
                        }),
                        ..Default::default()
                    },
                )
                .await
                .expect("publish video");
            emit(json!({"event": "published", "kind": "video", "track_id": track_id}));
            tokio::spawn(async move {
                let start = Instant::now();
                let mut n: u64 = 0;
                let mut tick =
                    tokio::time::interval(Duration::from_micros(1_000_000 / u64::from(fps)));
                loop {
                    tick.tick().await;
                    let (y, u, v) = signal::pattern_i420(w, h, n);
                    let mut buf = I420Buffer::new(w, h);
                    {
                        let (by, bu, bv) = buf.data_mut();
                        by.copy_from_slice(&y);
                        bu.copy_from_slice(&u);
                        bv.copy_from_slice(&v);
                    }
                    let mut frame = VideoFrame::new(VideoRotation::VideoRotation0, buf);
                    frame.timestamp_us =
                        i64::try_from(start.elapsed().as_micros()).unwrap_or(i64::MAX);
                    source.capture_frame(&frame);
                    n += 1;
                }
            });
        }
        Some(other) => panic!("--publish {other}: audio or video"),
        None => {}
    }

    // Commands.
    let mut lines = BufReader::new(tokio::io::stdin()).lines();
    while let Ok(Some(line)) = lines.next_line().await {
        let parts: Vec<&str> = line.split_whitespace().collect();
        match parts.as_slice() {
            ["install", epoch, key, roster] => {
                media.install_epoch(
                    epoch.parse().expect("epoch"),
                    &unhex::<NK>(key),
                    &parse_roster(roster),
                );
                emit(json!({"event": "installed", "epoch": epoch}));
            }
            ["send", epoch, key] => {
                media
                    .send_epoch(epoch.parse().expect("epoch"), &unhex::<NK>(key))
                    .expect("send epoch");
                emit(json!({"event": "sending", "epoch": epoch}));
            }
            ["stats"] => {
                let r = received.lock().await;
                let tail: Vec<f32> = r.audio_tail.iter().copied().collect();
                let rms =
                    (tail.iter().map(|x| x * x).sum::<f32>() / tail.len().max(1) as f32).sqrt();
                emit(json!({
                    "event": "stats",
                    "media": media.stats(),
                    "audio": {
                        "frames": r.audio_frames,
                        "peak_hz": if tail.is_empty() { 0.0 } else { signal::peak_hz(&tail, signal::RATE) },
                        "rms": rms,
                        "peaks": r.audio_peaks,
                    },
                    "video": {"frames": r.video_frames, "width": r.video_size.0, "height": r.video_size.1},
                }));
            }
            ["quit"] => break,
            _ => emit(json!({"event": "error", "line": line})),
        }
    }
    room.close().await.ok();
    emit(json!({"event": "closed"}));
}
