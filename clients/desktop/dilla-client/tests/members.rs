//! Task 14's client tests for the members area (standard procedure S2).
//!
//! The first half ports the logic tests of the TypeScript sources, so the same inputs give the same outputs:
//! packages/client-core/src/admin/{perm,rank,authority,roles}.test.ts (the resolver against
//! `protocol/vectors/permissions.json`), packages/web/src/screens/admin/{members-model,member-card,role-palette}.test.ts
//! and the reason checks of Moderation.tsx. They need no server.
//!
//! The second half runs against a real dillad (dilla-testhost), `#[ignore]`d like the other suites: it needs
//! `target/dilla-testhost` and the WASI core (see tests/sync.rs), and the desktop-gpui CI job runs it with `--ignored`.

use std::collections::HashMap;
use std::time::Duration;

use dilla_client::features::members::{
    GroupKind, MemberTag, MenuAction, member_card, member_groups, menu_for, signal_palette,
};
use dilla_client::features::moderation::{ReasonProblem, TimeoutLength, reason_problem};
use dilla_client::features::roles::authority::{AuthorityChannel, AuthorityView, authority_for};
use dilla_client::features::roles::perm::{
    self, PERM_ALL, PermInput, PermKey, PermOverwrite, PermRole, ROLE_PERM_GROUPS,
};
use dilla_client::features::roles::rank::{OWNER_TOP, access_of, can_moderate, top_of};
use dilla_client::features::roles::{
    Direction, RolePatch, create_position, create_problem, move_plan, update_problem,
};
use dilla_client::state::members::{Member, Presence};
use dilla_client::state::roles::{CommunityAccess, RoleSummary};
use dilla_client::state::{CommunityId, UserId};

fn id16(n: u8) -> [u8; 16] {
    [n; 16]
}

fn unhex(s: &str) -> [u8; 16] {
    let mut out = [0u8; 16];
    for (i, b) in out.iter_mut().enumerate() {
        *b = u8::from_str_radix(&s[2 * i..2 * i + 2], 16).unwrap();
    }
    out
}

// ---- perm.test.ts ----

#[test]
fn perm_pins_all_26_bits_of_perm_go_and_the_vector_file() {
    let file: serde_json::Value = serde_json::from_str(
        &std::fs::read_to_string(
            std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
                .join("../../../protocol/vectors/permissions.json"),
        )
        .unwrap(),
    )
    .unwrap();
    let bits = file["bits"].as_object().unwrap();
    let ours: [(&str, u32); 26] = [
        ("view_channel", perm::VIEW_CHANNEL),
        ("send_messages", perm::SEND_MESSAGES),
        ("manage_messages", perm::MANAGE_MESSAGES),
        ("pin_messages", perm::PIN_MESSAGES),
        ("attach_files", perm::ATTACH_FILES),
        ("add_reactions", perm::ADD_REACTIONS),
        ("read_history", perm::READ_HISTORY),
        ("connect", perm::CONNECT),
        ("speak", perm::SPEAK),
        ("video", perm::VIDEO),
        ("screen_share", perm::SCREEN_SHARE),
        ("create_invite", perm::CREATE_INVITE),
        ("kick_members", perm::KICK_MEMBERS),
        ("ban_members", perm::BAN_MEMBERS),
        ("manage_channels", perm::MANAGE_CHANNELS),
        ("manage_roles", perm::MANAGE_ROLES),
        ("manage_community", perm::MANAGE_COMMUNITY),
        ("view_audit_log", perm::VIEW_AUDIT_LOG),
        ("mention_everyone", perm::MENTION_EVERYONE),
        ("bypass_slowmode", perm::BYPASS_SLOWMODE),
        ("administrator", perm::ADMINISTRATOR),
        ("mute_members", perm::MUTE_MEMBERS),
        ("move_members", perm::MOVE_MEMBERS),
        ("manage_nicknames", perm::MANAGE_NICKNAMES),
        ("moderate_members", perm::MODERATE_MEMBERS),
        ("control_music", perm::CONTROL_MUSIC),
    ];
    assert_eq!(bits.len(), 26);
    for (name, bit) in ours {
        assert_eq!(bits[name].as_u64(), Some(u64::from(bit)), "{name}");
    }
    assert_eq!(
        perm::COMMUNITY_BITS,
        4096 | 8192 | 65536 | 32768 | 131_072 | 1_048_576 | 8_388_608 | 16_777_216
    );
}

#[test]
fn perm_keys_and_role_groups_cover_all_26_bits_once() {
    let mut all = 0;
    for k in PermKey::ALL {
        assert_eq!(all & k.bit(), 0, "{k:?} repeats a bit");
        all |= k.bit();
    }
    assert_eq!(all, PERM_ALL);
    let ids: Vec<&str> = ROLE_PERM_GROUPS.iter().map(|g| g.0).collect();
    assert_eq!(ids, ["general", "moderation", "voice", "music"]);
    let keys: Vec<PermKey> = ROLE_PERM_GROUPS
        .iter()
        .flat_map(|g| g.1.iter().copied())
        .collect();
    assert_eq!(keys.len(), 26);
    assert_eq!(
        &ROLE_PERM_GROUPS[0].1[..4],
        &[
            PermKey::View,
            PermKey::Send,
            PermKey::Pin,
            PermKey::Everyone
        ]
    );
    assert_eq!(
        &ROLE_PERM_GROUPS[1].1[..3],
        &[PermKey::ManageChannels, PermKey::Kick, PermKey::Ban]
    );
    assert_eq!(ROLE_PERM_GROUPS[3].1, &[PermKey::Music]);
    let channel: Vec<PermKey> = perm::PERM_GROUPS
        .iter()
        .flat_map(|g| g.1.iter().copied())
        .collect();
    assert_eq!(channel.len(), 26);
    assert!(perm::PERM_GROUPS[1].1.contains(&PermKey::Moderate));
}

#[test]
fn the_resolver_answers_every_case_of_the_vector_file() {
    let file: serde_json::Value = serde_json::from_str(
        &std::fs::read_to_string(
            std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
                .join("../../../protocol/vectors/permissions.json"),
        )
        .unwrap(),
    )
    .unwrap();
    let cases = file["cases"].as_array().unwrap();
    assert!(!cases.is_empty());
    for c in cases {
        let name = c["name"].as_str().unwrap();
        let roles: Vec<PermRole> = c["roles"]
            .as_array()
            .unwrap()
            .iter()
            .map(|r| PermRole {
                id: unhex(r["id"].as_str().unwrap()),
                position: r["position"].as_u64().unwrap(),
                allow: r["allow"].as_u64().unwrap() as u32,
                deny: r["deny"].as_u64().unwrap() as u32,
            })
            .collect();
        let held: Vec<[u8; 16]> = c["held"]
            .as_array()
            .unwrap()
            .iter()
            .map(|h| unhex(h.as_str().unwrap()))
            .collect();
        let overwrites: Vec<PermOverwrite> = c["overwrites"]
            .as_array()
            .unwrap()
            .iter()
            .map(|o| PermOverwrite {
                kind: o["kind"].as_u64().unwrap() as u8,
                target_id: unhex(o["target"].as_str().unwrap()),
                allow: o["allow"].as_u64().unwrap() as u32,
                deny: o["deny"].as_u64().unwrap() as u32,
            })
            .collect();
        let user = unhex(c["user"].as_str().unwrap());
        let mut reversed = roles.clone();
        reversed.reverse();
        for roles in [&roles, &reversed] {
            let input = PermInput {
                owner: unhex(c["owner"].as_str().unwrap()),
                everyone_id: unhex(c["everyone"].as_str().unwrap()),
                roles,
                held: &held,
            };
            assert_eq!(
                u64::from(perm::resolve_community(&input, user)),
                c["community"].as_u64().unwrap(),
                "{name}: community"
            );
            assert_eq!(
                u64::from(perm::resolve_channel(&input, &overwrites, user, false)),
                c["channel"].as_u64().unwrap(),
                "{name}: channel"
            );
        }
    }
}

