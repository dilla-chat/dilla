//! Package 7's quit guards through the public shell: beta.5 `e2e/voice/realtime-transport.spec.ts` "…defers an update
//! until the call ends" (the quit part; the update itself is the release plan's Task 14).

use std::cell::Cell;
use std::rc::Rc;

use dilla_client::seams::InstanceId;
use dilla_desktop::native::quit::{self, copy};
use dilla_desktop::native::{GuardState, NativeShell, QuitReason};
use gpui::TestAppContext;

/// "Restart to update" with a call on asks first and does not restart on Cancel; once the call ended it restarts
/// without a question. A guard that cannot tell holds the restart back ("Dilla is still in use", one button).
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

    restart(cx);
    cx.simulate_prompt_answer(copy::CANCEL);
    cx.run_until_parked();
    assert!(
        !cx.update(|cx| NativeShell::is_quitting(cx)),
        "the call holds the update back"
    );

    unknown.set(true);
    restart(cx);
    cx.simulate_prompt_answer(copy::KEEP_OPEN);
    cx.run_until_parked();
    assert!(!cx.update(|cx| NativeShell::is_quitting(cx)));

    unknown.set(false);
    call.set(false);
    restart(cx);
    assert!(!cx.has_pending_prompt(), "nothing in use: no question");
    assert!(cx.update(|cx| NativeShell::is_quitting(cx)));
}
