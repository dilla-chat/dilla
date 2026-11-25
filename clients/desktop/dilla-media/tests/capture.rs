//! Parity plan Package 1: a share's source through the real media path (beta.5 `screen-capture.spec.ts`).
//!
//! - A window share (a screen-slot source with no sound) reaches a viewer through the real SFU, sealed under the
//!   sharer's key and opened as the sharer's verified screen, and nothing else of it travels.
//! - A source that closes ends its track: the viewer's stream goes, the sharer is told.
//! - An unbound screen sender sends nothing while its input stays live (synthetic frames, no SFU).
//!
//! The live tests need dilla-testhost and the WASI core, as tests/live.rs:
//! `cargo test -p dilla-media --test capture -- --ignored`.

#[allow(dead_code, unused_imports, clippy::all)]
#[path = "../../../../core/dilla-core/tests/client_support/mod.rs"]
mod client_support;

mod support {
    pub mod live;
}

use std::time::{Duration, Instant};

use client_support::{CALL, call_raw_bob};
use dilla_core::sframe::{NK, Slot};
use dilla_media::transform::{CallMedia, Router};
use serde_json::Value;
use support::live::*;
use webrtc_sys::dilla_transform_testing as testing;

/// Waits until `peer`'s `videos` holds `key` with frames that are the test pattern, and returns that stream.
fn sees(peer: &mut Peer, key: &str, timeout: Duration) -> Value {
    let deadline = Instant::now() + timeout;
    loop {
        peer.send("videos");
        let v = peer.next("videos", Duration::from_secs(5));
        let stream = &v["videos"][key];
        if stream["frames"].as_u64().is_some_and(|n| n > 5) && stream["bars"] == true {
            return v;
        }
        assert!(Instant::now() < deadline, "no {key} stream: {v}");
        std::thread::sleep(Duration::from_millis(250));
    }
}

/// screen-capture.spec.ts "a selected native application window stays video-only and encrypted through the real media
/// worker": alice shares a window (a screen-slot source that has no sound). Bob sees exactly one stream of hers, her
/// screen, as the pattern she sent, every frame of it opened under her key id with nothing refused; alice's sender sealed
/// what went out, and it went out as VP8.
#[test]
#[ignore = "needs dilla-testhost (Go) and the WASI core"]
fn a_window_source_is_video_only_and_encrypted() {
    let host = start_host();
    let (_call, alice, bob, _) = call_raw_bob();
    let (a_dev, b_dev) = (hex(&alice.device), hex(&bob.device));
    let room = hex(&CALL);
    let (url, a_token) = token(&host.control, &room, &a_dev);
    let (_, b_token) = token(&host.control, &room, &b_dev);
    let mut a = Peer::start(&url, &a_token, 0);
    let mut b = Peer::start(&url, &b_token, 0);
    let e = alice_epoch(&alice);
    a.epoch(&e);
    b.epoch(&bob_epoch(&bob));
    a.next("connected", Duration::from_secs(30));
    b.next("connected", Duration::from_secs(30));

    a.send("share window");
    assert_eq!(
        a.next("share", Duration::from_secs(15))["error"],
        Value::Null
    );
    let seen = sees(&mut b, &format!("{a_dev}:Screen"), Duration::from_secs(30));
    let streams = seen["videos"].as_object().unwrap();
    assert_eq!(
        streams.keys().collect::<Vec<_>>(),
        vec![&format!("{a_dev}:Screen")],
        "one stream: the window, in the screen slot ({seen})"
    );
    let a_kid = format!("{:x}", (e.own_leaf << 8) | (e.epoch & 0xff));
    let (sa, sb) = (a.stats(), b.stats());
    assert!(
        sa["media"]["encrypted"][&a_kid].as_u64().unwrap_or(0) > 5,
        "{sa}"
    );
    assert!(opened(&sb, &a_kid) > 5, "{sb}");
    assert_eq!(failed(&sb), 0, "{sb}");
    assert_eq!(failed(&sa), 0, "{sa}");
    // Video only: one outbound video stream (the window), as VP8; a window source carries no sound of its own.
    a.send("sent");
    let sent = a.next("sent", Duration::from_secs(5));
    let video = sent["video"].as_array().unwrap();
    assert_eq!(video.len(), 1, "{sent}");
    assert_eq!(video[0][0], "video/VP8", "{sent}");
    assert!(video[0][1].as_u64().unwrap() > 0, "{sent}");
}