#[test]
fn a_base_without_view_stays_0_and_the_afk_channel_grants_view_and_connect() {
    let roles = [PermRole {
        id: id16(0xee),
        position: 0,
        allow: perm::SEND_MESSAGES,
        deny: 0,
    }];
    let input = PermInput {
        owner: id16(0xaa),
        everyone_id: id16(0xee),
        roles: &roles,
        held: &[],
    };
    assert_eq!(perm::resolve_community(&input, id16(0xbb)), 0);
    let own = [PermOverwrite {
        kind: 1,
        target_id: id16(0xbb),
        allow: perm::VIEW_CHANNEL,
        deny: 0,
    }];
    assert_eq!(perm::resolve_channel(&input, &own, id16(0xbb), false), 0);
    assert_eq!(
        perm::resolve_channel(&input, &[], id16(0xbb), true),
        perm::VIEW_CHANNEL | perm::CONNECT
    );
    assert_eq!(
        perm::resolve_channel(&input, &[], id16(0xaa), true),
        PERM_ALL
    );
}

#[test]
fn is_private_is_an_everyone_deny_of_view_only() {
    let e = id16(0xee);
    let ow = |kind, target, deny| PermOverwrite {
        kind,
        target_id: target,
        allow: 0,
        deny,
    };
    assert!(perm::is_private(&[ow(0, e, perm::VIEW_CHANNEL)], e));
    assert!(!perm::is_private(&[ow(0, e, perm::SEND_MESSAGES)], e));
    assert!(!perm::is_private(
        &[ow(0, id16(0x11), perm::VIEW_CHANNEL)],
        e
    ));
    assert!(!perm::is_private(&[ow(1, e, perm::VIEW_CHANNEL)], e));
}

// ---- rank.test.ts ----

#[test]
fn access_of_gives_owner_and_administrator_everything_and_reads_each_bit() {
    for a in [
        access_of(0, OWNER_TOP, true),
        access_of(u64::from(perm::ADMINISTRATOR), 3, false),
    ] {
        assert!(
            a.manage_community
                && a.view_audit_log
                && a.manage_channels
                && a.kick_members
                && a.ban_members
                && a.moderate_members
                && a.manage_messages
                && a.create_invite
                && a.manage_roles
                && a.loaded
        );
    }
    let member = access_of(1 | 2 | 64 | 128, 0, false);
    assert_eq!(
        member,
        CommunityAccess {
            loaded: true,
            ..CommunityAccess::default()
        }
    );
    let k = access_of(u64::from(perm::KICK_MEMBERS), 1, false);
    assert!(k.kick_members && !k.ban_members && !k.create_invite);
    let b = access_of(u64::from(perm::BAN_MEMBERS), 1, false);
    assert!(b.ban_members && !b.kick_members && !b.moderate_members);
    let m = access_of(u64::from(perm::MODERATE_MEMBERS), 1, false);
    assert!(m.moderate_members && !m.kick_members);
    let r = access_of(u64::from(perm::MANAGE_ROLES), 1, false);
    assert!(r.manage_roles && !r.manage_channels);
    let l = access_of(u64::from(perm::VIEW_AUDIT_LOG), 1, false);
    assert!(l.view_audit_log && !l.manage_community);
}

fn role(id: [u8; 16], position: u32) -> RoleSummary {
    RoleSummary {
        id,
        name: format!("r{position}"),
        color: 0,
        position,
        hoist: false,
        mentionable: false,
        allow: 0,
        deny: 0,
        members: 0,
    }
}

#[test]
fn top_of_is_the_highest_held_position_and_an_unknown_role_ranks_just_under_the_owner() {
    let roles = [role(id16(0xa), 2), role(id16(0xb), 7), role(id16(0xc), 4)];
    assert_eq!(top_of(&[id16(0xa), id16(0xc)], &roles), 4);
    assert_eq!(top_of(&[id16(0xa), id16(0xb), id16(0xc)], &roles), 7);
    assert_eq!(top_of(&[], &roles), 0);
    assert_eq!(top_of(&[id16(0xa), id16(0xff)], &roles), OWNER_TOP - 1);
}

#[test]
fn can_moderate_never_the_owner_or_self_and_only_from_a_strictly_higher_rank() {
    let (me, them, boss) = (id16(1), id16(2), id16(3));
    assert!(!can_moderate(me, boss, boss, 5, 3));
    assert!(!can_moderate(boss, boss, boss, OWNER_TOP, 0));
    assert!(!can_moderate(me, me, boss, 5, 3));
    assert!(can_moderate(boss, them, boss, 0, 9));
    assert!(!can_moderate(me, them, boss, 0, 0));
    assert!(!can_moderate(me, them, boss, 5, 5));
    assert!(!can_moderate(me, them, boss, 5, 6));
    assert!(can_moderate(me, them, boss, 5, 4));
    assert!(can_moderate(me, them, boss, 1, 0));
    let roles = [role(id16(0xa), 2)];
    assert!(!can_moderate(
        me,
        them,
        boss,
        top_of(&[id16(0xa)], &roles),
        top_of(&[id16(0xff)], &roles)
    ));
}

// ---- authority.test.ts ----

