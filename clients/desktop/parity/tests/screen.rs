//! Task 5: the screen harness against the app's own screens and the harness view.
//!
//! The channel-view tests need Linux (the references are shot there; macOS and Windows tolerances are PENDING-CI).
//! Everything that drives the app needs GPUI's headless renderer, which Windows lacks.

use std::path::{Path, PathBuf};
use std::process::Command;
use std::time::Duration;

use dilla_parity::fixture;
use dilla_parity::screen::{TEXT_FRINGE_PX, assert_screen, assert_screen_with, text_mask};
use dilla_parity::{Driver, NoReply, Os, ParityFailure, RgbaImage, TextMask, assert_parity_masked};

fn parity() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).to_path_buf()
}

fn fixture_path(name: &str) -> PathBuf {
    parity().join("fixtures").join(format!("{name}.json"))
}

fn reference(name: &str) -> PathBuf {
    parity()
        .join("references/linux")
        .join(format!("{name}-2x.png"))
}

/// The GPUI frame of a committed fixture and its R13 mask.
fn shoot(name: &str, env: &[(&str, &str)]) -> (RgbaImage, TextMask) {
    let loaded = fixture::load(&fixture_path(name), Os::Linux).unwrap();
    let mut driver = Driver::launch_fixture(&fixture_path(name), loaded.shot_path.as_deref(), env);
    driver.wait_idle();
    let shot = driver.screenshot();
    let mask = text_mask(&mut driver, TEXT_FRINGE_PX);
    (shot, mask)
}

// ---- Step 1, 2, 3: the screens ----------------------------------------------------------------------------------

#[test]
#[cfg_attr(not(target_os = "linux"), ignore = "references are Linux only")]
fn text_mask_matches_painted_lines() {
    let (normal, mask_normal) = shoot("shell/channel-view", &[]);
    let (width, height) = normal.dimensions();
    // Every rect lies inside the window.
    assert!(
        mask_normal.rects().len() >= 30,
        "the app reports its text lines"
    );
    for r in mask_normal.rects() {
        assert!(
            r.x + r.w <= width && r.y + r.h <= height,
            "{r:?} is outside {width}x{height}"
        );
    }
    // The mask covers every glyph pixel. The same screen with every line in a face the app does not embed draws other
    // glyphs: whatever differs between the two frames is glyph pixels of one or the other, so it must all lie in the
    // union of the two frames' masks, and no pixel outside them may differ.
    let (fallback, mask_fallback) = shoot(
        "shell/channel-view",
        &[("DILLA_AUTOMATION_FALLBACK_FONT", "1")],
    );
    let inside = |x: u32, y: u32| {
        mask_normal
            .rects()
            .iter()
            .chain(mask_fallback.rects())
            .any(|r| x >= r.x && x < r.x + r.w && y >= r.y && y < r.y + r.h)
    };
    let (mut glyph_pixels, mut outside) = (0u64, 0u64);
    for (x, y, p) in normal.enumerate_pixels() {
        if p != fallback.get_pixel(x, y) {
            glyph_pixels += 1;
            if !inside(x, y) {
                outside += 1;
            }
        }
    }
    assert!(
        glyph_pixels > 10_000,
        "the two faces differ ({glyph_pixels} pixels)"
    );
    // Measured on the committed fixture: 475 of 106774 (0.44 %) differ outside the masks. They are not glyphs the lines
    // report: avatar initials and the accent `#` glyph (drawn without a text probe: Task 6's avatar and icon components
    // should report them), and the timeline scrollbar, which the fallback face's different text height moves. A mask
    // that missed whole lines would miss far more than 1 %.
    println!("MASK {outside} of {glyph_pixels} differing pixels are outside the text masks");
    assert!(
        outside * 100 <= glyph_pixels,
        "{outside} of {glyph_pixels} differing pixels are outside the text masks"
    );
}

