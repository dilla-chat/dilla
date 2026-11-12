//! `communities` and `channels:<id>` (types.ts `CommunitySummary`, `ChannelSummary`, `ChannelGroupState`), read from
//! `GET /v1/communities` and `GET /v1/communities/{id}/channels` as routes.ts `listCommunities` / `listChannels` read
//! them, and derived against the core's group rows as derive.ts `channelGroupState` does.

use crate::gateway::frames::Value;
use crate::sync::GroupRow;
use crate::sync::value::{Rows, at_least, bin16, opt, text, u53, uint};

use super::{ChannelId, CommunityId, GroupId, UserId};

/// `CommunitySummary`, with the policy version the community's groups are bound to.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Community {
    pub id: CommunityId,
    pub name: String,
    pub owner: UserId,
    pub policy_version: u64,
}

/// `ChannelGroupState`.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Default)]
pub enum ChannelGroupState {
    /// No local text group yet.
    #[default]
    None,
    Joining,
    Active,
    Resync,
    NotMember,
    /// Not a text channel in mode 0 (voice, or a mode the client cannot read).
    Unsupported,
}

/// `ChannelSummary`, with the text group the instance names for it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Channel {
    pub id: ChannelId,
    pub community_id: CommunityId,
    /// 0 text, 1 voice, 2 category.
    pub kind: u8,
    pub mode: u8,
    pub name: String,
    pub topic: String,
    pub parent_id: Option<ChannelId>,
    pub position: u32,
    pub slowmode: u32,
    /// The channel's MLS text group as the instance knows it (`text_group_id`), `None` before anyone registered one.
    pub text_group_id: Option<GroupId>,
    pub group: ChannelGroupState,
}

impl Channel {
    /// A text channel in mode 0: the only kind with a timeline.
    pub fn is_text(&self) -> bool {
        self.kind == 0 && self.mode == 0
    }
}

/// routes.ts `listCommunities`: `[[id, name, owner, policy_version, ...]]`.
pub(crate) fn decode_communities(
    raw: &[u8],
) -> Result<Vec<Community>, crate::sync::value::DecodeError> {
    Rows::of(raw)?
        .iter()
        .map(|row| {
            let a = at_least(row, 4)?;
            Ok(Community {
                id: CommunityId(bin16(&a[0])?),
                name: text(&a[1])?,
                owner: UserId(bin16(&a[2])?),
                policy_version: uint(&a[3])?,
            })
        })
        .collect()
}

/// routes.ts `listChannels`: `[[id, kind, mode, visibility, parent_id|null, name, topic, position, slowmode, seq,
/// text_group_id|null, ...]]`.
pub(crate) fn decode_channels(
    community: CommunityId,
    raw: &[u8],
) -> Result<Vec<Channel>, crate::sync::value::DecodeError> {
    Rows::of(raw)?
        .iter()
        .map(|row| {
            let a = at_least(row, 11)?;
            Ok(Channel {
                id: ChannelId(bin16(&a[0])?),
                community_id: community,
                kind: small(&a[1])?,
                mode: small(&a[2])?,
                parent_id: opt(&a[4], bin16)?.map(ChannelId),
                name: text(&a[5])?,
                topic: text(&a[6])?,
                position: u32::try_from(u53(&a[7])?).unwrap_or(u32::MAX),
                slowmode: u32::try_from(u53(&a[8])?).unwrap_or(u32::MAX),
                text_group_id: opt(&a[10], bin16)?.map(GroupId),
                group: ChannelGroupState::None,
            })
        })
        .collect()
}

fn small(v: &Value) -> Result<u8, crate::sync::value::DecodeError> {
    u8::try_from(u53(v)?)
        .map_err(|_| crate::sync::value::DecodeError("a small integer is out of range"))
}

/// derive.ts `channelGroup`: the channel's first live local text group (state other than 4), else the first gone one.
pub(crate) fn channel_group(channel: ChannelId, groups: &[GroupRow]) -> Option<&GroupRow> {
    let mine = || {
        groups
            .iter()
            .filter(|g| g.kind == 0 && g.target_id == channel.0)
    };
    mine()
        .find(|g| g.state != 4)
        .or_else(|| mine().find(|g| g.state == 4))
}

/// derive.ts `channelGroupState`. `not_member` and `resyncing` are the groups the engine last reported so.
pub(crate) fn channel_group_state(
    channel: &Channel,
    groups: &[GroupRow],
    not_member: &dyn Fn(&GroupId) -> bool,
    resyncing: &dyn Fn(&GroupId) -> bool,
) -> ChannelGroupState {
    if !channel.is_text() {
        return ChannelGroupState::Unsupported;
    }
    let Some(group) = channel_group(channel.id, groups) else {
        return ChannelGroupState::None;
    };
    let g = GroupId(group.group_id);
    match group.state {
        0 | 1 => ChannelGroupState::Joining,
        2 if resyncing(&g) => ChannelGroupState::Resync,
        2 if not_member(&g) => ChannelGroupState::NotMember,
        2 => ChannelGroupState::Active,
        3 if not_member(&g) => ChannelGroupState::NotMember,
        3 => ChannelGroupState::Resync,
        _ => ChannelGroupState::NotMember,
    }
}
