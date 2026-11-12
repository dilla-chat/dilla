//! The chrome gate against Task 3's evidence renders (Linux, scale 2, 2560x1600): the correct GPUI
//! render and four deliberately wrong ones, all compared with the Chromium "nohint" reference.

use std::path::{Path, PathBuf};

use dilla_parity::{
    Os, ParityFailure, Rect, RgbaImage, TextMask, assert_parity, assert_parity_masked,
    assert_parity_masked_with, tolerance_for,
};

fn fixture(name: &str) -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("tests/fixtures")
        .join(name)
}

fn load(name: &str) -> RgbaImage {
    image::open(fixture(name)).unwrap().to_rgba8()
}

/// Text is the only content brighter than channel 70 (the brightest border is ~#2A303A = 58); a
/// 3 px fringe covers glyph anti-aliasing.
fn text_mask() -> TextMask {
    TextMask::from_bright_pixels(&load("reference-nohint-2x.png"), 70, 3)
}

fn run(name: &str) -> Result<(), ParityFailure> {
    assert_parity_masked(
        &load(name),
        &fixture("reference-nohint-2x.png"),
        Os::Linux,
        &text_mask(),
    )
}

#[test]
fn good_passes_both_gates() {
    run("good-2x.png").unwrap();
}

#[test]
fn border_only_shift_fails_chrome_gate() {
    let reference = fixture("reference-nohint-2x.png");
    let actual = load("border-shift-2x.png");
    // The whole-frame gate alone cannot see it (Task 3's known gap) ...
    assert_parity(&actual, &reference, Os::Linux).unwrap();
    // ... the chrome gate does.
    match run("border-shift-2x.png") {
        Err(ParityFailure::Chrome { pct, max_pct, .. }) => {
            assert!(pct > max_pct * 4.0, "margin too thin: {pct} vs {max_pct}");
        }
        other => panic!("expected a chrome failure, got {other:?}"),
    }
}

#[test]
fn layout_shift_fails() {
    assert!(run("layout-shift-2x.png").is_err());
}

#[test]
fn wrong_colour_fails() {
    // Text only: caught by the whole-frame gate, not the chrome gate.
    assert!(matches!(
        run("wrong-colour-2x.png"),
        Err(ParityFailure::Pixels { .. })
    ));
}

#[test]
fn fallback_font_fails() {
    assert!(matches!(
        run("fallback-font-2x.png"),
        Err(ParityFailure::Pixels { .. })
    ));
}

#[test]
fn chrome_gate_ignores_text_antialiasing() {
    // The good render's text differs from the reference (anti-aliasing, hinting) well beyond the
    // chrome delta, yet it passes: those pixels are masked. Without the mask the same pixels fail.
    let tolerance = tolerance_for(Os::Linux).unwrap();
    let reference = fixture("reference-nohint-2x.png");
    let good = load("good-2x.png");
    assert_parity_masked_with(&good, &reference, tolerance, &text_mask()).unwrap();
    assert!(matches!(
        assert_parity_masked_with(&good, &reference, tolerance, &TextMask::default()),
        Err(ParityFailure::Chrome { .. })
    ));
}

#[test]
fn explicit_rects_mask_pixels() {
    let reference = fixture("reference-nohint-2x.png");
    let mut actual = load("reference-nohint-2x.png");
    // One block of 100 x 100 changed pixels: 0.4 % of 2.56 Mpx would fail unmasked, passes masked.
    for y in 500..600 {
        for x in 1800..1900 {
            actual.put_pixel(x, y, image::Rgba([255, 0, 0, 255]));
        }
    }
    let tolerance = tolerance_for(Os::Linux).unwrap();
    let all = TextMask::from_rects([Rect {
        x: 1800,
        y: 500,
        w: 100,
        h: 100,
    }]);
    assert_parity_masked_with(&actual, &reference, tolerance, &all).unwrap();
    assert!(matches!(
        assert_parity_masked_with(&actual, &reference, tolerance, &TextMask::default()),
        Err(ParityFailure::Chrome { .. })
    ));
}
