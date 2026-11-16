use std::path::{Path, PathBuf};
use std::process::Command;

use dilla_parity::{
    Driver, ElementInfo, GlyphCoverage, Os, ParityFailure, RgbaImage, Tolerance, assert_parity,
    assert_parity_with, glyph_coverage, target_dir, tolerance_for,
};
use image::Rgba;

const EXACT: Tolerance = Tolerance {
    max_delta: 0,
    max_pct: 0.0,
    chrome_max_delta: 0,
    chrome_max_pct: 0.0,
    colour_max_delta: 0,
    colour_radius: 0,
    colour_max_pct: 0.0,
};

fn workspace_root() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../..")
        .canonicalize()
        .unwrap()
}

fn grey(width: u32, height: u32, value: u8) -> RgbaImage {
    RgbaImage::from_pixel(width, height, Rgba([value, value, value, 255]))
}

fn reference_file(dir: &Path, image: &RgbaImage) -> PathBuf {
    let path = dir.join("reference.png");
    image.save(&path).unwrap();
    path
}

/// What marks an automation build: the marker string and the module's symbol path in both manglings.
const AUTOMATION_NEEDLES: [&str; 3] = [
    "dilla-automation-interface",
    "dilla_desktop::automation",
    "dilla_desktop10automation",
];

fn contains(haystack: &[u8], needle: &[u8]) -> bool {
    haystack.windows(needle.len()).any(|w| w == needle)
}

#[test]
#[ignore = "slow: run explicitly in CI (cargo test -p dilla-parity -- --ignored)"]
fn release_build_has_no_automation() {
    let cargo = std::env::var_os("CARGO").unwrap_or_else(|| "cargo".into());
    // Positive control: the needles below really are in an automation build, so their absence means something.
    let status = Command::new(&cargo)
        .args(["build", "-p", "dilla-desktop", "--features", "automation"])
        .current_dir(workspace_root())
        .status()
        .unwrap();
    assert!(status.success(), "automation build failed");
    let automation = std::fs::read(target_dir().join("debug/dilla-desktop")).unwrap();
    assert!(
        contains(&automation, b"dilla-automation-interface"),
        "the marker is missing from the automation build"
    );

    let status = Command::new(&cargo)
        .args(["build", "--release", "-p", "dilla-desktop"])
        .current_dir(workspace_root())
        .status()
        .unwrap();
    assert!(status.success(), "release build failed");
    let binary = std::fs::read(target_dir().join("release/dilla-desktop")).unwrap();
    // The marker string and the module's symbol path exist only in an automation build.
    for needle in AUTOMATION_NEEDLES {
        assert!(
            !contains(&binary, needle.as_bytes()),
            "release binary contains `{needle}`"
        );
    }
}

#[test]
#[cfg_attr(windows, ignore = "symbol names differ in a PDB build")]
fn symbol_needles_positive_control() {
    // The release test greps for these needles; this proves each one can be found in an automation build at all (a
    // needle that never matches would make that test pass for nothing).
    let _ = Driver::launch_harness();
    // The binary the driver ran: `DILLA_AUTOMATION_BIN` when it overrides the build.
    let binary = std::env::var_os("DILLA_AUTOMATION_BIN")
        .map_or_else(|| target_dir().join("debug/dilla-desktop"), PathBuf::from);
    let automation = std::fs::read(binary).unwrap();
    for needle in AUTOMATION_NEEDLES {
        assert!(
            contains(&automation, needle.as_bytes()),
            "`{needle}` is not in the automation binary: the release check would pass vacuously"
        );
    }
}

#[test]
fn identical_images_pass() {
    let dir = tempfile::tempdir().unwrap();
    let image = grey(40, 30, 120);
    let reference = reference_file(dir.path(), &image);
    assert_parity_with(&image, &reference, EXACT).unwrap();
}

#[test]
fn one_pixel_over_tolerance_fails() {
    let dir = tempfile::tempdir().unwrap();
    let reference = reference_file(dir.path(), &grey(40, 30, 120));
    let tolerance = Tolerance {
        max_delta: 2,
        max_pct: 0.0,
        chrome_max_delta: 0,
        chrome_max_pct: 0.0,
        colour_max_delta: 0,
        colour_radius: 0,
        colour_max_pct: 0.0,
    };

    let mut within = grey(40, 30, 120);
    within.put_pixel(3, 4, Rgba([122, 120, 120, 255]));
    assert_parity_with(&within, &reference, tolerance).unwrap();

    let mut over = grey(40, 30, 120);
    over.put_pixel(3, 4, Rgba([123, 120, 120, 255]));
    match assert_parity_with(&over, &reference, tolerance) {
        Err(ParityFailure::Pixels {
            differing: 1,
            largest_delta: 3,
            ..
        }) => {}
        other => panic!("expected one differing pixel, got {other:?}"),
    }

    // The same single pixel is allowed once a share of pixels may differ.
    let lenient = Tolerance {
        max_delta: 2,
        max_pct: 0.1,
        chrome_max_delta: 0,
        chrome_max_pct: 0.0,
        colour_max_delta: 0,
        colour_radius: 0,
        colour_max_pct: 0.0,
    };
    assert_parity_with(&over, &reference, lenient).unwrap();
}

