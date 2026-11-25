//! Screens and windows through libwebrtc's desktop capturer, and on macOS through ScreenCaptureKit directly (see the
//! module docs of [`crate::sources`]).
//!
//! - [`portal`]: Linux on Wayland, where the XDG ScreenCast portal is the only chooser (beta.5 `screenPortal`): there
//!   is no list, [`open`] with no source opens the portal's own dialog, and its Cancel is [`OpenError::Cancelled`].
//!   Not on KDE Plasma when KWin offers its window list and screencast to Dilla ([`kde_picker`], decided by the
//!   registry once per process): there `super::desktop_kde` lists and captures for Dilla's own picker (D22).
//! - Elsewhere [`list`] names the screens and windows, [`snapshot`] lists them with a thumbnail each for the picker
//!   ([`thumbnail`] grabs one frame of a source), and [`open`] captures the chosen one.
//! - macOS (GPUI gaps Task 45, D22): Dilla's own picker, as on X11 and Windows, through webrtc-sys's `dilla_screen`
//!   bridge (`super::desktop_mac`): `SCShareableContent` lists, an own `SCStream` captures. Listing needs Screen
//!   Recording; without it [`snapshot`] says the system refused ([`Snapshot::denied`]).
//!
//! The capturer is used on one thread only (libwebrtc's desktop capturers are not thread-safe): each [`DesktopCapture`]
//! owns a thread that creates it, pulls a frame per interval and keeps the newest (on macOS the thread holds the
//! stream, whose frames arrive on its own queue). A permanent capture error (the window closed, the portal session
//! ended, the person cancelled the portal) ends the source.

use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicU32, Ordering};
use std::time::Duration;
#[cfg(not(target_os = "macos"))]
use std::time::Instant;

#[cfg(not(target_os = "macos"))]
use livekit::webrtc::desktop_capturer::{
    CaptureError, DesktopCaptureSourceType, DesktopCapturer, DesktopCapturerOptions, DesktopFrame,
};

use super::{Latest, OpenError};
#[cfg(not(target_os = "macos"))]
use crate::capture_source::PixelFormat;
use crate::capture_source::{CaptureSource, Frame, Pull, ShareKind};

/// A screen or a window.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum SourceKind {
    Screen,
    Window,
}

/// One source the system offers. `id` is the capturer's own (a RandR CRTC or an X11 window, a CGDirectDisplayID or a
/// CGWindowID, an HMONITOR or an HWND, a `wl_output` global or a hash of KWin's window uuid); it is never shown and never
/// leaves the process. Its `Debug` leaves the title out: titles are shown in the picker only, never logged.
#[derive(Clone, PartialEq, Eq, Hash)]
pub struct DesktopSource {
    pub kind: SourceKind,
    pub id: u64,
    pub title: String,
    /// The window's owning process as the system's window list gave it (macOS, Task 45); `None` for a screen and on
    /// Linux and Windows, where `window_owner_pid` asks the system instead. Never shown, never leaves the process.
    pub owner: Option<u32>,
}

impl std::fmt::Debug for DesktopSource {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("DesktopSource")
            .field("kind", &self.kind)
            .field("id", &self.id)
            .field("title_len", &self.title.chars().count())
            .field("owner", &self.owner.is_some())
            .finish()
    }
}

/// The longest wait for a chosen source's first frame (beta.5's broker gave a start 15 s).
pub const START_WAIT: Duration = Duration::from_secs(15);
/// The longest the portal's own dialog may stay open (beta.5 `PICKER_DEADLINE_MS`).
pub const PORTAL_WAIT: Duration = Duration::from_secs(120);

/// Whether the system chooses the source instead of Dilla's picker: Linux on Wayland (the XDG portal, beta.5's
/// `screenPortal`), unless the compositor offers Dilla KWin's window list and screencast ([`kde_picker`]: KDE Plasma).
/// macOS has Dilla's own picker since Task 45 (D22), as X11 and Windows do. Decided once per process.
pub fn portal() -> bool {
    static PORTAL: std::sync::OnceLock<bool> = std::sync::OnceLock::new();
    *PORTAL.get_or_init(|| portal_in(|k| std::env::var(k).ok(), own_picker_on_wayland))
}

/// Whether Dilla's own picker lists and captures through KWin (KDE Plasma on Wayland, both protocols offered to this
/// process: a registry round trip, once per process). False off Linux, on X11 and wherever the portal chooses.
pub fn kde_picker() -> bool {
    wayland(|k| std::env::var(k).ok()) && own_picker_on_wayland()
}

