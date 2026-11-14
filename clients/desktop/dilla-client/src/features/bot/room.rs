//! A music command's room (Task 28 wave 2, lane C): packages/web/src/screens/commands/bot-run.ts sends a music command
//! with the voice room this device is in as its channel ("Music has no text channel"), and CommandHost.tsx names that
//! room in the picker's bar ("… · {room}", else "join voice first").

use crate::state::{AppState, ChannelId, CommunityId};

/// picker-catalog.ts `MUSIC_COMMANDS`: the commands whose channel is the voice room and whose bar names it.
pub const MUSIC_COMMANDS: [&str; 16] = [
    "play",
    "pause",
    "resume",
    "skip",
    "stop",
    "queue",
    "nowplaying",
    "seek",
    "volume",
    "shuffle",
    "repeat",
    "queue remove",
    "queue move",
    "queue clear",
    "music",
    "music help",
];

/// Whether `id` is a music command.
pub fn is_music(id: &str) -> bool {
    MUSIC_COMMANDS.contains(&id)
}

/// The channel of this device's call (`getCall().channelId`): set while it joins or is in one.
fn call_channel(state: &AppState) -> Option<ChannelId> {
    state
        .voice
        .in_call()
        .then_some(state.voice.call.channel)
        .flatten()
}

/// bot-run.ts `callChannel ?? context.binding.channelId`: the channel a Bot command `id` typed in `here` is sent with.
/// A music command goes with the voice room this device is in, when it is in one of `here`'s community (w2-bot-review
/// M-C5: a call in another community is never sealed to this community's Bot); everything else with `here`.
pub fn send_channel(state: &AppState, id: &str, here: ChannelId) -> ChannelId {
    let community = state.channel(here).map(|c| c.community_id);
    if is_music(id)
        && let Some(room) = call_channel(state)
        && community.is_some()
        && state.channel(room).map(|c| c.community_id) == community
    {
        return room;
    }
    here
}

/// CommandHost.tsx `room`: the name of the voice room this device is in, among `community`'s channels; `None` when it
/// is in no call there (the bar then says "join voice first").
pub fn room_name(state: &AppState, community: Option<CommunityId>) -> Option<String> {
    let room = call_channel(state)?;
    state
        .channel(room)
        .filter(|c| Some(c.community_id) == community)
        .map(|c| c.name.clone())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::calls::CallPhase;
    use crate::state::{Channel, ChannelGroupState};

    const COMMUNITY: CommunityId = CommunityId([1; 16]);
    const GENERAL: ChannelId = ChannelId([2; 16]);
    const LOUNGE: ChannelId = ChannelId([3; 16]);

    fn channel(id: ChannelId, community: CommunityId, kind: u8, name: &str) -> Channel {
        Channel {
            id,
            community_id: community,
            kind,
            mode: 0,
            name: name.into(),
            topic: String::new(),
            parent_id: None,
            position: 0,
            slowmode: 0,
            text_group_id: None,
            group: ChannelGroupState::Unsupported,
        }
    }

    fn state(phase: CallPhase) -> AppState {
        let mut s = AppState::default();
        s.channels.push(channel(GENERAL, COMMUNITY, 0, "general"));
        s.channels.push(channel(LOUNGE, COMMUNITY, 1, "Lounge"));
        s.voice.call.phase = phase;
        s.voice.call.channel = Some(LOUNGE);
        s.voice.call.community = Some(COMMUNITY);
        s
    }

    /// bot-run.ts: `/play` and the other music commands go with the voice room this device is in; any other command,
    /// or a music command with no call, goes with the text channel it was typed in.
    #[test]
    fn a_music_command_is_sent_with_the_voice_room_this_device_is_in() {
        let in_call = state(CallPhase::In);
        assert_eq!(send_channel(&in_call, "play", GENERAL), LOUNGE);
        assert_eq!(send_channel(&in_call, "queue clear", GENERAL), LOUNGE);
        assert_eq!(send_channel(&in_call, "remind", GENERAL), GENERAL);
        assert_eq!(
            send_channel(&state(CallPhase::Joining), "stop", GENERAL),
            LOUNGE,
            "a joining call is the call"
        );
        assert_eq!(
            send_channel(&state(CallPhase::Idle), "play", GENERAL),
            GENERAL
        );
        // w2-bot-review M-C5: typed in another community's channel, the call here is not that community's room.
        let mut other = state(CallPhase::In);
        let elsewhere = ChannelId([8; 16]);
        other
            .channels
            .push(channel(elsewhere, CommunityId([9; 16]), 0, "elsewhere"));
        assert_eq!(send_channel(&other, "play", elsewhere), elsewhere);
    }

    /// CommandHost.tsx: the bar names the call's room when it is one of this community's channels.
    #[test]
    fn the_room_is_the_calls_channel_in_this_community() {
        let s = state(CallPhase::In);
        assert_eq!(room_name(&s, Some(COMMUNITY)).as_deref(), Some("Lounge"));
        assert_eq!(room_name(&s, Some(CommunityId([9; 16]))), None);
        assert_eq!(room_name(&state(CallPhase::Idle), Some(COMMUNITY)), None);
    }
}
