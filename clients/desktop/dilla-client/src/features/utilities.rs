//! Area **bot** (Task 20): the built-in utility commands (polls and reminders, which the community's Bot keeps).
//!
//! Ports: packages/client-core/src/bot/{utilities,utility-time}.ts.
//!
//! - [`time`]: reminder times (`in 1 hour`, `2026-10-26 09:00`), folds included.
//! - [`polls`]: the poll marker, the canonical poll payloads and a card's model.
//!
//! The polls and reminders themselves are published in [`crate::state::bot::BotState`].

pub mod polls;
pub mod time;
pub mod wire;

use std::sync::Arc;
use std::time::Duration;

use crate::client::{Client, Inner};
use crate::features::bot_admin::{BotArg, BotOperations, BotRequest, BotResult, OpError};
use crate::state::bot::{PollCheck, PollView, ReminderStatus, ReminderView};
use crate::state::{ChannelId, CommunityId, MessageId, TimelineItem, TimelineItemState, UserId};

/// The bot area's handle for utilities: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Utilities {
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Utilities`] handle.
    pub fn utilities(&self) -> Utilities {
        Utilities {
            inner: self.inner().clone(),
        }
    }
}

impl Utilities {
    /// The verified poll carried by `message`, as this device holds it.
    pub fn poll(&self, message: MessageId) -> Option<PollView> {
        self.inner.snapshot().bot.polls.get(&message).cloned()
    }

    /// Holds `poll` as the verified record of `message` (the Bot's answer to a poll read).
    pub fn hold_poll(&self, message: MessageId, poll: PollView) {
        self.inner.publish(move |s| {
            s.bot.polls.insert(message, poll);
        });
    }

    /// Starts reading the record of the poll the post `item` claims, once. Nothing happens when a check is under way or
    /// given up, or the poll is held. Otherwise the check is `Pending` and [`Utilities::read_poll`] runs on the client's
    /// runtime: success holds the poll and removes the check; a [`definitive`] failure or the last retry ([`retry_after`])
    /// gives up; any other failure waits and reads again. A given-up post whose channel had a hint (`Retry`, see
    /// [`retry_gave_up_in`]) is read once more without leaving the given-up state: success removes the check, failure
    /// gives up again. Only `read_poll` decides a post is a verified poll, so a forged or unreadable one never gets a card.
    pub fn verify_poll(
        &self,
        community: CommunityId,
        channel: ChannelId,
        thread: Option<MessageId>,
        item: TimelineItem,
    ) {
        let Some(message) = item.msg_id else { return };
        let snapshot = self.inner.snapshot();
        let again = match snapshot.bot.poll_checks.get(&message) {
            None => false,
            Some(PollCheck::Retry(_)) => true,
            Some(_) => return,
        };
        if snapshot.bot.polls.contains_key(&message) {
            return;
        }
        // The snapshot lags the publish below by a moment, and a view draws again in between: one read at a time per
        // message, which the guard frees when the read ends or its runtime stops.
        let Some(guard) = ReadingGuard::enter(&self.inner, message) else {
            return;
        };
        if !again {
            self.inner.publish(move |s| {
                s.bot
                    .poll_checks
                    .entry(message)
                    .or_insert(PollCheck::Pending);
            });
        }
        let this = self.clone();
        // The answer is only the check's state in the snapshot, so the receiver is not awaited.
        drop(self.inner.spawn(async move {
            // The whole guard moves into the task (a field assignment alone would capture only the flag).
            let mut guard = guard;
            let mut failures = 0;
            loop {
                let end = match this.read_poll(community, channel, thread, &item).await {
                    Ok(_) => None,
                    Err(e) if definitive(&e) || again => Some(PollCheck::GaveUp(channel)),
                    Err(_) => match retry_after(failures) {
                        Some(wait) => {
                            failures += 1;
                            tokio::time::sleep(wait).await;
                            continue;
                        }
                        None => Some(PollCheck::GaveUp(channel)),
                    },
                };
                this.inner.publish(move |s| match end {
                    Some(check) => {
                        s.bot.poll_checks.insert(message, check);
                    }
                    None => {
                        s.bot.poll_checks.remove(&message);
                    }
                });
                guard.finished = true;
                return Ok(());
            }
        }));
    }

    /// This member's reminders in `community`, soonest first.
    pub fn reminders(&self, community: CommunityId) -> Vec<ReminderView> {
        self.inner
            .snapshot()
            .bot
            .reminders
            .get(&community)
            .cloned()
            .unwrap_or_default()
    }

    /// Runs `job` on the client's own runtime (the Bot's operations need its reactor), so a view can await the answer on
    /// any executor. A client that stopped meanwhile answers `Unavailable("E_STOPPED")`.
    pub fn run<T: Send + 'static, F, J: FnOnce(Utilities) -> F>(
        &self,
        job: J,
    ) -> impl std::future::Future<Output = Result<T, UtilityError>> + use<T, F, J>
    where
        F: std::future::Future<Output = Result<T, UtilityError>> + Send + 'static,
    {
        let work = job(self.clone());
        let rx = self.inner.spawn(async move { Ok(work.await) });
        async move {
            match rx.await {
                Ok(Ok(answer)) => answer,
                _ => Err(UtilityError::Unavailable("E_STOPPED".into())),
            }
        }
    }

    /// The Bot this device talks to in `community` (UtilityCommandProvider.tsx's `installationId` and `botUserId`): the
    /// Bot user it pinned and its verified command key, which a reinstalled Bot changes. `None` while none is pinned.
    pub fn bot_identity(&self, community: CommunityId) -> Option<BotIdentity> {
        let view = crate::features::bot::Bot::of(&self.inner)
            .sealed()
            .view(community)?;
        Some(BotIdentity {
            user: view.bot_user?,
            key: view.fingerprint,
        })
    }

    /// Holds `community`'s reminders as the Bot answered them, soonest first.
    pub fn hold_reminders(&self, community: CommunityId, mut reminders: Vec<ReminderView>) {
        reminders.sort_by_key(|r| (r.due, r.id));
        self.inner.publish(move |s| {
            s.bot.reminders.insert(community, reminders);
        });
    }
}

/// Which Bot answers a community ([`Utilities::bot_identity`]).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct BotIdentity {
    pub user: crate::state::UserId,
    /// The verified command key's fingerprint; `None` while none is checked.
    pub key: Option<String>,
}

/// Why a poll or reminder action did not go through (utilities.ts errors).
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
pub enum UtilityError {
    /// The Bot refused or never took it (`provider-unavailable`).
    #[error("the Bot could not take it ({0})")]
    Unavailable(String),
    /// A mutation may have happened: reconcile with the same request id before trying again.
    #[error("uncertain-mutation")]
    Uncertain { request_id: [u8; 16] },
    /// The Bot's status for it (1 invalid, 2 forbidden, 3 stale, 4 conflict, ...).
    #[error("interaction-status {0}")]
    Status(u8),
    #[error("invalid-response")]
    Invalid,
    #[error("invalid-payload")]
    Payload,
    #[error("no such poll")]
    NoPoll,
    /// The post is no poll this device may show: not the pinned Bot's, edited, deleted, or no final marker.
    #[error("not a poll")]
    NotAPoll,
}

/// PollCard.tsx's gate: the post's message id and the poll id its final line names, only for a post by the community's
/// pinned Bot user that is neither edited nor deleted. A member who reposts a Bot's marker gets no card.
pub fn poll_claim(
    item: &TimelineItem,
    pinned_bot: Option<UserId>,
) -> Option<(MessageId, [u8; 16])> {
    let pinned = pinned_bot?;
    if item.edited || item.state != TimelineItemState::Ok || item.sender_user != Some(pinned) {
        return None;
    }
    Some((item.msg_id?, polls::final_poll_marker(&item.body)?))
}

/// The Bot's record is for the channel and thread the post is in.
pub fn poll_in_place(poll: &PollView, channel: ChannelId, thread: Option<MessageId>) -> bool {
    poll.channel == channel && poll.thread == thread
}

/// PollCard.tsx `RETRY_DELAYS`: a failed read tries again after 2, 5 and 10 seconds, then gives up. `failures` is how many
/// reads have failed so far.
pub fn retry_after(failures: usize) -> Option<Duration> {
    [2, 5, 10].get(failures).map(|s| Duration::from_secs(*s))
}

/// A failure that retrying cannot mend, as PollCard.tsx has it: the post is no poll this device may show (the author is
/// not the pinned Bot), or the Bot refused the read (`botDefinitelyUnavailable`: every refusal of an operation but op 3,
/// which includes status 5 on the poll read). Everything else is retried 2, 5 and 10 s: a read that got no answer in time
/// (`E_BOT_TIMEOUT`), a Bot status, a record for another channel or thread, an answer that cannot be read (`Invalid`,
/// `Payload`, an answer envelope that cannot be read: `E_BOT_ANSWER`), and a client that is stopping (`E_STOPPED`).
pub fn definitive(e: &UtilityError) -> bool {
    match e {
        UtilityError::NotAPoll => true,
        UtilityError::Unavailable(code) => !matches!(
            code.as_str(),
            "E_BOT_TIMEOUT" | "E_BOT_ANSWER" | "E_STOPPED"
        ),
        _ => false,
    }
}

/// PollCard.tsx: a pushed hint of `channel` reads its given-up polls again. Each becomes `Retry`, which the next draw of
/// its post turns into one read that leaves the card given up (nothing shows) until it succeeds. Other channels' are
/// untouched.
pub(crate) fn retry_gave_up_in(inner: &Arc<Inner>, channel: ChannelId) {
    inner.publish(move |s| retry_checks_in(&mut s.bot.poll_checks, channel));
}

fn retry_checks_in(
    checks: &mut std::collections::BTreeMap<MessageId, PollCheck>,
    channel: ChannelId,
) {
    for check in checks.values_mut() {
        if *check == PollCheck::GaveUp(channel) {
            *check = PollCheck::Retry(channel);
        }
    }
}

/// The poll reads under way: one per client and message ([`Utilities::verify_poll`]). A client is told apart by its
/// `Inner`'s address, as `HINTS` in bot.rs tells them apart.
static READING: std::sync::Mutex<std::collections::BTreeSet<(usize, MessageId)>> =
    std::sync::Mutex::new(std::collections::BTreeSet::new());

/// One message's read under way: it holds its place in [`READING`] and, when dropped before the read finished (the task
/// panicked, or the runtime shut down), takes its `Pending` check back so the card is not left loading for good.
struct ReadingGuard {
    inner: Arc<Inner>,
    message: MessageId,
    finished: bool,
}

impl ReadingGuard {
    fn key(inner: &Arc<Inner>, message: MessageId) -> (usize, MessageId) {
        (Arc::as_ptr(inner) as usize, message)
    }

    fn enter(inner: &Arc<Inner>, message: MessageId) -> Option<Self> {
        let mut reading = READING.lock().unwrap_or_else(|e| e.into_inner());
        reading.insert(Self::key(inner, message)).then(|| Self {
            inner: inner.clone(),
            message,
            finished: false,
        })
    }
}

impl Drop for ReadingGuard {
    fn drop(&mut self) {
        READING
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .remove(&Self::key(&self.inner, self.message));
        if !self.finished {
            let message = self.message;
            self.inner
                .publish(move |s| match s.bot.poll_checks.get(&message) {
                    Some(PollCheck::Pending) => {
                        s.bot.poll_checks.remove(&message);
                    }
                    Some(&PollCheck::Retry(channel)) => {
                        s.bot
                            .poll_checks
                            .insert(message, PollCheck::GaveUp(channel));
                    }
                    _ => {}
                });
        }
    }
}

/// The v1 operations that change something: an answer that cannot be read leaves them uncertain.
const MUTATIONS: [u64; 6] = [70, 73, 74, 80, 82, 83];

/// Sends `request` over `ops` and reads the Bot's status: a mutation that cannot be read is uncertain.
async fn send_over(ops: &BotOperations, request: BotRequest) -> Result<BotResult, UtilityError> {
    let operation = request.operation;
    let result = ops.request(request).await.map_err(|e| match e {
        OpError::Uncertain { request_id } if MUTATIONS.contains(&operation) => {
            UtilityError::Uncertain { request_id }
        }
        OpError::Uncertain { .. } => UtilityError::Unavailable("E_BOT_TIMEOUT".into()),
        OpError::Refused { code, .. } => UtilityError::Unavailable(code),
        OpError::Invalid => UtilityError::Payload,
    })?;
    match result.status {
        0 => Ok(result),
        5 if MUTATIONS.contains(&operation) => Err(UtilityError::Uncertain {
            request_id: result.request_id,
        }),
        5 => Err(UtilityError::Unavailable("E_UNAVAILABLE".into())),
        s => Err(UtilityError::Status(s)),
    }
}

/// `pollCreate` (utilities.ts:649-653): operation 70 at revision 0 in the poll's channel, with the one canonical payload.
/// The Bot answers the new poll; its id is the request id, it is for the form's channel and thread, and no vote is on it
/// (`decodePollCreate`: `my_choices` must be the empty list), else the mutation is uncertain.
async fn create_poll_over(
    ops: &BotOperations,
    input: &polls::PollCreate,
    request: [u8; 16],
) -> Result<PollView, UtilityError> {
    let payload = polls::encode_poll_create(input).map_err(|_| UtilityError::Payload)?;
    let result = send_over(
        ops,
        BotRequest {
            operation: 70,
            channel: Some(ChannelId(input.channel)),
            args: vec![BotArg::Bytes(payload)],
            revision: Some(0),
            request_id: Some(request),
        },
    )
    .await?;
    // An accepted answer that cannot be read as the new poll leaves the mutation uncertain (`decodeAcceptedMutation`): the
    // poll may exist, so a resend with this id settles it.
    let poll = wire::decode_poll(&result.data, request, None)
        .ok()
        .filter(|p| {
            poll_in_place(p, ChannelId(input.channel), input.thread.map(MessageId))
                && p.my_choices.as_deref() == Some(&[])
                && result.revision == Some(p.revision)
        })
        .ok_or(UtilityError::Uncertain {
            request_id: request,
        })?;
    Ok(poll)
}

fn hex16(b: &[u8; 16]) -> String {
    crate::state::hex(b)
}

impl Utilities {
    fn operations(&self, community: CommunityId) -> BotOperations {
        let bot = crate::features::bot::Bot::of(&self.inner);
        BotOperations::new(community, bot.transport())
    }

    async fn send(
        &self,
        community: CommunityId,
        request: BotRequest,
    ) -> Result<BotResult, UtilityError> {
        send_over(&self.operations(community), request).await
    }

    /// Reads the poll the post `item` (in `channel`, `thread`) claims to carry from the Bot (71) and holds it. Nothing is
    /// asked unless the post is the pinned Bot's, unedited and not deleted, with a final `[dilla-poll:v1:<id>]` line; the
    /// record must be for that channel and thread (PollCard.tsx).
    pub async fn read_poll(
        &self,
        community: CommunityId,
        channel: ChannelId,
        thread: Option<MessageId>,
        item: &TimelineItem,
    ) -> Result<PollView, UtilityError> {
        let bot = crate::features::bot::Bot::of(&self.inner);
        let pinned = bot.sealed().view(community).and_then(|v| v.bot_user);
        let (message, poll_id) = poll_claim(item, pinned).ok_or(UtilityError::NotAPoll)?;
        let result = self
            .send(
                community,
                BotRequest {
                    operation: 71,
                    args: vec![BotArg::Text(hex16(&poll_id))],
                    ..BotRequest::default()
                },
            )
            .await?;
        let prior = self.poll(message);
        let poll = wire::decode_poll(&result.data, poll_id, prior.as_ref())
            .ok()
            .filter(|p| poll_in_place(p, channel, thread))
            .ok_or(UtilityError::Invalid)?;
        self.hold_poll(message, poll.clone());
        Ok(poll)
    }

    /// Reads the poll `id` from `community`'s Bot (71), as [`Utilities::read_poll`] decodes it, without holding it: a
    /// poll's id is its create request id, so this settles a create whose answer was uncertain (the web's
    /// `pollGet(requestId, {channelId, threadId})`, UtilityCommandProvider.tsx:369). A record for another channel or
    /// thread is `Invalid`.
    pub async fn poll_by_id(
        &self,
        community: CommunityId,
        id: [u8; 16],
        channel: ChannelId,
        thread: Option<MessageId>,
    ) -> Result<PollView, UtilityError> {
        let result = self
            .send(
                community,
                BotRequest {
                    operation: 71,
                    args: vec![BotArg::Text(hex16(&id))],
                    ..BotRequest::default()
                },
            )
            .await?;
        wire::decode_poll(&result.data, id, None)
            .ok()
            .filter(|p| poll_in_place(p, channel, thread))
            .ok_or(UtilityError::Invalid)
    }

    /// Creates the poll of `input` in `community`'s Bot (70) under `request`: the form's one id, so a send that is
    /// repeated after an unanswered one settles the same poll instead of making a second. The poll is held under no
    /// message until its post is seen.
    pub async fn create_poll(
        &self,
        community: CommunityId,
        input: polls::PollCreate,
        request: [u8; 16],
    ) -> Result<PollView, UtilityError> {
        create_poll_over(&self.operations(community), &input, request).await
    }

    /// Votes on the held poll of `message` (73): `choices` replace this member's; an empty set withdraws the vote.
    pub async fn vote(
        &self,
        community: CommunityId,
        message: MessageId,
        choices: Vec<usize>,
    ) -> Result<PollView, UtilityError> {
        let poll = self.poll(message).ok_or(UtilityError::NoPoll)?;
        let selection =
            polls::encode_poll_selection(&choices).map_err(|_| UtilityError::Payload)?;
        let result = self
            .send(
                community,
                BotRequest {
                    operation: 73,
                    channel: Some(poll.channel),
                    args: vec![BotArg::Text(hex16(&poll.id)), BotArg::Bytes(selection)],
                    revision: Some(poll.revision),
                    ..BotRequest::default()
                },
            )
            .await?;
        let next = wire::decode_poll(&result.data, poll.id, Some(&poll)).map_err(|_| {
            UtilityError::Uncertain {
                request_id: result.request_id,
            }
        })?;
        if next.my_choices.as_deref() != Some(&choices[..]) {
            return Err(UtilityError::Uncertain {
                request_id: result.request_id,
            });
        }
        self.hold_poll(message, next.clone());
        Ok(next)
    }

    /// Closes the held poll of `message` (74; the Bot allows its creator and moderators).
    pub async fn close(
        &self,
        community: CommunityId,
        message: MessageId,
    ) -> Result<PollView, UtilityError> {
        let poll = self.poll(message).ok_or(UtilityError::NoPoll)?;
        let result = self
            .send(
                community,
                BotRequest {
                    operation: 74,
                    channel: Some(poll.channel),
                    args: vec![BotArg::Text(hex16(&poll.id))],
                    revision: Some(poll.revision),
                    ..BotRequest::default()
                },
            )
            .await?;
        let next = wire::decode_poll(&result.data, poll.id, Some(&poll))
            .ok()
            .filter(|p| p.closed)
            .ok_or(UtilityError::Uncertain {
                request_id: result.request_id,
            })?;
        self.hold_poll(message, next.clone());
        Ok(next)
    }

    /// Reads this member's open reminders (81) and the due ones (84), and holds them.
    pub async fn refresh_reminders(
        &self,
        community: CommunityId,
    ) -> Result<Vec<ReminderView>, UtilityError> {
        let mut all = Vec::new();
        for operation in [81, 84] {
            let mut cursor: Option<Vec<u8>> = None;
            for _ in 0..50 {
                let result = self
                    .send(
                        community,
                        BotRequest {
                            operation,
                            args: vec![cursor.clone().map_or(BotArg::Null, BotArg::Bytes)],
                            ..BotRequest::default()
                        },
                    )
                    .await?;
                let (items, next) =
                    wire::decode_reminder_page(&result.data).map_err(|_| UtilityError::Invalid)?;
                for r in items {
                    if !all.iter().any(|h: &ReminderView| h.id == r.id) {
                        all.push(r);
                    }
                }
                match next {
                    Some(c) => cursor = Some(c),
                    None => break,
                }
            }
        }
        self.hold_reminders(community, all.clone());
        Ok(all)
    }

    /// Cancels an upcoming reminder (82) or dismisses a due one (83).
    pub async fn settle_reminder(
        &self,
        community: CommunityId,
        reminder: [u8; 16],
        dismiss: bool,
    ) -> Result<ReminderView, UtilityError> {
        let held = self
            .reminders(community)
            .into_iter()
            .find(|r| r.id == reminder);
        let revision = held.as_ref().map_or(0, |r| r.revision);
        let (operation, state) = if dismiss {
            (83, ReminderStatus::Dismissed)
        } else {
            (82, ReminderStatus::Cancelled)
        };
        let result = self
            .send(
                community,
                BotRequest {
                    operation,
                    args: vec![BotArg::Text(hex16(&reminder))],
                    revision: Some(revision),
                    ..BotRequest::default()
                },
            )
            .await?;
        let r = wire::decode_reminder(&result.data, reminder, state).map_err(|_| {
            UtilityError::Uncertain {
                request_id: result.request_id,
            }
        })?;
        let mut kept = self.reminders(community);
        kept.retain(|h| h.id != reminder);
        self.hold_reminders(community, kept);
        Ok(r)
    }
}

/// The Upcoming and Due lists of the reminders popover: scheduled ones still ahead, and the ones that are due.
pub fn split_reminders(
    reminders: &[ReminderView],
    now: u64,
) -> (Vec<&ReminderView>, Vec<&ReminderView>) {
    let upcoming = reminders
        .iter()
        .filter(|r| r.state == ReminderStatus::Scheduled && r.due > now)
        .collect();
    let due = reminders
        .iter()
        .filter(|r| {
            r.state == ReminderStatus::Due || (r.state == ReminderStatus::Scheduled && r.due <= now)
        })
        .collect();
    (upcoming, due)
}

#[cfg(test)]
mod tests {
    use super::*;

    const BOT: UserId = UserId([0xb0; 16]);
    const MARKER: &str = "[dilla-poll:v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa]";

    fn post(sender: UserId, body: &str) -> TimelineItem {
        TimelineItem {
            key: "m".into(),
            state: TimelineItemState::Ok,
            reason: String::new(),
            sender_user: Some(sender),
            sender_device: None,
            own: false,
            bot: true,
            ts: 1,
            body: body.into(),
            msg_id: Some(MessageId([1; 16])),
            seq: Some(1),
            edited: false,
            reply: None,
            reactions: vec![],
            pinned: false,
            attachments: vec![],
            mention: false,
            actions: Vec::new(),
        }
    }

    /// PollCard.tsx `RETRY_DELAYS`: 2, 5 and 10 seconds, then no more.
    #[test]
    fn poll_retries_follow_the_web() {
        assert_eq!(retry_after(0), Some(Duration::from_secs(2)));
        assert_eq!(retry_after(1), Some(Duration::from_secs(5)));
        assert_eq!(retry_after(2), Some(Duration::from_secs(10)));
        assert_eq!(retry_after(3), None);
    }

    /// PollCard.tsx: a post that is no poll, a refusal and status 5 on the poll read (`botDefinitelyUnavailable`) give up at
    /// once. A Bot status is tried again.
    #[test]
    fn definitive_failures_give_up_at_once() {
        assert!(definitive(&UtilityError::NotAPoll));
        assert!(definitive(&UtilityError::Unavailable("E_NO_BOT".into())));
        // Status 5 on a read is the Bot's `provider-unavailable` (`send_over`): the web gives up at once.
        assert!(definitive(&UtilityError::Unavailable(
            "E_UNAVAILABLE".into()
        )));
        assert!(!definitive(&UtilityError::Status(3)));
    }

    /// A read that got no answer in time is retried, not given up (`BotOperationUncertainError` is rethrown by the web).
    #[test]
    fn a_read_timeout_is_retried() {
        assert!(!definitive(&UtilityError::Unavailable(
            "E_BOT_TIMEOUT".into()
        )));
        assert!(!definitive(&UtilityError::Unavailable("E_STOPPED".into())));
    }

    /// PollCard.tsx: a hint of one channel reads that channel's given-up posts again, and only those: the others stay given
    /// up, and a post still loading is not touched.
    #[test]
    fn a_hint_retries_only_its_channels_given_up_posts() {
        let (here, there) = (ChannelId([1; 16]), ChannelId([2; 16]));
        let id = |n: u8| MessageId([n; 16]);
        let mut checks = std::collections::BTreeMap::from([
            (id(1), PollCheck::GaveUp(here)),
            (id(2), PollCheck::GaveUp(there)),
            (id(3), PollCheck::Pending),
        ]);
        retry_checks_in(&mut checks, here);
        assert_eq!(checks[&id(1)], PollCheck::Retry(here));
        assert_eq!(checks[&id(2)], PollCheck::GaveUp(there));
        assert_eq!(checks[&id(3)], PollCheck::Pending);
    }

    /// An answer envelope the transport cannot decode is a plain failure in the web: retried.
    #[test]
    fn an_unreadable_answer_envelope_is_retried() {
        assert!(!definitive(&UtilityError::Unavailable(
            "E_BOT_ANSWER".into()
        )));
    }

    /// A record for another channel or thread, and an answer that cannot be decoded, are plain failures in the web, so
    /// they are retried 2, 5 and 10 s.
    #[test]
    fn another_place_and_an_unreadable_answer_are_retried() {
        assert!(!definitive(&UtilityError::Invalid));
        assert!(!definitive(&UtilityError::Payload));
    }

    /// slash-commands.spec.ts "reject forged markers": a member reposting the Bot's `[dilla-poll:v1:aaaa…]` line gets no
    /// card, nor does an edited or deleted Bot post, nor any post before a Bot user is pinned.
    #[test]
    fn only_the_pinned_bots_untouched_post_claims_a_poll() {
        let body = format!("Mira asked:\n{MARKER}");
        assert_eq!(
            poll_claim(&post(BOT, &body), Some(BOT)),
            Some((MessageId([1; 16]), [0xaa; 16]))
        );
        let mut repost = post(UserId([0x0a; 16]), &body);
        repost.bot = false;
        assert_eq!(
            poll_claim(&repost, Some(BOT)),
            None,
            "a member's repost of the marker"
        );
        assert_eq!(
            poll_claim(&post(UserId([0xb1; 16]), &body), Some(BOT)),
            None,
            "another Bot account"
        );
        let mut edited = post(BOT, &body);
        edited.edited = true;
        assert_eq!(poll_claim(&edited, Some(BOT)), None);
        let mut deleted = post(BOT, &body);
        deleted.state = TimelineItemState::Deleted;
        assert_eq!(poll_claim(&deleted, Some(BOT)), None);
        assert_eq!(
            poll_claim(&post(BOT, &body), None),
            None,
            "no pinned Bot yet"
        );
        assert_eq!(
            poll_claim(&post(BOT, &format!("{MARKER}\nmore")), Some(BOT)),
            None,
            "not the final line"
        );
    }

    #[test]
    fn the_record_must_be_for_the_posts_channel_and_thread() {
        let data = crate::gateway::frames::Value::Array(vec![
            crate::gateway::frames::Value::Uint(1),
            wire::tests::poll_value([1; 16], [0, 0], Some(vec![]), false),
        ])
        .encode();
        let poll = wire::decode_poll(&data, [1; 16], None).unwrap();
        assert!(poll_in_place(&poll, ChannelId([2; 16]), None));
        assert!(
            !poll_in_place(&poll, ChannelId([3; 16]), None),
            "another channel"
        );
        assert!(
            !poll_in_place(&poll, ChannelId([2; 16]), Some(MessageId([4; 16]))),
            "a thread"
        );
    }

    fn r(id: u8, due: u64, state: ReminderStatus) -> ReminderView {
        ReminderView {
            id: [id; 16],
            text: format!("r{id}"),
            due,
            timezone: "UTC".into(),
            source_channel: None,
            source_thread: None,
            state,
            revision: 1,
        }
    }

    #[test]
    fn upcoming_and_due_are_split_by_state_and_time() {
        let all = [
            r(1, 50, ReminderStatus::Scheduled),
            r(2, 150, ReminderStatus::Scheduled),
            r(3, 10, ReminderStatus::Due),
            r(4, 200, ReminderStatus::Cancelled),
        ];
        let (upcoming, due) = split_reminders(&all, 100);
        assert_eq!(upcoming.iter().map(|r| r.id[0]).collect::<Vec<_>>(), [2]);
        assert_eq!(due.iter().map(|r| r.id[0]).collect::<Vec<_>>(), [1, 3]);
    }

    /// A transport that answers from a closure and records every command it was sent (bot_admin.rs's test transport).
    struct Fake {
        sent: std::sync::Mutex<Vec<crate::features::bot_admin::SealedCommand>>,
        data: Vec<u8>,
    }

    #[async_trait::async_trait]
    impl crate::features::bot_admin::SealedBot for Fake {
        async fn send(
            &self,
            _: CommunityId,
            c: crate::features::bot_admin::SealedCommand,
        ) -> Result<crate::features::bot_admin::SealedAnswer, crate::features::bot_admin::SendError>
        {
            self.sent.lock().unwrap().push(c);
            let body = crate::gateway::frames::Value::Array(vec![
                crate::gateway::frames::Value::Uint(4),
                crate::gateway::frames::Value::Bytes(self.data.clone()),
            ])
            .encode();
            Ok(crate::features::bot_admin::SealedAnswer { status: 200, body })
        }
    }

    /// The Bot's answer to a create: the poll `[1; 16]` with `mine` as the vote it carries.
    fn created(mine: Option<Vec<u64>>) -> Vec<u8> {
        crate::gateway::frames::Value::Array(vec![
            crate::gateway::frames::Value::Uint(1),
            wire::tests::poll_value([1; 16], [0, 0], mine, false),
        ])
        .encode()
    }

    fn run<T>(f: impl std::future::Future<Output = T>) -> T {
        tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap()
            .block_on(f)
    }

    /// utilities.ts:649-653: the form's poll goes as operation 70 at revision 0 with the one canonical payload, under the
    /// form's request id; an answer that already carries a vote is no fresh poll (`decodePollCreate`).
    #[test]
    fn create_poll_sends_operation_70_at_revision_0() {
        let input = polls::PollCreate {
            channel: [2; 16],
            thread: None,
            question: "Raid?".into(),
            answers: vec!["the bridge".into(), "the harbour".into()],
            multi: false,
            duration: 86_400,
        };
        let community = CommunityId([9; 16]);
        let sent_with = |data: Vec<u8>| {
            let fake = Arc::new(Fake {
                sent: std::sync::Mutex::default(),
                data,
            });
            let ops = BotOperations::new(community, fake.clone());
            let answer = run(create_poll_over(&ops, &input, [1; 16]));
            (answer, fake.sent.lock().unwrap().clone())
        };
        let (answer, sent) = sent_with(created(Some(vec![])));
        let poll = answer.expect("a fresh poll");
        assert_eq!(poll.id, [1; 16]);
        assert_eq!(sent.len(), 1);
        assert_eq!(
            sent[0].operation,
            crate::state::bot_admin::sealed_operation(70)
        );
        assert_eq!(sent[0].channel, Some(ChannelId([2; 16])));
        assert_eq!(sent[0].request_id, [1; 16]);
        assert_eq!(
            sent[0].args,
            [
                BotArg::Uint(0),
                BotArg::Bytes(polls::encode_poll_create(&input).unwrap())
            ],
            "revision 0, then the one payload"
        );
        for mine in [Some(vec![0]), None] {
            assert_eq!(
                sent_with(created(mine.clone())).0,
                Err(UtilityError::Uncertain {
                    request_id: [1; 16]
                }),
                "{mine:?}"
            );
        }
        let mut long = input.clone();
        long.question = "q".repeat(301);
        let fake = Arc::new(Fake {
            sent: std::sync::Mutex::default(),
            data: created(Some(vec![])),
        });
        let ops = BotOperations::new(community, fake.clone());
        assert_eq!(
            run(create_poll_over(&ops, &long, [1; 16])),
            Err(UtilityError::Payload)
        );
        assert!(fake.sent.lock().unwrap().is_empty(), "nothing was sent");
    }
}
