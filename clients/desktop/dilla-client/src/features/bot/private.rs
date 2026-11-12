//! Private results (packages/web/src/screens/commands/private-results.ts): the rows a command answers with that only this
//! device shows. There is no persistence and no transport: the rows are part of the in-memory [`BotState`] and nothing
//! else. A finished row that is not held is stamped with when it settled and goes away [`PRIVATE_RESULT_TTL_MS`] later.

use crate::state::bot::{
    BotState, Conversation, PRIVATE_RESULT_TTL_MS, PrivateResult, ResultProvider, ResultStatus,
};

/// What a row may change after it was added (`Partial<PrivateResult>`).
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Patch {
    pub status: Option<ResultStatus>,
    pub text: Option<String>,
    pub hold: Option<bool>,
}

/// `stamp`: a pending or held row carries no `settled_at`; a finished one keeps the previous stamp while its status and
/// hold did not change, else it is stamped `now`.
fn stamp(mut next: PrivateResult, prev: Option<&PrivateResult>, now: u64) -> PrivateResult {
    if next.status == ResultStatus::Pending || next.hold {
        next.settled_at = None;
        return next;
    }
    let keep = prev
        .filter(|p| p.status == next.status && !p.hold)
        .and_then(|p| p.settled_at);
    next.settled_at = Some(keep.unwrap_or(now));
    next
}

/// The most conversations whose rows are kept (the web's LRU of fifty).
pub const MAX_CONVERSATIONS: usize = 50;

/// Adds a row to `conversation` at `now` (unix ms), answers its id.
pub fn add(
    state: &mut BotState,
    conversation: Conversation,
    status: ResultStatus,
    text: String,
    provider: ResultProvider,
    id: u64,
    now: u64,
) -> u64 {
    let row = stamp(
        PrivateResult {
            id,
            at: now,
            status,
            text,
            provider,
            hold: false,
            settled_at: None,
        },
        None,
        now,
    );
    let rows = state.private_results.entry(conversation).or_default();
    rows.push(row);
    rows.sort_by_key(|r| r.at);
    if state.private_results.len() > MAX_CONVERSATIONS {
        // Drop the conversations whose newest row is oldest, never the one just written.
        let mut by_age: Vec<(u64, Conversation)> = state
            .private_results
            .iter()
            .filter(|(c, _)| **c != conversation)
            .map(|(c, rows)| (rows.iter().map(|r| r.at).max().unwrap_or(0), *c))
            .collect();
        by_age.sort();
        let excess = state.private_results.len() - MAX_CONVERSATIONS;
        for (_, c) in by_age.into_iter().take(excess) {
            state.private_results.remove(&c);
        }
    }
    id
}

/// Applies `patch` to the row `id` of `conversation`; `false` when there is no such row.
pub fn update(
    state: &mut BotState,
    conversation: &Conversation,
    id: u64,
    patch: Patch,
    now: u64,
) -> bool {
    let Some(rows) = state.private_results.get_mut(conversation) else {
        return false;
    };
    let Some(index) = rows.iter().position(|r| r.id == id) else {
        return false;
    };
    let prev = rows[index].clone();
    let mut next = prev.clone();
    if let Some(s) = patch.status {
        next.status = s;
    }
    if let Some(t) = patch.text {
        next.text = t;
    }
    if let Some(h) = patch.hold {
        next.hold = h;
    }
    rows[index] = stamp(next, Some(&prev), now);
    rows.sort_by_key(|r| r.at);
    true
}

/// Removes the row `id` (the Dismiss button); `false` when there is no such row.
pub fn dismiss(state: &mut BotState, conversation: &Conversation, id: u64) -> bool {
    let Some(rows) = state.private_results.get_mut(conversation) else {
        return false;
    };
    let before = rows.len();
    rows.retain(|r| r.id != id);
    let removed = rows.len() != before;
    if rows.is_empty() {
        state.private_results.remove(conversation);
    }
    removed
}

/// Removes every row whose time is up at `now` (unix ms); answers whether any went.
pub fn sweep(state: &mut BotState, now: u64) -> bool {
    let mut changed = false;
    state.private_results.retain(|_, rows| {
        let before = rows.len();
        rows.retain(|r| r.auto_dismiss_at().is_none_or(|at| at > now));
        changed |= rows.len() != before;
        !rows.is_empty()
    });
    changed
}

/// The earliest moment a row of `state` dismisses itself, if any.
pub fn next_expiry(state: &BotState) -> Option<u64> {
    state
        .private_results
        .values()
        .flatten()
        .filter_map(PrivateResult::auto_dismiss_at)
        .min()
}

