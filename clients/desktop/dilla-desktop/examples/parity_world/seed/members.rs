//! Seeds the statuses, bios and Bot accounts of a fixture into the real dillad. Owned by Task 14; no other file does.
//!
//! A person's status and bio are their own profile's: each such person saves them through the app's own `Profile` handle
//! before the browser signs in (the web client reads the viewer's profile at sign-in), so every reference shows them.

use dilla_client::features::profile::ProfilePatch;
use dilla_client::testing::host::{self as world, Person};
use dilla_client::testing::spec::ScreenFixture;

/// Names the facet when the fixture asks for it and this world cannot seed it yet: a Bot account (it needs a running
/// Bot).
pub(super) fn unseedable(f: &ScreenFixture, out: &mut Vec<String>) {
    if f.world.people.iter().any(|p| p.bot) {
        out.push("Bot accounts".to_owned());
    }
}

/// Saves every person's status and bio, before the browser signs in.
pub(super) async fn before_sign_in(f: &ScreenFixture, people: &[(String, Person)]) {
    for p in f
        .world
        .people
        .iter()
        .filter(|p| !p.status.is_empty() || !p.bio.is_empty())
    {
        let person = &people
            .iter()
            .find(|(id, _)| *id == p.id)
            .expect("validated")
            .1;
        let client = world::start(person).await;
        let patch = ProfilePatch {
            display: None,
            status_msg: (!p.status.is_empty()).then(|| p.status.clone()),
            bio: (!p.bio.is_empty()).then(|| p.bio.clone()),
        };
        client
            .profile()
            .update(patch)
            .await
            .expect("the client runs")
            .unwrap_or_else(|e| panic!("{}'s status and bio: {e}", p.id));
        client.stop();
        world::clear_session(person);
    }
}

/// Seeds the facet once the history is posted: nothing more (statuses and bios are saved before the sign-in).
#[allow(clippy::unused_async)]
pub(super) async fn seed(_f: &ScreenFixture, _people: &[(String, Person)]) {}