#[test]
fn authority_lists_pinners_and_broadcasters_per_group_from_each_channels_own_overwrites() {
    let (owner, everyone, modr) = (id16(0x0a), id16(0x40), id16(0x41));
    let (a, b, c) = (id16(0x5c), id16(0x5a), id16(0x5b));
    let (pin, all, view) = (
        perm::PIN_MESSAGES,
        perm::MENTION_EVERYONE,
        perm::VIEW_CHANNEL,
    );
    let roles = vec![
        PermRole {
            id: everyone,
            position: 0,
            allow: view,
            deny: 0,
        },
        PermRole {
            id: modr,
            position: 1,
            allow: pin | all,
            deny: 0,
        },
    ];
    let members = vec![
        (owner, vec![]),
        (a, vec![modr]),
        (b, vec![]),
        (c, vec![modr]),
    ];
    let ch = |id, kind, mode, group| AuthorityChannel {
        id: id16(id),
        kind,
        mode,
        text_group: group,
    };
    let channels = vec![ch(0xc1, 0, 0, Some(id16(0xc2)))];
    let view_of = |overwrites: &HashMap<[u8; 16], Vec<PermOverwrite>>, roles: &[PermRole]| {
        authority_for(owner, everyone, roles, &members, overwrites, &channels)
    };
    let none = HashMap::new();
    assert_eq!(
        view_of(&none, &roles)[&id16(0xc2)],
        AuthorityView {
            pinners: vec![owner, c, a],
            broadcasters: vec![owner, c, a]
        }
    );
    // Only text channels (kind 0, mode 0) with a group.
    let mixed = vec![
        ch(0xc1, 0, 1, Some(id16(0xc2))),
        ch(0xc3, 1, 0, Some(id16(0xc4))),
        ch(0xc5, 0, 0, None),
        ch(0xc6, 0, 0, Some(id16(0xc7))),
    ];
    let keys: Vec<[u8; 16]> = authority_for(owner, everyone, &roles, &members, &none, &mixed)
        .into_keys()
        .collect();
    assert_eq!(keys, vec![id16(0xc7)]);
    // Another channel's overwrite does not count; its own do.
    let deny = PermOverwrite {
        kind: 0,
        target_id: modr,
        allow: 0,
        deny: pin,
    };
    let other = HashMap::from([(id16(0xc9), vec![deny.clone()])]);
    assert_eq!(
        view_of(&other, &roles)[&id16(0xc2)].pinners,
        vec![owner, c, a]
    );
    let own = HashMap::from([(
        id16(0xc1),
        vec![
            deny,
            PermOverwrite {
                kind: 1,
                target_id: b,
                allow: pin,
                deny: 0,
            },
        ],
    )]);
    assert_eq!(
        view_of(&own, &roles)[&id16(0xc2)],
        AuthorityView {
            pinners: vec![owner, b],
            broadcasters: vec![owner, c, a]
        }
    );
    // A role that lost the bit, and a channel nobody but the owner can view.
    let lost = vec![
        roles[0].clone(),
        PermRole {
            id: modr,
            position: 1,
            allow: all,
            deny: 0,
        },
    ];
    assert_eq!(view_of(&none, &lost)[&id16(0xc2)].pinners, vec![owner]);
    let hidden = HashMap::from([(
        id16(0xc1),
        vec![PermOverwrite {
            kind: 0,
            target_id: everyone,
            allow: 0,
            deny: view,
        }],
    )]);
    assert_eq!(
        view_of(&hidden, &roles)[&id16(0xc2)],
        AuthorityView {
            pinners: vec![owner],
            broadcasters: vec![owner]
        }
    );
}

// ---- roles.test.ts: input checks and the position plans ----

#[test]
fn role_input_checks_give_the_workers_reasons() {
    assert_eq!(
        create_problem("  ", 0, 0).as_deref(),
        Some("role name is blank")
    );
    assert_eq!(
        create_problem(&"é".repeat(51), 0, 0).as_deref(),
        Some("role name is longer than 100 bytes")
    );
    assert_eq!(
        create_problem("x", 16_777_216, 0).as_deref(),
        Some("color must be an integer 0..16777215")
    );
    assert_eq!(
        create_problem("x", 0, 67_108_864).as_deref(),
        Some("allow must be an integer 0..67108863")
    );
    assert_eq!(create_problem("x", 0xff_ffff, PERM_ALL), None);
    assert_eq!(
        update_problem(&RolePatch::default()).as_deref(),
        Some("nothing to change")
    );
    assert_eq!(
        update_problem(&RolePatch {
            name: Some(String::new()),
            ..RolePatch::default()
        })
        .as_deref(),
        Some("role name is blank")
    );
    assert_eq!(
        update_problem(&RolePatch {
            hoist: Some(true),
            ..RolePatch::default()
        }),
        None
    );
}

#[test]
fn create_takes_the_lowest_free_position_below_my_top() {
    let rows = [
        role(id16(0x40), 0),
        role(id16(0x41), 1),
        role(id16(0x42), 3),
    ];
    assert_eq!(create_position(&rows, false, 5), Some(2));
    assert_eq!(
        create_position(&[role(id16(0x40), 0), role(id16(0x41), 1)], false, 2),
        None
    );
    assert_eq!(
        create_position(&[role(id16(0x40), 0), role(id16(0x41), 1)], true, 0),
        Some(2)
    );
}

#[test]
fn move_trades_with_the_neighbour_below_my_top_and_never_with_everyone() {
    let (e, a, b) = (id16(0x40), id16(0x41), id16(0x42));
    let rows = [role(e, 0), role(a, 1), role(b, 2)];
    assert_eq!(
        move_plan(&rows, a, Direction::Up, false, 9),
        Some(vec![(a, 2), (b, 1)])
    );
    assert_eq!(move_plan(&rows, a, Direction::Up, false, 2), Some(vec![]));
    assert_eq!(move_plan(&rows, a, Direction::Down, true, 0), Some(vec![]));
    let tied = [role(e, 0), role(a, 2), role(b, 2)];
    assert_eq!(
        move_plan(&tied, a, Direction::Up, true, 0),
        Some(vec![(a, 3)])
    );
    assert_eq!(move_plan(&rows, id16(0x99), Direction::Up, true, 0), None);
}

// ---- members-model.test.ts, member-card.test.ts, role-palette ----

fn hid(n: u8) -> [u8; 16] {
    [n; 16]
}

fn member(user: u8, username: &str, roles: &[[u8; 16]], kind: u8, display: &str) -> Member {
    Member {
        user_id: UserId(hid(user)),
        username: username.into(),
        display: display.into(),
        kind,
        role_ids: roles.to_vec(),
        status_msg: String::new(),
        bio: String::new(),
    }
}

struct World {
    roles: Vec<RoleSummary>,
    members: Vec<Member>,
}

const OWNER: u8 = 1;
const ME: u8 = 2;
const BOB: u8 = 3;
const CAL: u8 = 4;
const BOT: u8 = 5;

fn world() -> World {
    let r = |id: u8, name: &str, position: u32, hoist: bool| RoleSummary {
        id: hid(id),
        name: name.into(),
        color: 0,
        position,
        hoist,
        mentionable: false,
        allow: 0,
        deny: 0,
        members: 0,
    };
    let (modr, crew, quiet) = (
        r(10, "mods", 5, true),
        r(11, "crew", 2, true),
        r(12, "quiet", 9, false),
    );
    let members = vec![
        member(OWNER, "olof", &[], 0, ""),
        member(ME, "ada", &[crew.id], 0, ""),
        member(BOB, "bob", &[crew.id, modr.id], 0, ""),
        member(CAL, "carol", &[quiet.id], 0, "Åsa"),
        member(BOT, "robo", &[], 1, ""),
    ];
    World {
        roles: vec![crew, modr, quiet],
        members,
    }
}

fn access(over: impl FnOnce(&mut CommunityAccess)) -> CommunityAccess {
    let mut a = CommunityAccess {
        top: 5,
        kick_members: true,
        ban_members: true,
        loaded: true,
        everyone_id: Some(hid(0xab)),
        ..CommunityAccess::default()
    };
    over(&mut a);
    a
}

