//! The server settings facet of a fixture: what the viewer may manage (`AppState::server_admin`), derived from who owns
//! each server, and the built-in AFK channel's settings. Owned by Task 15; no other file maps it.
//!
//! A fixture has no roles of this area to grant rights through (the roles facet is Task 14's): the owner may manage
//! everything, any other member has the rights of a member with no role: `@everyone`'s, which on a new server include
//! making invites (the web lists Invites for such a member). The AFK timeout is the instance's default.

use dilla_client::features::server_admin::{AFK_BUILTIN_POSITION, AFK_DEFAULT_TIMEOUT, access_of};
use dilla_client::state::AppState;
use dilla_client::state::server_admin::{AdminRole, AfkSettings};
use dilla_client::testing::spec::WorldSpec;

/// Maps the facet into `state`.
pub(super) fn apply(_world: &WorldSpec, _viewer: &str, state: &mut AppState) {
    let account = state.account.user_id;
    let communities = state.communities.clone();
    for community in communities {
        let owner = Some(community.owner) == account;
        let mut access = access_of(u64::from(super::roles::EVERYONE_DEFAULT), 0, owner);
        // The roles facet (applied before this one) made the server's roles; a new server's channels have no
        // overwrites. With both read the channel editor shows its permissions as the web's does, not an unread error.
        access.everyone = state
            .roles
            .access
            .get(&community.id)
            .and_then(|a| a.everyone_id);
        access.overwrites_loaded = true;
        state.server_admin.access.insert(community.id, access);
        let roles = state
            .roles
            .roles
            .get(&community.id)
            .map(|rows| {
                rows.iter()
                    .map(|r| AdminRole {
                        id: r.id,
                        name: r.name.clone(),
                        position: u64::from(r.position),
                        allow: r.allow,
                        deny: r.deny,
                    })
                    .collect()
            })
            .unwrap_or_default();
        state.server_admin.roles.insert(community.id, roles);
        state
            .server_admin
            .overwrites
            .insert(community.id, Vec::new());
        let channel = state
            .channels
            .iter()
            .find(|c| c.community_id == community.id && c.position == AFK_BUILTIN_POSITION)
            .map(|c| c.id);
        state.server_admin.afk.insert(
            community.id,
            AfkSettings {
                channel,
                timeout_seconds: AFK_DEFAULT_TIMEOUT,
            },
        );
    }
}
