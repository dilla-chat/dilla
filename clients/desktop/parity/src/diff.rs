use std::path::Path;

use image::RgbaImage;
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
pub fn assert_parity(actual: &RgbaImage, reference: &Path, os: Os) -> Result<(), ParityFailure> {
    assert_parity_with(actual, reference, tolerance_for(os)?)
}

/// [`assert_parity`] with an explicit tolerance.
pub fn assert_parity_with(
    actual: &RgbaImage,
    reference: &Path,
    tolerance: Tolerance,
) -> Result<(), ParityFailure> {
    let reference_image = image::open(reference)
        .map_err(|e| ParityFailure::Reference {
            path: reference.display().to_string(),
            reason: e.to_string(),
        })?
        .to_rgba8();
    compare(actual, &reference_image, tolerance)
}

fn compare(
    actual: &RgbaImage,
    reference: &RgbaImage,
    tolerance: Tolerance,
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
        let delta =
            p.0.iter()
                .zip(q.0)
                .map(|(x, y)| x.abs_diff(y))
                .max()
                .unwrap_or(0);
        largest = largest.max(delta);
        if delta > tolerance.max_delta {
            differing += 1;
        }
    }
    let total = u64::from(a.0) * u64::from(a.1);
    let pct = if total == 0 {
        0.0
    } else {
        differing as f64 * 100.0 / total as f64
    };
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
    Ok(())
}
