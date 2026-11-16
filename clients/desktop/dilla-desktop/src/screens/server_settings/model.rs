//! What the server settings decide without drawing: which sections a person sees, in which order and under which group
//! (settings/registry.ts, SettingsNavigation.tsx), how the Channels list is grouped (ChannelsSection.tsx `groupChannels`)
//! and the texts the Invites section builds from its rows (InvitesSection.tsx). Plain functions with their tests.

use dilla_client::features::server_admin::{AFK_BUILTIN_POSITION, is_builtin_afk};
use dilla_client::state::server_admin::{Access, Invite};
use dilla_client::state::{Channel, ChannelId};
use dilla_desktop_ui::IconName;

use crate::router::ServerSection;
use crate::strings::{t, tf};

/// ChannelEditFlow.tsx `canMark` (Bot v2 Task 22): a readable text channel (mode 0) that is not the AFK channel can be a
/// Bot channel; only who manages the server marks one.
pub fn can_mark_bot_channel(channel: &Channel, afk: bool, access: &Access) -> bool {
    channel.kind == 0 && channel.mode == 0 && !afk && access.loaded && access.manage_community
}

/// ChannelEditFlow.tsx `botChannel`: the catalog lists the channel among its community's Bot channels.
pub fn is_bot_channel(state: &dilla_client::state::AppState, channel: &Channel) -> bool {
    state
        .bot
        .catalogs
        .get(&channel.community_id)
        .and_then(|c| c.bot_channels.as_ref())
        .is_some_and(|ids| ids.contains(&channel.id))
}

/// registry.ts `server(..)` `visible`: a section is offered when the permissions are read and allow it.
pub fn visible(section: ServerSection, access: &Access) -> bool {
    if !access.loaded {
        return false;
    }
    use ServerSection::*;
    match section {
        Overview | Gifs | Bot | Welcome | Music => access.manage_community,
        Channels => access.manage_channels,
        Roles => access.manage_roles,
        Invites => access.create_invite,
        JoinRoles => access.manage_community && access.manage_roles,
        ModerationLog => access.view_audit_log,
        Bans => access.ban_members,
        Members | SelfRoles | Danger => true,
    }
}

/// registry.ts `wide`: the sections that use the 1024px column.
pub fn wide(section: ServerSection) -> bool {
    matches!(
        section,
        ServerSection::Channels
            | ServerSection::Members
            | ServerSection::ModerationLog
            | ServerSection::Bans
    )
}

/// The group a section's nav row sits under (`workspace.group.*`).
pub fn group_key(section: ServerSection) -> &'static str {
    use ServerSection::*;
    match section {
        Overview | Channels | Gifs => "workspace.group.server",
        Members | Roles | Invites | SelfRoles => "workspace.group.people",
        Bot | Welcome | JoinRoles | Music => "workspace.group.automation",
        ModerationLog | Bans => "workspace.group.moderation",
        Danger => "workspace.group.actions",
    }
}

/// settings/registry.ts `SETTINGS_ICONS`.
pub fn icon_of(section: ServerSection) -> IconName {
    use ServerSection::*;
    match section {
        Overview => IconName::Server,
        Channels => IconName::Hash,
        Gifs => IconName::Smile,
        Members => IconName::Users,
        Roles => IconName::Shield,
        Invites => IconName::Link,
        SelfRoles => IconName::User,
        Bot => IconName::Bot,
        Welcome => IconName::Megaphone,
        JoinRoles => IconName::UserPlus,
        Music => IconName::Music,
        ModerationLog => IconName::ScrollText,
        Bans => IconName::Ban,
        Danger => IconName::TriangleAlert,
    }
}

/// The label of a section (`workspace.<id>`).
pub fn label_of(section: ServerSection) -> &'static str {
    t(&format!("workspace.{}", section.id()))
}

