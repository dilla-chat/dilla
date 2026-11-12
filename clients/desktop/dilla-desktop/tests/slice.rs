//! The vertical slice (Task 12), driven through the automation build.
//!
//! - `slice_parity_channel_view` and `slice_parity_channel_view_wide`: the channel view drawn from each parity fixture
//!   matches the web client's reference of the same data (`clients/desktop/parity/references/linux`, shot from the
//!   current web client at scale 2 with Chromium's hinting off) under both gates, with the same tolerance and no
//!   per-fixture calibration: the whole frame, and the chrome outside the text lines the app reports (`text-bounds`).
//!   `basic` is 1280x800 with one server; `wide` is 1440x900 with a second server selected, a category, a long channel
//!   name, eleven members and a message that wraps.
//! - `slice_parity_fails_with_a_fallback_font`: the negative control. The same frame with every line asking for a font
//!   the app does not embed (the text system falls back) fails the whole-frame gate: a fallback face cannot pass.
//! - `slice_sign_in_read_send`: against a real dillad, the app signs a new device in (password, authenticator code,
//!   recovery key), opens #general, and a message typed there and sent with Enter reaches a second, headless client.
//!   Needs `target/dilla-testhost` and the WASI core (see `tests/support/world.rs`); `#[ignore]`d for the desktop CI
//!   job, which has no Go toolchain. Run it with `cargo test -p dilla-desktop --test slice -- --ignored`. Its screens
//!   are saved under `<target>/slice-screens/`.

mod support;

use std::path::{Path, PathBuf};
use std::time::Duration;

use dilla_parity::{
    Driver, ElementInfo, Os, ParityFailure, Rect, RgbaImage, TextMask, assert_parity_masked,
};

use support::world;

fn references() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("../parity/references/linux")
}

/// The device-pixel rectangle (scale 2) of a logical line box, grown by `grow` device pixels for the glyphs' fringe.
fn device_rect(e: &ElementInfo, grow: f32) -> Rect {
    let x0 = (e.x * 2.0 - grow).floor().max(0.0);
    let y0 = (e.y * 2.0 - grow).floor().max(0.0);
    let x1 = ((e.x + e.width) * 2.0 + grow).ceil();
    let y1 = ((e.y + e.height) * 2.0 + grow).ceil();
    Rect {
        x: x0 as u32,
        y: y0 as u32,
        w: (x1 - x0) as u32,
        h: (y1 - y0) as u32,
    }
}

/// The channel view of the parity fixture `name`, its text mask, compared with the reference.
fn channel_view(name: &str, env: &[(&str, &str)], save_as: &str) -> Result<(), ParityFailure> {
    let fixture = references().join(format!("{name}.fixture.json"));
    let reference = references().join(format!("{name}-2x.png"));
    let mut driver = Driver::launch_with_env(&fixture, env);
    driver.wait_for("composer", Duration::from_secs(30));
    let shot: RgbaImage = driver.screenshot();
    let text = driver.text_bounds();
    if std::env::var_os("DILLA_SLICE_DUMP").is_some() {
        for e in &text {
            println!("TEXT {} {} {} {}", e.x, e.y, e.width, e.height);
        }
    }
    assert!(
        text.len() >= 30,
        "the app reports its text lines ({} found)",
        text.len()
    );
    let mask = TextMask::from_rects(text.iter().map(|e| device_rect(e, 2.0)));
    let out = dilla_parity::target_dir().join("slice-parity");
    std::fs::create_dir_all(&out).unwrap();
    shot.save(out.join(save_as)).unwrap();
    assert_parity_masked(&shot, &reference, Os::current(), &mask)
}

#[test]
#[cfg_attr(
    not(target_os = "linux"),
    ignore = "the channel-view reference is shot on Linux only (macOS and Windows tolerances are PENDING-CI)"
)]
fn slice_parity_channel_view() {
    channel_view("channel-view", &[], "channel-view-gpui-2x.png")
        .unwrap_or_else(|e| panic!("the channel view differs from the web reference: {e:?}"));
}

#[test]
#[cfg_attr(
    not(target_os = "linux"),
    ignore = "the channel-view reference is shot on Linux only (macOS and Windows tolerances are PENDING-CI)"
)]
fn slice_parity_channel_view_wide() {
    channel_view("channel-view-wide", &[], "channel-view-wide-gpui-2x.png")
        .unwrap_or_else(|e| panic!("the wide channel view differs from the web reference: {e:?}"));
}

