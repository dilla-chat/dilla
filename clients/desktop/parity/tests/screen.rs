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
    shoot_with(name, env, TEXT_FRINGE_PX)
}

/// [`shoot`] with the mask grown by `grow` device pixels.
fn shoot_with(name: &str, env: &[(&str, &str)], grow: f32) -> (RgbaImage, TextMask) {
    let loaded = fixture::load(&fixture_path(name), Os::Linux).unwrap();
    let mut driver = Driver::launch_fixture(&fixture_path(name), loaded.shot_path.as_deref(), env);
    driver.wait_idle();
    // The fixture's steps, as `assert_screen` replays them (the channel views close the member list the shell opens).
    for step in &loaded.fixture.steps {
        dilla_parity::screen::replay(&mut driver, step);
    }
    dilla_parity::screen::rest_before_frame(&mut driver, &loaded.fixture);
    driver.wait_idle();
    let shot = driver.screenshot();
    let mask = text_mask(&mut driver, grow);
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
    // The mask covers every glyph pixel. The same screen with every line painted transparent (`DILLA_AUTOMATION_HIDE_TEXT`)
    // has the same geometry and none of the glyphs: whatever differs between the two frames is exactly the pixels the
    // glyphs cover, and every one of them must lie in the mask. No exclusions: a glyph the lines do not report fails.
    let (hidden, _) = shoot("shell/channel-view", &[("DILLA_AUTOMATION_HIDE_TEXT", "1")]);
    let (mut glyph_pixels, mut outside) = (0u64, 0u64);
    for (x, y, p) in normal.enumerate_pixels() {
        if p != hidden.get_pixel(x, y) {
            glyph_pixels += 1;
            let covered = mask_normal
                .rects()
                .iter()
                .any(|r| x >= r.x && x < r.x + r.w && y >= r.y && y < r.y + r.h);
            outside += u64::from(!covered);
        }
    }
    assert!(
        glyph_pixels > 10_000,
        "the text is drawn ({glyph_pixels} glyph pixels)"
    );
    assert_eq!(
        outside, 0,
        "{outside} of {glyph_pixels} glyph pixels are outside the text mask"
    );
}

