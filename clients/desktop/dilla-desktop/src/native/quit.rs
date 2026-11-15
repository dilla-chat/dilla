//! The quit dialogs and the quit guards (Package 7; beta.5 `main.ts` `collectQuitGuards`, `requestQuit`, and the web's
//! `setDesktopGuard` / `quit-ack`).
//!
//! **Guards.** One guard per instance, added the first time a window of it opens ([`super::WindowEvent`]) and kept
//! while the process runs: a closed window's client (and its call) goes on in the background, so its guard still
//! counts. A guard reads the instance's state at once ([`instance_guard`]):
//! - `active_call`: its client's own call is not idle (joining, live or reconnecting; Shell.tsx `call.channelId !==
//!   null || call.recoveryChannelId !== null`);
//! - `unsaved_draft`: files in one of its attachment trays (lost at quit), or composer text not written to the
//!   instance's prefs yet (the composer writes its draft 400 ms after the last change; the web's `pendingInput()`).
//!   A draft already in the prefs is not at risk: the native composer keeps drafts across restarts, which the web's
//!   in-memory drafts did not;
//! - `answered`: always, since a guard runs on the UI thread. The shell's "not responding" path stays for a provider
//!   that cannot tell.
//!
//! **Dialogs**, in beta.5's order, one per request ([`Dialog::for_request`]):
//! 1. a provider that did not answer: "Dilla is not responding" (Keep Dilla open / Quit anyway);
//! 2. something in use: "Quit Dilla?" (Cancel / Quit);
//! 3. an update restart with a provider that did not answer: "Dilla is still in use" (Keep Dilla open), since beta.5
//!    never installs over an unknown guard.
//!
//! Jonas (2026-11-15): on every OS the question is the Signal design's in-app confirmation (`ConfirmDialog` over
//! `Dialog`: a modal alert dialog for screen readers, focus on the safe button, Escape and the scrim keep the app), with
//! beta.5's copy: the title is beta.5's dialog title, the body its message and detail. It is drawn by the window that
//! carries it ([`overlay`], called from the app's and the instances window's roots): the active window, else the oldest,
//! else the instances window opened for it.

use std::collections::BTreeSet;
use std::rc::Rc;

use dilla_client::Client;
use dilla_client::calls::CallPhase;
use dilla_client::seams::InstanceId;
use dilla_desktop_ui::components::confirm_dialog::{ConfirmDialog, Tone};
use dilla_desktop_ui::components::dialog::{Dialog as Modal, DialogKind, InitialFocus};
use dilla_desktop_ui::text::{line_height, paragraph};
use dilla_desktop_ui::tokens::{color, text};
use gpui::{
    AnyElement, AnyWindowHandle, App, Global, IntoElement, ParentElement, Styled, Window, WindowId,
    div,
};

use super::{GuardState, NativeShell, QuitAnswer, QuitReason, QuitRequest, WindowEvent};

/// beta.5 `main.ts` copy, word for word (none of it is in the web client's string catalogue).
pub mod copy {
    pub const NOT_RESPONDING_TITLE: &str = "Dilla is not responding";
    pub const NOT_RESPONDING_MESSAGE: &str =
        "A Dilla window has not reported whether it is in use.";
    pub const NOT_RESPONDING_DETAIL: &str =
        "Quitting may end a call or discard text you have not sent.";
    pub const KEEP_OPEN: &str = "Keep Dilla open";
    pub const QUIT_ANYWAY: &str = "Quit anyway";
    pub const IN_USE_TITLE: &str = "Quit Dilla?";
    pub const IN_USE_MESSAGE: &str =
        "Quitting leaves your call and discards text you haven't sent.";
    pub const CANCEL: &str = "Cancel";
    pub const QUIT: &str = "Quit";
    pub const STILL_IN_USE_TITLE: &str = "Dilla is still in use";
    pub const STILL_IN_USE_MESSAGE: &str =
        "Finish your call and save your drafts before quitting or updating.";
    pub const STILL_IN_USE_DETAIL: &str =
        "You can close the window to keep Dilla running in the tray.";
}

/// The element id of the quit dialog (its buttons are `(id, "confirm")` and `(id, "close-button")`).
pub const DIALOG_ID: &str = "quit-dialog";

