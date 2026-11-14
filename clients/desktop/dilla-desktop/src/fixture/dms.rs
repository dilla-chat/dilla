//! The dms facet of a fixture: `AppState::dms` and `AppState::trust`, from `WorldSpec::dms` and the messages in `dm/<id>`. Owned by Task 13; no other file maps it.
//!
//! The viewer's DMs, named as the client names them (the other members' display names), each with its text group active,
//! and each DM's messages as its timeline (`AppState::timelines` under the DM's id), oldest first. The ids are the world's
//! world's (derived, or the sidecar's). The world names no trust state yet (the spec has no key rotation), so the trust slice is the core's
//! answer for a world where every contact is first seen: loaded, nothing blocked.

use dilla_client::state::dms::{DM_KIND_GROUP, DM_KIND_PAIR, DmSummary, dm_name};
use dilla_client::state::{
    AppState, ChannelGroupState, ChannelId, MessageId, Timeline, TimelineItem, TimelineItemState,
    UserId,
};
use dilla_client::testing::spec::WorldSpec;

/// Maps the facet into `state`.
pub(super) fn apply(world: &WorldSpec, viewer: &str, ids: super::Ids<'_>, state: &mut AppState) {
    state.trust.loaded = true;
    if world.dms.is_empty() {
        return;
    }
    // The world's ids: derived, or the ones a real dillad handed out (the reference's sidecar names the DM's own).
    let id = |kind: &str, local: &str| ids.map.get(&format!("{kind}:{local}")).copied();
    let own = id("person", viewer).map(UserId);
    let name_of = |user: UserId| {
        world
            .people
            .iter()
            .find(|p| id("person", &p.id).map(UserId) == Some(user))
            .map_or_else(|| user.hex()[..8].to_owned(), |p| p.display.clone())
    };
    let own_name = world
        .people
        .iter()
        .find(|p| p.id == viewer)
        .map_or(viewer, |p| p.username())
        .to_owned();
    let mut list = Vec::new();
    for dm in world
        .dms
        .iter()
        .filter(|d| d.members.iter().any(|m| m == viewer))
    {
        let Some(channel) = id("dm", &dm.id).map(ChannelId) else {
            continue;
        };
        let members: Vec<UserId> = dm
            .members
            .iter()
            .filter_map(|m| id("person", m).map(UserId))
            .collect();
        list.push(DmSummary {
            id: channel,
            kind: if dm.members.len() > 2 {
                DM_KIND_GROUP
            } else {
                DM_KIND_PAIR
            },
            name: dm_name(&members, own, &own_name, name_of),
            members,
            group: ChannelGroupState::Active,
        });
        let path = format!("dm/{}", dm.id);
        let items = world
            .messages
            .iter()
            .filter(|m| m.channel == path)
            .map(|m| TimelineItem {
                key: m.key.clone(),
                state: TimelineItemState::Ok,
                reason: String::new(),
                sender_user: id("person", &m.sender).map(UserId),
                sender_device: None,
                own: m.sender == viewer,
                bot: world.people.iter().any(|p| p.id == m.sender && p.bot),
                ts: m.ts,
                body: m.body.clone(),
                msg_id: id("message", &m.key).map(MessageId),
                seq: None,
                edited: m.edited,
                reply: None,
                reactions: Vec::new(),
                pinned: m.pinned,
                attachments: Vec::new(),
                mention: false,
            })
            .collect();
        state.timelines.insert(
            channel,
            Timeline {
                channel_id: channel,
                group: ChannelGroupState::Active,
                items,
                has_earlier: false,
            },
        );
    }
    state.dms.list = Some(list);
}
