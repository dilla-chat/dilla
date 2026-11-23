//! The notify facet of a fixture: `AppState::unread` and `AppState::notify`. Owned by Task 22; no other file maps it.
//!
//! What the web reference's browser holds when it is shot (`parity/shoot.mjs`): it opens the fixture's route channels
//! and every other channel with history only joins; the history is then posted by its senders while only the browser is
//! online. So every message another person sent to a channel the route does not show is unread, and the browser's
//! `Notification.permission` is `denied` (headless Chromium, as the settings reference shows: "Blocked in the browser").

use dilla_client::state::unread::Permission;
use dilla_client::state::{AppState, BadgeState, ChannelId};
use dilla_client::testing::spec::{ChannelKind, ScreenFixture};

use super::Ids;

/// Maps the facet into `state`.
pub(super) fn apply(fixture: &ScreenFixture, ids: Ids<'_>, state: &mut AppState) {
    state.notify.permission = Permission::Denied;
    let (world, viewer) = (&fixture.world, fixture.viewer.as_str());
    for server in world
        .servers
        .iter()
        .filter(|s| s.members.iter().any(|m| m == viewer))
    {
        for channel in server
            .channels
            .iter()
            .filter(|c| c.kind == ChannelKind::Text)
        {
            let path = format!("{}/{}", server.id, channel.id);
            if fixture.route.contains(&format!("{{{path}}}")) {
                continue;
            }
            let unread = world
                .messages
                .iter()
                .filter(|m| m.channel == path && m.sender != viewer)
                .count();
            if unread > 0 {
                let id: ChannelId = ids.channel(&path);
                state.unread.insert(
                    id,
                    BadgeState {
                        unread: u32::try_from(unread).unwrap_or(u32::MAX),
                        mentions: 0,
                    },
                );
            }
        }
    }
    // The same rule for the viewer's DMs: a message another person sent to a DM the route does not show is unread (Task 42).
    for dm in world
        .dms
        .iter()
        .filter(|d| d.members.iter().any(|m| m == viewer))
    {
        let path = format!("dm/{}", dm.id);
        if fixture.route.contains(&format!("{{{path}}}")) {
            continue;
        }
        let unread = world
            .messages
            .iter()
            .filter(|m| m.channel == path && m.sender != viewer)
            .count();
        if unread > 0 {
            state.unread.insert(
                ids.dm(&dm.id),
                BadgeState {
                    unread: u32::try_from(unread).unwrap_or(u32::MAX),
                    mentions: 0,
                },
            );
        }
    }
}

#[cfg(test)]
mod tests {
    use std::path::Path;

    use crate::fixture::Fixture;

    fn load(name: &str) -> Fixture {
        let path = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../parity/fixtures")
            .join(name);
        Fixture::load(&path, None).unwrap()
    }

    fn counts(f: &Fixture) -> Vec<(String, u32)> {
        let mut out: Vec<_> = f
            .state
            .unread
            .iter()
            .map(|(id, b)| (f.state.channel(*id).unwrap().name.clone(), b.unread))
            .collect();
        out.sort();
        out
    }

    #[test]
    fn only_channels_the_route_does_not_show_are_unread() {
        let f = load("notify/unread-badge.json");
        assert_eq!(counts(&f), [("loot".to_owned(), 1)]);
        let b = load("notify/unread-badge-b.json");
        let want = [
            ("design", 1),
            ("general", 1),
            ("weekly-triage-notes-for-the-whole-collective", 1),
        ];
        assert_eq!(counts(&b), want.map(|(n, c)| (n.to_owned(), c)).to_vec());
    }

    #[test]
    fn a_dm_the_route_does_not_show_is_unread_by_what_others_sent_to_it() {
        // dms/home: Mira wrote in dm/mira, Ada alone in dm/bjorn, and the route is the DM list.
        let f = load("dms/home.json");
        let id_of = |name: &str| {
            f.state
                .dms
                .all()
                .iter()
                .find(|d| d.name == name)
                .unwrap()
                .id
        };
        assert_eq!(
            f.state.unread.get(&id_of("Mira")).map(|b| b.unread),
            Some(1)
        );
        assert_eq!(f.state.unread.get(&id_of("Björn")), None);
        // dms/conversation shows dm/mira: nothing is unread there.
        assert!(load("dms/conversation.json").state.unread.is_empty());
    }

    #[test]
    fn the_browsers_permission_is_denied() {
        let f = load("notify/dm-badge.json");
        assert_eq!(
            f.state.notify.permission,
            dilla_client::state::unread::Permission::Denied
        );
        // the viewer's own messages are never unread
        assert_eq!(counts(&f), [("loot".to_owned(), 1)]);
    }
}