/// macOS CI: a real screen share from one macOS client to another. Alice shares the main display, captured through
/// ScreenCaptureKit (call-peer `share display`: its picker off, needs Screen Recording, which the runner grants the
/// shell); bob sees her screen stream move, opened under her key, with nothing refused.
#[cfg(target_os = "macos")]
#[test]
#[ignore = "needs dilla-testhost (Go) and the WASI core"]
fn a_real_display_share_reaches_a_viewer() {
    let host = start_host();
    let (_call, alice, bob, _) = call_raw_bob();
    let (a_dev, b_dev) = (hex(&alice.device), hex(&bob.device));
    let room = hex(&CALL);
    let (url, a_token) = token(&host.control, &room, &a_dev);
    let (_, b_token) = token(&host.control, &room, &b_dev);
    let mut a = Peer::start(&url, &a_token, 0);
    let mut b = Peer::start(&url, &b_token, 0);
    let e = alice_epoch(&alice);
    a.epoch(&e);
    b.epoch(&bob_epoch(&bob));
    a.next("connected", Duration::from_secs(30));
    b.next("connected", Duration::from_secs(30));

    a.send("share display");
    let shared = a.next("share", Duration::from_secs(30));
    assert_eq!(shared["error"], Value::Null, "the main display: {shared}");
    let key = format!("{a_dev}:Screen");
    let deadline = Instant::now() + Duration::from_secs(30);
    let seen = loop {
        b.send("videos");
        let v = b.next("videos", Duration::from_secs(5));
        if v["videos"][&key]["frames"].as_u64().is_some_and(|n| n > 5) {
            break v;
        }
        assert!(Instant::now() < deadline, "no {key} stream: {v}");
        std::thread::sleep(Duration::from_millis(250));
    };
    let streams = seen["videos"].as_object().unwrap();
    assert_eq!(streams.keys().collect::<Vec<_>>(), vec![&key], "{seen}");
    let a_kid = format!("{:x}", (e.own_leaf << 8) | (e.epoch & 0xff));
    let (sa, sb) = (a.stats(), b.stats());
    assert!(
        sa["media"]["encrypted"][&a_kid].as_u64().unwrap_or(0) > 5,
        "{sa}"
    );
    assert!(opened(&sb, &a_kid) > 5, "{sb}");
    assert_eq!(failed(&sb), 0, "{sb}");
}

/// screen-capture.spec.ts "closing the selected source ends its consumer track and destroys the broker": alice's
/// source closes after 45 frames (3 s at 15 fps). Her call says the share ended, and bob's stream of it goes.
#[test]
#[ignore = "needs dilla-testhost (Go) and the WASI core"]
fn a_closed_source_ends_the_track() {
    let host = start_host();
    let (_call, alice, bob, _) = call_raw_bob();
    let (a_dev, b_dev) = (hex(&alice.device), hex(&bob.device));
    let room = hex(&CALL);
    let (url, a_token) = token(&host.control, &room, &a_dev);
    let (_, b_token) = token(&host.control, &room, &b_dev);
    let mut a = Peer::start(&url, &a_token, 0);
    let mut b = Peer::start(&url, &b_token, 0);
    a.epoch(&alice_epoch(&alice));
    b.epoch(&bob_epoch(&bob));
    a.next("connected", Duration::from_secs(30));
    b.next("connected", Duration::from_secs(30));

    a.send("share screen 45");
    assert_eq!(
        a.next("share", Duration::from_secs(15))["error"],
        Value::Null
    );
    let key = format!("{a_dev}:Screen");
    sees(&mut b, &key, Duration::from_secs(30));
    let deadline = Instant::now() + Duration::from_secs(20);
    loop {
        a.send("ended");
        let ended = a.next("shares_ended", Duration::from_secs(5));
        if ended["kinds"].as_array().is_some_and(|k| !k.is_empty()) {
            assert_eq!(ended["kinds"], serde_json::json!(["Screen"]));
            break;
        }
        assert!(Instant::now() < deadline, "the share never ended");
        std::thread::sleep(Duration::from_millis(250));
    }
    let deadline = Instant::now() + Duration::from_secs(20);
    loop {
        b.send("videos");
        let v = b.next("videos", Duration::from_secs(5));
        if v["videos"][&key].is_null() {
            break;
        }
        assert!(Instant::now() < deadline, "the viewer's stream stayed: {v}");
        std::thread::sleep(Duration::from_millis(250));
    }
    // Nothing is sent after the end (the statistics keep the stopped sender's total: it no longer grows).
    let mut total = || {
        a.send("sent");
        let sent = a.next("sent", Duration::from_secs(5));
        sent["video"]
            .as_array()
            .map_or(0, |v| v.iter().filter_map(|s| s[1].as_u64()).sum::<u64>())
    };
    let before = total();
    std::thread::sleep(Duration::from_secs(2));
    assert_eq!(total(), before, "nothing is sent after the end");
}

