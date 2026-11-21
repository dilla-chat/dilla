//! Task 11's parity, behaviour and keyboard tests for the threads area (standard procedure S4 to S6).
//!
//! The parity tests compare the app's frame of each fixture with the web client's (`parity/references/linux/threads/`).
//! The behaviour and keyboard tests drive the automation build on those fixtures: a fixture draws the app without a
//! network, so what a click opens (the pane) is the area's own state; the client-level halves of
//! `e2e/web/threads.spec.ts` are in `dilla-client/tests/threads.rs`, against a real dillad.

use std::path::{Path, PathBuf};
use std::time::Duration;

use dilla_parity::Driver;
use dilla_parity::screen::assert_screen;

fn fixture(name: &str) -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../parity/fixtures/threads")
        .join(format!("{name}.json"))
}

fn launch(name: &str) -> Driver {
    let mut driver = Driver::launch_fixture(&fixture(name), None, &[]);
    driver.wait_idle();
    driver
}

macro_rules! parity {
    ($($test:ident => $name:literal $(, $why:literal)?),* $(,)?) => {$(
        #[test]
        #[cfg_attr(not(any(target_os = "linux", target_os = "macos")), ignore = "references are Linux and macOS only")]
        $(#[cfg_attr(any(target_os = "linux", target_os = "macos"), ignore = $why)])?  // elsewhere the line above already ignores it
        fn $test() {
            let report = assert_screen(&fixture($name))
                .unwrap_or_else(|e| panic!("threads/{} differs from its web reference: {e}", $name));
            println!("SCREEN threads/{}: {} text lines, chrome {:.4} %", $name, report.text_lines, report.chrome_pct);
        }
    )*};
}

parity! {
    parity_threads_reply_chip => "reply-chip",
    parity_threads_reply_chip_b => "reply-chip-b",
    parity_threads_pane_open => "pane-open",
    parity_threads_pane_open_b => "pane-open-b",
    parity_threads_count => "count",
    parity_threads_count_b => "count-b",
    // Task 19b: replies in the log and the open thread pane at the compact density (`"density": "compact"`).
    parity_threads_reply_compact => "reply-compact",
    parity_threads_pane_compact => "pane-compact",
    // Task 31: `/` typed in the thread's composer opens the command list over it.
    parity_threads_command_list => "command-list",
    parity_threads_command_list_b => "command-list-b",
}

/// A GPUI-only reference (the web never draws a "load earlier" in a thread): `pane-open`'s world with 201 replies, so the
/// open thread holds its newest 200 and "load earlier" sits over the first of them (Task 17).
#[test]
#[cfg_attr(
    not(any(target_os = "linux", target_os = "macos")),
    ignore = "references are Linux and macOS only"
)]
fn parity_thread_earlier() {
    dilla_parity::screen::assert_native_screen(
        &fixture("thread-earlier"),
        "thread-earlier",
        |app| {
            app.click("thread-chip");
            app.wait_for("thread-close", SOON);
            // The pane's `has_earlier` before the shot: the button is its only trace.
            app.wait_for("thread-earlier", SOON);
        },
    )
    .unwrap_or_else(|e| panic!("threads/thread-earlier differs from its golden shot: {e}"));
}

const SOON: Duration = Duration::from_secs(5);

/// A reply shows its reply line, naming the original (e2e/web/conversation.spec.ts: the reply line is a button).
#[test]
fn a_reply_draws_its_reply_line() {
    let mut app = launch("reply-chip");
    let line = app.wait_for("reply-line", SOON);
    assert!(line.width > 0.0 && line.height > 0.0, "{line:?}");
}

/// A root with replies shows its count under its body, and a message without a thread shows none.
#[test]
fn the_count_chip_is_on_the_root_and_nowhere_else() {
    let mut app = launch("count");
    let chip = app.wait_for("thread-chip", SOON);
    // 24px high (MessageRow.css `.d-message-row__thread`).
    assert_eq!(chip.height, 24.0, "{chip:?}");
    let mut plain = launch("reply-chip");
    assert!(plain.element("thread-chip").is_none(), "no thread, no chip");
}

