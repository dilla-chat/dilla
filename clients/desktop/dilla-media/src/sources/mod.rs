//! The real share sources (parity plan Package 1), behind [`crate::CaptureSource`]:
//!
//! - [`desktop`]: screens and windows through libwebrtc's own desktop capturer, the engine Electron's `desktopCapturer`
//!   ran on (beta.5): X11 (screens through RandR, windows through XComposite), the XDG ScreenCast portal and PipeWire on
//!   Wayland (the portal draws its own chooser), ScreenCaptureKit on macOS and Windows Graphics Capture on Windows;
//! - [`camera`]: the camera through libwebrtc's video capture module (V4L2 on Linux, DirectShow on Windows; the vendored
//!   webrtc-sys's `dilla_camera` bridge). macOS has no camera source yet.
//!
//! Each source runs on its own thread and keeps only its newest frame ([`Latest`]); the share's feeder pulls it at the
//! share's frame rate. Dropping a source stops its capture (the thread ends within one frame interval), so a share that
//! ends, a call that ends and a window that closes all release the screen or the camera.

use std::sync::{Arc, Condvar, Mutex};
use std::time::{Duration, Instant};

use crate::capture_source::{Frame, Pull};

#[cfg(any(target_os = "windows", target_os = "linux"))]
pub mod camera;
pub mod desktop;
#[cfg(target_os = "linux")]
mod glib;

/// Why a source did not open.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum OpenError {
    /// The person cancelled the choice (the portal's Cancel), or it was withdrawn (the call or the window went away).
    #[error("the selection was cancelled")]
    Cancelled,
    /// The source is gone or the system refused it (no permission, no such window, no camera).
    #[error("the source is unavailable: {0}")]
    Unavailable(String),
    /// No first frame within the wait.
    #[error("the source sent no frame in time")]
    Timeout,
}

#[derive(Default)]
struct Slot {
    frame: Option<Frame>,
    started: bool,
    ended: bool,
    last: Option<Instant>,
}

/// The newest frame of a running source, shared between its capture thread and the share's feeder.
#[derive(Clone, Default)]
pub(crate) struct Latest(Arc<(Mutex<Slot>, Condvar)>);

impl Latest {
    fn lock(&self) -> std::sync::MutexGuard<'_, Slot> {
        self.0.0.lock().unwrap_or_else(|p| p.into_inner())
    }

    /// A new frame replaces any frame not pulled yet.
    pub(crate) fn put(&self, frame: Frame) {
        let mut s = self.lock();
        if s.ended {
            return;
        }
        s.frame = Some(frame);
        s.started = true;
        s.last = Some(Instant::now());
        self.0.1.notify_all();
    }

    /// The source ended for good.
    pub(crate) fn end(&self) {
        self.lock().ended = true;
        self.0.1.notify_all();
    }

    pub(crate) fn ended(&self) -> bool {
        self.lock().ended
    }

    /// When the newest frame arrived (the camera's stall check; macOS has no camera source yet).
    #[cfg(any(target_os = "windows", target_os = "linux"))]
    pub(crate) fn last(&self) -> Option<Instant> {
        self.lock().last
    }

    pub(crate) fn pull(&self) -> Pull {
        let mut s = self.lock();
        if s.ended {
            return Pull::Ended;
        }
        s.frame.take().map_or(Pull::Idle, Pull::Frame)
    }

    /// Waits for the first frame (its size), the end, `cancel` or `wait`, checking `cancel` every 50 ms.
    pub(crate) fn first(
        &self,
        wait: Duration,
        cancel: &dyn Fn() -> bool,
    ) -> Result<(u32, u32), OpenError> {
        let deadline = Instant::now() + wait;
        let mut s = self.lock();
        loop {
            if let Some(f) = &s.frame {
                return Ok((f.width, f.height));
            }
            if s.ended {
                return Err(OpenError::Cancelled);
            }
            if cancel() {
                return Err(OpenError::Cancelled);
            }
            let left = deadline.saturating_duration_since(Instant::now());
            if left.is_zero() {
                return Err(OpenError::Timeout);
            }
            s = self
                .0
                .1
                .wait_timeout(s, left.min(Duration::from_millis(50)))
                .unwrap_or_else(|p| p.into_inner())
                .0;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::capture_source::PixelFormat;

    fn frame(w: u32) -> Frame {
        Frame::rgba(w, 2, vec![0; (w * 2 * 4) as usize])
    }

    #[test]
    fn the_latest_frame_replaces_an_unpulled_one_and_the_end_is_final() {
        let l = Latest::default();
        assert!(matches!(l.pull(), Pull::Idle));
        l.put(frame(2));
        l.put(frame(4));
        match l.pull() {
            Pull::Frame(f) => assert_eq!((f.width, f.format), (4, PixelFormat::Rgba)),
            other => panic!("{other:?}"),
        }
        assert!(
            matches!(l.pull(), Pull::Idle),
            "a frame is handed over once"
        );
        l.end();
        l.put(frame(2));
        assert!(matches!(l.pull(), Pull::Ended), "nothing after the end");
    }

    #[test]
    fn waiting_for_the_first_frame_ends_on_a_frame_an_end_a_cancel_or_the_deadline() {
        let l = Latest::default();
        assert_eq!(
            l.first(Duration::from_millis(60), &|| false),
            Err(OpenError::Timeout)
        );
        assert_eq!(
            l.first(Duration::from_secs(5), &|| true),
            Err(OpenError::Cancelled)
        );
        let feeder = l.clone();
        let t = std::thread::spawn(move || {
            std::thread::sleep(Duration::from_millis(30));
            feeder.put(frame(8));
        });
        assert_eq!(l.first(Duration::from_secs(5), &|| false), Ok((8, 2)));
        t.join().unwrap();
        let ended = Latest::default();
        ended.end();
        assert_eq!(
            ended.first(Duration::from_secs(5), &|| false),
            Err(OpenError::Cancelled),
            "a portal cancelled before its first frame is a cancel"
        );
    }
}
