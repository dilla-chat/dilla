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

use std::sync::Arc;

use crate::client::{Client, Inner};
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