/// SettingsNavigation.tsx `navigationSections('server')`: the prototype's order first (Overview, Roles, Invites, Bot, Music),
/// then every other available section in the registry's order.
pub fn nav_sections(access: &Access) -> Vec<ServerSection> {
    use ServerSection::*;
    let first = [Overview, Roles, Invites, Bot, Music];
    let allowed: Vec<ServerSection> = ServerSection::ALL
        .iter()
        .copied()
        .filter(|s| visible(*s, access))
        .collect();
    first
        .iter()
        .copied()
        .filter(|s| allowed.contains(s))
        .chain(allowed.iter().copied().filter(|s| !first.contains(s)))
        .collect()
}

/// ServerSettings.tsx `active`: the requested section if it is allowed, else the first allowed one.
pub fn active_of(requested: Option<ServerSection>, access: &Access) -> Option<ServerSection> {
    let allowed = ServerSection::ALL
        .iter()
        .copied()
        .filter(|s| visible(*s, access))
        .collect::<Vec<_>>();
    requested
        .filter(|r| allowed.contains(r))
        .or_else(|| allowed.first().copied())
}

/// ChannelsSection.tsx `Group`.
pub struct Group<'a> {
    pub category: Option<&'a Channel>,
    pub channels: Vec<&'a Channel>,
}

fn by_order(a: &&Channel, b: &&Channel) -> std::cmp::Ordering {
    a.position.cmp(&b.position).then(a.id.cmp(&b.id))
}

/// ChannelsSection.tsx `groupChannels`: channels with no category first, then each category with its channels, all in
/// position order.
pub fn grouped<'a>(all: &[&'a Channel]) -> Vec<Group<'a>> {
    let mut categories: Vec<&Channel> = all.iter().copied().filter(|c| c.kind == 2).collect();
    categories.sort_by(by_order);
    let rest: Vec<&Channel> = all.iter().copied().filter(|c| c.kind != 2).collect();
    let mut top: Vec<&Channel> = rest
        .iter()
        .copied()
        .filter(|c| {
            c.parent_id
                .is_none_or(|p| !categories.iter().any(|cat| cat.id == p))
        })
        .collect();
    top.sort_by(by_order);
    let mut out = vec![Group {
        category: None,
        channels: top,
    }];
    for category in categories {
        let mut channels: Vec<&Channel> = rest
            .iter()
            .copied()
            .filter(|c| c.parent_id == Some(category.id))
            .collect();
        channels.sort_by(by_order);
        out.push(Group {
            category: Some(category),
            channels,
        });
    }
    out
}

/// The label a row shows: a text channel with its hash.
pub fn shown(c: &Channel) -> String {
    if c.kind == 0 {
        format!("#{}", c.name)
    } else {
        c.name.clone()
    }
}

/// Whether a channel row is the built-in AFK channel (it keeps its place and has no move or delete).
pub fn fixed(c: &Channel, afk: Option<ChannelId>) -> bool {
    c.kind != 2 && is_builtin_afk(c.id, c.position, afk)
}

/// Where a row stands among the movable siblings of its group: the AFK channel is not counted (R-AFK8).
pub fn move_slots(group: &Group<'_>, afk: Option<ChannelId>, c: &Channel) -> (bool, bool) {
    let movable: Vec<&&Channel> = group.channels.iter().filter(|x| !fixed(x, afk)).collect();
    match movable.iter().position(|x| x.id == c.id) {
        Some(i) => (i > 0, i + 1 < movable.len()),
        None => (false, false),
    }
}

/// The category rows move among the categories.
pub fn category_slots(categories: &[&Channel], c: &Channel) -> (bool, bool) {
    match categories.iter().position(|x| x.id == c.id) {
        Some(i) => (i > 0, i + 1 < categories.len()),
        None => (false, false),
    }
}

