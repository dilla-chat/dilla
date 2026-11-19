//! The built-in commands that act on another area (builtin.ts): what each does once it may run, after its question
//! (if it asks one, [`super::confirm`]) was answered. [`dilla_client::features::bot::plan_in`] names the command
//! ([`Builtin`]); Tasks 10, 11, 24 and 25 add their commands here, each finishing the pending row it was given with
//! [`dilla_client::features::bot::Bot::finish`].

use dilla_client::features::bot::Builtin;
use dilla_client::state::bot::Conversation;
use gpui::App;

use crate::slots::SlotCx;

/// Runs `run` for the pending private row `row` of `conversation`.
pub fn run(
    run: Builtin,
    _row: u64,
    _conversation: Conversation,
    _ctx: &SlotCx,
    _window: gpui::WindowId,
    _cx: &mut App,
) {
    match run {
        // Nothing to run: the placeholder only drives the plumbing.
        Builtin::Probe => {}
    }
}
