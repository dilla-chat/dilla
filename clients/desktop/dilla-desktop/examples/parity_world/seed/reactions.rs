//! Seeds the reactions of a fixture into the real dillad. Owned by Task 10; no other file does.
//!
//! Every reactor reacts through the world's live client of that person (the viewer's, or a poster's: a client that joined
//! the channel's group before the history was posted, so it holds the message). Reactions go one at a time, in the
//! fixture's order, each awaited until the reactor's own timeline shows it, so the chips come in the order the file lists
//! them (the core orders chips by the first reaction of each emoji).

use std::time::Duration;

use dilla_client::Client;
use dilla_client::state::{ChannelId, MessageId};
use dilla_client::testing::host::{self as world, Person};
use dilla_client::testing::spec::{MessageSpec, ScreenFixture};

/// Names the facet when the fixture asks for it and this world cannot seed it: a reactor with no client (not the viewer
/// and never a sender) has no group to react in, and a message is found by its body, so those must differ.
pub(super) fn unseedable(f: &ScreenFixture, out: &mut Vec<String>) {
    let w = &f.world;
    let clients = f.seed_plan().clients;
    for m in w.messages.iter().filter(|m| !m.reactions.is_empty()) {
        for r in &m.reactions {
            for by in &r.by {
                if !clients.contains(by) {
                    out.push(format!(
                        "reactions: {by} reacts on {} but holds no client (only the viewer and the senders do)",
                        m.key
                    ));
                }
            }
        }
        let same = w
            .messages
            .iter()
            .filter(|o| o.channel == m.channel && o.body == m.body)
            .count();
        if same > 1 {
            out.push(format!(
                "reactions: message {} shares its body with another of its channel (a message is found by its body)",
                m.key
            ));
        }
    }
}

/// The channel and the message id of `m` as `client` holds them.
async fn locate(f: &ScreenFixture, client: &Client, m: &MessageSpec) -> (ChannelId, MessageId) {
    let (server, channel) = f.world.channel(&m.channel).expect("validated channel");
    let (server_name, channel_name) = (server.name.clone(), channel.name.clone());
    let body = m.body.clone();
    let state = world::wait_for(client, Duration::from_secs(30), "the message", move |s| {
        let community = s.communities.iter().find(|c| c.name == server_name);
        community
            .and_then(|c| s.channel_named(c.id, &channel_name))
            .and_then(|ch| s.timelines.get(&ch.id))
            .is_some_and(|t| t.items.iter().any(|i| i.body == body && i.msg_id.is_some()))
    })
    .await;
    let community = state
        .communities
        .iter()
        .find(|c| c.name == server.name)
        .expect("the server");
    let channel = state
        .channel_named(community.id, &channel.name)
        .expect("the channel");
    let msg = state.timelines[&channel.id]
        .items
        .iter()
        .find(|i| i.body == m.body && i.msg_id.is_some())
        .and_then(|i| i.msg_id)
        .expect("the message id");
    (channel.id, msg)
}

/// Seeds the facet once the history is posted, with the live clients keyed by person id.
pub(super) async fn seed(
    f: &ScreenFixture,
    _people: &[(String, Person)],
    clients: &[(&str, Client)],
) {
    for m in f.world.messages.iter().filter(|m| !m.reactions.is_empty()) {
        for r in &m.reactions {
            for by in &r.by {
                let client = &clients
                    .iter()
                    .find(|(id, _)| id == by)
                    .expect("a reactor holds a client (`unseedable` refuses the rest)")
                    .1;
                let (channel, msg) = locate(f, client, m).await;
                client
                    .reactions()
                    .react(channel, msg, &r.emoji, true)
                    .await
                    .expect("the answer arrives")
                    .expect("the reaction is queued");
                let emoji = r.emoji.clone();
                world::wait_for(client, Duration::from_secs(30), "the reaction", move |s| {
                    s.timelines
                        .get(&channel)
                        .and_then(|t| t.items.iter().find(|i| i.msg_id == Some(msg)))
                        .is_some_and(|i| i.reactions.iter().any(|x| x.emoji == emoji && x.mine))
                })
                .await;
            }
        }
    }
    // The viewer's own client holds every count before the browser is asked to show them.
    let want: Vec<(String, String, usize)> = f
        .world
        .messages
        .iter()
        .flat_map(|m| {
            m.reactions
                .iter()
                .map(|r| (m.body.clone(), r.emoji.clone(), r.by.len()))
        })
        .collect();
    let viewer = &clients[0].1;
    world::wait_for(
        viewer,
        Duration::from_secs(60),
        "every reaction",
        move |s| {
            let items = s.timelines.values().flat_map(|t| t.items.iter());
            let items: Vec<_> = items.collect();
            want.iter().all(|(body, emoji, n)| {
                items.iter().any(|i| {
                    &i.body == body
                        && i.reactions
                            .iter()
                            .any(|x| &x.emoji == emoji && x.count as usize == *n)
                })
            })
        },
    )
    .await;
    // And a moment for the browser, which hears the same folds over its own gateway.
    tokio::time::sleep(Duration::from_secs(3)).await;
}
