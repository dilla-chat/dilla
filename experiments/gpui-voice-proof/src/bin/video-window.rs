//! Step 4b: a remote VP8 stream (1920x1080 at 30 fps from `voice-proof --publish video`), opened
//! through the dilla hook and drawn in a GPUI window, measured for 60 s.
//!
//! ```text
//! video-window --url WS --token JWT --epoch E --base-key HEX --roster LEAF:DEVICE,...
//!              [--seconds 60] [--warmup 5] [--fps 30]
//! ```
//!
//! Decoding and the I420 to BGRA conversion run on LiveKit's threads; the main (GPUI) thread
//! uploads each new frame as a `RenderImage`, draws it with `img()` and drops the previous one.
//! Prints `VIDEO <os> <main-thread cpu %> <dropped %>` and one JSON line, then quits.
//!
//! dropped % = (expected - painted) / expected, expected = fps x seconds: a frame the publisher
//! never sent, the network or decoder lost, or the window replaced before painting it counts.

use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use cpu_time::{ProcessTime, ThreadTime};
use futures_util::StreamExt;
use gpui::{
    App, AppContext, Bounds, Context, IntoElement, ObjectFit, ParentElement, Render, RenderImage,
    Styled, StyledImage, TitlebarOptions, Window, WindowBounds, WindowOptions, div, img, px, size,
};
use gpui_voice_proof::media::{Hook, ProofMedia};
use gpui_voice_proof::sframe::{NK, Slot};
use livekit::e2ee::key_provider::{KeyProvider, KeyProviderOptions};
use livekit::e2ee::{E2eeOptions, EncryptionType};
use livekit::prelude::*;
use livekit::webrtc::prelude::VideoFormatType;
use livekit::webrtc::video_stream::native::NativeVideoStream;
use tokio::sync::Notify;

struct Args {
    url: String,
    token: String,
    epoch: u64,
    key: [u8; NK],
    roster: Vec<(u16, [u8; 16])>,
    seconds: u64,
    warmup: u64,
    fps: u64,
}

fn unhex<const N: usize>(s: &str) -> [u8; N] {
    assert_eq!(s.len(), 2 * N, "expected {N} hex bytes");
    let mut out = [0u8; N];
    for (i, b) in out.iter_mut().enumerate() {
        *b = u8::from_str_radix(&s[2 * i..2 * i + 2], 16).expect("hex digit");
    }
    out
}

fn args() -> Args {
    let mut a = Args {
        url: String::new(),
        token: String::new(),
        epoch: 1,
        key: [0; NK],
        roster: Vec::new(),
        seconds: 60,
        warmup: 5,
        fps: 30,
    };
    let mut it = std::env::args().skip(1);
    while let Some(k) = it.next() {
        let v = it.next().unwrap_or_else(|| panic!("{k} needs a value"));
        match k.as_str() {
            "--url" => a.url = v,
            "--token" => a.token = v,
            "--epoch" => a.epoch = v.parse().expect("--epoch"),
            "--base-key" => a.key = unhex::<NK>(&v),
            "--roster" => {
                a.roster = v
                    .split(',')
                    .map(|e| {
                        let (l, d) = e.split_once(':').expect("leaf:device");
                        (l.parse().expect("leaf"), unhex::<16>(d))
                    })
                    .collect();
            }
            "--seconds" => a.seconds = v.parse().expect("--seconds"),
            "--warmup" => a.warmup = v.parse().expect("--warmup"),
            "--fps" => a.fps = v.parse().expect("--fps"),
            _ => panic!("unknown argument {k}"),
        }
    }
    a
}

struct Latest {
    width: u32,
    height: u32,
    bgra: Vec<u8>,
}

/// Shared between LiveKit's decode task and the GPUI main thread.
#[derive(Default)]
struct Shared {
    latest: Mutex<Option<Latest>>,
    received: AtomicU64,
    notify: Notify,
}

struct VideoView {
    shared: Arc<Shared>,
    current: Option<Arc<RenderImage>>,
    painted: u64,
    renders: u64,
}

impl Render for VideoView {
    fn render(&mut self, window: &mut Window, _cx: &mut Context<Self>) -> impl IntoElement {
        self.renders += 1;
        let next = self
            .shared
            .latest
            .lock()
            .unwrap_or_else(|p| p.into_inner())
            .take();
        if let Some(f) = next {
            let buffer =
                image::RgbaImage::from_raw(f.width, f.height, f.bgra).expect("a whole frame");
            let image = Arc::new(RenderImage::new(vec![image::Frame::new(buffer)]));
            if let Some(old) = self.current.replace(image) {
                // Each frame is a new atlas texture; the previous one goes, or the atlas grows.
                window.drop_image(old).ok();
            }
            self.painted += 1;
        }
        let root = div().size_full();
        match &self.current {
            Some(image) => root.child(
                img(image.clone())
                    .size_full()
                    .object_fit(ObjectFit::Contain),
            ),
            None => root,
        }
    }
}

/// GPUI and LiveKit log through `log`; warnings and errors go to stderr (the proof's log file).
struct StderrLog;

impl log::Log for StderrLog {
    fn enabled(&self, m: &log::Metadata<'_>) -> bool {
        m.level() <= log::Level::Warn
    }
    fn log(&self, r: &log::Record<'_>) {
        if self.enabled(r.metadata()) {
            eprintln!("{} {}: {}", r.level(), r.target(), r.args());
        }
    }
    fn flush(&self) {}
}