fn wayland(env: impl Fn(&str) -> Option<String>) -> bool {
    cfg!(target_os = "linux")
        && (env("XDG_SESSION_TYPE").as_deref() == Some("wayland")
            || env("WAYLAND_DISPLAY").is_some_and(|v| !v.is_empty()))
}

/// The portal on Wayland, unless `own_picker` (asked on Wayland only) says the compositor lets Dilla list and capture.
fn portal_in(env: impl Fn(&str) -> Option<String>, own_picker: impl FnOnce() -> bool) -> bool {
    wayland(env) && !own_picker()
}

fn own_picker_on_wayland() -> bool {
    #[cfg(target_os = "linux")]
    {
        super::desktop_kde::available()
    }
    #[cfg(not(target_os = "linux"))]
    {
        false
    }
}

#[cfg(not(target_os = "macos"))]
fn options(kind: Option<SourceKind>) -> DesktopCapturerOptions {
    let source_type = match kind {
        Some(SourceKind::Screen) => DesktopCaptureSourceType::Screen,
        Some(SourceKind::Window) => DesktopCaptureSourceType::Window,
        // The XDG portal's one dialog offers screens and windows together (Chromium's "generic" capture).
        #[cfg(target_os = "linux")]
        None => DesktopCaptureSourceType::Generic,
        #[cfg(not(target_os = "linux"))]
        None => DesktopCaptureSourceType::Screen,
    };
    let mut o = DesktopCapturerOptions::new(source_type);
    o.set_include_cursor(true);
    o
}

/// The screens or windows that can be shared now (empty under the portal, without Screen Recording on macOS, or when
/// the system offers none).
pub fn list(kind: SourceKind) -> Vec<DesktopSource> {
    if portal() {
        return Vec::new();
    }
    #[cfg(target_os = "macos")]
    {
        super::desktop_mac::list(kind)
    }
    #[cfg(not(target_os = "macos"))]
    {
        #[cfg(target_os = "linux")]
        if kde_picker() {
            return super::desktop_kde::list(kind);
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
                owner: None,
            })
            .collect()
    }
}

/// What the picker shows: every screen, then every window, at most `max`, each with a thumbnail fitting
/// `max_w` x `max_h` (`None`: it gave none), and whether the system refused the listing (macOS without Screen
/// Recording: then `sources` is empty).
#[derive(Default)]
pub struct Snapshot {
    pub sources: Vec<(DesktopSource, Option<Frame>)>,
    pub denied: bool,
}

/// The picker's enumeration ([`Snapshot`]). Linux and Windows: [`list`] the screens, then the windows, and take one
/// [`thumbnail`] each; never denied. KDE Plasma on Wayland: `desktop_kde::snapshot` (one KWin stream at a time, each
/// closed after its one frame). macOS: `desktop_mac::snapshot` (`SCScreenshotManager` thumbnails on 14+, one captured
/// frame on 13). Empty under the portal. Blocking: call it off the UI thread.
pub fn snapshot(max: usize, max_w: u32, max_h: u32) -> Snapshot {
    if portal() {
        return Snapshot::default();
    }
    #[cfg(target_os = "macos")]
    {
        super::desktop_mac::snapshot(max, max_w, max_h)
    }
    #[cfg(not(target_os = "macos"))]
    {
        #[cfg(target_os = "linux")]
        if kde_picker() {
            return super::desktop_kde::snapshot(max, max_w, max_h);
        }
        Snapshot {
            sources: [SourceKind::Screen, SourceKind::Window]
                .into_iter()
                .flat_map(list)
                .take(max)
                .map(|source| {
                    let thumbnail = thumbnail(&source, max_w, max_h);
                    (source, thumbnail)
                })
                .collect(),
            denied: false,
        }
    }
}

/// Asks for what listing needs, from the picker's open (a press on the focused window came first). macOS: Screen
/// Recording, whose system prompt shows only the first time in a process (`dilla_screen_access(true)`); the answer is
/// whether it is granted now (a grant needs a relaunch to count). Elsewhere true. Never blocks long; never on the UI
/// thread all the same.
pub fn request_access() -> bool {
    #[cfg(target_os = "macos")]
    {
        super::desktop_mac::request_access()
    }
    #[cfg(not(target_os = "macos"))]
    {
        true
    }
}

