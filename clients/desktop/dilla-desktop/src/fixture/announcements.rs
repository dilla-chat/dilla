//! The announcements facet of a fixture (Task 28): what `/announcement …` needs of a server whose Bot has the
//! announcements capability: the Bot's command catalog in `AppState::bot` (the typed commands the picker lists, and the
//! app operation 150 that tells the handler the Bot offers announcements), and the viewer's resolved bits in each text
//! channel, which the shell hands the threads area as a conversation opens (a fixture has no shell loop to do it).
//!
//! It is the Bot's side of the world, as [`super::server_admin::apply_bot`] is the installation's: a server with no `bot`
//! facet, or without the capability, has none of it, as before. The Bot is as the catalog the worker publishes
//! (`bot-catalog.fixture.ts`): ready, with the Bot person the server's members hold as its user.

use dilla_client::features::roles::viewer_channel_bits;
use dilla_client::state::AppState;
use dilla_client::state::bot::{
    ArgShape, ArgType, BotCatalogView, BotCommandSpec, BotStatus, BotStatusState,
};
use dilla_client::state::bot_admin::sealed_operation;
use dilla_client::testing::spec::{ChannelKind, WorldSpec};

use super::Ids;

/// The capability bit of announcements (`BOT_CAPABILITIES`).
const ANNOUNCEMENTS: &str = "announcements";

/// bot-catalog.fixture.ts: the commands of the announcements area, with the argument shapes the Bot declares.
fn commands() -> Vec<BotCommandSpec> {
    let spec =
        |name: &str, operation: u64, args: &[(&str, bool)], needs_bot_channel| BotCommandSpec {
            name: name.to_owned(),
            description: format!("{name} command"),
            operation: u32::try_from(operation).unwrap_or(0),
            args: args
                .iter()
                .map(|&(name, required)| ArgShape {
                    name: name.to_owned(),
                    ty: ArgType::Text,
                    required,
                })
                .collect(),
            permission_bit: 0,
            needs_bot_channel,
            handler_kind: 0,
        };
    vec![
        spec("announcement list", 50, &[], false),
        spec("announcement create", 51, &[("text", false)], true),
        spec(
            "announcement edit",
            51,
            &[("announcement", true), ("text", true)],
            false,
        ),
        spec("announcement delete", 52, &[("announcement", true)], false),
        // What the handler asks the catalog for (`catalogOffers(…, 50)`): the Bot's list operation, sealed.
        spec("announcement rows", sealed_operation(50), &[], false),
    ]
}

/// Maps the facet into `state`.
pub(super) fn apply(world: &WorldSpec, ids: Ids<'_>, state: &mut AppState) {
    for server in &world.servers {
        let Some(bot) = &server.bot else {
            continue;
        };
        if !bot.capabilities.iter().any(|c| c == ANNOUNCEMENTS) {
            continue;
        }
        let community = ids.community(&server.id);
        state.bot.catalogs.insert(
            community,
            BotCatalogView {
                revision: 1,
                commands: commands(),
                status: BotStatus {
                    state: BotStatusState::Ready,
                    since: None,
                },
                bot_channels: Some(
                    bot.bot_channels
                        .iter()
                        .map(|c| ids.channel(&format!("{}/{c}", server.id)))
                        .collect(),
                ),
                bot_user: world
                    .people
                    .iter()
                    .find(|p| p.bot && server.members.contains(&p.id))
                    .map(|p| ids.user(&p.id)),
                fingerprint: None,
            },
        );
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
