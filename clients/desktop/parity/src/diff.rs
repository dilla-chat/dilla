use std::path::Path;

use image::{Rgba, RgbaImage};
use serde::Deserialize;

/// The operating systems with their own tolerance (text rasterisation differs per OS).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Os {
    Linux,
    MacOs,
    Windows,
}

impl Os {
    pub fn current() -> Self {
        if cfg!(target_os = "macos") {
            Self::MacOs
        } else if cfg!(target_os = "windows") {
            Self::Windows
        } else {
            Self::Linux
        }
    }
}

/// How far an actual image may stray from its reference.
#[derive(Clone, Copy, Debug, PartialEq, Deserialize)]
pub struct Tolerance {
    /// Largest per-channel difference (0..=255) at which a pixel still counts as equal.
    pub max_delta: u8,
    /// Share of pixels, in percent (0..=100), allowed to differ by more than `max_delta`.
    pub max_pct: f64,
    /// Chrome gate, applied only to pixels outside the text mask (see [`assert_parity_masked`]):
    /// largest per-channel difference at which a non-text pixel still counts as equal. It is far
    /// tighter than `max_delta`, because borders differ from their background by only ~40.
    pub chrome_max_delta: u8,
    /// Share of non-text (unmasked) pixels, in percent, allowed to differ by more than `chrome_max_delta`.
    pub chrome_max_pct: f64,
}

/// A rectangle in pixel coordinates of the reference (images are never rescaled, so also of the capture).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Rect {
    pub x: u32,
    pub y: u32,
    pub w: u32,
    pub h: u32,
}

/// The areas where text is drawn. Glyph anti-aliasing differs between renderers, so the chrome
/// gate ignores these pixels; the whole-frame gate still covers them.
///
/// Where screens get it from: the automation interface reports each text element's bounds
/// ([`crate::ElementInfo`]); a screen test converts them to device pixels (logical x scale) and
/// calls [`TextMask::from_rects`]. Until a screen reports bounds, [`TextMask::from_bright_pixels`]
/// derives a mask from the reference image itself.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct TextMask {
    rects: Vec<Rect>,
}

impl TextMask {
    pub fn from_rects(rects: impl IntoIterator<Item = Rect>) -> Self {
        Self {
            rects: rects.into_iter().collect(),
        }
    }

    pub fn rects(&self) -> &[Rect] {
        &self.rects
    }

    /// Masks everything within `grow` pixels of a `reference` pixel whose brightest channel is at
    /// least `min_channel`. Pick `min_channel` above the brightest border or fill colour (only text
    /// is that bright) and `grow` to the glyph anti-aliasing fringe.
    pub fn from_bright_pixels(reference: &RgbaImage, min_channel: u8, grow: u32) -> Self {
        let mut rects = Vec::new();
        let (w, h) = reference.dimensions();
        let max_gap = grow.max(1) * 2;
        for y in 0..h {
            let mut run: Option<(u32, u32)> = None; // first and last bright x of the current run
            let mut push = |(a, b): (u32, u32)| {
                rects.push(Rect {
                    x: a.saturating_sub(grow),
                    y: y.saturating_sub(grow),
                    w: b - a + 1 + 2 * grow,
                    h: 1 + 2 * grow,
                });
            };
            for x in 0..w {
                let p = reference.get_pixel(x, y).0;
                if p[0].max(p[1]).max(p[2]) < min_channel {
                    continue;
                }
                run = match run {
                    Some((a, last)) if x - last <= max_gap => Some((a, x)),
                    Some(done) => {
                        push(done);
                        Some((x, x))
                    }
                    None => Some((x, x)),
                };
            }
            if let Some(done) = run {
                push(done);
            }
        }
        Self { rects }
    }

    fn covered(&self, (w, h): (u32, u32)) -> Vec<bool> {
        let mut covered = vec![false; w as usize * h as usize];
        for r in &self.rects {
            let (a, b) = (r.x.min(w) as usize, r.x.saturating_add(r.w).min(w) as usize);
            for y in r.y.min(h)..r.y.saturating_add(r.h).min(h) {
                let row = y as usize * w as usize;
                covered[row + a..row + b].fill(true);
            }
        }
        covered
    }
}

#[derive(Debug, thiserror::Error)]
pub enum ParityFailure {
    /// The capture and the reference differ by a whole-number factor: they were taken at different
    /// display scales. Nothing is rescaled; capture at the reference's scale.
    #[error(
        "scale mismatch: capture is {actual:?} px, reference is {reference:?} px (scale ratio {ratio})"
    )]
    Scale {
        actual: (u32, u32),
        reference: (u32, u32),
        ratio: f64,
    },
    #[error("size mismatch: capture is {actual:?} px, reference is {reference:?} px")]
    Size {
        actual: (u32, u32),
        reference: (u32, u32),
    },
    #[error(
        "{differing} of {total} pixels ({pct:.4} %) differ by more than {max_delta}; \
         allowed {allowed_pct} %; largest channel delta {largest_delta}"
    )]
    Pixels {
        differing: u64,
        total: u64,
        pct: f64,
        largest_delta: u8,
        max_delta: u8,
        allowed_pct: f64,
    },
    /// Non-text pixels (borders, fills, shapes) moved or changed colour.
    #[error(
        "chrome (non-text) pixels: {pct:.4} % differ by more than {max_delta}; allowed {max_pct} %"
    )]
    Chrome {
        pct: f64,
        max_pct: f64,
        max_delta: u8,
    },
    #[error("reference image {path}: {reason}")]
    Reference { path: String, reason: String },
    #[error("tolerance file: {0}")]
    Tolerance(String),
}

