//! Seeds the invites of a server-settings fixture into the real dillad. Owned by Task 15; no other file does.
//!
//! The fixture schema has no invites field, so the facet is keyed by the fixture's name: `server-settings/invites-b` has
//! the owner of its first server make two more links (10 uses for 7 days, and one use for an hour) beside the link the
//! world's own community set-up leaves live (64 uses, an hour). `dilla-desktop/src/fixture/invites.rs` builds the same
//! rows on the GPUI side.

use std::time::Duration;

use dilla_client::state::CommunityId;
use dilla_client::testing::host::{Person, start, wait_for};
use dilla_client::testing::spec::ScreenFixture;

/// The extra links a fixture makes: `(max uses, ttl seconds)`.
pub fn extra_links(name: &str) -> &'static [(u64, u64)] {
    if name == "server-settings/invites-b" {
        &[(10, 604_800), (1, 3600)]
    } else {
        &[]
    }
}

/// Nothing is refused: the facet is always seedable.
pub(super) fn unseedable(_f: &ScreenFixture, _out: &mut Vec<String>) {}

/// Seeds the facet once the history is posted.
pub(super) async fn seed(f: &ScreenFixture, people: &[(String, Person)]) {
    let links = extra_links(&f.name);
    let Some(server) = f.world.servers.first().filter(|_| !links.is_empty()) else {
        return;
    };
    let Some((_, owner)) = people.iter().find(|(id, _)| *id == server.owner) else {
        return;
    };
    let client = start(owner).await;
    let state = wait_for(&client, Duration::from_secs(30), "the server", |s| {
        s.communities.iter().any(|c| c.name == server.name)
    })
    .await;
    let community: CommunityId = state
        .communities
        .iter()
        .find(|c| c.name == server.name)
        .expect("listed")
        .id;
    for (uses, ttl) in links {
        client
            .server_admin()
            .create_invite(community, *uses, *ttl)
            .wait()
            .await
            .expect("the owner makes a link");
    }
    client.stop();
}
