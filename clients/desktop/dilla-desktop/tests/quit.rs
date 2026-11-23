//! Package 7's quit guards through the public shell: beta.5 `e2e/voice/realtime-transport.spec.ts` "…defers an update
//! until the call ends" (the quit part; the update itself is the release plan's Task 14).

use std::cell::Cell;
use std::rc::Rc;

use dilla_client::seams::InstanceId;
use dilla_desktop::native::quit::{self, Dialog};
use dilla_desktop::native::{GuardState, NativeShell, QuitAnswer, QuitReason};
use gpui::TestAppContext;

/// "Restart to update" with a call on asks first ("Quit Dilla?") and does not restart on Cancel; a guard that cannot
/// tell holds the restart back ("Dilla is still in use", one button); once the call ended it restarts without a
/// question.
#[gpui::test]
fn an_update_restart_waits_for_the_call(cx: &mut TestAppContext) {
    let call = Rc::new(Cell::new(true));
    let unknown = Rc::new(Cell::new(false));
    cx.add_empty_window();
    let (c, u) = (call.clone(), unknown.clone());
    cx.update(|cx| {
        quit::install(cx);
        NativeShell::add_quit_guard(
            InstanceId(0),
            Box::new(move |_| GuardState {
                active_call: c.get(),
                unsaved_draft: false,
                answered: !u.get(),
            }),
            cx,
        );
    });
    let restart = |cx: &mut TestAppContext| {
        cx.update(|cx| NativeShell::request_quit_for(QuitReason::UpdateRestart, cx));
        cx.run_until_parked();
    };
    let showing = |cx: &mut TestAppContext| cx.update(|cx| quit::showing(cx)).map(|(_, d)| d);
    let quitting = |cx: &mut TestAppContext| cx.update(|cx| NativeShell::is_quitting(cx));

    restart(cx);
    assert_eq!(showing(cx), Some(Dialog::InUse));
    cx.update(|cx| quit::answer(QuitAnswer::Stay, cx));
    assert!(!quitting(cx), "the call holds the update back");

    unknown.set(true);
    restart(cx);
    assert_eq!(showing(cx), Some(Dialog::StillInUse));
    cx.update(|cx| quit::answer(QuitAnswer::Stay, cx));
    assert!(!quitting(cx));

    unknown.set(false);
    call.set(false);
    restart(cx);
    assert_eq!(showing(cx), None, "nothing in use: no question");
    assert!(quitting(cx));
}

// ---- parity: the quit dialog's GPUI-only golden shots (parity/references/linux/native/) ---------------------------
//
// The web client has no quit dialog (beta.5 asked through the OS), so these references are the app's own frame of the
// dialog over the channel view, checked by eye against the Signal design's confirmation (`ConfirmDialog`, the component
// of messages/moderate-remove's web-shot dialog) when they were made. Remake them with `DILLA_PARITY_BLESS=1` after a
// deliberate change, and look at them again (parity/references/linux/README.md, "GPUI-only references").

fn shoot_quit(state: &str) {
    let fixture = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../parity/fixtures/shell/channel-view.json");
    dilla_parity::screen::assert_native_screen(&fixture, &format!("quit-{state}"), |driver| {
        let reply = driver.request(&format!("quit-dialog {state}"));
        assert!(reply.starts_with("ok "), "quit-dialog {state}: {reply}");
    })
    .unwrap_or_else(|e| panic!("the quit dialog ({state}) differs from its golden shot: {e}"));
}

#[test]
#[cfg_attr(
    not(any(target_os = "linux", target_os = "macos")),
    ignore = "references are Linux and macOS only"
)]
fn parity_quit_dialog_active_call() {
    shoot_quit("active-call");
}

#[test]
#[cfg_attr(
    not(any(target_os = "linux", target_os = "macos")),
    ignore = "references are Linux and macOS only"
)]
fn parity_quit_dialog_unsent_draft() {
    shoot_quit("unsent-draft");
}

#[test]
#[cfg_attr(
    not(any(target_os = "linux", target_os = "macos")),
    ignore = "references are Linux and macOS only"
)]
fn parity_quit_dialog_not_responding() {
    shoot_quit("not-responding");
}
