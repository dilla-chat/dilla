//! The polls facet of a fixture: `AppState::bot` (polls), from `MessageSpec::poll`. Owned by Task 20; no other file maps it.
//!
//! A poll is the Bot's verified record of the message that carries it, keyed by that message's id (the timeline item the
//! world already holds, found by its key). What the spec does not say is the web's: the poll was made by the viewer
//! (the card offers Close poll), it runs 24 hours from its post, one answer each unless two votes come from one person,
//! and a voter's name is its display name.

use dilla_client::state::AppState;
use dilla_client::state::bot::PollView;
use dilla_client::testing::spec::{self, WorldSpec};

/// The duration a fixture's poll runs (the form's default, 24h).
const DURATION: u64 = 86_400;

/// Maps the facet into `state`.
pub(super) fn apply(world: &WorldSpec, viewer: &str, state: &mut AppState) {
    let Some(me) = state.account.user_id else {
        return;
    };
    let display = |id: &str| {
        world
            .people
            .iter()
            .find(|p| p.id == id)
            .map_or_else(|| id.to_owned(), |p| p.display.clone())
    };
    for m in world.messages.iter() {
        let Some(p) = &m.poll else { continue };
        let Some(item) = state
            .timelines
            .values()
            .flat_map(|t| t.items.iter())
            .find(|i| i.key == m.key)
        else {
            continue;
        };
        let (Some(msg_id), Some(channel)) = (
            item.msg_id,
            state
                .timelines
                .iter()
                .find(|(_, t)| t.items.iter().any(|i| i.key == m.key))
                .map(|(c, _)| *c),
        ) else {
            continue;
        };
        let mut counts = vec![0u64; p.options.len()];
        let mut previews = vec![Vec::new(); p.options.len()];
        let mut voters: Vec<&str> = Vec::new();
        for v in &p.votes {
            if let Some(c) = counts.get_mut(v.option) {
                *c += 1;
                previews[v.option].push(display(&v.by));
            }
            if !voters.contains(&v.by.as_str()) {
                voters.push(&v.by);
            }
        }
        let multi = voters.len() < p.votes.len();
        let mine: Vec<usize> = p
            .votes
            .iter()
            .filter(|v| v.by == viewer)
            .map(|v| v.option)
            .collect();
        state.bot.polls.insert(
            msg_id,
            PollView {
                id: spec::derive_id("poll", &m.key),
                channel,
                thread: None,
                creator: me,
                question: p.question.clone(),
                answers: p.options.clone(),
                multi,
                created_at: item.ts,
                due_at: item.ts + DURATION,
                closed: p.closed,
                counts,
                my_choices: Some(mine),
                revision: 1,
                previews: previews
                    .into_iter()
                    .map(|v| v.into_iter().take(5).collect())
                    .collect(),
                distinct: voters.len() as u64,
                capped: false,
            },
        );
    }
}
