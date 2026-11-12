//! `assert_screen`: one screen fixture against its web reference.
//!
//! seed (the GPUI side draws the fixture's world; the web side drew it when the reference was shot) → launch the
//! automation build → replay the steps → shoot → build the R13 text mask from the app's `text-bounds` → compare with
//! `references/<os>/<name>-2x.png` under both gates and the committed tolerance (`tolerance.toml`, no per-fixture
//! setting).

use std::path::{Path, PathBuf};

use image::RgbaImage;

use crate::fixture::{self, LoadedFixture, Step};
use crate::{
    Driver, ElementInfo, Os, ParityFailure, Rect, TextMask, assert_parity_masked_with,
    chrome_difference_pct, tolerance_for,
};

/// The glow of anti-aliasing around a glyph, in device pixels: the grow every screen mask uses. It is the one number of
/// the mask, kept here so that no screen test picks its own.
pub const TEXT_FRINGE_PX: f32 = 2.0;

/// The headless platform renders at scale 2.0 (`dilla-desktop/src/automation.rs`).
const SCALE: f32 = 2.0;

/// What a passing comparison measured.
#[derive(Clone, Debug)]
pub struct ScreenReport {
    pub name: String,
    /// SHA-256 of the fixture file.
    pub digest: String,
    /// The fixture line the app logged (`fixture <name> <digest> <ids>`).
    pub fixture_line: String,
    /// How many text lines the app reported.
    pub text_lines: usize,
    /// The share of unmasked pixels over the chrome delta.
    pub chrome_pct: f64,
    /// The GPUI frame, saved under `<target>/screen-parity/`.
    pub shot: PathBuf,
}

/// The device-pixel rectangle (scale 2) of a logical line box, grown by `grow` device pixels for the glyphs' fringe.
fn device_rect(e: &ElementInfo, grow: f32) -> Rect {
    let x0 = (e.x * SCALE - grow).floor().max(0.0);
    let y0 = (e.y * SCALE - grow).floor().max(0.0);
    let x1 = ((e.x + e.width) * SCALE + grow).ceil();
    let y1 = ((e.y + e.height) * SCALE + grow).ceil();
    Rect {
        x: x0 as u32,
        y: y0 as u32,
        w: (x1 - x0) as u32,
        h: (y1 - y0) as u32,
    }
}

/// The R13 mask of the frame the driver's app shows now: the line boxes of its `text-bounds` query, in device pixels,
/// each grown by `grow_px`. Nothing else: never the pixels of a reference.
pub fn text_mask(driver: &mut Driver, grow_px: f32) -> TextMask {
    let lines = driver.text_bounds();
    TextMask::from_rects(lines.iter().map(|e| device_rect(e, grow_px)))
}

/// Replays one step on the GPUI side. The `web` selectors are the web shooter's business; a `gpui` of `-` is a step with
/// no GPUI counterpart (the web side closes a pane the GPUI screen does not have yet) and is skipped.
pub fn replay(driver: &mut Driver, step: &Step) {
    match step {
        Step::Click { gpui, .. } | Step::Hover { gpui, .. } | Step::Type { gpui, .. }
            if gpui == "-" => {}
        Step::Click { gpui, .. } => driver.click(gpui),
        Step::Hover { gpui, .. } => driver.hover(gpui),
        Step::Key { chord } => driver.press(chord),
        Step::Type { gpui, text, .. } => driver.type_text(gpui, text),
        Step::Settle => {
            driver.wait_idle();
        }
    }
}

fn fixture_failure(reason: impl Into<String>, path: &Path) -> ParityFailure {
    ParityFailure::Reference {
        path: path.display().to_string(),
        reason: reason.into(),
    }
}

/// Compares the screen of `fixture` (`fixtures/<area>/<state>[-b].json`) with its reference on this OS.
pub fn assert_screen(fixture: &Path) -> Result<ScreenReport, ParityFailure> {
    assert_screen_with(fixture, &[])
}

/// [`assert_screen`] with environment variables for the app: the negative controls' hook (the fallback font).
pub fn assert_screen_with(
    fixture: &Path,
    env: &[(&str, &str)],
) -> Result<ScreenReport, ParityFailure> {
    let os = Os::current();
    let loaded = fixture::load(fixture, os).map_err(|e| fixture_failure(e.to_string(), fixture))?;
    if let Some(shot) = &loaded.shot
        && shot.digest != loaded.digest
    {
        return Err(fixture_failure(
            format!(
                "the reference was shot from another version of this fixture (shot {}, fixture {}): shoot it again with parity/shoot.mjs",
                shot.digest, loaded.digest
            ),
            fixture,
        ));
    }
    let tolerance = tolerance_for(os)?;
    let mut driver = Driver::launch_fixture(fixture, loaded.shot_path.as_deref(), env);
    let (shot, mask, text_lines, fixture_line) = capture(&mut driver, &loaded);
    let out = crate::target_dir().join("screen-parity");
    let shot_path = out.join(format!(
        "{}-gpui-2x.png",
        loaded.fixture.name.replace('/', "-")
    ));
    std::fs::create_dir_all(&out).expect("the screen-parity directory");
    shot.save(&shot_path).expect("the shot is saved");
    assert_parity_masked_with(&shot, &loaded.reference, tolerance, &mask)?;
    let reference = image::open(&loaded.reference)
        .map_err(|e| fixture_failure(e.to_string(), &loaded.reference))?
        .to_rgba8();
    let chrome_pct = chrome_difference_pct(&shot, &reference, tolerance.chrome_max_delta, &mask)?;
    Ok(ScreenReport {
        name: loaded.fixture.name.clone(),
        digest: loaded.digest.clone(),
        fixture_line,
        text_lines,
        chrome_pct,
        shot: shot_path,
    })
}

/// Settles the screen, replays the steps, and takes the frame with its R13 mask.
fn capture(driver: &mut Driver, loaded: &LoadedFixture) -> (RgbaImage, TextMask, usize, String) {
    let line = driver
        .fixture_line()
        .expect("a fixture launch answers `fixture`");
    driver.wait_idle();
    for step in &loaded.fixture.steps {
        replay(driver, step);
    }
    driver.wait_idle();
    let shot = driver.screenshot();
    let lines = driver.text_bounds().len();
    let mask = text_mask(driver, TEXT_FRINGE_PX);
    (shot, mask, lines, line)
}
