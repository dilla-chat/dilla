//! Where a call's camera and screen-share video come from: a [`CaptureSource`] hands frames at its own pace, and
//! [`crate::Call::publish_video`] encodes them under the call's `dilla-sframe/1` keys (the camera slot or the screen
//! slot), exactly as the microphone is.
//!
//! The real sources are [`crate::sources`]: the screens and windows of libwebrtc's desktop capturer (the XDG portal on
//! Wayland) and the camera of its video capture module. [`TestPattern`] is the moving test pattern the tests (and the
//! automation build) share. A cancelled selection is a source that never opens ([`crate::Call::publish_video`] is
//! simply not called), so nothing is published.

use std::time::Duration;

/// Which slot a source fills.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub enum ShareKind {
    Camera,
    Screen,
}

/// How a [`Frame`]'s bytes are laid out.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum PixelFormat {
    /// R, G, B, A bytes per pixel, `stride` bytes per row.
    Rgba,
    /// B, G, R, A (or X) bytes per pixel, `stride` bytes per row: what the desktop capturer hands over.
    Bgra,
    /// Planar I420, packed: the Y plane (`stride` bytes per row), then U and V (`(width + 1) / 2` bytes per row, `(height +
    /// 1) / 2` rows each).
    I420,
}

/// One captured frame.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Frame {
    pub width: u32,
    pub height: u32,
    pub format: PixelFormat,
    /// Bytes per row of the first plane.
    pub stride: u32,
    pub data: Vec<u8>,
}

impl Frame {
    /// A tightly packed RGBA frame.
    pub fn rgba(width: u32, height: u32, data: Vec<u8>) -> Frame {
        Frame {
            width,
            height,
            format: PixelFormat::Rgba,
            stride: width * 4,
            data,
        }
    }

    /// Whether the bytes cover what the size, stride and format say (a frame that does not is never encoded).
    pub fn is_whole(&self) -> bool {
        let (w, h) = (self.width as usize, self.height as usize);
        let stride = self.stride as usize;
        if w == 0 || h == 0 {
            return false;
        }
        match self.format {
            PixelFormat::Rgba | PixelFormat::Bgra => {
                stride >= w * 4 && self.data.len() >= stride * (h - 1) + w * 4
            }
            PixelFormat::I420 => {
                let (cw, ch) = (w.div_ceil(2), h.div_ceil(2));
                stride >= w && self.data.len() >= stride * h + 2 * cw * ch
            }
        }
    }

    /// The frame as tightly packed RGBA (a thumbnail, a test's check).
    pub fn to_rgba(&self) -> Vec<u8> {
        let (w, h, stride) = (
            self.width as usize,
            self.height as usize,
            self.stride as usize,
        );
        let mut out = Vec::with_capacity(w * h * 4);
        match self.format {
            PixelFormat::Rgba | PixelFormat::Bgra => {
                for y in 0..h {
                    for px in self.data[y * stride..y * stride + w * 4].as_chunks::<4>().0 {
                        if self.format == PixelFormat::Rgba {
                            out.extend_from_slice(&[px[0], px[1], px[2], 255]);
                        } else {
                            out.extend_from_slice(&[px[2], px[1], px[0], 255]);
                        }
                    }
                }
            }
            PixelFormat::I420 => {
                let cw = w.div_ceil(2);
                let (u0, v0) = (stride * h, stride * h + cw * h.div_ceil(2));
                for y in 0..h {
                    for x in 0..w {
                        let luma = f32::from(self.data[y * stride + x]) - 16.0;
                        let u = f32::from(self.data[u0 + (y / 2) * cw + x / 2]) - 128.0;
                        let v = f32::from(self.data[v0 + (y / 2) * cw + x / 2]) - 128.0;
                        let c = |f: f32| f.round().clamp(0.0, 255.0) as u8;
                        out.extend_from_slice(&[
                            c(1.164 * luma + 1.596 * v),
                            c(1.164 * luma - 0.392 * u - 0.813 * v),
                            c(1.164 * luma + 2.017 * u),
                            255,
                        ]);
                    }
                }
            }
        }
        out
    }
}

/// What a source has now.
#[derive(Debug)]
pub enum Pull {
    /// A frame not handed over before.
    Frame(Frame),
    /// Nothing new since the last pull (the screen did not change, the camera is between frames).
    Idle,
    /// The source ended (the person stopped it from the system, the window or the camera went away): the track ends.
    Ended,
}

