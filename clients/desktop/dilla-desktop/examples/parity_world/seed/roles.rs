//! Seeds the roles of a fixture into the real dillad. Owned by Task 14; no other file does.
//!
//! The server's owner makes each role in the spec's order through the app's own `Roles` handle (so the n-th role is at
//! position n, with its colour and permissions, neither hoisted nor mentionable) and grants it to its members.

use std::time::Duration;

use dilla_client::state::{CommunityId, UserId};
use dilla_client::testing::host::{self as world, Person};
use dilla_client::testing::spec::ScreenFixture;

/// Names the facet when the fixture asks for it and this world cannot seed it yet: a permission name it does not know.
pub(super) fn unseedable(f: &ScreenFixture, out: &mut Vec<String>) {
    let unknown = f.world.servers.iter().flat_map(|s| &s.roles).any(|r| {
        r.permissions.iter().any(|p| bit_of(p).is_none())
            || r.colour.as_deref().is_some_and(|c| colour_of(c).is_none())
    });
    if unknown {
        out.push("roles with unknown permission names or colours".to_owned());
    }
}

fn bit_of(name: &str) -> Option<u32> {
    use dilla_client::features::roles::perm::PermKey;
    PermKey::ALL
        .iter()
        .find(|k| k.key() == name)
        .map(|k| k.bit())
}

fn colour_of(c: &str) -> Option<u32> {
    let hex = c.strip_prefix('#')?;
    (hex.len() == 6).then(|| u32::from_str_radix(hex, 16).ok())?
}

/// Seeds the facet once the history is posted, with the live clients keyed by person id.
pub(super) async fn seed(f: &ScreenFixture, people: &[(String, Person)]) {
    for server in f.world.servers.iter().filter(|s| !s.roles.is_empty()) {
        let person = |id: &str| &people.iter().find(|(p, _)| p == id).expect("validated").1;
        let owner = world::start(person(&server.owner)).await;
        let name = server.name.clone();
        let state = world::wait_for(&owner, Duration::from_secs(30), "the server", |s| {
            s.communities.iter().any(|c| c.name == name)
        })
        .await;
        let community: CommunityId = state
            .communities
            .iter()
            .find(|c| c.name == server.name)
            .expect("listed")
            .id;
        for r in &server.roles {
            let allow = r
                .permissions
                .iter()
                .filter_map(|p| bit_of(p))
                .fold(0, |a, b| a | b);
            let colour = r.colour.as_deref().and_then(colour_of).unwrap_or(0);
            let role = owner
                .roles()
                .create_role(community, &r.name, colour, false, false, allow)
                .await
                .unwrap_or_else(|e| panic!("role {}: {e}", r.name));
            for m in &r.members {
                owner
                    .roles()
                    .grant_role(community, UserId(person(m).user_id), role)
                    .await
                    .unwrap_or_else(|e| panic!("grant {} to {m}: {e}", r.name));
            }
        }
        drop(owner);
    }
}