#[derive(Deserialize)]
struct ToleranceFile {
    linux: Tolerance,
    macos: Tolerance,
    windows: Tolerance,
}

/// Reads `tolerance.toml` (next to this crate's manifest) for `os`.
pub fn tolerance_for(os: Os) -> Result<Tolerance, ParityFailure> {
    let path = Path::new(env!("CARGO_MANIFEST_DIR")).join("tolerance.toml");
    let text = std::fs::read_to_string(&path)
        .map_err(|e| ParityFailure::Tolerance(format!("{}: {e}", path.display())))?;
    let file: ToleranceFile =
        toml::from_str(&text).map_err(|e| ParityFailure::Tolerance(e.to_string()))?;
    Ok(match os {
        Os::Linux => file.linux,
        Os::MacOs => file.macos,
        Os::Windows => file.windows,
    })
}

/// Compares `actual` with the reference PNG at `reference`, using the tolerance recorded for `os`
/// in `tolerance.toml`. The reference's scale is its pixel size; the images are never rescaled.
/// Whole-frame gate only; see [`assert_parity_masked`] for the chrome gate.
pub fn assert_parity(actual: &RgbaImage, reference: &Path, os: Os) -> Result<(), ParityFailure> {
    assert_parity_with(actual, reference, tolerance_for(os)?)
}

/// [`assert_parity`] with an explicit tolerance.
pub fn assert_parity_with(
    actual: &RgbaImage,
    reference: &Path,
    tolerance: Tolerance,
) -> Result<(), ParityFailure> {
    compare(actual, &load(reference)?, tolerance, None)
}

/// [`assert_parity`] plus the chrome gate: the whole-frame gate covers every pixel, then pixels
/// outside `mask` must also stay within `chrome_max_delta` / `chrome_max_pct`. This catches what
/// the whole-frame gate cannot, such as a 1px border shift.
pub fn assert_parity_masked(
    actual: &RgbaImage,
    reference: &Path,
    os: Os,
    mask: &TextMask,
) -> Result<(), ParityFailure> {
    assert_parity_masked_with(actual, reference, tolerance_for(os)?, mask)
}

/// [`assert_parity_masked`] with an explicit tolerance.
pub fn assert_parity_masked_with(
    actual: &RgbaImage,
    reference: &Path,
    tolerance: Tolerance,
    mask: &TextMask,
) -> Result<(), ParityFailure> {
    compare(actual, &load(reference)?, tolerance, Some(mask))
}

fn load(reference: &Path) -> Result<RgbaImage, ParityFailure> {
    Ok(image::open(reference)
        .map_err(|e| ParityFailure::Reference {
            path: reference.display().to_string(),
            reason: e.to_string(),
        })?
        .to_rgba8())
}

fn pixel_delta(p: &Rgba<u8>, q: &Rgba<u8>) -> u8 {
    p.0.iter()
        .zip(q.0)
        .map(|(x, y)| x.abs_diff(y))
        .max()
        .unwrap_or(0)
}

fn percent(part: u64, total: u64) -> f64 {
    if total == 0 {
        0.0
    } else {
        part as f64 * 100.0 / total as f64
    }
}

fn compare(
    actual: &RgbaImage,
    reference: &RgbaImage,
    tolerance: Tolerance,
    mask: Option<&TextMask>,
) -> Result<(), ParityFailure> {
    let (a, r) = (actual.dimensions(), reference.dimensions());
    if a != r {
        let ratio_w = f64::from(a.0) / f64::from(r.0);
        let ratio_h = f64::from(a.1) / f64::from(r.1);
        return Err(if (ratio_w - ratio_h).abs() < 1e-9 {
            ParityFailure::Scale {
                actual: a,
                reference: r,
                ratio: ratio_w,
            }
        } else {
            ParityFailure::Size {
                actual: a,
                reference: r,
            }
        });
    }
    let (mut differing, mut largest) = (0u64, 0u8);
    for (p, q) in actual.pixels().zip(reference.pixels()) {
        let delta = pixel_delta(p, q);
        largest = largest.max(delta);
        if delta > tolerance.max_delta {
            differing += 1;
        }
    }
    let total = u64::from(a.0) * u64::from(a.1);
    let pct = percent(differing, total);
    if differing > 0 && pct > tolerance.max_pct {
        return Err(ParityFailure::Pixels {
            differing,
            total,
            pct,
            largest_delta: largest,
            max_delta: tolerance.max_delta,
            allowed_pct: tolerance.max_pct,
        });
    }
    if let Some(mask) = mask {
        let covered = mask.covered(a);
        let (mut chrome_total, mut chrome_differing) = (0u64, 0u64);
        for ((p, q), &is_text) in actual.pixels().zip(reference.pixels()).zip(&covered) {
            if is_text {
                continue;
            }
            chrome_total += 1;
            if pixel_delta(p, q) > tolerance.chrome_max_delta {
                chrome_differing += 1;
            }
        }
        let chrome_pct = percent(chrome_differing, chrome_total);
        if chrome_differing > 0 && chrome_pct > tolerance.chrome_max_pct {
            return Err(ParityFailure::Chrome {
                pct: chrome_pct,
                max_pct: tolerance.chrome_max_pct,
                max_delta: tolerance.chrome_max_delta,
            });
        }
    }
    Ok(())
}
