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
        let Some(item) = state
            .timelines
            .values()
            .flat_map(|t| t.items.iter())
            .find(|i| i.key == m.key)
        else {
            continue;
        };
        let (Some(msg_id), Some(channel)) = (
            item.msg_id,
            state
                .timelines
                .iter()
                .find(|(_, t)| t.items.iter().any(|i| i.key == m.key))
                .map(|(c, _)| *c),
        ) else {
            continue;
        };
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
                thread: None,
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
