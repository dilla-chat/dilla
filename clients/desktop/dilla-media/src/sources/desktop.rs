//! Screens and windows through libwebrtc's desktop capturer (see the module docs of [`crate::sources`]).
//!
//! - [`portal`]: Linux on Wayland, where the XDG ScreenCast portal is the only chooser (beta.5 `screenPortal`): there
//!   is no list, [`open`] with no source opens the portal's own dialog, and its Cancel is [`OpenError::Cancelled`].
//! - Elsewhere [`list`] names the screens and windows, [`thumbnail`] grabs one frame of a source for the picker, and
//!   [`open`] captures the chosen one.
//!
//! The capturer is used on one thread only (libwebrtc's desktop capturers are not thread-safe): each [`DesktopCapture`]
//! owns a thread that creates it, pulls a frame per interval and keeps the newest. A permanent capture error (the window
//! closed, the portal session ended, the person cancelled the portal) ends the source.

use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicU32, Ordering};
use std::time::{Duration, Instant};

use livekit::webrtc::desktop_capturer::{
    CaptureError, DesktopCaptureSourceType, DesktopCapturer, DesktopCapturerOptions, DesktopFrame,
};

use super::{Latest, OpenError};
use crate::capture_source::{CaptureSource, Frame, PixelFormat, Pull, ShareKind};

/// A screen or a window.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum SourceKind {
    Screen,
    Window,
}

/// One source the system offers. `id` is the capturer's own (a RandR CRTC or an X11 window, a CGDirectDisplayID or a
/// CGWindowID, an HMONITOR or an HWND); it is never shown and never leaves the process.
#[derive(Clone, Debug, PartialEq, Eq, Hash)]
pub struct DesktopSource {
    pub kind: SourceKind,
    pub id: u64,
    pub title: String,
}

/// The longest wait for a chosen source's first frame (beta.5's broker gave a start 15 s).
pub const START_WAIT: Duration = Duration::from_secs(15);
/// The longest the portal's own dialog may stay open (beta.5 `PICKER_DEADLINE_MS`).
pub const PORTAL_WAIT: Duration = Duration::from_secs(120);

/// Whether the system portal chooses the source (Linux on Wayland), as beta.5's `screenPortal`.
pub fn portal() -> bool {
    portal_in(|k| std::env::var(k).ok())
}

fn portal_in(env: impl Fn(&str) -> Option<String>) -> bool {
    cfg!(target_os = "linux")
        && (env("XDG_SESSION_TYPE").as_deref() == Some("wayland")
            || env("WAYLAND_DISPLAY").is_some_and(|v| !v.is_empty()))
}

fn options(kind: Option<SourceKind>) -> DesktopCapturerOptions {
    let source_type = match kind {
        Some(SourceKind::Screen) => DesktopCaptureSourceType::Screen,
        Some(SourceKind::Window) => DesktopCaptureSourceType::Window,
        // The portal's one dialog offers screens and windows together (Chromium's "generic" capture).
        #[cfg(target_os = "linux")]
        None => DesktopCaptureSourceType::Generic,
        #[cfg(not(target_os = "linux"))]
        None => DesktopCaptureSourceType::Screen,
    };
    let mut o = DesktopCapturerOptions::new(source_type);
    o.set_include_cursor(true);
    // macOS: the sources are listed here and chosen in Dilla's picker, not in the system's.
    #[cfg(target_os = "macos")]
    o.set_sck_system_picker(false);
    o
}

/// The screens or windows that can be shared now (empty under the portal, or when the system offers none).
pub fn list(kind: SourceKind) -> Vec<DesktopSource> {
    if portal() {
        return Vec::new();
    }
    let Some(capturer) = DesktopCapturer::new(options(Some(kind))) else {
        return Vec::new();
    };
    capturer
        .get_source_list()
        .into_iter()
        .map(|s| DesktopSource {
            kind,
            id: s.id(),
            title: s.title(),
        })
        .collect()
}

/// One frame of `source`, scaled to fit `max_w` x `max_h` (RGBA), for the picker's tile; `None` when the source gives
/// no frame within a second (a minimised window).
pub fn thumbnail(source: &DesktopSource, max_w: u32, max_h: u32) -> Option<Frame> {
    let capture = open(Some(source), 10, Duration::from_secs(1), &|| false).ok()?;
    let frame = match capture.latest.pull() {
        Pull::Frame(f) => f,
        _ => return None,
    };
    drop(capture);
    Some(fit_rgba(&frame, max_w, max_h))
}