/// The process that owns a shared window, for its sound (Task 44). `None` for a screen, under the portal (Linux on
/// Wayland: the system's dialog chooses and Dilla never holds a window id), and when the system does not say.
///
/// KDE Plasma on Wayland: the window's process as KWin's window list gives it now (KWin knows a Wayland client's
/// process from its socket), the window still offered.
///
/// macOS (Task 45): the owner the window list carried (`SCWindow.owningApplication`, the WindowServer's record), with
/// no system call; the share is checked against that same list.
///
/// Blocking (an X11 round trip): call it off the UI thread.
pub fn window_owner_pid(source: &DesktopSource) -> Option<u32> {
    if source.kind != SourceKind::Window || portal() {
        return None;
    }
    #[cfg(target_os = "linux")]
    {
        if kde_picker() {
            return super::desktop_kde::owner(source);
        }
        x11_client_pid(u32::try_from(source.id).ok()?)
    }
    #[cfg(target_os = "windows")]
    {
        hwnd_pid(source.id)
    }
    #[cfg(target_os = "macos")]
    {
        source.owner.filter(|&pid| pid > 1)
    }
    #[cfg(not(any(target_os = "linux", target_os = "windows", target_os = "macos")))]
    {
        None
    }
}

/// The process behind an X11 client window, from the X server's own record of the client's socket peer (XRes 1.2
/// `LOCAL_CLIENT_PID`), so a window cannot claim another process's pid. `_NET_WM_PID` is client-written and is
/// deliberately not read. No XRes 1.2, a remote client or any error is `None`.
#[cfg(target_os = "linux")]
fn x11_client_pid(window: u32) -> Option<u32> {
    use x11rb::protocol::res::{ClientIdMask, ClientIdSpec, ConnectionExt};
    let (conn, _) = x11rb::rust_connection::RustConnection::connect(None).ok()?;
    let reply = conn
        .res_query_client_ids(&[ClientIdSpec {
            client: window,
            mask: ClientIdMask::LOCAL_CLIENT_PID,
        }])
        .ok()?
        .reply()
        .ok()?;
    let pid = *reply.ids.first()?.value.first()?;
    (pid != 0).then_some(pid)
}

/// The process of a window handle; `None` when the window is gone or the system gives no process. Store (UWP) windows
/// belong to `ApplicationFrameHost.exe`, which never plays, so they fall back to no application (documented, not
/// worked around).
#[cfg(target_os = "windows")]
#[allow(unsafe_code)]
fn hwnd_pid(hwnd: u64) -> Option<u32> {
    use windows::Win32::Foundation::HWND;
    use windows::Win32::UI::WindowsAndMessaging::{GetWindowThreadProcessId, IsWindow};
    let hwnd = HWND(hwnd as usize as *mut _);
    // SAFETY: both calls take a handle value only; a stale or invalid one answers false or 0.
    unsafe {
        if !IsWindow(Some(hwnd)).as_bool() {
            return None;
        }
        let mut pid = 0u32;
        let thread = GetWindowThreadProcessId(hwnd, Some(&mut pid));
        (thread != 0 && pid != 0).then_some(pid)
    }
}

