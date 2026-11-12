use std::path::{Path, PathBuf};
use std::process::Command;

use dilla_parity::{
    Driver, Os, ParityFailure, RgbaImage, Tolerance, assert_parity, assert_parity_with, target_dir,
    tolerance_for,
};
use image::Rgba;

const EXACT: Tolerance = Tolerance {
    max_delta: 0,
    max_pct: 0.0,
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
    for needle in [
        "dilla-automation-interface",
        "dilla_desktop::automation",
        "dilla_desktop10automation",
    ] {
        assert!(
            !contains(&binary, needle.as_bytes()),
            "release binary contains `{needle}`"
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
fn tolerance_file_ships_exact_placeholders() {
    for os in [Os::Linux, Os::MacOs, Os::Windows] {
        assert_eq!(tolerance_for(os).unwrap(), EXACT);
    }
    let dir = tempfile::tempdir().unwrap();
    let image = grey(8, 8, 9);
    assert_parity(&image, &reference_file(dir.path(), &image), Os::current()).unwrap();
}

#[test]
fn click_and_read_element() {
    let fixture = tempfile::tempdir().unwrap();
    let mut driver = Driver::launch(fixture.path());

    let root = driver.element("root").expect("the root element exists");
    assert_eq!(root.id, "root");
    assert_eq!((root.width, root.height), (1280.0, 800.0));
    assert_eq!((root.x, root.y), (0.0, 0.0));
    assert!(driver.element("no-such-element").is_none());

    driver.click("root");
    driver.type_text("root", "héllo");
    assert!(
        driver.element("root").is_some(),
        "the window survives input"
    );

    let shot = driver.screenshot();
    // The headless platform renders at scale 2.0, so the 1280x800 window is 2560x1600 device pixels.
    assert_eq!(shot.dimensions(), (2560, 1600));
}