#[test]
#[cfg_attr(
    not(target_os = "linux"),
    ignore = "the channel-view reference is shot on Linux only (macOS and Windows tolerances are PENDING-CI)"
)]
fn slice_parity_fails_with_a_fallback_font() {
    let result = channel_view(
        "channel-view",
        &[("DILLA_AUTOMATION_FALLBACK_FONT", "1")],
        "channel-view-fallback-font-2x.png",
    );
    assert!(
        matches!(result, Err(ParityFailure::Pixels { .. })),
        "a fallback face must fail the whole-frame gate, got {result:?}"
    );
}

/// Saves the app's frame as `<target>/slice-screens/<name>.png`.
fn keep(app: &mut Driver, name: &str) {
    let dir = dilla_parity::target_dir().join("slice-screens");
    std::fs::create_dir_all(&dir).unwrap();
    app.screenshot()
        .save(dir.join(format!("{name}.png")))
        .unwrap();
}

#[test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (no Go toolchain in the desktop-gpui CI job)"]
fn slice_sign_in_read_send() {
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let host = world::start_host("slice", "127.0.0.1:0", None);
    let invite = host.invites.last().unwrap().clone();
    let (mut alice, bob) = rt.block_on(async {
        let alice = world::sign_up(&host, &invite, "alice", "Alice", "slice").await;
        let bob = world::sign_up(&host, &invite, "bob", "Bob", "slice").await;
        (alice, bob)
    });
    let community = world::community(&host, &alice, "Slice", &[&bob]);
    world::clear_session(&alice);
    world::clear_session(&bob);
    let bob_client = rt.block_on(world::start(&bob));
    let general = rt.block_on(async {
        let s = world::wait_for(
            &bob_client,
            Duration::from_secs(30),
            "the channel list",
            |s| {
                s.channel_named(dilla_client::state::CommunityId(community), "general")
                    .is_some()
            },
        )
        .await;
        let id = s
            .channel_named(dilla_client::state::CommunityId(community), "general")
            .unwrap()
            .id;
        bob_client
            .open_channel(id)
            .await
            .unwrap()
            .expect("bob opens #general");
        id
    });

    // The app, on an empty data root: a new device of Alice's.
    let root = scratch_dir("slice-app");
    let mut app = Driver::launch(&root);
    app.wait_for("signin-server", Duration::from_secs(30));
    keep(&mut app, "1-sign-in");
    app.type_text("signin-server", &host.public);
    // Tab moves through the fields in the web's order: server, username, password.
    app.press("tab");
    app.type_focused("alice");
    app.press("tab");
    app.type_focused(world::PASSWORD);
    // Enter in a field submits the step, as in the browser.
    app.press("enter");
    app.wait_for("signin-code", Duration::from_secs(30));
    let code = alice.next_code(&host);
    app.type_text("signin-code", &code);
    keep(&mut app, "2-code");
    app.click("signin-verify");
    app.wait_for("signin-key", Duration::from_secs(30));
    app.type_text("signin-key", &alice.recovery_key);
    keep(&mut app, "3-recovery-key");
    app.click("signin-add");
    app.wait_for("signin-open", Duration::from_secs(60));
    keep(&mut app, "4-done");
    app.click("signin-open");
    app.wait_for("channel-general", Duration::from_secs(60));
    app.click("channel-general");
    app.wait_for("composer", Duration::from_secs(30));
    // The device joins #general's group as it opens it; give the join a moment before sending.
    std::thread::sleep(Duration::from_secs(2));
    app.type_text("composer", "hello from the preview");
    keep(&mut app, "5-typed");
    app.press("enter");

    rt.block_on(world::wait_for(
        &bob_client,
        Duration::from_secs(30),
        "bob to receive the preview's message",
        |s| {
            s.timelines.get(&general).is_some_and(|t| {
                t.items.iter().any(|i| {
                    i.body == "hello from the preview"
                        && i.sender_user == Some(dilla_client::state::UserId(alice.user_id))
                })
            })
        },
    ));
    keep(&mut app, "6-sent");
    drop(app);
    bob_client.stop();
    let _ = std::fs::remove_dir_all(&root);
}

fn scratch_dir(tag: &str) -> PathBuf {
    let dir = world::repo_root().join(format!("target/{tag}-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    dir
}
