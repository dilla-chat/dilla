//! Home (`/dm`, `/dm/<id>`): the sidebar's DM list in place of a server's channels (Shell.tsx `onDms`): the plain
//! "Direct messages" title, the "message someone" button, the online and conversation counts, the section label and one
//! person row per DM with its unread dot and mention count. A DM's conversation is Task 9's log and composer on the DM's
//! channel id, with this area's [`super::conversation::banners`] over it.

use dilla_client::features::dms::home_counts;
use dilla_client::state::dms::DM_KIND_GROUP;
use dilla_client::state::dms::DmSummary;
use dilla_client::state::members::{Member, Presence};
use dilla_client::state::unread::BadgeState;
use dilla_client::state::{AppState, ChannelId, UserId};
use gpui::{AnyElement, App, IntoElement, Window};

use std::rc::Rc;

use dilla_desktop_ui::IconName;
use dilla_desktop_ui::components::action_menu::{ActionMenu, MenuItem};
use dilla_desktop_ui::components::dms::{DmItem, DmList};
use gpui::Pixels;

use crate::screens::channel_list::css as head;
use dilla_desktop_ui::components::row::Person;

use crate::slots::SlotCx;
use crate::strings::{t, tf};

use super::{Nav, navigate, picker};

/// The Home tile's counts (`home.tileLabel`): every DM's unread and mentions.
pub fn tile_counts(state: &AppState) -> BadgeState {
    home_counts(state)
}

/// Shell.tsx `allMembers.find`: `user`'s member row from any server, the servers in the list's order.
pub fn member_anywhere(state: &AppState, user: UserId) -> Option<&Member> {
    state
        .communities
        .iter()
        .filter_map(|c| state.members.get(&c.id))
        .flatten()
        .find(|m| m.user_id == user)
}

/// Shell.tsx `presenceOf`: the first presence other than offline across the servers in the list's order, else offline.
pub fn presence_anywhere(state: &AppState, user: UserId) -> Presence {
    state
        .communities
        .iter()
        .map(|c| dilla_client::features::members::shown_presence(state, c.id, user))
        .find(|p| *p != Presence::Offline)
        .unwrap_or(Presence::Offline)
}

/// `presence.<status>`'s key.
fn presence_key(p: Presence) -> &'static str {
    match p {
        Presence::Online => "presence.online",
        Presence::Idle => "presence.idle",
        Presence::Dnd => "presence.dnd",
        Presence::Offline => "presence.offline",
    }
}

/// Shell.tsx `dmTopic` (Ruling Z): "@handle · status" for a two-person DM (the status message when the person has one,
/// else their presence), "group DM · Ada, Björn and you" for a group; `None` when the other person is not known.
pub fn dm_topic(state: &AppState, dm: &DmSummary) -> Option<String> {
    let own = state.account.user_id;
    let others: Vec<&Member> = dm
        .members
        .iter()
        .filter(|id| Some(**id) != own)
        .filter_map(|id| member_anywhere(state, *id))
        .collect();
    if dm.members.len() > 2 {
        let mut names: Vec<String> = others
            .iter()
            .map(|m| {
                if m.display.is_empty() {
                    m.username.clone()
                } else {
                    m.display.clone()
                }
            })
            .collect();
        names.push(t("shell.dm.you").to_owned());
        return Some(tf("shell.dm.topicGroup", &[("names", &list_and(&names))]));
    }
    let other = others.first()?;
    let status = if other.status_msg.is_empty() {
        t(presence_key(presence_anywhere(state, other.user_id))).to_owned()
    } else {
        other.status_msg.clone()
    };
    Some(tf(
        "shell.dm.topic",
        &[
            ("handle", other.username.as_str()),
            ("status", status.as_str()),
        ],
    ))
}

/// `Intl.ListFormat('en', { type: 'conjunction' })`: "a", "a and b", "a, b, and c".
fn list_and(items: &[String]) -> String {
    match items {
        [] => String::new(),
        [one] => one.clone(),
        [a, b] => format!("{a} and {b}"),
        [rest @ .., last] => format!("{}, and {last}", rest.join(", ")),
    }
}

/// Shell.tsx `dmOnline`: the two-person DMs whose other person is online.
pub fn online_count(state: &AppState) -> usize {
    let own = state.account.user_id;
    state
        .dms
        .all()
        .iter()
        .filter(|d| {
            d.members.len() == 2
                && d.members
                    .iter()
                    .any(|id| Some(*id) != own && presence_anywhere(state, *id) == Presence::Online)
        })
        .count()
}