/// threads.spec.ts test 1: the pane is in the aside, a column beside the channel, and opening leaves its composer unfocused.
#[test]
fn thread_pane_opens_in_the_aside_with_its_composer_unfocused() {
    let mut app = launch("pane-open");
    assert!(app.element("thread-close").is_none(), "closed until opened");
    app.click("thread-chip");
    let close = app.wait_for("thread-close", SOON);
    let composer = app.wait_for("thread-composer", SOON);
    // `--thread-w`: 340px at the window's right edge, over the sidebar-less column.
    assert!(
        close.x + close.width <= 1280.0 && close.x >= 1280.0 - 340.0,
        "{close:?}"
    );
    assert!(composer.y > close.y, "the composer is below the head");
    assert_ne!(
        app.focused().as_deref(),
        Some("thread-composer"),
        "no script focus on open"
    );
}

/// The pane's × closes it.
#[test]
fn the_close_button_closes_the_pane() {
    let mut app = launch("pane-open");
    app.click("thread-chip");
    app.wait_for("thread-close", SOON);
    app.click("thread-close");
    app.wait_idle();
    assert!(app.element("thread-close").is_none());
}

/// Keyboard (task 11): Escape inside the pane closes it and nothing more, and focus returns to the thread's chip.
#[test]
fn keyboard_threads_escape_closes_the_pane_and_returns_focus_to_the_chip() {
    let mut app = launch("pane-open");
    app.click("thread-chip");
    app.wait_for("thread-composer", SOON);
    app.click("thread-composer");
    assert_eq!(app.focused().as_deref(), Some("thread-composer"));
    app.press("escape");
    app.wait_idle();
    assert!(
        app.element("thread-close").is_none(),
        "Escape closed the pane"
    );
    assert_eq!(
        app.focused().as_deref(),
        Some("thread-chip"),
        "focus is back on the chip"
    );
}

/// Keyboard: the pane's focus order is the close button, then the composer; Enter on the chip opens the thread.
#[test]
fn keyboard_threads_focus_order() {
    let mut app = launch("pane-open");
    app.click("thread-chip");
    app.wait_for("thread-composer", SOON);
    app.click("thread-composer");
    let mut seen = Vec::new();
    for _ in 0..4 {
        app.press("shift-tab");
        seen.push(app.focused());
    }
    eprintln!("SHIFT-TAB SEQUENCE {seen:?}");
    app.click("thread-composer");
    app.press("shift-tab");
    assert_eq!(
        app.focused().as_deref(),
        Some("thread-close"),
        "the close button precedes the composer"
    );
    app.press("tab");
    assert_eq!(app.focused().as_deref(), Some("thread-composer"));
    // Closed from the keyboard, then opened from the keyboard: Enter on the focused chip.
    app.press("escape");
    app.wait_idle();
    assert!(app.element("thread-close").is_none());
    assert_eq!(app.focused().as_deref(), Some("thread-chip"));
    app.press("enter");
    app.wait_for("thread-close", SOON);
    // Closing from the keyboard-opened chip returns focus to the chip.
    app.click("thread-composer");
    app.press("escape");
    app.wait_idle();
    assert_eq!(app.focused().as_deref(), Some("thread-chip"));
}

/// Task 31 (ComposerArea.tsx:292): the thread's composer runs slash commands. `/` in it opens the list over it, not over
/// the channel's composer; a picked command's chips take its field's place, and Escape out of them closes the list.
#[test]
fn a_slash_in_the_thread_composer_opens_the_list_over_it() {
    let mut app = launch("pane-open");
    app.click("thread-chip");
    app.wait_for("thread-composer", SOON);
    app.type_text("thread-composer", "/");
    let list = app.wait_for("command-picker", SOON);
    let composer = app.element("thread-composer").expect("the thread composer");
    assert!(list.y + list.height <= composer.y, "{list:?} {composer:?}");
    let channel = app.element("composer").expect("the channel composer");
    assert!(
        list.x >= channel.x + channel.width,
        "the list is in the pane, beside the channel's composer: {list:?} {channel:?}"
    );
    app.type_focused("shrug");
    app.wait_idle();
    app.press("enter");
    app.wait_for("command-chips", SOON);
    app.press("escape");
    app.wait_idle();
    assert!(app.element("command-picker").is_none(), "no list stays");
}

mod support;
use support::world;

