//! `ring`: Task 18's slice of [`super::AppState`]: rings in and out, blocked calls, AFK moves. The TS worker's `rings`
//! slice, the `outgoing` and `blocked` parts of its `call` slice, and its `afk:<community>` slices.
//!
//! Nothing here holds key material: ids, names, codes and timestamps only.

use std::collections::HashMap;
use std::time::Duration;

use super::{ChannelId, CommunityId, UserId};

/// ring.ts `RING_MS` (R5): a ring lasts 30 s from its arrival here at the latest.
pub const RING: Duration = Duration::from_secs(30);
/// ring.ts `RINGS_MAX` (RV-4): at most eight concurrent rings per client.
pub const RINGS_MAX: usize = 8;
/// routes.ts `AFK_AWAY_MAX`: the most rows of an AFK away roster read.
pub const AFK_AWAY_MAX: usize = 1024;

/// types.ts `Ring`: an incoming DM ring, as the banner shows it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct IncomingRing {
    pub call_id: [u8; 16],
    pub channel: ChannelId,
    pub caller: UserId,
    pub caller_name: String,
    /// The instance's `expires_ts`: the ring's identity for the decline, never compared with this device's clock.
    pub ring_ts: u64,
    /// Arrival + [`RING`] on this device's monotonic clock.
    pub expires_at: tokio::time::Instant,
    /// This device is in a call: the busy text, no sound, no notification.
    pub busy: bool,
    /// The DM is muted: the banner only.
    pub quiet: bool,
}

/// The `outgoing` part of types.ts `CallState`: a DM call this device opened, and who it is still ringing.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Outgoing {
    pub channel: Option<ChannelId>,
    /// Rung and not in the call yet.
    pub ringing: Vec<UserId>,
    /// Declined (op 53 state 2), in the order they declined.
    pub declined: Vec<UserId>,
    /// Held back by the instance's pair rule (called too recently).
    pub not_rung: Vec<UserId>,
}

/// A call this device may not join or had to leave (decision 4, RV-1, RV-3): the `blocked` phase of types.ts `CallState`
/// with its error code and changed contact.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct CallBlock {
    pub channel: ChannelId,
    /// `E_UMK_CHANGED`, `E_DEVICE_UNLISTED`, `E_CREDENTIAL`, ...
    pub code: String,
    /// The contact whose identity changed (only for `E_UMK_CHANGED`, only among this call's own participants).
    pub changed_user: Option<UserId>,
    /// Why the notice shows: refused before the join, or left mid-call.
    pub when: BlockWhen,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum BlockWhen {
    /// Refused before anything of the join was sent.
    BeforeJoin,
    /// The call was left because of it.
    Left,
}

/// One member listed away under the AFK channel (AW4), and the unix second they became away.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Away {
    pub user: UserId,
    pub since: u64,
}

/// types.ts `AFKState`: a community's AFK snapshot.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct AfkSnapshot {
    pub channel: Option<ChannelId>,
    pub timeout_seconds: u64,
    /// This device has a pending cut.
    pub pending: bool,
    /// Why: 1 the idle sweep moved it, 2 this user chose AFK elsewhere, 3 a newer device took voice over; 0 none.
    pub pending_reason: u8,
    /// Rises with every landed read (one counter for the client's life): a read that says `pending` again is a new move.
    pub seq: u64,
    /// Sorted by user id.
    pub away: Vec<Away>,
}

/// The move a landed AFK read announces once (R-AFK4): "Moved to {channel} for inactivity", or the elsewhere and
/// takeover lines.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct AfkNotice {
    pub community: CommunityId,
    pub channel: Option<ChannelId>,
    pub reason: u8,
    /// The read's seq: the page keys the move on (community, seq).
    pub seq: u64,
}

/// The ring area's slice.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct RingState {
    /// Incoming rings, oldest first.
    pub rings: Vec<IncomingRing>,
    /// The DM call this device opened, while it rings someone.
    pub outgoing: Option<Outgoing>,
    /// The newest blocked call, until its cause clears or another call starts.
    pub blocked: Option<CallBlock>,
    /// Where this device's call is (the busy text's `{where}`), `None` outside a call. Set by the voice area.
    pub in_call: Option<String>,
    /// `afk:<community>`: absent until read.
    pub afk: HashMap<CommunityId, AfkSnapshot>,
    /// The newest move to announce, until the person moves on.
    pub afk_notice: Option<AfkNotice>,
}

impl RingState {
    /// The ring of `channel`, if one shows.
    pub fn ring_of(&self, channel: ChannelId) -> Option<&IncomingRing> {
        self.rings.iter().find(|r| r.channel == channel)
    }

    /// Whether `user` is listed away in `community`.
    pub fn is_away(&self, community: CommunityId, user: UserId) -> bool {
        self.afk
            .get(&community)
            .is_some_and(|s| s.away.iter().any(|a| a.user == user))
    }
}