/// InvitesSection.tsx `inTime`: "in 5 days", "in 3 hours", "in 12 minutes", from `now`. The unit is the largest one
/// left, as the web picks it, but the count is rounded to the nearest, not floored: a 7-day link reads "in 7 days" until
/// 6½ days are left, instead of turning to "in 6 days" a second after it was made. A count that rounds up to the next unit
/// is said in that unit (23 h 40 min is "in 1 day"), so the text never goes back up as time passes.
pub fn in_time(expires: u64, now: u64) -> String {
    let left = expires.saturating_sub(now);
    let rounded = |unit: u64| (left + unit / 2) / unit;
    let (days, hours) = if left >= 86_400 {
        (rounded(86_400), 0)
    } else if left >= 3600 {
        match rounded(3600) {
            24 => (1, 0),
            h => (0, h),
        }
    } else {
        match rounded(60).max(1) {
            60 => (0, 1),
            m => {
                return tf(
                    "serverSettings.invites.in.minutes",
                    &[("n", &m.to_string())],
                );
            }
        }
    };
    match (days, hours) {
        (1, _) => t("serverSettings.invites.in.day").to_owned(),
        (0, 1) => t("serverSettings.invites.in.hour").to_owned(),
        (0, h) => tf("serverSettings.invites.in.hours", &[("n", &h.to_string())]),
        (d, _) => tf("serverSettings.invites.in.days", &[("n", &d.to_string())]),
    }
}

/// InvitesSection.tsx `TTL_OPTIONS`: seconds and the string key of the label. 0 is "never".
pub const TTL_OPTIONS: [(u64, &str); 4] = [
    (3600, "serverSettings.invites.ttl.hour"),
    (86_400, "serverSettings.invites.ttl.day"),
    (604_800, "serverSettings.invites.ttl.week"),
    (0, "serverSettings.invites.ttl.never"),
];
/// `USES_OPTIONS`: 0 is "∞".
pub const USES_OPTIONS: [u64; 3] = [1, 10, 0];
pub const DEFAULT_TTL_S: u64 = 604_800;
pub const DEFAULT_USES: u64 = 10;

/// The label of a use limit.
pub fn uses_label(n: u64) -> String {
    if n == 0 {
        t("serverSettings.invites.uses.unlimited").to_owned()
    } else {
        n.to_string()
    }
}

/// The options someone offers: 0 ("never", "∞") needs Manage server (the instance asks for it).
pub fn ttl_options(manager: bool) -> Vec<(u64, &'static str)> {
    TTL_OPTIONS
        .iter()
        .copied()
        .filter(|(s, _)| *s != 0 || manager)
        .collect()
}

pub fn uses_options(manager: bool) -> Vec<u64> {
    USES_OPTIONS
        .iter()
        .copied()
        .filter(|n| *n != 0 || manager)
        .collect()
}

/// A choice someone without Manage server cannot make falls back to the default.
pub fn clamp_choice(ttl: u64, uses: u64, manager: bool) -> (u64, u64) {
    if manager {
        return (ttl, uses);
    }
    (
        if ttl == 0 { DEFAULT_TTL_S } else { ttl },
        if uses == 0 { DEFAULT_USES } else { uses },
    )
}

/// The note under a made link (InvitesSection.tsx `make`).
pub fn made_note(max_uses: u64, ttl_seconds: u64) -> String {
    let ttl_name = TTL_OPTIONS
        .iter()
        .find(|(s, _)| *s == ttl_seconds)
        .map_or("serverSettings.invites.ttl.week", |(_, k)| k);
    let uses = match max_uses {
        0 => t("serverSettings.invites.note.any").to_owned(),
        1 => t("serverSettings.invites.note.once").to_owned(),
        n => tf(
            "serverSettings.invites.note.times",
            &[("n", &n.to_string())],
        ),
    };
    let expires = if ttl_seconds == 0 {
        t("serverSettings.invites.note.never").to_owned()
    } else {
        tf("serverSettings.invites.note.in", &[("ttl", t(ttl_name))])
    };
    tf(
        "serverSettings.invites.note",
        &[("uses", &uses), ("expires", &expires)],
    )
}

/// One live link's texts: the masked code, the uses and the expiry.
pub struct InviteTexts {
    pub code: String,
    pub uses: String,
    pub expires: String,
}

