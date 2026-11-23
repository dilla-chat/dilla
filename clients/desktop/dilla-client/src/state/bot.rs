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
    /// The verified command key's fingerprint (12 Crockford characters); `None` while no key checked.
    pub fingerprint: Option<String>,
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

/// One private result row (private-results.ts `PrivateResult`): shown only on this device, in its conversation. Its Debug
/// gives the text's length only: a moderation command's row names the member it acted on ("{name} was banned.").
#[derive(Clone, PartialEq, Eq)]
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

impl std::fmt::Debug for PrivateResult {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("PrivateResult")
            .field("id", &self.id)
            .field("at", &self.at)
            .field("status", &self.status)
            .field("text_len", &self.text.len())
            .field("provider", &self.provider)
            .field("hold", &self.hold)
            .field("settled_at", &self.settled_at)
            .finish()
    }
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
#[derive(Clone, PartialEq, Eq)]
pub struct PollView {
    /// The poll's id (its post's final `[dilla-poll:v1:<id>]` line).
    pub id: [u8; 16],
    pub channel: ChannelId,
    /// The thread the poll was posted in, if any.
    pub thread: Option<MessageId>,
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
#[derive(Clone, PartialEq, Eq)]
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

// R21: a poll's question and answers and a reminder's text are decrypted posts; Debug gives their lengths only.

impl std::fmt::Debug for PollView {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("PollView")
            .field("id", &self.id)
            .field("channel", &self.channel)
            .field("thread", &self.thread)
            .field("question", &super::Redacted(&self.question))
            .field("answers", &super::RedactedList(&self.answers))
            .field("multi", &self.multi)
            .field("closed", &self.closed)
            .field("counts", &self.counts)
            .field("revision", &self.revision)
            .finish_non_exhaustive()
    }
}

impl std::fmt::Debug for ReminderView {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ReminderView")
            .field("id", &self.id)
            .field("text", &super::Redacted(&self.text))
            .field("due", &self.due)
            .field("state", &self.state)
            .field("revision", &self.revision)
            .finish_non_exhaustive()
    }
}

impl BotState {
    /// The checks of a card in the log (`root == false`) or of the pane's root card (`true`).
    pub fn checks(&self, root: bool) -> &BTreeMap<MessageId, PollCheck> {
        if root {
            &self.root_poll_checks
        } else {
            &self.poll_checks
        }
    }

    /// [`BotState::checks`], to write.
    pub fn checks_mut(&mut self, root: bool) -> &mut BTreeMap<MessageId, PollCheck> {
        if root {
            &mut self.root_poll_checks
        } else {
            &mut self.poll_checks
        }
    }

    /// A read of `message`'s card ended (`Utilities::verify_poll`'s loop): `end` is its check from now on, `None` removing
    /// it (the poll was read). Only while the read's own check (`Pending`, or `Retry` for a re-read) is still there: a root
    /// check the pane dropped on closing ([`BotState::forget_root_checks`]) is not brought back by a read that outlived it.
    pub fn settle(&mut self, root: bool, message: MessageId, end: Option<PollCheck>) {
        let checks = self.checks_mut(root);
        if !checks.contains_key(&message) {
            return;
        }
        match end {
            Some(check) => {
                checks.insert(message, check);
            }
            None => {
                checks.remove(&message);
            }
        }
    }

    /// A read of `message`'s card stopped before it ended (its task panicked or the runtime shut down): a `Pending` check
    /// goes, so the card is not left loading for good, and a `Retry` is given up again.
    pub fn release_unfinished(&mut self, root: bool, message: MessageId) {
        let checks = self.checks_mut(root);
        match checks.get(&message) {
            Some(PollCheck::Pending) => {
                checks.remove(&message);
            }
            Some(&PollCheck::Retry(channel)) => {
                checks.insert(message, PollCheck::GaveUp(channel));
            }
            _ => {}
        }
    }

    /// PollCard.tsx: a pushed hint of `channel` reads its given-up polls again, the log's cards and the pane's root card
    /// alike. Each becomes `Retry`, which the next draw of its post turns into one read that leaves the card given up
    /// (nothing shows) until it succeeds. Other channels' are untouched, and a check still loading too.
    pub fn retry_gave_up(&mut self, channel: ChannelId) {
        for checks in [&mut self.poll_checks, &mut self.root_poll_checks] {
            for check in checks.values_mut() {
                if *check == PollCheck::GaveUp(channel) {
                    *check = PollCheck::Retry(channel);
                }
            }
        }
    }

    /// The thread pane stopped showing `root`'s thread (it closed, or another thread took its place): its root card's
    /// checks go, as the web's per-card state goes with the card, so reopening it reads the poll again (loading, then 2, 5
    /// and 10 s). The log's checks of the same post stay.
    pub fn forget_root_checks(&mut self, root: MessageId) {
        self.root_poll_checks.remove(&root);
    }
}

/// Where the read of a poll post's record stands.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum PollCheck {
    /// Being read, or waiting to read again: the card says it is loading.
    Pending,
    /// The Bot said no for good, or the retries ran out, for a post in this channel: the post's own text shows and no card
    /// is drawn, until a read of it succeeds (PollCard.tsx's `gaveUp`).
    GaveUp(crate::state::ChannelId),
    /// A given-up post being read again, as a pushed hint of its channel asks: it still shows as given up while it reads,
    /// and goes back to `GaveUp` if the read fails again.
    Retry(crate::state::ChannelId),
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
    /// The polls being read from the Bot, by the message that claims each (PollCard.tsx's retry state). A message with
    /// neither a check nor a held poll has not been asked about yet; a successful read removes its check.
    pub poll_checks: BTreeMap<MessageId, PollCheck>,
    /// The same state for a post drawn as its own thread's root in the thread pane (the web's `PollCard` keeps its state per
    /// card, with the thread in its scope: ThreadView passes `threadId = rootMsgId` to the root row too). That card reads the
    /// poll scoped to the thread, which a channel's poll is never in, so it ends given up; nothing of it is written to
    /// [`BotState::poll_checks`], which the log's card of the same post reads. A root's checks go when the pane stops showing
    /// its thread ([`BotState::forget_root_checks`]), as the web's card state goes with the card.
    pub root_poll_checks: BTreeMap<MessageId, PollCheck>,
    /// This member's reminders, by community, soonest first.
    pub reminders: BTreeMap<CommunityId, Vec<ReminderView>>,
    /// The scheduled announcements the Bot last listed, by community (Task 28; the chips' suggestions). A community with
    /// no entry was not listed yet.
    pub announcements: BTreeMap<CommunityId, Vec<crate::features::bot::announcements::ScheduleRow>>,
}

impl BotState {
    /// The private results of `conversation`, oldest first.
    pub fn results(&self, conversation: &Conversation) -> &[PrivateResult] {
        self.private_results
            .get(conversation)
            .map_or(&[], Vec::as_slice)
    }
}