/// `frame` scaled down (box filter) to fit `max_w` x `max_h`, keeping its aspect, as packed RGBA.
pub fn fit_rgba(frame: &Frame, max_w: u32, max_h: u32) -> Frame {
    let rgba = frame.to_rgba();
    let (w, h) = (frame.width.max(1), frame.height.max(1));
    let scale = (f64::from(max_w) / f64::from(w))
        .min(f64::from(max_h) / f64::from(h))
        .min(1.0);
    let tw = ((f64::from(w) * scale).round() as u32).max(1);
    let th = ((f64::from(h) * scale).round() as u32).max(1);
    let mut out = Vec::with_capacity((tw * th * 4) as usize);
    for ty in 0..th {
        let (y0, y1) = (ty * h / th, ((ty + 1) * h / th).max(ty * h / th + 1));
        for tx in 0..tw {
            let (x0, x1) = (tx * w / tw, ((tx + 1) * w / tw).max(tx * w / tw + 1));
            let mut sum = [0u64; 3];
            let n = u64::from((y1 - y0) * (x1 - x0));
            for y in y0..y1 {
                for x in x0..x1 {
                    let i = ((y * w + x) * 4) as usize;
                    for (c, s) in sum.iter_mut().enumerate() {
                        *s += u64::from(rgba[i + c]);
                    }
                }
            }
            out.extend_from_slice(&[
                (sum[0] / n) as u8,
                (sum[1] / n) as u8,
                (sum[2] / n) as u8,
                255,
            ]);
        }
    }
    Frame::rgba(tw, th, out)
}

/// A running capture of one screen or window: the screen-share slot's [`CaptureSource`]. Video only (beta.5: a native
/// source never carries sound; application sound is its own track). Dropping it stops the capture.
pub struct DesktopCapture {
    latest: Latest,
    stop: Arc<AtomicBool>,
    fps: Arc<AtomicU32>,
    size: (u32, u32),
    surface: Option<SourceKind>,
}

impl DesktopCapture {
    /// What was shared: a screen or a window (`None` when the portal chose, which does not say).
    pub fn surface(&self) -> Option<SourceKind> {
        self.surface
    }
}

impl Drop for DesktopCapture {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Release);
    }
}

impl CaptureSource for DesktopCapture {
    fn kind(&self) -> ShareKind {
        ShareKind::Screen
    }
    fn size(&self) -> (u32, u32) {
        self.size
    }
    fn fps(&self) -> u32 {
        self.fps.load(Ordering::Relaxed)
    }
    fn pull(&mut self) -> Pull {
        self.latest.pull()
    }
    fn set_fps(&mut self, fps: u32) {
        self.fps.store(fps.clamp(1, 60), Ordering::Relaxed);
    }
}

fn copy(frame: &DesktopFrame) -> Option<Frame> {
    let (w, h) = (
        u32::try_from(frame.width()).ok()?,
        u32::try_from(frame.height()).ok()?,
    );
    let stride = frame.stride() as usize;
    let row = w as usize * 4;
    if w == 0 || h == 0 || stride < row {
        return None;
    }
    let src = frame.data();
    let mut data = Vec::with_capacity(row * h as usize);
    for y in 0..h as usize {
        data.extend_from_slice(src.get(y * stride..y * stride + row)?);
    }
    Some(Frame {
        width: w,
        height: h,
        format: PixelFormat::Bgra,
        stride: w * 4,
        data,
    })
}

/// Starts capturing `source` (or, under the portal, whatever the person chooses in its dialog: `None`) at `fps`, and
/// waits for its first frame: `wait` at most, ending early when `cancel` answers true. Nothing is captured after an
/// error: the thread is told to stop.
pub fn open(
    source: Option<&DesktopSource>,
    fps: u32,
    wait: Duration,
    cancel: &dyn Fn() -> bool,
) -> Result<DesktopCapture, OpenError> {
    if source.is_none() && !portal() {
        return Err(OpenError::Unavailable("no source chosen".to_owned()));
    }
    let latest = Latest::default();
    let stop = Arc::new(AtomicBool::new(false));
    let fps = Arc::new(AtomicU32::new(fps.clamp(1, 60)));
    let (kind, id) = (source.map(|s| s.kind), source.map(|s| s.id));
    let thread = {
        let (latest, stop, fps) = (latest.clone(), stop.clone(), fps.clone());
        std::thread::Builder::new()
            .name("dilla-screen".to_owned())
            .spawn(move || run(kind, id, &latest, &stop, &fps))
    };
    if let Err(e) = thread {
        return Err(OpenError::Unavailable(e.to_string()));
    }
    let mut capture = DesktopCapture {
        latest,
        stop,
        fps,
        size: (0, 0),
        surface: kind,
    };
    // Dropping `capture` on an error stops the thread.
    capture.size = capture.latest.first(wait, cancel)?;
    Ok(capture)
}