fn ids(groups: &[dilla_client::features::members::MemberGroup]) -> Vec<(GroupKind, Vec<u8>)> {
    groups
        .iter()
        .filter(|g| !g.members.is_empty())
        .map(|g| (g.kind, g.members.iter().map(|m| m.user_id.0[0]).collect()))
        .collect()
}

#[test]
fn member_groups_by_highest_hoisted_role_then_the_rest_bots_and_offline() {
    let w = world();
    let presence = HashMap::from([
        (UserId(hid(OWNER)), Presence::Offline),
        (UserId(hid(ME)), Presence::Online),
        (UserId(hid(BOB)), Presence::Dnd),
        (UserId(hid(CAL)), Presence::Idle),
        (UserId(hid(BOT)), Presence::Offline),
    ]);
    let groups = member_groups(
        &w.members,
        &w.roles,
        UserId(hid(OWNER)),
        Some(UserId(hid(ME))),
        Some(&presence),
    );
    assert_eq!(
        ids(&groups),
        vec![
            (GroupKind::Role, vec![BOB]),
            (GroupKind::Role, vec![ME]),
            (GroupKind::Online, vec![CAL]),
            (GroupKind::Bots, vec![BOT]),
            (GroupKind::Offline, vec![OWNER]),
        ]
    );
    assert_eq!(groups[0].role.as_ref().unwrap().name, "mods");
    let groups = member_groups(
        &w.members,
        &w.roles,
        UserId(hid(OWNER)),
        Some(UserId(hid(ME))),
        None,
    );
    assert_eq!(
        ids(&groups),
        vec![
            (GroupKind::Role, vec![BOB]),
            (GroupKind::Role, vec![ME]),
            (GroupKind::Online, vec![CAL, OWNER]),
            (GroupKind::Bots, vec![BOT]),
        ]
    );
    let rest = groups.iter().find(|g| g.kind == GroupKind::Online).unwrap();
    assert_eq!(
        rest.members
            .iter()
            .map(|m| m.name.as_str())
            .collect::<Vec<_>>(),
        ["Åsa", "olof"]
    );
    let tags: HashMap<u8, Vec<MemberTag>> = groups
        .iter()
        .flat_map(|g| &g.members)
        .map(|m| (m.user_id.0[0], m.tags.clone()))
        .collect();
    assert_eq!(tags[&OWNER], [MemberTag::Owner]);
    assert_eq!(tags[&ME], [MemberTag::You]);
    assert_eq!(tags[&BOT], [MemberTag::Bot]);
    assert!(tags[&BOB].is_empty());
}

#[test]
fn menu_for_follows_rank_and_each_loaded_permission() {
    use MenuAction::*;
    let w = world();
    let (me, owner) = (UserId(hid(ME)), UserId(hid(OWNER)));
    let menu = |m: &Member, a: Option<&CommunityAccess>| menu_for(m, a, me, owner, &w.roles);
    let crew = w.roles[0].id;
    let lower = member(CAL, "carol", &[crew], 0, "");
    let plain = member(CAL, "carol", &[], 0, "");
    let bob = &w.members[2];
    assert_eq!(
        menu(&lower, Some(&access(|_| {}))),
        [Message, Compare, Kick, Ban]
    );
    assert_eq!(menu(&w.members[1], Some(&access(|_| {}))), []);
    assert_eq!(
        menu(&w.members[1], Some(&access(|a| a.manage_roles = true))),
        [Roles]
    );
    assert_eq!(
        menu(&w.members[0], Some(&access(|_| {}))),
        [Message, Compare]
    );
    assert_eq!(menu(bob, Some(&access(|a| a.top = 2))), [Message, Compare]);
    assert_eq!(menu(bob, Some(&access(|a| a.top = 5))), [Message, Compare]);
    assert_eq!(
        menu(&plain, Some(&access(|a| a.ban_members = false))),
        [Message, Compare, Kick]
    );
    assert_eq!(
        menu(&plain, Some(&access(|a| a.kick_members = false))),
        [Message, Compare, Ban]
    );
    assert_eq!(
        menu(&plain, Some(&access(|a| a.loaded = false))),
        [Message, Compare]
    );
    assert_eq!(menu(&plain, None), [Message, Compare]);
    assert_eq!(
        menu(&lower, Some(&access(|a| a.moderate_members = true))),
        [Message, Compare, Warn, Timeout, Kick, Ban]
    );
    assert_eq!(
        menu(&w.members[0], Some(&access(|a| a.moderate_members = true))),
        [Message, Compare]
    );
    assert_eq!(
        menu(bob, Some(&access(|a| a.moderate_members = true))),
        [Message, Compare]
    );
    assert_eq!(
        menu(&plain, Some(&access(|a| a.manage_roles = true))),
        [Message, Compare, Roles, Kick, Ban]
    );
    assert_eq!(
        menu(
            &plain,
            Some(&access(|a| {
                a.manage_roles = true;
                a.loaded = false;
            }))
        ),
        [Message, Compare]
    );
}

#[test]
fn members_take_the_colour_of_their_highest_coloured_role() {
    let mut red = role(hid(20), 3);
    red.color = 0xE5_484D;
    let mut blue = role(hid(21), 4);
    blue.color = 0x00_90FF;
    let plain = role(hid(22), 8);
    let members = [
        member(BOB, "bob", &[red.id, blue.id, plain.id], 0, ""),
        member(CAL, "carol", &[plain.id], 0, ""),
    ];
    let rows: Vec<_> = member_groups(
        &members,
        &[red, blue, plain],
        UserId(hid(OWNER)),
        Some(UserId(hid(ME))),
        None,
    )
    .into_iter()
    .flat_map(|g| g.members)
    .collect();
    assert_eq!(
        rows.iter().find(|r| r.user_id.0[0] == BOB).unwrap().color,
        Some(0x00_90FF)
    );
    assert_eq!(
        rows.iter().find(|r| r.user_id.0[0] == CAL).unwrap().color,
        None
    );
}

#[test]
fn the_card_leaves_everyone_out_and_carries_status_and_bio() {
    let mut everyone = role(hid(0xab), 0);
    everyone.name = "@everyone".into();
    let mut modr = role(hid(0xcd), 5);
    modr.name = "mods".into();
    modr.color = 0x34_98db;
    let mut m = member(1, "bob", &[everyone.id, modr.id], 0, "");
    let card = member_card(
        &m,
        None,
        UserId(hid(0x77)),
        UserId(hid(0x78)),
        &[everyone.clone(), modr.clone()],
    );
    assert_eq!(card.roles, [("mods".to_owned(), 0x34_98db)]);
    assert_eq!((card.status.as_str(), card.bio.as_str()), ("", ""));
    m.status_msg = "on a call".into();
    m.bio = "one\ntwo".into();
    let card = member_card(&m, None, UserId(hid(1)), UserId(hid(0x78)), &[everyone]);
    assert_eq!(
        (card.status.as_str(), card.bio.as_str()),
        ("on a call", "one\ntwo")
    );
    assert!(card.is_you && card.actions.is_empty());
}

