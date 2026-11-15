//! Seeds the GIF settings of a server-settings fixture into the real dillad. Owned by Task 15; no other file does.
//!
//! Keyed by the fixture's name (the schema has no GIF field): `server-settings/gifs-b` has a Giphy key set and the
//! rating `r`. `dilla-desktop/src/fixture/gifs.rs` builds the same state on the GPUI side.

use std::time::Duration;

use dilla_client::state::server_admin::GifRating;
use dilla_client::testing::host::{Person, start, wait_for};
use dilla_client::testing::spec::ScreenFixture;

/// The key a fixture sets (never drawn: the key is write-only), if any, and its rating.
pub fn configured(name: &str) -> Option<GifRating> {
    (name == "server-settings/gifs-b").then_some(GifRating::R)
}

pub(super) fn unseedable(_f: &ScreenFixture, _out: &mut Vec<String>) {}

pub(super) async fn seed(f: &ScreenFixture, people: &[(String, Person)]) {
    let Some(rating) = configured(&f.name) else {
        return;
    };
    let Some(server) = f.world.servers.first() else {
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
    let community = state
        .communities
        .iter()
        .find(|c| c.name == server.name)
        .expect("listed")
        .id;
    let admin = client.server_admin();
    admin
        .set_gif_key(
            community,
            zeroize::Zeroizing::new("fixtureKey1234567".to_owned()),
        )
        .wait()
        .await
        .expect("the key is set");
    admin
        .set_gif_rating(community, rating)
        .wait()
        .await
        .expect("the rating is set");
    client.stop();
}
