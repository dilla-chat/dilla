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
    chrome_difference_pct, colour_difference_pct, tolerance_for, whole_difference_pct,
};

/// Device pixels the mask grows every reported line box by. Measured (`text_fringe_is_the_measured_reach_of_glyphs`):
/// GPUI's own glyph pixels reach 0 px beyond the boxes the app reports, over both shell fixtures. The 2 px is the margin
/// for the other renderer: Chromium's anti-aliasing of the reference glyphs, which the chrome gate must not read as
/// chrome. The test fails if GPUI's glyphs ever reach past it. The one number of the mask, so no screen picks its own.
pub const TEXT_FRINGE_PX: f32 = 2.0;

/// Set, `assert_screen_with` prints a [`measure_line`] for the frame and skips the comparison: the macOS CI plan's
/// measurement run (gpui-proofs.yml `mac-parity-measure`), which picks an OS's tolerances. Test support only: no CI gate
/// may set it (desktop-gpui.yml never does).
pub const MEASURE_VAR: &str = "DILLA_PARITY_MEASURE";

/// The deltas a measurement scores the whole-frame and chrome gates at, beside the Linux ones (64 and 16), in case an
/// OS's anti-aliasing needs a wider delta.
const MEASURE_WHOLE_DELTAS: [u8; 3] = [64, 96, 128];
const MEASURE_CHROME_DELTAS: [u8; 2] = [16, 24];

/// One `MEASURE` line: `label`'s frame scored against `reference` the way each gate scores it, at the Linux deltas
/// (`whole` at 64, `chrome` at 16, `colour` at 2 within 2 px) and at the wider ones (`whole96`, `whole128`, `chrome24`).
pub fn measure_line(
    label: &str,
    shot: &RgbaImage,
    reference: &RgbaImage,
    mask: &TextMask,
) -> Result<String, ParityFailure> {
    let linux = tolerance_for(Os::Linux)?;
    let whole: Vec<f64> = MEASURE_WHOLE_DELTAS
        .iter()
        .map(|d| whole_difference_pct(shot, reference, *d))
        .collect::<Result<_, _>>()?;
    let chrome: Vec<f64> = MEASURE_CHROME_DELTAS
        .iter()
        .map(|d| chrome_difference_pct(shot, reference, *d, mask))
        .collect::<Result<_, _>>()?;
    let colour = colour_difference_pct(
        shot,
        reference,
        linux.colour_max_delta,
        linux.colour_radius,
        mask,
    )?;
    Ok(format!(
        "MEASURE {label} whole={:.4} chrome={:.4} colour={:.4} whole96={:.4} whole128={:.4} chrome24={:.4}",
        whole[0], chrome[0], colour, whole[1], whole[2], chrome[1]
    ))
}

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
    // The sidecar is what ties a reference to the fixture it was shot from; a reference without one is unchecked.
    let Some(shot) = &loaded.shot else {
        return Err(fixture_failure(
            "no sidecar (<state>.shot.json) beside the reference: shoot it with parity/shoot.mjs",
            fixture,
        ));
    };
    if shot.digest != loaded.digest {
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
    let reference = image::open(&loaded.reference)
        .map_err(|e| fixture_failure(e.to_string(), &loaded.reference))?
        .to_rgba8();
    if std::env::var_os(MEASURE_VAR).is_some() {
        println!(
            "{}",
            measure_line(&loaded.fixture.name, &shot, &reference, &mask)?
        );
        return Ok(ScreenReport {
            name: loaded.fixture.name.clone(),
            digest: loaded.digest.clone(),
            fixture_line,
            text_lines,
            chrome_pct: chrome_difference_pct(
                &shot,
                &reference,
                tolerance.chrome_max_delta,
                &mask,
            )?,
            shot: shot_path,
        });
    }
    assert_parity_masked_with(&shot, &loaded.reference, tolerance, &mask)?;
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

/// What the web shooter does before its frame: with no hover step the pointer goes off the content and the focus is
/// cleared; with one the pointer stays where the step put it.
pub fn rest_before_frame(driver: &mut Driver, fixture: &fixture::ScreenFixture) {
    if !fixture
        .steps
        .iter()
        .any(|s| matches!(s, Step::Hover { .. }))
    {
        driver.rest();
    }
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
    // As the web shooter does before its frame: with no hover step the pointer goes off the content and the focus is
    // cleared; with one the pointer stays where the step put it.
    rest_before_frame(driver, &loaded.fixture);
    driver.wait_idle();
    let shot = driver.screenshot();
    let lines = driver.text_bounds().len();
    let mask = text_mask(driver, TEXT_FRINGE_PX);
    (shot, mask, lines, line)
}
