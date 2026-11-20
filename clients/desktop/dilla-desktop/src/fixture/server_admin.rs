//! The server settings facet of a fixture: what the viewer may manage (`AppState::server_admin`), derived from who owns
//! each server, and the built-in AFK channel's settings. Owned by Task 15; no other file maps it.
//!
//! A fixture has no roles of this area to grant rights through (the roles facet is Task 14's): the owner may manage
//! everything, any other member has the rights of a member with no role: `@everyone`'s, which on a new server include
//! making invites (the web lists Invites for such a member). The AFK timeout is the instance's default.

use dilla_client::features::server_admin::{AFK_BUILTIN_POSITION, AFK_DEFAULT_TIMEOUT};
use dilla_client::state::AppState;
use dilla_client::state::bot_admin::{BotCatalog, CatalogState, Installation};
use dilla_client::state::server_admin::{AdminRole, AfkSettings};
use dilla_client::testing::spec::WorldSpec;

use super::Ids;

/// The Bot of each server that carries one (decision D9): a ready installation with the spec's capabilities, and the catalog
/// listing the spec's Bot channels, as the web client reads them once the Bot is enabled and its channels are marked. A
/// server without one has neither, as before.
pub(super) fn apply_bot(world: &WorldSpec, ids: Ids<'_>, state: &mut AppState) {
    for server in &world.servers {
        let Some(bot) = &server.bot else {
            continue;
        };
        let community = ids.community(&server.id);
        state.bot_admin.installation.insert(
            community,
            Installation {
                id: dilla_client::testing::spec::derive_id("installation", &server.id),
                state: 3,
                revision: 1,
                user_id: None,
                capabilities: bot.capability_bits(),
                updated_at: 0,
                restart_requested: 0,
                restart_handled: 0,
                version: None,
                music_volume: 50,
                music_idle_minutes: 15,
            },
        );
        state.bot_admin.catalog.insert(
            community,
            BotCatalog {
                state: CatalogState::Ready,
                since: None,
                fingerprint: None,
                bot_channels: Some(
                    bot.bot_channels
                        .iter()
                        .map(|c| ids.channel(&format!("{}/{c}", server.id)))
                        .collect(),
                ),
            },
        );
    }
}

/// Maps the facet into `state`.
pub(super) fn apply(_world: &WorldSpec, _viewer: &str, state: &mut AppState) {
    let account = state.account.user_id;
    let communities = state.communities.clone();
    for community in communities {
        let owner = Some(community.owner) == account;
        // The one access slice (final review I2): the roles facet's resolved access when it mapped this server (its
        // bits include `@everyone`'s), else the owner's or a plain member's with `@everyone`'s rights.
        state.roles.access.entry(community.id).or_insert_with(|| {
            dilla_client::features::roles::rank::access_of(
                u64::from(super::roles::EVERYONE_DEFAULT),
                0,
                owner,
            )
        });
        // The overwrites are read (the fixture sets none): the one overwrites slice holds the server (re-review N4).
        state.roles.overwrites.entry(community.id).or_default();
        // The channel editor lists the server's roles (the roles facet, applied before this one, made them).
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