/// screen-capture.spec.ts "an unbound native screen source is blocked while its real input remains live": a screen
/// sender that is not bound to this call's screen slot (its track was never announced) drops every frame its live input
/// gives it; none leaves, sealed or not.
#[test]
fn an_unbound_screen_source_sends_nothing() {
    let router = Router::get().expect("hook");
    let media = CallMedia::new();
    router.begin(media.clone()).expect("no other call");
    media
        .install_epoch(3, &[0x44; NK], &[(0, [0xaa; 16]), (1, [0xbb; 16])], 0)
        .expect("keys");
    // The call expects its own screen track; this sender is another one.
    media.expect_local("screen-announced", Slot::ScreenVideo);
    let tx = testing::attach(true, true, "screen-unannounced");
    let vp8: &[u8] = &[
        0x50, 0x02, 0x00, 0x9d, 0x01, 0x2a, 0x80, 0x02, 0xe0, 0x01, 0xaa,
    ];
    for n in 0..30 {
        testing::frame(tx, "video/VP8", vp8, n % 10 == 0);
    }
    assert_eq!(testing::emitted_count(tx), 0, "nothing left");
    assert!(testing::take_emitted(tx).is_empty());
    assert_eq!(media.stats().failed.get("unbound"), Some(&30));
    assert!(media.stats().encrypted.is_empty());
    testing::release(tx);
    router.end(&media);
}

/// GPUI gaps Task 6: the macOS camera through AVFoundation (webrtc-sys `dilla_camera_mac.mm`). It opens a real camera,
/// so it runs only where a person granted the shell Camera access once: `DILLA_MAC_CAPTURE=1 cargo test -p dilla-media
/// --test capture mac_camera`. The built-in camera when there is one (a Continuity Camera iPhone is listed too), else
/// the first; at least 10 I420 frames of the asked 640 x 480 within 5 s.
#[cfg(target_os = "macos")]
#[test]
fn mac_camera_delivers_frames() {
    use dilla_media::capture_source::{CaptureSource, PixelFormat, Pull};
    use dilla_media::sources::camera;

    if std::env::var("DILLA_MAC_CAPTURE").as_deref() != Ok("1") {
        eprintln!("skipped: set DILLA_MAC_CAPTURE=1 on a Mac whose shell has Camera access");
        return;
    }
    let devices = camera::devices();
    assert!(!devices.is_empty(), "no camera listed");
    let device = devices
        .iter()
        .find(|d| d.name.contains("MacBook") || d.name.contains("FaceTime"))
        .unwrap_or(&devices[0]);
    eprintln!("cameras: {devices:?}; opening {:?}", device.name);

    let started = Instant::now();
    let mut cam = camera::open(Some(&device.id), Duration::from_secs(5), &|| false)
        .unwrap_or_else(|e| panic!("{:?} did not open: {e}", device.name));
    assert_eq!(cam.size(), (camera::WIDTH, camera::HEIGHT));
    let mut frames = 0;
    while frames < 10 {
        assert!(
            started.elapsed() < Duration::from_secs(5),
            "{frames} frames in 5 s"
        );
        match cam.pull() {
            Pull::Frame(f) => {
                assert_eq!(f.format, PixelFormat::I420);
                assert_eq!((f.width, f.height), (camera::WIDTH, camera::HEIGHT));
                assert!(f.is_whole());
                frames += 1;
            }
            Pull::Idle => std::thread::sleep(Duration::from_millis(5)),
            Pull::Ended => panic!("the camera ended after {frames} frames"),
        }
    }
    eprintln!("{frames} frames in {:?}", started.elapsed());
    drop(cam);
}