/// Task 31 (builtin.ts `targetThread`): in the thread's composer `/shrug hi` posts into the thread, and `/pin` of one of
/// its replies refuses, as a private row in the pane.
#[test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core"]
fn a_thread_command_posts_into_the_thread() {
    use dilla_client::state::CommunityId;
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let host = world::start_host("t31-thread", "127.0.0.1:0", None);
    let invite = host.invites.last().unwrap().clone();
    let mut ada = rt.block_on(world::sign_up(&host, &invite, "ada", "Ada", "t31"));
    let bea = rt.block_on(world::sign_up(&host, &invite, "bea", "Bea", "t31"));
    let community = CommunityId(world::community(&host, &ada, "Midgard", &[&bea]));
    world::clear_session(&ada);
    world::clear_session(&bea);
    let member = rt.block_on(world::start(&bea));
    let state = rt.block_on(world::wait_for(
        &member,
        Duration::from_secs(30),
        "bea's channel list",
        |s| s.channel_named(community, "general").is_some(),
    ));
    let general = state.channel_named(community, "general").unwrap().id;
    rt.block_on(member.open_channel(general)).unwrap().unwrap();
    let dir = std::env::temp_dir().join(format!("t31-thread-app-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    let mut app = Driver::launch(&dir);
    app.wait_for("onboarding-signin", Duration::from_secs(30));
    app.click("onboarding-signin");
    app.wait_for("signin-server", Duration::from_secs(30));
    app.type_text("signin-server", &host.public);
    app.press("tab");
    app.type_focused(&ada.username);
    app.press("tab");
    app.type_focused(world::PASSWORD);
    app.press("enter");
    app.wait_for("signin-code", Duration::from_secs(30));
    let code = ada.next_code(&host);
    app.type_text("signin-code", &code);
    app.click("signin-verify");
    app.wait_for("signin-key", Duration::from_secs(30));
    app.type_text("signin-key", &ada.recovery_key);
    app.click("signin-add");
    app.wait_for("signin-open", Duration::from_secs(60));
    app.click("signin-open");
    app.wait_for("channel-general", Duration::from_secs(60));
    app.click("channel-general");
    app.wait_for("composer", Duration::from_secs(30));
    // Bea posts once the app's device is in the group: a device reads nothing from before it joined.
    rt.block_on(member.send_text(general, "root".to_owned()))
        .unwrap()
        .unwrap();
    let state = rt.block_on(world::wait_for(
        &member,
        Duration::from_secs(30),
        "the root stored",
        |s| {
            s.timelines
                .get(&general)
                .is_some_and(|t| t.items.iter().any(|i| i.body == "root" && i.seq.is_some()))
        },
    ));
    let root = state.timelines[&general]
        .items
        .iter()
        .find(|i| i.body == "root")
        .and_then(|i| i.msg_id)
        .unwrap();
    let reply = rt
        .block_on(
            member
                .threads()
                .send_in_thread(general, root, "first reply".to_owned()),
        )
        .unwrap()
        .expect("queued");

    app.wait_for("thread-chip", Duration::from_secs(60));
    app.click("thread-chip");
    app.wait_for("thread-composer", SOON);

    // Bea reads the thread, so its replies are in her state.
    drop(member.threads().open(general, root));
    app.type_text("thread-composer", "/shrug hi");
    app.wait_idle();
    app.press("enter");
    rt.block_on(world::wait_for(
        &member,
        Duration::from_secs(60),
        "the shrug in the thread",
        |s| {
            s.threads.threads.get(&root).is_some_and(|d| {
                d.replies
                    .iter()
                    .any(|r| r.body.contains('¯') && r.body.ends_with("hi"))
            })
        },
    ));
    let s = member.state().borrow().clone();
    assert!(
        s.timelines[&general]
            .items
            .iter()
            .all(|i| !i.body.contains('¯')),
        "the channel timeline gains nothing"
    );
    app.type_text("thread-composer", &format!("/pin message:{}", reply.hex()));
    app.wait_idle();
    app.press("enter");
    let row = app.wait_for("bot-result-failed", Duration::from_secs(30));
    // The row is the pane's own, not the channel log's.
    let close = app.element("thread-close").expect("the pane is open");
    let pane = f32::from(dilla_desktop_ui::tokens::layout::THREAD);
    assert!(row.x >= close.x + close.width - pane, "{row:?} {close:?}");
    drop(app);
    member.stop();
    let _ = std::fs::remove_dir_all(&dir);
}
