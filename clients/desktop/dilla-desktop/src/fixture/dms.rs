//! The dms facet of a fixture: `AppState::dms` and `AppState::trust`, from `WorldSpec::dms` and the messages in `dm/<id>`. Owned by Task 13; no other file maps it.
//!
//! The viewer's DMs, named as the client names them (the other members' display names), each with its text group active,
//! and each DM's messages as its timeline (`AppState::timelines` under the DM's id), oldest first. The ids are the world's
//! world's (derived, or the sidecar's). The world names no trust state (the spec has no key rotation), so the trust slice is the core's
//! answer for a world where nothing is blocked, except in the trust fixtures [`apply_trust`] names.

use dilla_client::state::dms::{DM_KIND_GROUP, DM_KIND_PAIR, DmSummary, dm_name};
use dilla_client::state::trust::{SendBlock, TrustBlock, TrustSummary};
use dilla_client::state::{
    AppState, ChannelGroupState, ChannelId, MessageId, Timeline, TimelineItem, TimelineItemState,
    UserId,
};
use dilla_client::testing::spec::{ChannelKind, WorldSpec, derive_id};

/// Maps the facet into `state`.
pub(super) fn apply(
    world: &WorldSpec,
    viewer: &str,
    shift: u64,
    ids: super::Ids<'_>,
    state: &mut AppState,
) {
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
        // The instance lists a DM's members in id order (internal/api/dms.go sorts them), and the DMs newest first, then by channel id (the parity world makes them all in one second, so: by channel id).
        let mut members: Vec<UserId> = dm
            .members
            .iter()
            .filter_map(|m| id("person", m).map(UserId))
            .collect();
        members.sort_by_key(|u| u.0);
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
                ts: m.ts + shift,
                body: m.body.clone(),
                msg_id: id("message", &m.key).map(MessageId),
                seq: None,
                edited: m.edited,
                reply: None,
                reactions: Vec::new(),
                pinned: m.pinned,
                attachments: Vec::new(),
                mention: false,
                actions: Vec::new(),
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
    list.sort_by_key(|d| d.id.0);
    state.dms.list = Some(list);
}

/// The trust fixtures' verdicts, by fixture name (the spec names no key rotation, so the fixture's name says which of the
/// viewer's contacts the core reports verified and which changed, as `gifs` and `invites` key on theirs): `(verified,
/// changed)`, as people's ids. Every other fixture holds no contacts.
fn trust_of(name: &str) -> Option<(&'static [&'static str], &'static [&'static str])> {
    match name {
        "trust/privacy" => Some((&["bjorn"], &[])),
        "trust/privacy-changed" => Some((&["bjorn"], &["mira"])),
        // The same verdicts; its golden opens Mira's comparison from her alert (`dms::native_privacy_compare`).
        "trust/privacy-compare" => Some((&["bjorn"], &["mira"])),
        "dms/identity-changed" => Some((&[], &["mira"])),
        _ => None,
    }
}

/// A person's fictional public key in a trust fixture (64 lower-case hex digits), derived from the id as the world's ids
/// are; `next` is the key that replaces it.
fn fixture_umk(person: &str, next: bool) -> String {
    let tag = if next { "next" } else { "pinned" };
    [
        derive_id("umk", &format!("{person}/{tag}/0")),
        derive_id("umk", &format!("{person}/{tag}/1")),
    ]
    .iter()
    .flatten()
    .map(|b| format!("{b:02x}"))
    .collect()
}

/// Maps a trust fixture's verdicts ([`trust_of`]) into `AppState::trust`: every person who shares a server or a DM with
/// the viewer is a contact, sorted by name; a changed one holds its pending key and blocks the DMs and text channels it
/// shares with the viewer, as the core's send gate does.
pub(super) fn apply_trust(
    name: &str,
    world: &WorldSpec,
    viewer: &str,
    ids: super::Ids<'_>,
    first_seen: u64,
    state: &mut AppState,
) {
    let Some((verified, changed)) = trust_of(name) else {
        return;
    };
    let both = |members: &[String], person: &str| {
        members.iter().any(|m| m == viewer) && members.iter().any(|m| m == person)
    };
    let shares = |person: &str| {
        world.servers.iter().any(|s| both(&s.members, person))
            || world.dms.iter().any(|d| both(&d.members, person))
    };
    let mut contacts: Vec<TrustSummary> = world
        .people
        .iter()
        .filter(|p| p.id != viewer && !p.bot && shares(&p.id))
        .map(|p| {
            let is_changed = changed.contains(&p.id.as_str());
            TrustSummary {
                user_id: ids.user(&p.id),
                name: p.display.clone(),
                shared: true,
                umk: fixture_umk(&p.id, false),
                first_seen,
                verified: !is_changed && verified.contains(&p.id.as_str()),
                changed_umk: is_changed.then(|| fixture_umk(&p.id, true)),
                unlisted: false,
                removed_in: Vec::new(),
                blocked: is_changed.then_some(TrustBlock::UmkChanged),
            }
        })
        .collect();
    contacts.sort_by(|a, b| a.name.cmp(&b.name));
    let block = SendBlock {
        code: TrustBlock::UmkChanged,
        own: false,
    };
    for person in changed {
        for dm in world.dms.iter().filter(|d| both(&d.members, person)) {
            state.trust.send_blocks.insert(ids.dm(&dm.id), block);
        }
        for server in world.servers.iter().filter(|s| both(&s.members, person)) {
            for c in server
                .channels
                .iter()
                .filter(|c| c.kind == ChannelKind::Text)
            {
                let path = format!("{}/{}", server.id, c.id);
                state.trust.send_blocks.insert(ids.channel(&path), block);
            }
        }
    }
    state.trust.loaded = true;
    for c in &contacts {
        let key = c.comparison_key();
        crate::screens::dms::compare::fixture_number(key, fixture_safety(viewer, key));
    }
    state.trust.contacts = contacts;
}

/// The comparison's twelve groups in a trust fixture (a fixture has no client to ask the core): twelve groups of five
/// digits derived from `viewer` and `key`, as the world's ids are (fictional, nothing a core computed).
fn fixture_safety(viewer: &str, key: &str) -> Vec<String> {
    let bytes: Vec<u8> = (0..3)
        .flat_map(|n| derive_id("safety", &format!("{viewer}/{key}/{n}")))
        .collect();
    bytes
        .chunks(4)
        .take(12)
        .map(|c| {
            let n = u32::from_be_bytes([c[0], c[1], c[2], c[3]]) % 100_000;
            format!("{n:05}")
        })
        .collect()
}
