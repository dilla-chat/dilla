//! `voice`: Task 17's slice of [`super::AppState`]: who is in voice in every channel (types.ts `VoiceState`, calls/voice.ts
//! `VoiceBook`), this device's own call (types.ts `CallState`), its mute and deafen, who is speaking and what the media
//! layer reported.
//!
//! Nothing here holds key material: a call's media keys reach the media layer only through
//! [`crate::features::voice::MediaLink`], never through a slice (protocol/08, L-TS-52).

use std::collections::{BTreeMap, BTreeSet};

use super::{ChannelId, CommunityId, DeviceId, UserId};
use crate::calls::CallPhase;

/// calls/voice.ts `VOICE_MAX`: the most devices one call lists.
pub const VOICE_MAX: usize = 1024;

/// The bits of `voice_state`'s `flags` (protocol/02 § Gateway frames).
pub mod flags {
    pub const IN_CALL: u8 = 1;
    pub const SELF_MUTE: u8 = 2;
    pub const SELF_DEAF: u8 = 4;
    pub const VIDEO: u8 = 8;
    pub const SCREEN: u8 = 16;
}

/// One device in a channel's call (types.ts `VoiceParticipant`, the name left to the view: it reads the member list).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct VoiceParticipant {
    pub user_id: UserId,
    pub device_id: DeviceId,
    /// `voice_state` flags ([`flags`]).
    pub flags: u8,
    /// This device.
    pub own: bool,
}

impl VoiceParticipant {
    pub fn muted(&self) -> bool {
        self.flags & flags::SELF_MUTE != 0
    }
    pub fn deafened(&self) -> bool {
        self.flags & flags::SELF_DEAF != 0
    }
    pub fn video(&self) -> bool {
        self.flags & flags::VIDEO != 0
    }
    pub fn screen(&self) -> bool {
        self.flags & flags::SCREEN != 0
    }
}

/// A channel's live call (types.ts `VoiceCall`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct VoiceCall {
    pub call_id: [u8; 16],
    /// By user id, then device id (voice.ts `sorted`).
    pub participants: Vec<VoiceParticipant>,
}

/// What the media layer reported that the person should see (dilla-media `MediaEvent`, mirrored: this crate does not
/// link the media stack).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum VoiceNotice {
    EnhancedUnavailable,
    DevicesChanged,
    MicLost,
    /// The OS denies the app the microphone: the call joined listen-only (Package 7, `voice.micDenied`).
    MicDenied,
    SpeakerLost,
    EnhancedFailed,
    /// A share was asked for and the selection was cancelled: nothing was published.
    ShareCancelled,
    /// A screen share without sound (share-audio-advice.ts).
    ShareWithoutSound,
    /// The call's media did not start: the call was left.
    MediaFailed,
}

/// This device's own call (types.ts `CallState`, the parts the desktop draws).
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct OwnCall {
    pub phase: CallPhase,
    pub channel: Option<ChannelId>,
    pub community: Option<CommunityId>,
    pub call_id: Option<[u8; 16]>,
    pub group_id: Option<[u8; 16]>,
    pub epoch: Option<u64>,
    /// The epoch's safety code (6 groups of 5 digits).
    pub code: Option<String>,
    /// The `E_*` code of the last join or leave that failed.
    pub error: Option<String>,
    /// `(user, device, leaf)` of every member of the call group.
    pub roster: Vec<(UserId, DeviceId, u16)>,
    /// The link's ping to the SFU in ms (session.ts `link.rttMs`), read by the media every few seconds; `None` until
    /// one is measured (the dock and the call bar say "— ms").
    pub ping_ms: Option<u32>,
    /// What this device sends and receives, kb/s (stats.ts `linkSample`'s `upKbps` / `downKbps`), from the media's link
    /// reads; `None` until two reads gave a rate (the dock and the call bar then say the ping alone).
    pub rate_kbps: Option<(u32, u32)>,
}

/// What this device publishes besides its microphone.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct LocalShare {
    pub camera: bool,
    pub screen: bool,
    /// The screen share carries its sound.
    pub screen_audio: bool,
}