/// One of beta.5's quit dialogs.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Dialog {
    NotResponding,
    InUse,
    StillInUse,
}

impl Dialog {
    /// The dialog `request` needs, or `None` when it may quit at once (beta.5 `collectQuitGuards`).
    pub fn for_request(request: &QuitRequest) -> Option<Self> {
        let restart = request.reason == QuitReason::UpdateRestart;
        if request.unanswered() {
            Some(if restart {
                Self::StillInUse
            } else {
                Self::NotResponding
            })
        } else if request.in_use() {
            Some(Self::InUse)
        } else {
            None
        }
    }

    /// beta.5's dialog title: the dialog's heading.
    pub fn title(self) -> &'static str {
        match self {
            Self::NotResponding => copy::NOT_RESPONDING_TITLE,
            Self::InUse => copy::IN_USE_TITLE,
            Self::StillInUse => copy::STILL_IN_USE_TITLE,
        }
    }

    /// beta.5's message, then its detail: the dialog's body.
    pub fn body(self) -> String {
        match self {
            Self::NotResponding => {
                format!(
                    "{} {}",
                    copy::NOT_RESPONDING_MESSAGE,
                    copy::NOT_RESPONDING_DETAIL
                )
            }
            Self::InUse => copy::IN_USE_MESSAGE.to_owned(),
            Self::StillInUse => {
                format!(
                    "{} {}",
                    copy::STILL_IN_USE_MESSAGE,
                    copy::STILL_IN_USE_DETAIL
                )
            }
        }
    }

    /// The safe button (beta.5 `cancelId: 0`): it keeps the app, as Escape and the scrim do.
    pub fn stay_label(self) -> &'static str {
        match self {
            Self::NotResponding | Self::StillInUse => copy::KEEP_OPEN,
            Self::InUse => copy::CANCEL,
        }
    }

    /// The button that quits (`None`: the dialog only informs).
    pub fn quit_label(self) -> Option<&'static str> {
        match self {
            Self::NotResponding => Some(copy::QUIT_ANYWAY),
            Self::InUse => Some(copy::QUIT),
            Self::StillInUse => None,
        }
    }
}

/// The instances that have a guard.
#[derive(Default)]
struct Guarded(BTreeSet<u8>);

impl Global for Guarded {}

/// Called once by [`super::install`]: the quit dialogs, and a guard for each instance as its first window opens.
pub fn install(cx: &mut App) {
    NativeShell::set_quit_prompt(Box::new(ask), cx);
    NativeShell::on_window_event(
        Box::new(|event, cx| match *event {
            WindowEvent::Opened {
                instance: Some(id), ..
            }
            | WindowEvent::Moved { instance: id, .. } => guard(id, cx),
            // The window carrying the quit question went away: the question ends as Stay.
            WindowEvent::Closed { window, .. }
                if showing(cx).is_some_and(|(asked, _)| asked == window) =>
            {
                answer(QuitAnswer::Stay, cx)
            }
            _ => {}
        }),
        cx,
    );
    for (_, instance) in NativeShell::open_windows(cx) {
        if let Some(id) = instance {
            guard(id, cx);
        }
    }
}

/// Adds instance `id`'s guard, once.
pub fn guard(id: InstanceId, cx: &mut App) {
    if !cx.default_global::<Guarded>().0.insert(id.0) {
        return;
    }
    NativeShell::add_quit_guard(id, Box::new(move |cx| instance_guard(id, cx)), cx);
}

/// What instance `id` would lose if the app quit now.
pub fn instance_guard(id: InstanceId, cx: &App) -> GuardState {
    let Some(client) = crate::windows::running_client(cx, id) else {
        return GuardState::IDLE;
    };
    let active_call = client.state().borrow().voice.call.phase != CallPhase::Idle;
    GuardState {
        active_call,
        unsaved_draft: has_files(&client, cx) || has_unwritten_text(id, &client, cx),
        answered: true,
    }
}

/// Files wait in one of `client`'s attachment trays.
fn has_files(client: &Client, cx: &App) -> bool {
    let serial = client.serial();
    cx.try_global::<crate::screens::attachments::ui::AttachmentsUi>()
        .is_some_and(|ui| {
            ui.trays
                .iter()
                .any(|((owner, _), tray)| *owner == serial && !tray.entries().is_empty())
        })
}

