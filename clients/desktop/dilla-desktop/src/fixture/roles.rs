//! The roles facet of a fixture: `AppState::roles`, from `ServerSpec::roles`. Owned by Task 14; no other file maps it.
//!
//! The parity world makes each server's roles in the order the spec lists them, each at the lowest free position (the
//! owner's `createRole`), so the n-th role sits at position n. Hoist and mentionable are what the world makes: off. The
//! viewer's access is what the instance would answer: everything for the owner, else the resolved bits of the roles
//! held, ranked by the highest of them.

use std::collections::HashMap;

use dilla_client::features::roles::perm::{self, PERM_ALL, PermInput, PermKey, PermRole};
use dilla_client::features::roles::rank::{self, OWNER_TOP};
use dilla_client::state::roles::RoleSummary;
use dilla_client::state::{AppState, CommunityId};
use dilla_client::testing::spec::{WorldSpec, derive_id};

/// The bits of `@everyone` on a server the world makes (internal/api/communities.go `everyoneRoleAllow`).
pub(super) const EVERYONE_DEFAULT: u32 = perm::VIEW_CHANNEL
    | perm::SEND_MESSAGES
    | perm::ATTACH_FILES
    | perm::ADD_REACTIONS
    | perm::READ_HISTORY
    | perm::CONNECT
    | perm::SPEAK
    | perm::VIDEO
    | perm::SCREEN_SHARE
    | perm::CREATE_INVITE;

/// A permission name of the spec: the copy key (`kick`) or the wire name (`kick_members`).
pub(super) fn bit_of(name: &str) -> Option<u32> {
    if let Some(k) = PermKey::ALL.iter().find(|k| k.key() == name) {
        return Some(k.bit());
    }
    Some(match name {
        "view_channel" => perm::VIEW_CHANNEL,
        "send_messages" => perm::SEND_MESSAGES,
        "manage_messages" => perm::MANAGE_MESSAGES,
        "pin_messages" => perm::PIN_MESSAGES,
        "attach_files" => perm::ATTACH_FILES,
        "add_reactions" => perm::ADD_REACTIONS,
        "read_history" => perm::READ_HISTORY,
        "screen_share" => perm::SCREEN_SHARE,
        "create_invite" => perm::CREATE_INVITE,
        "kick_members" => perm::KICK_MEMBERS,
        "ban_members" => perm::BAN_MEMBERS,
        "manage_channels" => perm::MANAGE_CHANNELS,
        "manage_roles" => perm::MANAGE_ROLES,
        "manage_community" => perm::MANAGE_COMMUNITY,
        "view_audit_log" => perm::VIEW_AUDIT_LOG,
        "mention_everyone" => perm::MENTION_EVERYONE,
        "bypass_slowmode" => perm::BYPASS_SLOWMODE,
        "administrator" => perm::ADMINISTRATOR,
        "mute_members" => perm::MUTE_MEMBERS,
        "move_members" => perm::MOVE_MEMBERS,
        "manage_nicknames" => perm::MANAGE_NICKNAMES,
        "moderate_members" => perm::MODERATE_MEMBERS,
        "control_music" => perm::CONTROL_MUSIC,
        _ => return None,
    })
}

/// `#rrggbb` as 0xRRGGBB.
pub(super) fn colour_of(spec: Option<&str>) -> u32 {
    spec.and_then(|c| u32::from_str_radix(c.trim_start_matches('#'), 16).ok())
        .unwrap_or(0)
        & 0xff_ffff
}

/// Maps the facet into `state`.
///
/// The ids come from the state the shell's mapping built (the effective ids, a shot's included): each of the viewer's
/// servers is its community of the same name (the state's order is not the spec's: a shot's ids sort it), people by
/// username, and a role by a member holding it (the members' role ids are in the spec's role order). A role nobody holds
/// keeps its derived id.
pub(super) fn apply(world: &WorldSpec, viewer: &str, state: &mut AppState) {
    let derived = world.ids();
    let everyone_of = |server: &str| derive_id("role", &format!("{server}/@everyone"));
    let pairs: Vec<_> = world
        .servers
        .iter()
        .filter(|s| s.members.iter().any(|m| m == viewer))
        .filter_map(|s| {
            let community: CommunityId = state.communities.iter().find(|c| c.name == s.name)?.id;
            Some((s, community))
        })
        .collect();
    for (server, community) in pairs {
        let members = state.members.get(&community).cloned().unwrap_or_default();
        let user_of = |person: &str| {
            let username = world
                .people
                .iter()
                .find(|p| p.id == person)
                .map_or(person, |p| p.username());
            members
                .iter()
                .find(|m| m.username == username)
                .map(|m| m.user_id.0)
                .unwrap_or_else(|| derived[&format!("person:{person}")])
        };
        let role_id = |i: usize| -> [u8; 16] {
            let r = &server.roles[i];
            r.members
                .first()
                .and_then(|holder| {
                    let username = world.people.iter().find(|p| p.id == *holder)?.username();
                    let m = members.iter().find(|m| m.username == username)?;
                    let k = server.roles[..i]
                        .iter()
                        .filter(|o| o.members.contains(holder))
                        .count();
                    m.role_ids.get(k).copied()
                })
                .unwrap_or_else(|| derived[&format!("role:{}/{}", server.id, r.id)])
        };
        let everyone = everyone_of(&server.id);
        let mut rows = vec![RoleSummary {
            id: everyone,
            name: "@everyone".into(),
            color: 0,
            position: 0,
            hoist: false,
            mentionable: false,
            allow: EVERYONE_DEFAULT,
            deny: 0,
            members: server.members.len() as u32,
        }];
        for (i, r) in server.roles.iter().enumerate() {
            rows.push(RoleSummary {
                id: role_id(i),
                name: r.name.clone(),
                color: colour_of(r.colour.as_deref()),
                position: i as u32 + 1,
                hoist: false,
                mentionable: false,
                allow: r
                    .permissions
                    .iter()
                    .filter_map(|p| bit_of(p))
                    .fold(0, |a, b| a | b)
                    & PERM_ALL,
                deny: 0,
                members: r.members.len() as u32,
            });
        }
        let owner = server.owner == viewer;
        let held: Vec<[u8; 16]> = server
            .roles
            .iter()
            .enumerate()
            .filter(|(_, r)| r.members.iter().any(|m| m == viewer))
            .map(|(i, _)| role_id(i))
            .collect();
        let perm_roles: Vec<PermRole> = rows
            .iter()
            .map(|r| PermRole {
                id: r.id,
                position: u64::from(r.position),
                allow: r.allow,
                deny: r.deny,
            })
            .collect();
        let viewer_id = user_of(viewer);
        let owner_id = user_of(&server.owner);
        let bits = perm::resolve_community(
            &PermInput {
                owner: owner_id,
                everyone_id: everyone,
                roles: &perm_roles,
                held: &held,
            },
            viewer_id,
        );
        let top = if owner {
            OWNER_TOP
        } else {
            rank::top_of(&held, &rows)
        };
        let mut access = rank::access_of(u64::from(bits), top, owner);
        access.everyone_id = Some(everyone);
        state.roles.roles.insert(community, rows);
        state.roles.access.insert(community, access);
        state.roles.overwrites.insert(community, HashMap::new());
    }
}