#[test]
fn the_signal_palette_adds_the_roles_own_colour_only_when_new() {
    assert_eq!(signal_palette(0).len(), 6);
    assert_eq!(signal_palette(0xE9_B6EC).len(), 6);
    let p = signal_palette(0x12_3456);
    assert_eq!(p.len(), 7);
    assert_eq!(p[6], (0x12_3456, "custom"));
    assert_eq!(
        p.iter().map(|x| x.1).take(6).collect::<Vec<_>>(),
        ["pink", "blue", "amber", "green", "orange", "grey"]
    );
}

#[test]
fn a_reason_is_refused_over_512_bytes_or_with_a_control_character() {
    assert_eq!(reason_problem("spam"), None);
    assert_eq!(reason_problem("line\nfeed\tand tab"), None);
    assert_eq!(reason_problem(&"é".repeat(257)), Some(ReasonProblem::Long));
    assert_eq!(reason_problem(&"x".repeat(512)), None);
    assert_eq!(reason_problem("bell\u{7}"), Some(ReasonProblem::Control));
    assert_eq!(reason_problem("cr\r"), Some(ReasonProblem::Control));
    assert_eq!(reason_problem("c1\u{85}"), Some(ReasonProblem::Control));
    assert_eq!(TimeoutLength::TenMinutes.seconds(), Some(600));
    assert_eq!(TimeoutLength::Clear.seconds(), None);
}

// ---- RolesSection.tsx, SelfRolesSection.tsx, MemberRolesFlow.tsx ----

mod editors {
    use dilla_client::features::roles::perm::{self, PERM_ALL};
    use dilla_client::features::roles::{
        AssignLock, AssignRefusal, PermLock, SELF_ROLE_BITS, assign_lock, assign_refusal,
        assignable, held_bits, permission_lock, self_role_candidates,
    };
    use dilla_client::state::roles::{CommunityAccess, RoleSummary};
    use dilla_client::sync::api::ApiError;

    fn r(id: u8, position: u32, allow: u32) -> RoleSummary {
        RoleSummary {
            id: [id; 16],
            name: format!("r{id}"),
            color: 0,
            position,
            hoist: false,
            mentionable: false,
            allow,
            deny: 0,
            members: 0,
        }
    }

    fn access(owner: bool, top: u32) -> CommunityAccess {
        CommunityAccess {
            owner,
            top,
            loaded: true,
            manage_roles: true,
            ..CommunityAccess::default()
        }
    }

    const ME: [u8; 16] = [2; 16];
    const OWNER: [u8; 16] = [1; 16];
    const EVERYONE: u8 = 0x40;

    #[test]
    fn held_bits_are_all_for_owner_and_administrators_else_the_resolvers() {
        let roles = [
            r(EVERYONE, 0, perm::VIEW_CHANNEL | perm::SEND_MESSAGES),
            r(0x41, 1, perm::MANAGE_ROLES),
            r(0x42, 2, perm::ADMINISTRATOR),
        ];
        let e = [EVERYONE; 16];
        assert_eq!(held_bits(true, OWNER, e, &roles, &[], OWNER), PERM_ALL);
        assert_eq!(
            held_bits(false, OWNER, e, &roles, &[[0x41; 16]], ME),
            perm::VIEW_CHANNEL | perm::SEND_MESSAGES | perm::MANAGE_ROLES
        );
        assert_eq!(
            held_bits(false, OWNER, e, &roles, &[[0x42; 16]], ME),
            PERM_ALL
        );
    }

    #[test]
    fn a_manager_cannot_toggle_a_bit_they_lack() {
        let mine = perm::VIEW_CHANNEL | perm::MANAGE_ROLES;
        assert_eq!(permission_lock(true, mine, perm::VIEW_CHANNEL), None);
        assert_eq!(
            permission_lock(true, mine, perm::BAN_MEMBERS),
            Some(PermLock::NotHeld)
        );
        assert_eq!(
            permission_lock(false, mine, perm::VIEW_CHANNEL),
            Some(PermLock::Locked)
        );
    }

    #[test]
    fn self_role_candidates_carry_only_self_role_bits_and_sit_below_my_rank() {
        assert_eq!(
            SELF_ROLE_BITS,
            perm::VIEW_CHANNEL
                | perm::SEND_MESSAGES
                | perm::ATTACH_FILES
                | perm::ADD_REACTIONS
                | perm::READ_HISTORY
                | perm::CONNECT
                | perm::SPEAK
                | perm::VIDEO
                | perm::SCREEN_SHARE
        );
        let mut denies = r(0x45, 2, perm::VIEW_CHANNEL);
        denies.deny = perm::KICK_MEMBERS;
        let roles = [
            r(EVERYONE, 0, perm::VIEW_CHANNEL),
            r(0x41, 1, perm::SEND_MESSAGES | perm::SPEAK),
            r(0x42, 2, perm::SEND_MESSAGES | perm::PIN_MESSAGES),
            r(0x43, 4, perm::VIEW_CHANNEL),
            r(0x44, 5, 0),
            denies,
        ];
        let ids = |a: &CommunityAccess| -> Vec<u8> {
            self_role_candidates(&roles, a)
                .iter()
                .map(|r| r.id[0])
                .collect()
        };
        assert_eq!(ids(&access(false, 4)), [0x41]);
        assert_eq!(ids(&access(true, 0)), [0x41, 0x43, 0x44]);
    }

    #[test]
    fn assignable_roles_are_highest_first_without_everyone_and_locked_by_rank_then_bits() {
        let roles = [
            r(EVERYONE, 0, perm::VIEW_CHANNEL),
            r(0x41, 1, perm::SEND_MESSAGES),
            r(0x42, 3, perm::BAN_MEMBERS),
            r(0x43, 5, 0),
        ];
        let list = assignable(&roles, Some([EVERYONE; 16]));
        assert_eq!(
            list.iter().map(|r| r.id[0]).collect::<Vec<_>>(),
            [0x43, 0x42, 0x41]
        );
        let me = access(false, 4);
        let mine = perm::VIEW_CHANNEL | perm::SEND_MESSAGES | perm::MANAGE_ROLES;
        assert_eq!(assign_lock(&list[0], &me, mine), Some(AssignLock::Rank));
        assert_eq!(assign_lock(&list[1], &me, mine), Some(AssignLock::NotHeld));
        assert_eq!(assign_lock(&list[2], &me, mine), None);
        assert_eq!(assign_lock(&list[0], &access(true, 0), PERM_ALL), None);
    }

    #[test]
    fn an_assignment_refusal_reads_as_the_web_reads_it() {
        let e = |status, code: &str| ApiError::local(status, code, "");
        assert_eq!(
            assign_refusal(&e(403, "E_TARGET_NO_FACTOR"), true),
            AssignRefusal::TwoFactor
        );
        assert_eq!(
            assign_refusal(&e(403, "E_FORBIDDEN"), true),
            AssignRefusal::Forbidden
        );
        assert_eq!(
            assign_refusal(&e(0, "E_FORBIDDEN"), true),
            AssignRefusal::Forbidden
        );
        assert_eq!(
            assign_refusal(&e(404, "E_NOT_FOUND"), false),
            AssignRefusal::RoleGone
        );
        assert_eq!(
            assign_refusal(&e(404, "E_NOT_FOUND"), true),
            AssignRefusal::MemberGone
        );
        assert_eq!(
            assign_refusal(&e(500, "E_INTERNAL"), true),
            AssignRefusal::Other("E_INTERNAL".into())
        );
    }
}

