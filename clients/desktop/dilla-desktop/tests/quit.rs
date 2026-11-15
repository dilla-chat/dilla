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
