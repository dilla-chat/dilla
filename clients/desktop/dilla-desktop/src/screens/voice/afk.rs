//! Task 10: this device's call activity, reported so a live conversation is not moved to AFK (Shell.tsx's AFK activity
//! effect). The clock lives on the server; this device reports only real input or its own speech: listening, another
//! window and another device do not refresh this call's AFK timer.
//!
//! Reports go out only while this device is in a call on a community voice channel that is not that community's AFK
//! channel, and only when the community has one: once on joining (the tracking key `community:channel:afkChannel`
//! changes), then while its microphone sends speech (Task 5's own speaking: `voice.speaking` holds this device), and on
//! pointer and key input while the window is visible; never more than one per [`EVERY`]. The desktop never sends the
//! exempt report (phones and coarse pointers only).

use std::time::{Duration, Instant};

use dilla_client::calls::CallPhase;
use dilla_client::state::{AppState, ChannelId, CommunityId};

/// Shell.tsx: no more than one report per 5 s, and the speech tick's period.
pub const EVERY: Duration = Duration::from_secs(5);

/// Where a report goes: the call's community and voice channel, and that community's AFK channel (the tracking key).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Target {
    pub community: CommunityId,
    pub channel: ChannelId,
    pub afk: ChannelId,
}

/// The call of `state` that reports activity: this device is in a call on a community voice channel, that community's
/// AFK snapshot names an AFK channel, and the call is not in it. `None` otherwise (a DM's call, the AFK channel itself, a
/// community without an AFK channel, or one whose snapshot is not read yet: [`unread_afk`]).
pub fn target(state: &AppState) -> Option<Target> {
    let (community, channel) = community_call(state)?;
    let afk = state.ring.afk.get(&community)?.channel?;
    (channel != afk).then_some(Target {
        community,
        channel,
        afk,
    })
}

/// This device's call, while it is in one on a community voice channel: that community and channel.
fn community_call(state: &AppState) -> Option<(CommunityId, ChannelId)> {
    let call = &state.voice.call;
    if call.phase != CallPhase::In {
        return None;
    }
    let channel = state.channel(call.channel?)?;
    (channel.kind == 1).then_some((channel.community_id, channel.id))
}

/// The community of this device's community voice call whose AFK snapshot is not read yet: it is read, so the call can
/// report.
pub fn unread_afk(state: &AppState) -> Option<CommunityId> {
    let (community, _) = community_call(state)?;
    (!state.ring.afk.contains_key(&community)).then_some(community)
}

/// Whether this device is speaking now (Task 5: `voice.speaking` holds this device while its microphone sends speech).
pub fn own_speaking(state: &AppState) -> bool {
    state.voice.call.phase == CallPhase::In
        && state
            .account
            .device_id
            .is_some_and(|d| state.voice.speaking.contains(&d))
}

/// The reports of one window's call: what it reports to, and when it last did.
#[derive(Debug, Default)]
pub struct AfkTracker {
    target: Option<Target>,
    last: Option<Instant>,
}

impl AfkTracker {
    /// The call's target is `target` now. A new one (a join, another channel, the AFK channel read) resets the throttle
    /// and reports once; the same one reports nothing.
    pub fn follow(&mut self, target: Option<Target>, now: Instant) -> Option<Target> {
        if self.target == target {
            return None;
        }
        self.target = target;
        self.last = None;
        self.report(now)
    }

    /// The speech tick: while this device speaks, a report (throttled).
    pub fn speech(&mut self, speaking: bool, now: Instant) -> Option<Target> {
        if !speaking {
            return None;
        }
        self.report(now)
    }

    /// A pointer move, a press or a key in the window: a report while the window is visible (throttled).
    pub fn input(&mut self, visible: bool, now: Instant) -> Option<Target> {
        if !visible {
            return None;
        }
        self.report(now)
    }