// ---- against a real dillad ----

mod live {
    use super::*;
    use dilla_client::Client;
    use dilla_client::features::moderation::BanLength;
    use dilla_client::state::{AppState, ChannelId, TimelineItemState};
    use dilla_client::testing::host::{
        Host, Person, clear_session, community, sign_up, start, start_host, wait_for,
    };

    async fn people(tag: &str, n: usize) -> (Host, Vec<Person>, [u8; 16]) {
        let host = tokio::task::spawn_blocking({
            let tag = tag.to_owned();
            move || start_host(&tag, "127.0.0.1:0", None)
        })
        .await
        .unwrap();
        let invite = host.invites.last().unwrap().clone();
        let names = ["olof", "ada", "bob", "carol"];
        let mut ps = Vec::new();
        for name in &names[..n] {
            ps.push(sign_up(&host, &invite, name, name, tag).await);
        }
        tokio::task::spawn_blocking(move || {
            let others: Vec<&Person> = ps[1..].iter().collect();
            let c = community(&host, &ps[0], "crew", &others);
            for p in &ps {
                clear_session(p);
            }
            (host, ps, c)
        })
        .await
        .unwrap()
    }

    async fn listed(client: &Client, c: CommunityId, n: usize) {
        wait_for(client, Duration::from_secs(30), "the member list", |s| {
            s.members.get(&c).is_some_and(|m| m.len() == n)
        })
        .await;
    }

    /// The access a fresh read publishes, once it is in the snapshot.
    async fn access_of_in(
        client: &Client,
        c: CommunityId,
        pred: impl Fn(&CommunityAccess) -> bool,
    ) -> CommunityAccess {
        client.roles().load_access(c).await.unwrap();
        until(client, "the access read", |s| pred(&s.roles.access_of(c)))
            .await
            .roles
            .access_of(c)
    }

    /// What is published lands in the snapshot a moment after the call answers.
    async fn until(
        client: &Client,
        what: &str,
        pred: impl Fn(&dilla_client::state::AppState) -> bool,
    ) -> std::sync::Arc<dilla_client::state::AppState> {
        wait_for(client, Duration::from_secs(10), what, pred).await
    }

    /// e2e/web/roles.spec.ts (client half): the owner makes a role, assigns it, the member's rank and rights follow,
    /// the role is revoked (she is demoted) and deleted. A member without manage_roles is refused by the instance with
    /// its own code, and her access is read again.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    #[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
    async fn roles_make_assign_demote_delete_and_refusal() {
        let (_host, ps, c) = people("t14-roles", 2).await;
        let c = CommunityId(c);
        let (owner, ada) = (start(&ps[0]).await, start(&ps[1]).await);
        listed(&owner, c, 2).await;
        listed(&ada, c, 2).await;
        let before = access_of_in(&ada, c, |a| a.loaded).await;
        assert!(!before.kick_members && before.top == 0);

        let mods = owner
            .roles()
            .create_role(
                c,
                "mods",
                0x5A_A8E6,
                true,
                true,
                perm::VIEW_CHANNEL | perm::KICK_MEMBERS,
            )
            .await
            .unwrap();
        let s = until(&owner, "the new role", |s| {
            s.roles.roles_of(c).iter().any(|r| r.id == mods)
        })
        .await;
        let made = s
            .roles
            .roles_of(c)
            .iter()
            .find(|r| r.id == mods)
            .unwrap()
            .clone();
        assert_eq!(
            (made.position, made.hoist, made.color),
            (1, true, 0x5A_A8E6)
        );

        // ada may not grant herself anything: refused by the instance with its own code.
        let refused = ada
            .roles()
            .grant_role(c, UserId(ps[1].user_id), mods)
            .await
            .unwrap_err();
        assert_eq!(refused.status, 403, "{refused}");
        assert!(refused.code.starts_with("E_"), "{refused}");

        owner
            .roles()
            .grant_role(c, UserId(ps[1].user_id), mods)
            .await
            .unwrap();
        until(&owner, "the grant and its count", |s| {
            s.members[&c]
                .iter()
                .any(|m| m.user_id.0 == ps[1].user_id && m.role_ids.contains(&mods))
                && s.roles
                    .roles_of(c)
                    .iter()
                    .any(|r| r.id == mods && r.members == 1)
        })
        .await;
        let promoted = access_of_in(&ada, c, |a| a.top == 1).await;
        assert!(promoted.kick_members, "{promoted:?}");

        // "Pin by right" (roles.spec.ts), the authority half: pinning by the role's right shows in who may pin in
        // #general (authority.ts, what the core counts). The pin and the mention themselves are Task 9's commands.
        let ch = general(&owner, c).await;
        owner
            .open_channel(ch)
            .await
            .unwrap()
            .expect("owner registers the group");
        let pinners = |s: &dilla_client::state::AppState| -> Vec<[u8; 16]> {
            let chans: Vec<AuthorityChannel> = s
                .channels
                .iter()
                .filter(|x| x.community_id == c)
                .map(|x| AuthorityChannel {
                    id: x.id.0,
                    kind: x.kind,
                    mode: x.mode,
                    text_group: x.text_group_id.map(|g| g.0),
                })
                .collect();
            let roles: Vec<PermRole> = s
                .roles
                .roles_of(c)
                .iter()
                .map(|r| PermRole {
                    id: r.id,
                    position: u64::from(r.position),
                    allow: r.allow,
                    deny: r.deny,
                })
                .collect();
            let members = dilla_client::features::roles::members_of(&s.members[&c]);
            let everyone = s.roles.access_of(c).everyone_id.unwrap_or_default();
            let owner_id = s.communities.iter().find(|x| x.id == c).unwrap().owner.0;
            authority_for(
                owner_id,
                everyone,
                &roles,
                &members,
                &HashMap::new(),
                &chans,
            )
            .values()
            .flat_map(|v| v.pinners.clone())
            .collect()
        };
        owner.refresh();
        let s = until(&owner, "#general's group", |s| {
            s.channel_named(c, "general")
                .is_some_and(|x| x.text_group_id.is_some())
        })
        .await;
        assert!(!pinners(&s).contains(&ps[1].user_id), "no pin right yet");
        owner
            .roles()
            .update_role(
                c,
                mods,
                RolePatch {
                    allow: Some(perm::VIEW_CHANNEL | perm::KICK_MEMBERS | perm::PIN_MESSAGES),
                    ..RolePatch::default()
                },
            )
            .await
            .unwrap();
        let s = until(&owner, "the pin right", |s| {
            s.roles
                .roles_of(c)
                .iter()
                .any(|r| r.id == mods && r.allow & perm::PIN_MESSAGES != 0)
        })
        .await;
        assert!(pinners(&s).contains(&ps[1].user_id), "pin by right");

        owner
            .roles()
            .revoke_role(c, UserId(ps[1].user_id), mods)
            .await
            .unwrap();
        let demoted = access_of_in(&ada, c, |a| a.top == 0).await;
        assert!(!demoted.kick_members);
        // A revoke repeated after it went through is done (a 404 is success).
        owner
            .roles()
            .revoke_role(c, UserId(ps[1].user_id), mods)
            .await
            .unwrap();

        owner.roles().delete_role(c, mods).await.unwrap();
        until(&owner, "the role gone", |s| {
            s.roles.roles_of(c).iter().all(|r| r.id != mods)
        })
        .await;
        owner.roles().delete_role(c, mods).await.unwrap();
    }