/// A composer of instance `id` holds text its channel's draft in the prefs does not have yet.
fn has_unwritten_text(id: InstanceId, client: &Rc<Client>, cx: &App) -> bool {
    let Some(ui) = cx.try_global::<crate::screens::attachments::ui::AttachmentsUi>() else {
        return false;
    };
    NativeShell::open_windows(cx)
        .into_iter()
        .filter(|(_, instance)| *instance == Some(id))
        .any(|(window, _)| {
            let Some(target) = ui.targets.get(&window) else {
                return false;
            };
            let (Some(owner), Some(channel)) = (&target.client, target.channel) else {
                return false;
            };
            if !Rc::ptr_eq(owner, client) {
                return false;
            }
            let typed = crate::screens::commands::picker_state(window, cx).draft;
            if typed.is_empty() {
                return false;
            }
            let kept: String = client
                .prefs()
                .get(&crate::screens::messages::composing::draft_key(channel))
                .unwrap_or_default();
            typed != kept
        })
}

/// The quit question now: its dialog and the window that draws it.
struct Asking {
    window: WindowId,
    dialog: Dialog,
}

impl Global for Asking {}

/// The quit prompt: the dialog the request needs, drawn in a window, then [`NativeShell::finish_quit`] through
/// [`answer`].
fn ask(request: QuitRequest, cx: &mut App) {
    let Some(dialog) = Dialog::for_request(&request) else {
        NativeShell::finish_quit(QuitAnswer::Quit, cx);
        return;
    };
    let window = host_window(cx).or_else(|| {
        // In the tray with no window: the instances window carries the question (beta.5's dialog had no parent).
        let host = NativeShell::host(cx)?;
        crate::windows::open_manager(cx, &host.launch, host.allow_loopback)
            .ok()
            .map(|m| m.into())
    });
    let Some(window) = window else {
        log::warn!("dilla-desktop: no window to ask about quitting in; staying open");
        NativeShell::finish_quit(QuitAnswer::Stay, cx);
        return;
    };
    cx.set_global(Asking {
        window: window.window_id(),
        dialog,
    });
    let _ = window.update(cx, |_, window, _| window.activate_window());
    cx.activate(true);
    cx.refresh_windows();
}

/// The quit dialog showing now, if any.
pub fn showing(cx: &App) -> Option<(WindowId, Dialog)> {
    cx.try_global::<Asking>().map(|a| (a.window, a.dialog))
}

/// The person answered the quit dialog (a button, Escape or the scrim).
pub fn answer(answer: QuitAnswer, cx: &mut App) {
    if cx.try_global::<Asking>().is_none() {
        return;
    }
    cx.remove_global::<Asking>();
    cx.refresh_windows();
    NativeShell::finish_quit(answer, cx);
}

/// The quit dialog, drawn by the window that carries it (the roots of the app's and the instances window call this).
pub fn overlay(window: &Window, _cx: &App) -> Option<AnyElement> {
    let (id, dialog) = showing(_cx)?;
    if id != window.window_handle().window_id() {
        return None;
    }
    Some(element(dialog))
}

/// The Signal confirmation for `dialog`: [`ConfirmDialog`] for a question, a [`Modal`] with only the safe button for
/// "Dilla is still in use".
pub fn element(dialog: Dialog) -> AnyElement {
    let body = dialog.body();
    match dialog.quit_label() {
        Some(quit) => ConfirmDialog::new(
            DIALOG_ID,
            dialog.title(),
            body,
            quit,
            quit,
            dialog.stay_label(),
        )
        .tone(Tone::Danger)
        .on_confirm(|_, cx| answer(QuitAnswer::Quit, cx))
        .on_close(|_, cx| answer(QuitAnswer::Stay, cx))
        .into_any_element(),
        None => {
            // ConfirmDialog's head and body without its confirm button.
            let description = div().flex().flex_col().children(paragraph(
                &body,
                text::UI,
                line_height(text::UI, 1.5),
                color::FG_2,
                440.0 - 36.0,
            ));
            Modal::new(DIALOG_ID, dialog.title(), dialog.stay_label())
                .kind(DialogKind::Confirm)
                .description(description)
                .a11y_description(body)
                .body(div())
                .initial_focus(InitialFocus::Close)
                .on_close(|_, cx| answer(QuitAnswer::Stay, cx))
                .into_any_element()
        }
    }
}

