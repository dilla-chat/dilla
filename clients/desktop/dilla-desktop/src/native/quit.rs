//! The quit dialogs (Package 7; beta.5 `main.ts:281-333` `collectQuitGuards`, `requestQuit`).
//!
//! To do (Package 7): [`install`] sets [`super::NativeShell::set_quit_prompt`]. For a [`super::QuitRequest`] that is
//! not clear it shows, in beta.5's order: "Dilla is not responding" when [`super::QuitRequest::unanswered`] (Keep Dilla
//! open / Quit anyway), then "Quit Dilla?" when [`super::QuitRequest::in_use`] (Cancel / Quit), then for an update
//! restart "Finish your call and save your drafts…"; it ends with [`super::NativeShell::finish_quit`]. The guard
//! providers live in `screens/voice/` (call) and `screens/messages/` (drafts) and register through
//! [`super::NativeShell::add_quit_guard`].

use gpui::App;

/// Called once by [`super::install`]. Task 0: nothing yet (a quit request quits at once).
pub fn install(_cx: &mut App) {}