/// A source of video frames for one share.
pub trait CaptureSource: Send + 'static {
    fn kind(&self) -> ShareKind;
    /// The size of the source's frames when it opened (a window can change it later; each frame carries its own).
    fn size(&self) -> (u32, u32);
    /// The frames per second the source delivers at most.
    fn fps(&self) -> u32;
    /// Whether the share carries its sound (a screen share whose picker offered sound and the person kept it).
    fn has_audio(&self) -> bool {
        false
    }
    /// The newest frame, if any.
    fn pull(&mut self) -> Pull;
    /// The person changed the stream quality of the running share (beta.5 `configure-screen-capture`): the source
    /// captures at most `fps` from now on. The frame bound is the encoder side's.
    fn set_fps(&mut self, fps: u32) {
        let _ = fps;
    }

    /// The interval between two frames.
    fn interval(&self) -> Duration {
        Duration::from_micros(1_000_000 / u64::from(self.fps().max(1)))
    }
}

/// A moving test pattern: eight vertical colour bars and a white square that walks one step per frame. Deterministic,
/// so a receiver can tell it got the frames (the bars' colours) and that they move (the square).
pub struct TestPattern {
    kind: ShareKind,
    width: u32,
    height: u32,
    fps: u32,
    frame: u64,
    /// Ends after this many frames (`None`: never).
    limit: Option<u64>,
}

/// The bars, left to right (SMPTE-like, opaque).
pub const BARS: [[u8; 3]; 8] = [
    [235, 235, 235],
    [235, 235, 16],
    [16, 235, 235],
    [16, 235, 16],
    [235, 16, 235],
    [235, 16, 16],
    [16, 16, 235],
    [16, 16, 16],
];

impl TestPattern {
    pub fn new(kind: ShareKind, width: u32, height: u32, fps: u32) -> Self {
        Self {
            kind,
            width: width.max(16) & !1,
            height: height.max(16) & !1,
            fps: fps.max(1),
            frame: 0,
            limit: None,
        }
    }

    /// A pattern that ends after `frames` frames (the person stopping a share from the system).
    pub fn ending_after(mut self, frames: u64) -> Self {
        self.limit = Some(frames);
        self
    }

    /// The next frame of the pattern, `None` once it ended.
    pub fn next_frame(&mut self) -> Option<Frame> {
        if self.limit.is_some_and(|l| self.frame >= l) {
            return None;
        }
        let n = self.frame;
        self.frame += 1;
        let mut rgba = Vec::with_capacity((self.width * self.height * 4) as usize);
        for y in 0..self.height {
            for x in 0..self.width {
                let [r, g, b] = self.pixel(n, x, y);
                rgba.extend_from_slice(&[r, g, b, 255]);
            }
        }
        Some(Frame::rgba(self.width, self.height, rgba))
    }

    /// The colour of the pattern at `(x, y)` of frame `n`.
    pub fn pixel(&self, n: u64, x: u32, y: u32) -> [u8; 3] {
        let side = (self.height / 8).max(2);
        let travel = u64::from(self.width.saturating_sub(side).max(1));
        let sx = (n * 4 % travel) as u32;
        let sy = (self.height - side) / 2;
        if (sx..sx + side).contains(&x) && (sy..sy + side).contains(&y) {
            return [255, 255, 255];
        }
        BARS[(x * 8 / self.width) as usize % 8]
    }
}

impl CaptureSource for TestPattern {
    fn kind(&self) -> ShareKind {
        self.kind
    }
    fn size(&self) -> (u32, u32) {
        (self.width, self.height)
    }
    fn fps(&self) -> u32 {
        self.fps
    }
    fn pull(&mut self) -> Pull {
        self.next_frame().map_or(Pull::Ended, Pull::Frame)
    }
}

/// The bar a colour is closest to (a receiver's check that a decoded frame is the pattern), with its distance.
pub fn nearest_bar(rgb: [u8; 3]) -> (usize, u32) {
    BARS.iter()
        .enumerate()
        .map(|(i, b)| {
            let d: u32 = b
                .iter()
                .zip(rgb)
                .map(|(a, c)| u32::from(a.abs_diff(c)))
                .sum();
            (i, d)
        })
        .min_by_key(|(_, d)| *d)
        .unwrap_or((0, u32::MAX))
}

/// share-options.ts `ShareResolution`: the largest frame a screen share sends.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Hash)]
pub enum ShareResolution {
    P720,
    #[default]
    P1080,
    P1440,
    /// The source's own size.
    Source,
}

impl ShareResolution {
    pub const ALL: [ShareResolution; 4] = [Self::P720, Self::P1080, Self::P1440, Self::Source];

