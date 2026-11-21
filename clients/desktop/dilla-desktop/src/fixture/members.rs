//! The members facet of a fixture: `AppState::people`, the presence the members pane draws, and the viewer's own profile.
//! Owned by Task 14; no other file maps it.
//!
//! The parity world stops every headless client before the browser shoots, so only the viewer is connected: the viewer
//! is idle (connected, with no input: internal/gateway counts only real input on the current wire) and everyone else
//! offline, as the instance's presence snapshot says then (the web reference shows "idle" under the viewer).

use std::collections::HashMap;

use dilla_client::state::members::{BanSummary, Presence};
use dilla_client::state::profile::MyProfile;
use dilla_client::state::{AppState, CommunityId, UserId};
use dilla_client::testing::spec::WorldSpec;

/// Maps the facet into `state`. `now` is the frame's clock (the fixture's, lifted by the shot's shift): the parity world
/// makes each ban at that clock (`seed/members.rs`), so it was created then and ends `hours` later.
///
/// The ids come from the state the shell's mapping built (the effective ids, a shot's included), never re-derived here.
pub(super) fn apply(
    world: &WorldSpec,
    viewer: &str,
    ids: super::Ids<'_>,
    now: u64,
    state: &mut AppState,
) {
    for server in world
        .servers
        .iter()
        .filter(|s| !s.bans.is_empty() && s.members.iter().any(|m| m == viewer))
    {
        let community = ids.community(&server.id);
        let mut rows = Vec::new();
        for b in &server.bans {
            let user = ids.user(&b.user);
            // A ban removes its target's membership (internal/api/bans.go).
            if let Some(members) = state.members.get_mut(&community) {
                members.retain(|m| m.user_id != user);
            }
            let name = world
                .people
                .iter()
                .find(|p| p.id == b.user)
                .map_or_else(String::new, |p| {
                    if p.display.is_empty() {
                        p.username().to_owned()
                    } else {
                        p.display.clone()
                    }
                });
            rows.push(BanSummary {
                user_id: user,
                name,
                reason: b.reason.clone(),
                by_user: ids.user(&b.by),
                created: now,
                expires: b.hours.map(|h| now + u64::from(h) * 3600),
            });
        }
        state.people.bans.insert(community, rows);
    }
    // The viewer's own profile (`GET /v1/accounts/me`): their display name, status and bio, as the parity world saved them.
    if let Some(p) = world.people.iter().find(|p| p.id == viewer) {
        state.profile.me = Some(MyProfile {
            display: p.display.clone(),
            status_msg: p.status.clone(),
            bio: p.bio.clone(),
        });
    }
    let me: Option<UserId> = state.account.user_id;
    // A server's real Bot (`ServerSpec::bot`) stays connected to the gateway while the browser is shot, with no input: idle.
    let running_bots: Vec<(CommunityId, UserId)> = world
        .servers
        .iter()
        .filter(|s| s.bot.is_some())
        .flat_map(|s| {
            world
                .people
                .iter()
                .filter(|p| p.bot && s.members.contains(&p.id))
                .map(|p| (ids.community(&s.id), ids.user(&p.id)))
        })
        .collect();
    let mut all: Vec<(CommunityId, HashMap<UserId, Presence>)> = Vec::new();
    for (community, members) in &state.members {
        let presence = members
            .iter()
            .map(|m| {
                let p = if Some(m.user_id) == me || running_bots.contains(&(*community, m.user_id))
                {
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
