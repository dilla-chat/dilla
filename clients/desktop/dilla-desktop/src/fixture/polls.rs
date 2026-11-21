//! The polls facet of a fixture: `AppState::bot` (polls), from `MessageSpec::poll`. Owned by Task 20; no other file maps it.
//!
//! A poll is the Bot's verified record of the message that carries it, keyed by that message's id (the timeline item the
//! world already holds, found by its key). What the spec does not say is the web's: the poll was made by the viewer
//! (the card offers Close poll), it runs 24 hours from its post, one answer each unless two votes come from one person,
//! and a voter's name is its display name.

use dilla_client::features::roles::viewer_channel_bits;
use dilla_client::state::AppState;
use dilla_client::state::bot::{
    ArgShape, ArgType, BotCatalogView, BotCommandSpec, BotStatus, BotStatusState, PollView,
};
use dilla_client::state::bot_admin::sealed_operation;
use dilla_client::testing::spec::{self, ChannelKind, WorldSpec};

use super::Ids;

/// The duration a fixture's poll runs (the form's default, 24h).
const DURATION: u64 = 86_400;

/// The polls area's command of the Bot's catalog (bot-catalog.fixture.ts): `poll create` with its arguments, under the
/// sealed operation of v1 operation 70 that tells the handler the Bot offers poll creation (which opens the poll form).
fn commands() -> Vec<BotCommandSpec> {
    let spec = |name: &str, operation: u32, args: &[(&str, ArgType)]| BotCommandSpec {
        name: name.to_owned(),
        description: format!("{name} command"),
        operation,
        args: args
            .iter()
            .map(|&(name, ty)| ArgShape {
                name: name.to_owned(),
                ty,
                required: false,
            })
            .collect(),
        permission_bit: 0,
        needs_bot_channel: false,
        handler_kind: 0,
    };
    vec![spec(
        "poll create",
        u32::try_from(sealed_operation(70)).unwrap_or(0),
        &[
            ("question", ArgType::Text),
            ("answers", ArgType::Text),
            ("multiple", ArgType::Bool),
            ("duration", ArgType::Duration),
        ],
    )]
}

/// The Bot's catalog of a server whose Bot has the polls capability, as the web reads it once the Bot is enabled: ready,
/// with the Bot person the server's members hold as its user, the spec's Bot channels (the channel list marks them) and
/// the polls commands. A catalog another area made keeps what it has.
fn catalog(world: &WorldSpec, ids: Ids<'_>, state: &mut AppState) {
    for server in &world.servers {
        let Some(bot) = server
            .bot
            .as_ref()
            .filter(|b| b.capabilities.iter().any(|c| c == "polls"))
        else {
            continue;
        };
        let community = ids.community(&server.id);
        let user = world
            .people
            .iter()
            .find(|p| p.bot && server.members.contains(&p.id))
            .map(|p| ids.user(&p.id));
        let channels = bot
            .bot_channels
            .iter()
            .map(|c| ids.channel(&format!("{}/{c}", server.id)))
            .collect();
        let held = state
            .bot
            .catalogs
            .entry(community)
            .or_insert_with(|| BotCatalogView {
                revision: 1,
                commands: Vec::new(),
                status: BotStatus {
                    state: BotStatusState::Ready,
                    since: None,
                },
                bot_channels: None,
                bot_user: None,
                fingerprint: None,
            });
        held.bot_user = held.bot_user.or(user);
        held.bot_channels.get_or_insert(channels);
        for command in commands() {
            if !held.commands.iter().any(|c| c.name == command.name) {
                held.commands.push(command);
            }
        }
        // The viewer's resolved bits in each text channel, which the shell hands the threads area as a conversation opens (a
        // fixture has no shell loop to do it): the form opens only where the viewer may send.
        for channel in server
            .channels
            .iter()
            .filter(|c| c.kind == ChannelKind::Text)
        {
            let id = ids.channel(&format!("{}/{}", server.id, channel.id));
            if let Some(bits) = viewer_channel_bits(state, id) {
                state.threads.channel_bits.insert(id, bits);
            }
        }
    }
}

/// Maps the Bot's side of the facet into `state`, once the roles that resolve the viewer's channel bits are in.
pub(super) fn apply_bot(world: &WorldSpec, ids: Ids<'_>, state: &mut AppState) {
    catalog(world, ids, state);
}

