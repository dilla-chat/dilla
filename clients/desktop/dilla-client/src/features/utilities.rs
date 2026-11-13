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

use crate::client::{Client, Inner};
use crate::features::bot_admin::{BotArg, BotOperations, BotRequest, BotResult, OpError};
use crate::state::bot::{PollView, ReminderStatus, ReminderView};
use crate::state::{CommunityId, MessageId};

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

    /// Holds `community`'s reminders as the Bot answered them, soonest first.
    pub fn hold_reminders(&self, community: CommunityId, mut reminders: Vec<ReminderView>) {
        reminders.sort_by_key(|r| (r.due, r.id));
        self.inner.publish(move |s| {
            s.bot.reminders.insert(community, reminders);
        });
    }
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
}

/// The v1 operations that change something: an answer that cannot be read leaves them uncertain.
const MUTATIONS: [u64; 6] = [70, 73, 74, 80, 82, 83];

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
        let operation = request.operation;
        let result = self
            .operations(community)
            .request(request)
            .await
            .map_err(|e| match e {
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

    /// Reads the poll `message` carries (its final `[dilla-poll:v1:<id>]` line) from the Bot (71) and holds it.
    pub async fn read_poll(
        &self,
        community: CommunityId,
        message: MessageId,
        poll_id: [u8; 16],
    ) -> Result<PollView, UtilityError> {
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
            .map_err(|_| UtilityError::Invalid)?;
        self.hold_poll(message, poll.clone());
        Ok(poll)
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
}