#[test]
#[cfg_attr(not(target_os = "linux"), ignore = "references are Linux only")]
fn channel_view_two_fixtures() {
    for name in ["shell/channel-view", "shell/channel-view-b"] {
        let report = assert_screen(&fixture_path(name))
            .unwrap_or_else(|e| panic!("{name} differs from its web reference: {e}"));
        println!(
            "SCREEN {name}: {} text lines, chrome {:.4} %",
            report.text_lines, report.chrome_pct
        );
        assert!(
            report.text_lines >= 30,
            "{name}: {} text lines",
            report.text_lines
        );
    }
}

#[test]
#[cfg_attr(not(target_os = "linux"), ignore = "references are Linux only")]
fn a_reference_shot_from_another_fixture_is_refused() {
    // The sidecar records the digest of the fixture file the shot was made from.
    let dir = tempfile::tempdir().unwrap();
    let copy = dir.path().join("parity");
    for rel in ["fixtures/shell", "references/linux/shell"] {
        std::fs::create_dir_all(copy.join(rel)).unwrap();
    }
    let text = std::fs::read_to_string(fixture_path("shell/channel-view")).unwrap();
    std::fs::write(
        copy.join("fixtures/shell/channel-view.json"),
        text.replace("morning", "evening"),
    )
    .unwrap();
    std::fs::copy(
        reference("shell/channel-view"),
        copy.join("references/linux/shell/channel-view-2x.png"),
    )
    .unwrap();
    std::fs::copy(
        parity().join("references/linux/shell/channel-view.shot.json"),
        copy.join("references/linux/shell/channel-view.shot.json"),
    )
    .unwrap();
    match assert_screen(&copy.join("fixtures/shell/channel-view.json")) {
        Err(ParityFailure::Reference { reason, .. }) => {
            assert!(
                reason.contains("another version of this fixture"),
                "{reason}"
            );
        }
        other => panic!("expected a stale reference, got {other:?}"),
    }
}

/// The colour of a design token as 8-bit sRGB (`tokens.rs`, hsl converted): `BG_2`, `BG_3` and the two hairlines.
const BG_2: [u8; 3] = [11, 13, 16];
const BG_3: [u8; 3] = [13, 16, 20];
const HAIRLINE: [u8; 3] = [26, 29, 35];
const HAIRLINE_2: [u8; 3] = [35, 40, 49];

fn recolour(img: &RgbaImage, from: [u8; 3], to: [u8; 3]) -> (RgbaImage, u64) {
    let mut out = img.clone();
    let mut changed = 0;
    for (x, y, p) in img.enumerate_pixels() {
        if p.0[..3] == from {
            out.put_pixel(x, y, image::Rgba([to[0], to[1], to[2], 255]));
            changed += 1;
        }
    }
    (out, changed)
}

#[test]
#[cfg_attr(not(target_os = "linux"), ignore = "references are Linux only")]
fn wrong_fill_colour_fails() {
    let (shot, mask) = shoot("shell/channel-view", &[]);
    let reference = reference("shell/channel-view");
    // Positive control: the honest frame passes.
    assert_parity_masked(&shot, &reference, Os::Linux, &mask).unwrap();
    // The panel surface one token step off (BG_2 to BG_3): far under the chrome gate's delta, so only the colour gate sees it.
    let (wrong, changed) = recolour(&shot, BG_2, BG_3);
    assert!(
        changed > 500_000,
        "the frame has no BG_2 surface to recolour ({changed} pixels)"
    );
    match assert_parity_masked(&wrong, &reference, Os::Linux, &mask) {
        Err(ParityFailure::Colour { pct, max_pct, .. }) => assert!(pct > max_pct * 10.0, "{pct}"),
        other => panic!("a fill one token step off must fail the colour gate, got {other:?}"),
    }
}