/// Maps the facet into `state`.
pub(super) fn apply(world: &WorldSpec, viewer: &str, state: &mut AppState) {
    let Some(me) = state.account.user_id else {
        return;
    };
    let display = |id: &str| {
        world
            .people
            .iter()
            .find(|p| p.id == id)
            .map_or_else(|| id.to_owned(), |p| p.display.clone())
    };
    for m in world.messages.iter() {
        let Some(p) = &m.poll else { continue };
        // `threads::apply` runs after this one: a thread's reply is still a row of its channel's timeline, found by its key.
        // A reply is held for the thread of its root (`thread_of`), so the card reads it as the web's thread poll.
        let Some((channel, item)) = state
            .timelines
            .iter()
            .find_map(|(c, t)| t.items.iter().find(|i| i.key == m.key).map(|i| (*c, i)))
        else {
            continue;
        };
        let Some(msg_id) = item.msg_id else { continue };
        let thread = m.thread_of.as_deref().and_then(|root| {
            state
                .timelines
                .get(&channel)?
                .items
                .iter()
                .find(|i| i.key == root)?
                .msg_id
        });
        let mut counts = vec![0u64; p.options.len()];
        let mut previews = vec![Vec::new(); p.options.len()];
        let mut voters: Vec<&str> = Vec::new();
        for v in &p.votes {
            if let Some(c) = counts.get_mut(v.option) {
                *c += 1;
                previews[v.option].push(display(&v.by));
            }
            if !voters.contains(&v.by.as_str()) {
                voters.push(&v.by);
            }
        }
        let multi = voters.len() < p.votes.len();
        let mine: Vec<usize> = p
            .votes
            .iter()
            .filter(|v| v.by == viewer)
            .map(|v| v.option)
            .collect();
        state.bot.polls.insert(
            msg_id,
            PollView {
                id: spec::derive_id("poll", &m.key),
                channel,
                thread,
                creator: me,
                question: p.question.clone(),
                answers: p.options.clone(),
                multi,
                created_at: item.ts,
                due_at: item.ts + DURATION,
                closed: p.closed,
                counts,
                my_choices: Some(mine),
                revision: 1,
                previews: previews
                    .into_iter()
                    .map(|v| v.into_iter().take(5).collect())
                    .collect(),
                distinct: voters.len() as u64,
                capped: false,
            },
        );
    }
}

#[cfg(test)]
mod tests {
    use std::path::Path;

    use crate::fixture::{Fixture, FixtureScreen};

    /// A poll message that is a reply in a thread is held with that thread (the root's message id) and its channel, and a
    /// poll of the log with none; the two share nothing.
    #[test]
    fn a_poll_posted_in_a_thread_is_held_for_that_thread() {
        let path = Path::new(env!("CARGO_MANIFEST_DIR")).join("../parity/fixtures/polls/card.json");
        let mut spec: serde_json::Value =
            serde_json::from_str(&std::fs::read_to_string(&path).unwrap()).unwrap();
        let messages = spec["world"]["messages"].as_array_mut().unwrap();
        let root = messages
            .iter()
            .find(|m| m.get("poll").is_none())
            .expect("a plain message")["key"]
            .as_str()
            .unwrap()
            .to_owned();
        let poll = messages
            .iter()
            .position(|m| m.get("poll").is_some())
            .expect("a poll message");
        let poll_key = messages[poll]["key"].as_str().unwrap().to_owned();
        // The log's poll, held as it is.
        let plain = Fixture::parse(&spec.to_string(), None).unwrap();
        let FixtureScreen::Shell { channel } = plain.screen else {
            panic!("shell")
        };
        let id_of = |f: &Fixture, key: &str| {
            f.state.timelines[&channel]
                .items
                .iter()
                .find(|i| i.key == key)
                .and_then(|i| i.msg_id)
        };
        let log_id = id_of(&plain, &poll_key).unwrap();
        assert_eq!(plain.state.bot.polls[&log_id].thread, None);
        // The same poll as a reply of the first message's thread.
        spec["world"]["messages"][poll]["thread_of"] = serde_json::Value::String(root.clone());
        let threaded = Fixture::parse(&spec.to_string(), None).unwrap();
        let root_id = id_of(&threaded, &root).unwrap();
        let held = threaded
            .state
            .threads
            .threads
            .get(&root_id)
            .and_then(|d| d.replies.iter().find(|i| i.key == poll_key))
            .and_then(|i| i.msg_id)
            .expect("the reply is in its thread");
        assert_eq!(threaded.state.bot.polls[&held].thread, Some(root_id));
        assert_eq!(threaded.state.bot.polls[&held].channel, channel);
    }
}