fn main() {
    log::set_logger(&StderrLog).ok();
    log::set_max_level(log::LevelFilter::Warn);
    let a = args();
    let media = ProofMedia::new(1);
    media.install_epoch(a.epoch, &a.key, &a.roster);
    assert!(webrtc_sys::dilla_transform::install(Box::new(Hook(
        media.clone()
    ))));
    let shared = Arc::new(Shared::default());

    // LiveKit on its own runtime thread.
    {
        let shared = shared.clone();
        let media = media.clone();
        let (url, token) = (a.url.clone(), a.token.clone());
        std::thread::spawn(move || {
            let rt = tokio::runtime::Builder::new_multi_thread()
                .enable_all()
                .build()
                .expect("runtime");
            rt.block_on(async move {
                let mut options = RoomOptions::default();
                #[allow(deprecated)]
                {
                    options.e2ee = Some(E2eeOptions {
                        encryption_type: EncryptionType::Custom,
                        key_provider: KeyProvider::new(KeyProviderOptions::default()),
                    });
                }
                let (_room, mut events) =
                    Room::connect(&url, &token, options).await.expect("connect");
                while let Some(ev) = events.recv().await {
                    let RoomEvent::TrackSubscribed {
                        track: RemoteTrack::Video(t),
                        publication,
                        participant,
                    } = ev
                    else {
                        continue;
                    };
                    let identity = participant.identity().as_str().to_owned();
                    if identity.len() == 32 && publication.source() == TrackSource::Camera {
                        media.bind_remote(
                            &t.rtc_track().id(),
                            unhex::<16>(&identity),
                            Slot::Camera,
                        );
                    }
                    let shared = shared.clone();
                    tokio::spawn(async move {
                        let mut stream = NativeVideoStream::new(t.rtc_track());
                        while let Some(frame) = stream.next().await {
                            let (w, h) = (frame.buffer.width(), frame.buffer.height());
                            let mut bgra = vec![0u8; (w * h * 4) as usize];
                            // libyuv's ARGB is B, G, R, A in memory: GPUI's BGRA.
                            frame.buffer.to_argb(
                                VideoFormatType::ARGB,
                                &mut bgra,
                                w * 4,
                                w as i32,
                                h as i32,
                            );
                            *shared.latest.lock().unwrap_or_else(|p| p.into_inner()) =
                                Some(Latest {
                                    width: w,
                                    height: h,
                                    bgra,
                                });
                            shared.received.fetch_add(1, Ordering::Relaxed);
                            shared.notify.notify_one();
                        }
                    });
                }
            });
        });
    }

    let (seconds, warmup, fps) = (a.seconds, a.warmup, a.fps);
    gpui_platform::application().run(move |cx: &mut App| {
        let bounds = Bounds::centered(None, size(px(1280.), px(720.)), cx);
        let shared_view = shared.clone();
        let window = cx
            .open_window(
                WindowOptions {
                    window_bounds: Some(WindowBounds::Windowed(bounds)),
                    titlebar: Some(TitlebarOptions { title: Some("gpui-voice-proof video".into()), ..Default::default() }),
                    ..Default::default()
                },
                move |_, cx| cx.new(|_| VideoView { shared: shared_view, current: None, painted: 0, renders: 0 }),
            )
            .expect("window");
        let view = window.update(cx, |_, _, cx| cx.entity()).expect("view");

        // Repaint on every decoded frame.
        view.update(cx, |_, cx| {
            let shared = shared.clone();
            cx.spawn(async move |this, cx| {
                loop {
                    shared.notify.notified().await;
                    if this.update(cx, |_, cx| cx.notify()).is_err() {
                        break;
                    }
                }
            })
            .detach();
        });

        // The measurement, on the main thread (so ThreadTime is the main thread's CPU time).
        view.update(cx, |_, cx| {
            let shared = shared.clone();
            let executor = cx.background_executor().clone();
            cx.spawn(async move |this, cx| {
                while shared.received.load(Ordering::Relaxed) == 0 {
                    executor.timer(Duration::from_millis(100)).await;
                }
                executor.timer(Duration::from_secs(warmup)).await;
                let painted0 = this.update(cx, |v, _| v.painted).unwrap_or(0);
                let received0 = shared.received.load(Ordering::Relaxed);
                let (wall, main, process) = (Instant::now(), ThreadTime::now(), ProcessTime::now());
                executor.timer(Duration::from_secs(seconds)).await;
                let main_cpu = main.elapsed();
                let process_cpu = process.elapsed();
                let elapsed = wall.elapsed();
                let painted = this.update(cx, |v, _| v.painted).unwrap_or(0) - painted0;
                let renders = this.update(cx, |v, _| v.renders).unwrap_or(0);
                let received = shared.received.load(Ordering::Relaxed) - received0;
                let expected = (elapsed.as_secs_f64() * fps as f64).round();
                let dropped = ((expected - painted as f64) / expected * 100.0).max(0.0);
                let cpu = main_cpu.as_secs_f64() / elapsed.as_secs_f64() * 100.0;
                let os = std::env::consts::OS;
                println!("VIDEO {os} {cpu:.1} {dropped:.2}");
                println!(
                    "{}",
                    serde_json::json!({
                        "event": "video", "os": os, "seconds": elapsed.as_secs_f64(),
                        "main_thread_cpu_pct": cpu,
                        "process_cpu_pct": process_cpu.as_secs_f64() / elapsed.as_secs_f64() * 100.0,
                        "expected": expected, "received": received, "painted": painted, "renders": renders, "dropped_pct": dropped,
                        "media": media.stats(),
                    })
                );
                cx.update(|cx| cx.quit());
            })
            .detach();
        });
        cx.activate(true);
    });
}