#[test]
#[cfg_attr(not(target_os = "linux"), ignore = "references are Linux only")]
fn wrong_border_colour_fails() {
    let (shot, mask) = shoot("shell/channel-view", &[]);
    let reference = reference("shell/channel-view");
    assert_parity_masked(&shot, &reference, Os::Linux, &mask).unwrap();
    // Every hairline one token step off (HAIRLINE to HAIRLINE_2).
    let (wrong, changed) = recolour(&shot, HAIRLINE, HAIRLINE_2);
    assert!(
        changed > 5_000,
        "the frame has no hairlines to recolour ({changed} pixels)"
    );
    match assert_parity_masked(&wrong, &reference, Os::Linux, &mask) {
        Err(ParityFailure::Colour { pct, max_pct, .. }) => assert!(pct > max_pct, "{pct}"),
        other => panic!("a border one token step off must fail the colour gate, got {other:?}"),
    }
}

#[test]
#[cfg_attr(not(target_os = "linux"), ignore = "references are Linux only")]
fn fallback_font_fails() {
    let result = assert_screen_with(
        &fixture_path("shell/channel-view"),
        &[("DILLA_AUTOMATION_FALLBACK_FONT", "1")],
    );
    assert!(
        matches!(result, Err(ParityFailure::Pixels { .. })),
        "a fallback face must fail the whole-frame gate, got {result:?}"
    );
}

// ---- Step 4: named keys, focus, hover ----------------------------------------------------------------------------

#[test]
fn named_keys_reach_focused_element() {
    let mut app = Driver::launch_harness();
    app.click("field-a");
    app.press("enter");
    app.press("tab");
    assert_eq!(
        app.focused().as_deref(),
        Some("field-b"),
        "Tab moves the focus on"
    );
    app.press("escape");
    app.press("ctrl-k");
    app.press("shift-tab");
    assert_eq!(
        app.focused().as_deref(),
        Some("field-a"),
        "Shift-Tab moves it back"
    );
    app.press("ctrl-shift-m");
    assert_eq!(
        app.harness_log(),
        [
            // Tab and Shift-Tab are bound to the focus actions (`screens::FocusNext`), which take the key before a
            // key-down listener sees it: they move the focus and log nothing.
            "field-a:enter",
            "field-b:escape",
            "field-b:ctrl-k",
            "field-a:ctrl-shift-m",
        ]
    );
}

#[test]
#[should_panic(expected = "not a key chord")]
fn a_chord_outside_the_grammar_is_refused() {
    let mut app = Driver::launch_harness();
    app.press("meta-k");
}

#[test]
fn focused_reports_id() {
    let mut app = Driver::launch_harness();
    assert_eq!(app.focused(), None, "nothing holds the focus at the start");
    app.click("field-b");
    assert_eq!(app.focused().as_deref(), Some("field-b"));
    app.click("field-a");
    assert_eq!(app.focused().as_deref(), Some("field-a"));
}

#[test]
fn hover_changes_state() {
    let mut app = Driver::launch_harness();
    let box_ = app.element("hover-box").expect("the hover box is drawn");
    let centre = |app: &mut Driver| {
        let shot = app.screenshot();
        *shot.get_pixel(
            ((box_.x + box_.width / 2.0) * 2.0) as u32,
            ((box_.y + box_.height / 2.0) * 2.0) as u32,
        )
    };
    let before = centre(&mut app);
    app.hover("hover-box");
    assert_eq!(app.harness_log(), ["hover-box:enter"]);
    let during = centre(&mut app);
    assert_ne!(before, during, "the box fills with the hover colour");
    app.hover("field-a");
    assert_eq!(app.harness_log(), ["hover-box:enter", "hover-box:leave"]);
    assert_eq!(centre(&mut app), before, "and gives it back");
}

#[test]
fn scroll_moves_the_scroll_box() {
    let mut app = Driver::launch_harness();
    app.scroll("scroll-box", -48.0);
    let log = app.harness_log();
    assert_eq!(log.len(), 1, "{log:?}");
    assert!(
        log[0].starts_with("scroll-box:-") && log[0] != "scroll-box:-0",
        "the box scrolled: {log:?}"
    );
}

#[test]
fn wait_idle_settles() {
    let mut app = Driver::launch_harness();
    let frames = app.wait_idle();
    assert!((2..60).contains(&frames), "{frames} frames");
}

