//! What tests and the parity harness share (Task 5), behind the `testing` features and never in a shipping build.
//!
//! - [`spec`] (feature `testing`): the screen fixture schema, one JSON file both shooters read: the web shooter seeds a
//!   real dillad from it and shoots Chromium; the GPUI automation build draws the same world without a network.
//! - `host` (feature `testing-host`): a real dillad (dilla-testhost), accounts made the way the app makes them,
//!   communities, started clients and state waits: the helpers `tests/sync.rs`, `examples/peer.rs`, the desktop's
//!   `tests/support/world.rs` and `examples/parity_world.rs` used to carry each their own copy of.

pub mod spec;

#[cfg(feature = "testing-host")]
pub mod host;

use crate::state::{AppState, TimelineItem};

/// Every decrypted text `state` holds: the timelines' and threads' bodies, reply excerpts and attachment names, the
/// pins' and the reply bar's excerpts, the search's query and snippets, the tray's file names, the polls and reminders.
/// R21 keeps all of them out of `AppState`'s Debug output, so a test that asks "is this plaintext still anywhere in the
/// state?" reads them here rather than searching `{:?}`.
pub fn decrypted_texts(state: &AppState) -> Vec<&str> {
    fn item<'a>(i: &'a TimelineItem, out: &mut Vec<&'a str>) {
        out.push(&i.body);
        if let Some(r) = &i.reply {
            out.push(&r.excerpt);
        }
        out.extend(i.attachments.iter().map(|a| a.name.as_str()));
    }
    let mut out = Vec::new();
    for t in state.timelines.values() {
        t.items.iter().for_each(|i| item(i, &mut out));
    }
    for d in state.threads.threads.values() {
        d.root
            .iter()
            .chain(&d.replies)
            .for_each(|i| item(i, &mut out));
    }
    out.extend(state.threads.replying.iter().map(|r| r.excerpt.as_str()));
    for pins in state.messages.pins.values() {
        out.extend(pins.iter().map(|p| p.excerpt.as_str()));
    }
    out.push(&state.search.query);
    out.extend(state.search.hits.iter().map(|h| h.snippet.as_str()));
    for tray in state.attachments.trays.values() {
        out.extend(tray.iter().map(|t| t.name.as_str()));
    }
    for poll in state.bot.polls.values() {
        out.push(&poll.question);
        out.extend(poll.answers.iter().map(String::as_str));
    }
    for reminders in state.bot.reminders.values() {
        out.extend(reminders.iter().map(|r| r.text.as_str()));
    }
    for rows in state.bot.private_results.values() {
        out.extend(rows.iter().map(|r| r.text.as_str()));
    }
    out
}

/// Whether `needle` is in any of `state`'s [`decrypted_texts`].
pub fn holds_text(state: &AppState, needle: &str) -> bool {
    decrypted_texts(state).iter().any(|t| t.contains(needle))
}
