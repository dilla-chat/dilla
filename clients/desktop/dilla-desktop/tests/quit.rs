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

fn golden(state: &str) -> std::path::PathBuf {
    std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../parity/references/linux/native")
        .join(format!("quit-{state}-2x.png"))
}

fn shoot_quit(state: &str) {
    let parity = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("../parity");
    let fixture = parity.join("fixtures/shell/channel-view.json");
    let loaded = dilla_parity::fixture::load(&fixture, dilla_parity::Os::Linux).unwrap();
    let mut driver =
        dilla_parity::Driver::launch_fixture(&fixture, loaded.shot_path.as_deref(), &[]);
    driver.wait_idle();
    let reply = driver.request(&format!("quit-dialog {state}"));
    assert!(reply.starts_with("ok "), "quit-dialog {state}: {reply}");
    driver.rest();
    driver.wait_idle();
    let shot = driver.screenshot();
    let mask = dilla_parity::text_mask(&mut driver, dilla_parity::screen::TEXT_FRINGE_PX);
    let path = golden(state);
    if std::env::var_os("DILLA_PARITY_BLESS").is_some() {
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();
        shot.save(&path).unwrap();
        return;
    }
    // macOS compares with the same Linux frames under its own tolerance (decision D-A: no macOS goldens).
    if std::env::var_os(dilla_parity::screen::MEASURE_VAR).is_some() {
        let golden = image::open(&path).unwrap().to_rgba8();
        let line = dilla_parity::screen::measure_line(
            &format!("native/quit-{state}"),
            &shot,
            &golden,
            &mask,
        );
        println!("{}", line.unwrap());
        return;
    }
    dilla_parity::assert_parity_masked(&shot, &path, dilla_parity::Os::current(), &mask)
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