/// The TTL, for callers that schedule the sweep.
pub const TTL_MS: u64 = PRIVATE_RESULT_TTL_MS;

#[cfg(test)]
mod tests {
    //! Ported from packages/web/src/screens/commands/private-results{,-ttl}.test.ts.
    use super::*;
    use crate::state::ChannelId;

    fn conv(n: u8) -> Conversation {
        Conversation {
            channel: ChannelId([n; 16]),
            thread: None,
        }
    }

    fn row(state: &mut BotState, c: Conversation, status: ResultStatus, id: u64, now: u64) -> u64 {
        add(
            state,
            c,
            status,
            "Saving your reminder…".into(),
            ResultProvider::Bot,
            id,
            now,
        )
    }

    #[test]
    fn stamps_finished_rows_and_restamps_on_a_status_change() {
        let mut s = BotState::default();
        let c = conv(1);
        let id = row(&mut s, c, ResultStatus::Pending, 1, 1000);
        assert_eq!(s.results(&c)[0].settled_at, None);
        let patch = |status: Option<ResultStatus>, text: Option<&str>, hold: Option<bool>| Patch {
            status,
            text: text.map(str::to_owned),
            hold,
        };
        update(
            &mut s,
            &c,
            id,
            patch(Some(ResultStatus::Success), None, None),
            5000,
        );
        assert_eq!(s.results(&c)[0].settled_at, Some(5000));
        update(&mut s, &c, id, patch(None, Some("again"), None), 9000);
        assert_eq!(s.results(&c)[0].settled_at, Some(5000));
        update(
            &mut s,
            &c,
            id,
            patch(Some(ResultStatus::Failed), None, None),
            9000,
        );
        assert_eq!(s.results(&c)[0].settled_at, Some(9000));
        update(&mut s, &c, id, patch(None, None, Some(true)), 9000);
        assert_eq!(s.results(&c)[0].settled_at, None);
        update(&mut s, &c, id, patch(None, None, Some(false)), 12000);
        assert_eq!(s.results(&c)[0].settled_at, Some(12000));
        let done = row(&mut s, c, ResultStatus::Cancelled, 2, 12000);
        assert_eq!(
            s.results(&c)
                .iter()
                .find(|r| r.id == done)
                .unwrap()
                .settled_at,
            Some(12000)
        );
        assert_eq!(TTL_MS, 60_000);
    }

    #[test]
    fn a_finished_row_goes_away_after_the_ttl_and_a_pending_one_stays() {
        let mut s = BotState::default();
        let c = conv(2);
        row(&mut s, c, ResultStatus::Success, 1, 1000);
        row(&mut s, c, ResultStatus::Pending, 2, 1000);
        assert_eq!(next_expiry(&s), Some(61_000));
        assert!(!sweep(&mut s, 60_999));
        assert_eq!(s.results(&c).len(), 2);
        assert!(sweep(&mut s, 61_000));
        assert_eq!(s.results(&c).iter().map(|r| r.id).collect::<Vec<_>>(), [2]);
        assert_eq!(next_expiry(&s), None);
    }

    #[test]
    fn conversations_are_kept_apart_and_dismiss_removes_one_row() {
        let mut s = BotState::default();
        row(&mut s, conv(1), ResultStatus::Success, 1, 10);
        row(&mut s, conv(2), ResultStatus::Success, 2, 20);
        assert_eq!(s.results(&conv(1)).len(), 1);
        assert!(dismiss(&mut s, &conv(1), 1));
        assert!(!dismiss(&mut s, &conv(1), 1));
        assert!(s.results(&conv(1)).is_empty());
        assert_eq!(s.results(&conv(2)).len(), 1);
    }

    #[test]
    fn at_most_fifty_conversations_keep_rows() {
        let mut s = BotState::default();
        for n in 0..60u8 {
            row(
                &mut s,
                conv(n),
                ResultStatus::Success,
                u64::from(n),
                u64::from(n),
            );
        }
        assert_eq!(s.private_results.len(), MAX_CONVERSATIONS);
        assert!(s.results(&conv(59)).len() == 1 && s.results(&conv(0)).is_empty());
    }

    #[test]
    fn rows_sort_by_when_they_were_added() {
        let mut s = BotState::default();
        row(&mut s, conv(1), ResultStatus::Success, 1, 50);
        row(&mut s, conv(1), ResultStatus::Success, 2, 10);
        assert_eq!(
            s.results(&conv(1)).iter().map(|r| r.id).collect::<Vec<_>>(),
            [2, 1]
        );
    }
}