/// One frame of `source`, scaled to fit `max_w` x `max_h` (RGBA), for the picker's tile; `None` when the source gives
/// no frame within a second (a minimised window).
pub fn thumbnail(source: &DesktopSource, max_w: u32, max_h: u32) -> Option<Frame> {
    #[cfg(target_os = "linux")]
    if kde_picker() {
        return super::desktop_kde::thumbnail(source, max_w, max_h);
    }
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

#[cfg(not(target_os = "macos"))]
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
    let kind = source.map(|s| s.kind);
    let thread = {
        let (latest, stop, fps) = (latest.clone(), stop.clone(), fps.clone());
        let source = source.cloned();
        std::thread::Builder::new()
            .name("dilla-screen".to_owned())
            .spawn(move || run(source, &latest, &stop, &fps))
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

/// The capture thread on macOS: an own `SCStream` (`super::desktop_mac`). There is no portal there, so a source was
/// always chosen.
#[cfg(target_os = "macos")]
fn run(source: Option<DesktopSource>, latest: &Latest, stop: &AtomicBool, fps: &AtomicU32) {
    match source {
        Some(source) => super::desktop_mac::run(&source, latest, stop, fps),
        None => latest.end(),
    }
}

#[cfg(not(target_os = "macos"))]
fn run(source: Option<DesktopSource>, latest: &Latest, stop: &AtomicBool, fps: &AtomicU32) {
    // KDE Plasma on Wayland: KWin's own stream of the picked source (there is no portal there, so one was picked).
    #[cfg(target_os = "linux")]
    if let Some(source) = &source
        && kde_picker()
    {
        super::desktop_kde::run(source, latest, stop, fps);
        return;
    }
    let (kind, id) = (
        source.as_ref().map(|s| s.kind),
        source.as_ref().map(|s| s.id),
    );
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
    use crate::capture_source::PixelFormat;

    #[test]
    fn the_system_chooses_on_wayland_only() {
        let env = |pairs: &'static [(&'static str, &'static str)]| {
            move |k: &str| {
                pairs
                    .iter()
                    .find(|(n, _)| *n == k)
                    .map(|(_, v)| (*v).to_owned())
            }
        };
        // macOS has Dilla's own picker (D22): the system never chooses there, whatever the environment says.
        let linux = cfg!(target_os = "linux");
        let no_kwin = || false;
        assert_eq!(
            portal_in(env(&[("XDG_SESSION_TYPE", "wayland")]), no_kwin),
            linux
        );
        assert_eq!(
            portal_in(env(&[("WAYLAND_DISPLAY", "wayland-0")]), no_kwin),
            linux
        );
        let never = || panic!("the compositor is asked on Wayland only");
        assert!(!portal_in(
            env(&[("XDG_SESSION_TYPE", "x11"), ("DISPLAY", ":0")]),
            never
        ));
        assert!(!portal_in(env(&[("WAYLAND_DISPLAY", "")]), never));
    }

    /// KDE Plasma (KWin offers its window list and screencast): Dilla's own picker, never the portal. A compositor
    /// that does not (GNOME, a sandboxed Dilla, an older KWin without the trusted `.desktop` file) keeps the portal.
    #[test]
    fn kwin_s_protocols_replace_the_portal_on_wayland() {
        let wayland = |k: &str| (k == "XDG_SESSION_TYPE").then(|| "wayland".to_owned());
        assert!(!portal_in(wayland, || true));
        assert_eq!(portal_in(wayland, || false), cfg!(target_os = "linux"));
    }

    /// A source's `Debug` never carries its title (a window's title describes the person's desktop).
    #[test]
    fn a_source_s_debug_has_no_title() {
        let s = DesktopSource {
            kind: SourceKind::Window,
            id: 3,
            title: "Secret plans.txt".to_owned(),
            owner: Some(4242),
        };
        let shown = format!("{s:?}");
        assert!(!shown.contains("Secret"), "{shown}");
        assert!(!shown.contains("4242"), "{shown}");
        assert!(shown.contains("Window"));
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
    fn a_screen_has_no_window_owner() {
        let screen = DesktopSource {
            kind: SourceKind::Screen,
            id: 1,
            title: "Screen 1".to_owned(),
            // Even with an owner (none is listed for a screen), a screen brings no window's sound.
            owner: Some(4242),
        };
        assert_eq!(window_owner_pid(&screen), None);
    }

    #[cfg(target_os = "macos")]
    #[test]
    fn a_window_s_listed_owner_is_its_owner_on_macos() {
        let window = DesktopSource {
            kind: SourceKind::Window,
            id: 7,
            title: "Notes".to_owned(),
            owner: Some(4242),
        };
        assert_eq!(window_owner_pid(&window), Some(4242));
        let unowned = DesktopSource {
            owner: None,
            ..window
        };
        assert_eq!(window_owner_pid(&unowned), None);
    }

    #[cfg(target_os = "linux")]
    #[test]
    #[ignore = "needs an X11 display: DISPLAY set (Xorg or Xwayland with X-Resource)"]
    fn an_x11_window_s_owner_is_the_process_that_made_it() {
        use x11rb::connection::Connection;
        use x11rb::protocol::xproto::{ConnectionExt, CreateWindowAux, WindowClass};
        let (conn, screen) = x11rb::connect(None).expect("an X11 display");
        let root = conn.setup().roots[screen].root;
        let win = conn.generate_id().expect("an id");
        conn.create_window(
            0,
            win,
            root,
            0,
            0,
            8,
            8,
            0,
            WindowClass::INPUT_OUTPUT,
            0,
            &CreateWindowAux::new(),
        )
        .expect("create")
        .check()
        .expect("created");
        assert_eq!(x11_client_pid(win), Some(std::process::id()));
    }

    #[cfg(target_os = "windows")]
    #[test]
    fn a_window_that_is_gone_has_no_owner() {
        assert_eq!(hwnd_pid(1), None);
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