/// The voice slice.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct VoiceState {
    /// `voice`: each channel's (and DM's) live call, by channel.
    pub calls: BTreeMap<ChannelId, VoiceCall>,
    /// `call`: this device's call.
    pub call: OwnCall,
    /// The person's own mute (kept before a join: the join starts muted).
    pub self_mute: bool,
    /// The person's own deafen (a deafened device is muted too).
    pub self_deaf: bool,
    /// Push to talk is held: the microphone sends while it is.
    pub ptt_held: bool,
    /// The devices heard speaking now (this device's own included while its microphone is live).
    pub speaking: BTreeSet<DeviceId>,
    pub share: LocalShare,
    /// The last media notice, until the person dismisses it.
    pub notice: Option<VoiceNotice>,
}

impl VoiceState {
    /// The phase other areas read (Task 19's `in_call`, Task 18's ring busy).
    pub fn phase(&self) -> CallPhase {
        self.call.phase
    }

    /// In a call or on the way into one.
    pub fn in_call(&self) -> bool {
        self.call.phase != CallPhase::Idle
    }

    /// The flags this device asks the instance to show for it (`voice_self`: bit 0 mute, bit 1 deafen).
    pub fn self_flags(&self) -> u8 {
        u8::from(self.self_mute || self.self_deaf) | (u8::from(self.self_deaf) << 1)
    }

    /// The people of a channel's call.
    pub fn participants(&self, channel: ChannelId) -> &[VoiceParticipant] {
        self.calls
            .get(&channel)
            .map_or(&[][..], |c| c.participants.as_slice())
    }

    /// voice.ts `channelOf`: the channel whose call `call` is, else the channel of that id when `known` says it is one
    /// (a call id is its channel's id, protocol/09 § Voice).
    pub fn channel_of(
        &self,
        call: [u8; 16],
        known: impl Fn(ChannelId) -> bool,
    ) -> Option<ChannelId> {
        self.calls
            .iter()
            .find(|(_, c)| c.call_id == call)
            .map(|(ch, _)| *ch)
            .or_else(|| Some(ChannelId(call)).filter(|c| known(*c)))
    }

    /// voice.ts `apply`: one `voice_state` for `channel`. Flags 0 removes the device; any other adds or updates it. A
    /// device beyond [`VOICE_MAX`] is not added. Answers whether anything changed.
    pub fn apply(&mut self, channel: ChannelId, call: [u8; 16], row: VoiceParticipant) -> bool {
        let before = self.participants(channel).to_vec();
        let mut rest: Vec<VoiceParticipant> = before
            .iter()
            .filter(|p| p.device_id != row.device_id)
            .cloned()
            .collect();
        if row.flags != 0 && rest.len() == before.len() && rest.len() >= VOICE_MAX {
            return false;
        }
        if row.flags != 0 {
            rest.push(row);
            sort(&mut rest);
        }
        if rest.is_empty() {
            return self.calls.remove(&channel).is_some();
        }
        if rest == before {
            return false;
        }
        let call_id = self.calls.get(&channel).map_or(call, |c| c.call_id);
        self.calls.insert(
            channel,
            VoiceCall {
                call_id,
                participants: rest,
            },
        );
        true
    }

    /// voice.ts `place`: a snapshot of `channel`'s call (`None` or nobody in it: no call).
    pub fn place(
        &mut self,
        channel: ChannelId,
        call: Option<[u8; 16]>,
        rows: Vec<VoiceParticipant>,
    ) {
        match call {
            Some(call_id) if !rows.is_empty() => {
                let mut participants: Vec<VoiceParticipant> =
                    rows.into_iter().take(VOICE_MAX).collect();
                sort(&mut participants);
                self.calls.insert(
                    channel,
                    VoiceCall {
                        call_id,
                        participants,
                    },
                );
            }
            _ => {
                self.calls.remove(&channel);
            }
        }
    }

    /// voice.ts `dropDevice`: one device left a channel's call as far as this device knows.
    pub fn drop_device(&mut self, channel: ChannelId, device: DeviceId) -> bool {
        let Some(call) = self.calls.get_mut(&channel) else {
            return false;
        };
        let before = call.participants.len();
        call.participants.retain(|p| p.device_id != device);
        let changed = call.participants.len() != before;
        if call.participants.is_empty() {
            self.calls.remove(&channel);
        }
        changed
    }
}

