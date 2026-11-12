//! What the UI observes: one immutable [`AppState`] snapshot, replaced whole on every change and published through a
//! `tokio::sync::watch` channel ([`crate::client::Client::state`]). The slices mirror the TS worker's state slices
//! (`packages/client-core/src/state/types.ts`) by name: `connection`, `account`, `communities`, `channels:<id>`
//! (here one list with the community on each channel), `members:<id>`, `timeline:<id>` and `badges` (here `unread`).
//! Nothing here holds key material: message bodies are plaintext the core already decrypted and stored.

use std::collections::HashMap;
use std::fmt;

pub mod channels;
pub mod members;
pub mod messages;
pub mod unread;

pub use channels::{Channel, ChannelGroupState, Community};
pub use members::Member;
pub use messages::{TIMELINE_PAGE, Timeline, TimelineItem, TimelineItemState};
pub use unread::BadgeState;

macro_rules! id16 {
    ($($(#[$doc:meta])* $name:ident),* $(,)?) => {$(
        $(#[$doc])*
        #[derive(Clone, Copy, PartialEq, Eq, Hash, PartialOrd, Ord)]
        pub struct $name(pub [u8; 16]);

        impl $name {
            /// 32 lower-case hex digits, the form every route and slice key uses.
            pub fn hex(&self) -> String {
                hex(&self.0)
            }
        }

        impl fmt::Debug for $name {
            fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
                write!(f, "{}({})", stringify!($name), self.hex())
            }
        }

        impl fmt::Display for $name {
            fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
                f.write_str(&self.hex())
            }
        }
    )*};
}

id16!(
    /// A channel (`ChannelSummary.id`).
    ChannelId,
    /// A community (a "server" in the UI).
    CommunityId,
    /// An MLS group (a channel's text group).
    GroupId,
    /// An account.
    UserId,
    /// A device of an account.
    DeviceId,
    /// A message's id (an outbox row's, kept when it is stored).
    MessageId,
);

pub(crate) fn hex(bytes: &[u8]) -> String {
    use fmt::Write;
    let mut out = String::with_capacity(bytes.len() * 2);
    for b in bytes {
        let _ = write!(out, "{b:02x}");
    }
    out
}

/// `ConnectionState.status`.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Default)]
pub enum ConnectionStatus {
    #[default]
    Offline,
    Connecting,
    Online,
}

/// `ConnectionState`: the gateway as the UI shows it.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct ConnectionState {
    pub status: ConnectionStatus,
    /// The instance generation of the newest ready.
    pub generation: Option<u64>,
    /// The wire version the ready connection selected.
    pub wire_version: Option<u64>,
}

/// `AccountState.phase`, narrowed to what a running client reaches (sign-up and sign-in are the UI's flows).
#[derive(Clone, Copy, Debug, PartialEq, Eq, Default)]
pub enum AccountPhase {
    /// Opening the store and establishing the session.
    #[default]
    Loading,
    /// Signed in and syncing.
    Ready,
    /// The account owes a second factor (gateway close 4003 "second factor required", or a refusal
    /// E_FACTOR_REQUIRED): the session stands, the UI asks for the factor, then the client starts again.
    FactorSetup,
    /// The session was revoked or refused (close 4004, 4003, an upgrade 401, a challenge the instance no longer
    /// answers): sign in again.
    Revoked,
    /// The instance refused this client's version (close 4006): `upgrade` when this client must update.
    VersionRefused { upgrade: bool },
}

/// `AccountState`, the part a running client knows.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct AccountState {
    pub phase: AccountPhase,
    pub user_id: Option<UserId>,
    pub device_id: Option<DeviceId>,
    pub username: String,
}

/// The whole observable state. Every value is immutable; the client replaces the snapshot on each change.
#[derive(Clone, Debug, Default)]
pub struct AppState {
    pub account: AccountState,
    pub connection: ConnectionState,
    pub communities: Vec<Community>,
    /// Every channel of every community, by community then position.
    pub channels: Vec<Channel>,
    /// `timeline:<channel>`: the channels whose text group this device holds.
    pub timelines: HashMap<ChannelId, Timeline>,
    /// `members:<community>`.
    pub members: HashMap<CommunityId, Vec<Member>>,
    /// `badges`: unread and mention counts per channel; a channel without unread rows is absent.
    pub unread: HashMap<ChannelId, BadgeState>,
}

impl AppState {
    pub fn channel(&self, id: ChannelId) -> Option<&Channel> {
        self.channels.iter().find(|c| c.id == id)
    }

    /// The channel named `name` in community `community`.
    pub fn channel_named(&self, community: CommunityId, name: &str) -> Option<&Channel> {
        self.channels
            .iter()
            .find(|c| c.community_id == community && c.name == name)
    }

    /// The unread count of `channel` (0 when it has no badge).
    pub fn unread_of(&self, channel: ChannelId) -> u32 {
        self.unread.get(&channel).map_or(0, |b| b.unread)
    }
}