/// `TEXT_FRINGE_PX` is the measured reach of glyph pixels beyond the line boxes the app reports: the largest distance
/// (Chebyshev, device px) from a glyph pixel to the nearest ungrown line box, over both shell fixtures, must be within it.
#[test]
#[cfg_attr(not(target_os = "linux"), ignore = "references are Linux only")]
fn text_fringe_is_the_measured_reach_of_glyphs() {
    let mut reach = 0u32;
    for name in ["shell/channel-view", "shell/channel-view-b"] {
        let (normal, boxes) = shoot_with(name, &[], 0.0);
        let (hidden, _) = shoot_with(name, &[("DILLA_AUTOMATION_HIDE_TEXT", "1")], 0.0);
        for (x, y, p) in normal.enumerate_pixels() {
            if p == hidden.get_pixel(x, y) {
                continue;
            }
            let nearest = boxes
                .rects()
                .iter()
                .map(|r| {
                    let dx = r.x.saturating_sub(x).max(x.saturating_sub(r.x + r.w - 1));
                    let dy = r.y.saturating_sub(y).max(y.saturating_sub(r.y + r.h - 1));
                    dx.max(dy)
                })
                .min()
                .expect("line boxes");
            reach = reach.max(nearest);
        }
    }
    println!("FRINGE glyph pixels reach {reach} device px beyond their line boxes");
    assert!(
        reach as f32 <= TEXT_FRINGE_PX,
        "glyphs reach {reach} px beyond the line boxes: raise TEXT_FRINGE_PX (and document why)"
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

#[test]
#[cfg_attr(not(target_os = "linux"), ignore = "references are Linux only")]
fn a_missing_sidecar_is_a_reference_failure() {
    let dir = tempfile::tempdir().unwrap();
    let copy = dir.path().join("parity");
    for rel in ["fixtures/shell", "references/linux/shell"] {
        std::fs::create_dir_all(copy.join(rel)).unwrap();
    }
    std::fs::copy(
        fixture_path("shell/channel-view"),
        copy.join("fixtures/shell/channel-view.json"),
    )
    .unwrap();
    std::fs::copy(
        reference("shell/channel-view"),
        copy.join("references/linux/shell/channel-view-2x.png"),
    )
    .unwrap();
    match assert_screen(&copy.join("fixtures/shell/channel-view.json")) {
        Err(ParityFailure::Reference { reason, .. }) => {
            assert!(reason.contains("no sidecar"), "{reason}");
        }
        other => panic!("a reference with no sidecar must be refused, got {other:?}"),
    }
}

/// A copy of the channel-view fixture with `edit` applied to its JSON, written to a temp file.
fn variant(edit: impl FnOnce(&mut serde_json::Value)) -> (tempfile::TempDir, PathBuf) {
    let mut json: serde_json::Value =
        serde_json::from_str(&std::fs::read_to_string(fixture_path("shell/channel-view")).unwrap())
            .unwrap();
    edit(&mut json);
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("variant.json");
    std::fs::write(&path, json.to_string()).unwrap();
    (dir, path)
}

#[test]
fn the_welcome_route_draws_the_sign_up_first_step() {
    let (_dir, path) = variant(|j| {
        j["route"] = "/welcome".into();
        j["phase"] = "signed_out".into();
    });
    let mut app = Driver::try_launch_fixture(&path, None, &[]).expect("the app draws /welcome");
    // Task 28 lane F: /welcome is the sign-up's "Join with an invite", as on the web, not the sign-in form.
    app.wait_for("onboarding-invite", Duration::from_secs(30));
    assert!(app.element("signin-username").is_none(), "not the sign-in");
    assert!(app.element("composer").is_none(), "the shell is not drawn");
}

#[test]
fn a_route_the_app_cannot_draw_fails_the_launch() {
    for (route, phase) in [
        ("/settings", "ready"),
        ("/welcome", "ready"),
        ("/activity", "ready"),
    ] {
        let (_dir, path) = variant(|j| {
            j["route"] = route.into();
            j["phase"] = phase.into();
        });
        match Driver::try_launch_fixture(&path, None, &[]) {
            Err(why) => assert!(why.contains("cannot draw"), "{route}: {why}"),
            Ok(_) => panic!("{route} in phase {phase} must not draw anything"),
        }
    }
}

#[test]
fn capture_leaves_no_hover_and_no_focus() {
    // A Click step focuses the composer; the web shooter then moves the pointer to the corner and blurs the focus, and
    // the GPUI side does the same before its frame.
    let (_dir, path) = variant(|j| {
        j["steps"] =
            serde_json::json!([{"do": "click", "web": "css=textarea", "gpui": "composer"}]);
    });
    let fixture = fixture::load_unchecked(&path).unwrap();
    let mut app = Driver::launch_fixture(&path, None, &[]);
    app.wait_idle();
    for step in &fixture.steps {
        dilla_parity::screen::replay(&mut app, step);
    }
    assert!(app.focused().is_some(), "the click focused the composer");
    assert_ne!(app.pointer(), (1278.0, 798.0));
    dilla_parity::screen::rest_before_frame(&mut app, &fixture);
    assert_eq!(app.focused(), None, "no focus");
    assert_eq!(
        app.pointer(),
        (1278.0, 798.0),
        "the pointer is off the content"
    );
    // With a hover step the pointer stays where the step put it.
    let (_dir, path) = variant(|j| {
        j["steps"] =
            serde_json::json!([{"do": "hover", "web": "css=textarea", "gpui": "composer"}]);
    });
    let fixture = fixture::load_unchecked(&path).unwrap();
    let mut app = Driver::launch_fixture(&path, None, &[]);
    app.wait_idle();
    for step in &fixture.steps {
        dilla_parity::screen::replay(&mut app, step);
    }
    let before = app.pointer();
    dilla_parity::screen::rest_before_frame(&mut app, &fixture);
    assert_eq!(app.pointer(), before, "a hover step keeps the pointer");
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
    // Chords the app binds to no action: an app-wide binding (Ctrl+K opens the search, Ctrl+Shift+M mutes) takes its
    // key before any key-down listener, as Tab does below, so the harness proves delivery with free chords.
    app.press("ctrl-j");
    app.press("shift-tab");
    assert_eq!(
        app.focused().as_deref(),
        Some("field-a"),
        "Shift-Tab moves it back"
    );
    app.press("ctrl-shift-y");
    assert_eq!(
        app.harness_log(),
        [
            // Tab and Shift-Tab are bound to the focus actions (`screens::FocusNext`), which take the key before a
            // key-down listener sees it: they move the focus and log nothing.
            "field-a:enter",
            "field-b:escape",
            "field-b:ctrl-j",
            "field-a:ctrl-shift-y",
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
#[cfg_attr(
    target_os = "windows",
    ignore = "screenshots need a headless renderer: Linux (wgpu on lavapipe) and macOS have one, Windows' DirectX renderer none yet"
)]
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
    // The baseline's total is pinned here: raising it takes a reviewed edit of this test, not of the data file.
    let total: usize = baseline.values().sum();
    assert!(
        total <= BASELINE_TOTAL_MAX,
        "screens-px-baseline.txt holds {total} literals: more than the {BASELINE_TOTAL_MAX} it may"
    );
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
        match baseline.get(&rel).copied() {
            None if found > 0 => offenders.push(format!(
                "{rel}: {found} literal px(..)/point(..); a file with no baseline line holds none"
            )),
            Some(allowed) if found > allowed => offenders.push(format!(
                "{rel}: {found} literal px(..)/point(..), allowed {allowed}"
            )),
            // The ratchet: a count that fell must be written down, so it can never rise back.
            Some(allowed) if found < allowed => offenders.push(format!(
                "{rel}: only {found} literals left: lower its line in screens-px-baseline.txt from {allowed} to {found}{}",
                if found == 0 { " (delete the line)" } else { "" }
            )),
            _ => {}
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

/// The most literals the baseline may hold in total (what the Task 12 slice had when Task 5 landed).
const BASELINE_TOTAL_MAX: usize = 114;

// ---- Step 6: both shooters read the same fixture ----------------------------------------------------------------

#[test]
#[cfg_attr(
    not(target_os = "linux"),
    ignore = "the fixtures load as Linux (Os::Linux) and the references are Linux only"
)]
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
        let loaded = fixture::load(&path, Os::Linux).unwrap();
        let mut app = Driver::launch_fixture(&path, loaded.shot_path.as_deref(), &[]);
        let gpui = app.fixture_line().expect("a fixture launch answers");
        assert_eq!(web, gpui, "{name}: the shooters log different fixtures");
        // The line carries the file's digest and every world id, the sidecar's where it has them.
        let ids = loaded.fixture.effective_ids(loaded.shot.as_ref()).unwrap();
        assert_eq!(gpui, loaded.fixture.fixture_line_with(&loaded.digest, &ids));
        let real = loaded.shot.as_ref().unwrap().ids["person:ada"].clone();
        assert!(
            gpui.contains(&format!("person:ada={real}")),
            "the sidecar's ids are the logged ones"
        );
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
