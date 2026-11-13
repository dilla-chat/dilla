//! The server settings facet of a fixture: what the viewer may manage (`AppState::server_admin`), derived from who owns
//! each server, and the built-in AFK channel's settings. Owned by Task 15; no other file maps it.
//!
//! A fixture has no roles of this area to grant rights through (the roles facet is Task 14's): the owner may manage
//! everything, any other member has the rights of a member with no role. The AFK timeout is the instance's default.

use dilla_client::features::server_admin::{AFK_BUILTIN_POSITION, AFK_DEFAULT_TIMEOUT};
use dilla_client::state::AppState;
use dilla_client::state::server_admin::AfkSettings;
use dilla_client::testing::spec::WorldSpec;

/// Maps the facet into `state`.
pub(super) fn apply(_world: &WorldSpec, _viewer: &str, state: &mut AppState) {
    let account = state.account.user_id;
    let communities = state.communities.clone();
    for community in communities {
        let owner = Some(community.owner) == account;
        // The one access slice (final review I2): the roles facet's resolved access when it mapped this server, else
        // the owner's or a plain member's.
        state.roles.access.entry(community.id).or_insert_with(|| {
            let mut access = dilla_client::features::roles::rank::access_of(0, 0, owner);
            access.overwrites_loaded = true;
            access
        });
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
