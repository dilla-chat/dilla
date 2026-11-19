//! The camera through the vendored webrtc-sys's `dilla_camera` bridge: libwebrtc's video capture module (V4L2 on Linux,
//! DirectShow on Windows) and, since the prebuilt libwebrtc has no macOS capture module, AVFoundation on macOS
//! (`dilla_camera_mac.mm`: an `AVCaptureSession` in NV12 converted to I420; the built-in camera listed first). beta.5's
//! camera was `getUserMedia({ video: true })`: the default camera at Chromium's default 640 x 480, 30 fps, which is what
//! [`open`] asks the camera for (it answers with its nearest mode).
//!
//! On macOS a camera the person denied (or a policy restricts) opens nothing at once ([`OpenError::Unavailable`]); one
//! not asked about yet asks the system once (its prompt is the person's) and waits for the answer.

use std::time::Duration;

use webrtc_sys::dilla_camera::{DillaCameraSink, I420View, ffi};

use super::{Latest, OpenError};
use crate::capture_source::{CaptureSource, Frame, PixelFormat, Pull, ShareKind};

/// getUserMedia's default video constraints.
pub const WIDTH: u32 = 640;
pub const HEIGHT: u32 = 480;
pub const FPS: u32 = 30;
/// A camera that sends no frame for this long is gone (unplugged): the track ends, as a browser's track ends.
pub const STALL: Duration = Duration::from_secs(5);

/// One camera the system offers.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct CameraDevice {
    pub name: String,
    /// What opens it (a V4L2 device's bus info, a DirectShow moniker, an AVCaptureDevice unique id); never shown.
    pub id: String,
}

/// The cameras that can be opened now.
pub fn devices() -> Vec<CameraDevice> {
    ffi::dilla_camera_devices()
        .into_iter()
        .map(|d| CameraDevice {
            name: d.name,
            id: d.unique_id,
        })
        .collect()
}

/// A running camera: the camera slot's [`CaptureSource`]. Dropping it stops the camera (the bridge's destructor stops
/// the capture before it returns, so no frame arrives later).
pub struct Camera {
    _device: cxx::UniquePtr<ffi::DillaCamera>,
    latest: Latest,
    size: (u32, u32),
}

/// Packs a camera frame's planes (each with its own stride) into a [`PixelFormat::I420`] frame.
pub fn pack(v: &I420View<'_>) -> Option<Frame> {
    let (w, h) = (v.width as usize, v.height as usize);
    let (cw, ch) = (w.div_ceil(2), h.div_ceil(2));
    let mut data = Vec::with_capacity(w * h + 2 * cw * ch);
    let mut plane = |src: &[u8], stride: u32, width: usize, rows: usize| -> Option<()> {
        let stride = stride as usize;
        for r in 0..rows {
            data.extend_from_slice(src.get(r * stride..r * stride + width)?);
        }
        Some(())
    };
    plane(v.y, v.stride_y, w, h)?;
    plane(v.u, v.stride_u, cw, ch)?;
    plane(v.v, v.stride_v, cw, ch)?;
    Some(Frame {
        width: v.width,
        height: v.height,
        format: PixelFormat::I420,
        stride: v.width,
        data,
    })
}

/// Opens `device` (the first camera when `None`) and waits for its first frame (`wait` at most, or until `cancel`).
pub fn open(
    device: Option<&str>,
    wait: Duration,
    cancel: &dyn Fn() -> bool,
) -> Result<Camera, OpenError> {
    let id = match device {
        Some(id) => id.to_owned(),
        None => devices()
            .into_iter()
            .next()
            .map(|d| d.id)
            .ok_or_else(|| OpenError::Unavailable("no camera".to_owned()))?,
    };
    let latest = Latest::default();
    let sink = latest.clone();
    let device = ffi::dilla_camera_open(
        &id,
        DillaCameraSink::new(move |v| {
            if let Some(f) = pack(&v) {
                sink.put(f);
            }
        }),
    );
    if device.is_null() {
        return Err(OpenError::Unavailable("the camera did not open".to_owned()));
    }
    if device.start(WIDTH, HEIGHT, FPS) != 0 {
        return Err(OpenError::Unavailable(
            "the camera did not start".to_owned(),
        ));
    }
    let mut camera = Camera {
        _device: device,
        latest,
        size: (0, 0),
    };
    camera.size = camera.latest.first(wait, cancel)?;
    Ok(camera)
}

impl CaptureSource for Camera {
    fn kind(&self) -> ShareKind {
        ShareKind::Camera
    }
    fn size(&self) -> (u32, u32) {
        self.size
    }
    fn fps(&self) -> u32 {
        FPS
    }
    fn pull(&mut self) -> Pull {
        let pulled = self.latest.pull();
        if matches!(pulled, Pull::Idle) && self.latest.last().is_some_and(|t| t.elapsed() >= STALL)
        {
            self.latest.end();
            return Pull::Ended;
        }
        pulled
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_camera_frame_is_packed_plane_by_plane_without_its_padding() {
        // 3 x 3 luma with a stride of 4; chroma 2 x 2 with a stride of 3.
        let y = [1, 2, 3, 0, 4, 5, 6, 0, 7, 8, 9, 0];
        let u = [10, 11, 0, 12, 13, 0];
        let v = [20, 21, 0, 22, 23, 0];
        let f = pack(&I420View {
            width: 3,
            height: 3,
            y: &y,
            stride_y: 4,
            u: &u,
            stride_u: 3,
            v: &v,
            stride_v: 3,
        })
        .unwrap();
        assert_eq!(
            f.data,
            [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 20, 21, 22, 23]
        );
        assert!(f.is_whole());
        let short = I420View {
            width: 3,
            height: 3,
            y: &y[..8],
            stride_y: 4,
            u: &u,
            stride_u: 3,
            v: &v,
            stride_v: 3,
        };
        assert!(pack(&short).is_none(), "a short plane is no frame");
    }

    /// Review focus 4: a camera the person denied (or a policy restricts) is the "camera unavailable" error at once,
    /// never a wait for frames that cannot come. The bridge's authorisation gate runs with a forced status
    /// (`dilla_camera_testing`), so this needs neither a camera nor the machine's real TCC state.
    #[cfg(target_os = "macos")]
    #[test]
    fn denied_camera_is_an_error_not_a_hang() {
        use std::time::Instant;
        use webrtc_sys::dilla_camera_testing::{self as testing, Authorization};

        for (status, grants, requests) in [
            (Authorization::Denied, true, 0),
            (Authorization::Restricted, true, 0),
            // Not asked yet: the bridge asks once; refused, it is the same error.
            (Authorization::NotDetermined, false, 1),
        ] {
            let forced = testing::force(status, grants);
            let started = Instant::now();
            let opened = open(Some("any camera"), Duration::from_secs(5), &|| false);
            assert!(
                matches!(opened, Err(OpenError::Unavailable(_))),
                "{status:?}: {:?}",
                opened.err()
            );
            assert!(
                started.elapsed() < Duration::from_millis(500),
                "{status:?} took {:?}",
                started.elapsed()
            );
            assert_eq!(forced.refusals(), 1, "{status:?}: the gate refused it");
            assert_eq!(forced.requests(), requests, "{status:?}");
        }
    }
}