/// The DMs as the list draws them, in the instance's order. Shell.tsx `dmPerson`: a person's row is their avatar with
/// the online dot over their status message (or "@handle"); a group's the neutral avatar over its members; a person
/// this account shares no server with has neither a dot nor a sub line.
pub fn items(state: &AppState, selected: Option<ChannelId>) -> Vec<DmItem> {
    let own = state.account.user_id;
    state
        .dms
        .all()
        .iter()
        .map(|dm| {
            let badge = state.unread.get(&dm.id).copied().unwrap_or_default();
            let group = dm.kind == DM_KIND_GROUP;
            let other = dm
                .members
                .iter()
                .find(|id| Some(**id) != own)
                .and_then(|id| member_anywhere(state, *id));
            let (sub, presence) = match other {
                Some(m) if !group => (
                    Some(if m.status_msg.is_empty() {
                        format!("@{}", m.username)
                    } else {
                        m.status_msg.clone()
                    }),
                    Some(ui_presence(presence_anywhere(state, m.user_id))),
                ),
                _ => (None, None),
            };
            DmItem {
                name: dm.name.clone().into(),
                person: Person {
                    sub: if group {
                        Some(dm.name.clone().into())
                    } else {
                        sub.map(Into::into)
                    },
                    presence,
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

fn ui_presence(p: Presence) -> dilla_desktop_ui::components::avatar::Presence {
    use dilla_desktop_ui::components::avatar::Presence as P;
    match p {
        Presence::Online => P::Online,
        Presence::Idle => P::Idle,
        Presence::Dnd => P::Dnd,
        Presence::Offline => P::Offline,
    }
}

/// Shell.tsx `DM_DEFAULTS`: the device-wide default (it governs direct messages), in the DM list head's menu.
pub const DM_DEFAULTS: [(&str, &str); 3] = [
    ("dms-mentions", "signalSettings.menu.dmsMentions"),
    ("everything", "signalSettings.menu.everything"),
    ("nothing", "signalSettings.menu.notifyNothing"),
];

/// The DM list head's menu (Shell.tsx `dmsMenu`): open or not, the stored default (`None`: direct messages and
/// mentions), and what opening it and a pick do.
pub struct DmsMenu {
    pub open: bool,
    pub current: Option<String>,
    pub on_open: OnMenuOpen,
    pub on_pick: OnMenuPick,
    /// A DM row's context menu asked for (Shell.tsx's DM `rowMenu`: its call and pins), at the pointer.
    pub on_row_menu: OnRowMenu,
}

/// The DM list head's menu opened or closed.
pub type OnMenuOpen = Rc<dyn Fn(bool, &mut Window, &mut App)>;
/// The DM list head's menu picked a default (its `DM_DEFAULTS` value).
pub type OnMenuPick = Rc<dyn Fn(&str, &mut Window, &mut App)>;
/// A DM row's context menu was asked for, at the pointer.
pub type OnRowMenu = Rc<dyn Fn(ChannelId, gpui::Point<Pixels>, &mut Window, &mut App)>;

/// The menu's items, the stored default checked.
pub fn dms_menu_items(current: Option<&str>) -> Vec<MenuItem> {
    let current = current.unwrap_or("dms-mentions");
    DM_DEFAULTS
        .iter()
        .map(|(value, key)| MenuItem::new(*value, t(key)).checked(current == *value))
        .collect()
}

/// The Home sidebar.
pub fn sidebar(
    ctx: &SlotCx,
    selected: Option<ChannelId>,
    menu: DmsMenu,
    _window: &mut Window,
    _cx: &mut App,
) -> AnyElement {
    let online = online_count(&ctx.state);
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
    .on_select({
        let ids = ids.clone();
        move |i, window, cx| {
            if let Some(id) = ids.get(i) {
                navigate(Nav::Dm(*id), window, cx);
            }
        }
    })
    .on_menu({
        let on_row_menu = menu.on_row_menu.clone();
        move |i, at, window, cx| {
            if let Some(id) = ids.get(i) {
                on_row_menu(*id, at, window, cx);
            }
        }
    })
    .on_new(move |_, window, cx| {
        picker::open(
            window.window_handle().window_id(),
            community,
            client.clone(),
            cx,
        )
    })
    .menu({
        let (on_open, on_pick) = (menu.on_open.clone(), menu.on_pick.clone());
        // ChannelList.css `.d-channel-list__head-btn`: the ⌄ in a 26px button around a 15px glyph, as the server's.
        ActionMenu::new(
            "dm-list-menu",
            t("signalSettings.menu.dms"),
            dms_menu_items(menu.current.as_deref()),
        )
        .icon(IconName::ChevronDown)
        .icon_size(Pixels::from(head::GLYPH))
        .trigger_size(Pixels::from(head::MENU_HIT))
        .open(menu.open)
        .on_open_change(move |open, window, cx| on_open(open, window, cx))
        .on_select(move |item, window, cx| on_pick(item, window, cx))
    })
    .into_any_element()
}

#[cfg(test)]
mod tests {
    use super::*;
    use dilla_client::state::dms::{DM_KIND_PAIR, DmState};
    use dilla_client::state::{ChannelGroupState, Community, CommunityId};

    const ME: UserId = UserId([1; 16]);
    const MIRA: UserId = UserId([2; 16]);
    const BJORN: UserId = UserId([3; 16]);
    const C: CommunityId = CommunityId([9; 16]);

    fn member(user: UserId, username: &str, display: &str, status: &str) -> Member {
        Member {
            user_id: user,
            username: username.into(),
            display: display.into(),
            kind: 0,
            role_ids: Vec::new(),
            status_msg: status.into(),
            bio: String::new(),
        }
    }

    fn dm(id: u8, kind: u8, members: Vec<UserId>, name: &str) -> DmSummary {
        DmSummary {
            id: ChannelId([id; 16]),
            kind,
            members,
            name: name.into(),
            group: ChannelGroupState::Active,
        }
    }

    fn world(mira_status: &str) -> AppState {
        let mut s = AppState::default();
        s.account.user_id = Some(ME);
        s.communities.push(Community {
            id: C,
            name: "Midgard".into(),
            owner: ME,
            policy_version: 0,
        });
        s.members.insert(
            C,
            vec![
                member(ME, "ada", "Ada", ""),
                member(MIRA, "mira", "Mira", mira_status),
                member(BJORN, "bjorn", "Björn", ""),
            ],
        );
        s.dms = DmState {
            list: Some(vec![
                dm(5, DM_KIND_PAIR, vec![ME, MIRA], "Mira"),
                dm(6, DM_KIND_GROUP, vec![ME, MIRA, BJORN], "Mira, Björn"),
            ]),
        };
        s
    }

    fn mira_online(s: &mut AppState) {
        s.people
            .presence
            .insert(C, std::iter::once((MIRA, Presence::Online)).collect());
    }

    /// Shell.tsx `dmTopic` (Ruling Z): the handle and the presence, the status message when there is one, and a group's
    /// members with "you".
    #[test]
    fn the_dm_topic_is_the_handle_and_the_status() {
        let s = world("");
        let pair = s.dms.get(ChannelId([5; 16])).unwrap();
        assert_eq!(dm_topic(&s, pair).as_deref(), Some("@mira · offline"));
        let mut online = world("");
        mira_online(&mut online);
        assert_eq!(dm_topic(&online, pair).as_deref(), Some("@mira · online"));
        let busy = world("heads down");
        assert_eq!(dm_topic(&busy, pair).as_deref(), Some("@mira · heads down"));
        let group = s.dms.get(ChannelId([6; 16])).unwrap();
        assert_eq!(
            dm_topic(&s, group).as_deref(),
            Some("group DM · Mira, Björn, and you")
        );
    }

    /// Shell.tsx `dmPerson` and `dmOnline`: a person's row says "@handle" without a status message, with their presence;
    /// the online count is the two-person DMs whose other person is online.
    #[test]
    fn a_dm_row_says_the_handle_and_the_online_count_is_the_peers() {
        let s = world("");
        let rows = items(&s, Some(ChannelId([5; 16])));
        assert_eq!(rows[0].person.sub.as_deref(), Some("@mira"));
        assert!(rows[0].active);
        assert_eq!(online_count(&s), 0);
        let mut online = world("");
        mira_online(&mut online);
        assert_eq!(online_count(&online), 1);
        assert_eq!(
            items(&online, None)[0].person.presence,
            Some(dilla_desktop_ui::components::avatar::Presence::Online)
        );
    }

    /// Shell.tsx `dmsMenu`: the device-wide default, checked as stored (nothing stored: direct messages and mentions).
    #[test]
    fn the_dm_defaults_menu_checks_the_stored_default() {
        let checked = |current: Option<&str>| {
            dms_menu_items(current)
                .iter()
                .position(|i| i.checked == Some(true))
        };
        assert_eq!(checked(None), Some(0));
        assert_eq!(checked(Some("nothing")), Some(2));
    }
}
