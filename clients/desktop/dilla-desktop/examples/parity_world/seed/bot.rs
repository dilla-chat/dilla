//! Seeds a server's Bot (decision D9, `ServerSpec::bot`) into the real dillad: the owner enables it with the spec's
//! capabilities and marks each of its Bot channels, so the web client's Bot pages read a ready installation and its
//! catalog, as the GPUI fixture's state says (`dilla-desktop/src/fixture/server_admin.rs` `apply_bot`). Owned by Task 38.
//!
//! A world with a Bot runs on the host that supervises dilla's real Bot (`start_bot_host_with_web`), whose clock follows
//! the wall clock: a Bot fixture draws no times.

use std::collections::BTreeMap;

use dilla_client::Client;
use dilla_client::state::{ChannelId, CommunityId};
use dilla_client::testing::host::bot::enable_bot;
use dilla_client::testing::spec::{ScreenFixture, unhex16};

/// Whether a server of the fixture carries a Bot (the host then supervises one).
pub(super) fn needs_bot(f: &ScreenFixture) -> bool {
    f.world.servers.iter().any(|s| s.bot.is_some())
}

pub(super) fn unseedable(_f: &ScreenFixture, _out: &mut Vec<String>) {}

/// Enables each server's Bot and marks its Bot channels, on the owner's headless client. A fixture with a poll has the
/// Bot enabled before the history is posted (`early`, since the poll is made through it); every other one after it.
pub(super) async fn seed(
    f: &ScreenFixture,
    clients: &[(&str, Client)],
    ids: &BTreeMap<String, String>,
    early: bool,
) {
    if super::polls::needs_bot_first(f) != early {
        return;
    }
    for server in &f.world.servers {
        let Some(bot) = &server.bot else {
            continue;
        };
        let owner = clients
            .iter()
            .find(|(id, _)| *id == server.owner)
            .map(|(_, c)| c)
            .unwrap_or_else(|| panic!("server {}: its owner has no running client", server.id));
        let community = unhex16(&ids[&format!("server:{}", server.id)]).expect("hex");
        enable_bot(owner, community, bot.capability_bits()).await;
        for channel in &bot.bot_channels {
            let real = unhex16(&ids[&format!("channel:{}/{channel}", server.id)]).expect("hex");
            owner
                .bot_admin()
                .mark_bot_channel(CommunityId(community), ChannelId(real))
                .await
                .expect("an answer")
                .unwrap_or_else(|e| panic!("server {}: marking `{channel}`: {e}", server.id));
        }
    }
}
