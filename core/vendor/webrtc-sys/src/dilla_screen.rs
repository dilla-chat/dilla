// DILLA PATCH: Dilla's own macOS share picker (core/vendor/README.md; GPUI gaps Task 45, D22):
// ScreenCaptureKit directly (src/dilla_screen_mac.mm). `SCShareableContent` lists the displays and
// on-screen windows with each window's owning process, `SCScreenshotManager` takes the thumbnails
// on macOS 14+, and an own `SCStream` with an `SCContentFilter` captures the chosen one, its BGRA
// frames handed to a Rust sink. libwebrtc's ScreenCaptureKit capturer lists nothing without the
// system picker, and its window capturer cannot take a filter, so none of it is reused here.

#[cxx::bridge(namespace = "livekit_ffi")]
pub mod ffi {
    /// One display or on-screen window as ScreenCaptureKit lists it, unfiltered (dilla-media filters).
    #[derive(Clone, Debug)]
    struct DillaScreenRecord {
        is_window: bool,
        /// `SCDisplay.displayID` (a CGDirectDisplayID) or `SCWindow.windowID` (a CGWindowID).
        id: u64,
        /// `SCWindow.owningApplication.processID`; 0 for a display or a window without an application.
        owner_pid: i32,
        /// `owningApplication.bundleIdentifier`, "" when none.
        bundle_id: String,
        /// `owningApplication.applicationName`.
        app_name: String,
        /// The window's title, or the display's `NSScreen.localizedName` (matched by `CGDirectDisplayID`), "" when none.
        title: String,
        /// `SCWindow.windowLayer`; 0 for a display.
        layer: i64,
        /// The frame, in points.
        width: f64,
        height: f64,
        /// The display is `CGMainDisplayID()`.
        main: bool,
    }

    #[derive(Debug, PartialEq)]
    enum DillaScreenAccess {
        Granted,
        Denied,
        Failed,
    }

    struct DillaScreenList {
        access: DillaScreenAccess,
        records: Vec<DillaScreenRecord>,
    }

    /// One thumbnail, BGRA, tightly packed (`stride == width * 4`).
    struct DillaScreenThumbnail {
        is_window: bool,
        id: u64,
        width: u32,
        height: u32,
        bgra: Vec<u8>,
    }

    unsafe extern "C++" {
        include!("livekit/dilla_screen.h");

        type DillaScreenCapture;

        /// `CGPreflightScreenCaptureAccess()`; with `request`, the first false in this process also calls
        /// `CGRequestScreenCaptureAccess()` (the system prompt; it answers false at once).
        fn dilla_screen_access(request: bool) -> bool;
        /// Denied without touching ScreenCaptureKit when `dilla_screen_access(false)` is false. Otherwise
        /// `getShareableContentExcludingDesktopWindows:YES onScreenWindowsOnly:YES`, waited ≤ 5 s (Failed past that
        /// or on an error). Blocking: never on the main thread.
        fn dilla_screen_list() -> DillaScreenList;
        /// macOS 14+: one `SCScreenshotManager` capture per listed id (re-resolved from a fresh shareable content),
        /// all at once, waited ≤ 3 s together, each scaled to fit `max_w` x `max_h` with no cursor. An id that is
        /// gone, a late answer or macOS 13: absent from the result. Blocking.
        fn dilla_screen_thumbnails(
            displays: &[u64],
            windows: &[u64],
            max_w: u32,
            max_h: u32,
        ) -> Vec<DillaScreenThumbnail>;
        /// Re-resolves `id` from a fresh shareable content. For a window, `expect_pid` must still be its owner (a
        /// reused CGWindowID is refused). Then it starts an `SCStream`: 32BGRA, the source's pixel size (frame times
        /// the backing scale of its screen), `minimumFrameInterval` 1/`fps`, `showsCursor` = `cursor`, `queueDepth`
        /// 3, no audio, output on a serial queue of its own. Null when the source is gone, access is missing, or the
        /// start fails within 5 s.
        fn dilla_screen_open(
            is_window: bool,
            id: u64,
            expect_pid: i32,
            fps: u32,
            cursor: bool,
            sink: Box<DillaScreenSink>,
        ) -> UniquePtr<DillaScreenCapture>;
        /// `updateConfiguration:` with the new frame interval.
        fn set_fps(self: &DillaScreenCapture, fps: u32);
        fn width(self: &DillaScreenCapture) -> u32;
        fn height(self: &DillaScreenCapture) -> u32;
    }

    extern "Rust" {
        type DillaScreenSink;

        /// A complete frame (`SCStreamFrameInfoStatus` Complete only), the pixel buffer locked read-only for the
        /// call.
        fn on_frame(self: &mut DillaScreenSink, width: u32, height: u32, stride: u32, bgra: &[u8]);
        /// `stream:didStopWithError:` or a frame with `SCFrameStatusStopped` (the window closed, the display went
        /// away, access withdrawn). Called once.
        fn on_end(self: &mut DillaScreenSink);

        /// `bytes` as a Rust vector, in one copy (a thumbnail's pixels; rust::Vec has no bulk append).
        fn dilla_screen_bytes(bytes: &[u8]) -> Vec<u8>;
    }
}

// The destructor: `stopCaptureWithCompletionHandler:` waited ≤ 2 s, `removeStreamOutput:`, then a
// `dispatch_sync` on the output queue, so no `on_frame` runs after it returns (the camera's teardown
// order).
//
// SAFETY: Send only. The capture is created, reconfigured (`set_fps`) and destroyed on one thread at
// a time (dilla-media's `dilla-screen` thread owns it); ScreenCaptureKit's queues reach it only
// through its own locks (`Deliver`, `End`). It is not Sync: `set_fps(&self)` changes the stream's
// configuration without a lock.
unsafe impl Send for ffi::DillaScreenCapture {}

/// One BGRA frame as the stream delivered it (borrowed for the call only): `height` rows of `stride` bytes, each
/// starting with `width` pixels.
pub struct BgraView<'a> {
    pub width: u32,
    pub height: u32,
    pub stride: u32,
    pub bgra: &'a [u8],
}

type OnFrame = Box<dyn FnMut(BgraView<'_>) + Send>;
type OnEnd = Box<dyn FnMut() + Send>;

/// Where an open stream's frames go, and its end (called on ScreenCaptureKit's queues, one call at a time).
pub struct DillaScreenSink {
    frame: OnFrame,
    end: OnEnd,
}

impl DillaScreenSink {
    pub fn new(
        frame: impl FnMut(BgraView<'_>) + Send + 'static,
        end: impl FnMut() + Send + 'static,
    ) -> Box<Self> {
        Box::new(Self { frame: Box::new(frame), end: Box::new(end) })
    }

    fn on_frame(&mut self, width: u32, height: u32, stride: u32, bgra: &[u8]) {
        (self.frame)(BgraView { width, height, stride, bgra });
    }

    fn on_end(&mut self) {
        (self.end)();
    }
}

fn dilla_screen_bytes(bytes: &[u8]) -> Vec<u8> {
    bytes.to_vec()
}