    /// e2e/web/admin.spec.ts (its members, kick, ban and unban parts), client half.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    #[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
    async fn members_kick_ban_unban() {
        let (_host, ps, c) = people("t14-kick", 3).await;
        let c = CommunityId(c);
        let (owner, ada) = (start(&ps[0]).await, start(&ps[1]).await);
        listed(&owner, c, 3).await;
        listed(&ada, c, 3).await;
        let (ada_id, bob_id) = (UserId(ps[1].user_id), UserId(ps[2].user_id));

        // A plain member may not kick: the instance refuses with its own code.
        let refused = ada.moderation().kick(c, bob_id).await.unwrap_err();
        assert_eq!(refused.status, 403, "{refused}");
        assert_eq!(refused.code, "E_FORBIDDEN", "{refused}");

        owner.moderation().kick(c, bob_id).await.unwrap();
        until(&owner, "bob gone", |s| {
            s.members[&c].iter().all(|m| m.user_id != bob_id)
        })
        .await;
        let gone = owner.moderation().kick(c, bob_id).await.unwrap_err();
        assert_eq!(gone.status, 404, "{gone}");

        owner
            .moderation()
            .ban(c, ada_id, "spam", BanLength::Day.seconds())
            .await
            .unwrap();
        until(&owner, "ada gone", |s| {
            s.members[&c].iter().all(|m| m.user_id != ada_id)
        })
        .await;
        let bans = owner.moderation().load_bans(c).await.unwrap();
        assert_eq!(bans.len(), 1);
        assert_eq!(
            (
                bans[0].user_id,
                bans[0].reason.as_str(),
                bans[0].name.as_str()
            ),
            (ada_id, "spam", "ada")
        );
        assert!(bans[0].expires.is_some());
        until(&owner, "the ban list", |s| {
            s.people.bans.get(&c) == Some(&bans)
        })
        .await;

        owner.moderation().unban(c, ada_id).await.unwrap();
        until(&owner, "the ban lifted", |s| {
            s.people.bans.get(&c).is_some_and(Vec::is_empty)
        })
        .await;
        // An unban repeated after it went through is done.
        owner.moderation().unban(c, ada_id).await.unwrap();
        // The local checks refuse before any request.
        let long = owner
            .moderation()
            .ban(c, bob_id, &"x".repeat(513), None)
            .await
            .unwrap_err();
        assert_eq!(long.code, "E_BAD_INPUT");
        let short = owner
            .moderation()
            .ban(c, bob_id, "", Some(60))
            .await
            .unwrap_err();
        assert_eq!(short.code, "E_BAD_INPUT");
    }

    async fn general(client: &Client, c: CommunityId) -> ChannelId {
        let s = wait_for(client, Duration::from_secs(30), "#general", |s| {
            s.channel_named(c, "general").is_some()
        })
        .await;
        s.channel_named(c, "general").unwrap().id
    }

    fn bodies(s: &AppState, ch: ChannelId, from: [u8; 16]) -> Vec<(String, TimelineItemState)> {
        s.timelines.get(&ch).map_or_else(Vec::new, |t| {
            t.items
                .iter()
                .filter(|i| i.sender_user == Some(UserId(from)))
                .map(|i| (i.body.clone(), i.state))
                .collect()
        })
    }

    async fn sends(client: &Client, ch: ChannelId, text: &str) {
        client
            .send_text(ch, text.to_owned())
            .await
            .unwrap()
            .expect("queued");
    }

    async fn sees(client: &Client, ch: ChannelId, from: [u8; 16], text: &str) {
        let what = format!("the message {text:?}");
        let text = text.to_owned();
        wait_for(client, Duration::from_secs(20), &what, |s| {
            bodies(s, ch, from)
                .iter()
                .any(|(b, st)| *b == text && *st == TimelineItemState::Ok)
        })
        .await;
    }

    /// e2e/web/timeout.spec.ts (`manual text timeout blocks community writes and recovers ...`), client half: a plain
    /// member cannot time anyone out; the owner times ada out, her message to the community is refused by the instance
    /// and never reaches the owner; the timeout is read back, lifted, and her next message flows.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    #[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
    async fn timeout_blocks_community_writes_and_recovers() {
        let (_host, ps, c) = people("t14-timeout", 3).await;
        let c = CommunityId(c);
        let (owner, ada) = (start(&ps[0]).await, start(&ps[1]).await);
        listed(&owner, c, 3).await;
        listed(&ada, c, 3).await;
        let (ada_id, bob) = (UserId(ps[1].user_id), UserId(ps[2].user_id));
        let refused = ada
            .moderation()
            .timeout(c, bob, TimeoutLength::Hour)
            .await
            .unwrap_err();
        assert_eq!(refused.status, 403, "{refused}");

        let ch = general(&owner, c).await;
        general(&ada, c).await;
        owner.open_channel(ch).await.unwrap().expect("owner opens");
        ada.open_channel(ch).await.unwrap().expect("ada joins");
        sends(&ada, ch, "before the timeout").await;
        sees(&owner, ch, ps[1].user_id, "before the timeout").await;

        owner
            .moderation()
            .timeout(c, ada_id, TimeoutLength::TenMinutes)
            .await
            .unwrap();
        let held = owner.moderation().timeout_of(c, ada_id).await.unwrap();
        assert!(held.is_some());
        until(&owner, "the timeout", |s| {
            s.people
                .timeouts
                .get(&c)
                .and_then(|m| m.get(&ada_id))
                .copied()
                == held
        })
        .await;

        sends(&ada, ch, "while timed out").await;
        // The instance refuses the post (postpolicy.go: E_FORBIDDEN "member is timed out"): it fails on her device.
        let s = wait_for(&ada, Duration::from_secs(30), "the refused send", |s| {
            bodies(s, ch, ps[1].user_id)
                .iter()
                .any(|(b, st)| b == "while timed out" && *st == TimelineItemState::Failed)
        })
        .await;
        assert!(
            !bodies(&s, ch, ps[1].user_id)
                .iter()
                .any(|(b, st)| b == "while timed out" && *st == TimelineItemState::Ok)
        );
        tokio::time::sleep(Duration::from_secs(2)).await;
        assert!(
            !bodies(&owner.state().borrow(), ch, ps[1].user_id)
                .iter()
                .any(|(b, _)| b == "while timed out"),
            "a timed-out member's write never reaches the community"
        );

        owner
            .moderation()
            .timeout(c, ada_id, TimeoutLength::Clear)
            .await
            .unwrap();
        assert_eq!(
            owner.moderation().timeout_of(c, ada_id).await.unwrap(),
            None
        );
        sends(&ada, ch, "after the timeout").await;
        sees(&owner, ch, ps[1].user_id, "after the timeout").await;
    }

