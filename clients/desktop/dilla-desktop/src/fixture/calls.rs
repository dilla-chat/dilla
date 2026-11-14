//! The calls facet of a fixture: `AppState::voice` and `AppState::ring`, from `WorldSpec::calls`. Owned by Tasks 17 and 18; no other file maps it.
//!
//! The facet gets the state the other facets built, so it finds each call's channel and person there: the channel by
//! its `<server>/<channel>` path's last part within the server of that name, the person by their account name. A
//! person's device id is derived from their account (one device each); the viewer's is the account's own. The viewer
//! in a call is in it: the call is this device's.
//!
//! A DM's call (`dm/<id>`, the DM the dms facet made) is the parity world's seeding as the web client receives it: with
//! the viewer in it and nobody else, the viewer's call rings the DM's other members (the outgoing ring, as the instance
//! answers a start); without the viewer, its first other participant rings the viewer (an incoming ring, busy while the
//! viewer is in another call).

use dilla_client::calls::CallPhase;
use dilla_client::state::ring::{AfkSnapshot, IncomingRing, Outgoing, RING};
use dilla_client::state::voice::{OwnCall, VoiceParticipant, flags};
use dilla_client::state::{AppState, ChannelId, DeviceId, UserId};
use dilla_client::testing::spec::WorldSpec;

/// api/afk.go `afkDefaultTimeout`: a new server's AFK timeout, seconds.
const AFK_DEFAULT_TIMEOUT: u64 = 300;

/// Maps the facet into `state`.
pub(super) fn apply(world: &WorldSpec, viewer: &str, ids: super::Ids<'_>, state: &mut AppState) {
    // Each server's AFK snapshot, as the web client reads it: its built-in AFK channel (the system voice channel), the
    // default timeout and nobody away.
    for server in &world.servers {
        let Some(afk) = server.channels.iter().find(|c| c.system) else {
            continue;
        };
        let community = ids.community(&server.id);
        if state.communities.iter().any(|c| c.id == community) {
            state.ring.afk.insert(
                community,
                AfkSnapshot {
                    channel: Some(ids.channel(&format!("{}/{}", server.id, afk.id))),
                    timeout_seconds: AFK_DEFAULT_TIMEOUT,
                    ..AfkSnapshot::default()
                },
            );
        }
    }
    for call in &world.calls {
        let dm = call.channel.strip_prefix("dm/");
        let channel = match dm {
            Some(local) => ids
                .map
                .get(&format!("dm:{local}"))
                .map(|id| ChannelId(*id))
                .filter(|id| state.dms.get(*id).is_some()),
            None => channel_of(world, state, &call.channel),
        };
        let Some(channel) = channel else {
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
        if dm.is_some() {
            ring_of(world, call, channel, &rows, state);
        }
        state.voice.place(channel, Some(channel.0), rows);
    }
}

/// A DM call's ring: the viewer's own call rings the DM's other members while nobody else is in it; a call without the
/// viewer rings the viewer, from its first participant.
fn ring_of(
    world: &WorldSpec,
    call: &dilla_client::testing::spec::CallSpec,
    channel: ChannelId,
    rows: &[VoiceParticipant],
    state: &mut AppState,
) {
    let Some(members) = state.dms.get(channel).map(|d| d.members.clone()) else {
        return;
    };
    let own = state.account.user_id;
    if rows.iter().any(|r| r.own) {
        if rows.iter().all(|r| r.own) {
            state.ring.outgoing = Some(Outgoing {
                channel: Some(channel),
                ringing: members.into_iter().filter(|m| Some(*m) != own).collect(),
                ..Outgoing::default()
            });
        }
        return;
    }
    let Some(caller) = rows.first().map(|r| r.user_id) else {
        return;
    };
    let caller_name = call
        .participants
        .first()
        .and_then(|p| world.people.iter().find(|q| q.id == p.person))
        .map_or_else(String::new, |p| p.display.clone());
    state.ring.rings.push(IncomingRing {
        call_id: channel.0,
        channel,
        caller,
        caller_name,
        ring_ts: 0,
        expires_at: tokio::time::Instant::now() + RING,
        busy: state.voice.call.phase != CallPhase::Idle,
        quiet: false,
    });
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