/// GPUI gaps Task 45 (D22): Dilla's own macOS picker lists the real displays through ScreenCaptureKit (webrtc-sys
/// `dilla_screen_mac.mm`): not refused, at least one screen with a real `CGDirectDisplayID`, and never one of this
/// process's own windows. On macOS 14+ each screen's thumbnail comes from `SCScreenshotManager` and fits the tile. It
/// never asks for Screen Recording: without it the listing is refused (and this fails), it never prompts.
#[cfg(target_os = "macos")]
#[test]
#[ignore = "needs Screen Recording for the terminal that runs it (macOS; iTerm2 on dilla-MacBook, the macos-15 runner's shell): lists and captures the real screen"]
fn mac_picker_lists_a_display_with_its_thumbnail() {
    use dilla_media::sources::desktop::{self, SourceKind};

    let snapshot = desktop::snapshot(64, 320, 180);
    assert!(!snapshot.denied, "Screen Recording is refused");
    let screens: Vec<_> = snapshot
        .sources
        .iter()
        .filter(|(s, _)| s.kind == SourceKind::Screen)
        .collect();
    assert!(!screens.is_empty(), "no screen listed");
    assert!(screens.iter().all(|(s, _)| s.id != 0 && s.owner.is_none()));
    let own = std::process::id();
    assert!(
        snapshot.sources.iter().all(|(s, _)| s.owner != Some(own)),
        "one of this process's own windows is offered"
    );
    eprintln!(
        "{} screens, {} windows",
        screens.len(),
        snapshot.sources.len() - screens.len()
    );
    if objc2::available!(macos = 14.0) {
        for (_, thumbnail) in &screens {
            let t = thumbnail
                .as_ref()
                .expect("a screen's thumbnail on macOS 14+");
            assert!(t.width > 0 && t.height > 0 && t.width <= 320 && t.height <= 180);
        }
    }
}

/// GPUI gaps Task 45 (D22): a window of another process is listed with that process as its owner (the WindowServer's
/// record, `SCWindow.owningApplication`), which is the pid its sound is looked up for; it captures through an own
/// `SCStream`, and the window going away ends the capture. The helper app (tests/support/mac_window.swift) is compiled
/// with `swiftc -O` into this test's temp dir; it is killed on every path.
#[cfg(target_os = "macos")]
#[test]
#[ignore = "needs Screen Recording for the terminal that runs it (macOS; iTerm2 on dilla-MacBook, the macos-15 runner's shell): lists and captures the real screen"]
fn mac_picker_window_owner_is_the_helper_app() {
    use dilla_media::capture_source::{CaptureSource, Pull};
    use dilla_media::sources::desktop::{self, SourceKind};

    /// Kills the helper on every path.
    struct Helper(std::process::Child);
    impl Drop for Helper {
        fn drop(&mut self) {
            let _ = self.0.kill();
            let _ = self.0.wait();
        }
    }

    let nonce = format!(
        "{}-{}",
        std::process::id(),
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_nanos()
    );
    let dir = std::env::temp_dir().join(format!("dilla-mac-window-{nonce}"));
    std::fs::create_dir_all(&dir).unwrap();
    let bin = dir.join("mac_window");
    let swift =
        std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("tests/support/mac_window.swift");
    let built = std::process::Command::new("swiftc")
        .arg("-O")
        .arg(&swift)
        .arg("-o")
        .arg(&bin)
        .status()
        .expect("swiftc runs");
    assert!(built.success(), "the helper did not compile");
    let title = format!("dilla-share-probe-{nonce}");
    let mut helper = Helper(
        std::process::Command::new(&bin)
            .arg(&title)
            .spawn()
            .expect("the helper starts"),
    );
    let pid = helper.0.id();

    let deadline = Instant::now() + Duration::from_secs(10);
    let source = loop {
        if let Some(s) = desktop::list(SourceKind::Window)
            .into_iter()
            .find(|s| s.title == title)
        {
            break s;
        }
        assert!(
            Instant::now() < deadline,
            "the helper's window was never listed"
        );
        std::thread::sleep(Duration::from_millis(250));
    };
    assert_eq!(source.owner, Some(pid));
    assert_eq!(desktop::window_owner_pid(&source), Some(pid));

    let mut capture = desktop::open(Some(&source), 10, desktop::START_WAIT, &|| false)
        .unwrap_or_else(|e| panic!("the helper's window did not open: {e}"));
    let (w, h) = capture.size();
    assert!(w > 0 && h > 0, "an empty first frame");

    let _ = helper.0.kill();
    let _ = helper.0.wait();
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        if matches!(capture.pull(), Pull::Ended) {
            break;
        }
        assert!(Instant::now() < deadline, "the capture outlived its window");
        std::thread::sleep(Duration::from_millis(50));
    }
    drop(capture);
    drop(helper);
    let _ = std::fs::remove_dir_all(&dir);
}

