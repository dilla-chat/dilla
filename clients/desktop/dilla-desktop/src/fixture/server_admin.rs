//! The server settings facet of a fixture: what the viewer may manage (`AppState::server_admin`), derived from who owns
//! each server, and the built-in AFK channel's settings. Owned by Task 15; no other file maps it.
//!
//! A fixture has no roles of this area to grant rights through (the roles facet is Task 14's): the owner may manage
//! everything, any other member has the rights of a member with no role. The AFK timeout is the instance's default.

use dilla_client::features::server_admin::{AFK_BUILTIN_POSITION, AFK_DEFAULT_TIMEOUT, access_of};
use dilla_client::state::AppState;
use dilla_client::state::server_admin::{AfkSettings, GifRating, GifSettings};
use dilla_client::testing::spec::WorldSpec;

/// Maps the facet into `state`.
pub(super) fn apply(_world: &WorldSpec, _viewer: &str, state: &mut AppState) {
    let account = state.account.user_id;
    let communities = state.communities.clone();
    for community in communities {
        let owner = Some(community.owner) == account;
        let mut access = access_of(0, 0, owner);
        access.overwrites_loaded = true;
        state.server_admin.access.insert(community.id, access);
        let channel = state
            .channels
            .iter()
            .find(|c| c.community_id == community.id && c.position == AFK_BUILTIN_POSITION)
            .map(|c| c.id);
        // A server with no key and the instance's default rating (the GIF settings' "ready" state).
        state.server_admin.gifs.insert(
            community.id,
            GifSettings {
                on: false,
                rating: GifRating::Pg13,
            },
        );
        state.server_admin.afk.insert(
            community.id,
            AfkSettings {
                channel,
                timeout_seconds: AFK_DEFAULT_TIMEOUT,
            },
        );
    }
}