fn sort(rows: &mut [VoiceParticipant]) {
    rows.sort_by(|a, b| {
        a.user_id
            .cmp(&b.user_id)
            .then(a.device_id.cmp(&b.device_id))
    });
}

#[cfg(test)]
mod tests {
    use super::*;

    fn row(user: u8, device: u8, flags: u8) -> VoiceParticipant {
        VoiceParticipant {
            user_id: UserId([user; 16]),
            device_id: DeviceId([device; 16]),
            flags,
            own: false,
        }
    }
    const CH: ChannelId = ChannelId([7; 16]);

    #[test]
    fn a_frame_adds_updates_sorts_and_flags_zero_removes() {
        let mut v = VoiceState::default();
        assert!(v.apply(CH, CH.0, row(2, 2, 1)));
        assert!(v.apply(CH, CH.0, row(1, 1, 1)));
        let users: Vec<u8> = v.participants(CH).iter().map(|p| p.user_id.0[0]).collect();
        assert_eq!(users, vec![1, 2], "sorted by user");
        assert!(v.apply(CH, CH.0, row(1, 1, 3)));
        assert!(v.participants(CH)[0].muted());
        assert!(
            !v.apply(CH, CH.0, row(1, 1, 3)),
            "the same row again changes nothing"
        );
        assert!(v.apply(CH, CH.0, row(1, 1, 0)));
        assert!(v.apply(CH, CH.0, row(2, 2, 0)));
        assert!(
            !v.calls.contains_key(&CH),
            "the last one out ends the entry"
        );
    }

    #[test]
    fn a_device_beyond_the_bound_is_not_added_but_a_listed_one_is_updated() {
        let mut v = VoiceState::default();
        let rows: Vec<VoiceParticipant> = (0..VOICE_MAX)
            .map(|i| VoiceParticipant {
                user_id: UserId([0; 16]),
                device_id: DeviceId(u128::try_from(i).unwrap().to_be_bytes()),
                flags: 1,
                own: false,
            })
            .collect();
        v.place(CH, Some(CH.0), rows);
        assert!(!v.apply(CH, CH.0, row(9, 9, 1)));
        let mut listed = v.participants(CH)[3].clone();
        listed.flags = 3;
        assert!(v.apply(CH, CH.0, listed));
    }

    #[test]
    fn a_snapshot_without_a_call_or_people_drops_the_entry() {
        let mut v = VoiceState::default();
        v.place(CH, Some(CH.0), vec![row(1, 1, 1)]);
        v.place(CH, Some(CH.0), vec![]);
        assert!(v.calls.is_empty());
        v.place(CH, Some(CH.0), vec![row(1, 1, 1)]);
        v.place(CH, None, vec![row(1, 1, 1)]);
        assert!(v.calls.is_empty());
    }

    #[test]
    fn a_frame_lands_in_the_channel_whose_call_it_is_else_a_known_channel_of_that_id() {
        let mut v = VoiceState::default();
        v.place(CH, Some([9; 16]), vec![row(1, 1, 1)]);
        assert_eq!(v.channel_of([9; 16], |_| false), Some(CH));
        assert_eq!(
            v.channel_of([5; 16], |c| c.0 == [5; 16]),
            Some(ChannelId([5; 16]))
        );
        assert_eq!(v.channel_of([6; 16], |_| false), None);
    }

    #[test]
    fn deafen_sends_mute_too() {
        let mut v = VoiceState::default();
        assert_eq!(v.self_flags(), 0);
        v.self_mute = true;
        assert_eq!(v.self_flags(), 1);
        v.self_mute = false;
        v.self_deaf = true;
        assert_eq!(v.self_flags(), 3);
    }

    #[test]
    fn drop_device_removes_one_and_the_last_ends_the_entry() {
        let mut v = VoiceState::default();
        v.place(CH, Some(CH.0), vec![row(1, 1, 1), row(2, 2, 1)]);
        assert!(v.drop_device(CH, DeviceId([1; 16])));
        assert!(!v.drop_device(CH, DeviceId([1; 16])));
        assert!(v.drop_device(CH, DeviceId([2; 16])));
        assert!(v.calls.is_empty());
    }
}