#[test]
fn scale_mismatch_fails() {
    let dir = tempfile::tempdir().unwrap();
    let reference = reference_file(dir.path(), &grey(64, 40, 50));
    let captured_at_2x = grey(128, 80, 50);
    match assert_parity_with(&captured_at_2x, &reference, EXACT) {
        Err(ParityFailure::Scale {
            actual: (128, 80),
            reference: (64, 40),
            ratio,
        }) => {
            assert!((ratio - 2.0).abs() < 1e-9);
        }
        other => panic!("expected a scale failure, got {other:?}"),
    }
    // Not just a scale: unrelated sizes are a plain size failure.
    match assert_parity_with(&grey(70, 40, 50), &reference, EXACT) {
        Err(ParityFailure::Size { .. }) => {}
        other => panic!("expected a size failure, got {other:?}"),
    }
}

#[test]
fn tolerance_file_ships_measured_linux_and_exact_placeholders() {
    for os in [Os::MacOs, Os::Windows] {
        assert_eq!(tolerance_for(os).unwrap(), EXACT);
    }
    assert_eq!(
        tolerance_for(Os::Linux).unwrap(),
        Tolerance {
            max_delta: 64,
            max_pct: 0.5,
            chrome_max_delta: 16,
            chrome_max_pct: 0.2,
            colour_max_delta: 2,
            colour_radius: 2,
            colour_max_pct: 0.3,
        }
    );
    let dir = tempfile::tempdir().unwrap();
    let image = grey(8, 8, 9);
    assert_parity(&image, &reference_file(dir.path(), &image), Os::current()).unwrap();
}

#[test]
// GPUI's headless mode on Windows has no HeadlessRenderer ("render_to_image not available"), so the driver
// cannot capture there; Linux (lavapipe) and macOS (Metal) run it. Windows parity is measured with a real
// window in the screens plan's final sweep.
#[cfg_attr(windows, ignore = "GPUI headless has no renderer on Windows")]
fn click_and_read_element() {
    let fixture = tempfile::tempdir().unwrap();
    let mut driver = Driver::launch(fixture.path());

    let root = driver.element("root").expect("the root element exists");
    assert_eq!(root.id, "root");
    // A data root opens the shipping window (Task 8): the start window grown, in place, to the compact authentication
    // shape, 400x620 (`screens/shell/window.rs`: COMPACT_WIDTH, AUTH_HEIGHT), which lays out at that size.
    assert_eq!((root.width, root.height), (400.0, 620.0));
    assert_eq!((root.x, root.y), (0.0, 0.0));
    assert!(driver.element("no-such-element").is_none());

    driver.click("root");
    driver.type_text("root", "héllo");
    assert!(
        driver.element("root").is_some(),
        "the window survives input"
    );

    let shot = driver.screenshot();
    // The headless platform renders at scale 2.0, so the 400x620 window is 800x1240 device pixels.
    assert_eq!(shot.dimensions(), (800, 1240));
}

// ---- macOS CI plan, Tasks 1 and 2: glyphs drawn, and the title bar clear of the traffic lights ---------------------

fn shell_fixture(name: &str) -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("fixtures")
        .join(format!("{name}.json"))
}

/// Launches a shell fixture as `assert_screen` draws it (its steps replayed, the pointer and focus at rest), settled.
fn launch_shell(name: &str, env: &[(&str, &str)]) -> Driver {
    let path = shell_fixture(name);
    let loaded = dilla_parity::fixture::load(&path, Os::current()).unwrap();
    let mut driver = Driver::launch_fixture(&path, loaded.shot_path.as_deref(), env);
    driver.wait_idle();
    for step in &loaded.fixture.steps {
        dilla_parity::screen::replay(&mut driver, step);
    }
    dilla_parity::screen::rest_before_frame(&mut driver, &loaded.fixture);
    driver.wait_idle();
    driver
}

/// The glyph coverage of `shell/channel-view` drawn with `env`.
fn channel_view_glyphs(env: &[(&str, &str)]) -> GlyphCoverage {
    let mut driver = launch_shell("shell/channel-view", env);
    let shot = driver.screenshot();
    let lines = driver.text_bounds();
    let coverage = glyph_coverage(&shot, &lines, 2.0);
    println!("GLYPHS {env:?}: {coverage:?}");
    coverage
}

