//! The reactions facet of a fixture: `AppState::reactions` (who reacted). The counts on timeline items are already set by
//! the fixture's messages. Owned by Task 10; no other file maps it.

use dilla_client::state::reactions::Reactors;
use dilla_client::state::{AppState, UserId};
use dilla_client::testing::spec::WorldSpec;

/// Maps the facet into `state`: for every reaction of every message, the people who reacted, in the fixture's order.
/// A person is found by username among the members the state already holds, so a reactor who is in no server the viewer
/// sees has no face (the count still counts them).
pub(super) fn apply(world: &WorldSpec, _viewer: &str, state: &mut AppState) {
    let user_of = |state: &AppState, person: &str| -> Option<UserId> {
        let username = world
            .people
            .iter()
            .find(|p| p.id == person)
            .map_or(person, |p| p.username());
        state
            .members
            .values()
            .flatten()
            .find(|m| m.username == username)
            .map(|m| m.user_id)
    };
    let mut reactors = Vec::new();
    for m in &world.messages {
        for r in &m.reactions {
            reactors.push(Reactors {
                message: m.key.clone(),
                emoji: r.emoji.clone(),
                users: r.by.iter().filter_map(|p| user_of(state, p)).collect(),
            });
        }
    }
    state.reactions.reactors = reactors;
}
