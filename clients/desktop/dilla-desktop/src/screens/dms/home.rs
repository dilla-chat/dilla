//! Home (`/dm`, `/dm/<id>`): the sidebar's DM list in place of a server's channels (Shell.tsx `onDms`): the plain
//! "Direct messages" title, the "message someone" button, the online and conversation counts, the section label and one
//! person row per DM with its unread dot and mention count. A DM's conversation is Task 9's log and composer on the DM's
//! channel id, with this area's [`super::conversation::banners`] over it.

use dilla_client::features::dms::home_counts;
use dilla_client::state::dms::DM_KIND_GROUP;
use dilla_client::state::unread::BadgeState;
use dilla_client::state::{AppState, ChannelId};
use gpui::{AnyElement, App, IntoElement, Window};

use dilla_desktop_ui::components::dms::{DmItem, DmList};
use dilla_desktop_ui::components::row::Person;

use crate::slots::SlotCx;
use crate::strings::{t, tf};

use super::{Nav, navigate, picker};

/// The Home tile's counts (`home.tileLabel`): every DM's unread and mentions.
pub fn tile_counts(state: &AppState) -> BadgeState {
    home_counts(state)
}

/// The DMs as the list draws them, in the instance's order.
pub fn items(state: &AppState, selected: Option<ChannelId>) -> Vec<DmItem> {
    let own = state.account.user_id;
    state
        .dms
        .all()
        .iter()
        .map(|dm| {
            let badge = state.unread.get(&dm.id).copied().unwrap_or_default();
            let group = dm.kind == DM_KIND_GROUP;
            let status = dm.peer(own).and_then(|peer| {
                state
                    .members
                    .values()
                    .flatten()
                    .find(|m| m.user_id == peer)
                    .map(|m| m.status_msg.clone())
                    .filter(|s| !s.is_empty())
            });
            DmItem {
                name: dm.name.clone().into(),
                person: Person {
                    sub: if group {
                        Some(dm.name.clone().into())
                    } else {
                        status.map(Into::into)
                    },
                    presence: None,
                    group,
                    initials: None,
                },
                active: selected == Some(dm.id),
                unread: badge.unread,
                mentions: badge.mentions,
            }
        })
        .collect()
}

/// The Home sidebar. `online` is how many DM contacts are online (presence is the members area's).
pub fn sidebar(
    ctx: &SlotCx,
    selected: Option<ChannelId>,
    online: usize,
    _window: &mut Window,
    _cx: &mut App,
) -> AnyElement {
    let state = ctx.state.clone();
    if let Some(client) = &ctx.client {
        client.dms().start();
        // The DM view is showing: the list is read at its shortest interval.
        client.dms().viewing();
    }
    let n = state.dms.all().len();
    let count = if n == 1 {
        t("shell.dms.countOne").to_owned()
    } else {
        tf("shell.dms.count", &[("n", &n.to_string())])
    };
    let ids: Vec<ChannelId> = state.dms.all().iter().map(|d| d.id).collect();
    let (client, community) = (ctx.client.clone(), ctx.community);
    DmList::new(
        t("shell.dms.title"),
        vec![
            tf("shell.dms.online", &[("n", &online.to_string())]).into(),
            count.into(),
        ],
        t("shell.dms.section"),
        t("shell.dms.empty"),
        t("shell.dms.new"),
        items(&state, selected),
    )
    .on_select(move |i, window, cx| {
        if let Some(id) = ids.get(i) {
            navigate(Nav::Dm(*id), window, cx);
        }
    })
    .on_new(move |_, _, cx| picker::open(community, client.clone(), cx))
    .into_any_element()
}