pub fn invite_texts(invite: &Invite, host: &str, now: u64) -> InviteTexts {
    let date = if invite.expires == 0 {
        t("serverSettings.invites.ttl.never").to_owned()
    } else {
        in_time(invite.expires, now)
    };
    InviteTexts {
        code: tf("serverSettings.invites.code", &[("host", host)]),
        uses: tf(
            "serverSettings.invites.uses.row",
            &[
                ("used", &invite.uses.to_string()),
                (
                    "max",
                    &if invite.max_uses == 0 {
                        t("serverSettings.invites.uses.unlimited").to_owned()
                    } else {
                        invite.max_uses.to_string()
                    },
                ),
            ],
        ),
        expires: if invite.expires == 0 {
            t("serverSettings.invites.expires.never").to_owned()
        } else {
            tf("serverSettings.invites.expires.row", &[("date", &date)])
        },
    }
}

/// Where the AFK channel sits (the reservation); re-exported for the fixtures' sake.
pub const AFK_POSITION: u32 = AFK_BUILTIN_POSITION;

#[cfg(test)]
mod tests {
    use dilla_client::state::{ChannelGroupState, CommunityId};

    use super::*;

    fn access(f: impl FnOnce(&mut Access)) -> Access {
        let mut a = Access {
            loaded: true,
            ..Access::default()
        };
        f(&mut a);
        a
    }

    /// Integration 3, step 5 (ChannelEditFlow.tsx, Bot v2 Task 22): the "Bot channel" switch is offered on a readable
    /// text channel that is not the AFK channel, to who manages the server; it is on while the catalog lists the channel
    /// among the Bot channels.
    #[test]
    fn the_bot_channel_switch_follows_channel_edit_flow() {
        let manager = access(|a| a.manage_community = true);
        let text = ch(1, 0, None, 0);
        assert!(can_mark_bot_channel(&text, false, &manager));
        assert!(
            !can_mark_bot_channel(&text, true, &manager),
            "the AFK channel"
        );
        assert!(
            !can_mark_bot_channel(&ch(2, 1, None, 0), false, &manager),
            "a voice channel"
        );
        let mut announce = text.clone();
        announce.mode = 1;
        assert!(
            !can_mark_bot_channel(&announce, false, &manager),
            "not a readable one"
        );
        assert!(
            !can_mark_bot_channel(&text, false, &access(|_| {})),
            "not a manager"
        );
        let unloaded = Access {
            manage_community: true,
            ..Access::default()
        };
        assert!(
            !can_mark_bot_channel(&text, false, &unloaded),
            "before the read"
        );
        let mut s = dilla_client::state::AppState::default();
        let community = text.community_id;
        assert!(!is_bot_channel(&s, &text), "no catalog");
        s.bot.catalogs.insert(
            community,
            dilla_client::state::bot::BotCatalogView {
                revision: 1,
                commands: Vec::new(),
                status: dilla_client::state::bot::BotStatus {
                    state: dilla_client::state::bot::BotStatusState::Ready,
                    since: None,
                },
                bot_channels: Some(vec![text.id]),
                bot_user: None,
                fingerprint: None,
            },
        );
        assert!(is_bot_channel(&s, &text));
        assert!(!is_bot_channel(&s, &ch(3, 0, None, 0)));
    }

    fn ch(id: u8, kind: u8, parent: Option<u8>, position: u32) -> Channel {
        Channel {
            id: ChannelId([id; 16]),
            community_id: CommunityId([9; 16]),
            kind,
            mode: 0,
            name: format!("c{id}"),
            topic: String::new(),
            parent_id: parent.map(|p| ChannelId([p; 16])),
            position,
            slowmode: 0,
            text_group_id: None,
            group: ChannelGroupState::None,
        }
    }

    #[test]
    fn no_section_shows_before_the_permissions_are_read() {
        let unread = Access::default();
        for s in ServerSection::ALL {
            assert!(!visible(*s, &unread), "{s:?}");
        }
        assert_eq!(active_of(Some(ServerSection::Danger), &unread), None);
    }

    #[test]
    fn a_plain_member_sees_members_self_roles_and_danger() {
        let a = access(|_| {});
        let shown: Vec<_> = ServerSection::ALL
            .iter()
            .filter(|s| visible(**s, &a))
            .map(|s| s.id())
            .collect();
        assert_eq!(shown, ["members", "selfRoles", "danger"]);
        assert_eq!(active_of(None, &a), Some(ServerSection::Members));
    }