/// The bug of 2026-11-16: a macOS build without gpui-pre-platform's `font-kit` drew every line of text as nothing
/// (gpui-pre-macos falls back to `NoopTextSystem`). Every line box the app reports must hold ink.
/// Measured on `shell/channel-view` (GLYPHS line): Linux (lavapipe) 66 lines, 66 inked, ink 15.2 %; macOS (Metal,
/// macos-15) see TOLERANCES.md "Glyph presence". The thresholds (30 lines, 90 % inked) sit far from the Noop control's 0.
#[test]
#[cfg_attr(windows, ignore = "GPUI headless has no renderer on Windows")]
fn text_draws_glyphs() {
    let c = channel_view_glyphs(&[]);
    assert!(
        c.lines >= 30,
        "only {} text lines on screen: {c:?}",
        c.lines
    );
    assert!(
        c.inked_lines * 10 >= c.lines * 9,
        "{} of {} text lines hold no glyph (a text system that draws nothing?): {c:?}",
        c.lines - c.inked_lines,
        c.lines
    );
}

/// The glyph test's negative control: the same screen drawn with GPUI's `NoopTextSystem` (what a macOS build without
/// `font-kit` gets) has no inked line, so `text_draws_glyphs` would fail on it.
#[test]
#[cfg_attr(windows, ignore = "GPUI headless has no renderer on Windows")]
fn noop_text_system_draws_no_glyphs() {
    let c = channel_view_glyphs(&[("DILLA_AUTOMATION_TEXT_SYSTEM", "noop")]);
    assert_eq!(c.inked_lines, 0, "the Noop text system drew ink: {c:?}");
    assert!(
        c.inked_lines * 10 < c.lines * 9 || c.lines < 30,
        "text_draws_glyphs would pass on the Noop frame: {c:?}"
    );
}

fn intersects(a: &ElementInfo, b: &ElementInfo) -> bool {
    a.x < b.x + b.width && b.x < a.x + a.width && a.y < b.y + b.height && b.y < a.y + a.height
}

/// The tracked elements a shell fixture's title bar may draw under the traffic lights: the window's root, the bar
/// itself (the window's drag region) and the compact windows' drag strip.
const UNDER_THE_LIGHTS: [&str; 3] = ["root", "shell-topbar", "window-drag"];

/// What of `name`'s frame (drawn with `env`) lies in the traffic lights' rect: text lines and tracked elements.
fn under_the_lights(name: &str, env: &[(&str, &str)]) -> Vec<String> {
    let mut driver = launch_shell(name, env);
    let lights = driver.traffic_lights();
    let mut hits: Vec<String> = driver
        .text_bounds()
        .iter()
        .filter(|t| intersects(t, &lights))
        .map(|t| format!("text {} {} {}x{}", t.x, t.y, t.width, t.height))
        .collect();
    hits.extend(
        driver
            .elements()
            .iter()
            .filter(|e| !UNDER_THE_LIGHTS.contains(&e.id.as_str()) && intersects(e, &lights))
            .map(|e| format!("{} {} {} {}x{}", e.id, e.x, e.y, e.width, e.height)),
    );
    println!("UNDER THE LIGHTS {name} {env:?} ({lights:?}): {hits:?}");
    hits
}

/// The bug of 2026-11-16: the traffic lights sat over the top bar's brand. On macOS the shipping window's bar (a fixture
/// drawn with the window's controls) keeps every text line and every tracked element but the bar itself out of the
/// rect the lights are drawn in (`dilla_desktop::MAC_TRAFFIC_LIGHTS`, from the window options' `MAC_WINDOW_BUTTONS`).
#[test]
// Compiled on macOS only, not ignored elsewhere: the Linux shard's `cargo test -p dilla-parity -- --ignored` would run it there.
#[cfg(target_os = "macos")]
fn macos_header_clears_the_traffic_lights() {
    for name in ["shell/channel-view", "shell/channel-view-b"] {
        let hits = under_the_lights(name, &[("DILLA_AUTOMATION_WINDOW_CONTROLS", "1")]);
        assert!(
            hits.is_empty(),
            "{name}: drawn under the macOS traffic lights: {hits:?}"
        );
    }
}

/// The clearance test's negative control, on every OS with a renderer: a bar without the macOS inset (a fixture's web bar,
/// 14 px in) puts its brand under the lights, and the check sees it.
#[test]
#[cfg_attr(windows, ignore = "GPUI headless has no renderer on Windows")]
fn a_bar_without_the_inset_is_under_the_traffic_lights() {
    let hits = under_the_lights("shell/channel-view", &[]);
    assert!(
        hits.iter().any(|h| h.starts_with("topbar-brand ")),
        "the brand mark of a bar 14 px in must be under the lights: {hits:?}"
    );
}