/// The active window, else the oldest window the shell knows, else any.
fn host_window(cx: &App) -> Option<AnyWindowHandle> {
    let windows = cx.windows();
    let by_id = |id: WindowId| windows.iter().copied().find(|w| w.window_id() == id);
    cx.active_window()
        .or_else(|| {
            NativeShell::open_windows(cx)
                .into_iter()
                .find_map(|(id, _)| by_id(id))
        })
        .or_else(|| windows.first().copied())
}

#[cfg(test)]
mod tests {
    use gpui::{Context, Render, TestAppContext, VisualTestContext};

    use super::*;

    fn in_call() -> GuardState {
        GuardState {
            active_call: true,
            ..GuardState::IDLE
        }
    }

    fn drafting() -> GuardState {
        GuardState {
            unsaved_draft: true,
            ..GuardState::IDLE
        }
    }

    /// A window root that draws the quit overlay, as the app's and the instances window's roots do.
    struct Root;

    impl Render for Root {
        fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
            div().size_full().children(overlay(window, cx))
        }
    }

    /// The quit dialogs installed, one window drawing the overlay, and `guards` registered in order.
    fn setup(cx: &mut TestAppContext, guards: Vec<GuardState>) -> WindowId {
        cx.update(crate::install);
        let window = cx.add_window(|_, _| Root);
        cx.update(|cx| {
            install(cx);
            for (n, g) in guards.into_iter().enumerate() {
                NativeShell::add_quit_guard(InstanceId(n as u8), Box::new(move |_| g), cx);
            }
        });
        window.window_id()
    }

    fn quit(cx: &mut TestAppContext, reason: QuitReason) {
        cx.update(|cx| NativeShell::request_quit_for(reason, cx));
        cx.run_until_parked();
    }

    fn showing_now(cx: &mut TestAppContext) -> Option<Dialog> {
        cx.update(|cx| showing(cx)).map(|(_, d)| d)
    }

    fn reply(cx: &mut TestAppContext, a: QuitAnswer) {
        cx.update(|cx| answer(a, cx));
        cx.run_until_parked();
    }

    fn quitting(cx: &mut TestAppContext) -> bool {
        cx.update(|cx| NativeShell::is_quitting(cx))
    }

    /// beta.5: "Quit Dilla?" with a call on, in the window that has the focus; Cancel keeps the app (and the next Quit
    /// asks again), Quit quits.
    #[gpui::test]
    fn active_call_asks_before_quitting(cx: &mut TestAppContext) {
        let window = setup(cx, vec![GuardState::IDLE, in_call()]);
        quit(cx, QuitReason::User);
        assert_eq!(
            cx.update(|cx| showing(cx)),
            Some((window, Dialog::InUse)),
            "a call asks first"
        );
        reply(cx, QuitAnswer::Stay);
        assert!(!quitting(cx));
        assert_eq!(showing_now(cx), None);
        assert!(!cx.update(|cx| NativeShell::is_asking_quit(cx)));
        quit(cx, QuitReason::User);
        assert_eq!(showing_now(cx), Some(Dialog::InUse));
        reply(cx, QuitAnswer::Quit);
        assert!(quitting(cx));
    }

    /// Unsent text asks the same question.
    #[gpui::test]
    fn unsaved_draft_asks(cx: &mut TestAppContext) {
        setup(cx, vec![drafting()]);
        quit(cx, QuitReason::User);
        assert_eq!(showing_now(cx), Some(Dialog::InUse));
        reply(cx, QuitAnswer::Quit);
        assert!(quitting(cx));
    }

    /// beta.5 `unknownGuard`: a provider that did not answer counts as in use and asks "Dilla is not responding"
    /// first, before (and instead of) "Quit Dilla?".
    #[gpui::test]
    fn silent_window_counts_as_in_use(cx: &mut TestAppContext) {
        setup(cx, vec![in_call(), GuardState::UNANSWERED]);
        quit(cx, QuitReason::User);
        assert_eq!(showing_now(cx), Some(Dialog::NotResponding));
        reply(cx, QuitAnswer::Stay);
        assert!(!quitting(cx));
        quit(cx, QuitReason::User);
        reply(cx, QuitAnswer::Quit);
        assert!(quitting(cx));
        assert_eq!(showing_now(cx), None, "one dialog per request");
    }

    /// The dialog is drawn in its window as the Signal confirmation: a modal alert dialog named by its title, its safe
    /// button first in the tab order; Escape keeps the app.
    #[gpui::test]
    fn the_dialog_is_a_modal_signal_confirmation(cx: &mut TestAppContext) {
        let window = setup(cx, vec![in_call()]);
        quit(cx, QuitReason::User);
        let handle = cx
            .windows()
            .into_iter()
            .find(|w| w.window_id() == window)
            .unwrap();
        let mut vcx = VisualTestContext::from_window(handle, cx);
        vcx.run_until_parked();
        vcx.simulate_keystrokes("escape");
        vcx.run_until_parked();
        assert_eq!(showing_now(cx), None, "Escape answers Stay");
        assert!(!quitting(cx));
    }

    /// The window carrying the question closing answers Stay, so a later Quit asks again.
    #[gpui::test]
    fn closing_the_dialogs_window_keeps_the_app(cx: &mut TestAppContext) {
        let window = setup(cx, vec![in_call()]);
        cx.update(|cx| NativeShell::window_opened(window, None, cx));
        quit(cx, QuitReason::User);
        assert!(showing_now(cx).is_some());
        cx.update(|cx| NativeShell::window_closed(window, cx));
        assert_eq!(showing_now(cx), None);
        assert!(!cx.update(|cx| NativeShell::is_asking_quit(cx)));
        assert!(!quitting(cx));
    }

    #[test]
    fn dialogs_come_in_beta5_order() {
        let req = |reason, guards: Vec<GuardState>| QuitRequest {
            reason,
            guards: guards
                .into_iter()
                .enumerate()
                .map(|(n, g)| (InstanceId(n as u8), g))
                .collect(),
        };
        use QuitReason::{UpdateRestart, User};
        assert_eq!(
            Dialog::for_request(&req(User, vec![GuardState::IDLE])),
            None
        );
        assert_eq!(Dialog::for_request(&req(User, vec![])), None);
        assert_eq!(
            Dialog::for_request(&req(User, vec![in_call(), GuardState::UNANSWERED])),
            Some(Dialog::NotResponding)
        );
        assert_eq!(
            Dialog::for_request(&req(User, vec![drafting()])),
            Some(Dialog::InUse)
        );
        assert_eq!(
            Dialog::for_request(&req(UpdateRestart, vec![in_call()])),
            Some(Dialog::InUse)
        );
        assert_eq!(
            Dialog::for_request(&req(UpdateRestart, vec![GuardState::UNANSWERED])),
            Some(Dialog::StillInUse)
        );
        assert_eq!(Dialog::StillInUse.quit_label(), None);
        assert_eq!(Dialog::InUse.stay_label(), copy::CANCEL);
        assert_eq!(
            Dialog::NotResponding.body(),
            "A Dilla window has not reported whether it is in use. Quitting may end a call or discard text you have not sent."
        );
    }

    /// An instance without a running client has nothing to lose; each instance gets one guard however many windows
    /// it opens.
    #[gpui::test]
    fn an_instance_gets_one_guard_and_idle_without_a_client(cx: &mut TestAppContext) {
        let a = cx
            .add_empty_window()
            .update(|w, _| w.window_handle().window_id());
        let b = cx
            .add_empty_window()
            .update(|w, _| w.window_handle().window_id());
        cx.update(|cx| {
            install(cx);
            NativeShell::window_opened(a, Some(InstanceId(3)), cx);
            NativeShell::window_moved(Some(a), b, InstanceId(3), cx);
            NativeShell::window_opened(a, None, cx);
            let request = NativeShell::collect_guards(QuitReason::User, cx);
            assert_eq!(request.guards, [(InstanceId(3), GuardState::IDLE)]);
        });
    }
}
