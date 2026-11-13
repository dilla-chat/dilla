//! The Bot's poll and reminder answers (utilities.ts `readPollSummary`, `readReminderSummary` and their envelopes), read
//! strictly: a field out of its bounds refuses the whole answer.

use crate::gateway::frames::Value;
use crate::state::bot::{PollView, ReminderStatus, ReminderView};
use crate::state::{ChannelId, MessageId, UserId};

use super::polls::POLL_DURATIONS;

const MAX_UNIX_SECONDS: u64 = 253_402_300_799;

/// `invalid-response`.
#[derive(Clone, Copy, Debug, PartialEq, Eq, thiserror::Error)]
#[error("invalid-response")]
pub struct Invalid;

fn arr(v: &Value, n: usize) -> Result<&[Value], Invalid> {
    v.as_array().filter(|a| a.len() == n).ok_or(Invalid)
}

fn list(v: &Value, min: usize, max: usize) -> Result<&[Value], Invalid> {
    v.as_array()
        .filter(|a| (min..=max).contains(&a.len()))
        .ok_or(Invalid)
}

fn uint(v: &Value, min: u64, max: u64) -> Result<u64, Invalid> {
    v.as_uint()
        .filter(|n| (min..=max).contains(n))
        .ok_or(Invalid)
}

fn id(v: &Value) -> Result<[u8; 16], Invalid> {
    let b: [u8; 16] = v
        .as_bytes()
        .ok_or(Invalid)?
        .try_into()
        .map_err(|_| Invalid)?;
    if b == [0; 16] { Err(Invalid) } else { Ok(b) }
}

fn opt_id(v: &Value) -> Result<Option<[u8; 16]>, Invalid> {
    match v {
        Value::Null => Ok(None),
        other => id(other).map(Some),
    }
}

fn text(
    v: &Value,
    max_bytes: usize,
    max_chars: Option<usize>,
    non_blank: bool,
) -> Result<String, Invalid> {
    let t = v.as_text().ok_or(Invalid)?;
    if t.is_empty()
        || t.len() > max_bytes
        || max_chars.is_some_and(|m| t.chars().count() > m)
        || (non_blank && t.trim().is_empty())
    {
        return Err(Invalid);
    }
    Ok(t.to_owned())
}

/// `versionOne`: `[1, ...]` of exactly `n` fields.
fn envelope(data: &[u8], n: usize) -> Result<Vec<Value>, Invalid> {
    if data.is_empty() || data.len() > 2400 {
        return Err(Invalid);
    }
    let v = Value::decode(data).map_err(|_| Invalid)?;
    let a = arr(&v, n)?;
    if a[0].as_uint() != Some(1) {
        return Err(Invalid);
    }
    Ok(a.to_vec())
}

/// `readPollSummary`, with the voters of the card kept from `prior`.
pub fn read_poll(v: &Value, prior: Option<&PollView>) -> Result<PollView, Invalid> {
    let t = arr(v, 13)?;
    let poll_id = id(&t[0])?;
    let channel = ChannelId(id(&t[1])?);
    let _thread = opt_id(&t[2])?;
    let creator = UserId(id(&t[3])?);
    let question = text(&t[4], usize::MAX, Some(300), true)?;
    let answers = list(&t[5], 2, 10)?
        .iter()
        .map(|a| text(a, usize::MAX, Some(100), true))
        .collect::<Result<Vec<_>, _>>()?;
    let mut unique = answers.clone();
    unique.sort();
    unique.dedup();
    if unique.len() != answers.len() {
        return Err(Invalid);
    }
    let multi = uint(&t[6], 0, 1)? == 1;
    let created_at = uint(&t[7], 1, MAX_UNIX_SECONDS)?;
    let due_at = uint(&t[8], 1, MAX_UNIX_SECONDS)?;
    if !POLL_DURATIONS.contains(&due_at.checked_sub(created_at).ok_or(Invalid)?) {
        return Err(Invalid);
    }
    let closed = uint(&t[9], 0, 1)? == 1;
    let counts = list(&t[10], answers.len(), answers.len())?
        .iter()
        .map(|c| uint(c, 0, u64::MAX))
        .collect::<Result<Vec<_>, _>>()?;
    let my_choices = match &t[11] {
        Value::Null => None,
        other => {
            let picked = list(other, 0, answers.len())?
                .iter()
                .map(|c| uint(c, 0, answers.len() as u64 - 1).map(|n| n as usize))
                .collect::<Result<Vec<_>, _>>()?;
            let mut u = picked.clone();
            u.sort_unstable();
            u.dedup();
            if u.len() != picked.len() || (!multi && picked.len() > 1) {
                return Err(Invalid);
            }
            Some(picked)
        }
    };
    let revision = uint(&t[12], 1, u64::MAX)?;
    let total: u64 = counts.iter().sum();
    Ok(PollView {
        id: poll_id,
        channel,
        creator,
        question,
        answers,
        multi,
        created_at,
        due_at,
        closed,
        counts,
        my_choices,
        revision,
        previews: prior.map(|p| p.previews.clone()).unwrap_or_default(),
        distinct: prior.map_or(total, |p| p.distinct),
        capped: prior.is_some_and(|p| p.capped),
    })
}

/// A poll answer (`decodePollResponse`): `[1, summary]`, for `poll_id`.
pub fn decode_poll(
    data: &[u8],
    poll_id: [u8; 16],
    prior: Option<&PollView>,
) -> Result<PollView, Invalid> {
    let e = envelope(data, 2)?;
    let poll = read_poll(&e[1], prior)?;
    if poll.id != poll_id {
        return Err(Invalid);
    }
    Ok(poll)
}

