//! The vertical slice (Task 12), driven through the automation build.
//!
//! The channel view's parity tests (`channel_view_two_fixtures`, the negative controls) moved to
//! `clients/desktop/parity/tests/screen.rs` with Task 5's screen harness (`parity::screen::assert_screen`).
//!
//! - `slice_sign_in_read_send`: against a real dillad, the app signs a new device in (password, authenticator code,
//!   recovery key), opens #general, and a message typed there and sent with Enter reaches a second, headless client.
//!   Needs `target/dilla-testhost` and the WASI core (see `tests/support/world.rs`); `#[ignore]`d so a plain `cargo test` needs no Go toolchain; the desktop-gpui
//!   CI job builds the test host and runs it on Linux. Run it with `cargo test -p dilla-desktop --test slice -- --ignored`. Its screens
//!   are saved under `<target>/slice-screens/`.

mod support;

use std::path::PathBuf;
use std::time::Duration;

use dilla_parity::Driver;

use support::world;

/// Saves the app's frame as `<target>/slice-screens/<name>.png`.
fn keep(app: &mut Driver, name: &str) {
    let dir = dilla_parity::target_dir().join("slice-screens");
    std::fs::create_dir_all(&dir).unwrap();
    app.screenshot()
        .save(dir.join(format!("{name}.png")))
        .unwrap();
}

#[test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
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
    // A device with no account opens on the sign-up (Task 25); its link leads to the sign-in.
    app.wait_for("onboarding-signin", Duration::from_secs(30));
    app.click("onboarding-signin");
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
