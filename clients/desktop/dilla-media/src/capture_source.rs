//! Where a call's camera and screen-share video come from: a [`CaptureSource`] hands RGBA frames at its own pace, and
//! [`crate::Call::publish_video`] encodes them under the call's `dilla-sframe/1` keys (the camera slot or the screen
//! slot), exactly as the microphone is.
//!
//! The real screen and camera sources come with the next plan. Until then the only source is [`TestPattern`], the
//! moving test pattern the tests (and the automation build) share; a cancelled selection is a source that never starts
//! ([`crate::Call::publish_video`] is simply not called), so nothing is published.

use std::time::Duration;

/// Which slot a source fills.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub enum ShareKind {
    Camera,
    Screen,
}

/// One captured frame: `width * height` RGBA pixels, row after row.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct RgbaFrame {
    pub width: u32,
    pub height: u32,
    pub rgba: Vec<u8>,
}

/// A source of video frames for one share.
pub trait CaptureSource: Send + 'static {
    fn kind(&self) -> ShareKind;
    /// The frame size the encoder is opened with.
    fn size(&self) -> (u32, u32);
    /// The frames per second the source delivers at most.
    fn fps(&self) -> u32;
    /// Whether the share carries its sound (a screen share whose picker offered sound and the person kept it).
    fn has_audio(&self) -> bool {
        false
    }
    /// The next frame, or `None` once the source ended (the person stopped it from the system, the window closed).
    fn next_frame(&mut self) -> Option<RgbaFrame>;

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
    fn next_frame(&mut self) -> Option<RgbaFrame> {
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
        Some(RgbaFrame {
            width: self.width,
            height: self.height,
            rgba,
        })
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_pattern_has_eight_bars_and_a_moving_square() {
        let mut p = TestPattern::new(ShareKind::Screen, 160, 96, 15);
        let a = p.next_frame().unwrap();
        let b = p.next_frame().unwrap();
        assert_eq!((a.width, a.height, a.rgba.len()), (160, 96, 160 * 96 * 4));
        // The top row has no square: bar i spans x in [i*20, i*20+20).
        for i in 0..8u32 {
            let at = ((i * 20 + 10) * 4) as usize;
            assert_eq!(
                nearest_bar([a.rgba[at], a.rgba[at + 1], a.rgba[at + 2]]).0,
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
}
