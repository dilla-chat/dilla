//! Seeds the dms of a fixture into the real dillad. Owned by Task 13; lane G (Task 28 wave 2) made the one-to-one DMs a
//! call or a ring needs.
//!
//! A one-to-one DM of the viewer is opened by the viewer's headless client before the browser signs in, so the route can
//! name it (`{dm/<id>}`) and the browser, a device the viewer adds later, is taken into its group as the viewer's other
//! devices are: the viewer's client stays online while the browser is shot ([`keep_online`]). A group DM, a DM without
//! the viewer and a DM's history are not seeded yet.

use std::collections::BTreeMap;
use std::time::Duration;

use dilla_client::Client;
use dilla_client::state::UserId;
use dilla_client::testing::host::{self as world, Person};
use dilla_client::testing::spec::{self, ScreenFixture};

/// Names the facet when the fixture asks for what this world cannot seed yet.
pub(super) fn unseedable(f: &ScreenFixture, out: &mut Vec<String>) {
    let w = &f.world;
    let pair_of_viewer = |d: &spec::DmSpec| d.members.len() == 2 && d.members.contains(&f.viewer);
    let history = w.messages.iter().any(|m| m.channel.starts_with("dm/"));
    if w.dms.iter().any(|d| !pair_of_viewer(d)) || history {
        out.push("dms (a group DM, a DM without the viewer, or a DM's history)".to_owned());
    }
}

/// Opens each of the viewer's DMs from the viewer's client (the first of `clients`) and records its id as `dm:<id>`.
pub(super) async fn before_browser(
    f: &ScreenFixture,
    clients: &[(&str, Client)],
    ids: &mut BTreeMap<String, String>,
) {
    let Some((_, viewer)) = clients.iter().find(|(id, _)| *id == f.viewer) else {
        return;
    };
    for dm in &f.world.dms {
        let other = dm
            .members
            .iter()
            .find(|m| **m != f.viewer)
            .expect("validated pair");
        let user = UserId(spec::unhex16(&ids[&format!("person:{other}")]).expect("hex"));
        let channel = viewer
            .dms()
            .open_dm(user)
            .await
            .expect("the DM answer")
            .expect("the DM opens");
        world::wait_for(viewer, Duration::from_secs(30), "the DM", move |s| {
            s.dms.get(channel).is_some()
        })
        .await;
        ids.insert(format!("dm:{}", dm.id), world::hex(&channel.0));
    }
}

/// The viewer's client stays online while the browser is shot when the world has DMs: the browser, a device added after
/// the DM was made, joins the DM's group when it opens the DM, and a member device that is online admits it.
pub(super) fn keep_online(f: &ScreenFixture, person: &str) -> bool {
    person == f.viewer && !f.world.dms.is_empty()
}

/// Nothing more once the history is posted.
#[allow(clippy::unused_async)]
pub(super) async fn seed(_f: &ScreenFixture, _people: &[(String, Person)]) {}
