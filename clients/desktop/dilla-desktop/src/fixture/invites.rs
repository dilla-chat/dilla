//! The invites facet of a server-settings fixture: `AppState::server_admin.invites`. Owned by Task 15; no other file maps it.
//!
//! The parity world's community set-up leaves one link live: 64 uses for an hour, made by the owner, used once by each
//! member who joined. A fixture named in [`extra_links`] has the owner make more (`seed/invites.rs` of the parity world
//! makes the same).

use dilla_client::state::AppState;
use dilla_client::state::server_admin::{Invite, InviteId};

/// How long the set-up link has left when the frame is taken: the hour, less the history's few seconds of host clock.
const SETUP_LINK_LEFT_S: u64 = 3570;

/// The links a fixture makes besides the set-up one: `(max uses, ttl seconds)` (the same table as the world's seed).
fn extra_links(name: &str) -> &'static [(u64, u64)] {
    if name == "server-settings/invites-b" {
        &[(10, 604_800), (1, 3600)]
    } else {
        &[]
    }
}

pub(super) fn apply(name: &str, now: u64, state: &mut AppState) {
    // The parity world's dillad listens on a port of its own; the text is masked, only its width matters.
    state.server_admin.host = "127.0.0.1:45525".to_owned();
    let communities = state.communities.clone();
    for community in communities {
        let joined = state
            .members
            .get(&community.id)
            .map_or(0, |m| m.len().saturating_sub(1)) as u64;
        let mut rows = vec![Invite {
            id: InviteId([1; 16]),
            created_by: Some(community.owner),
            max_uses: 64,
            uses: joined,
            created: now,
            expires: now + SETUP_LINK_LEFT_S,
        }];
        for (i, (uses, ttl)) in extra_links(name).iter().enumerate() {
            rows.push(Invite {
                id: InviteId([2 + u8::try_from(i).unwrap_or(0); 16]),
                created_by: Some(community.owner),
                max_uses: *uses,
                uses: 0,
                created: now,
                expires: now + ttl - 30,
            });
        }
        state.server_admin.invites.insert(community.id, rows);
    }
}