// ---- Driver: the timeout path -----------------------------------------------------------------------------------

#[test]
fn a_slow_reply_times_out_and_kills_the_app() {
    let mut app = Driver::launch_harness();
    // `wait-idle` draws at least two frames with a pause between: far longer than a microsecond.
    let result = app.request_within("wait-idle", Duration::from_micros(1));
    assert_eq!(result, Err(NoReply::Timeout(Duration::from_micros(1))));
    assert!(
        app.has_exited(),
        "the driver kills an app that stopped answering"
    );
}

#[test]
fn target_dir_is_cargos() {
    let dir = dilla_parity::target_dir();
    assert!(dir.is_absolute() && dir.exists(), "{}", dir.display());
    if let Some(from_env) = std::env::var_os("CARGO_TARGET_DIR") {
        assert_eq!(
            dir,
            PathBuf::from(from_env)
                .canonicalize()
                .unwrap_or(dir.clone())
        );
    }
}

// ---- Step 5: the R21 tripwire -----------------------------------------------------------------------------------

/// Literal pixel offsets in a Rust source: `px(<number>)` and `point(<number>, ...)`. Comments are not code. A
/// `tokens::*` constant and an expression are not literals.
fn literal_offsets(source: &str) -> usize {
    let mut count = 0;
    for line in source.lines() {
        let code = line.split("//").next().unwrap_or("");
        for needle in ["px(", "point("] {
            let mut rest = code;
            while let Some(at) = rest.find(needle) {
                let before_ok = at == 0
                    || !rest[..at]
                        .chars()
                        .next_back()
                        .is_some_and(|c| c.is_alphanumeric() || c == '_');
                let after = rest[at + needle.len()..].trim_start();
                let after = after.strip_prefix('-').unwrap_or(after).trim_start();
                if before_ok && after.starts_with(|c: char| c.is_ascii_digit()) {
                    count += 1;
                }
                rest = &rest[at + needle.len()..];
            }
        }
    }
    count
}

fn rust_files(dir: &Path, out: &mut Vec<PathBuf>) {
    for entry in std::fs::read_dir(dir).unwrap() {
        let path = entry.unwrap().path();
        if path.is_dir() {
            rust_files(&path, out);
        } else if path.extension().is_some_and(|e| e == "rs") {
            out.push(path);
        }
    }
}

#[test]
fn the_scan_finds_literal_offsets() {
    // Positive control for the tripwire below.
    assert_eq!(literal_offsets("div().w(px(12.)).h(px(-3.5))"), 2);
    assert_eq!(literal_offsets("window.set(point(0., 8.))"), 1);
    assert_eq!(literal_offsets("gpui::point(px(0.), px(-(h - v)))"), 1);
    assert_eq!(literal_offsets("div().w(tokens::space::MD).h(px(h))"), 0);
    assert_eq!(literal_offsets("// px(12.) in a comment"), 0);
    assert_eq!(literal_offsets("let t = tpx(3.);"), 0);
}

/// R21: screens paint from their layout boxes. A source scan of `dilla-desktop/src/screens/**` fails on `px(<literal>)` and
/// `point(<literal>, ...)`: sizes come from `tokens::*`, positions from layout (and from the one documented sub-pixel
/// rule in `dilla-desktop-ui/src/text.rs`).
///
/// The code that exists today (the Task 12 slice) still has literals: `screens-px-baseline.txt` (`<file> <count>`) lists
/// them. The baseline only shrinks: a file may hold no more than its line, and a file with no line holds none. Nobody
/// regenerates it to make a screen pass.
#[test]
fn no_window_coordinates_in_screens() {
    let screens = parity().join("../dilla-desktop/src/screens");
    let baseline: std::collections::BTreeMap<String, usize> =
        std::fs::read_to_string(parity().join("screens-px-baseline.txt"))
            .unwrap()
            .lines()
            .filter(|l| !l.trim().is_empty() && !l.starts_with('#'))
            .map(|l| {
                let (file, n) = l.rsplit_once(' ').expect("`<file> <count>`");
                (file.to_owned(), n.parse().expect("a count"))
            })
            .collect();
    let mut files = Vec::new();
    rust_files(&screens, &mut files);
    let mut offenders = Vec::new();
    let mut seen = std::collections::BTreeSet::new();
    for file in files {
        let rel = file
            .strip_prefix(&screens)
            .unwrap()
            .to_string_lossy()
            .replace('\\', "/");
        let found = literal_offsets(&std::fs::read_to_string(&file).unwrap());
        let allowed = baseline.get(&rel).copied().unwrap_or(0);
        if found > allowed {
            offenders.push(format!(
                "{rel}: {found} literal px(..)/point(..), allowed {allowed}"
            ));
        }
        seen.insert(rel);
    }
    for file in baseline.keys() {
        assert!(
            seen.contains(file),
            "screens-px-baseline.txt names {file}, which no longer exists: delete its line"
        );
    }
    assert!(
        offenders.is_empty(),
        "literal pixel offsets in screens (R21: sizes come from tokens::*, positions from layout):\n{}",
        offenders.join("\n")
    );
}