    #[test]
    fn each_permission_shows_its_own_sections() {
        let manage = access(|a| a.manage_community = true);
        for s in [
            ServerSection::Overview,
            ServerSection::Gifs,
            ServerSection::Bot,
            ServerSection::Welcome,
            ServerSection::Music,
        ] {
            assert!(visible(s, &manage), "{s:?}");
        }
        // Join roles need both.
        assert!(!visible(ServerSection::JoinRoles, &manage));
        let both = access(|a| {
            a.manage_community = true;
            a.manage_roles = true;
        });
        assert!(visible(ServerSection::JoinRoles, &both));
        assert!(visible(ServerSection::Roles, &both));
        assert!(!visible(ServerSection::Channels, &both));
        assert!(visible(
            ServerSection::Channels,
            &access(|a| a.manage_channels = true)
        ));
        assert!(visible(
            ServerSection::Invites,
            &access(|a| a.create_invite = true)
        ));
        assert!(visible(
            ServerSection::Bans,
            &access(|a| a.ban_members = true)
        ));
        assert!(visible(
            ServerSection::ModerationLog,
            &access(|a| a.view_audit_log = true)
        ));
    }

    #[test]
    fn the_nav_lists_the_prototype_order_first() {
        let all = access(|a| {
            a.manage_community = true;
            a.manage_roles = true;
            a.manage_channels = true;
            a.create_invite = true;
            a.ban_members = true;
            a.view_audit_log = true;
        });
        let ids: Vec<_> = nav_sections(&all).iter().map(|s| s.id()).collect();
        assert_eq!(
            ids,
            [
                "overview",
                "roles",
                "invites",
                "bot",
                "music",
                "channels",
                "gifs",
                "members",
                "selfRoles",
                "welcome",
                "joinRoles",
                "moderationLog",
                "bans",
                "danger"
            ]
        );
    }

    #[test]
    fn a_lost_section_falls_back_to_the_first_allowed() {
        let a = access(|a| a.create_invite = true);
        assert_eq!(
            active_of(Some(ServerSection::Overview), &a),
            Some(ServerSection::Members)
        );
        assert_eq!(
            active_of(Some(ServerSection::Invites), &a),
            Some(ServerSection::Invites)
        );
    }

