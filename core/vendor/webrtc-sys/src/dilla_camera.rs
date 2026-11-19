// DILLA PATCH: the camera source of the desktop client (core/vendor/README.md): libwebrtc's own
// VideoCaptureModule (V4L2 on Linux, DirectShow on Windows) and AVFoundation on macOS
// (src/dilla_camera_mac.mm), its frames handed to Rust as I420.

use crate::impl_thread_safety;

#[cxx::bridge(namespace = "livekit_ffi")]
pub mod ffi {
    /// One camera: its name and the id that opens it.
    #[derive(Clone, Debug)]
    struct DillaCameraDevice {
        name: String,
        unique_id: String,
    }

    unsafe extern "C++" {
        include!("livekit/dilla_camera.h");

        type DillaCamera;

        fn dilla_camera_devices() -> Vec<DillaCameraDevice>;
        /// Null when the camera cannot be opened.
        fn dilla_camera_open(unique_id: &str, sink: Box<DillaCameraSink>) -> UniquePtr<DillaCamera>;
        /// Starts the capture at the camera's mode nearest to the asked one; 0 on success.
        fn start(self: &DillaCamera, width: u32, height: u32, fps: u32) -> i32;
        fn width(self: &DillaCamera) -> u32;
        fn height(self: &DillaCamera) -> u32;
    }

    extern "Rust" {
        type DillaCameraSink;

        fn on_frame(
            self: &mut DillaCameraSink,
            width: u32,
            height: u32,
            y: &[u8],
            stride_y: u32,
            u: &[u8],
            stride_u: u32,
            v: &[u8],
            stride_v: u32,
        );
    }
}

impl_thread_safety!(ffi::DillaCamera, Send + Sync);

/// One I420 frame as the camera delivered it (borrowed for the call only).
pub struct I420View<'a> {
    pub width: u32,
    pub height: u32,
    pub y: &'a [u8],
    pub stride_y: u32,
    pub u: &'a [u8],
    pub stride_u: u32,
    pub v: &'a [u8],
    pub stride_v: u32,
}

/// Where an open camera's frames go (called on the camera's own thread).
pub struct DillaCameraSink(Box<dyn FnMut(I420View<'_>) + Send>);

impl DillaCameraSink {
    pub fn new(f: impl FnMut(I420View<'_>) + Send + 'static) -> Box<Self> {
        Box::new(Self(Box::new(f)))
    }

    #[allow(clippy::too_many_arguments)]
    fn on_frame(
        &mut self,
        width: u32,
        height: u32,
        y: &[u8],
        stride_y: u32,
        u: &[u8],
        stride_u: u32,
        v: &[u8],
        stride_v: u32,
    ) {
        (self.0)(I420View { width, height, y, stride_y, u, stride_u, v, stride_v });
    }
}
