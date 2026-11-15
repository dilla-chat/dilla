//! `reactions`: Task 10's slice of [`super::AppState`]: who reacted to which message (the tooltip's list).
//!
//! The core's timeline rows carry only a count and a `mine` flag per emoji (`ReactionSummary`), so the live client has no
//! reactor list of its own: the slice is filled by whoever knows one (a parity fixture's world today). The tooltip draws
//! the names it has and, for the rest of the count, no faces. Nothing here holds key material.

use super::UserId;

/// Who reacted with one emoji to one message.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Reactors {
    /// The message's key in its timeline (`TimelineItem::key`).
    pub message: String,
    pub emoji: String,
    /// In the order they reacted.
    pub users: Vec<UserId>,
}

/// The reactor lists the client knows.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct ReactionState {
    pub reactors: Vec<Reactors>,
}

impl ReactionState {
    /// The users who reacted to `message` with `emoji`; empty when none is known.
    pub fn who(&self, message: &str, emoji: &str) -> &[UserId] {
        self.reactors
            .iter()
            .find(|r| r.message == message && r.emoji == emoji)
            .map_or(&[], |r| r.users.as_slice())
    }
}
