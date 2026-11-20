//! Seeds the dms of a fixture into the real dillad. Owned by Task 13; lane G (Task 28 wave 2) made the one-to-one DMs a
//! call or a ring needs.
//!
//! A one-to-one DM of the viewer is opened by the viewer's headless client before the browser signs in, so the route can
//! name it (`{dm/<id>}`) and the browser, a device the viewer adds later, is taken into its group as the viewer's other
//! devices are: the viewer's client stays online while the browser is shot ([`keep_online`]). A group DM (more than two
//! members, the viewer one of them) is made by the viewer's client with all the others as recipients. Every other
//! member who has a headless client of its own (it posts) lists the DM and joins its group before the history, so a
//! message is really its sender's. A DM's history is posted by the main loop through [`post`] (a DM is not a channel of a
//! community: `Client::send_text` refuses it). A DM without the viewer is not seeded.

use std::collections::BTreeMap;
use std::time::Duration;

use dilla_client::Client;
use dilla_client::state::{ChannelGroupState, ChannelId, MessageId, UserId};
use dilla_client::testing::host::{self as world, Person};
use dilla_client::testing::spec::{self, MessageSpec, ScreenFixture};

/// Names the facet when the fixture asks for what this world cannot seed yet.
pub(super) fn unseedable(f: &ScreenFixture, out: &mut Vec<String>) {
    if f.world.dms.iter().any(|d| !d.members.contains(&f.viewer)) {
        out.push("dms (a DM without the viewer)".to_owned());
    }
}

/// Makes each of the viewer's DMs from the viewer's client (the first of `clients`), records its id as `dm:<id>`, and
/// lets every other member that has a client list the DM and join its group.
pub(super) async fn before_browser(
    f: &ScreenFixture,
    clients: &[(&str, Client)],
    ids: &mut BTreeMap<String, String>,
) {
    let Some((_, viewer)) = clients.iter().find(|(id, _)| *id == f.viewer) else {
        return;
    };
    for dm in &f.world.dms {
        let others: Vec<UserId> = dm
            .members
            .iter()
            .filter(|m| **m != f.viewer)
            .map(|m| UserId(spec::unhex16(&ids[&format!("person:{m}")]).expect("hex")))
            .collect();
        let channel = if others.len() == 1 {
            viewer.dms().open_dm(others[0]).await
        } else {
            viewer.dms().open_group_dm(others).await
        }
        .expect("the DM answer")
        .expect("the DM opens");
        world::wait_for(viewer, Duration::from_secs(30), "the DM", move |s| {
            s.dms.get(channel).is_some()
        })
        .await;
        ids.insert(format!("dm:{}", dm.id), world::hex(&channel.0));
        // The viewer registers the group; the others find the DM in their list and join on its Welcome.
        viewer
            .dms()
            .open(channel)
            .await
            .expect("the open answer")
            .expect("the viewer registers the DM's group");
        for member in dm.members.iter().filter(|m| **m != f.viewer) {
            let Some((_, client)) = clients.iter().find(|(id, _)| id == member) else {
                continue;
            };
            client.dms().start();
            world::wait_for(
                client,
                Duration::from_secs(60),
                "the DM's group",
                move |s| {
                    s.dms
                        .get(channel)
                        .is_some_and(|d| d.group == ChannelGroupState::Active)
                },
            )
            .await;
        }
    }
}

/// Posts `m` when it is a DM's message: from its sender's own client, through the DM area. `None` for a channel's.
pub(super) async fn post(
    m: &MessageSpec,
    client: &Client,
    channel: ChannelId,
) -> Option<Result<MessageId, dilla_client::ClientError>> {
    m.channel.strip_prefix("dm/")?;
    Some(
        client
            .dms()
            .send_text(channel, m.body.clone())
            .await
            .unwrap_or(Err(dilla_client::ClientError::Stopped)),
    )
}

/// The viewer's client stays online while the browser is shot when the world has DMs: the browser, a device added after
/// the DM was made, joins the DM's group when it opens the DM, and a member device that is online admits it.
pub(super) fn keep_online(f: &ScreenFixture, person: &str) -> bool {
    person == f.viewer && !f.world.dms.is_empty()
}

/// Nothing more once the history is posted.
#[allow(clippy::unused_async)]
pub(super) async fn seed(_f: &ScreenFixture, _people: &[(String, Person)]) {}
