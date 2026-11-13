//! The calls facet of a fixture: `AppState::voice` and `AppState::ring`, from `WorldSpec::calls`. Owned by Tasks 17 and 18; no other file maps it.
//!
//! The facet gets the state the other facets built, so it finds each call's channel and person there: the channel by
//! its `<server>/<channel>` path's last part within the server of that name, the person by their account name. A
//! person's device id is derived from their account (one device each); the viewer's is the account's own. The viewer
//! in a call is in it: the call is this device's.

use dilla_client::calls::CallPhase;
use dilla_client::state::voice::{OwnCall, VoiceParticipant, flags};
use dilla_client::state::{AppState, ChannelId, DeviceId, UserId};
use dilla_client::testing::spec::WorldSpec;

/// Maps the facet into `state`.
pub(super) fn apply(world: &WorldSpec, viewer: &str, state: &mut AppState) {
    for call in &world.calls {
        let Some(channel) = channel_of(world, state, &call.channel) else {
            continue;
        };
        let mut rows = Vec::new();
        for p in &call.participants {
            let Some(user) = user_of(world, state, &p.person) else {
                continue;
            };
            let own = p.person == viewer;
            let device = if own {
                state
                    .account
                    .device_id
                    .unwrap_or(DeviceId(device_bytes(user)))
            } else {
                DeviceId(device_bytes(user))
            };
            let mut f = flags::IN_CALL;
            if p.muted {
                f |= flags::SELF_MUTE;
            }
            if p.deafened {
                f |= flags::SELF_DEAF | flags::SELF_MUTE;
            }
            if p.sharing {
                f |= flags::SCREEN;
            }
            if own {
                state.voice.self_mute = p.muted;
                state.voice.self_deaf = p.deafened;
                state.voice.share.screen = p.sharing;
                state.voice.call = OwnCall {
                    phase: CallPhase::In,
                    channel: Some(channel),
                    community: state.channel(channel).map(|c| c.community_id),
                    call_id: Some(channel.0),
                    ..OwnCall::default()
                };
            }
            rows.push(VoiceParticipant {
                user_id: user,
                device_id: device,
                flags: f,
                own,
            });
        }
        state.voice.place(channel, Some(channel.0), rows);
    }
}

/// A person's one device: their account id with the last byte flipped.
fn device_bytes(user: UserId) -> [u8; 16] {
    let mut d = user.0;
    d[15] ^= 0xd7;
    d
}

fn channel_of(world: &WorldSpec, state: &AppState, path: &str) -> Option<ChannelId> {
    let (server, name) = path.split_once('/')?;
    let server_name = world
        .servers
        .iter()
        .find(|s| s.id == server)
        .map(|s| s.name.as_str())?;
    let community = state.communities.iter().find(|c| c.name == server_name)?.id;
    let channel_name = world
        .servers
        .iter()
        .find(|s| s.id == server)?
        .channels
        .iter()
        .find(|c| c.id == name)
        .map_or(name, |c| c.name.as_str());
    state.channel_named(community, channel_name).map(|c| c.id)
}

fn user_of(world: &WorldSpec, state: &AppState, person: &str) -> Option<UserId> {
    let spec = world.people.iter().find(|p| p.id == person)?;
    let username = spec.username.as_deref().unwrap_or(&spec.id);
    state
        .members
        .values()
        .flatten()
        .find(|m| m.username == username)
        .map(|m| m.user_id)
}
