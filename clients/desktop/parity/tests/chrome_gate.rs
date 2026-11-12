//! The chrome gate against Task 3's evidence renders (Linux, scale 2, 2560x1600): the correct GPUI
//! render and four deliberately wrong ones, all compared with the Chromium "nohint" reference.

use std::path::{Path, PathBuf};

use dilla_parity::{
    Os, ParityFailure, Rect, RgbaImage, TextMask, assert_parity, assert_parity_masked,
    assert_parity_masked_with, chrome_difference_pct, tolerance_for,
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
    TextMask::from_bright_pixels_fixture_only(&load("reference-nohint-2x.png"), 70, 3)
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
    // Text is displaced too, so the whole-frame gate trips first.
    assert!(matches!(
        run("layout-shift-2x.png"),
        Err(ParityFailure::Pixels { .. })
    ));
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

#[test]
fn chrome_margins_measured_in_rust() {
    let tolerance = tolerance_for(Os::Linux).unwrap();
    let reference = load("reference-nohint-2x.png");
    let mask = text_mask();
    let pct = |name: &str| {
        chrome_difference_pct(&load(name), &reference, tolerance.chrome_max_delta, &mask).unwrap()
    };
    let (good, b1, b2) = (
        pct("good-2x.png"),
        pct("border-shift-2x.png"),
        pct("layout-shift-2x.png"),
    );
    println!(
        "CHROME good {good:.4} b1 {b1:.4} b2 {b2:.4} limit {}",
        tolerance.chrome_max_pct
    );
    assert!(good < tolerance.chrome_max_pct / 2.0, "good {good}");
    assert!(b1 > tolerance.chrome_max_pct * 4.0, "b1 {b1}");
}

/// A 1px-bordered accent box (#5AA8E6 fill, light border) whose border moves down by `border_dy`.
fn accent_box(border_dy: u32) -> RgbaImage {
    let edge = image::Rgba([230, 240, 250, 255]);
    let mut img = RgbaImage::from_pixel(600, 400, image::Rgba([8, 9, 11, 255]));
    let (x0, y0, w, h) = (100u32, 100u32, 200u32, 100u32);
    for y in y0..y0 + h {
        for x in x0..x0 + w {
            img.put_pixel(x, y, image::Rgba([90, 168, 230, 255]));
        }
    }
    for x in x0..x0 + w {
        img.put_pixel(x, y0 + border_dy, edge);
        img.put_pixel(x, y0 + h - 1 + border_dy, edge);
    }
    for y in y0..y0 + h {
        img.put_pixel(x0, y + border_dy, edge);
        img.put_pixel(x0 + w - 1, y + border_dy, edge);
    }
    img
}

#[test]
fn bright_pixel_mask_hides_accent_chrome_but_rect_mask_catches_it() {
    let dir = tempfile::tempdir().unwrap();
    let reference_path = dir.path().join("reference.png");
    accent_box(0).save(&reference_path).unwrap();
    let shifted = accent_box(1);
    let tolerance = tolerance_for(Os::Linux).unwrap();

    // The fixture-only bright mask swallows the whole box: the border shift is missed.
    let bright = TextMask::from_bright_pixels_fixture_only(&accent_box(0), 70, 3);
    assert_parity_masked_with(&shifted, &reference_path, tolerance, &bright).unwrap();

    // A rect mask that covers only the text (here: a label elsewhere) catches it.
    let text_only = TextMask::from_rects([Rect {
        x: 10,
        y: 10,
        w: 60,
        h: 12,
    }]);
    assert!(matches!(
        assert_parity_masked_with(&shifted, &reference_path, tolerance, &text_only),
        Err(ParityFailure::Chrome { .. })
    ));
}

#[test]
fn mask_covering_most_of_the_frame_is_an_error() {
    let reference = fixture("reference-nohint-2x.png");
    let everything = TextMask::from_rects([Rect {
        x: 0,
        y: 0,
        w: 2560,
        h: 1000,
    }]);
    assert!(matches!(
        assert_parity_masked_with(
            &load("good-2x.png"),
            &reference,
            tolerance_for(Os::Linux).unwrap(),
            &everything
        ),
        Err(ParityFailure::Mask { .. })
    ));
}