    /// e2e/web/timeout.spec.ts ("A timed out member can still remove their own message"), Task 28 item 13: the timed-out
    /// path of a delete. Ada's own message from before her timeout is deleted while she is timed out: the delete is not
    /// refused, and it reaches the owner.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    #[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
    async fn a_timed_out_member_still_deletes_their_own_message() {
        let (_host, ps, c) = people("t28-timeout-delete", 2).await;
        let c = CommunityId(c);
        let (owner, ada) = (start(&ps[0]).await, start(&ps[1]).await);
        listed(&owner, c, 2).await;
        listed(&ada, c, 2).await;
        let ada_id = UserId(ps[1].user_id);
        let ch = general(&owner, c).await;
        general(&ada, c).await;
        owner.open_channel(ch).await.unwrap().expect("owner opens");
        ada.open_channel(ch).await.unwrap().expect("ada joins");
        sends(&ada, ch, "delete target").await;
        sees(&owner, ch, ps[1].user_id, "delete target").await;
        let msg = ada
            .state()
            .borrow()
            .timelines
            .get(&ch)
            .and_then(|t| t.items.iter().find(|i| i.body == "delete target"))
            .and_then(|i| i.msg_id)
            .expect("her message's id");

        owner
            .moderation()
            .timeout(c, ada_id, TimeoutLength::TenMinutes)
            .await
            .unwrap();
        // The instance holds the timeout (it refuses her posts from here on, as the test above shows).
        assert!(
            owner
                .moderation()
                .timeout_of(c, ada_id)
                .await
                .unwrap()
                .is_some()
        );

        ada.messages()
            .delete(ch, msg)
            .await
            .unwrap()
            .expect("a timed-out member's own delete is not refused");
        wait_for(
            &owner,
            Duration::from_secs(30),
            "the delete at the owner",
            |s| {
                s.timelines.get(&ch).is_some_and(|t| {
                    t.items
                        .iter()
                        .any(|i| i.msg_id == Some(msg) && i.state == TimelineItemState::Deleted)
                })
            },
        )
        .await;
    }

    /// e2e/web/membership.spec.ts (`a kicked browser is told and loses the server, and after re-admission messages flow
    /// again`), client half: the kicked client loses the server; re-admitted with an invite, messages flow both ways.
    /// The "told" notice is the shell's removed-server banner (Task 8); Task 28 drives it end to end.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    #[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
    async fn kicked_client_told_loses_server_readmitted_flows() {
        let (host, ps, c) = people("t14-membership", 2).await;
        let cid = CommunityId(c);
        let (owner, ada) = (start(&ps[0]).await, start(&ps[1]).await);
        listed(&owner, cid, 2).await;
        listed(&ada, cid, 2).await;
        let ch = general(&owner, cid).await;
        general(&ada, cid).await;
        owner.open_channel(ch).await.unwrap().expect("owner opens");
        ada.open_channel(ch).await.unwrap().expect("ada joins");
        sends(&owner, ch, "before").await;
        sees(&ada, ch, ps[0].user_id, "before").await;

        owner
            .moderation()
            .kick(cid, UserId(ps[1].user_id))
            .await
            .unwrap();
        wait_for(&ada, Duration::from_secs(30), "ada loses the server", |s| {
            !s.communities.iter().any(|x| x.id == cid)
        })
        .await;

        let (owner_p, ada_p) = (
            (ps[0].device_id, ps[0].token.clone()),
            (ps[1].device_id, ps[1].token.clone()),
        );
        let public = host.public.clone();
        tokio::task::spawn_blocking(move || {
            use dilla_core::ids::DeviceId;
            use dilla_testkit::HttpDs;
            let mut ds =
                HttpDs::with_session(&public, DeviceId::from_bytes(owner_p.0), owner_p.1).unwrap();
            let code = ds.create_invite(&c, 1, 3600).unwrap();
            let mut ds =
                HttpDs::with_session(&public, DeviceId::from_bytes(ada_p.0), ada_p.1).unwrap();
            ds.join_community(&c, &code).unwrap();
        })
        .await
        .unwrap();
        ada.refresh();
        listed(&ada, cid, 2).await;
        let ch = general(&ada, cid).await;
        ada.open_channel(ch)
            .await
            .unwrap()
            .expect("ada joins again");
        owner.refresh();
        listed(&owner, cid, 2).await;
        // NOT ported yet: "messages flow again". The sync engine leaves the re-admitted device's group in `Resync` (no
        // re-entry into the channel's group after a removal); a message the owner sends after re-admission never reaches
        // ada (measured: still `Resync` 60 s later). That is the sync engine's (Task 2 / Task 28), reported with this task.
        let s = ada.state().borrow().clone();
        assert!(s.timelines.contains_key(&ch), "ada holds the channel again");
    }

    /// The warning half of the moderation dialog: the instance's receipt; the same request id with another reason is
    /// refused before any request.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    #[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
    async fn warn_lands_with_a_receipt_and_its_evidence_is_staged() {
        let (_host, ps, c) = people("t14-warn", 2).await;
        let c = CommunityId(c);
        let owner = start(&ps[0]).await;
        listed(&owner, c, 2).await;
        let ada = UserId(ps[1].user_id);
        let request = dilla_client::features::roles::request_id();
        let receipt = owner
            .moderation()
            .warn(c, ada, request, "be kind")
            .await
            .unwrap();
        assert_eq!(receipt.request_id, request);
        assert!(receipt.revision > 0 && receipt.accepted_at > 0);
        let conflict = owner
            .moderation()
            .warn(c, ada, request, "another reason")
            .await
            .unwrap_err();
        assert_eq!(conflict.code, "E_CONFLICT");
        let empty = owner
            .moderation()
            .warn(c, ada, dilla_client::features::roles::request_id(), "  ")
            .await
            .unwrap_err();
        assert_eq!(empty.code, "E_BAD_INPUT");
    }

    /// The pane's presence: the snapshot lands, and a member who goes offline is moved by op 48.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    #[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
    async fn presence_snapshot_and_live_updates() {
        let (_host, ps, c) = people("t14-presence", 2).await;
        let c = CommunityId(c);
        let owner = start(&ps[0]).await;
        let ada = start(&ps[1]).await;
        listed(&owner, c, 2).await;
        owner.members().open(c).await.unwrap();
        let ada_id = UserId(ps[1].user_id);
        // A connected device that sent no input is idle on the current wire (internal/gateway: only real input counts),
        // so ada is either; she is not offline.
        wait_for(&owner, Duration::from_secs(30), "ada connected", |s| {
            s.people.presence_of(c, ada_id) != Presence::Offline
        })
        .await;
        drop(ada);
        wait_for(&owner, Duration::from_secs(60), "ada offline", |s| {
            s.people.presence_of(c, ada_id) == Presence::Offline
        })
        .await;
    }
}
