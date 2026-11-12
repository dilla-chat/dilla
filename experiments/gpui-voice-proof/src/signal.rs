//! Test signals: the published tone and video pattern, and the peak search the receiver runs.

use std::f64::consts::TAU;

pub const RATE: u32 = 48_000;
/// 10 ms of mono audio at 48 kHz, the frame LiveKit's audio source takes.
pub const FRAME: usize = (RATE / 100) as usize;

/// A continuous sine at `hz`, half scale, 10 ms at a time.
pub struct Tone {
    hz: f64,
    n: u64,
}

impl Tone {
    pub fn new(hz: f64) -> Self {
        Self { hz, n: 0 }
    }

    pub fn next_frame(&mut self) -> Vec<i16> {
        (0..FRAME)
            .map(|_| {
                let t = self.n as f64 / f64::from(RATE);
                self.n += 1;
                ((TAU * self.hz * t).sin() * 0.5 * f64::from(i16::MAX)) as i16
            })
            .collect()
    }
}

/// The power of `hz` in `samples` (Goertzel), normalised by the sample count.
pub fn power_at(samples: &[f32], rate: u32, hz: f64) -> f64 {
    let w = TAU * hz / f64::from(rate);
    let coeff = 2.0 * w.cos();
    let (mut s1, mut s2) = (0.0f64, 0.0f64);
    for &x in samples {
        let s0 = f64::from(x) + coeff * s1 - s2;
        s2 = s1;
        s1 = s0;
    }
    let p = s1 * s1 + s2 * s2 - coeff * s1 * s2;
    p / (samples.len().max(1) as f64).powi(2)
}

/// The frequency with the most power between 100 Hz and 4 kHz, in 10 Hz steps.
pub fn peak_hz(samples: &[f32], rate: u32) -> f64 {
    let mut best = (0.0, f64::MIN);
    let mut hz = 100.0;
    while hz <= 4_000.0 {
        let p = power_at(samples, rate, hz);
        if p > best.1 {
            best = (hz, p);
        }
        hz += 10.0;
    }
    best.0
}

/// An I420 test pattern: moving diagonal luma bars and a colour ramp, so the encoder emits delta
/// frames with content. Returns (y, u, v) planes for `width` x `height`.
pub fn pattern_i420(width: u32, height: u32, frame: u64) -> (Vec<u8>, Vec<u8>, Vec<u8>) {
    let (w, h) = (width as usize, height as usize);
    let shift = (frame * 4) as usize;
    let mut y = vec![0u8; w * h];
    for row in 0..h {
        for col in 0..w {
            y[row * w + col] = ((col + row + shift) & 0xff) as u8;
        }
    }
    let (cw, ch) = (w.div_ceil(2), h.div_ceil(2));
    let mut u = vec![0u8; cw * ch];
    let mut v = vec![0u8; cw * ch];
    for row in 0..ch {
        for col in 0..cw {
            u[row * cw + col] = ((col * 255) / cw.max(1)) as u8;
            v[row * cw + col] = ((row * 255) / ch.max(1)) as u8;
        }
    }
    (y, u, v)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_tone_peaks_at_its_frequency() {
        let mut t = Tone::new(440.0);
        let samples: Vec<f32> = (0..20)
            .flat_map(|_| t.next_frame())
            .map(|s| f32::from(s) / 32768.0)
            .collect();
        assert_eq!(peak_hz(&samples, RATE), 440.0);
        assert!(power_at(&samples, RATE, 440.0) > 100.0 * power_at(&samples, RATE, 1000.0));
    }

    #[test]
    fn the_pattern_has_the_requested_planes() {
        let (y, u, v) = pattern_i420(640, 360, 3);
        assert_eq!(
            (y.len(), u.len(), v.len()),
            (640 * 360, 320 * 180, 320 * 180)
        );
        assert_ne!(pattern_i420(640, 360, 0).0, y);
    }
}