fn run(
    kind: Option<SourceKind>,
    id: Option<u64>,
    latest: &Latest,
    stop: &AtomicBool,
    fps: &AtomicU32,
) {
    #[cfg(target_os = "linux")]
    let glib = super::glib::ThreadContext::push();
    let Some(mut capturer) = DesktopCapturer::new(options(kind)) else {
        log::warn!("dilla-media: no desktop capturer on this system");
        latest.end();
        return;
    };
    let chosen = match id {
        // Chosen from a list: it must still be there (a window capturer started on nothing crashes in libwebrtc).
        Some(id) => match capturer
            .get_source_list()
            .into_iter()
            .find(|s| s.id() == id)
        {
            Some(s) => Some(s),
            None => {
                latest.end();
                return;
            }
        },
        None => None,
    };
    let sink = latest.clone();
    capturer.start_capture(chosen, move |result| match result {
        Ok(frame) => {
            if let Some(f) = copy(&frame) {
                sink.put(f);
            }
        }
        Err(CaptureError::Permanent) => sink.end(),
        Err(CaptureError::Temporary) => {}
    });
    while !stop.load(Ordering::Acquire) && !latest.ended() {
        let started = Instant::now();
        #[cfg(target_os = "linux")]
        glib.pump();
        capturer.capture_frame();
        let interval =
            Duration::from_micros(1_000_000 / u64::from(fps.load(Ordering::Relaxed).max(1)));
        if let Some(rest) = interval.checked_sub(started.elapsed()) {
            std::thread::sleep(rest);
        }
    }
    // The capturer (and with it the portal session or the X11 connection) goes with this thread.
    drop(capturer);
    latest.end();
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_portal_chooses_on_wayland_only() {
        let env = |pairs: &'static [(&'static str, &'static str)]| {
            move |k: &str| {
                pairs
                    .iter()
                    .find(|(n, _)| *n == k)
                    .map(|(_, v)| (*v).to_owned())
            }
        };
        let linux = cfg!(target_os = "linux");
        assert_eq!(portal_in(env(&[("XDG_SESSION_TYPE", "wayland")])), linux);
        assert_eq!(portal_in(env(&[("WAYLAND_DISPLAY", "wayland-0")])), linux);
        assert!(!portal_in(env(&[
            ("XDG_SESSION_TYPE", "x11"),
            ("DISPLAY", ":0")
        ])));
        assert!(!portal_in(env(&[("WAYLAND_DISPLAY", "")])));
    }

    #[test]
    fn a_thumbnail_keeps_the_aspect_and_averages_its_pixels() {
        // 4 x 2 BGRA: left half blue, right half red.
        let mut data = Vec::new();
        for _ in 0..2 {
            for x in 0..4 {
                data.extend_from_slice(if x < 2 {
                    &[255, 0, 0, 0]
                } else {
                    &[0, 0, 255, 0]
                });
            }
        }
        let f = Frame {
            width: 4,
            height: 2,
            format: PixelFormat::Bgra,
            stride: 16,
            data,
        };
        let t = fit_rgba(&f, 2, 2);
        assert_eq!((t.width, t.height), (2, 1));
        assert_eq!(&t.data, &[0, 0, 255, 255, 255, 0, 0, 255]);
        let same = fit_rgba(&f, 320, 180);
        assert_eq!((same.width, same.height), (4, 2), "never scaled up");
    }

    #[test]
    fn no_source_off_the_portal_opens_nothing() {
        if portal() {
            return;
        }
        assert!(matches!(
            open(None, 30, Duration::from_millis(10), &|| false),
            Err(OpenError::Unavailable(_))
        ));
    }
}
