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
        #[cfg_attr(not(target_os = "linux"), ignore = "references are Linux only")]
        $(#[ignore = $why])?
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
