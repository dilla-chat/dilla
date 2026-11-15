//! `dilla://` links (Package 6; beta.5 `policy.ts` `parseInvite`, `openInvite`, `confirmInstance`, `open-url`).
//!
//! To do (Package 6): [`parse`] accepts exactly `dilla://invite/<host[:port]>/<code>`; [`prepare`] registers
//! `Application::on_open_urls` (its callback gets no app: hand the parsed links to the UI thread, then
//! [`super::NativeShell::open_link`]); [`install`] sets the [`super::NativeShell::on_link`] handler, which confirms an
//! unknown instance and opens join-with-invite in the right instance window. The shell already queues the links that
//! come before the handler, and drops every link while the app asks to quit or quits.

use gpui::{App, Application};

use super::Link;

/// One command-line argument or URL as a link, or `None` when it is not exactly the invite grammar. Task 0: never a
/// link.
pub fn parse(_raw: &str) -> Option<Link> {
    None
}

/// Called once by [`super::prepare`], before the run loop. Task 0: nothing yet.
pub fn prepare(_app: &Application) {}

/// Called once by [`super::install`]. Task 0: nothing yet.
pub fn install(_cx: &mut App) {}
