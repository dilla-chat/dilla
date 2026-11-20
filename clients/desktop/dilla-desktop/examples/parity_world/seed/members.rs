//! Seeds the statuses, bios, bans and Bot accounts of a fixture into the real dillad. Owned by Task 14; no other file
//! does.
//!
//! A person's status and bio are their own profile's: each such person saves them through the app's own `Profile` handle
//! before the browser signs in (the web client reads the viewer's profile at sign-in), so every reference shows them.

use std::time::Duration;

use dilla_client::Client;
use dilla_client::features::moderation::now_s;
use dilla_client::features::profile::ProfilePatch;
use dilla_client::state::{CommunityId, UserId};
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

/// Makes each server's bans at the frame's clock, so a ban was created then and ends `hours` later, as the GPUI mapping
/// (`fixture/members.rs`) draws it. Each goes through the moderation ban route as its `by`, on that person's running
/// client when there is one. The route takes an expiry by this device's clock (`moderation::now_s`), so the expiry
/// asked for is the host's frozen clock plus `hours`, read back and asked again should the device's second have turned
/// between the two reads. Nothing about the ban (its reason above all) is logged.
pub(super) async fn at_frame(
    f: &ScreenFixture,
    people: &[(String, Person)],
    clients: &[(&str, Client)],
    host: &world::Host,
) {
    let person = |id: &str| &people.iter().find(|(p, _)| p == id).expect("validated").1;
    for server in &f.world.servers {
        // A failure names the ban by its place in the spec only, never its people or reason.
        for (i, b) in server.bans.iter().enumerate() {
            let running = clients.iter().find(|(id, _)| *id == b.by).map(|(_, c)| c);
            let started = match running {
                Some(_) => None,
                None => Some(world::start(person(&b.by)).await),
            };
            let client = running.or(started.as_ref()).expect("one of the two");
            let name = server.name.clone();
            let state = world::wait_for(client, Duration::from_secs(30), "the server", |s| {
                s.communities.iter().any(|c| c.name == name)
            })
            .await;
            let community: CommunityId = state
                .communities
                .iter()
                .find(|c| c.name == server.name)
                .expect("listed")
                .id;
            let user = UserId(person(&b.user).user_id);
            let frame = tokio::task::block_in_place(|| world::clock_now(host));
            let expires = b.hours.map(|h| frame + u64::from(h) * 3600);
            for attempt in 0.. {
                assert!(
                    attempt < 3,
                    "server {} ban {i}: the expiry did not hold",
                    server.id
                );
                let seconds = expires.map(|at| at - now_s());
                let reason = b.reason.clone();
                client
                    .moderation()
                    .run(move |m| async move { m.ban(community, user, &reason, seconds).await })
                    .await
                    .unwrap_or_else(|e| panic!("server {} ban {i}: {}", server.id, e.code));
                let bans = client
                    .moderation()
                    .run(move |m| async move { m.load_bans(community).await })
                    .await
                    .unwrap_or_else(|e| panic!("bans of {}: {}", server.id, e.code));
                if bans
                    .iter()
                    .any(|row| row.user_id == user && row.expires == expires)
                {
                    break;
                }
            }
            if let Some(c) = started {
                c.stop();
                world::clear_session(person(&b.by));
            }
        }
    }
}