    /// The value as share-options.ts names it (`720p`, ..., `source`).
    pub fn name(self) -> &'static str {
        match self {
            Self::P720 => "720p",
            Self::P1080 => "1080p",
            Self::P1440 => "1440p",
            Self::Source => "source",
        }
    }

    pub fn from_name(name: &str) -> Option<Self> {
        Self::ALL.into_iter().find(|r| r.name() == name)
    }

    /// The frame's bound, `None` for the source's size.
    pub fn frame(self) -> Option<(u32, u32)> {
        match self {
            Self::P720 => Some((1280, 720)),
            Self::P1080 => Some((1920, 1080)),
            Self::P1440 => Some((2560, 1440)),
            Self::Source => None,
        }
    }

    /// `SHARE_TARGET_KBPS`.
    fn target_kbps(self) -> u64 {
        match self {
            Self::P720 => 2500,
            Self::P1080 => 4500,
            Self::P1440 => 8000,
            Self::Source => 12000,
        }
    }
}

/// share-options.ts `SHARE_FPS`.
pub const SHARE_FPS: [u32; 3] = [15, 30, 60];
/// share-options.ts `SHARE_HEAVY_KBPS`: above it the estimate warns that frames may drop.
pub const SHARE_HEAVY_KBPS: u64 = 9000;

/// share-options.ts `ShareOptions`, narrowed to what a desktop share has: no surface choice and no system sound
/// (`shareSupport` with `desktop: true`).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct ShareOptions {
    pub resolution: ShareResolution,
    /// 15, 30 or 60.
    pub fps: u32,
}

impl Default for ShareOptions {
    /// `DEFAULT_SHARE_OPTIONS`: 1080p at 30 fps.
    fn default() -> Self {
        Self {
            resolution: ShareResolution::P1080,
            fps: 30,
        }
    }
}

impl ShareOptions {
    /// `shareBitrateBps`: the target for the resolution, scaled by the frame rate (`SHARE_FPS_FACTOR`), under the
    /// instance's cap when it has one.
    pub fn bitrate_bps(&self, cap_bps: Option<u64>) -> u64 {
        let factor = match self.fps {
            0..=15 => 0.6,
            16..=30 => 1.0,
            _ => 1.6,
        };
        let target = (self.resolution.target_kbps() as f64 * factor * 1000.0).round() as u64;
        cap_bps.map_or(target, |cap| target.min(cap))
    }

    /// `shareEstimate`: the upload in Mb/s with one decimal, and whether it is heavy.
    pub fn estimate(&self, cap_bps: Option<u64>) -> (String, bool) {
        let kbps = self.bitrate_bps(cap_bps) as f64 / 1000.0;
        (
            format!("{:.1}", kbps / 1000.0),
            kbps > SHARE_HEAVY_KBPS as f64,
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_pattern_has_eight_bars_and_a_moving_square() {
        let mut p = TestPattern::new(ShareKind::Screen, 160, 96, 15);
        let a = p.next_frame().unwrap();
        let b = p.next_frame().unwrap();
        assert_eq!((a.width, a.height, a.data.len()), (160, 96, 160 * 96 * 4));
        // The top row has no square: bar i spans x in [i*20, i*20+20).
        for i in 0..8u32 {
            let at = ((i * 20 + 10) * 4) as usize;
            assert_eq!(
                nearest_bar([a.data[at], a.data[at + 1], a.data[at + 2]]).0,
                i as usize
            );
        }
        assert_ne!(a, b, "the square moves");
    }

    #[test]
    fn a_pattern_ends_when_asked_and_its_interval_follows_its_rate() {
        let mut p = TestPattern::new(ShareKind::Camera, 32, 32, 20).ending_after(2);
        assert!(p.next_frame().is_some() && p.next_frame().is_some());
        assert!(p.next_frame().is_none());
        assert_eq!(p.interval(), Duration::from_millis(50));
        assert_eq!(p.kind(), ShareKind::Camera);
    }

    /// share-options.test.ts: the targets, the frame-rate factor, the cap and the heavy mark.
    #[test]
    fn share_bitrates_follow_share_options_ts() {
        let o = |resolution, fps| ShareOptions { resolution, fps };
        assert_eq!(ShareOptions::default().bitrate_bps(None), 4_500_000);
        assert_eq!(o(ShareResolution::P720, 15).bitrate_bps(None), 1_500_000);
        assert_eq!(o(ShareResolution::P1440, 60).bitrate_bps(None), 12_800_000);
        assert_eq!(
            o(ShareResolution::Source, 30).bitrate_bps(Some(3_000_000)),
            3_000_000
        );
        assert_eq!(
            ShareOptions::default().estimate(None),
            ("4.5".to_owned(), false)
        );
        assert_eq!(
            o(ShareResolution::P1440, 60).estimate(None),
            ("12.8".to_owned(), true)
        );
        assert_eq!(
            ShareResolution::from_name("1440p"),
            Some(ShareResolution::P1440)
        );
        assert_eq!(ShareResolution::P720.frame(), Some((1280, 720)));
    }
}