    /// A report to the current target, unless there is none or one went out less than [`EVERY`] ago.
    fn report(&mut self, now: Instant) -> Option<Target> {
        let target = self.target?;
        if self
            .last
            .is_some_and(|last| now.saturating_duration_since(last) < EVERY)
        {
            return None;
        }
        self.last = Some(now);
        Some(target)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use dilla_client::state::ring::AfkSnapshot;
    use dilla_client::state::voice::OwnCall;
    use dilla_client::state::{Channel, ChannelGroupState, DeviceId};

    const COMMUNITY: CommunityId = CommunityId([1; 16]);
    const VOICE: ChannelId = ChannelId([9; 16]);
    const OTHER: ChannelId = ChannelId([8; 16]);
    const AFK: ChannelId = ChannelId([7; 16]);
    const ME: DeviceId = DeviceId([100; 16]);

    fn t(channel: ChannelId) -> Target {
        Target {
            community: COMMUNITY,
            channel,
            afk: AFK,
        }
    }

    fn at(t0: Instant, secs: u64) -> Instant {
        t0 + Duration::from_secs(secs)
    }

    fn voice_channel(id: ChannelId) -> Channel {
        Channel {
            id,
            community_id: COMMUNITY,
            kind: 1,
            mode: 0,
            name: "Lounge".into(),
            topic: String::new(),
            parent_id: None,
            position: 0,
            slowmode: 0,
            text_group_id: None,
            group: ChannelGroupState::Unsupported,
        }
    }

    /// This device in a call on `channel`, its community's AFK channel `afk` (`None`: the snapshot says none; the
    /// snapshot is read unless `read` is false).
    fn in_call(channel: ChannelId, afk: Option<ChannelId>, read: bool) -> AppState {
        let mut s = AppState::default();
        s.channels.push(voice_channel(VOICE));
        s.channels.push(voice_channel(AFK));
        s.account.device_id = Some(ME);
        s.voice.call = OwnCall {
            phase: CallPhase::In,
            channel: Some(channel),
            community: Some(COMMUNITY),
            call_id: Some(channel.0),
            ..OwnCall::default()
        };
        if read {
            s.ring.afk.insert(
                COMMUNITY,
                AfkSnapshot {
                    channel: afk,
                    timeout_seconds: 300,
                    ..AfkSnapshot::default()
                },
            );
        }
        s
    }

    #[test]
    fn joining_a_call_reports_once() {
        let t0 = Instant::now();
        let mut k = AfkTracker::default();
        assert_eq!(k.follow(None, t0), None, "no call, no report");
        assert_eq!(k.follow(Some(t(VOICE)), at(t0, 1)), Some(t(VOICE)));
        // The same call again, however late: no second join report.
        assert_eq!(k.follow(Some(t(VOICE)), at(t0, 2)), None);
        assert_eq!(k.follow(Some(t(VOICE)), at(t0, 60)), None);
        // The target a call in VOICE has, once its AFK snapshot is read.
        let s = in_call(VOICE, Some(AFK), true);
        assert_eq!(target(&s), Some(t(VOICE)));
    }

    #[test]
    fn own_speech_reports_every_5_s() {
        let t0 = Instant::now();
        let mut k = AfkTracker::default();
        assert_eq!(k.follow(Some(t(VOICE)), t0), Some(t(VOICE)));
        assert_eq!(k.speech(true, at(t0, 1)), None, "within 5 s of the join");
        assert_eq!(k.speech(true, at(t0, 5)), Some(t(VOICE)));
        assert_eq!(k.speech(true, at(t0, 9)), None);
        assert_eq!(k.speech(true, at(t0, 10)), Some(t(VOICE)));
        assert_eq!(k.speech(false, at(t0, 20)), None, "silence reports nothing");
        assert_eq!(k.speech(true, at(t0, 21)), Some(t(VOICE)));
        // Own speaking is this device in `voice.speaking`; another device speaking is not.
        let mut s = in_call(VOICE, Some(AFK), true);
        assert!(!own_speaking(&s));
        s.voice.speaking.insert(DeviceId([101; 16]));
        assert!(!own_speaking(&s), "someone else speaking");
        s.voice.speaking.insert(ME);
        assert!(own_speaking(&s));
        s.voice.call.phase = CallPhase::Idle;
        assert!(!own_speaking(&s), "out of a call");
    }

    #[test]
    fn input_while_visible_reports_but_not_more_than_once_per_5_s() {
        let t0 = Instant::now();
        let mut k = AfkTracker::default();
        assert_eq!(k.follow(Some(t(VOICE)), t0), Some(t(VOICE)));
        assert_eq!(k.input(true, at(t0, 2)), None);
        assert_eq!(k.input(true, at(t0, 6)), Some(t(VOICE)));
        assert_eq!(k.input(true, at(t0, 7)), None);
        assert_eq!(
            k.speech(true, at(t0, 8)),
            None,
            "speech shares the throttle"
        );
        assert_eq!(k.input(true, at(t0, 11)), Some(t(VOICE)));
    }

    #[test]
    fn input_while_hidden_does_not_report() {
        let t0 = Instant::now();
        let mut k = AfkTracker::default();
        assert_eq!(k.follow(Some(t(VOICE)), t0), Some(t(VOICE)));
        assert_eq!(k.input(false, at(t0, 10)), None);
        assert_eq!(k.input(false, at(t0, 30)), None);
        // Hidden input does not take the throttle either.
        assert_eq!(k.input(true, at(t0, 31)), Some(t(VOICE)));
    }

    #[test]
    fn the_afk_channel_and_a_community_without_one_never_report() {
        let t0 = Instant::now();
        // In the AFK channel itself.
        let s = in_call(AFK, Some(AFK), true);
        assert_eq!(target(&s), None);
        assert_eq!(unread_afk(&s), None, "its snapshot is read");
        // A community without an AFK channel.
        let s = in_call(VOICE, None, true);
        assert_eq!(target(&s), None);
        assert_eq!(unread_afk(&s), None);
        // Its snapshot is not read yet: no report until it is, and it is asked for.
        let s = in_call(VOICE, Some(AFK), false);
        assert_eq!(target(&s), None);
        assert_eq!(unread_afk(&s), Some(COMMUNITY));
        // Not in a call yet (joining), or a DM's call (no community channel).
        let mut s = in_call(VOICE, Some(AFK), true);
        s.voice.call.phase = CallPhase::Joining;
        assert_eq!(target(&s), None);
        let s = in_call(ChannelId([5; 16]), Some(AFK), true);
        assert_eq!(target(&s), None, "a DM's call");
        assert_eq!(unread_afk(&s), None);
        // With no target nothing reports, speech and input included.
        let mut k = AfkTracker::default();
        assert_eq!(k.follow(None, t0), None);
        assert_eq!(k.speech(true, at(t0, 10)), None);
        assert_eq!(k.input(true, at(t0, 20)), None);
    }

    #[test]
    fn a_new_call_resets_the_throttle() {
        let t0 = Instant::now();
        let mut k = AfkTracker::default();
        assert_eq!(k.follow(Some(t(VOICE)), t0), Some(t(VOICE)));
        // Another channel within 5 s: its join reports at once.
        assert_eq!(k.follow(Some(t(OTHER)), at(t0, 1)), Some(t(OTHER)));
        assert_eq!(k.input(true, at(t0, 2)), None);
        // The call ends and the same channel is joined again within 5 s: a new call, a new join report.
        assert_eq!(k.follow(None, at(t0, 3)), None);
        assert_eq!(k.input(true, at(t0, 3)), None, "no call, no report");
        assert_eq!(k.follow(Some(t(OTHER)), at(t0, 4)), Some(t(OTHER)));
    }
}
