//! `bot`: Task 20's slice of [`super::AppState`]: the command catalog, private results, polls and reminders.
//!
//! Nothing here holds key material. Private results ([`PrivateResult`]) live only in this in-memory slice: they are never
//! written to the store, never sent and never part of a timeline, so they reach no other device and no restart.

use std::collections::BTreeMap;

use super::{ChannelId, CommunityId, MessageId, UserId};

/// Argument type codes of a Bot catalog command: 0 text, 1 uint, 2 user, 3 channel, 4 duration (seconds), 5 bool.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ArgType {
    Text,
    Uint,
    User,
    Channel,
    Duration,
    Bool,
}

impl ArgType {
    /// The wire code's type, or `None` past 5.
    pub fn from_code(code: u64) -> Option<Self> {
        Some(match code {
            0 => Self::Text,
            1 => Self::Uint,
            2 => Self::User,
            3 => Self::Channel,
            4 => Self::Duration,
            5 => Self::Bool,
            _ => return None,
        })
    }
}

/// `ArgShape`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ArgShape {
    pub name: String,
    pub ty: ArgType,
    pub required: bool,
}

/// `BotCommandSpec`: one command of a community Bot's catalog (display only: the Bot authorises every operation).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct BotCommandSpec {
    pub name: String,
    pub description: String,
    pub operation: u32,
    pub args: Vec<ArgShape>,
    pub permission_bit: u64,
    pub needs_bot_channel: bool,
    pub handler_kind: u32,
}

/// `BotState`: the instance's states, plus this device's own `Unverified` and `Changed`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum BotStatusState {
    Ready,
    Degraded,
    Offline,
    Disabled,
    Absent,
    /// A published key this device cannot check (yet).
    Unverified,
    /// A Bot account other than the one this device pinned.
    Changed,
}

/// `BotStatus`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct BotStatus {
    pub state: BotStatusState,
    /// Unix seconds since the state holds, when the instance says.
    pub since: Option<u64>,
}

/// `BotCatalogView`: a community's catalog as this device holds it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct BotCatalogView {
    pub revision: u64,
    pub commands: Vec<BotCommandSpec>,
    pub status: BotStatus,
    /// The Bot channels this member can view; `None` until read.
    pub bot_channels: Option<Vec<ChannelId>>,
    /// The Bot user this device pinned for the community.
    pub bot_user: Option<UserId>,
    /// The Bot's display name (the picker's source tag).
    pub bot_name: String,
}

/// A private result row's status (`BotStatus` of packages/ui BotResult).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ResultStatus {
    Pending,
    Success,
    Failed,
    /// "Not confirmed": an uncertain outcome.
    Cancelled,
}

/// Who answers a private result: this device ("dilla") or the community's Bot.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ResultProvider {
    Builtin,
    Bot,
}

/// One private result row (private-results.ts `PrivateResult`): shown only on this device, in its conversation.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PrivateResult {
    pub id: u64,
    /// When it was added, unix milliseconds (the rows' order).
    pub at: u64,
    pub status: ResultStatus,
    pub text: String,
    pub provider: ResultProvider,
    /// The row offers a way on (a Retry): it stays until it settles or the person acts.
    pub hold: bool,
    /// When it last finished (unix ms), while it is finished and not held: it goes away [`PRIVATE_RESULT_TTL_MS`] later.
    pub settled_at: Option<u64>,
}

/// private-results.ts `PRIVATE_RESULT_TTL_MS`: a finished private reply goes away this long after it finished.
pub const PRIVATE_RESULT_TTL_MS: u64 = 60_000;

impl PrivateResult {
    /// `autoDismissAt`: when the row dismisses itself, or `None` while it must stay.
    pub fn auto_dismiss_at(&self) -> Option<u64> {
        self.settled_at.map(|s| s + PRIVATE_RESULT_TTL_MS)
    }
}

/// The conversation a private result belongs to: a channel, or a thread in it.
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct Conversation {
    pub channel: ChannelId,
    pub thread: Option<MessageId>,
}

/// A poll as the Bot's durable record says (utilities.ts `PollSummary`), with the voters a card shows.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PollView {
    /// The poll's id (its post's final `[dilla-poll:v1:<id>]` line).
    pub id: [u8; 16],
    pub channel: ChannelId,
    pub creator: UserId,
    pub question: String,
    pub answers: Vec<String>,
    pub multi: bool,
    pub created_at: u64,
    pub due_at: u64,
    pub closed: bool,
    pub counts: Vec<u64>,
    /// This member's choices, `None` when it has not voted.
    pub my_choices: Option<Vec<usize>>,
    pub revision: u64,
    /// Up to five voters' display names per answer.
    pub previews: Vec<Vec<String>>,
    /// How many distinct people voted.
    pub distinct: u64,
    /// `distinct` is a lower bound.
    pub capped: bool,
}

/// `ReminderState`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ReminderStatus {
    Scheduled,
    Due,
    Cancelled,
    Dismissed,
}

/// utilities.ts `ReminderSummary`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ReminderView {
    pub id: [u8; 16],
    pub text: String,
    /// Unix seconds.
    pub due: u64,
    pub timezone: String,
    pub source_channel: Option<ChannelId>,
    pub source_thread: Option<MessageId>,
    pub state: ReminderStatus,
    pub revision: u64,
}

/// The bot area's slice.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct BotState {
    /// `botCatalog:<community>`.
    pub catalogs: BTreeMap<CommunityId, BotCatalogView>,
    /// Private results, by conversation, oldest first.
    pub private_results: BTreeMap<Conversation, Vec<PrivateResult>>,
    /// The verified polls, by the message that carries each.
    pub polls: BTreeMap<MessageId, PollView>,
    /// This member's reminders, by community, soonest first.
    pub reminders: BTreeMap<CommunityId, Vec<ReminderView>>,
}

impl BotState {
    /// The private results of `conversation`, oldest first.
    pub fn results(&self, conversation: &Conversation) -> &[PrivateResult] {
        self.private_results
            .get(conversation)
            .map_or(&[], Vec::as_slice)
    }
}