    #[test]
    fn channels_are_grouped_by_category_in_position_order() {
        let list = [
            ch(10, 2, None, 5),
            ch(11, 2, None, 2),
            ch(1, 0, Some(10), 3),
            ch(2, 0, Some(10), 1),
            ch(3, 0, None, 4),
            ch(4, 1, Some(99), 0), // a parent that is no category: top level
        ];
        let refs: Vec<&Channel> = list.iter().collect();
        let groups = grouped(&refs);
        let names = |g: &Group<'_>| {
            g.channels
                .iter()
                .map(|c| c.name.clone())
                .collect::<Vec<_>>()
        };
        assert!(groups[0].category.is_none());
        assert_eq!(names(&groups[0]), ["c4", "c3"]);
        assert_eq!(groups[1].category.unwrap().name, "c11");
        assert!(groups[1].channels.is_empty());
        assert_eq!(groups[2].category.unwrap().name, "c10");
        assert_eq!(names(&groups[2]), ["c2", "c1"]);
    }

    #[test]
    fn the_afk_row_has_no_moves_and_is_not_counted() {
        let list = [
            ch(1, 0, None, 0),
            ch(2, 0, None, 1),
            ch(3, 1, None, AFK_POSITION),
        ];
        let refs: Vec<&Channel> = list.iter().collect();
        let groups = grouped(&refs);
        let afk = Some(ChannelId([3; 16]));
        assert!(fixed(&list[2], afk));
        assert_eq!(move_slots(&groups[0], afk, &list[0]), (false, true));
        assert_eq!(move_slots(&groups[0], afk, &list[1]), (true, false));
        assert_eq!(move_slots(&groups[0], afk, &list[2]), (false, false));
    }

    #[test]
    fn a_text_channel_shows_its_hash() {
        assert_eq!(shown(&ch(1, 0, None, 0)), "#c1");
        assert_eq!(shown(&ch(2, 1, None, 0)), "c2");
    }

    #[test]
    fn an_expiry_reads_in_days_hours_or_minutes() {
        assert_eq!(in_time(1000 + 86_400, 1000), "in 1 day");
        assert_eq!(in_time(1000 + 5 * 86_400 + 99, 1000), "in 5 days");
        assert_eq!(in_time(1000 + 3600, 1000), "in 1 hour");
        assert_eq!(in_time(1000 + 3 * 3600, 1000), "in 3 hours");
        assert_eq!(in_time(1000 + 12 * 60, 1000), "in 12 minutes");
    }

    #[test]
    fn a_new_link_keeps_its_whole_expiry_and_never_counts_up() {
        // Made with "7 days": a minute (or a second) later it still reads 7 days; the floor read 6.
        assert_eq!(in_time(604_800, 60), "in 7 days");
        assert_eq!(in_time(604_800, 1), "in 7 days");
        assert_eq!(in_time(604_800, 43_199), "in 7 days");
        assert_eq!(in_time(604_800, 43_201), "in 6 days");
        // A count that rounds up to the next unit is said in it.
        assert_eq!(in_time(86_399, 0), "in 1 day");
        assert_eq!(in_time(23 * 3600 + 40 * 60, 0), "in 1 day");
        assert_eq!(in_time(3599, 0), "in 1 hour");
        assert_eq!(in_time(90 * 60, 0), "in 2 hours");
        assert_eq!(in_time(10, 0), "in 1 minutes");
        // Never up as time passes, over the whole first week.
        let mut last = u64::MAX;
        for now in (0..604_800).step_by(97) {
            let s = in_time(604_800, now);
            let n: u64 = s.split(' ').nth(1).unwrap().parse().unwrap();
            let unit = if s.contains("day") {
                86_400
            } else if s.contains("hour") {
                3600
            } else {
                60
            };
            assert!(n * unit <= last, "{now}: {s}");
            last = n * unit;
        }
        assert_eq!(in_time(1000 + 5, 1000), "in 1 minutes");
        assert_eq!(in_time(5, 1000), "in 1 minutes");
    }

    #[test]
    fn never_and_unlimited_are_for_those_who_manage_the_server() {
        assert_eq!(ttl_options(true).len(), 4);
        assert_eq!(ttl_options(false).len(), 3);
        assert_eq!(uses_options(true), [1, 10, 0]);
        assert_eq!(uses_options(false), [1, 10]);
        assert_eq!(clamp_choice(0, 0, false), (DEFAULT_TTL_S, DEFAULT_USES));
        assert_eq!(clamp_choice(0, 0, true), (0, 0));
        assert_eq!(clamp_choice(3600, 1, false), (3600, 1));
    }

    #[test]
    fn the_made_note_says_uses_and_expiry() {
        assert_eq!(
            made_note(10, 604_800),
            "It can be used 10 times and expires in 7 days. The link is shown only once."
        );
        assert_eq!(
            made_note(1, 3600),
            "It can be used 1 time and expires in 1 hour. The link is shown only once."
        );
        assert_eq!(
            made_note(0, 0),
            "It can be used any number of times and expires never. The link is shown only once."
        );
    }

    #[test]
    fn a_live_link_shows_its_uses_and_expiry() {
        let invite = Invite {
            id: dilla_client::state::server_admin::InviteId([1; 16]),
            created_by: None,
            max_uses: 10,
            uses: 3,
            created: 0,
            expires: 1000 + 2 * 86_400,
        };
        let t = invite_texts(&invite, "d.example", 1000);
        assert_eq!(t.code, "d.example/i/••••");
        assert_eq!(t.uses, "3 / 10 uses");
        assert_eq!(t.expires, "expires in 2 days");
        let forever = Invite {
            max_uses: 0,
            expires: 0,
            ..invite
        };
        let t = invite_texts(&forever, "d.example", 1000);
        assert_eq!(t.uses, "3 / ∞ uses");
        assert_eq!(t.expires, "expires never");
    }
}