/// The PipeWire nodes of Dilla's KWin capture streams (`dilla-screen-capture`) that exist now, counted from `pw-dump`'s
/// JSON (nothing of it is printed).
#[cfg(target_os = "linux")]
fn kde_capture_nodes() -> usize {
    let out = std::process::Command::new("pw-dump")
        .output()
        .expect("pw-dump runs");
    let dump: Value = serde_json::from_slice(&out.stdout).expect("pw-dump's JSON");
    dump.as_array()
        .map(|objects| {
            objects
                .iter()
                .filter(|o| {
                    o["type"] == "PipeWire:Interface:Node"
                        && o["info"]["props"]["node.name"] == "dilla-screen-capture"
                })
                .count()
        })
        .unwrap_or(0)
}

/// Waits up to 3 s for every KWin capture node of Dilla's to be gone.
#[cfg(target_os = "linux")]
fn kde_nodes_gone() -> bool {
    let deadline = Instant::now() + Duration::from_secs(3);
    while kde_capture_nodes() > 0 {
        if Instant::now() >= deadline {
            return false;
        }
        std::thread::sleep(Duration::from_millis(100));
    }
    true
}

/// KDE share picker plan, Task 1 (D22 on KDE Plasma): with KWin's window list and screencast offered, Dilla lists the
/// screens and windows itself (thumbnails within the tile, none of its own windows), captures a screen and another
/// process's window through KWin's PipeWire stream (linked by Dilla), repeats a still window's last frame at the
/// share's rate, ends the share when the window closes, and leaves no PipeWire node of its own behind. It prints counts
/// and sizes only: never a title, never a pixel. The helper window is a `kdialog` message box, killed on every path.
#[cfg(target_os = "linux")]
#[test]
#[ignore = "needs a KDE Plasma Wayland session whose KWin offers Dilla its window list and screencast (6.8+, or 6.0-6.7 with the installed .desktop file), pw-dump and kdialog"]
fn kde_picker_lists_captures_and_lets_go() {
    use dilla_media::capture_source::{CaptureSource, Pull};
    use dilla_media::sources::desktop::{self, SourceKind};

    /// The helper's process, killed on every path. It is started by a shell that exits at once, so it is not of this
    /// process's tree (Dilla's own tree is never offered).
    struct Helper(u32);
    impl Helper {
        fn start(title: &str) -> Helper {
            let out = std::process::Command::new("sh")
                .args([
                    "-c",
                    "kdialog --title \"$1\" --msgbox \"$1\" >/dev/null 2>&1 & echo $!",
                    "sh",
                    title,
                ])
                .output()
                .expect("sh runs");
            let pid = String::from_utf8_lossy(&out.stdout)
                .trim()
                .parse()
                .expect("the helper's pid");
            Helper(pid)
        }
        fn kill(&self) {
            let _ = std::process::Command::new("kill")
                .arg(self.0.to_string())
                .status();
        }
    }
    impl Drop for Helper {
        fn drop(&mut self) {
            self.kill();
        }
    }

    /// The frames a capture hands over in `span`, pulled every 10 ms (faster than the share's 10 fps, so a source
    /// sending more than it was asked for would show).
    fn frames_in(capture: &mut desktop::DesktopCapture, span: Duration) -> usize {
        let end = Instant::now() + span;
        let mut n = 0;
        while Instant::now() < end {
            match capture.pull() {
                Pull::Frame(f) => {
                    assert!(f.is_whole() && f.width > 0 && f.height > 0);
                    n += 1;
                }
                Pull::Idle => {}
                Pull::Ended => panic!("the capture ended early"),
            }
            std::thread::sleep(Duration::from_millis(10));
        }
        n
    }

    assert!(
        desktop::kde_picker(),
        "KWin offers no window list and screencast here"
    );
    assert!(!desktop::portal(), "the portal would choose");
    assert!(
        kde_nodes_gone(),
        "a capture node of Dilla's was left from before"
    );

    let snapshot = desktop::snapshot(64, 320, 180);
    assert!(!snapshot.denied);
    let screens = snapshot
        .sources
        .iter()
        .filter(|(s, _)| s.kind == SourceKind::Screen)
        .count();
    let windows = snapshot.sources.len() - screens;
    let thumbs = snapshot.sources.iter().filter(|(_, t)| t.is_some()).count();
    assert!(screens >= 1, "no screen listed");
    for (source, thumbnail) in &snapshot.sources {
        if let Some(t) = thumbnail {
            assert!(t.width > 0 && t.height > 0 && t.width <= 320 && t.height <= 180);
        } else {
            assert_eq!(
                source.kind,
                SourceKind::Window,
                "a screen without a thumbnail"
            );
        }
    }
    eprintln!("listed {screens} screens and {windows} windows, {thumbs} thumbnails");
    assert!(
        kde_nodes_gone(),
        "a thumbnail's stream outlived the listing"
    );
    // A listing whose picker closed at once opens no stream at all.
    let cancelled = desktop::snapshot_until(64, 320, 180, &|| true);
    assert!(cancelled.sources.iter().all(|(_, t)| t.is_none()));
    assert_eq!(cancelled.sources.len(), snapshot.sources.len());
    assert!(
        desktop::quick_titles(64).is_some_and(|t| t.sources.len() == snapshot.sources.len()),
        "the quick listing names the same sources"
    );

    // The screen: KWin's frames come at no more than the asked 10 fps (the feeder repeats the last one while the screen
    // stands still), then it lets go.
    let screen = snapshot
        .sources
        .iter()
        .find(|(s, _)| s.kind == SourceKind::Screen)
        .map(|(s, _)| s.clone())
        .unwrap();
    assert_eq!(desktop::window_owner_pid(&screen), None);
    let mut capture = desktop::open(Some(&screen), 10, desktop::START_WAIT, &|| false)
        .unwrap_or_else(|e| panic!("the screen did not open: {e}"));
    let (w, h) = capture.size();
    assert!(w > 0 && h > 0);
    assert_eq!(capture.surface(), Some(SourceKind::Screen));
    assert!(
        capture.repeats_when_idle(),
        "the feeder repeats KWin's last frame"
    );
    let n = frames_in(&mut capture, Duration::from_secs(2));
    eprintln!("screen {w}x{h}: {n} frames pulled in 2 s, 10 fps asked");
    assert!((1..=24).contains(&n), "KWin did not keep to the asked rate");
    // Re-review N3: the rate changes mid-share (a quality change): KWin is asked again and the share goes on.
    capture.set_fps(5);
    let n5 = frames_in(&mut capture, Duration::from_secs(2));
    eprintln!("screen after a change to 5 fps: {n5} frames pulled in 2 s");
    assert!(n5 <= 14, "KWin did not take the new rate");
    assert_eq!(kde_capture_nodes(), 1, "one capture node while sharing");
    drop(capture);
    assert!(kde_nodes_gone(), "the screen's stream outlived its share");

    // Another process's window: its owner is that process, a still window sends its first frame (the feeder repeats it),
    // and closing it ends the share.
    let nonce = format!("dilla-kde-probe-{}", std::process::id());
    let helper = Helper::start(&nonce);
    let pid = helper.0;
    let own = std::process::id();
    let deadline = Instant::now() + Duration::from_secs(10);
    let window = loop {
        let listed = desktop::list(SourceKind::Window);
        assert!(
            listed
                .iter()
                .all(|s| desktop::window_owner_pid(s) != Some(own)),
            "one of this process's windows is offered"
        );
        if let Some(s) = listed
            .into_iter()
            .find(|s| desktop::window_owner_pid(s) == Some(pid))
        {
            break s;
        }
        assert!(
            Instant::now() < deadline,
            "the helper's window was never listed"
        );
        std::thread::sleep(Duration::from_millis(250));
    };
    let mut capture = desktop::open(Some(&window), 10, desktop::START_WAIT, &|| false)
        .unwrap_or_else(|e| panic!("the helper's window did not open: {e}"));
    let (w, h) = capture.size();
    assert!(w > 0 && h > 0);
    assert!(capture.repeats_when_idle());
    let n = frames_in(&mut capture, Duration::from_secs(2));
    eprintln!("window {w}x{h}: {n} frames pulled in 2 s, 10 fps asked (KWin sends one per change)");
    assert!(n >= 1, "the still window's first frame");
    helper.kill();
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        if matches!(capture.pull(), Pull::Ended) {
            break;
        }
        assert!(Instant::now() < deadline, "the capture outlived its window");
        std::thread::sleep(Duration::from_millis(50));
    }
    drop(capture);
    drop(helper);
    assert!(kde_nodes_gone(), "the window's stream outlived its share");
}
