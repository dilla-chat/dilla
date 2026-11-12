//! `badges` (types.ts `BadgeState`, badges.ts `buildBadges`): the core's activity rows (unread and mention counts per
//! group, kept by the core at apply and by `mark_read`) summed per channel.

use std::collections::HashMap;

use crate::sync::value::{DecodeError, Rows, arr, bin16, u53, uint};

use super::{ChannelId, GroupId};

/// `BadgeState`.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct BadgeState {
    pub unread: u32,
    pub mentions: u32,
}

/// core-port.ts `ActivityRow`: `[group_id, unread, mentions, last_seq, last_ts, last_read_seq]`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) struct ActivityRow {
    pub group_id: [u8; 16],
    pub unread: u32,
    pub mentions: u32,
    pub last_seq: u64,
}

pub(crate) fn decode_activity(raw: &[u8]) -> Result<Vec<ActivityRow>, DecodeError> {
    Rows::of(raw)?
        .iter()
        .map(|row| {
            let a = arr(row, 6)?;
            Ok(ActivityRow {
                group_id: bin16(&a[0])?,
                unread: u32::try_from(u53(&a[1])?).unwrap_or(u32::MAX),
                mentions: u32::try_from(u53(&a[2])?).unwrap_or(u32::MAX),
                last_seq: uint(&a[3])?,
            })
        })
        .collect()
}

/// badges.ts `buildBadges`: every group bound to a known channel contributes its counts; a group bound to none adds
/// nothing. A channel whose counts are both zero is left out.
pub(crate) fn build_badges(
    activity: &[ActivityRow],
    channel_of: &HashMap<GroupId, ChannelId>,
) -> HashMap<ChannelId, BadgeState> {
    let mut badges: HashMap<ChannelId, BadgeState> = HashMap::new();
    for row in activity {
        let Some(channel) = channel_of.get(&GroupId(row.group_id)) else {
            continue;
        };
        let held = badges.entry(*channel).or_default();
        held.unread = held.unread.saturating_add(row.unread);
        held.mentions = held.mentions.saturating_add(row.mentions);
    }
    badges.retain(|_, b| b.unread > 0 || b.mentions > 0);
    badges
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn badges_sum_per_channel_and_skip_unbound_groups() {
        let row = |g: u8, unread, mentions| ActivityRow {
            group_id: [g; 16],
            unread,
            mentions,
            last_seq: 9,
        };
        let channel = ChannelId([7; 16]);
        let map = HashMap::from([(GroupId([1; 16]), channel), (GroupId([2; 16]), channel)]);
        let badges = build_badges(&[row(1, 2, 1), row(2, 1, 0), row(3, 5, 5)], &map);
        assert_eq!(
            badges,
            HashMap::from([(
                channel,
                BadgeState {
                    unread: 3,
                    mentions: 1
                }
            )])
        );
        assert!(build_badges(&[row(1, 0, 0)], &map).is_empty());
    }
}
