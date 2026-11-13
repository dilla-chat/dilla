//! The members facet of a fixture: `AppState::people`, the presence the members pane draws. Owned by Task 14; no other
//! file maps it.
//!
//! The parity world stops every headless client before the browser shoots, so only the viewer is connected: the viewer
//! is idle (connected, with no input: internal/gateway counts only real input on the current wire) and everyone else
//! offline, as the instance's presence snapshot says then (the web reference shows "idle" under the viewer).

use std::collections::HashMap;

use dilla_client::state::members::Presence;
use dilla_client::state::{AppState, CommunityId, UserId};
use dilla_client::testing::spec::WorldSpec;

/// Maps the facet into `state`.
///
/// The ids come from the state the shell's mapping built (the effective ids, a shot's included), never re-derived here.
pub(super) fn apply(_world: &WorldSpec, _viewer: &str, state: &mut AppState) {
    let me: Option<UserId> = state.account.user_id;
    let mut all: Vec<(CommunityId, HashMap<UserId, Presence>)> = Vec::new();
    for (community, members) in &state.members {
        let presence = members
            .iter()
            .map(|m| {
                let p = if Some(m.user_id) == me {
                    Presence::Idle
                } else {
                    Presence::Offline
                };
                (m.user_id, p)
            })
            .collect();
        all.push((*community, presence));
    }
    state.people.presence.extend(all);
}