/// `readReminderSummary`.
pub fn read_reminder(v: &Value) -> Result<ReminderView, Invalid> {
    let t = arr(v, 8)?;
    let source_channel = opt_id(&t[4])?;
    let source_thread = opt_id(&t[5])?;
    if source_channel.is_none() && source_thread.is_some() {
        return Err(Invalid);
    }
    Ok(ReminderView {
        id: id(&t[0])?,
        text: text(&t[1], 1500, None, false)?,
        due: uint(&t[2], 1, MAX_UNIX_SECONDS)?,
        timezone: text(&t[3], 100, None, false)?,
        source_channel: source_channel.map(ChannelId),
        source_thread: source_thread.map(MessageId),
        state: match uint(&t[6], 0, 3)? {
            0 => ReminderStatus::Scheduled,
            1 => ReminderStatus::Due,
            2 => ReminderStatus::Cancelled,
            _ => ReminderStatus::Dismissed,
        },
        revision: uint(&t[7], 1, u64::MAX)?,
    })
}

/// A reminder answer: `[1, summary]` for `reminder_id`, in `state`.
pub fn decode_reminder(
    data: &[u8],
    reminder_id: [u8; 16],
    state: ReminderStatus,
) -> Result<ReminderView, Invalid> {
    let e = envelope(data, 2)?;
    let r = read_reminder(&e[1])?;
    if r.id != reminder_id || r.state != state {
        return Err(Invalid);
    }
    Ok(r)
}

/// `decodeReminderListPage`: `[1, items (≤ 20, scheduled or due, by due then id), cursor | null]`.
pub fn decode_reminder_page(data: &[u8]) -> Result<(Vec<ReminderView>, Option<Vec<u8>>), Invalid> {
    let e = envelope(data, 3)?;
    let items = list(&e[1], 0, 20)?
        .iter()
        .map(read_reminder)
        .collect::<Result<Vec<_>, _>>()?;
    if items
        .iter()
        .any(|r| !matches!(r.state, ReminderStatus::Scheduled | ReminderStatus::Due))
        || items
            .windows(2)
            .any(|w| (w[1].due, w[1].id) <= (w[0].due, w[0].id))
    {
        return Err(Invalid);
    }
    let cursor = match &e[2] {
        Value::Null => None,
        Value::Bytes(b) if !b.is_empty() && b.len() <= 256 && !items.is_empty() => Some(b.clone()),
        _ => return Err(Invalid),
    };
    Ok((items, cursor))
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;

    pub(crate) fn poll_value(
        poll: [u8; 16],
        counts: [u64; 2],
        mine: Option<Vec<u64>>,
        closed: bool,
    ) -> Value {
        Value::Array(vec![
            Value::Bytes(poll.to_vec()),
            Value::Bytes(vec![2; 16]),
            Value::Null,
            Value::Bytes(vec![3; 16]),
            Value::Text("Raid?".into()),
            Value::Array(vec![
                Value::Text("the bridge".into()),
                Value::Text("the harbour".into()),
            ]),
            Value::Uint(0),
            Value::Uint(1000),
            Value::Uint(1000 + 86_400),
            Value::Uint(u64::from(closed)),
            Value::Array(counts.iter().map(|c| Value::Uint(*c)).collect()),
            mine.map_or(Value::Null, |m| {
                Value::Array(m.into_iter().map(Value::Uint).collect())
            }),
            Value::Uint(4),
        ])
    }

    #[test]
    fn a_poll_answer_is_read_whole_or_refused() {
        let data = Value::Array(vec![
            Value::Uint(1),
            poll_value([1; 16], [2, 1], Some(vec![0]), false),
        ])
        .encode();
        let p = decode_poll(&data, [1; 16], None).unwrap();
        assert_eq!(
            (p.counts.clone(), p.my_choices.clone(), p.revision),
            (vec![2, 1], Some(vec![0]), 4)
        );
        assert_eq!(
            decode_poll(&data, [9; 16], None),
            Err(Invalid),
            "another poll id"
        );
        let two = Value::Array(vec![
            Value::Uint(1),
            poll_value([1; 16], [2, 1], Some(vec![0, 1]), false),
        ])
        .encode();
        assert_eq!(
            decode_poll(&two, [1; 16], None),
            Err(Invalid),
            "two choices on a pick-one poll"
        );
    }

    #[test]
    fn a_reminder_page_is_ordered_and_open_only() {
        let r = |id: u8, due: u64, state: u64| {
            Value::Array(vec![
                Value::Bytes(vec![id; 16]),
                Value::Text("stretch".into()),
                Value::Uint(due),
                Value::Text("UTC".into()),
                Value::Null,
                Value::Null,
                Value::Uint(state),
                Value::Uint(1),
            ])
        };
        let page = |items: Vec<Value>| {
            Value::Array(vec![Value::Uint(1), Value::Array(items), Value::Null]).encode()
        };
        assert_eq!(
            decode_reminder_page(&page(vec![r(1, 10, 0), r(2, 20, 1)]))
                .unwrap()
                .0
                .len(),
            2
        );
        assert_eq!(
            decode_reminder_page(&page(vec![r(2, 20, 0), r(1, 10, 0)])),
            Err(Invalid)
        );
        assert_eq!(decode_reminder_page(&page(vec![r(1, 10, 2)])), Err(Invalid));
    }
}