// ---- Step 6: both shooters read the same fixture ----------------------------------------------------------------

#[test]
fn shooters_agree_on_fixture() {
    for name in ["shell/channel-view", "shell/channel-view-b"] {
        let path = fixture_path(name);
        let web = Command::new("node")
            .arg(parity().join("shoot.mjs"))
            .arg(&path)
            .arg("--check")
            .output()
            .expect("node runs the web shooter");
        assert!(
            web.status.success(),
            "shoot.mjs --check failed: {}",
            String::from_utf8_lossy(&web.stderr)
        );
        let web = String::from_utf8(web.stdout).unwrap().trim().to_owned();
        let mut app = Driver::launch_fixture(&path, None, &[]);
        let gpui = app.fixture_line().expect("a fixture launch answers");
        assert_eq!(web, gpui, "{name}: the shooters log different fixtures");
        // The line carries the file's digest and every world id.
        let loaded = fixture::load(&path, Os::Linux).unwrap();
        assert_eq!(gpui, loaded.fixture.fixture_line(&loaded.digest));
        assert!(gpui.contains(&format!(" {} ", loaded.digest)));
        assert!(gpui.contains("person:ada="));
    }
}

// ---- Step 7: the scripts ----------------------------------------------------------------------------------------

#[test]
#[cfg(unix)]
fn validate_tolerance_reports_a_crash_as_a_failure() {
    let dir = tempfile::tempdir().unwrap();
    let proof = dir.path();
    std::fs::copy(
        parity().join("../../../experiments/gpui-pixel-proof/validate-tolerance.sh"),
        proof.join("validate-tolerance.sh"),
    )
    .unwrap();
    std::fs::create_dir_all(proof.join("out/linux")).unwrap();
    std::fs::create_dir_all(proof.join("target/release")).unwrap();
    std::fs::write(proof.join("out/linux/linux-beta5-nohint-2x.png"), b"").unwrap();
    // A checker that renders fine and then crashes: it prints nothing and dies on a signal.
    std::fs::write(
        proof.join("target/release/gpui-pixel-proof"),
        "#!/usr/bin/env bash\n[ \"$1\" = check ] && kill -SEGV $$\nexit 0\n",
    )
    .unwrap();
    use std::os::unix::fs::PermissionsExt;
    std::fs::set_permissions(
        proof.join("target/release/gpui-pixel-proof"),
        std::fs::Permissions::from_mode(0o755),
    )
    .unwrap();
    let out = Command::new("bash")
        .arg(proof.join("validate-tolerance.sh"))
        .args(["linux", "16", "0.2"])
        .output()
        .unwrap();
    let text = String::from_utf8_lossy(&out.stdout);
    assert!(
        !out.status.success(),
        "a crashing checker must not validate:\n{text}"
    );
    assert!(text.contains("CRASH"), "{text}");
    assert!(
        !text.contains("[ok]"),
        "no variant may pass on a crash:\n{text}"
    );
}
